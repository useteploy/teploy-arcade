package arcade

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// M13. DeleteBackup ignored the per-server backup lock that CreateBackup and
// RestoreBackup both take. Removing the archive a backup is still writing
// unlinks the directory entry while tarGz keeps filling the orphaned inode: the
// create reports a size and audits a success, and nothing is left on disk. The
// lock is taken directly here because that is the state a backup in flight
// leaves behind, with no timing to lose.
func TestDeleteBackupRefusesWhileABackupIsRunning(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	b, err := mgr.CreateBackup(s, "keep me", "tester")
	if err != nil {
		t.Fatalf("backup: %v", err)
	}

	if !mgr.lockBackup(s.ID) {
		t.Fatal("could not take the backup lock")
	}
	err = mgr.DeleteBackup(s, b.ID, "tester")
	mgr.unlockBackup(s.ID)

	if err == nil {
		t.Error("a backup was deleted while one was in flight for the same server")
	}

	list, lerr := mgr.ListBackups(s)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(list) != 1 {
		t.Fatalf("ListBackups = %d entries, want 1 - the archive was unlinked", len(list))
	}
	if list[0].Note != "keep me" {
		t.Errorf("note = %q, want %q", list[0].Note, "keep me")
	}
}

// The lock must not outlive the delete, or one delete would block every later
// backup of that server for the life of the process.
func TestDeleteBackupReleasesTheLock(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	b, err := mgr.CreateBackup(s, "", "tester")
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if err := mgr.DeleteBackup(s, b.ID, "tester"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if mgr.backupLocked(s.ID) {
		t.Fatal("the backup lock is still held after DeleteBackup returned")
	}
	if _, err := mgr.CreateBackup(s, "", "tester"); err != nil {
		t.Fatalf("a backup after a delete was refused: %v", err)
	}
}

// L4. Backup IDs were second-resolution, so two backups of one server inside
// the same second shared a name: the second os.Create truncated the first
// archive and its .note, and an operator who asked for two backups was left
// with one.
func TestTwoBackupsInTheSameSecondBothSurvive(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	if err := mgr.WriteFile(s, "world/level.dat", "first"); err != nil {
		t.Fatal(err)
	}

	// Both creates have to land inside one second or the collision does not
	// exist and the test proves nothing. Starting near the top of a second is
	// enough: each backup of a seeded server takes single-digit milliseconds.
	for time.Now().Nanosecond() > int(400*time.Millisecond) {
		time.Sleep(5 * time.Millisecond)
	}

	first, err := mgr.CreateBackup(s, "first", "tester")
	if err != nil {
		t.Fatalf("first backup: %v", err)
	}
	if err := mgr.WriteFile(s, "world/level.dat", "second"); err != nil {
		t.Fatal(err)
	}
	second, err := mgr.CreateBackup(s, "second", "tester")
	if err != nil {
		t.Fatalf("second backup: %v", err)
	}

	if first.ID == second.ID {
		t.Fatalf("both backups got the id %q; the second overwrote the first", first.ID)
	}

	list, err := mgr.ListBackups(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("ListBackups = %d entries, want 2", len(list))
	}

	// The earlier archive must still hold the earlier world, not a truncated
	// copy of the later one.
	if err := mgr.RestoreBackup(s, first.ID, "tester"); err != nil {
		t.Fatalf("restoring the earlier backup: %v", err)
	}
	got, err := mgr.ReadFile(s, "world/level.dat")
	if err != nil || got != "first" {
		t.Fatalf("world/level.dat = %q (%v), want %q", got, err, "first")
	}
}

// M14. The restore path copied every entry with an unbounded io.Copy. A crafted
// archive expands until the data volume is full, which takes down the panel and
// every server sharing the disk. The refusal has to name the size limit: an
// unguarded untarGz also fails on this archive, but only by running out of tar
// stream, so asserting on the message is what makes this test able to fail.
func TestRestoreRefusesAnEntryOverTheSizeCap(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "bomb.tar.gz")
	writeOversizedArchive(t, archive, "world/region/r.0.0.mca", defaultRestoreLimits.entryBytes+1)

	dst := t.TempDir()
	err := untarGz(archive, dst)
	if err == nil {
		t.Fatal("an archive claiming an entry over the per-file cap was restored")
	}
	if !strings.Contains(err.Error(), "limit for one file") {
		t.Errorf("refused for the wrong reason: %v", err)
	}

	// The guard has to fire before anything is written, or aborting halfway has
	// already spent the disk the bomb was aiming for.
	ents, rerr := os.ReadDir(dst)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(ents) != 0 {
		t.Errorf("the destination has %d entries; nothing should have been written", len(ents))
	}
}

// The same archive through the real restore path: the refusal must leave the
// live world exactly as it was.
func TestBombRestoreLeavesTheLiveWorldAlone(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	if err := mgr.WriteFile(s, "world/level.dat", "the real world"); err != nil {
		t.Fatal(err)
	}

	dir := mgr.backupDir(s.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "20990101-000000-000-bomb"
	writeOversizedArchive(t, filepath.Join(dir, id+".tar.gz"),
		"world/region/r.0.0.mca", defaultRestoreLimits.entryBytes+1)

	if err := mgr.RestoreBackup(s, id, "tester"); err == nil {
		t.Fatal("a decompression bomb was accepted as a restore")
	}
	got, err := mgr.ReadFile(s, "world/level.dat")
	if err != nil || got != "the real world" {
		t.Fatalf("world/level.dat = %q (%v) after a refused restore, want it untouched", got, err)
	}
}

// The cumulative and entry-count caps, driven at limits a test can reach. The
// production numbers are deliberately far above any real world (8 GiB per file,
// 64 GiB per archive, 500k entries), so the mechanism is what gets exercised
// here and the numbers are asserted to be the ones untarGz uses.
func TestRestoreCapsTotalSizeAndEntryCount(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "world"), 0o755); err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("x", 4096)
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		if err := os.WriteFile(filepath.Join(src, "world", name), []byte(payload), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	archive := filepath.Join(t.TempDir(), "world.tar.gz")
	if _, err := tarGz(src, archive); err != nil {
		t.Fatal(err)
	}

	generous := restoreLimits{entryBytes: 1 << 20, totalBytes: 1 << 20, entries: 100}
	if err := untarGzLimited(archive, t.TempDir(), generous); err != nil {
		t.Fatalf("a legitimate archive was refused under generous limits: %v", err)
	}

	overTotal := generous
	overTotal.totalBytes = 10 << 10 // two of the five 4 KiB files fit, the third does not
	if err := untarGzLimited(archive, t.TempDir(), overTotal); err == nil {
		t.Error("an archive expanding past the cumulative cap was restored")
	} else if !strings.Contains(err.Error(), "expands to more than") {
		t.Errorf("refused for the wrong reason: %v", err)
	}

	overCount := generous
	overCount.entries = 3
	if err := untarGzLimited(archive, t.TempDir(), overCount); err == nil {
		t.Error("an archive over the entry-count cap was restored; empty files cost no bytes at all")
	} else if !strings.Contains(err.Error(), "entries") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
}

// writeOversizedArchive builds a gzip'd tar whose single header claims size
// bytes and carries no body. The claim is the whole point: a test that had to
// produce the bytes could not assert on a multi-gigabyte cap at all.
func writeOversizedArchive(t *testing.T, path, name string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: name, Mode: 0o644, Size: size, Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close() // owes a body it will never write; the header is already out
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// A restore must replace the world's CONTENTS, never the directory itself.
//
// serverDir resolves symlinks, so for a server adopted in place it returns the
// operator's own tree. The previous shape renamed that directory aside,
// recreated it and deleted the original - destroying a directory outside the
// panel and breaking the symlink pointing at it.
func TestRestoreDoesNotReplaceAnAdoptedDirectory(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	// Stand in for an adopted server: the panel's path is a symlink to a tree
	// the operator owns.
	owned := t.TempDir()
	if err := os.WriteFile(filepath.Join(owned, "server.properties"), []byte("motd=mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(owned, "world"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owned, "world", "level.dat"), []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(mgr.dataDir, "servers", s.ID)
	_ = os.RemoveAll(link)
	if err := os.Symlink(owned, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	b, err := mgr.CreateBackup(s, "before", "tester")
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(owned, "world", "level.dat"), []byte("ruined"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := mgr.RestoreBackup(s, b.ID, "tester"); err != nil {
		t.Fatalf("restore: %v", err)
	}

	// The operator's directory must still exist, still be the symlink target,
	// and now hold the restored contents.
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the panel's symlink was replaced by a real directory: %v", err)
	}
	if _, err := os.Stat(owned); err != nil {
		t.Fatalf("the operator's own directory was destroyed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(owned, "world", "level.dat"))
	if err != nil || string(got) != "original" {
		t.Errorf("world not restored in place: %q %v", got, err)
	}
	if _, err := os.Stat(owned + ".pre-restore"); err == nil {
		t.Error("a .pre-restore copy of the operator's directory was left beside it")
	}
}

// Deleting a plugin that was never installed must not report success.
// DeletePath is os.RemoveAll underneath, which is nil for a missing path.
func TestDeletingAnAbsentPluginIsAnError(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	if err := mgr.DeletePlugin(s, "never-installed.jar"); err == nil {
		t.Error("deleting a plugin that does not exist reported success")
	}

	// A real one still deletes.
	if err := mgr.WriteFile(s, "plugins/real.jar", "PK\x03\x04"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.DeletePlugin(s, "real.jar"); err != nil {
		t.Errorf("deleting an installed plugin failed: %v", err)
	}
	if _, err := mgr.StatRel(s, "plugins/real.jar"); err == nil {
		t.Error("the plugin is still on disk after a successful delete")
	}
}

// Concurrent imports must not all claim the same port. A copy import registers
// its server minutes later on a background goroutine, so a bare portOwner check
// let every simultaneous import pass.
func TestConcurrentImportsCannotClaimOnePort(t *testing.T) {
	_, mgr := newTestAgent(t)

	const port = 25800
	const n = 8
	var mu sync.Mutex
	var granted int

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, ok := mgr.claimPort(port, fmt.Sprintf("import-%d", i)); ok {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if granted != 1 {
		t.Errorf("%d of %d concurrent imports claimed port %d; exactly one may", granted, n, port)
	}

	// And releasing must hand it back, or a failed import blocks the port forever.
	mgr.releasePort(port)
	if _, ok := mgr.claimPort(port, "later"); !ok {
		t.Error("a released port could not be claimed again")
	}
}
