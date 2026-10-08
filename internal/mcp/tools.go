package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Tools are prefixed `arcade_` rather than `teploy_`.
//
// teploy-dash already owns the `teploy_` namespace, and both servers can be
// attached to the same client at once. Worse than a literal name collision:
// "server" means a deploy target in dash and a game server here, so an
// unprefixed `teploy_list_servers` would be actively misleading to a model
// holding both toolsets.

func toolSpecs() []map[string]any {
	str := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}
	obj := func(props map[string]any, required ...string) map[string]any {
		if required == nil {
			required = []string{}
		}
		return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
	}

	return []map[string]any{
		{
			"name":        "arcade_list_servers",
			"description": "List every game server on this host with its status, player count, resource use and address.",
			"inputSchema": obj(map[string]any{}),
		},
		{
			"name":        "arcade_get_server",
			"description": "Full detail for one game server, including its settings and last exit reason.",
			"inputSchema": obj(map[string]any{"id": str("Server id")}, "id"),
		},
		{
			"name":        "arcade_console_tail",
			"description": "Recent console output for a game server, newest last. Use this to diagnose a crash or lag before acting.",
			"inputSchema": obj(map[string]any{
				"id":    str("Server id"),
				"lines": map[string]any{"type": "integer", "description": "How many lines (default 80, max 500)"},
			}, "id"),
		},
		{
			"name":        "arcade_lifecycle",
			"description": "Start, stop or restart a game server. 'kill' is deliberately not offered: it is SIGKILL and loses unsaved chunks, so it stays a human decision.",
			"inputSchema": obj(map[string]any{
				"id":     str("Server id"),
				"action": map[string]any{"type": "string", "enum": []string{"start", "stop", "restart"}},
			}, "id", "action"),
		},
		{
			"name":        "arcade_list_backups",
			"description": "List backups for a game server, newest first.",
			"inputSchema": obj(map[string]any{"id": str("Server id")}, "id"),
		},
		{
			"name":        "arcade_create_backup",
			"description": "Take a backup. Pauses world saves, flushes to disk, archives, then resumes. Blocks file writes for the duration.",
			"inputSchema": obj(map[string]any{
				"id":   str("Server id"),
				"note": str("Optional note recorded with the backup"),
			}, "id"),
		},
		{
			"name":        "arcade_host_status",
			"description": "Host-level view: CPU and memory allocated versus available, how many servers are running, whether Docker is reachable.",
			"inputSchema": obj(map[string]any{}),
		},
	}
}

func (h *Handler) call(name string, raw json.RawMessage) (string, error) {
	allowed := map[string][]string{
		"arcade_list_servers": {}, "arcade_get_server": {"id"}, "arcade_console_tail": {"id", "lines"}, "arcade_lifecycle": {"id", "action"}, "arcade_list_backups": {"id"}, "arcade_create_backup": {"id", "note"}, "arcade_host_status": {},
	}
	keys, known := allowed[name]
	if !known {
		return "", fmt.Errorf("unknown tool %q", name)
	}
	if len(raw) > 0 {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || fields == nil {
			return "", fmt.Errorf("arguments must be an object")
		}
		for field := range fields {
			valid := false
			for _, key := range keys {
				if field == key {
					valid = true
				}
			}
			if !valid {
				return "", fmt.Errorf("argument %q is not supported by %s", field, name)
			}
		}
	}
	var args struct {
		ID     string `json:"id"`
		Action string `json:"action"`
		Note   string `json:"note"`
		Lines  int    `json:"lines"`
	}
	if len(raw) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&args); err != nil {
			return "", fmt.Errorf("bad arguments: %w", err)
		}
		if err := dec.Decode(&struct{}{}); err != io.EOF {
			return "", fmt.Errorf("arguments need one JSON object")
		}
	}

	needID := func() error {
		if args.ID == "" {
			return fmt.Errorf("id is required")
		}
		return nil
	}

	switch name {
	case "arcade_list_servers":
		return h.Backend.ListServers()

	case "arcade_get_server":
		if err := needID(); err != nil {
			return "", err
		}
		return h.Backend.GetServer(args.ID)

	case "arcade_console_tail":
		if err := needID(); err != nil {
			return "", err
		}
		n := args.Lines
		if n <= 0 {
			n = 80
		}
		if n > 500 {
			n = 500
		}
		return h.Backend.ConsoleTail(args.ID, n)

	case "arcade_lifecycle":
		if err := needID(); err != nil {
			return "", err
		}
		switch args.Action {
		case "start", "stop", "restart":
		case "kill":
			return "", fmt.Errorf("kill is not available over MCP: it is SIGKILL and loses unsaved chunks, so it stays a human decision in the panel")
		default:
			return "", fmt.Errorf("action must be start, stop or restart")
		}
		h.logf("mcp: %s on %s", args.Action, args.ID)
		return h.Backend.Lifecycle(args.ID, args.Action)

	case "arcade_list_backups":
		if err := needID(); err != nil {
			return "", err
		}
		return h.Backend.ListBackups(args.ID)

	case "arcade_create_backup":
		if err := needID(); err != nil {
			return "", err
		}
		h.logf("mcp: backup on %s", args.ID)
		return h.Backend.CreateBackup(args.ID, args.Note)

	case "arcade_host_status":
		return h.Backend.HostStatus()
	}

	return "", fmt.Errorf("unknown tool %q", name)
}
