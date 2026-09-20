package arcade

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Phase 5: backup and restore.
//
// The interesting part is the quiesce window (PLAN.md §8). A snapshot taken
// while the game is mid-chunk-write is a corrupt snapshot, so:
//
//	save-off ; save-all  ->  archive  ->  save-on
//
// and the file API refuses writes for the whole window. The lock is per-server,
// so backing one server up never blocks another.

type Backup struct {
	ID      string `json:"id"`
	Server  string `json:"server"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Created int64  `json:"created"`
	Note    string `json:"note"`
	// True while tarGz is still writing this archive. Size is partial until it
	// clears.
	InProgress bool `json:"in_progress"`
}

type backupState struct {
	mu      sync.Mutex
	locked  map[string]bool
	current map[string]string // server id -> the archive id being written
}

func (m *Manager) backupDir(id string) string {
	return filepath.Join(m.dataDir, "backups", id)
}

func (m *Manager) backupLocked(id string) bool {
	m.backups.mu.Lock()
	defer m.backups.mu.Unlock()
	return m.backups.locked[id]
}

func (m *Manager) lockBackup(id string) bool {
	m.backups.mu.Lock()
	defer m.backups.mu.Unlock()
	if m.backups.locked[id] {
		return false
	}
	m.backups.locked[id] = true
	return true
}

func (m *Manager) markBackupInFlight(serverID, archiveID string) {
	m.backups.mu.Lock()
	if m.backups.current == nil {
		m.backups.current = map[string]string{}
	}
	m.backups.current[serverID] = archiveID
	m.backups.mu.Unlock()
}

func (m *Manager) currentBackupID(serverID string) string {
	m.backups.mu.Lock()
	defer m.backups.mu.Unlock()
	return m.backups.current[serverID]
}

func (m *Manager) unlockBackup(id string) {
	m.backups.mu.Lock()
	delete(m.backups.locked, id)
	delete(m.backups.current, id)
	m.backups.mu.Unlock()
}

func (m *Manager) ListBackups(s *Server) ([]Backup, error) {
	dir := m.backupDir(s.ID)
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []Backup{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Backup{}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tar.gz") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".tar.gz")
		note := ""
		if b, err := os.ReadFile(filepath.Join(dir, id+".note")); err == nil {
			note = string(b)
		}
		// An archive still being written has a partial size. Reporting it as a
		// finished backup's size is a small lie with a large consequence: it is
		// what an operator checks to decide the backup worked.
		out = append(out, Backup{
			ID: id, Server: s.ID, Name: e.Name(), Size: info.Size(),
			Created: info.ModTime().Unix(), Note: note,
			InProgress: m.backupLocked(s.ID) && id == m.currentBackupID(s.ID),
		})
	}
	// Newest first, and the ID breaks a tie. Two archives can carry the same
	// mtime to the second - a restore, a copy off another host, two backups a
	// moment apart - and mtime alone then orders them arbitrarily, which for
	// retention means the arbitrary one is the archive that gets deleted. The
	// ID is a millisecond timestamp and sorts lexicographically, so it is the
	// tiebreak that makes "keep the newest N" mean something.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Created != out[j].Created {
			return out[i].Created > out[j].Created
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// quiesceForBackup pauses world saves on a running server so an archive cannot
// catch a half-written chunk, and returns the func that resumes them.
//
// States are classified explicitly, because "not running" and "not writing"
// are different facts. A `starting` server is already generating its world,
// and the old shape treated every non-running state as safe to archive.
func (m *Manager) quiesceForBackup(s *Server) (func() error, error) {
	switch s.State() {
	case StatusRunning:
		// Quiesce below.
	case StatusStopped, StatusFailed:
		// The panel believes the game is down. For a Docker server the
		// container is the truth, and a still-running container means the
		// tree is being written under whatever the panel believes.
		if s.Runtime == RuntimeDocker && containerRunning(s.ID) {
			return nil, fmt.Errorf("this server's container is still running; stop it before backing it up")
		}
		return func() error { return nil }, nil
	default:
		return nil, fmt.Errorf("wait until this server finishes starting or stopping before backing it up")
	}
	if s.Game != "minecraft-java" {
		return nil, fmt.Errorf(
			"live backups are not quiesce-safe for %s; stop the server before backing it up", s.Game)
	}

	r := m.runnerFor(s)
	// Ask through query where the runner supports it, so a command the game
	// rejected is a failure instead of a silent success: plain Send used to
	// discard the reply, and delivery of the bytes was all "verified" meant.
	sendQuiesce := func(cmd string) error {
		if dr, ok := r.(*dockerRunner); ok {
			_, err := dr.query(s, cmd)
			return err
		}
		return r.Send(s, cmd)
	}
	if err := sendQuiesce("save-off"); err != nil {
		return nil, fmt.Errorf("could not pause world saves, backup aborted (the world is untouched): %w", err)
	}
	resume := func() error {
		if err := sendQuiesce("save-on"); err != nil {
			return fmt.Errorf("world saves could not be resumed: %w", err)
		}
		return nil
	}
	if err := sendQuiesce("save-all flush"); err != nil {
		_ = resume()
		return nil, fmt.Errorf("could not flush the world, backup aborted (the world is untouched): %w", err)
	}
	// Give the game a moment to actually flush before we read the tree.
	time.Sleep(1500 * time.Millisecond)
	return resume, nil
}

// CreateBackup runs the full quiesce -> archive -> resume cycle.
func (m *Manager) CreateBackup(s *Server, note, actor string) (b *Backup, retErr error) {
	// The whole archive window is exclusive on the server's filesystem gate:
	// file and plugin mutations hold it shared from before their backup-state
	// check through their completed write, so nothing can pass the check and
	// land mid-archive. The backupState lock below still provides the friendly
	// "already running" error; this one provides the invariant.
	s.fsMu.Lock()
	defer s.fsMu.Unlock()

	if err := m.requireRegistered(s); err != nil {
		return nil, err
	}
	if !m.lockBackup(s.ID) {
		return nil, fmt.Errorf("a backup is already running for this server")
	}
	defer m.unlockBackup(s.ID)

	src, err := m.ensureServerDir(s)
	if err != nil {
		return nil, err
	}
	dir := m.backupDir(s.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	// Refuse a backup the disk cannot hold, the same way create, clone and
	// import already do - this was the one path writing gigabytes with nothing
	// checking there was room for them.
	//
	// It matters more here than anywhere else, and for a reason particular to
	// this panel: worlds and backups share a filesystem by design (see
	// hostcap.go), so a backup that fills the disk does not merely fail. It
	// takes every running server on the host down with it, mid-chunk-write,
	// while protecting the thing it just destroyed.
	//
	// The uncompressed size is the bar because it is the only bound that is
	// certainly enough - a tar.gz is normally a fraction of it, so this refuses
	// some backups that would have fit. That trade is deliberate: a refused
	// backup costs an operator a disk cleanup, and a full disk costs them the
	// fleet.
	if size, _, _ := measureTree(src); size > 0 {
		if free, err := diskFree(m.dataDir); err == nil && free < size+importFreeMargin {
			return nil, fmt.Errorf(
				"backing up %s could need up to %s and only %s is free on the panel's disk; "+
					"delete an old backup or raise retention first",
				s.Name, humanSize(size+importFreeMargin), humanSize(free))
		}
	}

	wasRunning := s.State() == StatusRunning
	if wasRunning {
		m.panelLine(s, "info", "Backup starting - pausing world saves and flushing to disk.")
	}
	resume, err := m.quiesceForBackup(s)
	if err != nil {
		if wasRunning {
			m.panelLine(s, "error", "Backup aborted: "+err.Error())
		}
		return nil, err
	}
	// Resume saves no matter how the archive goes - a server left with
	// save-off is a far worse outcome than a failed backup. But a resume that
	// FAILS is no longer reported as one that succeeded: the response and the
	// console both say saves are still off, because reporting a verified
	// snapshot with saves resumed was a lie exactly when it mattered.
	defer func() {
		if !wasRunning {
			return
		}
		if rerr := resume(); rerr != nil {
			m.panelLine(s, "error", "Backup finished, but world saves could NOT be resumed - run save-on from the console.")
			retErr = errors.Join(retErr, fmt.Errorf("the archive was created, but %s", rerr))
			return
		}
		m.panelLine(s, "info", "Backup finished - world saves resumed.")
	}()

	// The ID used to be second-resolution, so two backups of one server inside
	// the same second produced the same name: the second os.Create truncated
	// the first archive and its .note, and the operator who asked for two
	// backups silently ended up with one. Milliseconds separate them, and
	// tarGz's no-replace publication is what makes a clash impossible rather
	// than merely unlikely - a name already on disk is never written through.
	// The retry re-stamps instead of failing a backup over a name.
	var (
		id   string
		dst  string
		size int64
	)
	for attempt := 0; ; attempt++ {
		now := time.Now()
		id = fmt.Sprintf("%s-%03d-%s", now.Format("20060102-150405"), now.Nanosecond()/1e6, s.ID)
		dst = filepath.Join(dir, id+".tar.gz")
		size, err = tarGz(src, dst)
		if !errors.Is(err, os.ErrExist) {
			break
		}
		if attempt == 4 {
			return nil, fmt.Errorf("could not find a free name for the backup archive")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if err != nil {
		return nil, friendlyFSError(err, "the backup archive")
	}
	if note != "" {
		// Atomic and checked: the returned Backup and the UI both report this
		// note, so silently failing to write it makes the panel describe a
		// backup that does not exist as described.
		if err := writeFileAtomic(filepath.Join(dir, id+".note"), []byte(note), 0o644); err != nil {
			log.Printf("backup %s: could not record its note: %v", id, err)
			note = ""
		}
	}

	m.audit(actor, "backup.create", s.ID, fmt.Sprintf("%s (%s)", id, humanSize(size)))
	m.broadcastEvent("backup.created", s.ID)

	m.pruneBackups(s, actor)

	return &Backup{ID: id, Server: s.ID, Name: id + ".tar.gz", Size: size,
		Created: time.Now().Unix(), Note: note}, nil
}

// pruneBackups enforces this server's retention, newest kept.
//
// Called only after a backup has actually landed, never on a timer. Retention
// that runs on its own schedule deletes the last copy of a world on the day the
// backup job is broken - precisely when the operator needs it - so a new archive
// existing is the precondition for an old one going away.
//
// A prune failure is logged and not returned: the backup succeeded, and telling
// the operator it failed because the cleanup afterwards did would be a lie about
// the thing they asked for.
func (m *Manager) pruneBackups(s *Server, actor string) {
	s.mu.Lock()
	keep := s.BackupKeep
	s.mu.Unlock()
	if keep <= 0 {
		return
	}
	list, err := m.ListBackups(s)
	if err != nil || len(list) <= keep {
		return
	}
	// ListBackups returns newest first, so everything past the keep count is
	// the tail to drop.
	dir := m.backupDir(s.ID)
	for _, b := range list[keep:] {
		if err := os.Remove(filepath.Join(dir, b.ID+".tar.gz")); err != nil {
			log.Printf("%s: could not prune backup %s: %v", s.Name, b.ID, err)
			continue
		}
		_ = os.Remove(filepath.Join(dir, b.ID+".note"))
		m.audit(actor, "backup.prune", s.ID, fmt.Sprintf("%s (retention keeps %d)", b.ID, keep))
	}
	m.broadcastEvent("backup.pruned", s.ID)
}

// SetBackupKeep sets this server's retention. 0 keeps every archive.
//
// Deliberately does not prune on the way out: lowering retention from ten to
// two is a statement about future backups, and acting on it immediately would
// destroy eight archives from a settings field. The next backup applies it,
// by which point there is a fresh copy to keep.
func (m *Manager) SetBackupKeep(s *Server, keep int, actor string) error {
	if keep < 0 || keep > 1000 {
		return fmt.Errorf("keep must be between 0 (keep everything) and 1000")
	}
	s.mu.Lock()
	s.BackupKeep = keep
	s.mu.Unlock()
	if err := m.Save(); err != nil {
		return err
	}
	m.audit(actor, "backup.retention", s.ID, fmt.Sprintf("keep=%d", keep))
	return nil
}

// RestoreBackup replaces the server directory with an archive's contents. The
// server must be stopped: restoring under a running game would have the process
// writing into a tree being replaced beneath it.
func (m *Manager) RestoreBackup(s *Server, backupID, actor string) error {
	// The filesystem gate comes BEFORE the state check: Start commits under
	// the same gate, so with the check first a server could launch between
	// the two and the restore would swap files under a live game.
	s.fsMu.Lock()
	defer s.fsMu.Unlock()

	if st := s.State(); st != StatusStopped && st != StatusFailed {
		return fmt.Errorf("stop the server before restoring a backup")
	}

	if !m.lockBackup(s.ID) {
		return fmt.Errorf("a backup is already running for this server")
	}
	defer m.unlockBackup(s.ID)

	// A strict charset, not the old "reject separators unless it starts with
	// 20" rule - which exempted every real id from the check it was performing,
	// so `20/../../etc/passwd` passed it and only filepath.Base below made the
	// path safe. Defence should not be accidental.
	if !validBackupID(backupID) {
		return fmt.Errorf("bad backup id %q", backupID)
	}
	archive := filepath.Join(m.backupDir(s.ID), filepath.Base(backupID)+".tar.gz")
	st, err := os.Stat(archive)
	if err != nil {
		return fmt.Errorf("no such backup")
	}

	// The server dir resolves first: for an adopted-in-place server the tree
	// lives on the operator's filesystem, and the free-space guard must
	// measure the disk the archive is actually about to expand onto - not
	// the panel's.
	dir, err := m.ensureServerDir(s)
	if err != nil {
		return err
	}

	// A restore extracts into a staging directory on the same filesystem before
	// anything live is touched, so running out of room part way through cannot
	// damage the world - the staging tree is removed and the server directory
	// is untouched. This check is not what makes that safe; it is what stops
	// the panel from spending ten minutes filling a shared disk, and taking
	// every other server on the host down with it, to reach that conclusion.
	//
	// The estimate is only an admission heuristic (the gzip size trailer wraps
	// modulo 4 GiB); extraction re-checks real free space per entry.
	if need := uncompressedSize(archive, st.Size()); need > 0 {
		if free, err := diskFree(dir); err == nil && free < need+importFreeMargin {
			return fmt.Errorf(
				"restoring this backup needs about %s free and only %s is left on the server's filesystem",
				humanSize(need+importFreeMargin), humanSize(free))
		}
	}

	// Restore the directory's CONTENTS, never the directory itself.
	//
	// serverDir resolves symlinks, so for a server adopted in place `dir` is
	// the operator's own tree - somewhere outside the panel entirely. The
	// previous shape renamed `dir` aside, recreated it and deleted the
	// original, which for an adopted server destroyed a directory the panel
	// does not own and broke the symlink pointing at it.
	//
	// Staging keeps the old guarantee (nothing is touched until the archive has
	// fully extracted) without ever unlinking the target directory.
	//
	// It is created INSIDE the target directory, not under the panel's data
	// dir: an adopted-in-place server's tree can live on another filesystem,
	// and every rename below crosses between the target and the staging area -
	// a staging dir on the panel's own volume made restore fail with EXDEV on
	// the very first entry for exactly that layout.
	//
	// Extracted data and transaction bookkeeping live in SEPARATE siblings
	// ("new" and "old"): the archive namespace is the server's own, and a
	// legitimate top-level ".previous" entry used to collide with the holding
	// directory and silently never be installed.
	//
	// keepStaging gates the cleanup: a rollback that could not finish leaves
	// the original world held inside staging, and deleting it would destroy
	// the only remaining copy. The unconditional defer used to do exactly
	// that.
	staging, err := os.MkdirTemp(dir, ".arcade-restore-")
	if err != nil {
		return err
	}
	keepStaging := false
	defer func() {
		if !keepStaging {
			_ = os.RemoveAll(staging)
		}
	}()
	stagingName := filepath.Base(staging)

	extracted := filepath.Join(staging, "new")
	held := filepath.Join(staging, "old")
	if err := os.Mkdir(extracted, 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(held, 0o700); err != nil {
		return err
	}

	if err := untarGz(archive, extracted); err != nil {
		return fmt.Errorf("restore failed, the live world was not touched: %w", err)
	}

	// The staged tree's server.properties is validated BEFORE anything live
	// moves: an archive carrying a port another server owns would otherwise
	// be installed first and rejected by reloadProps afterwards, leaving disk
	// and the panel's model disagreeing about the port.
	if content, err := os.ReadFile(filepath.Join(extracted, "server.properties")); err == nil {
		if err := m.validatePropsPort(s, string(content)); err != nil {
			return err
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == stagingName {
			continue
		}
		if err := os.Rename(filepath.Join(dir, e.Name()), filepath.Join(held, e.Name())); err != nil {
			// Put back whatever moved. If even that fails, the staging tree
			// is RETAINED - it holds the only copies of what moved out.
			if rerr := restoreHeld(held, dir); rerr != nil {
				keepStaging = true
				return fmt.Errorf("could not clear the live world, and the previous world is retained at %s: %w", staging, rerr)
			}
			return fmt.Errorf("could not clear the live world, nothing was changed: %w", err)
		}
	}

	// Installed entries are tracked so a failure part way through the install
	// removes them before the held entries go back: restoring an old file over
	// a just-installed new one fails with the destination already occupied
	// (directories cannot be renamed over at all), which used to leave a mixed
	// old/new world behind an error message claiming the previous world was
	// put back.
	var installed []string
	staged, err := os.ReadDir(extracted)
	if err != nil {
		if rerr := rollbackRestore(installed, held, dir); rerr != nil {
			keepStaging = true
			return fmt.Errorf("restore failed and the previous world is retained at %s: %w", staging, rerr)
		}
		return err
	}
	for _, e := range staged {
		if err := os.Rename(filepath.Join(extracted, e.Name()), filepath.Join(dir, e.Name())); err != nil {
			if rerr := rollbackRestore(installed, held, dir); rerr != nil {
				keepStaging = true
				return fmt.Errorf("restore failed part way, and the previous world is retained at %s: %w", staging, rerr)
			}
			return fmt.Errorf("restore failed part way, the previous world was put back: %w", err)
		}
		installed = append(installed, e.Name())
	}

	// Extracted by root into staging and moved in, so every restored file is
	// root's while the game runs as uid 1000 - a restore would hand back a
	// world the server cannot write. The directory itself was never replaced,
	// so it still carries the ownership everything under it should have.
	chownTreeLike(dir, dir)

	// The archive carries its own server.properties; adopt it.
	if content, err := os.ReadFile(filepath.Join(dir, "server.properties")); err == nil {
		// Best-effort on the restore path: the tree is already installed,
		// and a persistence failure must not unwind a completed restore.
		if err := m.reloadProps(s, string(content)); err != nil {
			log.Printf("%s: restored server.properties applied in memory but not persisted: %v", s.ID, err)
		}
		// If the restored port was refused (another server claimed it in the
		// window between the pre-swap validation and this commit), the model
		// kept the old port while the file names the refused one - republish
		// the model's port so disk and panel agree.
		s.mu.Lock()
		modelPort := s.Props["server-port"]
		s.mu.Unlock()
		if modelPort != "" && !hasPortLine(string(content), modelPort) {
			log.Printf("%s: restored server.properties names a port the panel refused; republishing the panel's port %s",
				s.ID, modelPort)
			if err := m.writePropsHeld(s); err != nil {
				log.Printf("%s: could not republish the panel's port after a refused restore: %v", s.ID, err)
			}
		}
	}

	// Commit marker LAST - after ownership repair and props reload: a marker
	// written earlier let boot recovery treat a restore as complete when
	// only the renames had finished. Crash before this point and recovery
	// puts the previous world back; crash after it and recovery only drops
	// the staging tree.
	if err := os.WriteFile(filepath.Join(staging, ".committed"), []byte("1"), 0o644); err != nil {
		// The world IS restored; failing the request now would roll nothing
		// back but leave the marker missing, so boot recovery would undo a
		// finished restore. Log loudly instead.
		log.Printf("%s: restore completed but its commit marker could not be written: %v", s.Name, err)
	}

	m.audit(actor, "backup.restore", s.ID, backupID)
	m.broadcastEvent("backup.restored", s.ID)
	return nil
}

// uncompressedSize estimates what a .tar.gz will expand to.
//
// gzip records the uncompressed length in the last four bytes of the stream,
// which is exact - and modulo 4 GiB, which most game-server archives are not
// but a modpack world can be. A recorded length below the compressed size is
// proof the counter wrapped, so in that case it is discarded for a flat
// multiple of the file on disk. Four is a deliberate under-estimate of what
// region files actually compress to: this decides whether to attempt a restore,
// and refusing one that would have fit is worse than attempting one that then
// fails safely into staging.
//
// Returns 0 when the trailer cannot be read at all, which the caller treats as
// "unknown" and does not refuse on.
func uncompressedSize(path string, compressed int64) int64 {
	if compressed < 18 { // smaller than an empty gzip member
		return 0
	}
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	var trailer [4]byte
	if _, err := f.ReadAt(trailer[:], compressed-4); err != nil {
		return 0
	}
	isize := int64(trailer[0]) | int64(trailer[1])<<8 | int64(trailer[2])<<16 | int64(trailer[3])<<24
	if isize <= compressed {
		return compressed * 4
	}
	return isize
}

// validBackupID accepts only the shape CreateBackup produces:
// 20060102-150405-<millis>-<serverID>, digits, letters and dashes.
func validBackupID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '-':
		default:
			return false
		}
	}
	return true
}

func (m *Manager) DeleteBackup(s *Server, backupID, actor string) error {
	// Delete takes the same per-server lock CreateBackup and RestoreBackup do.
	// Without it, removing the archive a backup is still writing unlinks the
	// directory entry while the writer keeps filling the orphaned inode: tarGz
	// reports a size, the audit records backup.create as a success, and no
	// archive exists on disk. The operator is told a backup they do not have is
	// safe, which is the worst thing a backup feature can do.
	if !m.lockBackup(s.ID) {
		return fmt.Errorf("a backup is already running for this server")
	}
	defer m.unlockBackup(s.ID)

	if !validBackupID(backupID) {
		return fmt.Errorf("bad backup id %q", backupID)
	}
	base := filepath.Base(backupID)
	dir := m.backupDir(s.ID)
	if err := os.Remove(filepath.Join(dir, base+".tar.gz")); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(dir, base+".note"))
	m.audit(actor, "backup.delete", s.ID, backupID)
	return nil
}

// ---------------------------------------------------------------- archive

// tarGz refuses to publish over an existing dst. The archive is assembled in
// a uniquely-named temp file (the fixed "<dst>.part" name was both predictable
// - a symlink planted on it redirected the archive write - and leaked on every
// early error), published with link-then-unlink so an existing final name
// fails with EEXIST instead of being silently replaced, and cleaned up on
// every path by the defer that follows creation.
//
// The whole walk is confined to an os.Root held for the archive's duration:
// the game and its plugins can replace entries between a stat and an open, and
// an unconfined walk used to reopen each path by name - a swapped symlink
// could feed the archiver a file from outside the server tree, and a special
// file could block it forever. Files are opened nonblocking and must be
// regular.
func tarGz(src, dst string) (int64, error) {
	root, err := os.OpenRoot(src)
	if err != nil {
		return 0, err
	}
	defer root.Close()

	f, err := os.CreateTemp(filepath.Dir(dst), ".arcade-tmp-backup-*")
	if err != nil {
		return 0, err
	}
	part := f.Name()
	// Every path from here cleans up after itself; only a completed, synced,
	// published archive survives this function.
	defer func() { _ = f.Close(); _ = os.Remove(part) }()

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	var walk func(rel string) error
	walk = func(rel string) error {
		// O_DIRECTORY|O_NONBLOCK (R15, audit pass 7): a FIFO swapped in where
		// a directory was expected used to block the plain open for as long
		// as no writer appeared - with the save-off quiesce held the whole
		// time.
		d, err := openDirIn(root, rel)
		if err != nil {
			return err
		}
		ents, err := d.ReadDir(-1)
		d.Close()
		if err != nil {
			return err
		}
		for _, e := range ents {
			child := e.Name()
			if rel != "." {
				child = rel + "/" + e.Name()
			}
			fi, err := root.Lstat(child)
			if err != nil {
				return err
			}
			// Never archive a symlink's target; skip links entirely.
			if fi.Mode()&os.ModeSymlink != 0 {
				continue
			}
			if !fi.IsDir() && !fi.Mode().IsRegular() {
				return fmt.Errorf("%s is not a regular file; refusing to archive it", child)
			}
			if fi.IsDir() {
				hdr, err := tar.FileInfoHeader(fi, "")
				if err != nil {
					return err
				}
				hdr.Name = filepath.ToSlash(child)
				if err := tw.WriteHeader(hdr); err != nil {
					return err
				}
				if err := walk(child); err != nil {
					return err
				}
				continue
			}
			// Metadata comes from the descriptor this archive actually reads,
			// not from the walk's earlier stat of the name.
			in, st, err := openRegularIn(root, child)
			if err != nil {
				return err
			}
			if !st.Mode().IsRegular() {
				in.Close()
				return fmt.Errorf("%s changed under the archive into a non-regular file", child)
			}
			// R29 (audit pass 7): the header describes the DESCRIPTOR's size
			// and the copy writes exactly that many bytes. The header used to
			// come from the walk's earlier Lstat while io.Copy ran to EOF, so
			// a log growing under the archive blew past its declared entry
			// size and failed the whole backup with tar.ErrWriteTooLong, and
			// a shrinking file left a short entry. A short read is an error,
			// not a silently truncated member.
			hdr, err := tar.FileInfoHeader(st, "")
			if err != nil {
				in.Close()
				return err
			}
			hdr.Name = filepath.ToSlash(child)
			hdr.Size = st.Size()
			if err := tw.WriteHeader(hdr); err != nil {
				in.Close()
				return err
			}
			_, err = io.CopyN(tw, in, st.Size())
			in.Close()
			if err != nil {
				return fmt.Errorf("%s changed size under the archive: %w", child, err)
			}
		}
		return nil
	}
	err = walk(".")

	if cerr := tw.Close(); err == nil {
		err = cerr
	}
	if cerr := gz.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	// Size and durability both have to come after a successful Close. Buffered
	// data is flushed there, so ENOSPC and EDQUOT surface at Close and nowhere
	// earlier - and a discarded Close error means CreateBackup audits a
	// truncated archive as a completed backup, which is the one thing a backup
	// system must never do. Sync before it, so a power loss right after a
	// "successful" backup cannot leave a holed file either.
	if err := f.Sync(); err != nil {
		return 0, err
	}
	info, serr := f.Stat()
	if serr != nil {
		return 0, serr
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	// Link-then-unlink instead of rename: rename silently REPLACES an
	// existing dst, which lost the no-overwrite guarantee. link() fails with
	// EEXIST, so the caller's re-stamp retry works.
	if err := os.Link(part, dst); err != nil {
		return 0, err
	}
	// The directory entry for the published name is durable before the
	// function reports success.
	if parent, perr := os.Open(filepath.Dir(dst)); perr == nil {
		_ = parent.Sync()
		_ = parent.Close()
	}
	return info.Size(), nil
}

// restoreLimits bounds what one archive is allowed to expand into. Fields
// rather than constants so the guards can be driven from a test without
// generating tens of gigabytes.
type restoreLimits struct {
	entryBytes int64
	totalBytes int64
	entries    int
}

// The restore path honoured hdr.Size but never checked it, so a few kilobytes
// of crafted gzip could expand until the data volume was full - taking down the
// panel and every server sharing the disk with it, from a restore an operator
// asked for.
//
// The numbers are picked so that nothing real reaches them. The largest single
// file in a server tree is a region file, a mod jar or a rotated log - tens of
// megabytes - so 8 GiB for one entry is three orders of magnitude of headroom.
// Whole worlds genuinely do reach several GB (a long-lived modded world with a
// large render distance is the honest worst case), so the archive total is
// 64 GiB: past any world a single host can serve, and still far below the
// expansion a bomb needs to be worth building. The entry count is the same
// guard for inodes, which bytes alone do not cover - ten million empty files
// cost nothing against the byte caps - and 500k is well past the file count of
// a large modpack plus its region files.
var defaultRestoreLimits = restoreLimits{
	entryBytes: 8 << 30,
	totalBytes: 64 << 30,
	entries:    500_000,
}

func untarGz(archive, dst string) error {
	return untarGzLimited(archive, dst, defaultRestoreLimits)
}

func untarGzLimited(archive, dst string, lim restoreLimits) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	root, err := filepath.Abs(dst)
	if err != nil {
		return err
	}

	var (
		total   int64
		entries int
	)

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			// tar's end is not gzip's end. The trailer holding the checksum
			// and length is only read by draining the gzip stream, so a
			// corrupt archive used to pass validation the moment the tar
			// records stopped - and its checksum failure surfaced nowhere.
			// The drain is bounded: an archive with a second member or junk
			// padding after the tar end is refused, not decompressed.
			const maxTail = int64(1 << 20)
			n, drainErr := io.Copy(io.Discard, io.LimitReader(gz, maxTail+1))
			if drainErr != nil {
				return fmt.Errorf("gzip integrity check failed: %w", drainErr)
			}
			if n > maxTail {
				return fmt.Errorf("archive carries data after the tar end marker")
			}
			return gz.Close()
		}
		if err != nil {
			return err
		}

		entries++
		if entries > lim.entries {
			return fmt.Errorf("archive holds more than %d entries; refusing to restore it", lim.entries)
		}

		// Zip-slip guard: a crafted archive must not write outside dst.
		target := filepath.Join(root, filepath.Clean("/"+hdr.Name))
		if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
			return fmt.Errorf("archive entry %q escapes the destination", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode).Perm()); err != nil {
				return err
			}
		case tar.TypeReg:
			// Both caps are checked before a byte is written: aborting halfway
			// through the bomb has already spent the disk it was aiming for.
			if hdr.Size > lim.entryBytes {
				return fmt.Errorf("archive entry %q is %s, over the %s limit for one file",
					hdr.Name, humanSize(hdr.Size), humanSize(lim.entryBytes))
			}
			if total+hdr.Size > lim.totalBytes {
				return fmt.Errorf("archive expands to more than %s; refusing to restore it",
					humanSize(lim.totalBytes))
			}
			// Free space is re-checked per entry against the filesystem being
			// written, not estimated once up front: the gzip ISIZE trailer the
			// admission check relies on wraps modulo 4 GiB and can understate
			// a large archive by exactly that much, and other processes can
			// consume the space after admission anyway.
			if free, ferr := diskFree(root); ferr == nil && free-importFreeMargin < hdr.Size {
				return fmt.Errorf("the destination does not have %s free for %s; refusing mid-archive rather than filling the disk",
					humanSize(hdr.Size+importFreeMargin), hdr.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			// Executable bits travel with the archive; setuid, setgid and
			// sticky deliberately do not.
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode).Perm())
			if err != nil {
				return err
			}
			// CopyN rather than a bare io.Copy: the cap is only a cap if the
			// write cannot outrun the size the header was vetted on, and a
			// short stream is an error rather than a silently truncated file.
			n, err := io.CopyN(out, tr, hdr.Size)
			total += n
			if err == nil && n != hdr.Size {
				err = io.ErrUnexpectedEOF
			}
			var syncErr error
			if err == nil {
				syncErr = out.Sync()
			}
			closeErr := out.Close()
			if err := errors.Join(err, syncErr, closeErr); err != nil {
				return fmt.Errorf("extract %q: %w", hdr.Name, err)
			}
		default:
			// symlinks and devices are deliberately not restored
		}
	}
}

// rollbackRestore undoes a half-installed restore: the newly-installed entries
// are removed first so restoreHeld's renames back into place cannot collide
// with them. Errors are returned: the caller must keep the staging tree when
// rollback could not finish, because it holds the only remaining copy of the
// original world.
func rollbackRestore(installed []string, held, dir string) error {
	for _, name := range installed {
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("could not remove the half-installed %s: %w", name, err)
		}
	}
	return restoreHeld(held, dir)
}

// restoreHeld puts the previous contents back after a failed restore. Errors
// are returned rather than discarded: a silently failed rename left originals
// stranded in old/ while the caller's cleanup deleted the staging tree - the
// only remaining copy - and reported the previous world restored.
func restoreHeld(held, dir string) error {
	entries, err := os.ReadDir(held)
	if err != nil {
		return fmt.Errorf("could not enumerate the held originals in %s: %w", held, err)
	}
	var failed []string
	for _, e := range entries {
		if err := os.Rename(filepath.Join(held, e.Name()), filepath.Join(dir, e.Name())); err != nil {
			failed = append(failed, e.Name())
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not restore %s; they remain held in %s", strings.Join(failed, ", "), held)
	}
	return nil
}

// hasPortLine reports whether the properties text already carries the given
// port as its server-port value.
func hasPortLine(content, port string) bool {
	for _, ln := range strings.Split(content, "\n") {
		ln = strings.TrimSpace(ln)
		if k, v, ok := strings.Cut(ln, "="); ok && strings.TrimSpace(k) == "server-port" {
			return strings.TrimSpace(v) == port
		}
	}
	return false
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func sprintf(f string, a ...any) string { return fmt.Sprintf(f, a...) }
