package arcade

import (
	"testing"
	"time"
)

// Containers run with --rm and no restart policy, so a host reboot takes every
// server down and leaves nothing for reconcile() to re-adopt. The panel then
// came back up reporting eight stopped servers and no reason why - on the
// deployed host the box had been up 23 hours and the containers 19, because a
// human had to notice and press Start on each one.
//
// resume() puts back what the host took down, and - just as important - leaves
// alone what an operator took down on purpose.

type resumeRunner struct{ started chan string }

func (r *resumeRunner) Start(s *Server, emit func(Line)) error { r.started <- s.ID; return nil }
func (r *resumeRunner) Stop(s *Server) error                   { return nil }
func (r *resumeRunner) Kill(s *Server) error                   { return nil }
func (r *resumeRunner) Send(s *Server, cmd string) error       { return nil }

// loadWithRecordedRunner reloads dir into a fresh manager whose docker runner
// records what it is asked to start.
func loadWithRecordedRunner(t *testing.T, dir string) *resumeRunner {
	t.Helper()

	reach, local := dockerReachable, dockerImageLocal
	stagger := resumeStagger
	t.Cleanup(func() {
		dockerReachable, dockerImageLocal = reach, local
		resumeStagger = stagger
	})
	dockerReachable = func() bool { return true }
	dockerImageLocal = func(string) bool { return true }
	resumeStagger = time.Millisecond

	rec := &resumeRunner{started: make(chan string, 8)}
	m := NewManager(dir, NewHub())
	m.docker = rec
	if err := m.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	return rec
}

func TestResumeRestartsWhatTheHostTookDown(t *testing.T) {
	dir := t.TempDir()

	first := NewManager(dir, NewHub())
	if err := first.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	list := first.List()
	if len(list) < 2 {
		t.Fatalf("expected the seed to provide at least two servers, got %d", len(list))
	}
	up, down := list[0], list[1]
	for _, s := range []*Server{up, down} {
		s.mu.Lock()
		s.Runtime = RuntimeDocker
		s.mu.Unlock()
	}
	up.mu.Lock()
	up.Status = StatusRunning
	up.mu.Unlock()
	down.mu.Lock()
	down.Status = StatusStopped
	down.mu.Unlock()
	if err := first.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	rec := loadWithRecordedRunner(t, dir)

	select {
	case id := <-rec.started:
		if id != up.ID {
			t.Fatalf("resumed %s; expected the server that was running (%s)", id, up.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a server that was running before the reboot was never resumed")
	}

	// The operator stopped this one. A reboot is not permission to start it.
	select {
	case id := <-rec.started:
		t.Fatalf("resumed %s, which was stopped on purpose", id)
	case <-time.After(400 * time.Millisecond):
	}
}

// The simulator is a development fixture. Bringing five fake servers back on
// every `go run` is noise, not recovery, so resume is docker-only.
func TestResumeIgnoresSimulatorServers(t *testing.T) {
	dir := t.TempDir()

	first := NewManager(dir, NewHub())
	if err := first.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	s := first.List()[0]
	s.mu.Lock()
	s.Runtime = RuntimeSim
	s.Status = StatusRunning
	s.mu.Unlock()
	if err := first.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	rec := loadWithRecordedRunner(t, dir)

	select {
	case id := <-rec.started:
		t.Fatalf("resumed simulator server %s", id)
	case <-time.After(400 * time.Millisecond):
	}
}
