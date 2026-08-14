package arcade

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// newAuthedTestAgent wires the attach middleware the binary uses. newTestAgent
// serves the bare mux, so no request there ever carries a session and anything
// about a signed-in caller is untestable against it.
func newAuthedTestAgent(t *testing.T) (*httptest.Server, *Manager) {
	t.Helper()
	hub := NewHub()
	mgr := NewManager(t.TempDir(), hub)
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	api := &API{mgr: mgr, hub: hub}
	mux := http.NewServeMux()
	api.Routes(mux)
	return httptest.NewServer(mgr.auth.attach(mux)), mgr
}

func dialConsoleAs(t *testing.T, srv *httptest.Server, id, token string) *websocket.Conn {
	t.Helper()
	h := http.Header{}
	h.Set("Cookie", "gss_session="+token)
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/console?server=" + id
	c, _, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c
}

func countAuditAction(mgr *Manager, action string) int {
	n := 0
	for _, e := range mgr.auth.Audit(0) {
		if e.Action == action {
			n++
		}
	}
	return n
}

// H5. The console socket resolved its session once, at upgrade, and held that
// pointer for the life of the connection: deleting a user revoked their cookie
// but not their open console, so a sacked admin kept command access until they
// happened to disconnect. Revocation has to land on the next command.
func TestConsoleRevalidatesTheSessionOnEveryCommand(t *testing.T) {
	srv, mgr := newAuthedTestAgent(t)
	defer srv.Close()
	s := startServer(t, mgr)

	// A second admin, so deleting the operator is not "the last admin".
	newAccount(t, mgr.auth, "boss", "correct-horse-battery", RoleAdmin)
	newAccount(t, mgr.auth, "op", "correct-horse-battery", RoleOperator)
	sess, err := mgr.auth.Login("op", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}

	c := dialConsoleAs(t, srv, s.ID, sess.Token)
	defer c.CloseNow()
	readMsg(t, c, 3*time.Second) // replay

	send := func(id, text string) {
		t.Helper()
		if err := c.Write(context.Background(), websocket.MessageText, mustJSON(map[string]any{
			"t": "command", "id": id, "text": text,
		})); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	send("1", "list")
	ack := readUntil(t, c, 3*time.Second, func(m msg) bool { return m["t"] == "command_ack" })
	if ack == nil || ack["accepted"] != true {
		t.Fatalf("a live operator's command was rejected: %v", ack)
	}

	if err := mgr.auth.DeleteUser("op"); err != nil {
		t.Fatalf("delete user: %v", err)
	}

	send("2", "stop")
	ack = readUntil(t, c, 3*time.Second, func(m msg) bool {
		return m["t"] == "command_ack" && m["id"] == "2"
	})
	if ack != nil && ack["accepted"] == true {
		t.Error("a deleted user's command was accepted; the socket is still trusting the session it captured at upgrade")
	}
	if n := countAuditAction(mgr, "console.command"); n != 1 {
		t.Errorf("%d console commands reached the server, want 1 - the second ran after the user was deleted", n)
	}
}

// H6. POST /api/servers/{id}/command took the actor from the request body, so an
// operator could attribute a command to a colleague in both the console echo and
// the audit log - and the audit row's detail was empty, so the forged command
// text was recorded nowhere.
func TestPostedCommandIsAttributedToTheSession(t *testing.T) {
	srv, mgr := newAuthedTestAgent(t)
	defer srv.Close()
	s := startServer(t, mgr)

	newAccount(t, mgr.auth, "boss", "correct-horse-battery", RoleAdmin)
	sess, err := mgr.auth.Login("boss", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}

	body := strings.NewReader(`{"text":"say hello","mode":"command","actor":"victim"}`)
	req, _ := http.NewRequest("POST", srv.URL+"/api/servers/"+s.ID+"/command", body)
	req.Header.Set("Cookie", "gss_session="+sess.Token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("command returned %d", res.StatusCode)
	}

	for _, l := range mgr.hub.Tail(s.ID, 500) {
		if strings.Contains(l.Text, "victim >") {
			t.Errorf("console echo attributes the command to %q: %q", "victim", l.Text)
		}
	}
	found := false
	for _, e := range mgr.auth.Audit(0) {
		if e.Actor == "victim" {
			t.Errorf("audit row forged onto %q: %+v", e.Actor, e)
		}
		if e.Action == "server.command" && e.Actor == "boss" && e.Detail == "say hello" {
			found = true
		}
	}
	if !found {
		t.Error("no audit row naming the signed-in actor and the command text; a forged command would be undetectable")
	}
}

// M11. randHex discarded crypto/rand's error and returned the zero-filled
// buffer, so an entropy failure gave every new user the same all-zeros salt and
// every new session the same all-zeros token - one known password would unlock
// every account. Failing closed is the only safe answer.
func TestSaltAndTokenFailClosedWithoutEntropy(t *testing.T) {
	_, mgr := newTestAgent(t)
	a := mgr.auth

	newAccount(t, a, "admin", "correct-horse-battery", RoleAdmin)

	orig := randRead
	randRead = func([]byte) (int, error) { return 0, errors.New("entropy source unavailable") }
	defer func() { randRead = orig }()

	if _, err := a.CreateUser("second", "correct-horse-battery", RoleOperator); err == nil {
		t.Error("a user was created without entropy; its salt is 32 hex zeros")
	}
	if len(a.Users()) != 1 {
		t.Errorf("%d users exist, want 1 - the failed create was persisted anyway", len(a.Users()))
	}
	if _, err := a.Login("admin", "correct-horse-battery"); err == nil {
		t.Error("login minted a session without entropy; its token is 64 hex zeros and every login shares it")
	}
}

// M8. /api/setup checked Enabled() and CreateUser took the lock as two separate
// steps, so concurrent first-run requests all cleared the gate and all got an
// admin. On a panel reachable before setup, that hands whoever races the
// operator an account of their own.
func TestSetupCannotBeRacedIntoTwoAdmins(t *testing.T) {
	srv, mgr := newAuthedTestAgent(t)
	defer srv.Close()

	// Setup is gated on the bootstrap token now, so every caller carries the
	// SAME valid one. That is the sharper version of this race: the token is
	// not what serialises them, the create-under-lock is.
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
		t.Fatal("no bootstrap token for a panel with no users")
	}

	const callers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := mustJSON(map[string]string{
				"name": string(rune('a'+i)) + "dmin", "password": "correct-horse-battery",
				"token": token,
			})
			<-start
			res, err := http.Post(srv.URL+"/api/setup", "application/json", strings.NewReader(string(body)))
			if err != nil {
				return
			}
			res.Body.Close()
		}(i)
	}
	close(start)
	wg.Wait()

	if n := len(mgr.auth.Users()); n != 1 {
		t.Errorf("setup created %d admins, want 1", n)
	}
}

// M17. Manager.Create treats 0 as "assign one" and coerces an unknown runtime,
// but takes any other number as a port. An out-of-range one reaches
// server.properties and the docker -p flag, failing at container start with an
// error that points nowhere near the request that caused it.
func TestCreateServerRejectsOutOfRangePorts(t *testing.T) {
	srv, mgr := newTestAgent(t)
	defer srv.Close()
	before := len(mgr.List())

	for _, port := range []int{-1, 70000} {
		body := mustJSON(map[string]any{
			"name": "Ranged", "template": "paper", "version": "1.20.4", "port": port,
		})
		res, err := http.Post(srv.URL+"/api/servers", "application/json", strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 400 {
			t.Errorf("port %d returned %d, want 400", port, res.StatusCode)
		}
	}
	if got := len(mgr.List()); got != before {
		t.Errorf("%d servers exist, want %d - an out-of-range port created one", got, before)
	}
}

// M16. The file handlers pass the caller's path straight to the file layer,
// which is where traversal is refused (files.go resolve). This pins the handler
// surface: a future handler that reaches around resolve fails here rather than
// in review.
func TestFileHandlersRefuseTraversalPaths(t *testing.T) {
	srv, mgr := newTestAgent(t)
	defer srv.Close()
	id := mgr.List()[0].ID

	for _, p := range []string{"../../../../etc/passwd", "/etc/passwd", "world/../../../etc/hosts"} {
		res, err := http.Get(srv.URL + "/api/servers/" + id + "/file?path=" + p)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		res.Body.Close()
		if res.StatusCode == 200 {
			t.Errorf("read of %q returned 200: %v", p, out["content"])
		}

		req, _ := http.NewRequest("DELETE", srv.URL+"/api/servers/"+id+"/file?path="+p, nil)
		dres, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		dres.Body.Close()
		if dres.StatusCode == 200 {
			t.Errorf("delete of %q was accepted", p)
		}
	}
}
