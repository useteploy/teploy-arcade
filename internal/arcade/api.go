package arcade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
)

type API struct {
	mgr *Manager
	hub *Hub
}

func (a *API) Routes(mux *http.ServeMux) {
	auth := a.mgr.auth

	// Reads are gated too. A configured panel was still handing an anonymous
	// caller the full server list, per-server settings (level-seed, ports,
	// online-mode, whitelist state) and the live event feed. Only /api/health
	// and /api/me stay open, because the proxy healthcheck and the login page
	// need them before a session exists.
	mux.HandleFunc("GET /api/host", auth.require(RoleViewer, a.getHost))
	mux.HandleFunc("GET /api/templates", auth.require(RoleViewer, a.getTemplates))
	mux.HandleFunc("GET /api/servers", auth.require(RoleViewer, a.listServers))
	mux.HandleFunc("GET /api/servers/{id}", auth.require(RoleViewer, a.getServer))
	mux.HandleFunc("GET /api/servers/{id}/settings", auth.require(RoleViewer, a.getSettings))
	mux.HandleFunc("GET /api/events", auth.require(RoleViewer, a.events))

	// Creating and destroying servers is an admin act; driving one is not.
	mux.HandleFunc("POST /api/servers", auth.require(RoleAdmin, a.createServer))
	mux.HandleFunc("DELETE /api/servers/{id}", auth.require(RoleAdmin, a.deleteServer))
	mux.HandleFunc("POST /api/servers/{id}/{action}", auth.require(RoleOperator, a.serverAction))
	mux.HandleFunc("PATCH /api/servers/{id}/settings", auth.require(RoleOperator, a.patchSettings))
	// Resource limits are a host-capacity decision, not a per-server setting, so
	// this sits with the admin routes rather than with patchSettings.
	mux.HandleFunc("PATCH /api/servers/{id}", auth.require(RoleAdmin, a.patchServer))
	// Reordering is a view preference, not a privileged change, so an operator
	// can arrange their own tab strip.
	mux.HandleFunc("POST /api/servers/order", auth.require(RoleOperator, a.reorderServers))

	// The socket authorises on connect; a viewer may watch but not type.
	mux.HandleFunc("GET /ws/console", auth.require(RoleViewer, a.console))

	a.RoutesPlugins(mux)
	a.RoutesImport(mux)
	a.RoutesExt(mux)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	// MaxBytesReader surfaces as a decode failure; report it as what it is.
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
			"error": fmt.Sprintf("request body is larger than the %d MB limit", maxBodyBytes>>20)})
		return
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (a *API) getHost(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, a.mgr.Host())
}

func (a *API) getTemplates(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"templates":      allTemplates(),
		"next_free_port": a.mgr.NextFreePort(25565),
		"docker":         dockerAvailable(),
	})
}

func (a *API) listServers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"servers": a.mgr.listSnapshot(),
		"host":    a.mgr.Host(),
	})
}

func (a *API) getServer(w http.ResponseWriter, r *http.Request) {
	s := a.mgr.Get(r.PathValue("id"))
	if s == nil {
		writeErr(w, 404, fmt.Errorf("no such server"))
		return
	}
	writeJSON(w, 200, s.Snapshot())
}

func (a *API) createServer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string  `json:"name"`
		Template string  `json:"template"`
		Version  string  `json:"version"`
		Port     int     `json:"port"`
		MemoryMB int     `json:"memory_mb"`
		CPU      float64 `json:"cpu"`
		Runtime  string  `json:"runtime"`
		Start    bool    `json:"start"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, err)
		return
	}
	s, err := a.mgr.Create(body.Name, body.Template, body.Version, body.Port, body.MemoryMB, body.CPU, body.Runtime)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	a.mgr.audit(actorOf(r), "server.create", s.ID, s.Name+" ("+s.Template+"/"+s.Runtime+")")
	if body.Start {
		if err := a.mgr.Start(s.ID); err != nil {
			log.Printf("autostart %s: %v", s.ID, err)
		}
	}
	writeJSON(w, 201, s.Snapshot())
}

// patchServer changes a server's resource allotment. Settings that live in
// server.properties go through patchSettings; these two are the container's own
// limits and only apply on the next start.
func (a *API) patchServer(w http.ResponseWriter, r *http.Request) {
	s := a.server(w, r)
	if s == nil {
		return
	}
	var body struct {
		MemoryMB int     `json:"memory_mb"`
		CPU      float64 `json:"cpu"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, err)
		return
	}
	pending, err := a.mgr.SetResources(s, body.MemoryMB, body.CPU)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	a.mgr.audit(actorOf(r), "server.resources", s.ID,
		fmt.Sprintf("memory=%dMB cpu=%g", s.MemoryMB, s.CPU))
	writeJSON(w, 200, map[string]any{
		"server": s.Snapshot(), "pending_restart": pending,
	})
}

func (a *API) reorderServers(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Order []string `json:"order"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, err)
		return
	}
	if len(body.Order) == 0 {
		writeErr(w, 400, fmt.Errorf("order is required"))
		return
	}
	if err := a.mgr.Reorder(body.Order); err != nil {
		writeErr(w, 500, err)
		return
	}
	a.mgr.audit(actorOf(r), "servers.reorder", "", fmt.Sprintf("%d servers", len(body.Order)))
	writeJSON(w, 200, map[string]any{"servers": a.mgr.listSnapshot()})
}

func (a *API) deleteServer(w http.ResponseWriter, r *http.Request) {
	if err := a.mgr.Delete(r.PathValue("id")); err != nil {
		writeErr(w, 400, err)
		return
	}
	a.mgr.audit(actorOf(r), "server.delete", r.PathValue("id"), "")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *API) serverAction(w http.ResponseWriter, r *http.Request) {
	id, action := r.PathValue("id"), r.PathValue("action")
	var err error
	detail := ""
	switch action {
	case "start":
		err = a.mgr.Start(id)
	case "stop":
		err = a.mgr.Stop(id)
	case "restart":
		err = a.mgr.Restart(id)
	case "kill":
		err = a.mgr.Kill(id)
	case "clear-failures":
		s := a.mgr.Get(id)
		if s == nil {
			writeErr(w, 404, fmt.Errorf("no such server"))
			return
		}
		a.mgr.ClearFailures(s, actorOf(r))
	case "command":
		// The actor comes from the session, never from the body. Taking it from
		// the body let anyone with the operator role POST a command attributed to
		// a colleague, into both the console echo and the audit log - and the
		// audit row carried an empty detail, so the forged command text was never
		// recorded anywhere. The console socket has always derived it this way.
		var body struct{ Text, Mode string }
		if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
			writeErr(w, 400, e)
			return
		}
		detail = strings.TrimSpace(body.Text)
		err = a.mgr.Send(id, body.Text, body.Mode, actorOf(r))
	default:
		writeErr(w, 404, fmt.Errorf("unknown action %q", action))
		return
	}
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	a.mgr.audit(actorOf(r), "server."+action, id, detail)
	s := a.mgr.Get(id)
	if s == nil {
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	writeJSON(w, 200, s.Snapshot())
}

func (a *API) getSettings(w http.ResponseWriter, r *http.Request) {
	s := a.mgr.Get(r.PathValue("id"))
	if s == nil {
		writeErr(w, 404, fmt.Errorf("no such server"))
		return
	}
	writeJSON(w, 200, map[string]any{
		"server": s.Snapshot(),
		"groups": a.mgr.SettingsView(s),
	})
}

func (a *API) patchSettings(w http.ResponseWriter, r *http.Request) {
	s := a.mgr.Get(r.PathValue("id"))
	if s == nil {
		writeErr(w, 404, fmt.Errorf("no such server"))
		return
	}
	var changes map[string]string
	if err := json.NewDecoder(r.Body).Decode(&changes); err != nil {
		writeErr(w, 400, err)
		return
	}
	needRestart, err := a.mgr.ApplySettings(s, changes)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	a.mgr.audit(actorOf(r), "server.settings", s.ID, fmt.Sprintf("%d key(s)", len(changes)))
	writeJSON(w, 200, map[string]any{
		"saved": true, "requires_restart": needRestart, "server": s.Snapshot(),
	})
}

// events is the panel-wide SSE feed: status transitions and metric samples, so
// the server list updates without polling.
func (a *API) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, fmt.Errorf("streaming unsupported"))
		return
	}
	// Subscribe before the 200. Once the event-stream header is out there is no
	// status code left to refuse with, and a refusal the panel can actually
	// read is the point of the cap.
	ch, err := a.mgr.Subscribe()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	defer a.mgr.Unsubscribe(ch)

	// This response never ends, so the server's write deadline has to go.
	clearStreamDeadlines(w)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)

	fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{
		"event": "hello", "servers": a.mgr.listSnapshot(),
	}))
	fl.Flush()

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", msg)
			fl.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

// liveSession resolves the caller's session again, now, from the cookie it sent
// with the request. Long-lived connections need this: the session in the request
// context is a pointer captured at upgrade and stays non-nil after the user is
// gone.
func (a *API) liveSession(r *http.Request) *Session {
	c, err := r.Cookie("gss_session")
	if err != nil {
		return nil
	}
	return a.mgr.auth.Session(c.Value)
}

// console is the per-server WebSocket.
//
// Inbound messages route to the game server's stdin and are never fanned out to
// other viewers (NEUTRON-ISSUES NI-002). Outbound is normal room fanout.
func (a *API) console(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("server")
	s := a.mgr.Get(id)
	if s == nil {
		http.Error(w, "no such server", 404)
		return
	}

	// Lifted before the upgrade, not after: Accept hijacks the connection and
	// keeps whatever deadline the server armed on it, and once hijacked the
	// library owns the socket and there is no ResponseWriter left to reach it
	// through. A console open longer than WriteTimeout would go mute.
	clearStreamDeadlines(w)

	// The Origin check is the ONLY defence here: WebSockets are not subject to
	// CORS, so with it disabled any page the operator visits could open a
	// socket to this panel and run console commands on their servers. The
	// library's default requires Origin's host to equal Host, which is what we
	// want; -origin adds hostnames for a deployment fronted by a proxy.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: allowedOrigins,
	})
	if err != nil {
		log.Printf("ws rejected from origin %q: %v", r.Header.Get("Origin"), err)
		return
	}
	defer c.CloseNow()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn := NewConn(256)
	replay, seq, capacity := a.hub.Join(id, conn)
	defer a.hub.Leave(id, conn)

	// Replay first so a refresh never lands on a blank console.
	_ = c.Write(ctx, websocket.MessageText, mustJSON(map[string]any{
		"t": "replay", "lines": replay, "count": len(replay),
		"buffer_capacity": capacity, "seq": seq,
		"server": s.Snapshot(),
	}))
	a.mgr.pushPlayers(s)

	// writer: drains the room, and reports its own shed count so the UI can be
	// honest about backpressure instead of silently losing output.
	go func() {
		defer cancel()
		var lastDropped int64
		t := time.NewTicker(700 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-conn.Send:
				if !ok {
					return
				}
				if err := c.Write(ctx, websocket.MessageText, msg); err != nil {
					return
				}
			case <-t.C:
				if d := conn.Dropped(); d > lastDropped {
					n := d - lastDropped
					lastDropped = d
					_ = c.Write(ctx, websocket.MessageText, mustJSON(map[string]any{
						"t": "dropped", "count": n, "total": d,
						"ts": time.Now().Format("15:04:05"),
					}))
				}
			}
		}
	}()

	// reader: commands only.
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var in struct {
			T    string `json:"t"`
			ID   string `json:"id"`
			Text string `json:"text"`
			Mode string `json:"mode"`
		}
		if err := json.Unmarshal(data, &in); err != nil {
			continue
		}
		if in.T != "command" || strings.TrimSpace(in.Text) == "" {
			continue
		}
		// in.Actor is deliberately ignored. With auth on it was already
		// overwritten with the session user below; with auth off it was written
		// straight into the audit log, so a client could name itself anything
		// it liked in the only record of who ran what.
		actor := "panel"
		sess := sessionFrom(r)
		// Re-resolve per command rather than trusting the session the context
		// carries: that one was resolved at upgrade and is captured for the life
		// of the socket, so deleting a user revoked their cookie but not their
		// open console - a sacked admin kept full command access until they
		// happened to disconnect.
		if a.mgr.auth.Enabled() {
			if sess = a.liveSession(r); sess == nil {
				_ = c.Write(ctx, websocket.MessageText, mustJSON(map[string]any{
					"t": "command_ack", "id": in.ID, "accepted": false,
					"error": "your session is no longer valid; sign in again",
				}))
				_ = c.Close(websocket.StatusPolicyViolation, "session revoked")
				return
			}
		}
		if sess != nil {
			actor = sess.User
			if roleRank[sess.Role] < roleRank[RoleOperator] {
				_ = c.Write(ctx, websocket.MessageText, mustJSON(map[string]any{
					"t": "command_ack", "id": in.ID, "accepted": false,
					"error": "the viewer role cannot run console commands",
				}))
				continue
			}
		}

		err = a.mgr.Send(id, strings.TrimSpace(in.Text), in.Mode, actor)
		if err == nil {
			a.mgr.audit(actor, "console.command", id, strings.TrimSpace(in.Text))
		}

		// An ack is what lets the UI distinguish "sent" from "swallowed".
		ack := map[string]any{"t": "command_ack", "id": in.ID, "accepted": err == nil}
		if err != nil {
			ack["error"] = err.Error()
		}
		_ = c.Write(ctx, websocket.MessageText, mustJSON(ack))
	}
}
