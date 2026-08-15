package arcade

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Backups were the one path that wrote gigabytes with nothing checking there
// was room. Worlds and backups share a filesystem by design, so filling it
// takes down every running server on the host.
func TestBackupRefusedWhenDiskIsFull(t *testing.T) {
	mgr := NewManager(t.TempDir(), NewHub())
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	s := mgr.List()[0]
	if _, err := mgr.ensureServerDir(s); err != nil {
		t.Fatalf("server dir: %v", err)
	}
	// measureTree only counts regular files, so the tree has to contain one.
	if err := os.WriteFile(filepath.Join(mgr.serverDir(s), "world.dat"), make([]byte, 4096), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	restore := diskFree
	diskFree = func(string) (int64, error) { return 1 << 10, nil }
	defer func() { diskFree = restore }()

	_, err := mgr.CreateBackup(s, "", "test")
	if err == nil {
		t.Fatal("a backup was taken on a disk with 1 KB free")
	}
	if !strings.Contains(err.Error(), "free") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
}

// Retention is opt-in and applies only after a new archive has landed. A panel
// upgrade must not delete anybody's backups, and a retention pass that runs
// when the backup job is broken deletes the last copy of a world.
func TestRetentionKeepsNewestAndDefaultsToEverything(t *testing.T) {
	mgr := NewManager(t.TempDir(), NewHub())
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	s := mgr.List()[0]
	dir := mgr.backupDir(s.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, id := range []string{"20260101-000001-000-x", "20260101-000002-000-x", "20260101-000003-000-x"} {
		if err := os.WriteFile(filepath.Join(dir, id+".tar.gz"), []byte("x"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// Default: keep everything.
	mgr.pruneBackups(s, "test")
	if list, _ := mgr.ListBackups(s); len(list) != 3 {
		t.Fatalf("default retention deleted backups: %d left of 3", len(list))
	}

	if err := mgr.SetBackupKeep(s, 2, "test"); err != nil {
		t.Fatalf("set retention: %v", err)
	}
	// Setting it does not act on its own - lowering retention is a statement
	// about future backups, not an instruction to delete eight archives now.
	if list, _ := mgr.ListBackups(s); len(list) != 3 {
		t.Fatalf("setting retention deleted %d backup(s) immediately", 3-len(list))
	}

	mgr.pruneBackups(s, "test")
	list, _ := mgr.ListBackups(s)
	if len(list) != 2 {
		t.Fatalf("retention kept %d backups, want 2", len(list))
	}
	// Newest first, so the oldest is what should have gone.
	for _, b := range list {
		if b.ID == "20260101-000001-000-x" {
			t.Error("retention kept the oldest archive and dropped a newer one")
		}
	}
}

// Settings could hand one server more memory than the host has, and the only
// thing that objected was the OOM killer at the next restart.
func TestResourceChangeRefusedBeyondHost(t *testing.T) {
	mgr := NewManager(t.TempDir(), NewHub())
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	s := mgr.List()[0]

	prev := hostMemMB
	hostMemMB = 8192
	defer func() { hostMemMB = prev }()

	if _, err := mgr.SetResources(s, 65536, 0); err == nil {
		t.Fatal("a server was given 64 GB on an 8 GB host")
	}
	if _, err := mgr.SetResources(s, 4096, 0); err != nil {
		t.Fatalf("a limit that fits was refused: %v", err)
	}
}

// A server's ID names its container, its directory and its backups. The old
// generator restarted its counter with the panel, so the same ID came back
// around - and nothing checked against the servers that already existed.
func TestNewIDNeverCollides(t *testing.T) {
	mgr := NewManager(t.TempDir(), NewHub())
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	seen := map[string]bool{}
	for _, s := range mgr.List() {
		seen[s.ID] = true
	}
	for i := 0; i < 500; i++ {
		id := mgr.newID()
		if seen[id] {
			t.Fatalf("newID handed out %q twice", id)
		}
		seen[id] = true
		// Register it, so the next call has to route around it the way a real
		// create would.
		mgr.mu.Lock()
		mgr.servers[id] = &Server{ID: id}
		mgr.mu.Unlock()
	}
}

// Deleting a server left its tasks in tasks.json, firing on their schedule at
// a server that no longer exists.
func TestDeletingAServerTakesItsTasks(t *testing.T) {
	mgr := NewManager(t.TempDir(), NewHub())
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	s := mgr.List()[0]
	other := mgr.List()[1]

	for _, target := range []*Server{s, other} {
		if _, err := mgr.sched.Add(&Task{
			ServerID: target.ID, Name: "nightly", Time: "04:00", Commands: "backup",
		}); err != nil {
			t.Fatalf("add task: %v", err)
		}
	}

	if err := mgr.Delete(s.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := mgr.sched.List(s.ID); len(got) != 0 {
		t.Errorf("%d task(s) survived the server they belong to", len(got))
	}
	if got := mgr.sched.List(other.ID); len(got) != 1 {
		t.Errorf("another server's task was dropped: %d left", len(got))
	}
}

// Publishing was one hardcoded TCP port, so every UDP game installed, booted,
// reported healthy and could not be reached by anybody.
func TestUDPGamesPublishUDP(t *testing.T) {
	join := func(s *Server) string { return strings.Join(publishArgs(s), " ") }

	java := &Server{Port: 25565}
	if got := join(java); got != "-p 25565:25565/tcp" {
		t.Errorf("java publish is %q", got)
	}

	bedrock := &Server{Port: 19132, Protocols: []string{"udp"}}
	if got := join(bedrock); got != "-p 19132:19132/udp" {
		t.Errorf("bedrock publish is %q", got)
	}

	valheim := &Server{Port: 2456, Protocols: []string{"udp"}, PortSpan: 3}
	want := "-p 2456:2456/udp -p 2457:2457/udp -p 2458:2458/udp"
	if got := join(valheim); got != want {
		t.Errorf("valheim publish is %q, want %q", got, want)
	}

	// A template asking for a thousand ports is a bad template, not a thousand
	// bindings on the daemon.
	wild := &Server{Port: 100, Protocols: []string{"udp"}, PortSpan: 9999}
	if n := len(publishArgs(wild)) / 2; n > 16 {
		t.Errorf("an unbounded span published %d ports", n)
	}
}

// Every non-Java game was judged ready by a banner only Java prints, so it
// stayed "starting" for its whole uptime.
func TestReadyDetectionPerGame(t *testing.T) {
	java := &Server{Game: "minecraft-java"}
	if !isReady(java, `[12:00:00 INFO]: Done (21.3s)! For help, type "help"`) {
		t.Error("the Java ready banner is no longer recognised")
	}
	proxy := &Server{Game: "proxy"}
	if !isReady(proxy, `[12:00:00 INFO]: Done (1.43s)!`) {
		t.Error("the proxy ready banner is no longer recognised")
	}
	if isReady(java, `[12:00:00 INFO]: Done (1.43s)!`) {
		t.Error("a proxy banner marked a Java server ready")
	}
	bedrock := &Server{Game: "minecraft-bedrock", ReadyLog: "Server started."}
	if !isReady(bedrock, `[2026-08-15 03:00:00 INFO] Server started.`) {
		t.Error("a template's own ready line was ignored")
	}
	if isReady(bedrock, `[2026-08-15 03:00:00 INFO] Starting Server`) {
		t.Error("a starting line was read as ready")
	}
}

// rcon-cli does not exist in the Bedrock image, and Bedrock speaks no RCON at
// all, so every console command sent to one failed with "not found in $PATH".
func TestConsoleToolPerImage(t *testing.T) {
	if got := consoleTool("itzg/minecraft-server:java21"); got != "rcon-cli" {
		t.Errorf("java console tool is %q", got)
	}
	if got := consoleTool("itzg/minecraft-bedrock-server"); got != "send-command" {
		t.Errorf("bedrock console tool is %q", got)
	}
	// send-command writes to stdin and returns nothing, so nothing may ask it a
	// question and believe the silence.
	if hasRCON("itzg/minecraft-bedrock-server") {
		t.Error("the panel thinks it can query a Bedrock server for its player list")
	}
}

// Create checked ports by walking the registered servers and did not register
// its own until several steps later, so two creates could both pass - and an
// import already holding a reservation was invisible to both.
func TestCreateClaimsItsPort(t *testing.T) {
	mgr := NewManager(t.TempDir(), NewHub())
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}

	held := 25999
	if _, ok := mgr.claimPort(held, "an import"); !ok {
		t.Fatal("could not take a reservation to test against")
	}
	if _, err := mgr.Create("clash", "vanilla", "1.20.4", held, 0, 0, RuntimeSim); err == nil {
		t.Error("create took a port an import was already holding")
	}
	// And it must not be suggested either.
	if p := mgr.NextFreePort(held); p == held {
		t.Error("NextFreePort suggested a reserved port")
	}
	mgr.releasePort(held)

	s, err := mgr.Create("fine", "vanilla", "1.20.4", held, 0, 0, RuntimeSim)
	if err != nil {
		t.Fatalf("create on a free port: %v", err)
	}
	// The reservation must be handed over to the server, not leaked - otherwise
	// the port is unusable for the life of the process.
	mgr.mu.RLock()
	_, stillReserved := mgr.reservedPorts[held]
	mgr.mu.RUnlock()
	if stillReserved {
		t.Error("create leaked its port reservation")
	}
	if s.Port != held {
		t.Errorf("server landed on port %d, want %d", s.Port, held)
	}
}
