package arcade

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// postJSON is a small helper: these tests are about status codes on the auth
// routes, and every one of them needs the same three lines.
func postJSON(t *testing.T, url, body string) (int, string) {
	t.Helper()
	res, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return res.StatusCode, strings.TrimSpace(string(b))
}

// A panel with no users is open, and admin here means creating containers as
// root on the host. Whoever reaches /api/setup first would otherwise own the
// box. The token is printed to the log at startup, so claiming the panel takes
// access to the machine, not merely a route to it.
//
// Matches teploy-dash's bootstrapToken design, deliberately - the two panels
// are run by the same people.
func TestFirstRunSetupRequiresTheBootstrapToken(t *testing.T) {
	srv, mgr := newAuthedTestAgent(t)
	defer srv.Close()

	// Load() is what mints the token on a panel with no users, exactly as the
	// real startup path does.
	// The real startup sequence: load users, then open setup if there are none.
	// Minting lives in BeginSetup so startup credentials can be provisioned
	// first - a panel given -admin-password never opens setup at all.
	if err := mgr.auth.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := mgr.auth.BeginSetup(); err != nil {
		t.Fatalf("begin setup: %v", err)
	}
	token := mgr.auth.bootstrapToken
	if token == "" {
		t.Fatal("no bootstrap token was generated for a panel with no users")
	}

	for _, c := range []struct {
		name, body string
		wantCode   int
	}{
		{"no token", `{"name":"admin","password":"correct-horse-battery"}`, 403},
		{"empty token", `{"name":"admin","password":"correct-horse-battery","token":""}`, 403},
		{"wrong token", `{"name":"admin","password":"correct-horse-battery","token":"deadbeef"}`, 403},
		{"token of the right shape but wrong value", `{"name":"admin","password":"correct-horse-battery","token":"` + strings.Repeat("a", len(token)) + `"}`, 403},
	} {
		code, body := postJSON(t, srv.URL+"/api/setup", c.body)
		if code != c.wantCode {
			t.Errorf("%s: got %d %s, want %d", c.name, code, body, c.wantCode)
		}
	}
	if n := len(mgr.auth.Users()); n != 0 {
		t.Fatalf("a refused setup still created %d user(s)", n)
	}

	// The real token works.
	code, body := postJSON(t, srv.URL+"/api/setup",
		`{"name":"admin","password":"correct-horse-battery","token":"`+token+`"}`)
	if code != 201 {
		t.Fatalf("setup with the correct token: got %d %s, want 201", code, body)
	}
	if n := len(mgr.auth.Users()); n != 1 {
		t.Fatalf("setup created %d users, want 1", n)
	}

	// And it is spent: the panel now has an admin, so setup is closed even to a
	// caller holding the token.
	code, _ = postJSON(t, srv.URL+"/api/setup",
		`{"name":"second","password":"correct-horse-battery","token":"`+token+`"}`)
	if code != 409 {
		t.Errorf("setup after an admin exists: got %d, want 409", code)
	}
	if n := len(mgr.auth.Users()); n != 1 {
		t.Errorf("a second setup created a user: %d total", n)
	}
}

// An abandoned setup must not leave the panel claimable forever, and the
// operator needs to be told which of the two things went wrong.
func TestAnExpiredBootstrapTokenIsRefusedAndSaysSo(t *testing.T) {
	srv, mgr := newAuthedTestAgent(t)
	defer srv.Close()
	// The real startup sequence: load users, then open setup if there are none.
	// Minting lives in BeginSetup so startup credentials can be provisioned
	// first - a panel given -admin-password never opens setup at all.
	if err := mgr.auth.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := mgr.auth.BeginSetup(); err != nil {
		t.Fatalf("begin setup: %v", err)
	}
	token := mgr.auth.bootstrapToken

	mgr.auth.mu.Lock()
	mgr.auth.bootstrapExpiry = time.Now().Add(-time.Minute)
	mgr.auth.mu.Unlock()

	if mgr.auth.CheckBootstrapToken(token) {
		t.Error("an expired token was accepted")
	}
	code, body := postJSON(t, srv.URL+"/api/setup",
		`{"name":"admin","password":"correct-horse-battery","token":"`+token+`"}`)
	if code != 403 {
		t.Fatalf("expired token: got %d %s, want 403", code, body)
	}
	// "wrong token" and "too late" need different fixes, so they need different
	// messages: one is retyped, the other needs the panel restarted.
	if !strings.Contains(body, "window has closed") {
		t.Errorf("an expired token should say the window closed, got: %s", body)
	}
	if n := len(mgr.auth.Users()); n != 0 {
		t.Errorf("an expired setup created %d user(s)", n)
	}
}

// The login ROUTE, over HTTP.
//
// This did not exist, and its absence hid a real break: a misplaced edit put the
// setup-token gate inside login, so every login returned 403, and the whole
// suite still passed because every other test calls auth.Login() directly and
// never touches the handler. The most security-relevant route in the panel had
// no HTTP-level coverage at all.
func TestLoginRouteIssuesAWorkingSession(t *testing.T) {
	srv, mgr := newAuthedTestAgent(t)
	defer srv.Close()
	// The real startup sequence: load users, then open setup if there are none.
	// Minting lives in BeginSetup so startup credentials can be provisioned
	// first - a panel given -admin-password never opens setup at all.
	if err := mgr.auth.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := mgr.auth.BeginSetup(); err != nil {
		t.Fatalf("begin setup: %v", err)
	}
	if _, err := mgr.auth.CreateFirstUser("admin", "correct-horse-battery"); err != nil {
		t.Fatalf("create admin: %v", err)
	}

	// Wrong password is refused.
	if code, _ := postJSON(t, srv.URL+"/api/login", `{"name":"admin","password":"wrong"}`); code != 401 {
		t.Errorf("bad password: got %d, want 401", code)
	}
	// Unknown user is refused, with the same status - a different one enumerates
	// which names exist.
	if code, _ := postJSON(t, srv.URL+"/api/login", `{"name":"nobody","password":"correct-horse-battery"}`); code != 401 {
		t.Errorf("unknown user: got %d, want 401", code)
	}

	// The real credentials work and hand back a usable session.
	res, err := http.Post(srv.URL+"/api/login", "application/json",
		strings.NewReader(`{"name":"admin","password":"correct-horse-battery"}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("login: got %d, want 200", res.StatusCode)
	}
	var cookie *http.Cookie
	for _, c := range res.Cookies() {
		if c.Value != "" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("login returned no session cookie")
	}

	// And the session actually opens a gated route.
	req, _ := http.NewRequest("GET", srv.URL+"/api/host", nil)
	req.AddCookie(cookie)
	got, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got.Body.Close()
	if got.StatusCode != 200 {
		t.Errorf("session from /api/login did not open /api/host: got %d", got.StatusCode)
	}

	// Without it, the same route is closed.
	anon, err := http.Get(srv.URL + "/api/host")
	if err != nil {
		t.Fatal(err)
	}
	anon.Body.Close()
	if anon.StatusCode != 401 {
		t.Errorf("anonymous /api/host: got %d, want 401", anon.StatusCode)
	}
}

// /api/me is what the settings screen renders "Signed in as" from, and its
// content had no test - only a check that it returned 200.
//
// It used to answer with an invented {name: "local", role: "admin"} whenever
// auth was off, which the UI showed as "Signed in as local (admin)". That
// conflates what a caller may DO (with no accounts, everything) with who they
// ARE (nobody), and it reads as though an admin account exists in exactly the
// state where the operator most needs to know that one does not.
func TestMeDoesNotInventAUserOnAnUnclaimedPanel(t *testing.T) {
	srv, mgr := newAuthedTestAgent(t)
	defer srv.Close()
	// The real startup sequence: load users, then open setup if there are none.
	// Minting lives in BeginSetup so startup credentials can be provisioned
	// first - a panel given -admin-password never opens setup at all.
	if err := mgr.auth.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := mgr.auth.BeginSetup(); err != nil {
		t.Fatalf("begin setup: %v", err)
	}

	get := func() map[string]any {
		t.Helper()
		res, err := http.Get(srv.URL + "/api/me")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var m map[string]any
		if err := json.NewDecoder(res.Body).Decode(&m); err != nil {
			t.Fatalf("decode /api/me: %v", err)
		}
		return m
	}

	// Unclaimed: no account, so no user.
	m := get()
	if m["needs_setup"] != true || m["auth_enabled"] != false {
		t.Errorf("unclaimed panel: needs_setup=%v auth_enabled=%v", m["needs_setup"], m["auth_enabled"])
	}
	if _, ok := m["user"]; ok {
		t.Errorf("an unclaimed panel reported a user: %#v", m["user"])
	}
	if m["unclaimed"] != true {
		t.Errorf("unclaimed=%v, want true", m["unclaimed"])
	}
	// The authority is still reported, just not as an account.
	if m["effective_role"] != RoleAdmin {
		t.Errorf("effective_role=%v, want %q", m["effective_role"], RoleAdmin)
	}

	// Claimed and signed in: a real user, and no "unclaimed" flag.
	token := mgr.auth.bootstrapToken
	if code, body := postJSON(t, srv.URL+"/api/setup",
		`{"name":"tyler","password":"correct-horse-battery","token":"`+token+`"}`); code != 201 {
		t.Fatalf("setup: %d %s", code, body)
	}
	res, err := http.Post(srv.URL+"/api/login", "application/json",
		strings.NewReader(`{"name":"tyler","password":"correct-horse-battery"}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	var cookie *http.Cookie
	for _, c := range res.Cookies() {
		if c.Value != "" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("login returned no session cookie")
	}

	req, _ := http.NewRequest("GET", srv.URL+"/api/me", nil)
	req.AddCookie(cookie)
	got, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()
	var signed map[string]any
	if err := json.NewDecoder(got.Body).Decode(&signed); err != nil {
		t.Fatal(err)
	}
	if signed["needs_setup"] != false || signed["auth_enabled"] != true {
		t.Errorf("claimed panel: needs_setup=%v auth_enabled=%v", signed["needs_setup"], signed["auth_enabled"])
	}
	if _, ok := signed["unclaimed"]; ok {
		t.Error("a claimed panel still reports unclaimed")
	}
	u, ok := signed["user"].(map[string]any)
	if !ok {
		t.Fatalf("no user for a signed-in caller: %#v", signed["user"])
	}
	if u["name"] != "tyler" || u["role"] != RoleAdmin {
		t.Errorf("user = %#v, want tyler/admin", u)
	}
	// The invented identity must be gone for good.
	if u["name"] == "local" {
		t.Error(`the synthetic "local" user came back`)
	}
}

// The path most deployments should take: credentials arrive with the deploy, so
// the panel is never unclaimed and the operator never meets a setup token.
//
// This is what TEPLOY_DASH_PASSWORD does for teploy-dash, and its absence here
// was a real gap - the token was Arcade's ONLY route to a first account, which
// made a reasonable fallback look like a requirement.
func TestStartupCredentialsSkipSetupEntirely(t *testing.T) {
	_, mgr := newAuthedTestAgent(t)
	if err := mgr.auth.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}

	created, err := mgr.auth.EnsureAdmin("tyler", "correct-horse-battery")
	if err != nil {
		t.Fatalf("ensure admin: %v", err)
	}
	if !created {
		t.Fatal("no admin was created from startup credentials")
	}
	if !mgr.auth.Enabled() {
		t.Error("auth is not enforced after provisioning an admin")
	}

	// BeginSetup must now be a no-op: there is an account, so there is nothing
	// to claim and no token to leak into the log.
	if err := mgr.auth.BeginSetup(); err != nil {
		t.Fatalf("begin setup: %v", err)
	}
	if mgr.auth.bootstrapToken != "" {
		t.Error("a provisioned panel still minted a setup token")
	}

	// The provisioned credentials actually work.
	if _, err := mgr.auth.Login("tyler", "correct-horse-battery"); err != nil {
		t.Errorf("provisioned credentials do not log in: %v", err)
	}

	// And they do not override an existing panel: running with the env var set
	// on an already-configured panel must not touch its accounts.
	again, err := mgr.auth.EnsureAdmin("someone-else", "another-password")
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if again {
		t.Error("startup credentials created a second account on a configured panel")
	}
	if n := len(mgr.auth.Users()); n != 1 {
		t.Errorf("%d users after a second EnsureAdmin, want 1", n)
	}
}

// No password means no provisioning - the panel falls back to setup rather than
// creating an account with an empty or defaulted credential.
func TestNoStartupPasswordLeavesSetupOpen(t *testing.T) {
	_, mgr := newAuthedTestAgent(t)
	if err := mgr.auth.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	created, err := mgr.auth.EnsureAdmin("admin", "")
	if err != nil {
		t.Fatalf("ensure admin: %v", err)
	}
	if created {
		t.Fatal("an account was created with no password")
	}
	if err := mgr.auth.BeginSetup(); err != nil {
		t.Fatal(err)
	}
	if mgr.auth.bootstrapToken == "" {
		t.Error("an unprovisioned panel did not open setup")
	}
}

// -no-auth switches enforcement off for one process; it does not delete the
// accounts. Reporting needs_setup there showed the create-first-admin form on a
// panel that already had an admin. Found running a no-auth instance during a
// migration.
func TestNoAuthDoesNotMakeAClaimedPanelLookUnclaimed(t *testing.T) {
	srv, mgr := newAuthedTestAgent(t)
	defer srv.Close()
	if err := mgr.auth.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := mgr.auth.CreateFirstUser("tyler", "correct-horse-battery"); err != nil {
		t.Fatalf("create admin: %v", err)
	}

	mgr.auth.Disable() // as -no-auth does

	res, err := http.Get(srv.URL + "/api/me")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(res.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m["auth_enabled"] != false {
		t.Errorf("auth_enabled = %v; -no-auth should switch enforcement off", m["auth_enabled"])
	}
	if m["needs_setup"] == true {
		t.Error("a panel with an account reported needs_setup under -no-auth")
	}
	if _, ok := m["unclaimed"]; ok {
		t.Error("a panel with an account reported itself unclaimed under -no-auth")
	}
	if !mgr.auth.HasUsers() {
		t.Error("HasUsers() lost the account when enforcement was disabled")
	}
}
