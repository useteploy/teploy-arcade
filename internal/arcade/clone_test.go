package arcade

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// waitForJob polls an import/clone job to completion.
func waitForJob(t *testing.T, id string) ImportJob {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		j, ok := importJobByID(id)
		if !ok {
			t.Fatalf("job %s disappeared", id)
		}
		v := j.view()
		if v.State != "running" {
			return v
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("job %s never finished", id)
	return ImportJob{}
}

func cloneFixture(t *testing.T) (*Manager, *Server) {
	t.Helper()
	mgr := NewManager(t.TempDir(), NewHub())
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	src := mgr.List()[0]
	dir, err := mgr.ensureServerDir(src)
	if err != nil {
		t.Fatalf("server dir: %v", err)
	}
	// A world, and the things a clone must leave behind.
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("world/level.dat", "LEVELDATA")
	write("world/region/r.0.0.mca", "REGION")
	write("world/session.lock", "\x00\x00")
	write("plugins/EssentialsX.jar", "PK\x03\x04")
	write("logs/latest.log", "log line")
	write("crash-reports/crash-2026.txt", "boom")
	write("crafty_managed.txt", "1")
	write(".rcon-cli.env", "RCON_PASSWORD=hunter2")
	return mgr, src
}

func TestCloneCopiesTheWorldButNotTheIdentity(t *testing.T) {
	mgr, src := cloneFixture(t)

	job, err := mgr.StartClone(CloneRequest{Source: src.ID}, "tester")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	done := waitForJob(t, job.ID)
	if done.State != "done" {
		t.Fatalf("clone did not finish: %s %s", done.State, done.Error)
	}

	clone := mgr.Get(done.ServerID)
	if clone == nil {
		t.Fatal("the clone is not registered with the manager")
	}
	dir, err := mgr.ensureServerDir(clone)
	if err != nil {
		t.Fatalf("clone dir: %v", err)
	}

	for _, rel := range []string{"world/level.dat", "world/region/r.0.0.mca", "plugins/EssentialsX.jar"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("%s did not come across: %v", rel, err)
		}
	}
	// Each of these breaks something specific if copied: a lock file describing
	// another process, another server's logs, an RCON password the clone did
	// not generate, and a marker claiming a different panel owns this tree.
	for _, rel := range []string{"world/session.lock", "logs/latest.log", "crash-reports/crash-2026.txt", "crafty_managed.txt", ".rcon-cli.env"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err == nil {
			t.Errorf("%s was copied into the clone", rel)
		}
	}

	if clone.Port == src.Port {
		t.Errorf("the clone kept the source's port %d; both would fail to bind", clone.Port)
	}
	if clone.Props["server-port"] != itoa(clone.Port) {
		t.Errorf("server.properties says port %q, the panel says %d",
			clone.Props["server-port"], clone.Port)
	}
	if clone.Name != src.Name+" copy" {
		t.Errorf("default name is %q", clone.Name)
	}
	if clone.Template != src.Template || clone.Version != src.Version || clone.Image != src.Image {
		t.Errorf("the clone did not inherit the source's runtime identity: %s/%s/%s",
			clone.Template, clone.Version, clone.Image)
	}
}

// The panel's own written props must match the file on disk, or the game binds
// the source's port and the panel reports the new one.
func TestCloneRewritesTheCopiedServerProperties(t *testing.T) {
	mgr, src := cloneFixture(t)

	job, err := mgr.StartClone(CloneRequest{Source: src.ID, Name: "Second"}, "tester")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	done := waitForJob(t, job.ID)
	if done.State != "done" {
		t.Fatalf("clone did not finish: %s %s", done.State, done.Error)
	}
	clone := mgr.Get(done.ServerID)
	dir, _ := mgr.ensureServerDir(clone)

	b, err := os.ReadFile(filepath.Join(dir, "server.properties"))
	if err != nil {
		t.Fatalf("read props: %v", err)
	}
	props := parseProperties(string(b))
	if props["server-port"] != itoa(clone.Port) {
		t.Errorf("server.properties on disk says %q, the panel says %d",
			props["server-port"], clone.Port)
	}
	if props["motd"] != "Second" {
		t.Errorf("motd is %q; the clone would advertise itself as the source", props["motd"])
	}
}

func TestCloneRefusesAPortAnotherServerHolds(t *testing.T) {
	mgr, src := cloneFixture(t)
	other := mgr.List()[1]

	_, err := mgr.StartClone(CloneRequest{Source: src.ID, Port: other.Port}, "tester")
	if err == nil {
		t.Fatal("a clone was allowed onto a port another server holds")
	}
}

func TestCloneRefusesAnUnknownSource(t *testing.T) {
	mgr, _ := cloneFixture(t)
	if _, err := mgr.StartClone(CloneRequest{Source: "nope"}, "tester"); err == nil {
		t.Fatal("cloning a server that does not exist was allowed")
	}
}

// portReleasedOnceFailed polls until the port a failed clone claimed is free
// again. releasePort runs before job.fail, but the deferred unlockBackup races
// the poll by design, so both are waited on rather than assumed.
func portReleasedOnceFailed(t *testing.T, mgr *Manager, port int) {
	t.Helper()
	waitFor(t, 3*time.Second, func() bool {
		mgr.mu.Lock()
		_, held := mgr.reservedPorts[port]
		mgr.mu.Unlock()
		return !held
	})
}

// A clone whose server.properties cannot be written must not be exposed. The
// copied file still carries the source's port, so a "successful" job here
// hands the operator a panel and a game that disagree about where to connect.
func TestCloneFailsWhenServerPropertiesCannotBeWritten(t *testing.T) {
	mgr, src := cloneFixture(t)
	// The copy lands a directory where server.properties must go, so the
	// panel's atomic write fails the way an unwritable disk would.
	dir, err := mgr.ensureServerDir(src)
	if err != nil {
		t.Fatalf("server dir: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "server.properties")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "server.properties"), 0o755); err != nil {
		t.Fatal(err)
	}

	before := len(mgr.List())
	job, err := mgr.StartClone(CloneRequest{Source: src.ID, Port: 25599}, "tester")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	done := waitForJob(t, job.ID)
	if done.State != "failed" {
		t.Fatalf("a clone with unwritable properties reported %q", done.State)
	}
	if !strings.Contains(done.Error, "server.properties") {
		t.Errorf("the failure does not name the file: %s", done.Error)
	}
	if len(mgr.List()) != before {
		t.Error("a clone whose properties could not be written was registered")
	}
	portReleasedOnceFailed(t, mgr, 25599)
}

// A clone the panel cannot persist exists only in memory; reporting it as
// done would be a success that vanishes on the next restart.
func TestCloneFailsWhenTheServerListCannotBeSaved(t *testing.T) {
	mgr, src := cloneFixture(t)
	// servers.json lives in the data dir; making that unwritable is what a
	// full or read-only system disk looks like to Save().
	if err := os.Chmod(mgr.dataDir, 0o500); err != nil {
		t.Skipf("cannot chmod in this environment: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(mgr.dataDir, 0o755) })

	before := len(mgr.List())
	job, err := mgr.StartClone(CloneRequest{Source: src.ID, Port: 25599}, "tester")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	done := waitForJob(t, job.ID)
	if done.State != "failed" {
		t.Fatalf("a clone Save could not record reported %q", done.State)
	}
	if !strings.Contains(done.Error, "server list") {
		t.Errorf("the failure does not name the server list: %s", done.Error)
	}
	if len(mgr.List()) != before {
		t.Error("a clone the panel cannot persist was left registered")
	}
	portReleasedOnceFailed(t, mgr, 25599)
}
