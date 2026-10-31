package arcade

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

// chownCall is one handover the panel would have made. tree is true for the
// Lchown seam, which only chownTree uses: a whole tree being given away, as
// opposed to one replaced file keeping its owner.
type chownCall struct {
	path     string
	uid, gid int
	tree     bool
}

// withChownRecorder makes the process look like root and records what it would
// have handed over, so the logic is testable without actually being root.
// Both seams are recorded: regular files go through Chown, and since R13
// (audit pass 7) tree repair goes through Lchown so a symlink in the tree
// changes its own inode, never its target.
func withChownRecorder(t *testing.T) *[]chownCall {
	t.Helper()
	calls := &[]chownCall{}
	oldChown, oldLchown, oldEuid := chownFile, chownLink, geteuid
	t.Cleanup(func() { chownFile, chownLink, geteuid = oldChown, oldLchown, oldEuid })
	geteuid = func() int { return 0 }
	chownFile = func(path string, uid, gid int) error {
		*calls = append(*calls, chownCall{path, uid, gid, false})
		return nil
	}
	chownLink = func(path string, uid, gid int) error {
		*calls = append(*calls, chownCall{path, uid, gid, true})
		return nil
	}
	return calls
}

// handToGameUser gives path to the container's user for real when the test runs
// as root, which is the deployed state of a docker server's files. Unprivileged,
// the test's own non-root user already owns it, and that answers the same
// question: the file belongs to someone other than root.
func handToGameUser(t *testing.T, path string) {
	t.Helper()
	if os.Geteuid() != 0 {
		return
	}
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, containerRunUID, containerRunGID)
	})
	if err != nil {
		t.Fatalf("handing %s to the game user: %v", path, err)
	}
}

func TestAtomicWriteKeepsTheGameAsOwnerOfItsFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "server.properties")
	if err := os.WriteFile(target, []byte("server-port=25565\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	handToGameUser(t, target)
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
	if got.uid != uid || got.gid != gid {
		t.Errorf("handed to %d:%d, want %d:%d", got.uid, got.gid, uid, gid)
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
		if c.uid != containerRunUID || c.gid != containerRunGID {
			t.Errorf("%s handed to %d:%d, want %d:%d", c.path, c.uid, c.gid, containerRunUID, containerRunGID)
		}
		rel, _ := filepath.Rel(root, c.path)
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
	handToGameUser(t, filepath.Join(mgr.dataDir, "servers", s.ID))
	// Read before the save: the file being replaced is what decides, and with
	// the chown recorded rather than made, the replacement on disk is the
	// writer's.
	uid, gid, ok := fileOwner(filepath.Join(mgr.dataDir, "servers", s.ID, "server.properties"))
	if !ok {
		t.Skip("no ownership information on this filesystem")
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
	for _, h := range handed {
		if h[1] != uid || h[2] != gid {
			t.Errorf("%v handed to %v:%v, want %d:%d", h[0], h[1], h[2], uid, gid)
		}
	}
}

// The first attempt at this landed in seed(), which only ever makes simulator
// servers, so the guard was never true and the fix was dead code. The existing
// helper test could not catch that - it calls chownTree directly. This drives
// Manager.Create, which is the route a server is actually made by.
func TestCreatingADockerServerHandsTheTreeToTheGameUser(t *testing.T) {
	mgr := NewManager(t.TempDir(), NewHub())
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}

	oldReach := dockerReachable
	t.Cleanup(func() { dockerReachable = oldReach })
	dockerReachable = func() bool { return true }

	calls := withChownRecorder(t)
	s, err := mgr.Create("Handover", "paper", "1.20.4", 25599, 0, 0, RuntimeDocker)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Scoped to the server's own tree. The same run also rewrites servers.json,
	// which is panel state and correctly keeps the data directory's owner - a
	// create must not hand the panel's ledger to the game. Compared against
	// that owner rather than against the game's uid, because the user running
	// the suite can itself be uid 1000.
	panelUID, panelGID, ok := fileOwner(mgr.dataDir)
	if !ok {
		t.Skip("no ownership information on this filesystem")
	}
	want := filepath.Join(mgr.dataDir, "servers", s.ID)
	var sawRoot bool
	for _, c := range *calls {
		if !strings.HasPrefix(c.path, want) {
			if c.tree || c.uid != panelUID || c.gid != panelGID {
				t.Errorf("%s is outside the server tree but was handed to %d:%d (tree=%v); "+
					"panel state keeps the data directory's owner %d:%d",
					c.path, c.uid, c.gid, c.tree, panelUID, panelGID)
			}
			continue
		}
		if c.uid != containerRunUID || c.gid != containerRunGID {
			t.Errorf("%s handed to %d:%d, want %d:%d", c.path, c.uid, c.gid, containerRunUID, containerRunGID)
		}
		if c.path == want {
			sawRoot = true
		}
	}
	if !sawRoot {
		t.Fatalf("a new docker server's tree was never handed to uid %d; it would fail on its first start "+
			"the way Lobby did. chowned: %v", containerRunUID, *calls)
	}
}

// A simulator server runs in-process as the panel's own user, so handing its
// files to uid 1000 would take them away from the only process that reads them.
func TestCreatingASimulatorServerDoesNotChangeOwnership(t *testing.T) {
	mgr := NewManager(t.TempDir(), NewHub())
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	panelUID, panelGID, ok := fileOwner(mgr.dataDir)
	if !ok {
		t.Skip("no ownership information on this filesystem")
	}
	calls := withChownRecorder(t)
	if _, err := mgr.Create("Sim", "paper", "1.20.4", 25601, 0, 0, RuntimeSim); err != nil {
		t.Fatalf("create: %v", err)
	}
	// No tree handover at all, and every replaced file keeps the panel's own
	// owner. Both, because the suite's user can be uid 1000 itself, which
	// makes a handover to the container user indistinguishable by uid alone.
	for _, c := range *calls {
		if c.tree {
			t.Fatalf("a simulator server's tree was handed over: %+v", c)
		}
		if c.uid != panelUID || c.gid != panelGID {
			t.Fatalf("%s was handed to %d:%d; the simulator runs as the panel's own %d:%d",
				c.path, c.uid, c.gid, panelUID, panelGID)
		}
	}
}
