package arcade

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// ------------------------------------------------------------------ helpers

// managerLogSink captures what recoverPanic writes. A recovered panic leaves no
// other trace, so the log line is the assertion - and without the recover the
// goroutine does not fail the test, it takes the whole test binary down, which
// is exactly what it does to the panel.
type managerLogSink struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *managerLogSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *managerLogSink) contains(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Contains(l.buf.String(), sub)
}

func captureManagerLog(t *testing.T) *managerLogSink {
	t.Helper()
	sink := &managerLogSink{}
	log.SetOutput(sink)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return sink
}

func waitForManagerLog(sink *managerLogSink, sub string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if sink.contains(sub) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// faultingRunner faults on one lifecycle call. The point is where the panic
// lands: inside the goroutine Stop, Kill and Restart spawn, which no HTTP
// handler's recover can reach - the request that started it has already been
// answered 200.
type faultingRunner struct{ on string }

func (p *faultingRunner) boom(which string) {
	if p.on == which {
		panic("synthetic fault in " + which)
	}
}

func (p *faultingRunner) Start(s *Server, emit func(Line)) error { p.boom("Start"); return nil }
func (p *faultingRunner) Stop(s *Server) error                   { p.boom("Stop"); return nil }
func (p *faultingRunner) Kill(s *Server) error                   { p.boom("Kill"); return nil }
func (p *faultingRunner) Send(s *Server, cmd string) error       { return nil }

// stallingRunner accepts a stop that never completes, which is what a docker
// stop under load looks like from the panel's side: the container is asked to
// go away and the panel hears nothing back.
type stallingRunner struct{}

func (stallingRunner) Start(s *Server, emit func(Line)) error { return nil }
func (stallingRunner) Stop(s *Server) error                   { return nil }
func (stallingRunner) Kill(s *Server) error                   { return nil }
func (stallingRunner) Send(s *Server, cmd string) error       { return nil }

// -------------------------------------------------------------------- tests

// H1. metricsLoop was the only background loop without a recover, so any fault
// on its 2s tick propagated to the runtime and killed the panel - taking every
// server's management with it for the sake of a metrics sample.
func TestMetricsLoopRecoversItsPanic(t *testing.T) {
	sink := captureManagerLog(t)

	// A manager with no hub faults the tick exactly where the real one can: the
	// loop ends in hub.PublishRaw for every running server.
	mgr := NewManager(t.TempDir(), nil)
	s := &Server{ID: "metrics1", Name: "Metrics", Status: StatusRunning, Props: map[string]string{}}
	mgr.servers[s.ID] = s
	mgr.order = append(mgr.order, s.ID)

	go mgr.metricsLoop()

	if !waitForManagerLog(sink, "panic in metrics loop", 8*time.Second) {
		t.Error("the metrics loop did not recover its panic")
	}
}

// M2. Stop, Kill and Restart each hand the real work to a goroutine that ends
// in hub.Publish. A viewer disconnecting at the wrong moment faults it, and
// with no recover the panic escaped from a path whose HTTP request had already
// returned successfully - the panel died and the caller saw a 200.
func TestLifecycleWorkersRecoverTheirPanic(t *testing.T) {
	for _, tc := range []struct {
		name, faultOn, want string
		call                func(*Manager, string) error
	}{
		{"stop", "Stop", "panic in stop worker", (*Manager).Stop},
		{"kill", "Kill", "panic in kill worker", (*Manager).Kill},
		{"restart", "Start", "panic in restart worker", (*Manager).Restart},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := captureManagerLog(t)
			_, mgr := newTestAgent(t)
			mgr.sim = &faultingRunner{on: tc.faultOn}

			s := mgr.List()[0]
			s.mu.Lock()
			s.Status = StatusRunning
			s.mu.Unlock()

			if err := tc.call(mgr, s.ID); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if !waitForManagerLog(sink, tc.want, 10*time.Second) {
				t.Errorf("the %s worker did not recover its panic", tc.name)
			}
		})
	}
}

// M1. fail() cleared the proc handle without cancelling the runner behind it.
// The simulator's goroutine only ends when its context does, so a "failed"
// server went on rewriting cpu/memory every 2s and emitting console lines every
// 12s - status and reality diverging, which is the problem this project exists
// to solve.
func TestFailCancelsTheRunner(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := startServer(t, mgr)

	s.mu.Lock()
	p, _ := s.proc.(*simProc)
	s.mu.Unlock()
	if p == nil {
		t.Fatal("no simulator process to begin with")
	}

	mgr.fail(s, 137, "OOM")

	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
		t.Fatal("the simulator outlived the failure; a failed server keeps reporting live CPU, memory and console output")
	}
}

// M5. Delete permits a server that is still stopping, and once the map entry is
// gone nothing else can ever cancel its runner: the goroutine keeps publishing,
// which recreates the room DropRoom just removed and leaks both for the life of
// the process.
func TestDeleteCancelsARunnerThatIsStillStopping(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := startServer(t, mgr)

	s.mu.Lock()
	p, _ := s.proc.(*simProc)
	s.Status = StatusStopping
	s.mu.Unlock()
	if p == nil {
		t.Fatal("no simulator process to begin with")
	}

	if err := mgr.Delete(s.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
		t.Fatal("the runner outlived the server it belongs to")
	}
}

// M5. Delete and Start both read the server's state and only then commit to it.
// With no lock across the pair both pass their own check on a stopped server:
// the entry is removed while the runner is starting, and the survivor is a
// goroutine driving a server the panel no longer knows about.
func TestDeleteAndStartCannotBothWin(t *testing.T) {
	_, mgr := newTestAgent(t)

	for round := 0; round < 40; round++ {
		s, err := mgr.Create(fmt.Sprintf("race-%d", round), "paper", "1.20.4", 0, 0, 0, RuntimeSim)
		if err != nil {
			t.Fatalf("create: %v", err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = mgr.Start(s.ID) }()
		go func() { defer wg.Done(); _ = mgr.Delete(s.ID) }()
		wg.Wait()

		if mgr.Get(s.ID) == nil {
			if st := s.State(); st == StatusStarting || st == StatusRunning {
				t.Fatalf("round %d: the server was deleted and started at once (state %q); its runner now drives a server that no longer exists",
					round, st)
			}
			continue
		}
		// Start won the race, so this one is genuinely alive and has to be torn
		// down before the next round.
		s.mu.Lock()
		p := s.proc
		s.mu.Unlock()
		if p != nil {
			p.stop()
		}
	}
}

// L3. room() is get-or-create, which is right for Join and Publish and wrong
// for everything else. Rooms are only ever removed by DropRoom, so anything
// that materialises one after the server is gone - the reader's deferred Leave,
// a status push from a runner that has not noticed yet, an MCP tail - leaks a
// ring buffer for the life of the process.
func TestHubDoesNotMaterialiseRoomsForServersThatAreGone(t *testing.T) {
	hub := NewHub()
	c := NewConn(4)
	hub.Join("gone", c)
	hub.Publish("gone", Line{Text: "output"})
	hub.DropRoom("gone")

	hub.Leave("gone", c)
	hub.PublishRaw("gone", map[string]any{"t": "status"})
	_ = hub.Tail("gone", 10)
	_ = hub.Viewers("gone")

	// A dropped room is tombstoned, not deleted: get-or-create has to stay, or a
	// server that boots with nobody watching buffers nothing for replay. What is
	// checked here is that none of these calls put lines back into a dead room.
	hub.mu.RLock()
	rooms := make([]*Room, 0, len(hub.rooms))
	for _, r := range hub.rooms {
		rooms = append(rooms, r)
	}
	hub.mu.RUnlock()
	for _, r := range rooms {
		r.mu.Lock()
		held, viewers, dead := cap(r.ring), len(r.conns), r.dead
		r.mu.Unlock()
		if !dead {
			t.Errorf("room %q was rebuilt after DropRoom", r.ID)
		}
		if held != 0 || viewers != 0 {
			t.Errorf("room %q holds %d line(s) of ring and %d viewer(s) after DropRoom", r.ID, held, viewers)
		}
	}
}

// M7. Restart waited a fixed 60s and then called Start regardless. A docker
// stop that runs long left the server still stopping, Start refused it, and the
// failure went to the log only: the operator's restart was accepted and then
// silently turned into a stop.
func TestRestartReportsAServerThatNeverStops(t *testing.T) {
	_, mgr := newTestAgent(t)
	mgr.docker = stallingRunner{}

	s := mgr.List()[0]
	s.Runtime = RuntimeDocker // only the docker path waits for the runtime to report the stop back
	s.mu.Lock()
	s.Status = StatusRunning
	s.mu.Unlock()

	restore := restartStopWait
	restartStopWait = 300 * time.Millisecond
	defer func() { restartStopWait = restore }()

	if err := mgr.Restart(s.ID); err != nil {
		t.Fatalf("restart: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, l := range mgr.hub.Tail(s.ID, 50) {
			if l.Source == "panel" && strings.Contains(l.Text, "never finished stopping") {
				if st := s.State(); st == StatusStarting || st == StatusRunning {
					t.Errorf("the server was started while it was still %q", st)
				}
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("a restart that could not stop the server told the operator nothing; the request is accepted and then lost")
}
