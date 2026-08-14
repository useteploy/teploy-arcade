package arcade

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// H10. WriteFile writes through <name>.tmp, and that path was never checked
// against the sandbox. A symlink planted at it - by a plugin, or by the game
// process, which is the actor the file manager's security model exists to
// contain - redirected the write anywhere on disk, and the rename afterwards
// left the trusted name pointing at the attacker's target.
// Renamed and re-aimed. It used to assert the write was REFUSED when a
// "<name>.tmp" symlink was planted, which tested the mechanism (O_NOFOLLOW)
// rather than the property. Refusing is also a denial of service: planting
// notes.txt.tmp blocked every future write to notes.txt.
//
// The temp name is now random (os.CreateTemp, O_CREATE|O_EXCL), so a planted
// name cannot be targeted at all. The write succeeds AND stays inside the
// sandbox, which is strictly better. What must hold is unchanged: nothing
// outside the server directory is touched, and the published file is a regular
// file rather than a symlink.
func TestWriteFileIgnoresAPlantedTempSymlink(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]
	dir, err := mgr.ensureServerDir(s)
	if err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("untouched"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "notes.txt.tmp")); err != nil {
		t.Skipf("symlinks are unavailable here: %v", err)
	}

	if err := mgr.WriteFile(s, "notes.txt", "legitimate content"); err != nil {
		t.Fatalf("a planted temp name must not block a legitimate write: %v", err)
	}
	if got, err := mgr.ReadFile(s, "notes.txt"); err != nil || got != "legitimate content" {
		t.Errorf("notes.txt = %q %v, want the content just written", got, err)
	}
	if b, err := os.ReadFile(outside); err != nil || string(b) != "untouched" {
		t.Errorf("a file outside the server directory was overwritten: %q %v", b, err)
	}
	if fi, err := os.Lstat(filepath.Join(dir, "notes.txt")); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		t.Error("the rename published the symlink under the trusted name")
	}
}

// M16. resolve only ran EvalSymlinks over the whole path, which fails outright
// when the leaf does not exist yet - so on a create the parent directories went
// unchecked. A directory symlink planted inside the tree turned every new file
// under it into a write outside the server directory, with the path still
// looking like one inside it.
func TestCreateThroughAPlantedDirectorySymlinkIsRefused(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]
	dir, err := mgr.ensureServerDir(s)
	if err != nil {
		t.Fatal(err)
	}

	outsideDir := t.TempDir()
	if err := os.Symlink(outsideDir, filepath.Join(dir, "linked")); err != nil {
		t.Skipf("symlinks are unavailable here: %v", err)
	}

	if err := mgr.WriteFile(s, "linked/evil.txt", "pwned"); err == nil {
		t.Error("a write through a directory symlink was accepted")
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "evil.txt")); err == nil {
		t.Error("the write landed outside the server directory")
	}
	if err := mgr.MkDir(s, "linked/evil"); err == nil {
		t.Error("a mkdir through a directory symlink was accepted")
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "evil")); err == nil {
		t.Error("the mkdir landed outside the server directory")
	}
}

// H9. writeProps truncated server.properties and then filled it, so a reader in
// that window - or a crash in it - saw an empty or half-written file. The game
// refuses to boot on a malformed one, and every configured setting goes with it.
func TestPropsAreNeverVisiblyTruncated(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	// Enough keys that the truncate-then-fill window is wide enough to lose a
	// race with, instead of hiding inside one small write.
	s.mu.Lock()
	for i := 0; i < 4000; i++ {
		s.Props[fmt.Sprintf("filler-%04d", i)] = strings.Repeat("x", 40)
	}
	s.mu.Unlock()

	if err := mgr.writeProps(s); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(mgr.serverDir(s), "server.properties")
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for i := 0; i < 200; i++ {
			if err := mgr.writeProps(s); err != nil {
				t.Errorf("writeProps: %v", err)
				return
			}
		}
	}()
	defer wg.Wait()

	for {
		select {
		case <-stop:
			return
		default:
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("server.properties was unreadable mid-write: %v", err)
		}
		if len(b) != len(full) {
			t.Fatalf("a reader saw %d of %d bytes; the file was truncated in place",
				len(b), len(full))
		}
	}
}

// L5. The old model checked a path string and then made the syscall on it, so
// a symlink planted in the gap escaped the root; openNoFollow refused a final
// symlink to narrow that. Both are gone: operations run against an *os.Root, so
// a link that leaves the root is refused by the syscall that would follow it.
//
// This replaces TestOpenNoFollowRefusesASymlink, which tested that helper
// directly. The behaviour deliberately changed: a symlink pointing *inside* the
// server directory is now followed rather than refused, because it cannot
// escape and refusing it broke nothing an operator wanted. What must still be
// refused is anything that leaves.
func TestSymlinksOutOfTheServerDirectoryAreRefused(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]
	dir, err := mgr.ensureServerDir(s)
	if err != nil {
		t.Fatal(err)
	}

	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "escape.txt")); err != nil {
		t.Skipf("symlinks are unavailable here: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escapedir")); err != nil {
		t.Skip("symlinks are unavailable here")
	}
	// A link that stays inside the tree, to prove the refusal is about escaping
	// and not about symlinks in general.
	if err := mgr.WriteFile(s, "real.txt", "inside"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(dir, "internal.txt")); err != nil {
		t.Skip("symlinks are unavailable here")
	}

	if got, err := mgr.ReadFile(s, "escape.txt"); err == nil {
		t.Errorf("read through an escaping symlink returned %q", got)
	}
	if got, err := mgr.ReadFile(s, "escapedir/secret.txt"); err == nil {
		t.Errorf("read through an escaping directory symlink returned %q", got)
	}
	if err := mgr.WriteFile(s, "escapedir/planted.txt", "x"); err == nil {
		t.Error("a write landed through an escaping directory symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, "planted.txt")); err == nil {
		t.Error("a file was created outside the server directory")
	}
	if err := mgr.DeletePath(s, "escapedir/secret.txt"); err == nil {
		t.Error("a delete reached outside the server directory")
	}
	if _, err := os.Stat(secret); err != nil {
		t.Errorf("a file outside the server directory was destroyed: %v", err)
	}

	// The internal link still reads, and the regular file it points at too.
	if got, err := mgr.ReadFile(s, "internal.txt"); err != nil || got != "inside" {
		t.Errorf("a symlink inside the tree should still read: %q %v", got, err)
	}
	if got, err := mgr.ReadFile(s, "real.txt"); err != nil || got != "inside" {
		t.Errorf("a regular file should still read: %q %v", got, err)
	}
}

// L6. reloadProps discarded the Save error, so an edit that could not be
// persisted still looked applied: the panel and servers.json disagree until a
// restart quietly reverts the change and nobody knows why.
func TestPropsReloadReportsAFailedSave(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	content, err := mgr.ReadFile(s, "server.properties")
	if err != nil {
		t.Fatal(err)
	}

	// A directory where servers.json belongs: Save's rename cannot replace it,
	// so the save fails for real rather than through a test-only hook.
	if err := os.Remove(mgr.storePath()); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Mkdir(mgr.storePath(), 0o755); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	mgr.reloadProps(s, content)

	if !strings.Contains(buf.String(), "server.properties") {
		t.Errorf("a state save that failed after a properties edit was silent; log held %q", buf.String())
	}
}

// The seed writes stat-then-wrote and dropped the error, so a create that could
// not land left a server the game refuses to boot while the panel reported
// success - and a symlink left in the server directory sent the seed content
// outside it, because stat follows links and the write followed it too.
func TestSeedFilesDoNotFollowAPlantedSymlink(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]
	dir, err := mgr.ensureServerDir(s)
	if err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(t.TempDir(), "planted.txt")
	if err := os.Remove(filepath.Join(dir, "eula.txt")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "eula.txt")); err != nil {
		t.Skipf("symlinks are unavailable here: %v", err)
	}

	if err := mgr.seedServerFiles(s); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Error("a seeded file was written through a symlink, outside the server directory")
	}
}
