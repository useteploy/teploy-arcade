package arcade

import (
	"context"
	"testing"
	"time"
)

// Stopping a docker server left it in "stopping" forever.
//
// Stop cancels the attach context immediately after `docker stop` returns, and
// that cancel kills the `docker wait` that was going to report the exit code.
// watchExit then saw a cancelled context and returned silently, assuming
// somebody else had handled it - and nobody had. The server was then unusable
// without restarting the panel: Start refuses ("still stopping"), Kill returns
// 200 and changes nothing, and reconcile skips it because its container is not
// running.
//
// Observed 2 out of 2 stops in the template exercise, on containers that were
// gone within four seconds.
func TestACancelledWaitStillReportsAContainerThatIsGone(t *testing.T) {
	mgr := NewManager(t.TempDir(), NewHub())
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	s := mgr.List()[0]
	s.mu.Lock()
	s.Runtime = RuntimeDocker
	s.Status = StatusStopping
	s.mu.Unlock()

	old := containerRunning
	t.Cleanup(func() { containerRunning = old })
	containerRunning = func(string) bool { return false } // it is already gone

	// A context that is already cancelled, which is the state Stop leaves
	// behind, driving the same path `docker wait` failing would.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &dockerRunner{mgr: mgr}
	r.watchExit(ctx, s, "gamepanel-"+s.ID)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.State() == StatusStopped {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the server is still %q; nothing else will ever move it on", s.State())
}

// The other half: a container that IS still running must not be reported as
// stopped just because the panel cancelled its watcher - that would show a live
// server as stopped and offer to start a second one.
func TestACancelledWaitLeavesALiveContainerAlone(t *testing.T) {
	mgr := NewManager(t.TempDir(), NewHub())
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	s := mgr.List()[0]
	s.mu.Lock()
	s.Runtime = RuntimeDocker
	s.Status = StatusRunning
	s.mu.Unlock()

	old := containerRunning
	t.Cleanup(func() { containerRunning = old })
	containerRunning = func(string) bool { return true }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &dockerRunner{mgr: mgr}
	r.watchExit(ctx, s, "gamepanel-"+s.ID)

	time.Sleep(100 * time.Millisecond)
	if got := s.State(); got != StatusRunning {
		t.Fatalf("a running container was reported as %q", got)
	}
}
