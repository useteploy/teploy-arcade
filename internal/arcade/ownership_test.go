package arcade

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// Saving a setting from the panel made the server unstartable.
//
// The panel runs as root and the game runs as uid 1000. writeFileAtomic
// replaces a file with a brand new inode owned by the writer, so one settings
// save handed root ownership of server.properties to a file the container has
// to write, and the next start died before the world loaded:
//
//	java.nio.file.AccessDeniedException: /data/server.properties
//	[init] [ERROR] Failed to update server.properties
//	panel Server exited 1
//
// Nothing in the panel noticed, because the write itself succeeded. On the
// deployed host it was visible only as one file out of eight owned by 0:0.

// withChownRecorder makes the process look like root and records what it would
// have handed over, so the logic is testable without actually being root.
func withChownRecorder(t *testing.T) *[][3]any {
	t.Helper()
	calls := &[][3]any{}
	oldChown, oldEuid := chownFile, geteuid
	t.Cleanup(func() { chownFile, geteuid = oldChown, oldEuid })
	geteuid = func() int { return 0 }
	chownFile = func(path string, uid, gid int) error {
		*calls = append(*calls, [3]any{path, uid, gid})
		return nil
	}
	return calls
}

func TestAtomicWriteKeepsTheGameAsOwnerOfItsFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "server.properties")
	if err := os.WriteFile(target, []byte("server-port=25565\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	uid, gid, ok := fileOwner(target)
	if !ok {
		t.Skip("no ownership information on this filesystem")
	}

	calls := withChownRecorder(t)
	if err := writeFileAtomic(target, []byte("server-port=25566\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if len(*calls) != 1 {
		t.Fatalf("the replacement was not handed back to the file's owner: %v", *calls)
	}
	got := (*calls)[0]
	if got[1] != uid || got[2] != gid {
		t.Errorf("handed to %v:%v, want %d:%d", got[1], got[2], uid, gid)
	}
}

// The panel's own state lives in a root-owned directory and must stay there:
// servers.json, users.json and audit.json go through the same helper.
func TestAtomicWriteLeavesRootOwnedStateAlone(t *testing.T) {
	calls := withChownRecorder(t)

	// "/" is root-owned on every host this ships to, so a file landing beside
	// it inherits root - which means: do nothing.
	preserveOwner(filepath.Join(t.TempDir(), "tmpfile"), "/no-such-panel-state-file")

	if len(*calls) != 0 {
		t.Fatalf("root-owned state was given away: %v", *calls)
	}
}

// A server the panel creates is seeded by root into a directory root made, so
// every file in it is root's and the container - uid 1000, no --user flag -
// cannot write one of them.
func TestANewTreeIsHandedToTheContainerUser(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"server.properties", "plugins/.keep", "logs/.keep"} {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	calls := withChownRecorder(t)
	chownTree(root, containerRunUID, containerRunGID)

	var got []string
	for _, c := range *calls {
		if c[1] != containerRunUID || c[2] != containerRunGID {
			t.Errorf("%v handed to %v:%v, want %d:%d", c[0], c[1], c[2], containerRunUID, containerRunGID)
		}
		rel, _ := filepath.Rel(root, c[0].(string))
		got = append(got, rel)
	}
	sort.Strings(got)
	want := []string{".", "logs", "logs/.keep", "plugins", "plugins/.keep", "server.properties"}
	if len(got) != len(want) {
		t.Fatalf("chowned %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("chowned %v, want %v", got, want)
		}
	}
}

// The path that actually writes server.properties is the Root-confined one,
// not writeFileAtomic - which is why the first version of this fix changed
// nothing on the deployed host: the test passed, the file still came back
// owned by root. This asserts the path the panel really takes.
func TestSavingSettingsKeepsTheGameAsOwner(t *testing.T) {
	mgr := NewManager(t.TempDir(), NewHub())
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	s := mgr.List()[0]
	if _, err := mgr.ensureServerDir(s); err != nil {
		t.Fatalf("server dir: %v", err)
	}
	if err := mgr.writeProps(s); err != nil {
		t.Fatalf("seed props: %v", err)
	}

	oldRootChown, oldEuid := rootChown, geteuid
	t.Cleanup(func() { rootChown, geteuid = oldRootChown, oldEuid })
	geteuid = func() int { return 0 }
	var handed [][3]any
	rootChown = func(r *os.Root, name string, uid, gid int) error {
		handed = append(handed, [3]any{name, uid, gid})
		return nil
	}

	if _, err := mgr.ApplySettings(s, map[string]string{"motd": "ownership check"}); err != nil {
		t.Fatalf("apply settings: %v", err)
	}

	if len(handed) == 0 {
		t.Fatal("saving a setting replaced server.properties without handing it back to the game's user")
	}
	uid, gid, ok := fileOwner(filepath.Join(mgr.dataDir, "servers", s.ID, "server.properties"))
	if !ok {
		t.Skip("no ownership information on this filesystem")
	}
	for _, h := range handed {
		if h[1] != uid || h[2] != gid {
			t.Errorf("%v handed to %v:%v, want %d:%d", h[0], h[1], h[2], uid, gid)
		}
	}
}
