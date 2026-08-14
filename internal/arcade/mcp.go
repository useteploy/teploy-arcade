package arcade

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/useteploy/teploy-arcade/internal/mcp"
)

// MCP integration. The endpoint lives at POST /api/mcp with its own bearer
// tokens, minted and revoked at /api/mcp-tokens behind the normal session auth
// — same split as teploy-dash, so a token leaking never grants panel access.
//
// The Backend below is deliberately narrower than the HTTP API: an agent gets
// reads, the lifecycle verbs and backups. No delete, no restore, no user
// management, no kill.

// lastUseResolution is how precisely token last-use is tracked. Minutes are
// plenty for "when did this agent last talk to us", and it turns a
// per-request write into a per-minute one.
const lastUseResolution = 60

type mcpToken struct {
	Name    string `json:"name"`
	Hash    string `json:"hash"` // sha256 of the token; the plaintext is shown once
	Created int64  `json:"created"`
	LastUse int64  `json:"last_use"`
}

type mcpTokens struct {
	mu   sync.RWMutex
	path string
	toks []mcpToken
}

func newMCPTokens(dataDir string) *mcpTokens {
	t := &mcpTokens{path: filepath.Join(dataDir, "mcp-tokens.json")}
	if b, err := os.ReadFile(t.path); err == nil {
		if err := json.Unmarshal(b, &t.toks); err != nil {
			quarantine(t.path, err)
		}
	}
	return t
}

func (t *mcpTokens) save() error {
	b, err := json.MarshalIndent(t.toks, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(t.path, b, 0o600)
}

func (t *mcpTokens) Issue(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("name is required")
	}
	// Fail closed rather than issue "tpa_" with an empty random half: that token
	// would be stored as a valid hash, so anyone who guessed the bare prefix
	// would authenticate as this token for the rest of its life.
	suffix, err := randomHex(24)
	if err != nil {
		return "", err
	}
	raw := "tpa_" + suffix
	t.mu.Lock()
	defer t.mu.Unlock()
	t.toks = append(t.toks, mcpToken{
		Name: name, Hash: hashPassword(raw, "mcp"), Created: time.Now().Unix(),
	})
	return raw, t.save()
}

func (t *mcpTokens) Check(raw string) bool {
	if raw == "" {
		return false
	}
	h := hashPassword(raw, "mcp")
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now().Unix()
	for i := range t.toks {
		// Constant-time, like the password path: a plain == returns at the
		// first differing byte, which tells a caller timing how much of a
		// guessed token's hash was right.
		if subtle.ConstantTimeCompare([]byte(t.toks[i].Hash), []byte(h)) == 1 {
			// Debounced. Rewriting the file on every authenticated call meant
			// an agent under load kept the token store permanently mid-write;
			// one crash in that window destroyed every token.
			if now-t.toks[i].LastUse >= lastUseResolution {
				t.toks[i].LastUse = now
				if err := t.save(); err != nil {
					log.Printf("mcp: could not record token use: %v", err)
				}
			}
			return true
		}
	}
	return false
}

func (t *mcpTokens) List() []map[string]any {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]map[string]any, 0, len(t.toks))
	for _, x := range t.toks {
		out = append(out, map[string]any{
			"name": x.Name, "created": x.Created, "last_use": x.LastUse,
		})
	}
	return out
}

func (t *mcpTokens) Revoke(name string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, x := range t.toks {
		if x.Name == name {
			t.toks = append(t.toks[:i], t.toks[i+1:]...)
			return t.save()
		}
	}
	return fmt.Errorf("no such token")
}

// ---------------------------------------------------------------- backend

type mcpBackend struct{ m *Manager }

func (b mcpBackend) js(v any) (string, error) {
	out, err := json.MarshalIndent(v, "", "  ")
	return string(out), err
}

func (b mcpBackend) server(id string) (*Server, error) {
	s := b.m.Get(id)
	if s == nil {
		return nil, fmt.Errorf("no server with id %q — call arcade_list_servers first", id)
	}
	return s, nil
}

func (b mcpBackend) ListServers() (string, error) { return b.js(b.m.listSnapshot()) }

func (b mcpBackend) GetServer(id string) (string, error) {
	s, err := b.server(id)
	if err != nil {
		return "", err
	}
	// Snapshot already carries a locked copy of the settings map; reading
	// s.Props directly here raced reloadProps and crashed the process.
	return b.js(s.Snapshot())
}

func (b mcpBackend) ConsoleTail(id string, lines int) (string, error) {
	if _, err := b.server(id); err != nil {
		return "", err
	}
	tail := b.m.hub.Tail(id, lines)
	var sb strings.Builder
	for _, l := range tail {
		fmt.Fprintf(&sb, "%s %-5s %s\n", l.TS, strings.ToUpper(l.Level), l.Text)
	}
	if sb.Len() == 0 {
		return "(no console output buffered; the server may never have started)", nil
	}
	return sb.String(), nil
}

// mcpBlockedVerbs are the console commands an agent may not run, for the same
// reason arcade_lifecycle refuses kill: they hand out operator rights, remove a
// player's access or take the server down. An agent is told to read the result
// of its commands back from the console, and console output is written by
// players, plugins and the MOTD - so a chat line asking for op must not be able
// to travel back out as an op command.
var mcpBlockedVerbs = map[string]bool{
	"op": true, "deop": true, "ban": true, "ban-ip": true,
	"pardon": true, "pardon-ip": true, "whitelist": true, "stop": true,
}

// consoleVerb pulls the command word out of a console line. The leading slash
// is tolerated because a model will type one however the tool is described,
// and the verb check has to see the same command the game will.
func consoleVerb(text string) string {
	v := strings.TrimPrefix(strings.TrimSpace(text), "/")
	if i := strings.IndexAny(v, " \t"); i >= 0 {
		v = v[:i]
	}
	return strings.ToLower(v)
}

func (b mcpBackend) SendCommand(id, text string) (string, error) {
	if _, err := b.server(id); err != nil {
		return "", err
	}
	// A line break would smuggle a second command past the verb check below:
	// the game console reads each line on its own.
	if strings.ContainsAny(text, "\r\n") {
		return "", fmt.Errorf("a console command cannot contain a line break; send one command per call")
	}
	if v := consoleVerb(text); mcpBlockedVerbs[v] {
		return "", fmt.Errorf("%q is not available over MCP: it grants or removes access, or takes the server down, so it stays a human decision in the panel. Use arcade_lifecycle to stop or restart a server", v)
	}
	if err := b.m.Send(id, text, "command", "mcp"); err != nil {
		return "", err
	}
	b.m.audit("mcp", "console.command", id, text)
	return fmt.Sprintf("Sent %q. Read the result with arcade_console_tail.", text), nil
}

func (b mcpBackend) Lifecycle(id, action string) (string, error) {
	s, err := b.server(id)
	if err != nil {
		return "", err
	}
	switch action {
	case "start":
		err = b.m.Start(id)
	case "stop":
		err = b.m.Stop(id)
	case "restart":
		err = b.m.Restart(id)
	}
	if err != nil {
		return "", err
	}
	b.m.audit("mcp", "server."+action, id, "")
	// Read the state back off the server already in hand. Looking it up again
	// returned nil when another admin deleted it in this window, and the deref
	// panicked - leaving the agent with a broken response for an action that
	// did take effect, which it then retries.
	return fmt.Sprintf("%s requested; status is now %q.", action, s.State()), nil
}

func (b mcpBackend) ListBackups(id string) (string, error) {
	s, err := b.server(id)
	if err != nil {
		return "", err
	}
	list, err := b.m.ListBackups(s)
	if err != nil {
		return "", err
	}
	return b.js(list)
}

func (b mcpBackend) CreateBackup(id, note string) (string, error) {
	s, err := b.server(id)
	if err != nil {
		return "", err
	}
	bk, err := b.m.CreateBackup(s, note, "mcp")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Backup %s created (%s).", bk.ID, humanSize(bk.Size)), nil
}

func (b mcpBackend) HostStatus() (string, error) { return b.js(b.m.Host()) }

// ------------------------------------------------------------------ routes

func (a *API) mcpRoutes(mux *http.ServeMux) {
	h := &mcp.Handler{Backend: mcpBackend{m: a.mgr}, Check: a.mgr.mcp.Check}
	mux.Handle("/api/mcp", h)

	auth := a.mgr.auth
	mux.HandleFunc("GET /api/mcp-tokens", auth.require(RoleAdmin, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"tokens": a.mgr.mcp.List()})
	}))
	mux.HandleFunc("POST /api/mcp-tokens", auth.require(RoleAdmin, func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Name string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, 400, err)
			return
		}
		raw, err := a.mgr.mcp.Issue(body.Name)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		a.mgr.audit(actorOf(r), "mcp.token_issue", body.Name, "")
		// Shown once. Only the hash is stored.
		writeJSON(w, 201, map[string]any{"name": body.Name, "token": raw})
	}))
	mux.HandleFunc("DELETE /api/mcp-tokens/{name}", auth.require(RoleAdmin, func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if err := a.mgr.mcp.Revoke(name); err != nil {
			writeErr(w, 404, err)
			return
		}
		a.mgr.audit(actorOf(r), "mcp.token_revoke", name, "")
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))
}
