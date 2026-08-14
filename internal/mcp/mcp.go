package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// Hand-rolled JSON-RPC 2.0 over streamable HTTP (request/response only — GET
// returns 405, no server push). Stateless: no sessions, every POST carries a
// bearer token. Kept dependency-free on purpose; the protocol surface needed
// (initialize, ping, tools/list, tools/call) is small.
//
// Deliberately the same shape as teploy-dash/internal/mcp so the two behave
// identically for a client attaching to both.

const latestProtocol = "2025-06-18"

var supportedProtocols = map[string]bool{
	"2025-06-18": true,
	"2025-03-26": true,
	"2024-11-05": true,
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Backend is the panel surface the tools are allowed to touch. Narrow on
// purpose: an agent gets read access plus the lifecycle verbs, and nothing that
// destroys state. No delete, no restore, no user management.
type Backend interface {
	ListServers() (string, error)
	GetServer(id string) (string, error)
	ConsoleTail(id string, lines int) (string, error)
	SendCommand(id, text string) (string, error)
	Lifecycle(id, action string) (string, error)
	ListBackups(id string) (string, error)
	CreateBackup(id, note string) (string, error)
	HostStatus() (string, error)
}

// TokenChecker validates a bearer token. Supplied by the server so token
// storage stays in one place.
type TokenChecker func(token string) bool

type Handler struct {
	Backend Backend
	Check   TokenChecker
	Logf    func(string, ...any)
}

func (h *Handler) logf(f string, a ...any) {
	if h.Logf != nil {
		h.Logf(f, a...)
		return
	}
	log.Printf(f, a...)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "MCP endpoint accepts POST only", http.StatusMethodNotAllowed)
		return
	}

	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if h.Check == nil || !h.Check(strings.TrimSpace(tok)) {
		writeRPC(w, nil, nil, &rpcError{Code: -32001, Message: "invalid or missing bearer token"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeRPC(w, nil, nil, &rpcError{Code: -32700, Message: "unreadable body"})
		return
	}

	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPC(w, nil, nil, &rpcError{Code: -32700, Message: "parse error"})
		return
	}

	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		proto := latestProtocol
		if supportedProtocols[p.ProtocolVersion] {
			proto = p.ProtocolVersion
		}
		writeRPC(w, req.ID, map[string]any{
			"protocolVersion": proto,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "teploy-arcade", "version": "0.1.0"},
		}, nil)

	case "ping":
		writeRPC(w, req.ID, map[string]any{}, nil)

	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)

	case "tools/list":
		writeRPC(w, req.ID, map[string]any{"tools": toolSpecs()}, nil)

	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			writeRPC(w, req.ID, nil, &rpcError{Code: -32602, Message: "bad params"})
			return
		}
		text, err := h.call(p.Name, p.Arguments)
		if err != nil {
			// Tool failures are results with isError, not transport errors -
			// the model needs to read what went wrong.
			writeRPC(w, req.ID, map[string]any{
				"content": []any{map[string]any{"type": "text", "text": err.Error()}},
				"isError": true,
			}, nil)
			return
		}
		writeRPC(w, req.ID, map[string]any{
			"content": []any{map[string]any{"type": "text", "text": text}},
		}, nil)

	default:
		writeRPC(w, req.ID, nil, &rpcError{
			Code: -32601, Message: fmt.Sprintf("method %q not found", req.Method)})
	}
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, e *rpcError) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: id, Result: result, Error: e})
}
