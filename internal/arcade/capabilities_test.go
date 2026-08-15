package arcade

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// /api/capabilities declared scheduled_backups false while the scheduler had
// dispatched `!backup` to CreateBackup since the day it was written, and the
// Scheduler tab documented the action in the panel itself. Nothing failed:
// a wrong flag is just a wrong flag - until the dashboard started reading
// capabilities to build its "Not built yet" list, at which point the panel
// advertised one of its own working features as missing, on its front page.
//
// So the flag is asserted against the behaviour rather than against a copy of
// itself: run the action, then read the flag.
func TestScheduledBackupsCapabilityMatchesTheScheduler(t *testing.T) {
	hub := NewHub()
	mgr := NewManager(t.TempDir(), hub)
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	s := mgr.List()[0]

	if err := mgr.sched.step(s, "!backup nightly", "test"); err != nil {
		t.Fatalf("the scheduler cannot take a backup: %v", err)
	}
	backups, err := mgr.ListBackups(s)
	if err != nil {
		t.Fatalf("list backups: %v", err)
	}
	made := len(backups) > 0
	if made && backups[0].Note != "nightly" {
		t.Errorf("note not carried through: %q", backups[0].Note)
	}

	api := &API{mgr: mgr, hub: hub}
	mux := http.NewServeMux()
	api.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/capabilities")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var caps struct {
		Features map[string]bool `json:"features"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&caps); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if caps.Features["scheduled_backups"] != made {
		t.Fatalf("capabilities says scheduled_backups=%v; the scheduler taking a backup says %v",
			caps.Features["scheduled_backups"], made)
	}
}
