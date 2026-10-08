package arcade

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
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
	Name      string   `json:"name"`
	Hash      string   `json:"hash"` // sha256 of the token; the plaintext is shown once
	Created   int64    `json:"created"`
	LastUse   int64    `json:"last_use"`
	ServerIDs []string `json:"server_ids"`
	Actions   []string `json:"actions"`
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
	legacy := 0
	for _, tok := range t.toks {
		if !strings.HasPrefix(tok.Hash, mcpHashPrefix) {
			legacy++
		}
	}
	if legacy > 0 {
		// Stored under the password KDF (PBKDF2, 120k rounds) these tokens
		// made every unauthenticated guess expensive on purpose - the right
		// property for a human password, and a free CPU-exhaustion lever for
		// a 192-bit random bearer secret nobody guesses. They no longer
		// authenticate; reissue from the panel's token screen.
		log.Printf("mcp: %d stored token(s) predate the token-hash upgrade and will not authenticate; revoke and reissue them", legacy)
	}
	return t
}

// mcpHashPrefix marks hashes stored under the token scheme. A bearer token is
// 192 bits of entropy, not a human password - stretching it with PBKDF2 cost
// the panel one 120,000-round computation per unauthenticated guess, while
// adding nothing a fixed-size digest compared in constant time does not
// already give.
const mcpHashPrefix = "sha256:"

func hashMCPToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return mcpHashPrefix + hex.EncodeToString(sum[:])
}

func (t *mcpTokens) save() error {
	b, err := json.MarshalIndent(t.toks, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(t.path, b, 0o600)
}

// Revoke removes every token carrying this name. It used to remove only the
// first match, so a legacy store holding duplicates kept a working credential
// behind the name the operator believed they had revoked.
func (t *mcpTokens) Revoke(name string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	next := make([]mcpToken, 0, len(t.toks))
	for _, token := range t.toks {
		if strings.TrimSpace(token.Name) != strings.TrimSpace(name) {
			next = append(next, token)
		}
	}
	if len(next) == len(t.toks) {
		return fmt.Errorf("no such token")
	}
	return t.persistLocked(next)
}

func (t *mcpTokens) Issue(name string) (string, error) {
	return t.IssueScoped(name, []string{"*"}, []string{"server.read", "host.read"})
}

func (t *mcpTokens) IssueScoped(name string, serverIDs, actions []string) (string, error) {
	if err := validateMCPScope(serverIDs, actions); err != nil {
		return "", err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	if len(name) > 64 {
		return "", fmt.Errorf("name must be at most 64 characters")
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
	// Duplicate names are refused: names are how tokens are listed and
	// revoked, and a second token under a live name used to hide behind it -
	// revoking the name removed whichever record came first, and the other
	// credential kept working.
	for _, x := range t.toks {
		if strings.TrimSpace(x.Name) == name {
			return "", fmt.Errorf("a token named %q already exists; revoke it or choose another name", name)
		}
	}
	// Persist before publish, matching every other credential store here: a
	// failed save used to leave a working token in memory that no restart
	// would ever know about.
	next := make([]mcpToken, 0, len(t.toks)+1)
	next = append(next, t.toks...)
	next = append(next, mcpToken{
		Name: name, Hash: hashMCPToken(raw), Created: time.Now().Unix(), ServerIDs: append([]string{}, serverIDs...), Actions: append([]string{}, actions...),
	})
	if err := t.persistLocked(next); err != nil {
		return "", err
	}
	return raw, nil
}

// persistLocked writes a prospective token list and publishes it only when
// the write commits. Callers hold t.mu.
func (t *mcpTokens) persistLocked(next []mcpToken) error {
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(t.path, b, 0o600); err != nil {
		return err
	}
	t.toks = next
	return nil
}

func (t *mcpTokens) Check(raw string) bool {
	if raw == "" {
		return false
	}
	h := hashMCPToken(raw)
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now().Unix()
	for i := range t.toks {
		// Legacy pre-upgrade hashes carry no prefix and never match; anything
		// else is compared in constant time, as before.
		if !strings.HasPrefix(t.toks[i].Hash, mcpHashPrefix) {
			continue
		}
		// Constant-time, like the password path: a plain == returns at the
		// first differing byte, which tells a caller timing how much of a
		// guessed token's hash was right.
		if subtle.ConstantTimeCompare([]byte(t.toks[i].Hash), []byte(h)) == 1 && len(t.toks[i].Actions) > 0 {
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
			"name": x.Name, "created": x.Created, "last_use": x.LastUse, "server_ids": x.ServerIDs, "actions": x.Actions, "requires_reissue": len(x.Actions) == 0,
		})
	}
	return out
}

// ---------------------------------------------------------------- backend

type mcpBackend struct {
	m     *Manager
	scope *mcpToken
}

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

func (b mcpBackend) ListServers() (string, error) {
	if err := b.permit("", "server.read"); err != nil {
		return "", err
	}
	snapshots := b.m.listSnapshot()
	filtered := snapshots[:0]
	for _, snapshot := range snapshots {
		id, _ := snapshot["id"].(string)
		if b.permit(id, "server.read") == nil {
			filtered = append(filtered, snapshot)
		}
	}
	return b.js(filtered)
}

func (b mcpBackend) GetServer(id string) (string, error) {
	if err := b.permit(id, "server.read"); err != nil {
		return "", err
	}
	s, err := b.server(id)
	if err != nil {
		return "", err
	}
	// Snapshot already carries a locked copy of the settings map; reading
	// s.Props directly here raced reloadProps and crashed the process.
	return b.js(s.Snapshot())
}

func (b mcpBackend) ConsoleTail(id string, lines int) (string, error) {
	if err := b.permit(id, "server.read"); err != nil {
		return "", err
	}
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
//
// This remains a denylist, and a denylist is not a capability boundary: plugin
// aliases and wrappers can reach the same operations under other names. It
// stops the straightforward spellings; the durable fix is typed tools with
// scoped capabilities (see AUDIT_OPEN).
var mcpBlockedVerbs = map[string]bool{
	"op": true, "deop": true, "ban": true, "ban-ip": true,
	"pardon": true, "pardon-ip": true, "whitelist": true, "stop": true,
}

// consoleVerb pulls the command word out of a console line. The leading slash
// is tolerated because a model will type one however the tool is described,
// and a namespace prefix (`minecraft:op`, a plugin's `essentials:ban`) is
// stripped because Paper-ecosystem servers dispatch those as the plain verb -
// the check has to see the command the game will run, not the first token of
// the line.
func consoleVerb(text string) string {
	v := strings.TrimPrefix(strings.TrimSpace(text), "/")
	if i := strings.IndexAny(v, " \t"); i >= 0 {
		v = v[:i]
	}
	if i := strings.LastIndex(v, ":"); i >= 0 {
		v = v[i+1:]
	}
	return strings.ToLower(v)
}

func (b mcpBackend) SendCommand(id, text string) (string, error) {
	if b.scope != nil {
		return "", fmt.Errorf("raw console commands are not available to MCP tokens")
	}
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
	if err := b.permit(id, "server."+action); err != nil {
		return "", err
	}
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
	default:
		return "", fmt.Errorf("action must be start, stop or restart")
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
	if err := b.permit(id, "server.read"); err != nil {
		return "", err
	}
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
	if err := b.permit(id, "server.backup"); err != nil {
		return "", err
	}
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

func (b mcpBackend) HostStatus() (string, error) {
	if err := b.permit("", "host.read"); err != nil {
		return "", err
	}
	return b.js(b.m.Host())
}

// ------------------------------------------------------------------ routes

func (a *API) mcpRoutes(mux *http.ServeMux) {
	// An unclaimed panel refuses MCP dispatch outright: no token can be
	// legitimately issued before the first admin exists (the minting route is
	// gated), and honouring a pre-setup token here would let a credential
	// minted during the open window outlive the setup it preceded.
	mux.Handle("/api/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Snapshot scope per request; every backend action rechecks revocation.
		parts := strings.Fields(r.Header.Get("Authorization"))
		var scope *mcpToken
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") && !a.mgr.auth.SetupRequired() {
			scope = a.mgr.mcp.Scope(parts[1])
		}
		h := &mcp.Handler{Backend: mcpBackend{m: a.mgr, scope: scope}, Check: func(raw string) bool { return scope != nil && a.mgr.mcp.Check(raw) }, PublicOrigin: a.mgr.publicOrigin}
		h.ServeHTTP(w, r)
	}))

	auth := a.mgr.auth
	mux.HandleFunc("GET /api/mcp-tokens", auth.require(RoleAdmin, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"tokens": a.mgr.mcp.List()})
	}))
	mux.HandleFunc("POST /api/mcp-tokens", auth.require(RoleAdmin, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name      string
			ServerIDs []string `json:"server_ids"`
			Actions   []string `json:"actions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, 400, err)
			return
		}
		// Existing clients that only send a name get read-only access to the
		// current server set. Newly created servers need explicit reissuance.
		if body.Actions == nil {
			body.Actions = []string{"server.read"}
		}
		if body.ServerIDs == nil {
			for _, server := range a.mgr.List() {
				body.ServerIDs = append(body.ServerIDs, server.ID)
			}
		}
		for _, id := range body.ServerIDs {
			if id == "*" || a.mgr.Get(id) == nil {
				writeErr(w, 400, fmt.Errorf("scope needs existing explicit server IDs"))
				return
			}
		}
		raw, err := a.mgr.mcp.IssueScoped(body.Name, body.ServerIDs, body.Actions)
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

var mcpActions = map[string]bool{"server.read": true, "server.start": true, "server.stop": true, "server.restart": true, "server.backup": true, "host.read": true}

func validateMCPScope(ids, actions []string) error {
	if len(ids) > 1024 || len(actions) == 0 || len(actions) > len(mcpActions) {
		return fmt.Errorf("MCP scope needs explicit supported actions and at most 1024 server IDs")
	}
	seen := map[string]bool{}
	for _, action := range actions {
		if !mcpActions[action] || seen[action] {
			return fmt.Errorf("unsupported or duplicate MCP action %q", action)
		}
		seen[action] = true
	}
	needsServers := false
	for _, action := range actions {
		if strings.HasPrefix(action, "server.") {
			needsServers = true
		}
	}
	if needsServers && len(ids) == 0 {
		return fmt.Errorf("server actions need at least one server ID")
	}
	seen = map[string]bool{}
	for _, id := range ids {
		if id == "" || len(id) > 128 || seen[id] {
			return fmt.Errorf("invalid or duplicate server ID")
		}
		seen[id] = true
	}
	return nil
}
func (t *mcpTokens) Scope(raw string) *mcpToken {
	h := hashMCPToken(raw)
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, x := range t.toks {
		if subtle.ConstantTimeCompare([]byte(x.Hash), []byte(h)) == 1 && len(x.Actions) > 0 {
			copy := x
			copy.Actions = append([]string{}, x.Actions...)
			copy.ServerIDs = append([]string{}, x.ServerIDs...)
			return &copy
		}
	}
	return nil
}
func (b mcpBackend) permit(id, action string) error {
	if b.scope == nil {
		return nil
	} // only direct internal callers; HTTP rejects absent scope
	b.m.mcp.mu.RLock()
	live := false
	for _, x := range b.m.mcp.toks {
		if x.Hash == b.scope.Hash {
			live = true
			break
		}
	}
	b.m.mcp.mu.RUnlock()
	if !live {
		return fmt.Errorf("MCP token revoked")
	}
	allowed := false
	for _, a := range b.scope.Actions {
		if a == action {
			allowed = true
		}
	}
	if !allowed {
		return fmt.Errorf("MCP action %q is outside token scope", action)
	}
	if id == "" {
		return nil
	}
	for _, server := range b.scope.ServerIDs {
		if server == id || server == "*" {
			return nil
		}
	}
	return fmt.Errorf("server is outside MCP token scope")
}
