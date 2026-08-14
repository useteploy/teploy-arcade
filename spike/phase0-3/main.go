// Phase 0.3 spike — WS transport + command-routing handler.
//
// DoD (PLAN.md §10 Phase 0.3): browser ↔ agent echo demo with command-routing
// shape. Picks the WS lib (coder/websocket, the maintained nhooyr successor),
// writes the Upgrader (NI-003), and builds a custom console handler that routes
// inbound messages to an app callback instead of auto-broadcasting them (NI-002
// reference implementation — this is the shape the upstream fix should take).
//
// Run the demo:
//
//	go run .                 # serves http://localhost:8080
//
// Open http://localhost:8080 in a browser; the console streams fake lines.
// Typing a command sends it to the agent (onCommand), which broadcasts
// "executed: <cmd>" — it is NOT echoed to other viewers directly.
//
// Automated DoD proof:
//
//	go test .
package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/neutron-dev/neutron-go/neutron"
	"github.com/neutron-dev/neutron-go/neutronrealtime"
)

//go:embed web/index.html
var webFS embed.FS

const room = "server:1"

type coderConn struct{ c *websocket.Conn }

func (cc *coderConn) ReadMessage(ctx context.Context) ([]byte, error) {
	_, b, err := cc.c.Read(ctx)
	return b, err
}
func (cc *coderConn) WriteMessage(ctx context.Context, msg []byte) error {
	return cc.c.Write(ctx, websocket.MessageText, msg)
}
func (cc *coderConn) Close() error { return cc.c.CloseNow() }

func coderUpgrader(w http.ResponseWriter, r *http.Request) (neutronrealtime.WebSocketConn, error) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return nil, err
	}
	return &coderConn{c: c}, nil
}

func connID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// consoleHandler is the NI-002 reference implementation. Unlike
// neutronrealtime.WebSocketHandler (which broadcasts inbound messages to the
// room), it routes inbound messages to onCommand. For a console, inbound =
// commands that the agent must execute — they must never be echoed to other
// viewers. Outbound (room → socket) is the normal fanout.
func consoleHandler(
	hub *neutronrealtime.Hub,
	room string,
	upgrader neutronrealtime.Upgrader,
	onCommand func([]byte),
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			originURL, err := url.Parse(origin)
			if err != nil || originURL.Host != r.Host {
				http.Error(w, "Origin not allowed", http.StatusForbidden)
				return
			}
		}

		ws, err := upgrader(w, r)
		if err != nil {
			http.Error(w, "WebSocket upgrade failed", http.StatusBadRequest)
			return
		}
		defer ws.Close()

		conn := neutronrealtime.NewConn(connID(), 256)
		hub.Register(conn)
		hub.Subscribe(room, conn)
		defer func() {
			hub.Unsubscribe(room, conn)
			hub.Unregister(conn)
		}()

		ctx := r.Context()
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for msg := range conn.Send {
				if err := ws.WriteMessage(ctx, msg); err != nil {
					return
				}
			}
		}()

		for {
			msg, err := ws.ReadMessage(ctx)
			if err != nil {
				break
			}
			onCommand(msg)
		}
		wg.Wait()
	})
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	data, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

// newApp builds the demo agent app. Shared by main (serves on :8080) and the
// routing test (served via httptest on a random port).
func newApp() (*neutron.App, *neutronrealtime.Hub) {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	hub := neutronrealtime.NewHub()

	var lineN atomic.Int64
	onCommand := func(msg []byte) {
		hub.Broadcast(room, []byte(fmt.Sprintf("executed: %s [%s]", string(msg), time.Now().Format("15:04:05"))))
	}

	app := neutron.New(neutron.WithLogger(logger))
	r := app.Router()
	r.HandleFunc("GET /", indexHandler)
	r.Handle("GET /ws/console", consoleHandler(hub, room, coderUpgrader, onCommand))

	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			n := lineN.Add(1)
			hub.Broadcast(room, []byte(fmt.Sprintf("[server] tick line %d @ %s", n, time.Now().Format("15:04:05"))))
		}
	}()

	return app, hub
}

func main() {
	app, _ := newApp()
	app.Run(":8080")
}
