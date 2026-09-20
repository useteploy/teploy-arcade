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

// bearerToken parses the Authorization header strictly. R09 (audit pass 7):
// TrimPrefix accepted "bearer x" (lowercase scheme) and treated a missing
// scheme as a bare token guess; the MCP HTTP transport specifies a
// well-formed Bearer challenge, and a malformed one is a 401, not a lookup.
func bearerToken(h string) (string, bool) {
	parts := strings.Fields(h)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "MCP endpoint accepts POST only", http.StatusMethodNotAllowed)
		return
	}

	// R09: authentication failures are answered at the HTTP boundary with a
	// real 401 challenge, not folded into a JSON-RPC error body - a client
	// cannot distinguish "bad token" from "bad request" otherwise.
	tok, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok || h.Check == nil || !h.Check(tok) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="teploy-arcade"`)
		http.Error(w, "invalid or missing bearer token", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeRPC(w, nil, nil, &rpcError{Code: -32700, Message: "unreadable body"})
		return
	}
	// R09: a bounded reader alone does not prove the body fits - a valid JSON
	// prefix followed by more data used to decode fine and ignore the rest.
	// Exactly one JSON value per request.
	dec := json.NewDecoder(strings.NewReader(string(body)))
	var req rpcRequest
	if err := dec.Decode(&req); err != nil {
		writeRPC(w, nil, nil, &rpcError{Code: -32700, Message: "parse error"})
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeRPC(w, nil, nil, &rpcError{Code: -32600, Message: "exactly one JSON-RPC request per POST"})
		return
	}
	// R09: the version field is checked rather than assumed, and a request
	// carrying an ID must be string or number - MCP forbids null and
	// fractional IDs. An absent ID means notification, which never gets a
	// response body.
	isNotification := len(req.ID) == 0 || string(req.ID) == "null"
	if req.JSONRPC != "" && req.JSONRPC != "2.0" {
		writeRPC(w, req.ID, nil, &rpcError{Code: -32600, Message: `jsonrpc must be "2.0"`})
		return
	}
	if !isNotification && !validRPCID(req.ID) {
		writeRPC(w, req.ID, nil, &rpcError{Code: -32600, Message: "id must be a string or an integer"})
		return
	}
	if isNotification && req.Method != "notifications/initialized" {
		// Notifications never receive a response; nothing else is currently
		// accepted as one, so say so the way the protocol wants.
		w.WriteHeader(http.StatusAccepted)
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

// validRPCID reports whether a request ID has a shape JSON-RPC 2.0 - and,
// more strictly, MCP - permits: a string or an integer. Null, bools, floats
// and arrays/objects are refused (R09, audit pass 7).
func validRPCID(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return false
	}
	switch id := v.(type) {
	case string:
		return true
	case json.Number:
		_, err := id.Int64()
		return err == nil
	}
	return false
}
