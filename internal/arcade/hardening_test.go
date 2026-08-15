package arcade

import (
	"archive/zip"
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
	join := func(s *Server) string { return strings.Join(publishArgs(s, ""), " ") }

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
	if n := len(publishArgs(wild, "")) / 2; n > 16 {
		t.Errorf("an unbounded span published %d ports", n)
	}

	// Geyser listens on UDP 19132 regardless of the proxy's own port, so a span
	// cannot express it. It ran on the deployed proxy for the whole migration
	// with nothing published for it.
	proxy := &Server{Port: 25565, ExtraPorts: []string{"19132/udp"}}
	got := join(proxy)
	if !strings.Contains(got, "-p 25565:25565/tcp") || !strings.Contains(got, "-p 19132:19132/udp") {
		t.Errorf("proxy publish is %q", got)
	}
	// A protocol-less entry is tcp, matching docker's own -p.
	if got := join(&Server{Port: 1, ExtraPorts: []string{"8080"}}); !strings.Contains(got, "-p 8080:8080/tcp") {
		t.Errorf("a bare extra port is %q", got)
	}
	// A typo in a template must not be the reason a working server will not
	// start, and must not duplicate a binding either.
	messy := &Server{Port: 25565, ExtraPorts: []string{"", "notaport", "70000/udp", "19132/sctp", "25565/tcp", "19132/udp", "19132/udp"}}
	got = join(messy)
	if strings.Count(got, "-p ") != 2 {
		t.Errorf("bad and duplicate entries were not handled: %q", got)
	}
}

// Geyser is a plugin, not a server, and it can sit on a proxy or on a Paper
// server. Its port therefore belongs to whatever has it installed - putting
// 19132 in the Velocity template would have every proxy claim a Bedrock port
// whether or not it runs Geyser, and made a second proxy fail over a plugin it
// does not have.
func TestGeyserPortFollowsThePlugin(t *testing.T) {
	dir := t.TempDir()
	s := &Server{Port: 25565, Name: "Proxy"}

	// No plugin, no port. A proxy without Geyser publishes what a proxy needs.
	if got := strings.Join(publishArgs(s, dir), " "); got != "-p 25565:25565/tcp" {
		t.Fatalf("a proxy with no Geyser published %q", got)
	}

	// Installed but not yet configured - the normal state on a first boot,
	// before Geyser has written a config.
	plugins := filepath.Join(dir, "plugins")
	if err := os.MkdirAll(plugins, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugins, "Geyser-Velocity.jar"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(publishArgs(s, dir), " "); !strings.Contains(got, "-p 19132:19132/udp") {
		t.Errorf("an installed Geyser got no port: %q", got)
	}

	// Configured on a non-default port. Publishing 19132 while Geyser listens
	// elsewhere reproduces the original bug with more steps.
	cfgDir := filepath.Join(plugins, "Geyser-Velocity")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "# comment\nbedrock:\n  address: 0.0.0.0\n  port: 19140\n  clone-remote-port: false\nremote:\n  port: 25565\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(publishArgs(s, dir), " ")
	if !strings.Contains(got, "-p 19140:19140/udp") {
		t.Errorf("the configured Bedrock port was not read: %q", got)
	}
	// `remote.port` is the Java server Geyser talks to, not a port to publish.
	if strings.Contains(got, "-p 19132:") {
		t.Errorf("published the default alongside the configured port: %q", got)
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

	// Copied from a real Terraria boot on the deployed host. Everything before
	// the listen line is world generation, which on one core runs for minutes -
	// exactly the window in which a wrong marker would have the panel claim a
	// server is up while it is still making terrain.
	terraria := &Server{Game: "terraria", ReadyLog: templateBySlug("terraria").ReadyLog}
	for _, notYet := range []string{
		"Creating world - Seed: 687713729, Width: 6400, Height: 1800",
		"Generating jungle",
		"Settling liquids",
	} {
		if isReady(terraria, notYet) {
			t.Errorf("world generation was read as ready: %q", notYet)
		}
	}
	if !isReady(terraria, "Listening on port 7777") {
		t.Error("the line Terraria prints when it starts accepting players was not recognised")
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

// Version detection reads version.json out of the jar, and always has - but it
// ran at import time and only at import time. Four Paper servers imported
// before that reader existed kept the "unknown" they were written with, and
// read "paper unknown" in their own header while the answer sat in a jar on
// disk.
func TestVersionBackfillFillsOnlyBlanks(t *testing.T) {
	mgr := NewManager(t.TempDir(), NewHub())
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}

	stale := mgr.List()[0]
	kept := mgr.List()[1]
	for _, s := range []*Server{stale, kept} {
		if _, err := mgr.ensureServerDir(s); err != nil {
			t.Fatalf("server dir: %v", err)
		}
	}

	writeJar(t, filepath.Join(mgr.serverDir(stale), "paper.jar"),
		map[string]string{"version.json": `{"id":"26.1.2","name":"26.1.2"}`})
	writeJar(t, filepath.Join(mgr.serverDir(kept), "paper.jar"),
		map[string]string{"version.json": `{"id":"26.1.2"}`})

	stale.Version, stale.LaunchJar = "unknown", "paper.jar"
	kept.Version, kept.LaunchJar = "1.20.4", "paper.jar"

	mgr.backfillVersions()

	if stale.Version != "26.1.2" {
		t.Errorf("an unknown version was not filled in: %q", stale.Version)
	}
	// A recorded version is the operator's or the importer's answer, and a jar
	// that may since have been swapped does not get to overrule it.
	if kept.Version != "1.20.4" {
		t.Errorf("a known version was overwritten with %q", kept.Version)
	}
}

// A proxy is not a Minecraft build and ships no version.json, so the manifest
// is the honest source - and the proxy is where knowing the build matters, since
// a plugin refusing to load against a newer Velocity is a failure this fleet has
// already had.
func TestProxyVersionComesFromTheManifest(t *testing.T) {
	dir := t.TempDir()
	jar := filepath.Join(dir, "velocity.jar")
	writeJar(t, jar, map[string]string{
		"META-INF/MANIFEST.MF": "Manifest-Version: 1.0\r\n" +
			"Implementation-Version: 3.5.0-SNAPSHOT (git-a7581821-b605)\r\n" +
			"Implementation-Title: Velocity\r\n",
	})
	if v := versionFromJar(jar); v != "" {
		t.Errorf("a proxy jar reported a Minecraft version of %q", v)
	}
	// The git build identifier is not part of the version and does not belong
	// in a header.
	if v := jarImplVersion(jar); v != "3.5.0-SNAPSHOT" {
		t.Errorf("proxy version is %q, want 3.5.0-SNAPSHOT", v)
	}
	// A jar with neither is not an error, just no answer.
	plain := filepath.Join(dir, "plain.jar")
	writeJar(t, plain, map[string]string{"a.txt": "hello"})
	if v := jarImplVersion(plain); v != "" {
		t.Errorf("a jar with no manifest reported %q", v)
	}
}

func writeJar(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create jar: %v", err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip entry: %v", err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
}

// The templates directory was seeded once, when empty, and never looked at
// again - so every template fix shipped in an upgrade was inert on any panel
// that had already been installed. v0.18.0's Bedrock, Rust and Valheim fixes
// all landed in a build that could not reach the deployed panel's own copies.
func TestSeededTemplatesRefreshButNeverClobberAnEdit(t *testing.T) {
	dir := t.TempDir()

	// First run seeds and records what it wrote.
	if err := LoadTemplates(dir); err != nil {
		t.Fatalf("first load: %v", err)
	}
	tpl := filepath.Join(dir, "templates", "bedrock.json")
	if _, err := os.Stat(filepath.Join(dir, "templates", seedLedger)); err != nil {
		t.Fatalf("no seed ledger written: %v", err)
	}

	// A stale seed - what the deployed panel actually had on disk: the dead
	// pinned version, no UDP, no ready banner.
	stale := `{"slug":"bedrock","name":"Bedrock","game":"minecraft-bedrock","group":"Other",
	  "mark":"bedrock","description":"x","maturity":"preview",
	  "image":"itzg/minecraft-bedrock-server","versions":["1.20.71"],
	  "memory_mb":2048,"cpu":2,"disk_gb":10,"max_players":10,"port_hint":19132}`
	if err := os.WriteFile(tpl, []byte(stale), 0o644); err != nil {
		t.Fatalf("write stale: %v", err)
	}
	// A hand-edited file the panel did not write must survive, so the ledger
	// has to treat "differs from what I wrote" as "not mine". Remove the ledger
	// to reproduce a panel installed before it existed.
	if err := os.Remove(filepath.Join(dir, "templates", seedLedger)); err != nil {
		t.Fatalf("remove ledger: %v", err)
	}
	if err := LoadTemplates(dir); err != nil {
		t.Fatalf("second load: %v", err)
	}
	got := templateBySlug("bedrock")
	if got == nil || len(got.Versions) == 0 || got.Versions[0] != "LATEST" {
		t.Fatalf("a stale seeded template was not refreshed: %+v", got)
	}
	if len(got.Protocols) == 0 || got.Protocols[0] != "udp" {
		t.Error("the refreshed template still does not publish UDP")
	}
	// Nothing is thrown away when the panel cannot tell whose file it was.
	if _, err := os.Stat(tpl + ".superseded"); err != nil {
		t.Errorf("the previous file was discarded rather than kept aside: %v", err)
	}

	// Now an operator edits it on purpose, with the ledger in place.
	edited := strings.Replace(string(mustRead(t, tpl)), `"max_players": 10`, `"max_players": 44`, 1)
	if edited == string(mustRead(t, tpl)) {
		t.Fatal("test did not actually change the file")
	}
	if err := os.WriteFile(tpl, []byte(edited), 0o644); err != nil {
		t.Fatalf("write edit: %v", err)
	}
	if err := LoadTemplates(dir); err != nil {
		t.Fatalf("third load: %v", err)
	}
	if got := templateBySlug("bedrock"); got == nil || got.MaxPlayers != 44 {
		t.Fatalf("an operator's edit was reverted by an upgrade: %+v", got)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// Every environment variable the runner set was an itzg convention, and the
// bind mount always landed on /data. Terraria keeps its worlds elsewhere and
// takes its settings as command-line flags, so it would have been handed seven
// variables it ignores and a mount on a path it never reads - generating a
// world in its own empty directory while the panel backed up nothing.
func TestTemplateDrivenImageLaunchesFromItsOwnTemplate(t *testing.T) {
	tpl := templateBySlug("terraria")
	if tpl == nil {
		t.Fatal("the terraria template is missing")
	}
	s := &Server{
		ID: "s1", Name: "T", Template: "terraria", Image: tpl.Image,
		Port: 7777, MemoryMB: 1024, CPU: 1, MaxPlayers: 8,
		DataPath: tpl.DataPath, Env: tpl.Env, Args: tpl.Args, Console: tpl.Console,
		Props: map[string]string{"motd": "friends"},
	}
	got := strings.Join(dockerRunArgs(s, "gamepanel-s1", "/var/teploy-arcade/servers/s1", "secret"), " ")

	// None of the Minecraft environment belongs on this image.
	for _, unwanted := range []string{"EULA=", "TYPE=", "VERSION=", "MEMORY=", "SERVER_PORT=", "MAX_PLAYERS=", "RCON"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("a Minecraft variable (%s) was passed to a non-Minecraft image: %s", unwanted, got)
		}
	}
	for _, want := range []string{
		"WORLD_FILENAME=world.wld",
		"-v /var/teploy-arcade/servers/s1:/root/.local/share/Terraria/Worlds",
		"-p 7777:7777/tcp",
		"-autocreate 2",
		"-port 7777",         // ${PORT} expanded
		"-maxplayers 8",      // ${MAX_PLAYERS} expanded
		"-worldname friends", // ${MOTD} expanded
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in: %s", want, got)
		}
	}
	// Arguments go after the image, not before it.
	if strings.Index(got, tpl.Image) > strings.Index(got, "-autocreate") {
		t.Error("command arguments were placed before the image name")
	}

	// TShock reads its console on stdin and containers run detached, so there
	// is no pipe. Refused with a reason beats accepted and dropped.
	if consoleMode(s) != ConsoleNone {
		t.Fatalf("terraria console mode is %q", consoleMode(s))
	}
	// And nothing may ask it for a player list it cannot answer.
	mgr := NewManager(t.TempDir(), NewHub())
	if mgr.canAskWhoIsOnline(s) {
		t.Error("the panel would query a server that cannot answer")
	}

	// A Minecraft image must be completely unaffected by all of this.
	mc := &Server{
		ID: "s2", Template: "paper", Image: "itzg/minecraft-server", Port: 25565,
		MemoryMB: 2048, CPU: 2, MaxPlayers: 20, Props: map[string]string{},
	}
	mcArgs := strings.Join(dockerRunArgs(mc, "gamepanel-s2", "/srv", "secret"), " ")
	for _, want := range []string{"EULA=TRUE", "ENABLE_RCON=true", "SERVER_PORT=25565", "-v /srv:/data"} {
		if !strings.Contains(mcArgs, want) {
			t.Errorf("the Minecraft launch changed: missing %q", want)
		}
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

// Palworld is the case that proved the memory guard earns its place. Its own
// documentation asks for 16 GB, the deployed host has 13, and the honest thing
// is a template carrying the real number and a create that refuses with the
// machine's actual capacity - not a smaller number that starts and is killed.
func TestPalworldIsRefusedOnASmallHost(t *testing.T) {
	tpl := templateBySlug("palworld")
	if tpl == nil {
		t.Fatal("the palworld template is missing")
	}
	if tpl.MemoryMB != 16384 {
		t.Errorf("palworld asks for %d MB; the honest figure is its documented 16384", tpl.MemoryMB)
	}

	mgr := NewManager(t.TempDir(), NewHub())
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	prev := hostMemMB
	hostMemMB = 13312 // the deployed LXC
	defer func() { hostMemMB = prev }()

	_, err := mgr.Create("pals", "palworld", "latest", 0, tpl.MemoryMB, tpl.CPU, RuntimeSim)
	if err == nil {
		t.Fatal("a 16 GB server was created on a 13 GB host")
	}
	if !strings.Contains(err.Error(), "13312") {
		t.Errorf("the refusal does not say what the host actually has: %v", err)
	}

	// And on a host that can hold it, nothing stands in the way.
	hostMemMB = 32768
	if _, err := mgr.Create("pals2", "palworld", "latest", 0, tpl.MemoryMB, tpl.CPU, RuntimeSim); err != nil {
		t.Fatalf("refused on a host with room: %v", err)
	}
}

// Palworld's ports are two UDP listeners that a span cannot express: the game
// on 8211 and Steam's query on 27015.
func TestPalworldPublishesBothUDPPorts(t *testing.T) {
	tpl := templateBySlug("palworld")
	s := &Server{Port: tpl.PortHint, Protocols: tpl.Protocols, ExtraPorts: tpl.ExtraPorts}
	got := strings.Join(publishArgs(s, ""), " ")
	for _, want := range []string{"-p 8211:8211/udp", "-p 27015:27015/udp"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "/tcp") {
		t.Errorf("published a TCP port for a UDP-only game: %q", got)
	}
}

// CS2 is the inverse of Palworld: it fits in memory on this host and the cost
// is a ~30 GB download on first start. Its values came from the image's own
// config over the registry API, so they are worth pinning.
func TestCS2TemplateMatchesItsImage(t *testing.T) {
	tpl := templateBySlug("cs2")
	if tpl == nil {
		t.Fatal("the cs2 template is missing")
	}
	s := &Server{
		ID: "s1", Template: "cs2", Image: tpl.Image, Port: tpl.PortHint,
		MemoryMB: tpl.MemoryMB, CPU: tpl.CPU, MaxPlayers: tpl.MaxPlayers,
		Protocols: tpl.Protocols, ExtraPorts: tpl.ExtraPorts,
		DataPath: tpl.DataPath, Env: tpl.Env, Console: tpl.Console,
		Props: map[string]string{"motd": "friends only"},
	}
	got := strings.Join(dockerRunArgs(s, "gamepanel-s1", "/srv/cs2", "secret"), " ")

	for _, want := range []string{
		"-p 27015:27015/udp", "-p 27015:27015/tcp", // game and rcon
		"-p 27020:27020/udp",                    // GOTV
		"-v /srv/cs2:/home/steam/cs2-dedicated", // STEAMAPPDIR, not /data
		"CS2_PORT=27015", "CS2_MAXPLAYERS=10", "CS2_SERVERNAME=friends only",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in: %s", want, got)
		}
	}
	// RCON rides on a published port. Shipping the image's own "changeme" on it
	// would be handing out a remote console to anyone who scans the host.
	if strings.Contains(got, "changeme") {
		t.Error("the image's default RCON password was shipped on a published port")
	}
	if !strings.Contains(got, "CS2_RCONPW=") {
		t.Error("RCON password is not being set at all, so the image default applies")
	}
}

// The groups drifted from what they meant. "Playable Server" held the seven
// Java flavours and Terraria while Bedrock - which is Minecraft - sat under
// "Other" with Rust and Valheim. Terraria, Rust and CS2 are all playable
// servers too, so the label described nothing; the split was Minecraft versus
// everything else all along.
func TestGroupsSplitMinecraftFromOtherGames(t *testing.T) {
	byGroup := map[string][]string{}
	for _, tpl := range allTemplates() {
		byGroup[tpl.Group] = append(byGroup[tpl.Group], tpl.Slug)
	}

	inMinecraft := map[string]bool{}
	for _, slug := range byGroup["Minecraft"] {
		inMinecraft[slug] = true
	}
	// Both editions, and every Java flavour.
	for _, slug := range []string{"vanilla", "bedrock", "spigot", "paper", "purpur", "fabric", "forge", "neoforge"} {
		if !inMinecraft[slug] {
			t.Errorf("%s is Minecraft but is not in the Minecraft group", slug)
		}
	}
	// And nothing that is not Minecraft.
	for _, slug := range byGroup["Minecraft"] {
		tpl := templateBySlug(slug)
		if !strings.HasPrefix(tpl.Game, "minecraft") {
			t.Errorf("%s (%s) is in the Minecraft group but is not Minecraft", slug, tpl.Game)
		}
	}
	for _, slug := range []string{"terraria", "rust", "valheim", "palworld", "cs2"} {
		if tpl := templateBySlug(slug); tpl == nil || tpl.Group != "Other" {
			t.Errorf("%s should be grouped with the other games, not %q", slug, tpl.Group)
		}
	}
	if len(byGroup["Network Proxy"]) != 1 || byGroup["Network Proxy"][0] != "velocity" {
		t.Errorf("the proxy group is %v", byGroup["Network Proxy"])
	}
	if len(byGroup["Playable Server"]) != 0 {
		t.Errorf("the old group name is still in use: %v", byGroup["Playable Server"])
	}
}
