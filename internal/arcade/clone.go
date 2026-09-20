package arcade

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
	// Claimed with the template's FULL geometry: the clone inherits the
	// source's span and fixed extras, and a base-only claim let a Valheim
	// clone at 65535 pass admission only for Start to publish 65537.
	cloneCand, err := candidateBindings(port, tpl.Protocols, tpl.PortSpan, tpl.ExtraPorts)
	if err != nil {
		return nil, err
	}
	// Claimed under a unique lease, never the display name: two same-named
	// clones used to be able to release each other's reservation.
	lease, holder, ok := m.claimPortBindings(cloneCand, name)
	if !ok {
		if holder == "" {
			return nil, fmt.Errorf("could not allocate a port reservation")
		}
		return nil, fmt.Errorf("port %d is already used by %q; give a different port", port, holder)
	}
	claimHeld := true
	defer func() {
		if claimHeld {
			m.releaseReservation(lease)
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

		// A clone is a snapshot transaction: exclusive on the source's
		// filesystem gate, so settings writes, file edits and restores
		// cannot interleave generations into the copy. For a stopped source
		// the old shape took no gate at all.
		src.fsMu.Lock()
		defer src.fsMu.Unlock()

		if !m.lockBackup(src.ID) {
			m.releaseReservation(lease)
			job.fail(fmt.Errorf("a snapshot operation is running for %s; try again when it finishes", src.Name))
			return
		}
		defer m.unlockBackup(src.ID)

		// A running source is quiesced exactly the way a backup quiesces it.
		// Copying a live world without pausing saves reads region files mid
		// write, and the clone boots on a torn world - which looks like
		// corruption in the copy rather than a bad copy. A quiesce that
		// cannot be delivered aborts the clone before any bytes are copied.
		resume, err := m.quiesceForBackup(src)
		if err != nil {
			m.releaseReservation(lease)
			job.fail(fmt.Errorf("could not quiesce %s for cloning: %v", src.Name, err))
			return
		}
		// R27 (audit pass 7): the resume result is part of the clone's
		// result. A bare `defer resume()` discarded its error, so a clone
		// could report success with the source still holding save-off - the
		// source's world silently unsaveable until somebody noticed. The
		// copy finishing and the source being safely resumed are separate
		// outcomes, and both have to be true before the job is done.
		resumed := false
		resumeOrFail := func() error {
			if resumed {
				return nil
			}
			resumed = true
			if rerr := resume(); rerr != nil {
				m.panelLine(src, "error", "Clone finished, but world saves could NOT be resumed - run save-on from the console.")
				return rerr
			}
			return nil
		}
		defer func() { _ = resumeOrFail() }()

		if err := copyTreeFiltered(srcDir, dst, job, cloneSkip); err != nil {
			// A half-copied tree is a server that boots on a truncated world.
			_ = os.RemoveAll(dst)
			m.releaseReservation(lease)
			job.fail(friendlyFSError(err, "the cloned server"))
			return
		}

		// The source must be safely resumed before the clone is allowed to
		// succeed (R27): registration and job.done only happen when the
		// save-on landed, and a failed resume fails the job with the copy
		// still cleaned up - never a green clone over a muted source.
		if rerr := resumeOrFail(); rerr != nil {
			_ = os.RemoveAll(dst)
			m.releaseReservation(lease)
			job.fail(fmt.Errorf("copied the files but could not resume world saves on %s: %w", src.Name, rerr))
			return
		}

		// The copy was made by root; the source's own ownership is the answer
		// to who should hold it, since that is what the game has been running
		// as. Without this a clone starts once and dies on server.properties.
		chownTreeLike(dst, srcDir)

		// The copied server.properties still carries the source's port. Left
		// alone the panel and the game disagree, and the game wins. Written
		// and checked BEFORE the clone is registered: a clone whose
		// properties could not be written must not be exposed as a
		// configured server, because the panel and the game would disagree
		// about its port from the first boot.
		if err := m.writeProps(s); err != nil {
			m.releaseReservation(lease)
			job.fail(fmt.Errorf("copied the files but could not write the clone's server.properties: %v", err))
			return
		}

		// Registration through persistence under the lifecycle mutex (no
		// fsMu reachable inside): a provisional clone must never be startable
		// before its Save commits, or rollback would orphan a running server.
		m.lifecycle.Lock()
		defer m.lifecycle.Unlock()
		m.mu.Lock()
		m.servers[s.ID] = s
		m.order = append(m.order, s.ID)
		// The whole lease (span and extras included), dropped in the same
		// critical section as the registration.
		m.releaseReservationLocked(lease)
		m.mu.Unlock()

		// Registered, but not exposed until it survives persistence: a clone
		// Save cannot record exists only in memory and vanishes on the next
		// restart, so the registration is rolled back and the job fails
		// rather than reporting a success the panel cannot keep.
		if err := m.Save(); err != nil {
			m.mu.Lock()
			delete(m.servers, s.ID)
			for i, id := range m.order {
				if id == s.ID {
					m.order = append(m.order[:i], m.order[i+1:]...)
					break
				}
			}
			m.mu.Unlock()
			m.releasePort(port)
			job.fail(fmt.Errorf("copied the files but could not persist the server list: %v", err))
			return
		}

		m.audit(actor, "server.clone", s.ID, fmt.Sprintf("%s from %s (%s)",
			s.Name, src.Name, humanSize(size)))
		m.broadcastEvent("server.created", s.ID)
		job.done(s.ID)
	}()

	view := job.view()
	return &view, nil
}
