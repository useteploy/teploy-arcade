package arcade

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Routes added by phases 4-8: metrics, files, backups, auth, users, audit.

func (a *API) RoutesExt(mux *http.ServeMux) {
	auth := a.mgr.auth

	// ---- health + capabilities: the two endpoints every teploy service exposes
	mux.HandleFunc("GET /api/health", a.health)
	mux.HandleFunc("GET /api/capabilities", auth.require(RoleViewer, a.capabilities))

	// ---- Phase 4: metrics
	mux.HandleFunc("GET /api/servers/{id}/metrics", auth.require(RoleViewer, a.serverMetrics))
	mux.HandleFunc("GET /api/metrics", auth.require(RoleViewer, a.hostMetrics))

	// ---- Phase 5: files
	mux.HandleFunc("GET /api/servers/{id}/files", auth.require(RoleViewer, a.listFiles))
	mux.HandleFunc("GET /api/servers/{id}/file", auth.require(RoleViewer, a.readFile))
	mux.HandleFunc("PUT /api/servers/{id}/file", auth.require(RoleOperator, a.writeFile))
	mux.HandleFunc("DELETE /api/servers/{id}/file", auth.require(RoleOperator, a.deleteFile))
	mux.HandleFunc("POST /api/servers/{id}/mkdir", auth.require(RoleOperator, a.mkdir))
	mux.HandleFunc("GET /api/servers/{id}/download", auth.require(RoleViewer, a.download))

	// ---- Phase 5: backups
	mux.HandleFunc("GET /api/servers/{id}/backups", auth.require(RoleViewer, a.listBackups))
	mux.HandleFunc("POST /api/servers/{id}/backups", auth.require(RoleOperator, a.createBackup))
	mux.HandleFunc("POST /api/servers/{id}/backups/{bid}/restore", auth.require(RoleAdmin, a.restoreBackup))
	mux.HandleFunc("DELETE /api/servers/{id}/backups/{bid}", auth.require(RoleAdmin, a.deleteBackup))

	// ---- players: whitelist / operators / bans
	mux.HandleFunc("GET /api/servers/{id}/players", auth.require(RoleViewer, a.getPlayers))
	mux.HandleFunc("POST /api/servers/{id}/players", auth.require(RoleOperator, a.addPlayer))
	mux.HandleFunc("DELETE /api/servers/{id}/players", auth.require(RoleOperator, a.removePlayer))

	// ---- scheduler
	mux.HandleFunc("GET /api/servers/{id}/tasks", auth.require(RoleViewer, a.listTasks))
	mux.HandleFunc("POST /api/servers/{id}/tasks", auth.require(RoleOperator, a.createTask))
	mux.HandleFunc("PATCH /api/servers/{id}/tasks/{tid}", auth.require(RoleOperator, a.updateTask))
	mux.HandleFunc("DELETE /api/servers/{id}/tasks/{tid}", auth.require(RoleOperator, a.deleteTask))
	mux.HandleFunc("POST /api/servers/{id}/tasks/{tid}/run", auth.require(RoleOperator, a.runTask))

	// ---- Phase 8: auth, users, audit
	mux.HandleFunc("GET /api/me", a.me)
	mux.HandleFunc("POST /api/login", a.login)
	mux.HandleFunc("POST /api/logout", a.logout)
	mux.HandleFunc("POST /api/setup", a.setup)
	mux.HandleFunc("GET /api/users", auth.require(RoleAdmin, a.listUsers))
	mux.HandleFunc("POST /api/users", auth.require(RoleAdmin, a.createUser))
	mux.HandleFunc("DELETE /api/users/{name}", auth.require(RoleAdmin, a.deleteUser))
	// Viewer, not admin: this is the route a viewer uses to change their own
	// password. Who may change WHOSE password is decided inside the handler.
	mux.HandleFunc("POST /api/users/{name}/password", auth.requireEvenLockedOut(RoleViewer, a.setPassword))
	mux.HandleFunc("GET /api/audit", auth.require(RoleViewer, a.getAudit))

	a.mcpRoutes(mux)
}

func (a *API) server(w http.ResponseWriter, r *http.Request) *Server {
	s := a.mgr.Get(r.PathValue("id"))
	if s == nil {
		writeErr(w, 404, fmt.Errorf("no such server"))
		return nil
	}
	return s
}

// ------------------------------------------------------------- Phase 4

func windowOf(r *http.Request) (time.Duration, int) {
	switch r.URL.Query().Get("window") {
	case "1m":
		return time.Minute, 60
	case "5m":
		return 5 * time.Minute, 90
	case "30m":
		return 30 * time.Minute, 120
	case "1h":
		return time.Hour, 140
	case "4h":
		return 4 * time.Hour, 160
	default:
		return 5 * time.Minute, 90
	}
}

func (a *API) serverMetrics(w http.ResponseWriter, r *http.Request) {
	s := a.server(w, r)
	if s == nil {
		return
	}
	win, points := windowOf(r)
	writeJSON(w, 200, map[string]any{
		"samples":   a.mgr.metrics.Series(s.ID, win, points),
		"limit_mb":  s.MemoryMB,
		"limit_cpu": s.CPU,
	})
}

func (a *API) hostMetrics(w http.ResponseWriter, r *http.Request) {
	win, points := windowOf(r)
	writeJSON(w, 200, map[string]any{
		"samples": a.mgr.metrics.HostSeries(win, points),
		"host":    a.mgr.Host(),
	})
}

// ------------------------------------------------------------- Phase 5

func (a *API) listFiles(w http.ResponseWriter, r *http.Request) {
	s := a.server(w, r)
	if s == nil {
		return
	}
	path := r.URL.Query().Get("path")
	entries, err := a.mgr.ListFiles(s, path)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"path": path, "entries": entries})
}

func (a *API) readFile(w http.ResponseWriter, r *http.Request) {
	s := a.server(w, r)
	if s == nil {
		return
	}
	content, err := a.mgr.ReadFile(s, r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"path": r.URL.Query().Get("path"), "content": content})
}

func (a *API) writeFile(w http.ResponseWriter, r *http.Request) {
	s := a.server(w, r)
	if s == nil {
		return
	}
	var body struct{ Path, Content string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := a.mgr.WriteFile(s, body.Path, body.Content); err != nil {
		writeErr(w, 400, err)
		return
	}
	a.mgr.audit(actorOf(r), "file.write", s.ID, body.Path)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *API) deleteFile(w http.ResponseWriter, r *http.Request) {
	s := a.server(w, r)
	if s == nil {
		return
	}
	path := r.URL.Query().Get("path")
	if err := a.mgr.DeletePath(s, path); err != nil {
		writeErr(w, 400, err)
		return
	}
	a.mgr.audit(actorOf(r), "file.delete", s.ID, path)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *API) mkdir(w http.ResponseWriter, r *http.Request) {
	s := a.server(w, r)
	if s == nil {
		return
	}
	var body struct{ Path string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := a.mgr.MkDir(s, body.Path); err != nil {
		writeErr(w, 400, err)
		return
	}
	a.mgr.audit(actorOf(r), "file.mkdir", s.ID, body.Path)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *API) download(w http.ResponseWriter, r *http.Request) {
	s := a.server(w, r)
	if s == nil {
		return
	}
	// A large world archive over a slow link outruns the server's WriteTimeout,
	// and the deadline is absolute, not idle-based - the download would stop
	// mid-file with no error the operator can see.
	clearStreamDeadlines(w)

	rc, name, size, err := a.mgr.OpenForDownload(s, r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	_, _ = io.Copy(w, rc)
}

func (a *API) listBackups(w http.ResponseWriter, r *http.Request) {
	s := a.server(w, r)
	if s == nil {
		return
	}
	list, err := a.mgr.ListBackups(s)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"backups": list,
		"locked":  a.mgr.backupLocked(s.ID),
	})
}

func (a *API) createBackup(w http.ResponseWriter, r *http.Request) {
	s := a.server(w, r)
	if s == nil {
		return
	}
	// Archiving or unpacking a multi-GB world outruns the server's WriteTimeout,
	// and the client then sees EOF while the archive lands on disk anyway - the
	// same "the panel lied about a backup" outcome M13 was filed to stop.
	clearStreamDeadlines(w)
	var body struct{ Note string }
	_ = json.NewDecoder(r.Body).Decode(&body)

	b, err := a.mgr.CreateBackup(s, body.Note, actorOf(r))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 201, b)
}

func (a *API) restoreBackup(w http.ResponseWriter, r *http.Request) {
	s := a.server(w, r)
	if s == nil {
		return
	}
	// Archiving or unpacking a multi-GB world outruns the server's WriteTimeout,
	// and the client then sees EOF while the archive lands on disk anyway - the
	// same "the panel lied about a backup" outcome M13 was filed to stop.
	clearStreamDeadlines(w)
	if err := a.mgr.RestoreBackup(s, r.PathValue("bid"), actorOf(r)); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *API) deleteBackup(w http.ResponseWriter, r *http.Request) {
	s := a.server(w, r)
	if s == nil {
		return
	}
	if err := a.mgr.DeleteBackup(s, r.PathValue("bid"), actorOf(r)); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ------------------------------------------------------------- Phase 8

func (a *API) me(w http.ResponseWriter, r *http.Request) {
	auth := a.mgr.auth
	s := sessionFrom(r)
	// "auth is off" and "there are no accounts" are different states, and only
	// the second one needs setup. A panel run with -no-auth has accounts and is
	// not claimable - reporting needs_setup there put the create-first-admin
	// form in front of an operator whose panel already had an admin.
	unclaimed := !auth.HasUsers()
	resp := map[string]any{
		"auth_enabled": auth.Enabled(),
		"needs_setup":  unclaimed,
	}
	if s != nil {
		resp["user"] = map[string]any{"name": s.User, "role": s.Role}
		// The panel puts a change-password screen in front of everything else
		// while this is set, and the API refuses the rest regardless.
		resp["must_change"] = auth.MustChangePassword(s.User)
	} else if unclaimed {
		// No account, no session - so no user. This used to answer with an
		// invented {name: "local", role: "admin"}, which the settings screen
		// then rendered as "Signed in as local (admin)". That conflates two
		// different things: what the caller is ALLOWED to do (with no accounts,
		// everything) and who the caller IS (nobody). It reads as though an
		// admin account already exists, in precisely the state where the
		// operator most needs to know that one does not - the panel is
		// unclaimed and anyone who can reach it has full control.
		//
		// The authority is still reported, under a name that cannot be mistaken
		// for an account.
		resp["effective_role"] = RoleAdmin
		resp["unclaimed"] = true
	}
	writeJSON(w, 200, resp)
}

func setSessionCookie(w http.ResponseWriter, s *Session) {
	http.SetCookie(w, &http.Cookie{
		Name:     "gss_session",
		Value:    s.Token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  s.Expires,
	})
}

func (a *API) login(w http.ResponseWriter, r *http.Request) {
	var body struct{ Name, Password string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, err)
		return
	}
	s, err := a.mgr.auth.Login(body.Name, body.Password)
	if err != nil {
		a.mgr.audit(body.Name, "auth.login_failed", "", "")
		writeErr(w, 401, err)
		return
	}
	setSessionCookie(w, s)
	a.mgr.audit(s.User, "auth.login", "", s.Role)
	writeJSON(w, 200, map[string]any{"user": map[string]any{"name": s.User, "role": s.Role}})
}

func (a *API) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("gss_session"); err == nil {
		a.mgr.auth.Logout(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: "gss_session", Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// setup creates the first admin. Only available while no users exist, so it
// cannot be used to add an admin to a configured panel.
func (a *API) setup(w http.ResponseWriter, r *http.Request) {
	if a.mgr.auth.Enabled() {
		writeErr(w, 409, fmt.Errorf("this panel already has users; sign in instead"))
		return
	}
	var body struct {
		Name, Password, Token string
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, err)
		return
	}
	// The first account is what makes this panel enforce anything, and admin
	// here means creating containers as root on the host. Gate it on the token
	// printed to the log at startup, so claiming this panel requires access to
	// the machine rather than merely reaching it over the network.
	token := strings.TrimSpace(body.Token)
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("X-Setup-Token"))
	}
	if !a.mgr.auth.CheckBootstrapToken(token) {
		if a.mgr.auth.BootstrapExpired() {
			writeErr(w, 403, fmt.Errorf("the first-run setup window has closed; restart the panel to get a fresh token from its log"))
			return
		}
		writeErr(w, 403, fmt.Errorf("a setup token is required: it is printed in the panel's log at startup (journalctl -u teploy-arcade)"))
		return
	}
	// CreateFirstUser re-checks emptiness under the same lock as the insert. The
	// Enabled() check above is a courtesy for the 409; on its own it is a
	// separate step from the create, and two concurrent first-run requests both
	// cleared it and both got an admin.
	u, err := a.mgr.auth.CreateFirstUser(body.Name, body.Password)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	s, err := a.mgr.auth.Login(body.Name, body.Password)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	setSessionCookie(w, s)
	a.mgr.audit(u.Name, "auth.setup", "", "first admin created")
	writeJSON(w, 201, map[string]any{"user": map[string]any{"name": u.Name, "role": u.Role}})
}

func (a *API) listUsers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"users": a.mgr.auth.Users()})
}

func (a *API) createUser(w http.ResponseWriter, r *http.Request) {
	var body struct{ Name, Password, Role string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, err)
		return
	}
	u, err := a.mgr.auth.CreateUser(body.Name, body.Password, body.Role)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	a.mgr.audit(actorOf(r), "user.create", u.Name, u.Role)
	writeJSON(w, 201, map[string]any{"name": u.Name, "role": u.Role})
}

// setPassword serves both "change my own" and "an admin resets someone
// else's". A user may always change their own with the current password; only
// an admin may set another account's, and doing so marks it must-change.
func (a *API) setPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, err)
		return
	}

	target := r.PathValue("name")
	auth := a.mgr.auth
	s := sessionFrom(r)

	// With auth off there is no session and no "self" to be; treat it as the
	// admin path so a local run can still repair an account.
	self, byAdmin, actor := false, true, "local"
	if s != nil {
		self = strings.EqualFold(s.User, target)
		byAdmin = !self
		actor = s.User
		if byAdmin && roleRank[s.Role] < roleRank[RoleAdmin] {
			writeJSON(w, 403, map[string]string{
				"error": "only an admin can set another user's password"})
			return
		}
	}

	token := ""
	if c, err := r.Cookie("gss_session"); err == nil {
		token = c.Value
	}
	if err := auth.SetPassword(target, body.Current, body.New, token, byAdmin); err != nil {
		writeErr(w, 400, err)
		return
	}
	action := "user.password.set"
	if self {
		action = "user.password.change"
	}
	a.mgr.audit(actor, action, target, "")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *API) deleteUser(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := a.mgr.auth.DeleteUser(name); err != nil {
		writeErr(w, 400, err)
		return
	}
	a.mgr.audit(actorOf(r), "user.delete", name, "")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *API) getAudit(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	writeJSON(w, 200, map[string]any{"entries": a.mgr.auth.Audit(limit)})
}

// health is the endpoint the Teploy proxy healthcheck hits. Cheap and
// dependency-free on purpose: it must answer while servers are misbehaving.
func (a *API) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"ok": true, "version": agentVersion, "servers": len(a.mgr.List()),
	})
}

// capabilities tells a client what this build can actually do, so the UI and
// any MCP consumer stop guessing. Mirrors teploy-dash's /api/capabilities.
func (a *API) capabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"version":  agentVersion,
		"docker":   dockerAvailable(),
		"runtimes": []string{RuntimeSim, RuntimeDocker},
		"auth":     a.mgr.auth.Enabled(),
		"mcp":      true,
		"console":  map[string]any{"websocket": true, "ring_buffer": ringSize},
		"features": map[string]bool{
			"files": true, "backups": true, "metrics": true, "audit": true,
			"import":            true,
			"scheduled_backups": false, "disk_quota": false, "plugins": true,
		},
	})
}

// ------------------------------------------------------------- players

func (a *API) getPlayers(w http.ResponseWriter, r *http.Request) {
	s := a.server(w, r)
	if s == nil {
		return
	}
	out, err := a.mgr.PlayerLists(s)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, out)
}

func (a *API) addPlayer(w http.ResponseWriter, r *http.Request) {
	s := a.server(w, r)
	if s == nil {
		return
	}
	var body struct{ List, Name, Reason string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := a.mgr.AddToList(s, PlayerList(body.List), body.Name, body.Reason, actorOf(r)); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *API) removePlayer(w http.ResponseWriter, r *http.Request) {
	s := a.server(w, r)
	if s == nil {
		return
	}
	q := r.URL.Query()
	if err := a.mgr.RemoveFromList(s, PlayerList(q.Get("list")), q.Get("name"), actorOf(r)); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ------------------------------------------------------------ scheduler

func (a *API) listTasks(w http.ResponseWriter, r *http.Request) {
	s := a.server(w, r)
	if s == nil {
		return
	}
	writeJSON(w, 200, map[string]any{"tasks": a.mgr.sched.TaskView(s.ID)})
}

func (a *API) createTask(w http.ResponseWriter, r *http.Request) {
	s := a.server(w, r)
	if s == nil {
		return
	}
	var body Task
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, err)
		return
	}
	body.ServerID = s.ID
	t, err := a.mgr.sched.Add(&body)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	a.mgr.audit(actorOf(r), "task.create", s.ID, t.Name+" @ "+t.Time)
	writeJSON(w, 201, t)
}

func (a *API) updateTask(w http.ResponseWriter, r *http.Request) {
	var body Task
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, err)
		return
	}
	t, err := a.mgr.sched.Update(r.PathValue("tid"), func(t *Task) {
		if body.Name != "" {
			t.Name = body.Name
		}
		if body.Commands != "" {
			t.Commands = body.Commands
		}
		if body.Time != "" {
			t.Time = body.Time
		}
		t.Repeat = body.Repeat
		t.Enabled = body.Enabled
	})
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	a.mgr.audit(actorOf(r), "task.update", r.PathValue("id"), t.Name)
	writeJSON(w, 200, t)
}

func (a *API) deleteTask(w http.ResponseWriter, r *http.Request) {
	if err := a.mgr.sched.Delete(r.PathValue("tid")); err != nil {
		writeErr(w, 404, err)
		return
	}
	a.mgr.audit(actorOf(r), "task.delete", r.PathValue("id"), r.PathValue("tid"))
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *API) runTask(w http.ResponseWriter, r *http.Request) {
	if err := a.mgr.sched.Run(r.PathValue("tid"), actorOf(r)); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
