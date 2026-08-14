package arcade

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func rpc(t *testing.T, srv *httptest.Server, token, method string, params any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	req, _ := http.NewRequest("POST", srv.URL+"/api/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rpc: %v", err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func toolText(t *testing.T, resp map[string]any) (string, bool) {
	t.Helper()
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", resp)
	}
	isErr, _ := result["isError"].(bool)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		return "", isErr
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return text, isErr
}

// An unauthenticated caller must get nothing. This is the whole point of the
// separate token: the MCP endpoint is reachable without a panel session.
func TestMCPRequiresBearerToken(t *testing.T) {
	srv, _ := newTestAgent(t)
	defer srv.Close()

	resp := rpc(t, srv, "", "tools/list", nil)
	if resp["error"] == nil {
		t.Fatal("tools/list succeeded with no token")
	}
	resp = rpc(t, srv, "tpa_not_a_real_token", "tools/list", nil)
	if resp["error"] == nil {
		t.Fatal("tools/list succeeded with a bogus token")
	}
}

func TestMCPHandshakeAndTools(t *testing.T) {
	srv, mgr := newTestAgent(t)
	defer srv.Close()

	tok, err := mgr.mcp.Issue("test-agent")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if !strings.HasPrefix(tok, "tpa_") {
		t.Errorf("token %q should carry the tpa_ prefix", tok)
	}

	init := rpc(t, srv, tok, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	res, ok := init["result"].(map[string]any)
	if !ok {
		t.Fatalf("initialize failed: %v", init)
	}
	if res["protocolVersion"] != "2025-06-18" {
		t.Errorf("protocolVersion = %v", res["protocolVersion"])
	}
	info := res["serverInfo"].(map[string]any)
	if info["name"] != "teploy-arcade" {
		t.Errorf("serverInfo.name = %v", info["name"])
	}

	list := rpc(t, srv, tok, "tools/list", nil)
	tools := list["result"].(map[string]any)["tools"].([]any)
	if len(tools) < 8 {
		t.Fatalf("only %d tools registered", len(tools))
	}
	// Every tool must be arcade_-prefixed: teploy-dash owns `teploy_`, and both
	// servers can be attached to one client at the same time.
	for _, raw := range tools {
		name := raw.(map[string]any)["name"].(string)
		if !strings.HasPrefix(name, "arcade_") {
			t.Errorf("tool %q is not namespaced under arcade_", name)
		}
	}
}

func TestMCPToolsDriveThePanel(t *testing.T) {
	srv, mgr := newTestAgent(t)
	defer srv.Close()
	tok, _ := mgr.mcp.Issue("test-agent")

	call := func(name string, args map[string]any) (string, bool) {
		return toolText(t, rpc(t, srv, tok, "tools/call",
			map[string]any{"name": name, "arguments": args}))
	}

	out, isErr := call("arcade_list_servers", map[string]any{})
	if isErr || !strings.Contains(out, "My Purpur Server") {
		t.Fatalf("list_servers = %q (err=%v)", out, isErr)
	}

	id := mgr.List()[0].ID

	// A bad id must explain itself rather than returning empty.
	out, isErr = call("arcade_get_server", map[string]any{"id": "nope"})
	if !isErr || !strings.Contains(out, "arcade_list_servers") {
		t.Errorf("unknown id should point at the discovery tool, got %q", out)
	}

	if out, isErr = call("arcade_lifecycle", map[string]any{"id": id, "action": "start"}); isErr {
		t.Fatalf("start failed: %s", out)
	}
	waitFor(t, 12*time.Second, func() bool { return mgr.Get(id).State() == StatusRunning })

	out, isErr = call("arcade_console_tail", map[string]any{"id": id, "lines": 20})
	if isErr || !strings.Contains(out, "INFO") {
		t.Errorf("console_tail = %q", out)
	}

	// kill must not be reachable from an agent.
	out, isErr = call("arcade_lifecycle", map[string]any{"id": id, "action": "kill"})
	if !isErr || !strings.Contains(out, "SIGKILL") {
		t.Errorf("kill should be refused with a reason, got %q", out)
	}

	if out, isErr = call("arcade_host_status", map[string]any{}); isErr || !strings.Contains(out, "allocated_vcpu") {
		t.Errorf("host_status = %q", out)
	}

	// Everything an agent does lands in the audit log under its own actor.
	found := false
	for _, e := range mgr.auth.Audit(50) {
		if e.Actor == "mcp" && e.Action == "server.start" {
			found = true
		}
	}
	if !found {
		t.Error("MCP actions are not attributed to 'mcp' in the audit log")
	}
}

func TestMCPTokenRevoke(t *testing.T) {
	srv, mgr := newTestAgent(t)
	defer srv.Close()

	tok, _ := mgr.mcp.Issue("temp")
	if resp := rpc(t, srv, tok, "ping", nil); resp["error"] != nil {
		t.Fatalf("ping with a fresh token failed: %v", resp)
	}
	if err := mgr.mcp.Revoke("temp"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if resp := rpc(t, srv, tok, "ping", nil); resp["error"] == nil {
		t.Fatal("a revoked token still works")
	}
}
