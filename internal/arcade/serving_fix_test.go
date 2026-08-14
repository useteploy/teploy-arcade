package arcade

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// timedTestAgent wires the panel the way Run does - newHTTPServer and all - with
// the timeouts shrunk so a stream can outlive them inside a test. Using the real
// constructor is the point: a test that built its own bare http.Server would
// prove nothing about what ships.
func timedTestAgent(t *testing.T, timeout time.Duration) (string, *Manager) {
	t.Helper()
	hub := NewHub()
	mgr := NewManager(t.TempDir(), hub)
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	api := &API{mgr: mgr, hub: hub}
	mux := http.NewServeMux()
	api.Routes(mux)

	srv := newHTTPServer("", limitBodies(mgr.auth.attach(mux)))
	srv.ReadTimeout = timeout
	srv.WriteTimeout = timeout
	srv.IdleTimeout = timeout

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String(), mgr
}

// readSSEEvent returns the next data frame. Bounded, and returning an error
// rather than failing: the condition it reports is a stream that went quiet, an
// unbounded read would hang the suite instead of naming it, and half its callers
// are not the test goroutine.
func readSSEEvent(rd *bufio.Reader, budget time.Duration) (string, error) {
	type result struct {
		data string
		err  error
	}
	out := make(chan result, 1)
	go func() {
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				out <- result{err: err}
				return
			}
			if strings.HasPrefix(line, "data: ") {
				out <- result{data: strings.TrimSpace(strings.TrimPrefix(line, "data: "))}
				return
			}
		}
	}()
	select {
	case r := <-out:
		if r.err != nil {
			return "", fmt.Errorf("the stream ended instead of delivering: %w", r.err)
		}
		return r.data, nil
	case <-time.After(budget):
		return "", fmt.Errorf("no event arrived within %s", budget)
	}
}

// H8, second half. Subscribe handed a channel to anyone who asked and kept no
// count, so /api/events was an unbounded allocator: one goroutine, one socket,
// one ticker and a 64-slot buffer per call, held until the caller chose to let
// go.
func TestEventSubscribersAreCapped(t *testing.T) {
	_, mgr := newTestAgent(t)

	held := make([]chan []byte, 0, maxSubscribers)
	for i := 0; i < maxSubscribers; i++ {
		ch, err := mgr.Subscribe()
		if err != nil {
			t.Fatalf("subscriber %d of %d was refused: %v", i+1, maxSubscribers, err)
		}
		held = append(held, ch)
	}
	if _, err := mgr.Subscribe(); err == nil {
		t.Fatalf("subscriber %d was accepted; the fan-out is still unbounded", maxSubscribers+1)
	}

	// A cap that never gives a slot back is a panel that stops working after
	// enough refreshes, which is its own outage.
	mgr.Unsubscribe(held[0])
	ch, err := mgr.Subscribe()
	if err != nil {
		t.Fatalf("the slot freed by a closed stream was not reusable: %v", err)
	}
	mgr.Unsubscribe(ch)
	for _, c := range held[1:] {
		mgr.Unsubscribe(c)
	}
}

// The cap has to sit far above real use. An operator with a handful of tabs
// open, each holding its own SSE stream, must never meet it.
func TestSeveralPanelTabsAllGetTheEventStream(t *testing.T) {
	srv, _ := newTestAgent(t)
	defer srv.Close()

	const tabs = 8
	bodies := make(chan io.ReadCloser, tabs)
	var wg sync.WaitGroup
	for i := 1; i <= tabs; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			res, err := http.Get(srv.URL + "/api/events")
			if err != nil {
				t.Errorf("tab %d could not open the stream: %v", n, err)
				return
			}
			bodies <- res.Body
			if res.StatusCode != http.StatusOK {
				t.Errorf("tab %d was refused with %d", n, res.StatusCode)
				return
			}
			ev, err := readSSEEvent(bufio.NewReader(res.Body), 5*time.Second)
			if err != nil {
				t.Errorf("tab %d: %v", n, err)
				return
			}
			if !strings.Contains(ev, `"hello"`) {
				t.Errorf("tab %d opened with %s", n, ev)
			}
		}(i)
	}
	wg.Wait()
	close(bodies)
	for b := range bodies {
		_ = b.Close()
	}
}

// A refusal has to be a status code the panel can read, not a stalled stream:
// once the event-stream header is out there is nothing left to say no with.
func TestEventStreamRefusalIsAClearError(t *testing.T) {
	srv, mgr := newTestAgent(t)
	defer srv.Close()

	held := make([]chan []byte, 0, maxSubscribers)
	for i := 0; i < maxSubscribers; i++ {
		ch, err := mgr.Subscribe()
		if err != nil {
			t.Fatalf("subscriber %d was refused early: %v", i+1, err)
		}
		held = append(held, ch)
	}
	defer func() {
		for _, ch := range held {
			mgr.Unsubscribe(ch)
		}
	}()

	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatalf("the refused request hung instead of answering: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a full panel answered /api/events with %d, want 503", res.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("the refusal was not readable JSON: %v", err)
	}
	if body["error"] == "" {
		t.Error("the refusal carried no reason for the operator")
	}
}

// M9. WriteTimeout is armed once, before the handler runs, as an absolute
// deadline on the connection - not an idle timer, and nothing re-arms it while
// a response is still being written. Set blanket, it stops the SSE feed dead the
// moment it expires, and says so nowhere: the panel just quietly stops updating.
func TestEventStreamOutlivesTheServerWriteTimeout(t *testing.T) {
	const timeout = 250 * time.Millisecond
	base, mgr := timedTestAgent(t, timeout)

	res, err := http.Get(base + "/api/events")
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer res.Body.Close()
	rd := bufio.NewReader(res.Body)

	ev, err := readSSEEvent(rd, 3*time.Second)
	if err != nil {
		t.Fatalf("opening frame: %v", err)
	}
	if !strings.Contains(ev, `"hello"`) {
		t.Fatalf("opening frame was %s", ev)
	}

	time.Sleep(3 * timeout) // well past the deadline the server armed
	mgr.broadcastEvent("server.updated", mgr.List()[0].ID)

	ev, err = readSSEEvent(rd, 3*time.Second)
	if err != nil {
		t.Fatalf("after the write deadline: %v", err)
	}
	if !strings.Contains(ev, "server.updated") {
		t.Fatalf("event after the write deadline was %s", ev)
	}
}

// M9, the case a blanket WriteTimeout breaks worst. The console upgrade hijacks
// the connection and inherits the deadline, so the socket stays open and looking
// healthy while delivering no output and accepting no commands - the operator
// watches a live server print nothing.
func TestConsoleSocketOutlivesTheServerWriteTimeout(t *testing.T) {
	const timeout = 250 * time.Millisecond
	base, mgr := timedTestAgent(t, timeout)
	id := mgr.List()[0].ID

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/ws/console?server="+id, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	if m := readMsg(t, c, 3*time.Second); m["t"] != "replay" {
		t.Fatalf("first frame was %v, want replay", m["t"])
	}

	time.Sleep(3 * timeout)
	mgr.hub.Publish(id, Line{Level: "info", Source: "server", Text: "printed after the deadline"})

	got := readUntil(t, c, 3*time.Second, func(m msg) bool {
		l, ok := m["line"].(map[string]any)
		if !ok {
			return false
		}
		text, _ := l["text"].(string)
		return strings.Contains(text, "printed after the deadline")
	})
	if got == nil {
		t.Fatal("the console went silent once the server's write deadline passed")
	}
}

// The lifecycle mutex is one lock for the whole process, and Start used to hold
// it across `docker info`, `docker images` and a `docker pull` that runs for
// minutes on a first start. One unresponsive daemon therefore blocked starting,
// stopping and deleting every other server on the host.
func TestASlowDockerPreflightDoesNotStallOtherServers(t *testing.T) {
	_, mgr := newTestAgent(t)
	slow, other := mgr.List()[0], mgr.List()[1]
	slow.Runtime = RuntimeDocker

	release := make(chan struct{})
	reach, local := dockerReachable, dockerImageLocal
	t.Cleanup(func() { dockerReachable, dockerImageLocal = reach, local })
	dockerReachable = func() bool { <-release; return false }
	dockerImageLocal = func(string) bool { return true }

	stalled := make(chan struct{})
	go func() {
		defer close(stalled)
		_ = mgr.Start(slow.ID)
	}()
	time.Sleep(150 * time.Millisecond) // let it reach the preflight

	done := make(chan error, 1)
	go func() { done <- mgr.Start(other.ID) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the second server refused to start: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("a second server could not start while a docker preflight was blocked: the lifecycle lock is held across the shell-out")
	}

	close(release)
	<-stalled
	_ = mgr.Stop(other.ID)
}

// BUGS.md L10. The data directory holds users.json, mcp-tokens.json, audit.json
// and every world. Those files carry 0o600 themselves, but a 0o755 directory
// still hands any other account on the host the worlds, the backups, and the
// listing of what is there.
func TestDataDirIsNotWorldReadable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	mgr := NewManager(dir, NewHub())
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat data dir: %v", err)
	}
	if mode := st.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("data dir is %04o; group and other must have nothing", mode)
	}
}
