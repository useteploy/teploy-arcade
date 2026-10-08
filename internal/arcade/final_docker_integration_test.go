//go:build integration

package arcade

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Opt-in, disposable container only. Never remove an existing container or
// pull/build an image implicitly on a shared operator daemon.
func TestFinalDockerWorldSafetyLifecycle(t *testing.T) {
	if os.Getenv("TEPLOY_ARCADE_LIVE_ACCEPTANCE") != "1" {
		t.Skip("set TEPLOY_ARCADE_LIVE_ACCEPTANCE=1 on an assigned healthy daemon")
	}
	image := os.Getenv("TEPLOY_ARCADE_FIXTURE_IMAGE")
	if image == "" {
		image = "alpine:3.24"
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := dockerOutput(ctx, 10*time.Second, false, "image", "inspect", image); err != nil {
		t.Fatalf("preloaded fixture image required: %v", err)
	}
	server, m := newTestAgent(t)
	defer server.Close()
	defer m.Close()
	s := m.List()[0]
	s.Game = "fixture"
	s.Image = image
	dir := m.serverDir(s)
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	world := filepath.Join(dir, "world", "level.dat")
	if err := os.MkdirAll(filepath.Dir(world), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(world, []byte("original-generation"), 0600); err != nil {
		t.Fatal(err)
	}
	backup, err := m.CreateBackup(s, "disposablefixture", "test")
	if err != nil {
		t.Fatal(err)
	}
	s.Runtime = RuntimeDocker
	name := containerPrefix + "-" + s.ID
	if state := containerState(s.ID); state != ContainerMissing {
		t.Fatalf("fixture name must be authoritatively absent, got %v", state)
	}
	if _, err := dockerOutput(ctx, 30*time.Second, true, "run", "-d", "--name", name, "--label", "io.teploy.arcade.server="+s.ID, "-v", dir+":/data", image, "sh", "-c", "sleep 300"); err != nil {
		t.Fatalf("create disposable container: %v", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := dockerOutput(cleanup, 20*time.Second, true, "rm", "-f", name); err != nil {
			t.Errorf("own fixture cleanup failed: %v", err)
		}
	}()
	if state := containerState(s.ID); state != ContainerRunning {
		t.Fatalf("running fixture observation %v", state)
	}
	if _, err := m.CreateBackup(s, "must-refuse", "test"); err == nil {
		t.Fatal("unobserved panel stopped state allowed active-world backup")
	}
	if err := m.RestoreBackup(s, backup.ID, "test"); err == nil {
		t.Fatal("active-world restore accepted")
	}
	if bytes, err := os.ReadFile(world); err != nil || string(bytes) != "original-generation" {
		t.Fatal("refused operation changed generation")
	}
	if err := m.recoverContainerIdentity(s); err != nil {
		t.Fatalf("owned exact mount adoption identity: %v", err)
	}
	if _, err := dockerOutput(ctx, 20*time.Second, true, "stop", "-t", "1", name); err != nil {
		t.Fatal(err)
	}
	if state := containerState(s.ID); state != ContainerStopped {
		t.Fatalf("stopped fixture observation %v", state)
	}
	if _, err := m.CreateBackup(s, "confirmed-stopped", "test"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(world, []byte("later-generation"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.RestoreBackup(s, backup.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if bytes, err := os.ReadFile(world); err != nil || string(bytes) != "original-generation" {
		t.Fatal("confirmed stopped restore failed")
	}
}
