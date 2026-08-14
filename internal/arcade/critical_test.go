package arcade

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// C1. trySend must be atomic with Close. A flag checked before the send lets
// Close run in the gap, and sending on a closed channel is an unrecoverable
// panic that kills the panel. Run with -race for the full picture.
func TestFanoutNeverSendsOnAClosedChannel(t *testing.T) {
	for round := 0; round < 40; round++ {
		hub := NewHub()
		conns := make([]*Conn, 12)
		for i := range conns {
			conns[i] = NewConn(2) // tiny buffer: force the shedding path
			hub.Join("room", conns[i])
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { // producer, as a runner would be
			defer wg.Done()
			for i := 0; i < 400; i++ {
				hub.Publish("room", Line{Text: "output"})
			}
		}()
		go func() { // viewers disconnecting mid-flight
			defer wg.Done()
			for _, c := range conns {
				hub.Leave("room", c)
			}
			hub.DropRoom("room")
		}()
		wg.Wait() // a panic here fails the test by crashing it
	}
}

// C2. With auth configured, every route that exposes state must demand a
// session. Enumerated rather than spot-checked: the bug was six handlers
// registered without the wrapper, which no single-route test would catch.
func TestEveryStatefulRouteRequiresASession(t *testing.T) {
	srv, mgr := newTestAgent(t)
	defer srv.Close()

	if _, err := mgr.auth.CreateUser("admin", "correct-horse-battery", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	id := mgr.List()[0].ID

	gated := []struct{ method, path string }{
		{"GET", "/api/host"}, {"GET", "/api/templates"}, {"GET", "/api/servers"},
		{"GET", "/api/servers/" + id}, {"GET", "/api/servers/" + id + "/settings"},
		{"GET", "/api/servers/" + id + "/metrics"}, {"GET", "/api/metrics"},
		{"GET", "/api/capabilities"}, {"GET", "/api/audit"},
		{"GET", "/api/servers/" + id + "/files"}, {"GET", "/api/servers/" + id + "/backups"},
		{"GET", "/api/servers/" + id + "/players"}, {"GET", "/api/servers/" + id + "/tasks"},
		{"GET", "/api/users"},
		{"POST", "/api/servers"}, {"POST", "/api/servers/" + id + "/start"},
		{"PATCH", "/api/servers/" + id + "/settings"},
		{"DELETE", "/api/servers/" + id},
	}
	for _, r := range gated {
		req, _ := http.NewRequest(r.method, srv.URL+r.path, strings.NewReader("{}"))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", r.method, r.path, err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s returned %d to an anonymous caller, want 401",
				r.method, r.path, res.StatusCode)
		}
	}

	// The handful that must stay open, or nobody can log in or health-check.
	for _, p := range []string{"/api/health", "/api/me"} {
		res, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s returned %d; it must stay reachable", p, res.StatusCode)
		}
	}
}

// C3/C6. State files must be written atomically. A truncated users.json fails
// to parse, gets quarantined, empties the user set - and an empty user set
// disables auth entirely.
func TestStateFilesAreWrittenAtomically(t *testing.T) {
	_, mgr := newTestAgent(t)

	if _, err := mgr.auth.CreateUser("admin", "correct-horse-battery", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.mcp.Issue("agent"); err != nil {
		t.Fatal(err)
	}
	mgr.audit("admin", "test", "x", "")

	for _, f := range []string{"users.json", "mcp-tokens.json", "audit.json"} {
		p := filepath.Join(mgr.dataDir, f)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("%s missing: %v", f, err)
		}
		var v any
		if err := json.Unmarshal(b, &v); err != nil {
			t.Errorf("%s is not valid JSON: %v", f, err)
		}
		// Glob, not a fixed name. This assertion was a tautology for a while:
		// it checked for exactly "<file>.tmp" after writeFileAtomic moved to
		// os.CreateTemp, which appends a random suffix, so it could no longer
		// fail whether or not the rename happened.
		leftovers, _ := filepath.Glob(p + ".tmp*")
		if len(leftovers) > 0 {
			t.Errorf("%s left temp files behind: %v", f, leftovers)
		}
	}
}

// C4. Concurrent appends must not lose entries. Snapshot-under-lock then
// write-outside-lock lets a later write land an older snapshot.
func TestAuditAppendsAreNotLost(t *testing.T) {
	_, mgr := newTestAgent(t)

	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mgr.audit("tester", "action", "target", string(rune('a'+i%26)))
		}(i)
	}
	wg.Wait()

	if got := len(mgr.auth.Audit(0)); got != n {
		t.Errorf("in-memory audit has %d entries, want %d", got, n)
	}

	b, err := os.ReadFile(mgr.auth.auditPath())
	if err != nil {
		t.Fatal(err)
	}
	var onDisk []AuditEntry
	if err := json.Unmarshal(b, &onDisk); err != nil {
		t.Fatalf("audit.json unreadable after concurrent appends: %v", err)
	}
	if len(onDisk) != n {
		t.Errorf("audit.json has %d entries, want %d - appends were lost", len(onDisk), n)
	}
}

// C5. Snapshot and reloadProps touch the same map from different goroutines.
// Go panics fatally on concurrent map read/write; it cannot be recovered.
func TestSettingsSnapshotDoesNotRaceReload(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	content, err := mgr.ReadFile(s, "server.properties")
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(2)
	go func() { // readers: what MCP and the API do
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			snap := s.Snapshot()
			if set, ok := snap["settings"].(map[string]string); ok {
				_ = set["difficulty"]
			}
			_ = mgr.writeProps(s)
		}
	}()
	go func() { // writer: someone editing server.properties in the file manager
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			v := "hard"
			if i%2 == 0 {
				v = "normal"
			}
			mgr.reloadProps(s, strings.Replace(content, "difficulty=normal", "difficulty="+v, 1))
		}
	}()
	time.Sleep(400 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// C6. The token store must not be rewritten on every authenticated call.
func TestMCPTokenUseIsDebounced(t *testing.T) {
	_, mgr := newTestAgent(t)
	tok, err := mgr.mcp.Issue("agent")
	if err != nil {
		t.Fatal(err)
	}

	info := func() time.Time {
		st, err := os.Stat(mgr.mcp.path)
		if err != nil {
			t.Fatal(err)
		}
		return st.ModTime()
	}

	if !mgr.mcp.Check(tok) {
		t.Fatal("fresh token rejected")
	}
	before := info()
	time.Sleep(20 * time.Millisecond)

	for i := 0; i < 50; i++ {
		if !mgr.mcp.Check(tok) {
			t.Fatal("token rejected mid-run")
		}
	}
	if after := info(); !after.Equal(before) {
		t.Error("the token file was rewritten by repeated auth checks; that is the corruption window")
	}
}

// C5, second site. The original C5 fix covered Snapshot and writeProps but not
// ApplySettings, which read and wrote s.Props with no lock at all. A concurrent
// map read/write is a fatal runtime abort - not a recoverable panic - so this
// has to be driven directly. Run under -race.
func TestApplySettingsDoesNotRaceReloadProps(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	content, err := mgr.ReadFile(s, "server.properties")
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(3)
	go func() { // the settings screen saving
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			v := "hard"
			if i%2 == 0 {
				v = "easy"
			}
			if _, err := mgr.ApplySettings(s, map[string]string{"difficulty": v}); err != nil {
				t.Errorf("ApplySettings: %v", err)
				return
			}
		}
	}()
	go func() { // someone editing server.properties in the file manager
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			mgr.reloadProps(s, content)
		}
	}()
	go func() { // and MCP reading it
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = s.Snapshot()
		}
	}()

	time.Sleep(500 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// Two concurrent saves must both succeed. A shared "<file>.tmp" name meant one
// writer removed the other's temp and the loser failed with ENOENT.
func TestConcurrentPropertyWritesBothSucceed(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	const n = 24
	errs := make(chan error, n*2)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); errs <- mgr.writeProps(s) }()
		go func(i int) {
			defer wg.Done()
			errs <- mgr.WriteFile(s, "plugins/config.yml", "n: "+string(rune('a'+i%26)))
		}(i)
	}
	wg.Wait()
	close(errs)

	var failed int
	var first error
	for err := range errs {
		if err != nil {
			failed++
			if first == nil {
				first = err
			}
		}
	}
	if failed > 0 {
		t.Errorf("%d of %d concurrent writes failed, first: %v", failed, n*2, first)
	}
}
