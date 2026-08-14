package arcade

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Save() marshals every Server. json.Marshal reads Status, StartedAt, Props and
// PendingRestart with no lock while runner goroutines write them.
func TestSaveDoesNotRaceWithStatusWrites(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() { // writer, as a runner goroutine would
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			mgr.setStatus(s, []string{StatusRunning, StatusStopped}[i%2], 0, "")
		}
	}()

	wg.Add(1)
	go func() { // persister
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = mgr.Save()
		}
	}()

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// Deleting a server must not leave its console room behind. Rooms hold a
// 500-line ring buffer each, so leaking them leaks memory for the life of the
// process.
func TestDeletedServerReleasesItsConsoleRoom(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	for i := 0; i < 50; i++ {
		mgr.hub.Publish(s.ID, Line{Text: fmt.Sprintf("line %d", i)})
	}
	if len(mgr.hub.Tail(s.ID, 10)) == 0 {
		t.Fatal("no buffered lines to begin with")
	}

	if err := mgr.Delete(s.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// The room entry survives deletion on purpose, as a tombstone: what must be
	// released is the 500-line ring buffer, and what must not happen is a late
	// Publish from a leaked runner goroutine rebuilding a full room that nothing
	// will ever drain. Assert both, rather than the presence of the map key.
	mgr.hub.Publish(s.ID, Line{Text: "late line from a goroutine that outlived the server"})
	if n := len(mgr.hub.Tail(s.ID, 100)); n != 0 {
		t.Errorf("deleted server buffered %d line(s); the ring should stay released", n)
	}
	mgr.hub.mu.RLock()
	r := mgr.hub.rooms[s.ID]
	mgr.hub.mu.RUnlock()
	if r != nil {
		r.mu.Lock()
		held, viewers := cap(r.ring), len(r.conns)
		r.mu.Unlock()
		if held != 0 || viewers != 0 {
			t.Errorf("deleted room still holds %d line(s) of ring and %d viewer(s)", held, viewers)
		}
	}
}

// A server that crashes immediately, over and over, must stop being restarted.
// Without a breaker a crash-looping server pins a core and floods the console.
func TestCrashLoopIsBroken(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	for i := 0; i < maxRestarts+3; i++ {
		mgr.fail(s, 1, "synthetic crash")
	}
	if !mgr.circuitOpen(s) {
		t.Fatalf("after %d failures the breaker should be open (restarts=%d)",
			maxRestarts+3, s.Restarts)
	}
	if err := mgr.Start(s.ID); err == nil {
		t.Error("start should be refused while the breaker is open")
	}
	// A human clearing it must work, or the server is stuck forever.
	mgr.ClearFailures(s, "tester")
	if mgr.circuitOpen(s) {
		t.Error("clearing failures should close the breaker")
	}
}

// Starting a server whose port another *running* server holds must fail before
// the container is launched, not after Docker rejects the bind.
func TestStartRefusesAPortAlreadyInUse(t *testing.T) {
	_, mgr := newTestAgent(t)
	list := mgr.List()
	a, b := list[0], list[1]

	startServer(t, mgr)

	// force a conflict the way a hand-edited server.properties would
	b.mu.Lock()
	b.Port = a.Port
	b.mu.Unlock()

	if err := mgr.Start(b.ID); err == nil {
		t.Error("starting a server on an occupied port should be refused up front")
	}
}

// Goroutines must not accumulate across a start/stop cycle.
func TestStartStopDoesNotLeakGoroutines(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	settle := func() int {
		for i := 0; i < 20; i++ {
			runtime.GC()
			time.Sleep(60 * time.Millisecond)
		}
		return runtime.NumGoroutine()
	}

	_ = mgr.Start(s.ID)
	waitFor(t, 12*time.Second, func() bool { return s.State() == StatusRunning })
	_ = mgr.Stop(s.ID)
	waitFor(t, 8*time.Second, func() bool { return s.State() == StatusStopped })
	base := settle()

	for i := 0; i < 3; i++ {
		_ = mgr.Start(s.ID)
		waitFor(t, 12*time.Second, func() bool { return s.State() == StatusRunning })
		_ = mgr.Stop(s.ID)
		waitFor(t, 8*time.Second, func() bool { return s.State() == StatusStopped })
	}
	after := settle()

	if after > base+6 {
		t.Errorf("goroutines grew %d -> %d across three cycles", base, after)
	}
}

// WebSockets are not subject to CORS, so the Origin check is the only thing
// stopping a page the operator happens to visit from opening a console socket
// to their panel and running commands. Regression test for exactly that.
func TestConsoleSocketRejectsForeignOrigins(t *testing.T) {
	srv, mgr := newTestAgent(t)
	defer srv.Close()
	id := mgr.List()[0].ID

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/console?server=" + id

	// A page on another site must not be able to attach.
	_, _, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"https://evil.example"}},
	})
	if err == nil {
		t.Fatal("a cross-origin console socket was accepted")
	}

	// Same-origin must still work, or the panel is broken.
	c, _, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{srv.URL}},
	})
	if err != nil {
		t.Fatalf("same-origin dial failed: %v", err)
	}
	c.CloseNow()
}
