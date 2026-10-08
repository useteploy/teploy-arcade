package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFinalPolicyNoRawToolOrFallback(t *testing.T) {
	h := &Handler{}
	for _, spec := range toolSpecs() {
		if spec["name"] == "arcade_send_command" {
			t.Fatal("raw tool still advertised")
		}
	}
	for _, raw := range []string{`{"id":"server","text":"execute run op player"}`, `{"id":"server","text":"minecraft:op player"}`, `{"id":"server","text":"alias player"}`} {
		if _, err := h.call("arcade_send_command", json.RawMessage(raw)); err == nil {
			t.Fatal("raw fallback accepted")
		}
	}
	if _, err := h.call("arcade_lifecycle", json.RawMessage(`{"id":"x","action":"stop","text":"op player"}`)); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("injected raw parameter accepted: %v", err)
	}
	for _, raw := range []string{`null`, `[]`, `{"id":"x","action":"kill"}`} {
		if _, err := h.call("arcade_lifecycle", json.RawMessage(raw)); err == nil {
			t.Fatalf("invalid typed action accepted: %s", raw)
		}
	}
}
