package arcade

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ------------------------------------------------------------------ helpers

// fakeDockerBin puts a stub `docker` first on PATH so the runner's exec calls
// answer instantly and hermetically instead of reaching a daemon that may or
// may not exist on the machine running the tests. It returns the path of the
// file each invocation logs its subcommand to.
func fakeDockerBin(t *testing.T, containerUp bool) string {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls.log")
	up := "false"
	if containerUp {
		up = "true"
	}
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$1\" >> \"" + calls + "\"\n" +
		"case \"$1\" in\n" +
		"  inspect) echo " + up + " ;;\n" +
		"  logs) echo 'container output' ;;\n" +
		"  run) echo 'container output' ;;\n" +
		"  wait) sleep 0.4; echo 0 ;;\n" +
		"  stats) echo '5.00% 100MiB / 1024MiB' ;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

func dockerCallCount(t *testing.T, calls, sub string) int {
	t.Helper()
	b, err := os.ReadFile(calls)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == sub {
			n++
		}
	}
	return n
}

// runnerLogSink captures what recoverPanic writes. A recovered panic leaves no
// other trace, so the log line is the assertion: without the recover the
// goroutine does not fail the test, it takes the test binary down with it -
// which is precisely what it does to the panel.
type runnerLogSink struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *runnerLogSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *runnerLogSink) contains(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Contains(l.buf.String(), sub)
}

func captureRunnerLog(t *testing.T) *runnerLogSink {
	t.Helper()
	sink := &runnerLogSink{}
	log.SetOutput(sink)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return sink
}

func waitForRunnerLog(sink *runnerLogSink, sub string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if sink.contains(sub) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// -------------------------------------------------------------------- tests

// H2. An adopted container has no cmd.Wait handler, so nothing used to cancel
// the context its pollStats runs on: when the container stopped the poller kept
// waking every 3s and shelling out to `docker stats` against a container that
// no longer exists, for the life of the process, once per adopted server.
func TestAdoptedContainerStopsPollingAfterItExits(t *testing.T) {
	calls := fakeDockerBin(t, true)
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]
	r := &dockerRunner{mgr: mgr}

	if err := r.Adopt(s, func(l Line) { mgr.emit(s, l) }); err != nil {
		t.Fatalf("adopt: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for s.State() != StatusStopped && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if s.State() != StatusStopped {
		t.Fatalf("the adopted container's exit was never reported (state %q)", s.State())
	}

	// One whole poll interval plus slack: a live context means the poller wakes
	// inside this window and we see the wasted exec.
	time.Sleep(3500 * time.Millisecond)
	if n := dockerCallCount(t, calls, "stats"); n != 0 {
		t.Errorf("`docker stats` ran %d time(s) after the container exited; the poller outlived it", n)
	}
}

// H2. Stop and Kill are the operator-driven end of the same leak: for an
// adopted container they are the only thing that can tear the runner down, and
// neither used to touch the proc's cancel func the way simRunner.Stop does.
func TestDockerStopAndKillCancelTheRunner(t *testing.T) {
	fakeDockerBin(t, false)
	r := &dockerRunner{}

	for _, tc := range []struct {
		name string
		call func(*Server) error
	}{
		{"Stop", r.Stop},
		{"Kill", r.Kill},
	} {
		s := &Server{ID: "adopted" + tc.name}
		ctx, cancel := context.WithCancel(context.Background())
		s.proc = &dockerProc{cancel: cancel}

		if err := tc.call(s); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if ctx.Err() == nil {
			t.Errorf("%s left the runner's context live; its stats poller keeps running", tc.name)
		}
		cancel()
	}
}

// H3. pollStats locks s.mu and parses docker output every tick; its siblings
// stream and watchExit recover, this one did not, so one fault took the panel
// with it.
func TestStatsPollerRecoversItsPanic(t *testing.T) {
	fakeDockerBin(t, false)
	sink := captureRunnerLog(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A nil server is the cheapest way to fault the tick body. It also proves
	// the recover is installed before anything dereferences s: a label built
	// from s.ID would panic at the defer statement itself, leaving nothing to
	// recover.
	go (&dockerRunner{}).pollStats(ctx, nil, "gamepanel-ghost")

	if !waitForRunnerLog(sink, "panic in docker stats poller", 6*time.Second) {
		t.Error("the stats poller did not recover its panic")
	}
}

// H4. The exit handler is the busiest goroutine in the docker lifecycle - it
// fires on every container exit and cascades through stopped/fail/Save and the
// hub. A panic anywhere down there escaped and killed the panel.
//
// It used to be the `cmd.Wait` handler on an attached `docker run`. Containers
// are started detached now (so a panel restart does not stop them), so the exit
// is reported by `docker wait` in watchExit instead. Same cascade, same hazard,
// different goroutine - hence the different label below.
func TestDockerExitHandlerRecoversItsPanic(t *testing.T) {
	fakeDockerBin(t, false)
	sink := captureRunnerLog(t)

	// A manager with no hub faults the handler exactly where the real one can:
	// processExited ends in hub.Publish.
	mgr := NewManager(t.TempDir(), nil)
	if err := mgr.Load(); err != nil {
		t.Fatal(err)
	}
	s := mgr.List()[0]

	r := &dockerRunner{mgr: mgr}
	if err := r.Start(s, func(Line) {}); err != nil {
		t.Fatalf("start: %v", err)
	}

	if !waitForRunnerLog(sink, "panic in exit watcher", 6*time.Second) {
		t.Error("the container exit handler did not recover its panic")
	}
}

// M3. The simulator's stop, flood and crash commands each spawn a bare
// goroutine that reaches hub.Publish. A viewer disconnecting at the wrong
// moment faults them, and with no recover the panic escaped from a code path an
// HTTP handler had already answered successfully.
func TestSimulatorCommandGoroutinesRecoverTheirPanic(t *testing.T) {
	sink := captureRunnerLog(t)

	// A nil manager stands in for any fault on the publish path these commands
	// reach through m.Stop and m.fail.
	r := &simRunner{}
	s := &Server{ID: "sim1", MaxPlayers: 20}
	quiet := func(Line) {}

	r.handleCommand(s, "stop", quiet)
	r.handleCommand(s, "crash", quiet)
	r.handleCommand(s, "flood", func(l Line) {
		if strings.HasPrefix(l.Text, "[flood]") {
			panic("viewer disconnected mid-flood")
		}
	})

	for _, want := range []string{
		"panic in simulator stop for sim1",
		"panic in simulator crash for sim1",
		"panic in simulator flood for sim1",
	} {
		if !waitForRunnerLog(sink, want, 3*time.Second) {
			t.Errorf("%q never appeared; that goroutine kills the panel instead", want)
		}
	}
}

// L2. dockerRunner.Start used to create its context above the already-running
// guard and return without cancelling it. The leak is invisible at runtime - an
// uncalled cancel against context.Background() holds no goroutine and no
// channel, and only go vet's lostcancel sees it - so the regression guard is
// structural: Start must be committed to starting a container before it creates
// the context it would then owe a cancel to.
func TestDockerStartDoesNotBuildAContextItCannotCancel(t *testing.T) {
	src, err := os.ReadFile("runner.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	from := strings.Index(body, "func (r *dockerRunner) Start(")
	if from < 0 {
		t.Fatal("dockerRunner.Start not found in runner.go")
	}
	body = body[from:]
	if end := strings.Index(body[1:], "\nfunc "); end >= 0 {
		body = body[:end]
	}

	guard := strings.Index(body, "containerRunning(s.ID)")
	made := strings.Index(body, "context.WithCancel")
	if guard < 0 || made < 0 {
		t.Fatal("dockerRunner.Start no longer has both the running guard and the context")
	}
	if made < guard {
		t.Error("Start creates its context before the already-running guard, so the early return leaks the cancel func")
	}
}
