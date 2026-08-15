package arcade

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Cloning a server: the same tree, a new identity.
//
// Import already knows how to copy a directory into a new server, report
// progress and refuse when the disk cannot take it, so clone is that machinery
// pointed at a directory the panel already owns rather than one an operator
// named. What clone has to add is the part import does not need: deciding what
// must NOT come across.
//
// A copy of a world is the point. A copy of the source's identity is a bug -
// two panel entries claiming the same port, a lock file from a live server, or
// a marker that makes the clone look like another panel still manages it.

// CloneRequest is the create wizard's Clone Existing tab.
type CloneRequest struct {
	Source string `json:"source"` // server id
	Name   string `json:"name"`
	Port   int    `json:"port"`
}

// cloneSkip decides what does not travel.
//
// Everything here is either meaningless in the copy or actively harmful:
//
//   - session.lock is held by the source while it runs. Copied, the clone
//     starts against a lock file describing another process.
//   - logs and crash reports belong to the server that produced them. They are
//     also, on a long-lived server, most of the small files in the tree.
//   - the RCON credentials the image writes at start are regenerated per
//     container; carrying them over hands the clone the source's password.
//   - the panel markers are the whole reason import can warn "another control
//     panel manages this directory". Copied into a clone that no other panel
//     has ever seen, that warning becomes a lie the operator cannot explain.
func cloneSkip(rel string, d fs.DirEntry) bool {
	base := filepath.Base(rel)
	if d.IsDir() {
		switch base {
		case "logs", "crash-reports", "cache", ".cache":
			return true
		}
		return false
	}
	switch base {
	case "session.lock", ".rcon-cli.env", ".rcon-cli.yaml":
		return true
	}
	if _, isMarker := managedMarkers[base]; isMarker {
		return true
	}
	return false
}

// StartClone copies an existing server into a new one and returns the job that
// reports on the copy.
func (m *Manager) StartClone(req CloneRequest, actor string) (*ImportJob, error) {
	src := m.Get(req.Source)
	if src == nil {
		return nil, fmt.Errorf("no such server")
	}

	// Resolved, because an adopted-in-place server's directory is a link to
	// the operator's own tree. Following it copies the world; not following it
	// copies a symlink and produces an empty server.
	srcDir, err := m.ensureServerDir(src)
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(srcDir); err == nil {
		srcDir = resolved
	}

	tpl := templateBySlug(src.Template)
	if tpl == nil {
		return nil, fmt.Errorf("the source server's template %q no longer exists", src.Template)
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = src.Name + " copy"
	}

	port := req.Port
	if port == 0 {
		port = m.NextFreePort(tpl.PortHint)
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("port %d is out of range", port)
	}
	// Claimed rather than checked, for the same reason import claims: the copy
	// finishes minutes later on a goroutine, and two clones started together
	// would otherwise both pass a bare check and land on one port.
	if holder, ok := m.claimPort(port, name); !ok {
		return nil, fmt.Errorf("port %d is already used by %q; give a different port", port, holder)
	}
	claimHeld := true
	defer func() {
		if claimHeld {
			m.releasePort(port)
		}
	}()

	size, files, complete := measureTree(srcDir)
	if free, err := diskFree(m.dataDir); err == nil && free < size+importFreeMargin {
		return nil, fmt.Errorf("copying %s needs %s free and only %s is left on the panel's disk",
			src.Name, humanSize(size+importFreeMargin), humanSize(free))
	}

	s := m.newServer(name, tpl, src.Version, port, src.Runtime)
	// The clone runs what the source runs, not what the template would pick.
	// An imported modpack's jar is the case that matters: matching by version
	// hands the clone a loader build its mods were not compiled against.
	s.Image = src.Image
	s.LaunchJar = src.LaunchJar
	s.MemoryMB = src.MemoryMB
	s.CPU = src.CPU
	s.DiskGB = src.DiskGB
	s.MaxPlayers = src.MaxPlayers
	// Under the source's lock: Go panics fatally, unrecoverably, on a
	// concurrent map read/write, and reloadProps writes this map whenever
	// server.properties changes.
	src.mu.Lock()
	for k, v := range src.Props {
		s.Props[k] = v
	}
	src.mu.Unlock()
	// Two things in server.properties are identity rather than configuration.
	s.Props["server-port"] = itoa(port)
	s.Props["motd"] = name

	dst := filepath.Join(m.dataDir, "servers", s.ID)
	if _, err := os.Lstat(dst); err == nil {
		return nil, fmt.Errorf("%s already exists; the panel will not clone on top of it", dst)
	}

	// The job is the import UI's progress contract; a clone reports through the
	// same one so the wizard has one flow rather than two.
	sc := &ImportScan{
		Path: srcDir, Name: name, Template: src.Template, Version: src.Version,
		SizeBytes: size, Files: files, SizePartial: !complete,
		SizeHuman: humanSize(size),
	}
	job := newImportJob(sc, "clone", name)
	claimHeld = false // the goroutine owns the claim now

	go func() {
		defer recoverPanic("clone of " + src.Name)

		// A running source is quiesced exactly the way a backup quiesces it.
		// Copying a live world without pausing saves reads region files mid
		// write, and the clone boots on a torn world - which looks like
		// corruption in the copy rather than a bad copy.
		wasRunning := src.State() == StatusRunning
		if wasRunning {
			if !m.lockBackup(src.ID) {
				m.releasePort(port)
				job.fail(fmt.Errorf("a backup is running for %s; try again when it finishes", src.Name))
				return
			}
			defer m.unlockBackup(src.ID)
			m.panelLine(src, "info", "Cloning - pausing world saves and flushing to disk.")
			_ = m.runnerFor(src).Send(src, "save-off")
			_ = m.runnerFor(src).Send(src, "save-all flush")
			time.Sleep(1500 * time.Millisecond)
			defer func() {
				_ = m.runnerFor(src).Send(src, "save-on")
				m.panelLine(src, "info", "Clone finished - world saves resumed.")
			}()
		}

		if err := copyTreeFiltered(srcDir, dst, job, cloneSkip); err != nil {
			// A half-copied tree is a server that boots on a truncated world.
			_ = os.RemoveAll(dst)
			m.releasePort(port)
			job.fail(friendlyFSError(err, "the cloned server"))
			return
		}

		m.mu.Lock()
		m.servers[s.ID] = s
		m.order = append(m.order, s.ID)
		delete(m.reservedPorts, s.Port)
		m.mu.Unlock()

		if err := m.Save(); err != nil {
			log.Printf("cloned %s but could not persist the server list: %v", s.Name, err)
		}
		// The copied server.properties still carries the source's port. Left
		// alone the panel and the game disagree, and the game wins.
		if err := m.writeProps(s); err != nil {
			log.Printf("cloned %s but could not write server.properties: %v", s.Name, err)
		}

		m.audit(actor, "server.clone", s.ID, fmt.Sprintf("%s from %s (%s)",
			s.Name, src.Name, humanSize(size)))
		m.broadcastEvent("server.created", s.ID)
		job.done(s.ID)
	}()

	view := job.view()
	return &view, nil
}
