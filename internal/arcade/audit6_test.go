package arcade

// Regression tests for the sixth audit pass (AUDIT-CHATGPT-6.md). Each test
// names the finding it pins: A01, A02, A03, A04, A09, A16, A19, A26, A27,
// A28, A31, A36, A42.

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A01. A freshly installed panel is unclaimed but no longer open: with the
// setup gate armed, every protected route refuses before its handler runs,
// the MCP endpoint refuses tokens minted during the open window, and setup
// itself still works through the bootstrap token.
func TestUnclaimedPanelRefusesOperationalRoutes(t *testing.T) {
	srv, mgr := newAuthedTestAgent(t)
	defer srv.Close()
	if err := mgr.auth.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := mgr.auth.BeginSetup(); err != nil {
		t.Fatalf("begin setup: %v", err)
	}
	if !mgr.auth.SetupRequired() {
		t.Fatal("an unclaimed panel with setup armed does not report SetupRequired")
	}

	for _, c := range []struct{ method, path string }{
		{"GET", "/api/servers"},
		{"POST", "/api/servers"},
		{"DELETE", "/api/servers/whatever"},
		{"GET", "/api/servers/whatever/files"},
		{"POST", "/api/mcp-tokens"},
		{"GET", "/api/audit"},
	} {
		req, _ := http.NewRequest(c.method, srv.URL+c.path, strings.NewReader("{}"))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", c.method, c.path, err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s %s on an unclaimed panel: got %d, want 503 (the handler must not run)", c.method, c.path, res.StatusCode)
		}
	}

	// MCP dispatch refuses even a well-formed bearer request while unclaimed.
	// JSON-RPC errors ride a 200, so the payload is what must say no.
	req, _ := http.NewRequest("POST", srv.URL+"/api/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	req.Header.Set("Authorization", "Bearer tpa_nonsense")
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var rpc struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&rpc); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if rpc.Error == nil || !strings.Contains(rpc.Error.Message, "token") {
		t.Errorf("MCP dispatch on an unclaimed panel answered %v; it must refuse the token", rpc.Error)
	}

	// The one route that must work: claiming through the bootstrap token.
	token := mgr.auth.bootstrapToken
	code, body := postJSON(t, srv.URL+"/api/setup",
		`{"name":"admin","password":"correct-horse-battery","token":"`+token+`"}`)
	if code != 201 {
		t.Fatalf("setup on an unclaimed panel: got %d %s, want 201", code, body)
	}
	if mgr.auth.SetupRequired() {
		t.Error("setup gate still armed after the first admin exists")
	}
}

// A02. A crash partway through evacuation leaves some originals still live.
// Recovery used to read "old/ is non-empty" as "everything live is a
// replacement", delete every live entry and restore old/ - destroying the
// originals evacuation never reached.
func TestBootRecoveryPreservesUnmovedOriginals(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]
	dir := mgr.serverDir(s)

	// Fixture: crash DURING evacuation. a.txt made it into old/, b.txt never
	// moved, and staging/new holds the extracted archive.
	staging := filepath.Join(dir, ".arcade-restore-crashed")
	held := filepath.Join(staging, "old")
	extracted := filepath.Join(staging, "new")
	for _, d := range []string{held, extracted} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(held, "a.txt"), []byte("original-a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("original-b"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr.recoverInterruptedRestores()

	got, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil || string(got) != "original-a" {
		t.Errorf("a.txt = %q (%v); the held original must come back", got, err)
	}
	got, err = os.ReadFile(filepath.Join(dir, "b.txt"))
	if err != nil || string(got) != "original-b" {
		t.Fatalf("b.txt = %q (%v); an original the evacuation never reached was destroyed", got, err)
	}
}

// A03. A rollback that cannot finish must say so, and the held originals must
// survive it - the caller keeps the staging tree instead of deleting the only
// remaining copy.
func TestRestoreHeldReportsFailureAndRetainsOriginals(t *testing.T) {
	dir := t.TempDir()
	held := filepath.Join(dir, "old")
	if err := os.MkdirAll(filepath.Join(held, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A live non-empty directory with the held entry's name makes the rename
	// back fail: directories cannot be renamed over.
	if err := os.MkdirAll(filepath.Join(dir, "sub", "blocking"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(held, "sub", "world.dat"), []byte("irreplaceable"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := restoreHeld(held, dir)
	if err == nil {
		t.Fatal("restoreHeld reported success over a blocking collision")
	}
	if !strings.Contains(err.Error(), "sub") {
		t.Errorf("the failure does not name what it could not restore: %v", err)
	}
	// The held original must still exist for an operator to recover by hand.
	if b, rerr := os.ReadFile(filepath.Join(held, "sub", "world.dat")); rerr != nil || string(b) != "irreplaceable" {
		t.Errorf("the held original did not survive the failed rollback: %q %v", b, rerr)
	}
}

// A04. tar completion is not gzip completion: a corrupt trailer used to pass
// validation the moment the tar records stopped, because nothing ever read
// the checksum.
func TestUntarRefusesACorruptGzipTrailer(t *testing.T) {
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	if err := tw.WriteHeader(&tar.Header{Name: "world.txt", Mode: 0o600, Size: 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var packed bytes.Buffer
	zw := gzip.NewWriter(&packed)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	b := packed.Bytes()
	b[len(b)-8] ^= 0xff // corrupt the CRC half of the trailer

	archive := filepath.Join(t.TempDir(), "corrupt.tar.gz")
	if err := os.WriteFile(archive, b, 0o644); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := untarGz(archive, dst); err == nil {
		t.Fatal("a corrupt gzip stream was accepted for restore")
	}
}

// A09. `[null]` decodes successfully into a slice holding a nil pointer; the
// loaders must refuse the record instead of panicking at boot.
func TestNullRecordsQuarantineInsteadOfPanicking(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "users.json"), []byte(`[null]`), 0o644); err != nil {
		t.Fatal(err)
	}
	a := NewAuth(dir)
	if err := a.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(a.Users()) != 0 {
		t.Errorf("a null user record became %d account(s)", len(a.Users()))
	}

	if err := os.WriteFile(filepath.Join(dir, "servers.json"), []byte(`[null]`), 0o644); err != nil {
		t.Fatal(err)
	}
	hub := NewHub()
	mgr := NewManager(dir, hub)
	if err := mgr.Load(); err != nil {
		t.Fatalf("manager load panicked or failed past [null]: %v", err)
	}
}

// A16. Only the ROOT server.properties is the panel's port identity. A nested
// file with the same basename must be an ordinary file edit.
func TestNestedPropertiesFileIsNotTheServersIdentity(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	before := s.Port
	if err := mgr.WriteFile(s, "plugins/example/server.properties", "# local override\nserver-port=9999\n"); err != nil {
		t.Fatalf("nested write: %v", err)
	}
	s.mu.Lock()
	after := s.Port
	s.mu.Unlock()
	if after != before {
		t.Errorf("editing plugins/example/server.properties moved the server's port %d -> %d", before, after)
	}
}

// A19. Reservations are per-lease: releasing one operation's claim must drop
// its whole set (span and extras) without touching another lease held under
// the same display name.
func TestPortLeasesReleaseWholeSetsWithoutCrossRelease(t *testing.T) {
	mgr := NewManager(t.TempDir(), NewHub())
	if err := mgr.Load(); err != nil {
		t.Fatal(err)
	}

	span, err := candidateBindings(31000, []string{"udp"}, 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	other, err := candidateBindings(31100, []string{"udp"}, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	l1, _, ok := mgr.claimPortBindings(span, "same display name")
	if !ok {
		t.Fatal("could not claim the first lease")
	}
	if _, _, ok := mgr.claimPortBindings(other, "same display name"); !ok {
		t.Fatal("could not claim the second lease under the same display name")
	}

	mgr.releaseReservation(l1)
	mgr.mu.RLock()
	_, spanResidue := mgr.reservedPorts[31000]
	_, otherHeld := mgr.reservedPorts[31100]
	mgr.mu.RUnlock()
	if spanResidue {
		t.Error("releasing a lease leaked its base port")
	}
	if !otherHeld {
		t.Error("releasing one lease dropped a different lease's port (cross-release by display name)")
	}
}

// A26. An oversized console line is truncated in place and reading continues;
// the next line must still arrive.
func TestReadLogLineTruncatesAndKeepsReading(t *testing.T) {
	payload := strings.Repeat("A", 3<<20) + "\nafter the flood\n"
	br := bufio.NewReader(strings.NewReader(payload))
	line, truncated, err := readLogLine(br, 1<<20)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !truncated || len(line) != 1<<20 {
		t.Fatalf("truncated=%v len=%d; want the 1 MiB cap applied in place", truncated, len(line))
	}
	next, _, err := readLogLine(br, 1<<20)
	if err != nil || string(next) != "after the flood" {
		t.Fatalf("line after the flood = %q (%v); the reader must continue", next, err)
	}
}

// A27. Namespaced commands are dispatched as their plain verb by
// Paper-ecosystem servers; the denylist has to see the verb, not the prefix.
func TestConsoleVerbStripsNamespaces(t *testing.T) {
	for in, want := range map[string]string{
		"op Steve":           "op",
		"/op Steve":          "op",
		"minecraft:op Steve": "op",
		"essentials:ban x":   "ban",
		"ban-ip 1.2.3.4":     "ban-ip",
	} {
		if got := consoleVerb(in); got != want {
			t.Errorf("consoleVerb(%q) = %q, want %q", in, got, want)
		}
	}
	if !mcpBlockedVerbs[consoleVerb("minecraft:op Steve")] {
		t.Error("a namespaced op command reached past the blocked-verb check")
	}
}

// A28. Duplicate token names make revocation ambiguous: issuing them is
// refused, and a legacy store holding duplicates is revoked completely.
func TestMCPTokensRejectDuplicateNamesAndRevokeAll(t *testing.T) {
	toks := newMCPTokens(t.TempDir())
	if _, err := toks.Issue("agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := toks.Issue("agent"); err == nil {
		t.Error("issuing a second token under a live name was allowed")
	}
	// A legacy store can already hold duplicates from before the check.
	toks.mu.Lock()
	toks.toks = append(toks.toks,
		mcpToken{Name: "legacy", Hash: mcpHashPrefix + "aa"},
		mcpToken{Name: "legacy", Hash: mcpHashPrefix + "bb"})
	toks.mu.Unlock()
	if err := toks.Revoke("legacy"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	for _, x := range toks.List() {
		if x["name"] == "legacy" {
			t.Error("revoking a duplicated name left a working credential behind")
		}
	}
}

// A31. The preview must agree with civil time on a daylight-saving day:
// midnight-plus-duration lands on 04:30 for a 03:30 task on the spring-forward
// day, while the loop compares wall-clock time.
func TestNextRunUsesCivilTimeOnDSTDays(t *testing.T) {
	loc, err := time.LoadLocation("America/Vancouver")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	task := &Task{Time: "03:30", Repeat: true}
	now := time.Date(2026, time.March, 7, 12, 0, 0, 0, loc) // the day before spring-forward
	next := task.NextRun(now)
	if next.IsZero() {
		t.Fatal("no next run computed")
	}
	if next.Hour() != 3 || next.Minute() != 30 {
		t.Fatalf("next run for a 03:30 task across spring-forward = %s; the preview must name civil 03:30, not the shifted wall time", next.Format(time.RFC3339))
	}
}

// A36. Two installs racing for one plugin name: the existence check and the
// publish used to be two steps, and the last rename silently won. Exactly one
// install may land.
func TestConcurrentInstallsCannotOverwrite(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := newPluginServer(t, mgr, "paper")

	mux := http.NewServeMux()
	mux.HandleFunc("/race.jar", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond) // widen the window the check used to leave open
		_, _ = w.Write(jarBytes("racer"))
	})
	origin := httptest.NewServer(mux)
	defer origin.Close()

	var wg sync.WaitGroup
	var mu sync.Mutex
	var succeeded int
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := mgr.InstallPlugin(context.Background(), s, origin.URL+"/race.jar", ""); err == nil {
				mu.Lock()
				succeeded++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if succeeded != 1 {
		t.Errorf("%d of 2 racing installs succeeded; exactly one may", succeeded)
	}
}

// A42. An unknown runtime is refused rather than silently simulated, and the
// template's own default resources must fit the host - not just the values
// the caller supplied.
func TestCreateRejectsUnknownRuntimeAndOversizedDefaults(t *testing.T) {
	mgr := NewManager(t.TempDir(), NewHub())
	if err := mgr.Load(); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Create("typo", "vanilla", "1.20.4", 26500, 0, 0, "dcker"); err == nil {
		t.Error("a misspelled runtime silently created a simulated server")
	}

	oldMem := hostMemMB
	hostMemMB = 1024 // paper's own default is 4096
	defer func() { hostMemMB = oldMem }()
	if _, err := mgr.Create("toobig", "paper", "1.20.4", 26501, 0, 0, RuntimeSim); err == nil {
		t.Error("a template default larger than the host was accepted because only supplied values were checked")
	}
}

// A31, second half: the loop's once-per-local-date suppression keeps a task
// from firing twice when the clocks fall back and the same local hour repeats.
func TestSchedulerSuppressesARefireWithinTheSameLocalDate(t *testing.T) {
	a := time.Date(2026, time.November, 1, 1, 30, 0, 0, time.Local)
	b := a.Add(90 * time.Minute) // the repeated 01:30
	if !sameLocalDate(a, b) {
		t.Fatalf("the repeated fall-back hour %v and %v must count as the same local date", a, b)
	}
	nextDay := b.Add(24 * time.Hour)
	if sameLocalDate(a, nextDay) {
		t.Error("the following day counts as the same local date")
	}
}

// A20. serverMetrics reads the resource limits under the server lock; this is
// the HTTP-level check that a concurrent SetResources does not race it. Run
// under -race in CI.
func TestServerMetricsSurvivesConcurrentResourceUpdates(t *testing.T) {
	srv, mgr := newAuthedTestAgent(t)
	defer srv.Close()
	s := mgr.List()[0]

	_, err := mgr.auth.CreateFirstUser("boss", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := mgr.auth.Login("boss", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			_, _ = mgr.SetResources(s, 1024+i, 1)
		}
	}()
	for i := 0; i < 200; i++ {
		req, _ := http.NewRequest("GET", srv.URL+"/api/servers/"+s.ID+"/metrics", nil)
		req.AddCookie(&http.Cookie{Name: "gss_session", Value: sess.Token})
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("metrics: %v", err)
		}
		var body struct {
			LimitMB int `json:"limit_mb"`
		}
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		res.Body.Close()
		if body.LimitMB == 0 {
			t.Fatal("metrics returned no limit")
		}
	}
	<-done
}

// A12. A revoked session loses the SSE event stream, not just future
// commands: the feed reauthorizes on every frame and on every ping.
func TestRevokedSessionLosesTheEventStream(t *testing.T) {
	srv, mgr := newAuthedTestAgent(t)
	defer srv.Close()
	if _, err := mgr.auth.CreateFirstUser("boss", "correct-horse-battery"); err != nil {
		t.Fatal(err)
	}
	// A second admin, so deleting the streaming one is allowed at all.
	if _, err := mgr.auth.CreateUser("decoy", "correct-horse-battery", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	sess, err := mgr.auth.Login("boss", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest("GET", srv.URL+"/api/events", nil)
	req.AddCookie(&http.Cookie{Name: "gss_session", Value: sess.Token})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("events: %d", res.StatusCode)
	}

	// Revoke by deleting the account, then nudge the feed with an event. The
	// stream must end shortly after: the next frame or ping reauthorizes and
	// finds nothing.
	if err := mgr.auth.DeleteUser("boss"); err != nil {
		t.Fatal(err)
	}
	mgr.broadcastEvent("metrics", "")

	ended := make(chan struct{})
	go func() {
		defer close(ended)
		_, _ = io.Copy(io.Discard, res.Body)
	}()
	select {
	case <-ended:
		// The stream ended: the revocation landed.
	case <-time.After(15 * time.Second):
		t.Fatal("the event stream kept serving a deleted user's session")
	}
}

// A30. Task routes match both the server ID and the task ID: a URL naming the
// wrong server must not find (or touch) another server's task.
func TestTaskMutationsRequireTheRightServer(t *testing.T) {
	_, mgr := newTestAgent(t)
	first, second := mgr.List()[0], mgr.List()[1]

	task, err := mgr.sched.Add(&Task{
		ServerID: first.ID, Name: "nightly", Commands: "say hi", Time: "04:00", Repeat: true, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.sched.Update(second.ID, task.ID, func(t *Task) { t.Name = "hijacked" }); err == nil {
		t.Error("a task was updated through the wrong server's route")
	}
	if err := mgr.sched.Delete(second.ID, task.ID); err == nil {
		t.Error("a task was deleted through the wrong server's route")
	}
	if got := mgr.sched.Get(task.ID); got == nil || got.Name != "nightly" {
		t.Errorf("the task did not survive mismatched-route mutations: %+v", got)
	}
	if err := mgr.sched.Run(second.ID, task.ID, "tester"); err == nil {
		t.Error("a task ran through the wrong server's route")
	}
}

// A06. The backup note the handler accepts is bounded, and the note body is
// parsed while the request deadline still applies. HTTP-level check.
func TestBackupNoteIsBounded(t *testing.T) {
	srv, mgr := newAuthedTestAgent(t)
	defer srv.Close()
	if _, err := mgr.auth.CreateFirstUser("boss", "correct-horse-battery"); err != nil {
		t.Fatal(err)
	}
	sess, err := mgr.auth.Login("boss", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	s := mgr.List()[0]

	body, _ := json.Marshal(map[string]string{"note": strings.Repeat("x", 5000)})
	req, _ := http.NewRequest("POST", srv.URL+"/api/servers/"+s.ID+"/backups", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "gss_session", Value: sess.Token})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("an oversized backup note got %d, want 400", res.StatusCode)
	}
}

// A11. Login rejects absurd credential lengths before spending PBKDF2 work
// and before the name can reach the audit log unbounded.
func TestLoginBoundsCredentialLengths(t *testing.T) {
	srv, mgr := newAuthedTestAgent(t)
	defer srv.Close()
	if _, err := mgr.auth.CreateFirstUser("boss", "correct-horse-battery"); err != nil {
		t.Fatal(err)
	}
	for name, password := range map[string]string{
		strings.Repeat("n", 5000): "correct-horse-battery",
		"boss":                    strings.Repeat("p", 5000),
	} {
		body, _ := json.Marshal(map[string]string{"name": name, "password": password})
		res, err := http.Post(srv.URL+"/api/login", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("login with absurd field lengths got %d, want 401", res.StatusCode)
		}
	}
	for _, e := range mgr.auth.Audit(0) {
		if e.Action == "auth.login_failed" && len(e.Actor) > 100 {
			t.Errorf("an unbounded claimed name reached the audit log: %d chars", len(e.Actor))
		}
	}
}

// A43. NextFreePort never recommends a port it watched be occupied; it
// reports 0 when the range is exhausted.
func TestNextFreePortReportsExhaustion(t *testing.T) {
	mgr := NewManager(t.TempDir(), NewHub())
	if err := mgr.Load(); err != nil {
		t.Fatal(err)
	}
	// 65535 itself is a legal port and may be suggested; the bug was scanning
	// PAST it (400 candidates above the hint) and recommending ports that do
	// not exist, or falling back to an occupied hint. Occupy it and the search
	// must honestly report exhaustion instead.
	lease, _, ok := mgr.claimPort(65535, "holder")
	if !ok {
		t.Fatal("could not occupy 65535")
	}
	defer mgr.releaseReservation(lease)
	if p := mgr.NextFreePort(65535); p != 0 {
		t.Errorf("NextFreePort suggested %d with 65535 taken and nothing above it; exhaustion must be 0, never an occupied fallback", p)
	}
}

// A07. The gzip ISIZE trailer wraps modulo 4 GiB, so the admission heuristic
// falls back to a multiple of the compressed size when the recorded value is
// below it - and admits it knows nothing (0) for a stream too small to carry
// a trailer. Per-entry free-space checks during extraction are the real
// guard; this pins the heuristic's own branches.
func TestUncompressedSizeHeuristicBranches(t *testing.T) {
	dir := t.TempDir()
	// Smaller than an empty gzip member: unknown, reported as 0.
	tiny := filepath.Join(dir, "tiny.gz")
	if err := os.WriteFile(tiny, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := uncompressedSize(tiny, 10); got != 0 {
		t.Errorf("tiny stream estimated %d, want 0 (unknown)", got)
	}
	// A wrapped ISIZE (recorded length below the compressed size) falls back
	// to the compressed multiple instead of trusting the trailer.
	wrapped := filepath.Join(dir, "wrapped.gz")
	body := make([]byte, 64)
	for i := range body {
		body[i] = byte(i * 7)
	}
	copy(body[60:], []byte{1, 0, 0, 0}) // ISIZE = 1 < compressed 64
	if err := os.WriteFile(wrapped, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := uncompressedSize(wrapped, 64); got != 64*4 {
		t.Errorf("wrapped-trailer estimate = %d, want the %d fallback", got, 64*4)
	}
}
