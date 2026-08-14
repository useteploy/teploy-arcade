package arcade

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A backup id reaches the filesystem as a path component. The guard it used to
// have (contains a separator, does not start with "20") let plenty through:
// "..%2f" style names, an id with a NUL, an absolute path. The charset check
// that replaced it is only worth having if it is actually enforced on both the
// restore and delete paths, so both are exercised here.
func TestValidBackupIDCharset(t *testing.T) {
	ok := []string{"20240101-000000", "20240101-000000-1", "abcXYZ-09", "a"}
	bad := []string{
		"", "..", ".", "../escape", "a/b", `a\b`, "a b", "a;b", "a~b", "a*b",
		"a\x00b", "a\nb", "20240101-000000/../../escape", strings.Repeat("a", 129),
	}
	for _, id := range ok {
		if !validBackupID(id) {
			t.Errorf("validBackupID(%q) = false, want true", id)
		}
	}
	for _, id := range bad {
		if validBackupID(id) {
			t.Errorf("validBackupID(%q) = true, want false", id)
		}
	}
}

// Two layers hold this property: validBackupID rejects the id, and
// filepath.Base flattens whatever survives. Base is the load-bearing one -
// disabling validBackupID alone changes nothing observable, which is worth
// knowing before anyone "simplifies" the Base call away.
//
// So this asserts the property rather than either mechanism, and it does so by
// planting a real archive where a traversal id resolves to. Asserting only
// "restore returned an error" would be vacuous: a crafted id errors anyway
// because no such archive exists, and the test would pass with both guards
// deleted. TestValidBackupIDCharset covers the charset rule on its own.
func TestCraftedBackupIDsCannotReachOutsideTheBackupDirectory(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	if err := mgr.WriteFile(s, "world/level.dat", "the real world"); err != nil {
		t.Fatal(err)
	}
	// A genuine archive, so a successful escape would actually restore.
	real, err := mgr.CreateBackup(s, "source", "tester")
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	archive, err := os.ReadFile(filepath.Join(mgr.backupDir(s.ID), real.ID+".tar.gz"))
	if err != nil {
		t.Fatal(err)
	}

	// backupDir is <data>/backups/<server>, so "../../escape" resolves here.
	escape := filepath.Join(mgr.dataDir, "escape.tar.gz")
	if err := os.WriteFile(escape, archive, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := mgr.WriteFile(s, "world/level.dat", "current"); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"../../escape", "../../../etc/passwd", "..", ".", "", "a b", "a;b", strings.Repeat("a", 129)} {
		if err := mgr.RestoreBackup(s, id, "tester"); err == nil {
			t.Errorf("RestoreBackup accepted crafted id %q", id)
		}
		if err := mgr.DeleteBackup(s, id, "tester"); err == nil {
			t.Errorf("DeleteBackup accepted crafted id %q", id)
		}
	}

	// The two things that prove the guard, not a missing-file error, did the work.
	if _, err := os.Stat(escape); err != nil {
		t.Errorf("an archive outside the backup directory was deleted: %v", err)
	}
	got, err := mgr.ReadFile(s, "world/level.dat")
	if err != nil {
		t.Fatal(err)
	}
	if got != "current" {
		t.Errorf("the world was restored from outside the backup directory: %q", got)
	}
}

// A real id must still work, or the guard above is just an outage.
func TestARealBackupIDStillRestores(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	if err := mgr.WriteFile(s, "world/level.dat", "original"); err != nil {
		t.Fatal(err)
	}
	b, err := mgr.CreateBackup(s, "note", "tester")
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if !validBackupID(b.ID) {
		t.Fatalf("the id we generate is rejected by our own guard: %q", b.ID)
	}
	if err := mgr.WriteFile(s, "world/level.dat", "changed"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.RestoreBackup(s, b.ID, "tester"); err != nil {
		t.Fatalf("restore of a legitimate id: %v", err)
	}
	got, err := mgr.ReadFile(s, "world/level.dat")
	if err != nil || got != "original" {
		t.Errorf("restore did not bring the file back: %q %v", got, err)
	}
}

// The size in a Backup record is what an operator reads to decide the backup
// worked. It is taken after the gzip and tar writers are flushed and the file
// is closed, so it must equal the archive actually on disk - not a count of
// bytes handed to a writer that still had a buffer to flush.
func TestBackupSizeMatchesTheArchiveOnDisk(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	// Enough compressible content that a discarded flush would show up as a
	// difference rather than hiding inside one gzip block.
	for i := 0; i < 40; i++ {
		body := strings.Repeat("region data ", 500)
		if err := mgr.WriteFile(s, filepath.Join("world", "r."+string(rune('a'+i%26))+".mca"), body); err != nil {
			t.Fatal(err)
		}
	}

	b, err := mgr.CreateBackup(s, "sized", "tester")
	if err != nil {
		t.Fatalf("backup: %v", err)
	}

	fi, err := os.Stat(filepath.Join(mgr.backupDir(s.ID), b.ID+".tar.gz"))
	if err != nil {
		t.Fatalf("stat archive: %v", err)
	}
	if b.Size != fi.Size() {
		t.Errorf("recorded size %d, archive on disk is %d: the record was taken before the writers flushed",
			b.Size, fi.Size())
	}
	if b.Size == 0 {
		t.Error("a backup of a populated world came out empty")
	}
	if b.InProgress {
		t.Error("a finished backup is still flagged in progress")
	}

	// And the listing agrees with the record.
	list, err := mgr.ListBackups(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) == 0 {
		t.Fatal("the backup we just took is not listed")
	}
	if list[0].Size != fi.Size() {
		t.Errorf("listing reports %d, archive is %d", list[0].Size, fi.Size())
	}
}

// While tarGz is still writing, the archive is on disk at a partial size. The
// listing has to say so, because an operator reading "2 KB" for a 400 MB world
// concludes the backup is broken - or worse, reads a plausible-looking number
// and concludes it is fine.
func TestAnInFlightBackupIsNotReportedAsFinished(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	// Stand where CreateBackup stands mid-archive: lock held, current id set,
	// a partial file on disk.
	if !mgr.lockBackup(s.ID) {
		t.Fatal("could not take the backup lock")
	}
	const id = "20240101-000000-partial"
	mgr.markBackupInFlight(s.ID, id)
	dir := mgr.backupDir(s.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".tar.gz"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}

	list, err := mgr.ListBackups(s)
	mgr.unlockBackup(s.ID)
	if err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, b := range list {
		if b.ID != id {
			continue
		}
		found = true
		if !b.InProgress {
			t.Error("an archive still being written is listed as a finished backup")
		}
	}
	if !found {
		t.Fatalf("the in-flight archive is missing from the listing entirely: %+v", list)
	}
}
