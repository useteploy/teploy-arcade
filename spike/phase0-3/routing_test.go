package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func collect(ctx context.Context, t *testing.T, c *websocket.Conn) []string {
	t.Helper()
	var out []string
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		for {
			_, msg, err := c.Read(ctx)
			if err != nil {
				close(done)
				return
			}
			mu.Lock()
			out = append(out, string(msg))
			mu.Unlock()
		}
	}()
	<-done
	mu.Lock()
	defer mu.Unlock()
	return out
}

func TestCommandRouting(t *testing.T) {
	app, _ := newApp()
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	wsURL := strings.Replace(server.URL, "http", "ws", 1) + "/ws/console"

	ctxConnect, cancelConnect := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelConnect()
	connA, _, err := websocket.Dial(ctxConnect, wsURL, nil)
	if err != nil {
		t.Fatalf("dial A: %v", err)
	}
	defer connA.CloseNow()
	connB, _, err := websocket.Dial(ctxConnect, wsURL, nil)
	if err != nil {
		t.Fatalf("dial B: %v", err)
	}
	defer connB.CloseNow()

	time.Sleep(300 * time.Millisecond)

	if err := connA.Write(context.Background(), websocket.MessageText, []byte("say hi")); err != nil {
		t.Fatalf("A write: %v", err)
	}

	winCtx, cancelWin := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancelWin()
	gotB := collect(winCtx, t, connB)

	winCtx2, cancelWin2 := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancelWin2()
	gotA := collect(winCtx2, t, connA)

	for _, m := range gotB {
		if m == "say hi" {
			t.Fatalf("FAIL: B received the raw command — inbound was broadcast (NI-002 not resolved)")
		}
	}

	bHasExecuted := contains(gotB, "executed: say hi")
	aHasExecuted := contains(gotA, "executed: say hi")
	if !bHasExecuted || !aHasExecuted {
		t.Fatalf("FAIL: expected both viewers to receive 'executed: say hi'.\nA got: %v\nB got: %v", gotA, gotB)
	}

	bHasFanout := false
	for _, m := range gotB {
		if strings.HasPrefix(m, "[server] tick") {
			bHasFanout = true
			break
		}
	}
	if !bHasFanout {
		t.Fatalf("FAIL: B received no emitter lines — room fanout broken. B got: %v", gotB)
	}

	t.Logf("PASS: inbound routed (B did NOT receive raw command); both received 'executed:'; fanout confirmed.")
	t.Logf("A sample: %d msgs, B sample: %d msgs", len(gotA), len(gotB))
}

func contains(s []string, sub string) bool {
	for _, v := range s {
		if strings.Contains(v, sub) {
			return true
		}
	}
	return false
}
