package arcade

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// M10. AddToList read s.State(), and only then wrote the list file, with no lock
// held across the pair. Manager.Start commits its own check-then-transition
// under m.lifecycle, so a start could land in that gap: the panel wrote
// whitelist.json believing the server was down, the game booted on the copy it
// read a moment earlier and rewrote the file on its next save, and the operator's
// change was gone with nothing logged anywhere. This is the exact failure the
// running/stopped split at the top of players.go exists to prevent.
//
// Holding m.lifecycle here stands in for a start that has committed but not yet
// flipped the status. The write must not be able to proceed underneath it.
func TestPlayerListWriteCannotRaceTheStartTransition(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	mgr.lifecycle.Lock()
	added := make(chan error, 1)
	go func() { added <- mgr.AddToList(s, ListWhitelist, "Newcomer", "", "tester") }()

	select {
	case err := <-added:
		mgr.lifecycle.Unlock()
		t.Fatalf("the list file was written while a lifecycle transition was in flight (err=%v)", err)
	case <-time.After(200 * time.Millisecond):
	}
	mgr.lifecycle.Unlock()

	select {
	case err := <-added:
		if err != nil {
			t.Fatalf("add once the transition had finished: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AddToList never returned after the lifecycle lock was released")
	}

	entries, err := mgr.readList(s, ListWhitelist)
	if err != nil || len(entries) != 1 || entries[0].Name != "Newcomer" {
		t.Fatalf("whitelist = %+v (%v), want the entry written once the lock was free", entries, err)
	}

	// Removal composes the same check-then-write and had the same gap.
	mgr.lifecycle.Lock()
	removed := make(chan error, 1)
	go func() { removed <- mgr.RemoveFromList(s, ListWhitelist, "Newcomer", "tester") }()

	select {
	case err := <-removed:
		mgr.lifecycle.Unlock()
		t.Fatalf("the list file was rewritten while a lifecycle transition was in flight (err=%v)", err)
	case <-time.After(200 * time.Millisecond):
	}
	mgr.lifecycle.Unlock()

	select {
	case err := <-removed:
		if err != nil {
			t.Fatalf("remove once the transition had finished: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RemoveFromList never returned after the lifecycle lock was released")
	}
}

// M10, the half a lock cannot fix. "Not running" was treated as "safe to edit
// the file", which swept starting and stopping in with stopped - and in both of
// those the game already owns the files: it reads them as it boots and rewrites
// them as it shuts down. The direct write was therefore lost exactly as if it
// had raced Start, only without needing to win a race at all.
func TestPlayerListEditIsRefusedWhileTheServerIsMidTransition(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	for _, st := range []string{StatusStarting, StatusStopping, StatusRunning} {
		s.mu.Lock()
		s.Status = st
		s.mu.Unlock()

		if err := mgr.AddToList(s, ListOps, "Newcomer", "", "tester"); err == nil {
			t.Errorf("a %s server accepted a player-list change the game would overwrite", st)
		}
		if e, err := mgr.readList(s, ListOps); err != nil || len(e) != 0 {
			t.Errorf("ops.json was edited directly while the server was %s: %+v (%v)", st, e, err)
		}
	}

	// And the stopped path still works, or the refusal above has just broken the
	// only way to edit these lists at all.
	s.mu.Lock()
	s.Status = StatusStopped
	s.mu.Unlock()
	if err := mgr.AddToList(s, ListOps, "Newcomer", "", "tester"); err != nil {
		t.Fatalf("a stopped server refused a list change: %v", err)
	}
	if e, err := mgr.readList(s, ListOps); err != nil || len(e) != 1 {
		t.Fatalf("ops.json = %+v (%v), want the entry", e, err)
	}
}

// C5's class, in players.go. PlayerLists read s.Props["white-list"] with no lock
// while reloadProps writes that same map from the file-manager path. A
// concurrent map read/write is a fatal runtime abort, not a recoverable panic,
// so this takes the panel down whole. Run under -race.
func TestPlayerListsDoNotRaceAPropertiesEdit(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	content, err := mgr.ReadFile(s, "server.properties")
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(2)
	go func() { // the Players screen polling
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := mgr.PlayerLists(s); err != nil {
				t.Errorf("PlayerLists: %v", err)
				return
			}
		}
	}()
	go func() { // someone editing server.properties in the file manager
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			v := "true"
			if i%2 == 0 {
				v = "false"
			}
			mgr.reloadProps(s, strings.Replace(content, "white-list=false", "white-list="+v, 1))
		}
	}()

	time.Sleep(400 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// friendlyFSError promises in its own doc comment to keep the panel's internal
// paths - and specifically the temp file it writes through - out of the message,
// then wrapped the raw error in its default branch. *os.LinkError carries both
// the temp path and the target, so any errno the friendly list does not name
// handed the operator an absolute path into the data directory, pointing at a
// randomly-suffixed file the panel had already deleted.
func TestWriteErrorsDoNotNameThePanelsTempFile(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]
	dir, err := mgr.ensureServerDir(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A directory where the write wants a file: the rename at the end of the
	// atomic write fails EISDIR, which is not one of the errnos named above and
	// so lands in the branch that used to leak.
	err = mgr.WriteFile(s, "config", "x")
	if err == nil {
		t.Fatal("writing a file over a directory succeeded")
	}
	msg := err.Error()
	if strings.Contains(msg, ".tmp") {
		t.Errorf("the error names the panel's own temp file: %q", msg)
	}
	if strings.Contains(msg, mgr.dataDir) {
		t.Errorf("the error names an absolute path inside the data directory: %q", msg)
	}
	if !strings.Contains(msg, "config") {
		t.Errorf("the error does not name the file the operator asked for: %q", msg)
	}
}

// Same leak, one line earlier. WriteFile creates the parent directory before the
// atomic write and returned that failure raw, so an unwritable directory
// reported the panel's absolute path rather than the relative one the operator
// typed.
func TestWriteFileMkdirErrorDoesNotLeakThePanelPath(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions this relies on")
	}
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]
	dir, err := mgr.ensureServerDir(s)
	if err != nil {
		t.Fatal(err)
	}

	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	err = mgr.WriteFile(s, "locked/sub/config.yml", "x")
	if err == nil {
		t.Fatal("a write into an unwritable directory succeeded")
	}
	if strings.Contains(err.Error(), mgr.dataDir) {
		t.Errorf("the failed mkdir handed the operator the panel's own path: %q", err)
	}
	if !strings.Contains(err.Error(), "locked/sub/config.yml") {
		t.Errorf("the error does not name the file the operator asked for: %q", err)
	}
}
