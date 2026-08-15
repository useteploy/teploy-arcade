package arcade

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Roles have been enforced per route since RBAC landed, and no two sessions had
// ever overlapped anywhere - not in a test, not on the deployed panel, which has
// exactly one account. Everything below is the first time this panel has had two
// people on it at once.

func threeUsers(t *testing.T, mgr *Manager) (admin, operator, viewer string) {
	t.Helper()
	if _, err := mgr.auth.CreateFirstUser("boss", "correct-horse-battery"); err != nil {
		t.Fatalf("first user: %v", err)
	}
	for _, u := range []struct{ name, role string }{
		{"opal", RoleOperator}, {"vera", RoleViewer},
	} {
		if _, err := mgr.auth.CreateUser(u.name, "correct-horse-battery", u.role); err != nil {
			t.Fatalf("create %s: %v", u.name, err)
		}
	}
	// An account still on the password an admin chose is refused every route
	// until it sets its own - deliberately, so the admin's copy stops working.
	// So the onboarding here is the real one: sign in, choose a password, and
	// keep the session that did it.
	onboard := func(name string, isFirst bool) string {
		sess, err := mgr.auth.Login(name, "correct-horse-battery")
		if err != nil {
			t.Fatalf("login %s: %v", name, err)
		}
		if isFirst {
			return sess.Token
		}
		if err := mgr.auth.SetPassword(name, "correct-horse-battery", name+"-own-password", sess.Token, false); err != nil {
			t.Fatalf("%s could not set their own password: %v", name, err)
		}
		if mgr.auth.MustChangePassword(name) {
			t.Fatalf("%s is still locked out after setting a password", name)
		}
		return sess.Token
	}
	return onboard("boss", true), onboard("opal", false), onboard("vera", false)
}

func doAs(t *testing.T, base, token, method, path, body string) int {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, base+path, rdr)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Cookie", "gss_session="+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, path, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// Three people signed in at once, each doing what their role allows and being
// refused what it does not. The sessions are live simultaneously rather than
// one after another, because a session map shared across goroutines is exactly
// where "works for one user" stops being true.
func TestThreeConcurrentSessionsKeepTheirOwnRoles(t *testing.T) {
	srv, mgr := newAuthedTestAgent(t)
	defer srv.Close()
	admin, operator, viewer := threeUsers(t, mgr)
	id := mgr.List()[0].ID

	type check struct {
		who, token, method, path string
		want                     int
	}
	// Reads are open to everyone; driving a server needs operator; creating and
	// destroying needs admin.
	checks := []check{
		{"admin", admin, "GET", "/api/servers", 200},
		{"operator", operator, "GET", "/api/servers", 200},
		{"viewer", viewer, "GET", "/api/servers", 200},

		{"viewer", viewer, "POST", "/api/servers/" + id + "/clear-failures", 403},
		{"operator", operator, "POST", "/api/servers/" + id + "/clear-failures", 200},

		{"viewer", viewer, "DELETE", "/api/servers/" + id, 403},
		{"operator", operator, "DELETE", "/api/servers/" + id, 403},

		{"viewer", viewer, "GET", "/api/users", 403},
		{"operator", operator, "GET", "/api/users", 403},
		{"admin", admin, "GET", "/api/users", 200},
	}

	// All at once, not in sequence.
	var wg sync.WaitGroup
	errs := make(chan string, len(checks)*4)
	for round := 0; round < 4; round++ {
		for _, c := range checks {
			wg.Add(1)
			go func(c check) {
				defer wg.Done()
				if got := doAs(t, srv.URL, c.token, c.method, c.path, ""); got != c.want {
					errs <- fmt.Sprintf("%s %s %s = %d, want %d", c.who, c.method, c.path, got, c.want)
				}
			}(c)
		}
	}
	wg.Wait()
	close(errs)
	seen := map[string]bool{}
	for e := range errs {
		if !seen[e] {
			seen[e] = true
			t.Error(e)
		}
	}
}

// Signing in as one user must not disturb anyone else's session. Login used to
// hold the auth mutex through 120,000 PBKDF2 rounds, which made this a latency
// problem; it must not also be a correctness one.
func TestALoginDoesNotDisturbLiveSessions(t *testing.T) {
	srv, mgr := newAuthedTestAgent(t)
	defer srv.Close()
	admin, operator, viewer := threeUsers(t, mgr)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if _, err := mgr.auth.Login("boss", "correct-horse-battery"); err != nil {
					t.Errorf("login failed mid-flight: %v", err)
					return
				}
				// And a wrong password, which is the path that spends the work
				// without producing a session.
				_, _ = mgr.auth.Login("boss", "wrong")
			}
		}
	}()

	for i := 0; i < 30; i++ {
		for who, tok := range map[string]string{"admin": admin, "operator": operator, "viewer": viewer} {
			if got := doAs(t, srv.URL, tok, "GET", "/api/servers", ""); got != 200 {
				t.Fatalf("%s lost access while someone else was signing in: %d", who, got)
			}
		}
	}
	close(stop)
	wg.Wait()
}

// Deleting a user must end their access immediately, including a console socket
// they already have open - a sacked operator keeping command access until they
// happen to disconnect is the failure this guards.
func TestRevokingAUserEndsTheirLiveSession(t *testing.T) {
	srv, mgr := newAuthedTestAgent(t)
	defer srv.Close()
	admin, operator, _ := threeUsers(t, mgr)
	id := mgr.List()[0].ID

	if got := doAs(t, srv.URL, operator, "GET", "/api/servers", ""); got != 200 {
		t.Fatalf("operator could not read before revocation: %d", got)
	}
	c := dialConsoleAs(t, srv, id, operator)
	defer c.CloseNow()

	if err := mgr.auth.DeleteUser("opal"); err != nil {
		t.Fatalf("delete user: %v", err)
	}

	if got := doAs(t, srv.URL, operator, "GET", "/api/servers", ""); got == 200 {
		t.Error("a deleted user's cookie still reads the server list")
	}
	// The admin who did the deleting is unaffected.
	if got := doAs(t, srv.URL, admin, "GET", "/api/servers", ""); got != 200 {
		t.Errorf("the admin lost access after deleting somebody else: %d", got)
	}

	// The open socket must refuse a command rather than run it.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText,
		[]byte(`{"t":"command","id":"1","text":"say still here"}`)); err != nil {
		return // the server closed it outright, which is also correct
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, data, err := c.Read(ctx)
		if err != nil {
			return // closed - correct
		}
		s := string(data)
		if strings.Contains(s, "command_ack") {
			if strings.Contains(s, `"accepted":true`) {
				t.Fatal("a deleted user ran a console command on an open socket")
			}
			return
		}
	}
}

// Two people watching the same console both receive its output, and one leaving
// does not take the other's stream with them.
func TestTwoViewersShareOneConsole(t *testing.T) {
	srv, mgr := newAuthedTestAgent(t)
	defer srv.Close()
	admin, operator, _ := threeUsers(t, mgr)
	s := mgr.List()[0]

	a := dialConsoleAs(t, srv, s.ID, admin)
	defer a.CloseNow()
	b := dialConsoleAs(t, srv, s.ID, operator)

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	drainReplay := func(c *websocket.Conn) {
		for i := 0; i < 4; i++ {
			_, data, err := c.Read(ctx)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if strings.Contains(string(data), `"t":"replay"`) {
				return
			}
		}
	}
	drainReplay(a)
	drainReplay(b)

	// want is a parameter, not a closed-over literal: the second wait is for a
	// different line, and hardcoding the first one made this pass for the wrong
	// reason and then hang.
	seen := func(c *websocket.Conn, who, want string) {
		for i := 0; i < 12; i++ {
			_, data, err := c.Read(ctx)
			if err != nil {
				t.Fatalf("%s: read: %v", who, err)
			}
			if strings.Contains(string(data), want) {
				return
			}
		}
		t.Fatalf("%s never received %q", who, want)
	}

	const shared = "a line both of them must see"
	mgr.panelLine(s, "info", shared)
	seen(a, "first viewer", shared)
	seen(b, "second viewer", shared)

	// One leaves; the other must keep working. This is the case that matters
	// for a beta: two people on one console, one closes their laptop.
	b.CloseNow()
	time.Sleep(300 * time.Millisecond)
	if r, ok := mgr.hub.lookup(s.ID); ok {
		r.mu.Lock()
		n := len(r.conns)
		r.mu.Unlock()
		if n != 1 {
			t.Errorf("after one viewer left the room holds %d connections, want 1", n)
		}
	}
	const after = "a line after the other viewer left"
	mgr.panelLine(s, "info", after)
	seen(a, "the remaining viewer", after)
}

// The existing round-trip test in phases_test.go proves a backup comes back.
// What it does not check is that a restore *replaces* rather than merges: a
// file that was never in the archive surviving means the next restore inherits
// whatever the broken state left behind, which is the opposite of the point.
func TestRestoreReplacesRatherThanMerges(t *testing.T) {
	mgr := NewManager(t.TempDir(), NewHub())
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	s := mgr.List()[0]
	dir, err := mgr.ensureServerDir(s)
	if err != nil {
		t.Fatalf("server dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "world", "region"), 0o755); err != nil {
		t.Fatal(err)
	}
	original := []byte("chunk data that must survive the round trip")
	if err := os.WriteFile(filepath.Join(dir, "world", "region", "r.0.0.mca"), original, 0o644); err != nil {
		t.Fatal(err)
	}

	b, err := mgr.CreateBackup(s, "round trip", "test")
	if err != nil {
		t.Fatalf("backup: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "world", "stray.dat"), []byte("not in the backup"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := mgr.RestoreBackup(s, b.ID, "test"); err != nil {
		t.Fatalf("restore: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "world", "region", "r.0.0.mca"))
	if err != nil || string(got) != string(original) {
		t.Errorf("a nested chunk came back as %q (%v)", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "world", "stray.dat")); err == nil {
		t.Error("a file that was not in the backup survived the restore")
	}
	// The directory itself must survive: it is a symlink target for a server
	// adopted in place, and replacing it breaks the link.
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the server directory did not survive its own restore: %v", err)
	}
}

// A failed restore must leave the world exactly as it was. The archive is
// extracted into staging first for this reason; if that guarantee slips, a
// corrupt backup takes the live world with it.
func TestFailedRestoreLeavesTheWorldAlone(t *testing.T) {
	mgr := NewManager(t.TempDir(), NewHub())
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	s := mgr.List()[0]
	dir, err := mgr.ensureServerDir(s)
	if err != nil {
		t.Fatal(err)
	}
	live := []byte("the world as it stands")
	if err := os.WriteFile(filepath.Join(dir, "level.dat"), live, 0o644); err != nil {
		t.Fatal(err)
	}

	// An archive that is not a valid gzip stream at all.
	bad := filepath.Join(mgr.backupDir(s.ID), "20260101-000000-000-bad.tar.gz")
	if err := os.MkdirAll(filepath.Dir(bad), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("this is not an archive"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := mgr.RestoreBackup(s, "20260101-000000-000-bad", "test"); err == nil {
		t.Fatal("a corrupt archive restored successfully")
	}
	got, err := os.ReadFile(filepath.Join(dir, "level.dat"))
	if err != nil {
		t.Fatalf("the live world is gone after a failed restore: %v", err)
	}
	if string(got) != string(live) {
		t.Errorf("the live world was modified by a failed restore: %q", got)
	}
}

// A wrong ready marker must not strand a working server in "starting" forever.
// Five templates here have never been booted and an operator can add their own,
// so a ready_log that never matches is a permanent condition of the design.
func TestReadyWatchdogPromotesAServerThatIsActuallyUp(t *testing.T) {
	mgr := NewManager(t.TempDir(), NewHub())
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	s := mgr.List()[0]
	s.mu.Lock()
	s.Status = StatusStarting
	s.ReadyLog = "a line this server will never print"
	s.mu.Unlock()

	// The watchdog only promotes a container that is genuinely up; one that
	// died is the exit watcher's business.
	prevRunning := containerRunning
	prevFallback := readyFallbackFor
	containerRunning = func(string) bool { return true }
	readyFallbackFor = 40 * time.Millisecond
	defer func() { containerRunning = prevRunning; readyFallbackFor = prevFallback }()

	r := &dockerRunner{mgr: mgr}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.watchReady(ctx, s, "gamepanel-"+s.ID)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.State() == StatusRunning {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("a container that was up stayed in 'starting' past the grace period")
}

// And it must not promote one that has actually died.
func TestReadyWatchdogLeavesADeadContainerAlone(t *testing.T) {
	mgr := NewManager(t.TempDir(), NewHub())
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	s := mgr.List()[0]
	s.mu.Lock()
	s.Status = StatusStarting
	s.mu.Unlock()

	prevRunning := containerRunning
	prevFallback := readyFallbackFor
	containerRunning = func(string) bool { return false }
	readyFallbackFor = 40 * time.Millisecond
	defer func() { containerRunning = prevRunning; readyFallbackFor = prevFallback }()

	r := &dockerRunner{mgr: mgr}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.watchReady(ctx, s, "gamepanel-"+s.ID)

	if s.State() != StatusStarting {
		t.Errorf("a dead container was reported as %q", s.State())
	}
}
