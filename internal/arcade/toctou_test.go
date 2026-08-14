package arcade

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// L5, the actual defect: the file API used to check a path string and then make
// the syscall on it. A server directory is writable by the game process and its
// plugins, so a directory component could be swapped for a symlink in the gap
// between the two, and the operation landed outside the tree the check had just
// approved.
//
// This drives that race directly: one goroutine flips `plugins` between a real
// directory and a symlink pointing outside, while others write, read, list and
// delete through it. Nothing may ever appear outside the server directory, and
// nothing outside may be read or removed.
//
// It is a race, so it is not guaranteed to catch a regression on any single
// run - but with os.Root there is no window to lose, and reverting to the
// resolve-then-operate model fails it reliably (see BUGS.md L5).
func TestPathOperationsCannotBeRacedOutOfTheServerDirectory(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]
	dir, err := mgr.ensureServerDir(s)
	if err != nil {
		t.Fatal(err)
	}

	outside := t.TempDir()
	bait := filepath.Join(outside, "bait.txt")
	if err := os.WriteFile(bait, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}

	plugins := filepath.Join(dir, "plugins")
	_ = os.RemoveAll(plugins)
	if err := os.Symlink(outside, plugins); err != nil {
		t.Skipf("symlinks are unavailable here: %v", err)
	}
	_ = os.Remove(plugins)

	const rounds = 120
	var workers, attacker sync.WaitGroup
	stop := make(chan struct{})

	// The attacker: flip `plugins` between a real directory and a link out.
	// Deliberately NOT in the workers' WaitGroup - it runs until told to stop,
	// and waiting on it before closing stop is a deadlock.
	attacker.Add(1)
	go func() {
		defer attacker.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.RemoveAll(plugins)
			_ = os.Mkdir(plugins, 0o755)
			_ = os.RemoveAll(plugins)
			_ = os.Symlink(outside, plugins)
			_ = os.Remove(plugins)
		}
	}()

	// The panel, doing ordinary work through the file API.
	for i := 0; i < 4; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for n := 0; n < rounds; n++ {
				_ = mgr.WriteFile(s, "plugins/planted.jar", "PK\x03\x04")
				_, _ = mgr.ReadFile(s, "plugins/bait.txt")
				_, _ = mgr.ListFiles(s, "plugins")
				_ = mgr.MkDir(s, "plugins/sub")
				_ = mgr.DeletePath(s, "plugins/bait.txt")
				_, _, _, _ = mgr.OpenForDownload(s, "plugins/bait.txt")
			}
		}()
	}

	workers.Wait()
	close(stop)
	attacker.Wait()

	// The bait must be untouched, and nothing may have been created beside it.
	if _, err := os.Stat(bait); err != nil {
		t.Errorf("a file outside the server directory was deleted: %v", err)
	}
	ents, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.Name() != "bait.txt" {
			t.Errorf("%q was created outside the server directory", e.Name())
		}
	}
}

// The same property for the plugin API, which reaches the filesystem by its own
// route and was the site the original fix missed.
func TestPluginInstallCannotEscapeThroughASwappedDirectory(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]
	dir, err := mgr.ensureServerDir(s)
	if err != nil {
		t.Fatal(err)
	}

	outside := t.TempDir()
	plugins := filepath.Join(dir, "plugins")
	_ = os.RemoveAll(plugins)
	if err := os.Symlink(outside, plugins); err != nil {
		t.Skipf("symlinks are unavailable here: %v", err)
	}

	// Every plugin operation must refuse while `plugins` points out of the tree.
	// A plant to notice: if any operation follows the link, this is what it
	// would list, read or remove.
	if err := os.WriteFile(filepath.Join(outside, "outside.jar"), []byte("PK\x03\x04"), 0o644); err != nil {
		t.Fatal(err)
	}

	v, err := mgr.PluginView(s)
	if err == nil {
		for _, e := range v.Entries {
			if e.File == "outside.jar" {
				t.Error("plugin listing reached outside the server directory")
			}
		}
	}
	if _, err := mgr.SetPluginEnabled(s, "outside.jar", false); err == nil {
		t.Error("plugin toggle reached through an escaping directory symlink")
	}
	if err := mgr.DeletePlugin(s, "outside.jar"); err == nil {
		t.Error("plugin delete reached through an escaping directory symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, "outside.jar")); err != nil {
		t.Errorf("a jar outside the server directory was removed: %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("the directory outside was destroyed: %v", err)
	}
}
