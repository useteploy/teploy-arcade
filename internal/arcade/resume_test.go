package arcade

import (
	"sync"
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

// The panel's unit is ordered After=docker.service, which is satisfied when
// that unit starts rather than when the socket answers. On a cold boot the
// panel can therefore come up first, find no daemon, and give up - turning the
// one scenario resume exists for into the failure it was built to prevent, and
// looking identical to the original bug: every server stopped, nothing saying
// why.
func TestBootRecoveryWaitsForADaemonThatIsNotUpYet(t *testing.T) {
	dir := t.TempDir()

	first := NewManager(dir, NewHub())
	if err := first.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	up := first.List()[0]
	up.mu.Lock()
	up.Runtime = RuntimeDocker
	up.Status = StatusRunning
	up.mu.Unlock()
	if err := first.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	reach, local := dockerReachable, dockerImageLocal
	window, poll, stagger := bootRecoveryWindow, bootRecoveryPoll, resumeStagger
	t.Cleanup(func() {
		dockerReachable, dockerImageLocal = reach, local
		bootRecoveryWindow, bootRecoveryPoll, resumeStagger = window, poll, stagger
	})
	dockerImageLocal = func(string) bool { return true }
	bootRecoveryWindow = 5 * time.Second
	bootRecoveryPoll = 20 * time.Millisecond
	resumeStagger = time.Millisecond

	// Down for the first few asks, the way a daemon still starting behaves.
	var mu sync.Mutex
	asks := 0
	dockerReachable = func() bool {
		mu.Lock()
		defer mu.Unlock()
		asks++
		return asks > 3
	}

	rec := &resumeRunner{started: make(chan string, 8)}
	m := NewManager(dir, NewHub())
	m.docker = rec
	if err := m.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}

	select {
	case id := <-rec.started:
		if id != up.ID {
			t.Fatalf("resumed %s, expected %s", id, up.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the panel gave up on a daemon that was only a moment late; the server stayed down")
	}

	// resume writes an audit entry after Start returns, and t.TempDir removes
	// the directory the moment the test does - which fails the cleanup rather
	// than the assertion. Let the goroutine finish its write.
	time.Sleep(250 * time.Millisecond)
}

// A run with no docker-runtime servers must not leave a polling goroutine
// behind - which is every test and every simulator-only run.
func TestBootRecoveryDoesNotPollWhenThereIsNothingToRecover(t *testing.T) {
	reach := dockerReachable
	window := bootRecoveryWindow
	t.Cleanup(func() { dockerReachable, bootRecoveryWindow = reach, window })
	bootRecoveryWindow = time.Hour // a poll loop here would outlive the test
	asked := 0
	dockerReachable = func() bool { asked++; return false }

	m := NewManager(t.TempDir(), NewHub())
	if err := m.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if asked > 1 {
		t.Errorf("asked docker %d times with only simulator servers; expected one probe", asked)
	}
}
