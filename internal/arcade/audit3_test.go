package arcade

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// A backup that cannot be written must fail cleanly AND still resume world
// saves. A server left with save-off silently stops persisting anything the
// players do - far worse than a failed backup.
func TestFailedBackupStillResumesSaves(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := startServer(t, mgr)

	// Make the backup destination unwritable, the way a full or read-only
	// disk would.
	dir := mgr.backupDir(s.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skipf("cannot chmod in this environment: %v", err)
	}
	defer os.Chmod(dir, 0o755)

	_, err := mgr.CreateBackup(s, "should fail", "tester")
	if err == nil {
		t.Fatal("backup succeeded against an unwritable directory")
	}

	// The resume must have happened regardless of the failure.
	sawResume := false
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) && !sawResume {
		for _, l := range mgr.hub.Tail(s.ID, 60) {
			if strings.Contains(l.Text, "Automatic saving is now enabled") ||
				strings.Contains(l.Text, "saves resumed") {
				sawResume = true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !sawResume {
		t.Error("world saves were not resumed after a failed backup")
	}

	// And the lock must be released, or every future backup is refused.
	if mgr.backupLocked(s.ID) {
		t.Error("the backup lock survived the failure; the server can never be backed up again")
	}
}

// A write that cannot complete must leave the previous file intact rather than
// truncating it. server.properties is the file this matters most for.
func TestFailedWriteDoesNotDestroyTheOriginal(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	original, err := mgr.ReadFile(s, "server.properties")
	if err != nil || original == "" {
		t.Fatalf("no original to protect: %v", err)
	}

	// Point the write at a path that cannot be created.
	if err := mgr.WriteFile(s, "nope/../server.properties/child.txt", "x"); err == nil {
		t.Error("writing through a file as if it were a directory should fail")
	}

	after, err := mgr.ReadFile(s, "server.properties")
	if err != nil {
		t.Fatalf("original became unreadable: %v", err)
	}
	if after != original {
		t.Error("a failed write damaged server.properties")
	}
}

// Everything that accumulates must be bounded, or a panel left up for weeks
// grows without limit.
func TestLongRunningStructuresAreBounded(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	// console ring
	for i := 0; i < ringSize*4; i++ {
		mgr.hub.Publish(s.ID, Line{Text: "chatter"})
	}
	if n := len(mgr.hub.Tail(s.ID, 100000)); n > ringSize {
		t.Errorf("console ring holds %d lines, cap is %d", n, ringSize)
	}

	// metrics ring
	now := time.Now().Unix()
	for i := 0; i < samplesKept*2; i++ {
		mgr.metrics.push(s.ID, Sample{T: now + int64(i)})
	}
	if n := len(mgr.metrics.Series(s.ID, 0, 0)); n > samplesKept {
		t.Errorf("metrics ring holds %d samples, cap is %d", n, samplesKept)
	}

	// audit log
	for i := 0; i < auditKept+200; i++ {
		mgr.audit("tester", "noise", "x", "")
	}
	if n := len(mgr.auth.Audit(0)); n > auditKept {
		t.Errorf("audit holds %d entries, cap is %d", n, auditKept)
	}
}

// Heap must not grow without bound as console traffic flows through.
func TestConsoleTrafficDoesNotGrowTheHeap(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	hub := NewHub()

	settle := func() uint64 {
		for i := 0; i < 5; i++ {
			runtime.GC()
			time.Sleep(20 * time.Millisecond)
		}
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return ms.HeapAlloc
	}

	for i := 0; i < 20_000; i++ {
		hub.Publish("room", Line{Text: "warming up the ring"})
	}
	base := settle()

	for i := 0; i < 200_000; i++ {
		hub.Publish("room", Line{Text: "sustained console traffic on a busy server"})
	}
	after := settle()

	// The ring is fixed-size, so 10x the traffic must not move the heap much.
	if after > base*3 && after-base > 24<<20 {
		t.Errorf("heap grew %d KB -> %d KB across 200k lines; the ring should hold it flat",
			base>>10, after>>10)
	}
}

// State files must survive an interrupted write. Save() writes a temp file and
// renames, so a crash mid-write leaves the previous state readable.
func TestStateFileWriteIsAtomic(t *testing.T) {
	_, mgr := newTestAgent(t)

	if err := mgr.Save(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(mgr.storePath())
	if err != nil {
		t.Fatal(err)
	}

	// A stale temp file from a previous crash must not be mistaken for state.
	tmp := mgr.storePath() + ".tmp"
	if err := os.WriteFile(tmp, []byte("half-written garbage"), 0o644); err != nil {
		t.Fatal(err)
	}

	hub := NewHub()
	m2 := NewManager(mgr.dataDir, hub)
	if err := m2.Load(); err != nil {
		t.Fatalf("a stray .tmp file broke loading: %v", err)
	}
	if len(m2.List()) != len(mgr.List()) {
		t.Errorf("loaded %d servers, expected %d", len(m2.List()), len(mgr.List()))
	}

	after, _ := os.ReadFile(mgr.storePath())
	if string(before) != string(after) {
		t.Error("the real state file was modified by the presence of a temp file")
	}
	_ = filepath.Walk // keep the import honest
}

// A real deploy puts the panel behind a proxy on a different hostname. The
// Origin check added to stop cross-origin console access must not also break
// the console in that configuration - which is exactly the kind of hardening
// that gets discovered in production.
func TestConsoleWorksBehindAReverseProxy(t *testing.T) {
	srv, mgr := newTestAgent(t)
	defer srv.Close()
	id := mgr.List()[0].ID

	upstream, _ := url.Parse(srv.URL)
	proxy := httptest.NewServer(httputil.NewSingleHostReverseProxy(upstream))
	defer proxy.Close()

	dial := func(origin string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, _, err := websocket.Dial(ctx,
			"ws"+strings.TrimPrefix(proxy.URL, "http")+"/ws/console?server="+id,
			&websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{origin}}})
		if err == nil {
			c.CloseNow()
		}
		return err
	}

	// Same-origin through the proxy: the browser sends the proxy's hostname as
	// Origin, and the proxy forwards its own Host. This must work.
	if err := dial(proxy.URL); err != nil {
		t.Fatalf("the console is unreachable behind a proxy: %v", err)
	}

	// A genuinely foreign origin must still be refused through the proxy.
	if err := dial("https://evil.example"); err == nil {
		t.Error("a cross-origin socket was accepted through the proxy")
	}
}

// -origin must widen the allow-list for a deploy where the proxy presents a
// different hostname than the panel's own Host header.
func TestOriginFlagAllowsAConfiguredHostname(t *testing.T) {
	old := allowedOrigins
	defer func() { allowedOrigins = old }()
	allowedOrigins = []string{"arcade.example.com"}

	srv, mgr := newTestAgent(t)
	defer srv.Close()
	id := mgr.List()[0].ID

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx,
		"ws"+strings.TrimPrefix(srv.URL, "http")+"/ws/console?server="+id,
		&websocket.DialOptions{HTTPHeader: http.Header{
			"Origin": []string{"https://arcade.example.com"}}})
	if err != nil {
		t.Fatalf("-origin did not admit the configured hostname: %v", err)
	}
	c.CloseNow()
}

// When the panel runs in a container, bind mounts for sibling game containers
// must use host paths. Getting this wrong does not error - Docker creates an
// empty directory and the game boots a blank world - so it is worth a test.
func TestHostPathMappingForSiblingContainers(t *testing.T) {
	oldDir, oldHost := dataDirPath, dataHostPath
	defer func() { dataDirPath, dataHostPath = oldDir, oldHost }()

	// not containerised: paths pass through untouched
	dataDirPath, dataHostPath = "/app/data", ""
	if got := hostPathFor("/app/data/servers/s1"); got != "/app/data/servers/s1" {
		t.Errorf("unmapped path was rewritten: %s", got)
	}

	// containerised: in-container path maps onto the host path
	dataDirPath, dataHostPath = "/app/data", "/var/teploy-arcade"
	if got := hostPathFor("/app/data/servers/s1"); got != "/var/teploy-arcade/servers/s1" {
		t.Errorf("hostPathFor = %q, want /var/teploy-arcade/servers/s1", got)
	}

	// identical paths (the recommended deploy) are a no-op
	dataDirPath, dataHostPath = "/var/teploy-arcade", "/var/teploy-arcade"
	if got := hostPathFor("/var/teploy-arcade/servers/s1"); got != "/var/teploy-arcade/servers/s1" {
		t.Errorf("identical-path mapping altered the path: %s", got)
	}

	// a path outside the data dir is never rewritten
	dataDirPath, dataHostPath = "/app/data", "/var/teploy-arcade"
	if got := hostPathFor("/etc/passwd"); got != "/etc/passwd" {
		t.Errorf("unrelated path rewritten: %s", got)
	}
}
