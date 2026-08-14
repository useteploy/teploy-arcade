package arcade

import (
	"net/http"
	"strings"
	"testing"
)

// The panel had no way to change a password at all: no route, no UI, no field.
// An admin who created an account knew its password forever, nobody could
// rotate their own, and the first admin's password was fixed for the life of
// the install unless someone hand-edited users.json.

// post sends a JSON body as the given session and returns the status code.
func post(t *testing.T, srv string, path, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("POST", srv+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.AddCookie(&http.Cookie{Name: "gss_session", Value: token})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 512)
	n, _ := resp.Body.Read(buf)
	return resp.StatusCode, string(buf[:n])
}

func getAs(t *testing.T, srv, path, token string) int {
	t.Helper()
	req, _ := http.NewRequest("GET", srv+path, nil)
	if token != "" {
		req.AddCookie(&http.Cookie{Name: "gss_session", Value: token})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestAUserCanChangeTheirOwnPassword(t *testing.T) {
	srv, mgr := newAuthedTestAgent(t)
	defer srv.Close()

	if _, err := mgr.auth.CreateFirstUser("ada", "firstpassword"); err != nil {
		t.Fatalf("create: %v", err)
	}
	sess, err := mgr.auth.Login("ada", "firstpassword")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	// The wrong current password is refused, or a stolen session is enough to
	// lock the owner out of their own account.
	if code, body := post(t, srv.URL, "/api/users/ada/password", sess.Token,
		`{"current":"notit","new":"secondpassword"}`); code != 400 {
		t.Fatalf("wrong current password accepted: %d %s", code, body)
	}

	if code, body := post(t, srv.URL, "/api/users/ada/password", sess.Token,
		`{"current":"firstpassword","new":"secondpassword"}`); code != 200 {
		t.Fatalf("change refused: %d %s", code, body)
	}
	if _, err := mgr.auth.Login("ada", "secondpassword"); err != nil {
		t.Fatalf("new password does not work: %v", err)
	}
	if _, err := mgr.auth.Login("ada", "firstpassword"); err == nil {
		t.Fatal("the old password still works")
	}
}

// Changing a password is what you do when you think someone else has it. It
// means nothing if the session they opened with it keeps working.
func TestChangingAPasswordDropsTheOtherSessions(t *testing.T) {
	srv, mgr := newAuthedTestAgent(t)
	defer srv.Close()

	if _, err := mgr.auth.CreateFirstUser("ada", "firstpassword"); err != nil {
		t.Fatalf("create: %v", err)
	}
	mine, _ := mgr.auth.Login("ada", "firstpassword")
	theirs, _ := mgr.auth.Login("ada", "firstpassword")

	if code, body := post(t, srv.URL, "/api/users/ada/password", mine.Token,
		`{"current":"firstpassword","new":"secondpassword"}`); code != 200 {
		t.Fatalf("change refused: %d %s", code, body)
	}

	if mgr.auth.Session(theirs.Token) != nil {
		t.Fatal("the other session survived the password change")
	}
	if mgr.auth.Session(mine.Token) == nil {
		t.Fatal("the session that made the change was dropped")
	}
}

// An admin sets, and therefore knows, another user's first password. That
// account is refused everything until it sets one of its own.
func TestAdminCreatedAccountsAreLockedUntilTheyChangeTheirPassword(t *testing.T) {
	srv, mgr := newAuthedTestAgent(t)
	defer srv.Close()

	if _, err := mgr.auth.CreateFirstUser("ada", "adapassword"); err != nil {
		t.Fatalf("create first: %v", err)
	}
	if _, err := mgr.auth.CreateUser("bob", "temporarypass", RoleOperator); err != nil {
		t.Fatalf("create: %v", err)
	}
	if mgr.auth.MustChangePassword("ada") {
		t.Fatal("the first admin chose their own password; it must not be flagged")
	}
	if !mgr.auth.MustChangePassword("bob") {
		t.Fatal("an admin-created account is not flagged must-change")
	}

	bob, err := mgr.auth.Login("bob", "temporarypass")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	if code := getAs(t, srv.URL, "/api/servers", bob.Token); code != 403 {
		t.Fatalf("a locked-out account reached the panel: %d", code)
	}
	// /api/me has to keep answering, or the client cannot find out why.
	if code := getAs(t, srv.URL, "/api/me", bob.Token); code != 200 {
		t.Fatalf("/api/me refused a locked-out account: %d", code)
	}

	if code, body := post(t, srv.URL, "/api/users/bob/password", bob.Token,
		`{"current":"temporarypass","new":"bobsownpassword"}`); code != 200 {
		t.Fatalf("the one route a locked-out account needs was refused: %d %s", code, body)
	}
	if mgr.auth.MustChangePassword("bob") {
		t.Fatal("still flagged after setting a password")
	}
	if code := getAs(t, srv.URL, "/api/servers", bob.Token); code != 200 {
		t.Fatalf("still locked out after the change: %d", code)
	}
}

func TestOnlyAnAdminCanSetSomeoneElsesPassword(t *testing.T) {
	srv, mgr := newAuthedTestAgent(t)
	defer srv.Close()

	if _, err := mgr.auth.CreateFirstUser("ada", "adapassword"); err != nil {
		t.Fatalf("create first: %v", err)
	}
	if _, err := mgr.auth.CreateUser("bob", "temporarypass", RoleOperator); err != nil {
		t.Fatalf("create bob: %v", err)
	}
	if _, err := mgr.auth.CreateUser("eve", "temporarypass", RoleOperator); err != nil {
		t.Fatalf("create eve: %v", err)
	}
	// Clear bob's lockout so the refusal below is about role, not must-change.
	if err := mgr.auth.SetPassword("bob", "temporarypass", "bobsownpassword", "", false); err != nil {
		t.Fatalf("bob sets his own: %v", err)
	}
	bob, err := mgr.auth.Login("bob", "bobsownpassword")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	if code, body := post(t, srv.URL, "/api/users/eve/password", bob.Token,
		`{"new":"takenoverpass"}`); code != 403 {
		t.Fatalf("an operator reset another account's password: %d %s", code, body)
	}

	ada, _ := mgr.auth.Login("ada", "adapassword")
	if code, body := post(t, srv.URL, "/api/users/eve/password", ada.Token,
		`{"new":"resetbyadmin1"}`); code != 200 {
		t.Fatalf("admin reset refused: %d %s", code, body)
	}
	if !mgr.auth.MustChangePassword("eve") {
		t.Fatal("an admin reset did not re-arm must-change")
	}
}

// newAccount creates a user the way an admin does, then clears the must-change
// lock the way its owner would at first sign-in.
//
// Tests that just want a working session for a role want this. Tests about the
// lock itself call CreateUser directly - which is what made three of them fail
// the moment the lock landed, correctly: an account an admin created cannot use
// the panel until it sets its own password.
func newAccount(t *testing.T, a *Auth, name, password, role string) {
	t.Helper()
	if _, err := a.CreateUser(name, password, role); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.users[strings.ToLower(name)].MustChange = false
}
