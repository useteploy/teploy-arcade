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
	sort.Slice(out, func(i, j int) bool { return out[i].Created > out[j].Created })
	return out, nil
}

// CreateBackup runs the full quiesce -> archive -> resume cycle.
func (m *Manager) CreateBackup(s *Server, note, actor string) (*Backup, error) {
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

	wasRunning := s.State() == StatusRunning

	if wasRunning {
		m.panelLine(s, "info", "Backup starting - pausing world saves and flushing to disk.")
		_ = m.runnerFor(s).Send(s, "save-off")
		_ = m.runnerFor(s).Send(s, "save-all flush")
		// Give the game a moment to actually flush before we read the tree.
		time.Sleep(1500 * time.Millisecond)
	}

	// Resume saves no matter how the archive goes - a server left with
	// save-off is a far worse outcome than a failed backup.
	defer func() {
		if wasRunning {
			_ = m.runnerFor(s).Send(s, "save-on")
			m.panelLine(s, "info", "Backup finished - world saves resumed.")
		}
	}()

	// The ID used to be second-resolution, so two backups of one server inside
	// the same second produced the same name: the second os.Create truncated
	// the first archive and its .note, and the operator who asked for two
	// backups silently ended up with one. Milliseconds separate them, and
	// tarGz's O_EXCL is what makes a clash impossible rather than merely
	// unlikely - a name already on disk is never reopened for writing. The
	// retry re-stamps instead of failing a backup over a name.
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
		_ = os.Remove(dst) // never leave a half-written archive that looks valid
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

	return &Backup{ID: id, Server: s.ID, Name: id + ".tar.gz", Size: size,
		Created: time.Now().Unix(), Note: note}, nil
}

// RestoreBackup replaces the server directory with an archive's contents. The
// server must be stopped: restoring under a running game would have the process
// writing into a tree being replaced beneath it.
func (m *Manager) RestoreBackup(s *Server, backupID, actor string) error {
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
	if _, err := os.Stat(archive); err != nil {
		return fmt.Errorf("no such backup")
	}

	dir, err := m.ensureServerDir(s)
	if err != nil {
		return err
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
	staging, err := os.MkdirTemp(m.dataDir, "restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	if err := untarGz(archive, staging); err != nil {
		return fmt.Errorf("restore failed, the live world was not touched: %w", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	held := filepath.Join(staging, ".previous")
	if err := os.MkdirAll(held, 0o700); err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.Rename(filepath.Join(dir, e.Name()), filepath.Join(held, e.Name())); err != nil {
			// Put back whatever moved, so a partial failure is not a wipe.
			restoreHeld(held, dir)
			return fmt.Errorf("could not clear the live world, nothing was changed: %w", err)
		}
	}

	staged, err := os.ReadDir(staging)
	if err != nil {
		restoreHeld(held, dir)
		return err
	}
	for _, e := range staged {
		if e.Name() == ".previous" {
			continue
		}
		if err := os.Rename(filepath.Join(staging, e.Name()), filepath.Join(dir, e.Name())); err != nil {
			restoreHeld(held, dir)
			return fmt.Errorf("restore failed part way, the previous world was put back: %w", err)
		}
	}

	// Extracted by root into staging and moved in, so every restored file is
	// root's while the game runs as uid 1000 - a restore would hand back a
	// world the server cannot write. The directory itself was never replaced,
	// so it still carries the ownership everything under it should have.
	chownTreeLike(dir, dir)

	// The archive carries its own server.properties; adopt it.
	if content, err := os.ReadFile(filepath.Join(dir, "server.properties")); err == nil {
		m.reloadProps(s, string(content))
	}

	m.audit(actor, "backup.restore", s.ID, backupID)
	m.broadcastEvent("backup.restored", s.ID)
	return nil
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

// tarGz refuses to open an existing dst (O_EXCL). os.Create truncated whatever
// was already there, so a repeated backup ID destroyed the earlier archive
// silently; the caller re-stamps the ID on ErrExist instead.
func tarGz(src, dst string) (int64, error) {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	// Kept for the panic and early-error paths; the success path closes
	// explicitly below and checks the error. A double Close is harmless.
	defer f.Close()

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	err = filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// Never archive a symlink's target; skip links entirely.
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(tw, in)
		return err
	})

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
			return nil
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
			if err := os.MkdirAll(target, 0o755); err != nil {
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
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			// LimitReader rather than a bare io.Copy: the cap is only a cap if
			// the write cannot outrun the size the header was vetted on.
			n, err := io.Copy(out, io.LimitReader(tr, hdr.Size))
			total += n
			if err != nil {
				out.Close()
				return err
			}
			out.Close()
		default:
			// symlinks and devices are deliberately not restored
		}
	}
}

// restoreHeld puts the previous contents back after a failed restore. Best
// effort by design: the alternative to a partial recovery is none at all.
func restoreHeld(held, dir string) {
	entries, err := os.ReadDir(held)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.Rename(filepath.Join(held, e.Name()), filepath.Join(dir, e.Name()))
	}
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
