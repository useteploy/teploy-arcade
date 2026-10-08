package arcade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/useteploy/teploy-arcade/internal/mcp"
)

func TestFinalPolicySessionCapRejectsWithoutEviction(t *testing.T) {
	a := NewAuth(t.TempDir())
	if _, err := a.CreateFirstUser("admin", "correct-password"); err != nil {
		t.Fatal(err)
	}
	var tokens []string
	for i := 0; i < 32; i++ {
		token := fmt.Sprintf("live-%d", i)
		tokens = append(tokens, token)
		a.sessions[token] = &Session{Token: token, User: "admin", Role: RoleAdmin, Expires: time.Now().Add(time.Hour)}
	}
	if _, err := a.Login("admin", "correct-password"); err == nil || !strings.Contains(err.Error(), "session limit") {
		t.Fatalf("full account admitted login: %v", err)
	}
	for _, token := range tokens {
		if a.Session(token) == nil {
			t.Fatalf("live session evicted: %s", token)
		}
	}
	a.sessions[tokens[0]].Expires = time.Now().Add(-time.Second)
	if _, err := a.Login("admin", "correct-password"); err != nil {
		t.Fatalf("expired slot not reclaimed: %v", err)
	}
}

func TestFinalPolicyLoginLimiterBoundedConcurrentExpiry(t *testing.T) {
	a := NewAuth(t.TempDir())
	var wg sync.WaitGroup
	admitted := make(chan bool, 120)
	for i := 0; i < 120; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); admitted <- a.AllowLoginOrigin("127.0.0.1:1234") }()
	}
	wg.Wait()
	close(admitted)
	count := 0
	for ok := range admitted {
		if ok {
			count++
		}
	}
	if count != 60 {
		t.Fatalf("concurrent peer budget %d", count)
	}
	if a.AllowLoginOrigin("attacker.example") {
		t.Fatal("non-IP peer accepted")
	}
	a.mu.Lock()
	a.loginBuckets["ip:127.0.0.1"] = loginBucket{start: time.Now().Add(-time.Minute), attempts: 60}
	a.mu.Unlock()
	if !a.AllowLoginOrigin("127.0.0.1") {
		t.Fatal("expired budget not reset")
	}
	for i := 0; i < 20; i++ {
		if !a.AllowLoginAccount("boss") {
			t.Fatal("account budget prematurely exhausted")
		}
	}
	if a.AllowLoginAccount("BOSS") {
		t.Fatal("account casing bypassed budget")
	}
	a.mu.Lock()
	for i := 0; i < 4096; i++ {
		a.loginBuckets[fmt.Sprint("key", i)] = loginBucket{start: time.Now()}
	}
	a.mu.Unlock()
	if a.AllowLoginOrigin("127.0.0.2") {
		t.Fatal("full table admitted unknown key")
	}
}

func TestFinalPolicyUnknownTemplateOwnershipSurvivesRepeatedBoot(t *testing.T) {
	dir := t.TempDir()
	if err := syncSeededTemplates(dir); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, "bedrock.json")
	body := []byte(`{"custom":"operator evidence"}`)
	if err := os.WriteFile(name, body, 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(filepath.Join(dir, seedLedger), []byte("{"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := syncSeededTemplates(dir); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(name)
		if err != nil || string(got) != string(body) {
			t.Fatal("active custom file changed")
		}
	}
	candidates, err := filepath.Glob(name + ".update-*")
	if err != nil || len(candidates) != 1 {
		t.Fatalf("repeat migration candidate mismatch: %v %v", candidates, err)
	}
}

func TestFinalPolicyCompleteTemplateContract(t *testing.T) {
	cases := map[string]func(*Template){
		"negative span": func(t *Template) { t.PortSpan = -1 }, "overflow": func(t *Template) { t.PortHint = 65535; t.PortSpan = 2 },
		"bad extras": func(t *Template) { t.ExtraPorts = []string{"22/sctp"} }, "container root": func(t *Template) { t.DataPath = "/" },
		"path traversal": func(t *Template) { t.DataPath = "/data/../etc" }, "capability": func(t *Template) { t.Console = "shell" },
		"unknown expansion": func(t *Template) { t.Args = []string{"${PASSWORD}"} }, "env key": func(t *Template) { t.Env = map[string]string{"A=B": "value"} },
		"slug traversal": func(t *Template) { t.Slug = "../admin" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			tpl := tplBuiltin[0]
			change(&tpl)
			if validateTemplate(&tpl) == nil {
				t.Fatal("invalid launch contract accepted")
			}
		})
	}
	for _, tpl := range tplBuiltin {
		if err := validateTemplate(&tpl); err != nil {
			t.Fatalf("builtin %s: %v", tpl.Slug, err)
		}
	}
}

func TestFinalPolicyPluginTLSAndDialedIP(t *testing.T) {
	t.Setenv("TEPLOY_ARCADE_ALLOW_HTTP_PLUGINS", "")
	for _, raw := range []string{"http://example.com/a.jar", "https://u:p@example.com/a.jar", "https://example.com/a.jar#fragment"} {
		u, _ := url.Parse(raw)
		if checkDownloadScheme(u) == nil {
			t.Fatalf("unsafe URL allowed %s", raw)
		}
	}
	t.Setenv("TEPLOY_ARCADE_ALLOW_HTTP_PLUGINS", "1")
	u, _ := url.Parse("http://mirror.example/a.jar")
	if err := checkDownloadScheme(u); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"127.0.0.1", "::1", "::ffff:127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1"} {
		ip := netip.MustParseAddr(text)
		if pluginAddressAllowed(ip, false) {
			t.Fatalf("private dial target accepted: %s", text)
		}
		if !pluginAddressAllowed(ip, true) {
			t.Fatalf("explicit mirror refused: %s", text)
		}
	}
	if !pluginAddressAllowed(netip.MustParseAddr("8.8.8.8"), false) {
		t.Fatal("public IP refused")
	}
}

func TestFinalPolicyMCPScopeAndMigration(t *testing.T) {
	_, mgr := newTestAgent(t)
	id := mgr.List()[0].ID
	other := mgr.List()[1].ID
	raw, err := mgr.mcp.IssueScoped("scoped", []string{id}, []string{"server.read"})
	if err != nil {
		t.Fatal(err)
	}
	b := mcpBackend{m: mgr, scope: mgr.mcp.Scope(raw)}
	if _, err := b.GetServer(id); err != nil {
		t.Fatal(err)
	}
	if _, err := b.GetServer(other); err == nil {
		t.Fatal("cross-server read allowed")
	}
	if _, err := b.Lifecycle(id, "stop"); err == nil {
		t.Fatal("read scope permitted stop")
	}
	for _, text := range []string{"op player", "execute run op player", "minecraft:op player", "alias player", "say hello"} {
		if _, err := b.SendCommand(id, text); err == nil {
			t.Fatal("raw console permitted")
		}
	}
	if err := mgr.mcp.Revoke("scoped"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.GetServer(id); err == nil {
		t.Fatal("revoked snapshot authorized")
	}
	mgr.mcp.mu.Lock()
	mgr.mcp.toks = append(mgr.mcp.toks, mcpToken{Name: "legacy", Hash: hashMCPToken("legacy")})
	mgr.mcp.mu.Unlock()
	if mgr.mcp.Check("legacy") || mgr.mcp.Scope("legacy") != nil {
		t.Fatal("legacy authority silently migrated")
	}
	if _, err := mgr.mcp.IssueScoped("empty", nil, nil); err == nil {
		t.Fatal("empty scope accepted")
	}
}

func TestFinalPolicyMCPTransport(t *testing.T) {
	h := &mcp.Handler{Check: func(string) bool { return true }, PublicOrigin: "https://panel.example"}
	for _, test := range []struct {
		name, body, origin, version string
		status                      int
	}{
		{"wrong origin", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "https://evil.example", "", 403},
		{"bad version", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "", "future", 400},
		{"oversize", strings.Repeat(" ", 1<<20) + `{}`, "", "", 413},
		{"valid", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "https://panel.example", "2025-06-18", 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "https://panel.example/api/mcp", strings.NewReader(test.body))
			r.Header.Set("Authorization", "Bearer test")
			r.Header.Set("Origin", test.origin)
			r.Header.Set("MCP-Protocol-Version", test.version)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("status %d body %s", w.Code, w.Body.String())
			}
		})
	}
	for _, body := range []string{`{"jsonrpc":"2.0","id":null,"method":"ping"}`, `{"id":1,"method":"ping"}`} {
		r := httptest.NewRequest(http.MethodPost, "https://panel.example/api/mcp", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer test")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var response map[string]json.RawMessage
		if json.Unmarshal(w.Body.Bytes(), &response) != nil || response["error"] == nil {
			t.Fatalf("invalid RPC accepted: %s", w.Body.String())
		}
	}
}

func TestFinalPolicyPluginQueueCancellationAndPrivateDial(t *testing.T) {
	t.Setenv("TEPLOY_ARCADE_ALLOW_PRIVATE_PLUGIN_HOSTS", "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if conn, err := pluginDialContext(ctx, "tcp", "127.0.0.1:9"); err == nil {
		conn.Close()
		t.Fatal("private destination dialed without policy opt-in")
	} else if !strings.Contains(err.Error(), "not public") {
		t.Fatalf("wrong dial policy error: %v", err)
	}
	pluginInstallWork <- struct{}{}
	pluginInstallWork <- struct{}{}
	defer func() { <-pluginInstallWork; <-pluginInstallWork }()
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if _, err := (&Manager{}).InstallPlugin(canceled, nil, "https://example.com/a.jar", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("queued install did not honor cancellation: %v", err)
	}
}
