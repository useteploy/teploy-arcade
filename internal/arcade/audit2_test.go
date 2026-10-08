package arcade

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Corrupt authoritative state must remain evidence and require explicit repair.
func TestCorruptStateFilesRequireExplicitRecovery(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"servers.json", "tasks.json", "users.json", "mcp-tokens.json"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("{{{ not json"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	mgr := NewManager(dir, NewHub())
	defer mgr.Close()
	if err := mgr.auth.Load(); err == nil {
		t.Fatal("corrupt credentials silently enabled fresh setup")
	}
	if err := mgr.Load(); err == nil {
		t.Fatal("corrupt server state silently created an empty/demo registry")
	}
	for _, f := range []string{"servers.json", "users.json"} {
		if b, err := os.ReadFile(filepath.Join(dir, f)); err != nil || string(b) != "{{{ not json" {
			t.Fatal("corrupt source was overwritten or removed")
		}
	}
}

// A panic in a runner or a scheduled task must not take down the panel. If it
// does, one bad task means nobody can reach any server.
func TestPanicInBackgroundWorkIsContained(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer recoverPanic("test worker")
		panic("boom")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never returned")
	}
	// Reaching here at all means the panic was contained rather than fatal.
}

// Request bodies must be bounded. Without a cap, one POST can exhaust memory.
func TestRequestBodiesAreBounded(t *testing.T) {
	srv, _ := newTestAgent(t)
	defer srv.Close()

	huge := bytes.Repeat([]byte("a"), 12<<20) // 12 MB
	body, _ := json.Marshal(map[string]string{"name": string(huge)})

	res, err := http.Post(srv.URL+"/api/servers", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode == 201 {
		t.Fatal("a 12 MB body was accepted and created a server")
	}
	if res.StatusCode >= 500 {
		t.Errorf("oversized body produced %d; it should be a clean 4xx", res.StatusCode)
	}
}

// A single absurd console line must not be buffered and fanned out whole. The
// ring holds 500 lines, so unbounded lines mean an unbounded ring.
func TestConsoleLinesAreTruncated(t *testing.T) {
	hub := NewHub()
	hub.Publish("room", Line{Text: strings.Repeat("x", 200_000)})

	got := hub.Tail("room", 1)
	if len(got) != 1 {
		t.Fatal("line not buffered")
	}
	if len(got[0].Text) > maxLineBytes+64 {
		t.Errorf("line stored at %d bytes; it should be truncated near %d",
			len(got[0].Text), maxLineBytes)
	}
	if !strings.Contains(got[0].Text, "truncated") {
		t.Error("a truncated line should say so rather than silently losing content")
	}
}

// Expired sessions must be reaped, not merely ignored on lookup, or the map
// grows for the life of the process.
func TestExpiredSessionsAreReaped(t *testing.T) {
	_, mgr := newTestAgent(t)
	a := mgr.auth

	if _, err := a.CreateUser("admin", "correct-horse", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if _, err := a.Login("admin", "correct-horse"); err != nil {
			t.Fatal(err)
		}
	}

	a.mu.Lock()
	for _, s := range a.sessions {
		s.Expires = time.Now().Add(-time.Hour) // pretend they aged out
	}
	n := len(a.sessions)
	a.mu.Unlock()
	if n != 20 {
		t.Fatalf("expected 20 sessions, got %d", n)
	}

	a.reapSessions()

	a.mu.RLock()
	left := len(a.sessions)
	a.mu.RUnlock()
	if left != 0 {
		t.Errorf("%d expired sessions survived the reaper", left)
	}
}

// A container that outlives the panel must be re-adopted, and its exit must
// still be noticed afterwards. Uses a trivial container rather than a real
// Minecraft image so the test runs in seconds.
func TestAdoptedContainerExitIsNoticed(t *testing.T) {
	if os.Getenv("TEPLOY_ARCADE_LIVE_ACCEPTANCE") != "1" {
		t.Skip("explicit live Docker acceptance required")
	}
	image := os.Getenv("TEPLOY_ARCADE_FIXTURE_IMAGE")
	if image == "" {
		t.Fatal("TEPLOY_ARCADE_FIXTURE_IMAGE must identify a preloaded image")
	}
	if !dockerAvailable() {
		t.Fatal("assigned Docker daemon not available")
	}
	if err := exec.Command("docker", "image", "inspect", image).Run(); err != nil {
		t.Fatalf("preloaded fixture image required: %v", err)
	}
	_, mgr := newTestAgent(t)
	defer mgr.Close()
	s := mgr.List()[0]
	s.Runtime = RuntimeDocker

	name := containerPrefix + "-" + s.ID
	// Never delete a pre-existing workload: run must claim its name exclusively.
	if err := exec.Command("docker", "run", "--pull=never", "-d", "--name", name,
		image, "sh", "-c", "sleep 300").Run(); err != nil {
		t.Fatalf("could not start own probe container: %v", err)
	}
	defer exec.Command("docker", "rm", "-f", name).Run()

	if !containerRunning(s.ID) {
		t.Fatal("probe container is not reported as running")
	}

	dr := mgr.docker.(*dockerRunner)
	if err := dr.Adopt(s, func(l Line) {}); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if s.State() != StatusRunning {
		t.Fatalf("adopted server status = %q, want running", s.State())
	}

	// Start must refuse rather than bulldoze the live container.
	if err := mgr.Start(s.ID); err == nil {
		t.Error("Start was allowed against a live adopted container")
	}

	// And when the container really goes away, the panel must notice.
	_ = exec.Command("docker", "kill", name).Run()
	waitFor(t, 20*time.Second, func() bool {
		st := s.State()
		return st == StatusStopped || st == StatusFailed
	})
}
