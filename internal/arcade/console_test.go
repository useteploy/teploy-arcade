package arcade

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func newTestAgent(t *testing.T) (*httptest.Server, *Manager) {
	t.Helper()
	hub := NewHub()
	mgr := NewManager(t.TempDir(), hub)
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	api := &API{mgr: mgr, hub: hub}
	mux := http.NewServeMux()
	api.Routes(mux)
	return httptest.NewServer(mux), mgr
}

func dialConsole(t *testing.T, srv *httptest.Server, id string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/console?server=" + id
	c, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c
}

type msg map[string]any

func readMsg(t *testing.T, c *websocket.Conn, timeout time.Duration) msg {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m msg
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}

// readUntil drains messages until pred matches or the budget runs out.
func readUntil(t *testing.T, c *websocket.Conn, budget time.Duration, pred func(msg) bool) msg {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Until(deadline))
		_, data, err := c.Read(ctx)
		cancel()
		if err != nil {
			return nil
		}
		var m msg
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		if pred(m) {
			return m
		}
	}
	return nil
}

func startServer(t *testing.T, mgr *Manager) *Server {
	t.Helper()
	s := mgr.List()[0]
	if err := mgr.Start(s.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	for i := 0; i < 100; i++ {
		if s.State() == StatusRunning {
			return s
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("server never reached running (status %q)", s.State())
	return nil
}

// The first frame on a fresh socket is the replay, so a page refresh never
// lands on a blank console.
func TestConsoleReplayOnConnect(t *testing.T) {
	srv, mgr := newTestAgent(t)
	defer srv.Close()
	s := startServer(t, mgr)

	c := dialConsole(t, srv, s.ID)
	defer c.CloseNow()

	m := readMsg(t, c, 3*time.Second)
	if m["t"] != "replay" {
		t.Fatalf("first frame = %v, want replay", m["t"])
	}
	if m["buffer_capacity"].(float64) != float64(ringSize) {
		t.Errorf("buffer_capacity = %v, want %d", m["buffer_capacity"], ringSize)
	}
	lines, _ := m["lines"].([]any)
	if len(lines) == 0 {
		t.Fatal("replay carried no lines; the boot output should be buffered")
	}

	// Every line must carry a monotonic seq - that is what lets a client
	// localise a gap and reconcile after a reconnect.
	var last float64
	for i, raw := range lines {
		l := raw.(map[string]any)
		seq, ok := l["seq"].(float64)
		if !ok || seq <= last {
			t.Fatalf("line %d: seq %v not monotonic (previous %v)", i, l["seq"], last)
		}
		last = seq
	}
}

// NI-002: a command from one viewer must reach the game server, and must NOT be
// echoed to other viewers as their inbound message. Both viewers should see the
// resulting output.
func TestCommandRoutesToServerNotPeers(t *testing.T) {
	srv, mgr := newTestAgent(t)
	defer srv.Close()
	s := startServer(t, mgr)

	a := dialConsole(t, srv, s.ID)
	defer a.CloseNow()
	b := dialConsole(t, srv, s.ID)
	defer b.CloseNow()

	readMsg(t, a, 3*time.Second) // replay
	readMsg(t, b, 3*time.Second)

	if err := a.Write(context.Background(), websocket.MessageText, mustJSON(map[string]any{
		"t": "command", "id": "1", "text": "list", "actor": "tester",
	})); err != nil {
		t.Fatalf("write: %v", err)
	}

	// A must be acked.
	ack := readUntil(t, a, 3*time.Second, func(m msg) bool { return m["t"] == "command_ack" })
	if ack == nil {
		t.Fatal("no command_ack; the sender cannot tell sent from swallowed")
	}
	if ack["accepted"] != true {
		t.Fatalf("command rejected: %v", ack["error"])
	}

	// B must see the *result* of the command, and must never receive a frame
	// that is the raw inbound command echoed back at it.
	sawResult := false
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Until(deadline))
		_, data, err := b.Read(ctx)
		cancel()
		if err != nil {
			break
		}
		var m msg
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		if m["t"] == "command" {
			t.Fatal("peer received the raw inbound command - inbound is being fanned out (NI-002)")
		}
		if m["t"] == "line" {
			l := m["line"].(map[string]any)
			text, _ := l["text"].(string)
			if strings.Contains(text, "players online") {
				sawResult = true
				break
			}
		}
	}
	if !sawResult {
		t.Fatal("peer never saw the command's output; fanout is broken")
	}
}

// NI-008: the socket counts its own shed lines, so the UI can say "N dropped"
// truthfully instead of silently losing output.
func TestBackpressureIsCountedAndReported(t *testing.T) {
	hub := NewHub()
	c := NewConn(4)

	hub.Join("room", c)
	for i := 0; i < 200; i++ {
		hub.Publish("room", Line{Text: "flood"})
	}

	if got := c.Dropped(); got == 0 {
		t.Fatal("Conn.Dropped() == 0 after flooding a 4-slot buffer; drops are invisible")
	}
	// The buffer should be full, not overrun.
	if len(c.Send) > cap(c.Send) {
		t.Fatalf("send buffer overrun: %d > %d", len(c.Send), cap(c.Send))
	}
	// Everything published must be either delivered or counted - no line may
	// vanish without being accounted for somewhere.
	if int64(len(c.Send))+c.Dropped() != 200 {
		t.Errorf("delivered %d + dropped %d != 200 published", len(c.Send), c.Dropped())
	}
}

// A slow viewer must never stall the broadcaster or other viewers.
func TestSlowViewerDoesNotBlockOthers(t *testing.T) {
	hub := NewHub()
	slow := NewConn(2)
	fast := NewConn(500)
	hub.Join("room", slow)
	hub.Join("room", fast)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 400; i++ {
			hub.Publish("room", Line{Text: "x"})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("broadcaster blocked behind a slow viewer")
	}

	if len(fast.Send) != 400 {
		t.Errorf("fast viewer got %d of 400 lines", len(fast.Send))
	}
	if slow.Dropped() == 0 {
		t.Error("slow viewer dropped nothing, so its buffer was not actually the constraint")
	}
}

// The lifecycle the panel's buttons drive.
func TestLifecycleStartStop(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	if s.State() != StatusStopped {
		t.Fatalf("fresh server status = %q", s.State())
	}
	if err := mgr.Start(s.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := mgr.Start(s.ID); err == nil {
		t.Error("starting an already-starting server should fail")
	}

	waitFor(t, 12*time.Second, func() bool { return s.State() == StatusRunning })

	if err := mgr.Stop(s.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitFor(t, 8*time.Second, func() bool { return s.State() == StatusStopped })

	// A stopped server reports no player count at all - "0 of 20" would assert
	// that nobody is playing, which is a different fact from "not running".
	if snap := s.Snapshot(); snap["players"] != nil {
		t.Errorf("stopped server reported players = %v, want null", snap["players"])
	}
}

// Settings that need a restart must say so, and the port must be validated
// against the host before it is written rather than at container start.
func TestSettingsRestartFlagAndPortConflict(t *testing.T) {
	_, mgr := newTestAgent(t)
	list := mgr.List()
	s, other := list[0], list[1]

	startServer(t, mgr)

	need, err := mgr.ApplySettings(s, map[string]string{"spawn-monsters": "false"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(need) != 1 {
		t.Fatalf("requires_restart = %v, want one entry", need)
	}

	// immediate settings must not claim a restart is needed
	need, err = mgr.ApplySettings(s, map[string]string{"pvp": "false"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(need) != 0 {
		t.Errorf("pvp is immediate but reported %v", need)
	}

	if _, err := mgr.ApplySettings(s, map[string]string{"server-port": itoa(other.Port)}); err == nil {
		t.Error("taking another server's port should be rejected at save time")
	}
}

func TestCreateRejectsDuplicatePort(t *testing.T) {
	_, mgr := newTestAgent(t)
	taken := mgr.List()[0].Port

	if _, err := mgr.Create("Dupe", "paper", "1.20.4", taken, 0, 0, RuntimeSim); err == nil {
		t.Error("creating a server on an occupied port should fail")
	}
	s, err := mgr.Create("Fine", "paper", "1.20.4", 0, 0, 0, RuntimeSim)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if s.Port == taken {
		t.Errorf("auto-assigned port %d collides with an existing server", s.Port)
	}
}

func waitFor(t *testing.T, budget time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(80 * time.Millisecond)
	}
	t.Fatal("condition not met within budget")
}
