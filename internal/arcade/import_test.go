package arcade

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Import: adopting a server directory that already exists on the host.
//
// The fixtures mirror a real Crafty Controller host - four Paper servers, a
// Velocity proxy with no world, a Fabric server with a mods directory and a
// Forge 1.12.2 server, some carrying another panel's config file and
// crafty_managed.txt from an earlier migration. Those are the directories the
// feature exists for, so those are the directories it is tested against.

func newImportAgent(t *testing.T) (*httptest.Server, *Manager) {
	t.Helper()
	hub := NewHub()
	mgr := NewManager(t.TempDir(), hub)
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	// The five seeded demo servers hold ports 25565-25577, which are the ports
	// the real fixtures actually use. Import has to be tested against those, not
	// against ports chosen to dodge the seed.
	for _, s := range mgr.List() {
		if err := mgr.Delete(s.ID); err != nil {
			t.Fatalf("clearing seeded server %s: %v", s.Name, err)
		}
	}
	api := &API{mgr: mgr, hub: hub}
	mux := http.NewServeMux()
	api.RoutesImport(mux)
	return httptest.NewServer(mgr.auth.attach(mux)), mgr
}

// mkTree writes a fixture directory. Values are file contents; a key ending in
// "/" is an empty directory.
func mkTree(t *testing.T, name string, files map[string]string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if strings.HasSuffix(rel, "/") {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// EvalSymlinks because t.TempDir() is under /var on macOS, which is a link
	// into /private/var - the scan resolves it and the assertions must compare
	// against the same form.
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

const propsPaper = `#Minecraft server properties
motd=Survie PSEU1
server-port=25566
max-players=40
level-name=world
difficulty=hard
online-mode=true
enable-rcon=true
rcon.port=25575
level-type=minecraft:normal
`

func paperFixture(t *testing.T) string {
	return mkTree(t, "Survie PSEU1", map[string]string{
		"paper.jar":               "PK\x03\x04 pretend jar",
		"server.properties":       propsPaper,
		"eula.txt":                "eula=true\n",
		"crafty_managed.txt":      "managed by crafty\n",
		"world/level.dat":         "\x1f\x8b level data",
		"world/region/r.0.0.mca":  strings.Repeat("chunk", 400),
		"plugins/EssentialsX.jar": "plugin",
		"logs/latest.log":         "[12:00:00] [Server thread/INFO]: Done\n",
		"banned-players.json":     "[]\n",
	})
}

func velocityFixture(t *testing.T) string {
	return mkTree(t, "proxy", map[string]string{
		"velocity.jar":      "PK\x03\x04 pretend proxy",
		"velocity.toml":     "config-version = \"2.6\"\nbind = \"0.0.0.0:25565\"\nmotd = \"<#09add3>A Velocity Server\"\n",
		"forwarding.secret": "not-a-real-secret",
		// Deliberately stale: proxies collect a server.properties from whatever
		// the directory used to be, and taking the port from it would put the
		// panel's ledger on a port nothing is listening on.
		"server.properties": "server-port=25577\nmotd=leftover\n",
	})
}

func fabricFixture(t *testing.T) string {
	return mkTree(t, "cobblemon", map[string]string{
		"fabric-1.21.1.jar":       "PK\x03\x04 pretend jar",
		"server.properties":       "server-port=25571\nmotd=Cobblemon\nmax-players=20\nlevel-name=world\n",
		"mcss_server_config.json": `{"name":"BigTar's Cobblemon Server","autostart":false}`,
		"mods/cobblemon.jar":      "mod",
		"mods/architectury.jar":   "mod",
		"mods/fabric-api.jar":     "mod",
		"world/level.dat":         "level",
	})
}

func waitImport(t *testing.T, id string, budget time.Duration) ImportJob {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		j, ok := importJobByID(id)
		if !ok {
			t.Fatalf("import job %s vanished", id)
		}
		if v := j.view(); v.State != ImportRunning {
			return v
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("import job %s never finished", id)
	return ImportJob{}
}

// treeFingerprint is content plus permissions for every file under root. Used
// to prove an import did not touch the operator's directory.
func treeFingerprint(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			out[rel+"/"] = "dir " + info.Mode().Perm().String()
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = fmt.Sprintf("%x %s", sha256.Sum256(b), info.Mode().Perm())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Detection has to be right about the four layouts the real host actually has,
// including the two that are easy to get wrong: a proxy whose port lives in
// velocity.toml rather than server.properties, and a Forge jar whose name
// carries two version numbers (the Minecraft one and Forge's own build).
func TestScanIdentifiesTheRealServerLayouts(t *testing.T) {
	_, mgr := newImportAgent(t)

	forge := mkTree(t, "pixelmon", map[string]string{
		"forge-1.12.2-14.23.5.2860.jar": "jar",
		"server.properties":             "server-port=25572\nmotd=BigTar's Pixelmon Server\n",
		"world/level.dat":               "level",
	})

	cases := []struct {
		name       string
		path       string
		template   string
		version    string
		port       int
		portSource string
		proxy      bool
		world      bool
		mods       int
	}{
		{"paper", paperFixture(t), "paper", "", 25566, "server.properties", false, true, 0},
		{"velocity", velocityFixture(t), "velocity", "", 25565, "velocity.toml", true, false, 0},
		{"fabric", fabricFixture(t), "fabric", "1.21.1", 25571, "server.properties", false, true, 3},
		{"forge", forge, "forge", "1.12.2", 25572, "server.properties", false, true, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sc, err := mgr.ScanImport(c.path)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if !sc.IsServer || !sc.Recognised {
				t.Fatalf("is_server=%v recognised=%v reason=%q; this is a real server directory",
					sc.IsServer, sc.Recognised, sc.Reason)
			}
			if sc.Template != c.template {
				t.Errorf("template %q, want %q (jar %q)", sc.Template, c.template, sc.Jar)
			}
			if sc.Version != c.version {
				t.Errorf("version %q, want %q", sc.Version, c.version)
			}
			if sc.Port != c.port {
				t.Errorf("port %d from %q, want %d", sc.Port, sc.PortSource, c.port)
			}
			if sc.PortSource != c.portSource {
				t.Errorf("port came from %q, want %q", sc.PortSource, c.portSource)
			}
			if sc.Proxy != c.proxy {
				t.Errorf("proxy=%v, want %v", sc.Proxy, c.proxy)
			}
			if sc.HasWorld != c.world {
				t.Errorf("has_world=%v (world %q), want %v", sc.HasWorld, sc.World, c.world)
			}
			if sc.Mods != c.mods {
				t.Errorf("mods=%d, want %d", sc.Mods, c.mods)
			}
			if sc.SizeBytes <= 0 {
				t.Errorf("size_bytes=%d; the operator is deciding whether to copy this", sc.SizeBytes)
			}
		})
	}
}

// The names these servers were given by whatever managed them last are the
// names their operator knows them by; the directory is often a slug.
func TestScanRecoversTheNameAndThePreviousManager(t *testing.T) {
	_, mgr := newImportAgent(t)

	sc, err := mgr.ScanImport(fabricFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if sc.Name != "BigTar's Cobblemon Server" {
		t.Errorf("suggested name %q, want the one from the previous panel's config", sc.Name)
	}
	if sc.NameSource != "mcss_server_config.json" {
		t.Errorf("name_source %q; the UI has to say where the name came from", sc.NameSource)
	}
	if len(sc.ManagedBy) != 1 || sc.ManagedBy[0] != "another control panel" {
		t.Errorf("managed_by %v, want the de-branded label", sc.ManagedBy)
	}

	paper, err := mgr.ScanImport(paperFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(paper.ManagedBy) != 1 || paper.ManagedBy[0] != "Crafty Controller" {
		t.Errorf("managed_by %v, want Crafty Controller", paper.ManagedBy)
	}
	if !hasWarning(paper.Warnings, "two panels") {
		t.Errorf("a directory another panel still manages must warn against adopting it in place; got %v", paper.Warnings)
	}
}

// Guessing here picks the server software. A jar named server.jar says nothing
// about what is inside it, and a directory with two different server jars says
// less - both must be reported as unrecognised rather than resolved silently.
func TestScanSaysUnrecognisedRatherThanGuessing(t *testing.T) {
	_, mgr := newImportAgent(t)

	anon := mkTree(t, "anon", map[string]string{
		"server.jar":        "jar",
		"server.properties": "server-port=25580\n",
	})
	sc, err := mgr.ScanImport(anon)
	if err != nil {
		t.Fatal(err)
	}
	if !sc.IsServer {
		t.Fatal("a directory with a jar and a server.properties is a server directory")
	}
	if sc.Recognised || sc.Template != "" {
		t.Errorf("recognised=%v template=%q; server.jar cannot identify anything", sc.Recognised, sc.Template)
	}
	if !strings.Contains(sc.Reason, "server.jar") {
		t.Errorf("reason %q must name the file it could not identify", sc.Reason)
	}

	if _, err := mgr.StartImport(ImportRequest{Path: anon, Name: "Anon"}, "tester"); err == nil {
		t.Error("import of an unidentified directory must be refused until a template is chosen")
	}
	// ...and accepted the moment the operator says which one it is.
	job, err := mgr.StartImport(ImportRequest{Path: anon, Name: "Anon", Template: "paper"}, "tester")
	if err != nil {
		t.Fatalf("import with an explicit template: %v", err)
	}
	if v := waitImport(t, job.ID, 10*time.Second); v.State != ImportDone {
		t.Fatalf("import state %q: %s", v.State, v.Error)
	}

	both := mkTree(t, "both", map[string]string{
		"paper.jar": "jar", "forge.jar": "jar", "server.properties": "server-port=25581\n",
	})
	sc, err = mgr.ScanImport(both)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Recognised {
		t.Errorf("two server jars cannot identify one server; got template %q", sc.Template)
	}
	if !strings.Contains(sc.Reason, "paper.jar") || !strings.Contains(sc.Reason, "forge.jar") {
		t.Errorf("reason %q must name both jars", sc.Reason)
	}
}

// Every refusal has to name what is wrong with the path the operator typed.
func TestImportRefusalsSayWhatIsWrong(t *testing.T) {
	_, mgr := newImportAgent(t)

	notAServer := mkTree(t, "documents", map[string]string{
		"README.md": "# notes\n", "notes/todo.txt": "buy milk\n",
	})
	sc, err := mgr.ScanImport(notAServer)
	if err != nil {
		t.Fatalf("scanning a directory that is not a server must still answer: %v", err)
	}
	if sc.IsServer {
		t.Fatal("a directory of text files is not a game server")
	}
	if !strings.Contains(sc.Reason, "jar") {
		t.Errorf("reason %q must say a jar is what is missing", sc.Reason)
	}
	if _, err := mgr.StartImport(ImportRequest{Path: notAServer, Name: "Docs"}, "tester"); err == nil {
		t.Error("importing a directory with no server jar must be refused")
	}

	missing := filepath.Join(notAServer, "does-not-exist")
	if _, err := mgr.ScanImport(missing); err == nil || !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("a missing path must be refused by name, got %v", err)
	}
	if _, err := mgr.ScanImport("servers/paper"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("a relative path resolves against the panel's working directory, not the operator's; got %v", err)
	}
	if _, err := mgr.ScanImport(filepath.Join(notAServer, "README.md")); err == nil {
		t.Error("a file is not a server directory")
	}
	// The panel's own tree is off limits: those servers are already managed
	// here, and copying a parent of the data directory copies the copy.
	if _, err := mgr.ScanImport(mgr.dataDir); err == nil {
		t.Error("importing the panel's own data directory must be refused")
	}
}

// A port already in the panel's ledger is a bind failure waiting for the next
// start, and the message has to name the server holding it.
func TestImportRefusesAPortThePanelAlreadyHolds(t *testing.T) {
	_, mgr := newImportAgent(t)
	src := paperFixture(t)

	job, err := mgr.StartImport(ImportRequest{Path: src, Name: "Survie PSEU1"}, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if v := waitImport(t, job.ID, 10*time.Second); v.State != ImportDone {
		t.Fatalf("first import: %s", v.Error)
	}

	// Scanning the same directory again must now report the clash before the
	// operator commits to it.
	sc, err := mgr.ScanImport(src)
	if err != nil {
		t.Fatal(err)
	}
	if sc.PortTakenBy != "Survie PSEU1" {
		t.Errorf("port_taken_by %q, want the server already on port 25566", sc.PortTakenBy)
	}

	_, err = mgr.StartImport(ImportRequest{Path: src, Name: "Survie PSEU1 again"}, "tester")
	if err == nil {
		t.Fatal("importing a second server onto port 25566 must be refused")
	}
	if !strings.Contains(err.Error(), "25566") || !strings.Contains(err.Error(), "Survie PSEU1") {
		t.Errorf("refusal %q must name the port and the server holding it", err)
	}
}

// The source may be a live server another panel is still running, so a copy
// import must leave it byte for byte as it found it.
func TestImportCopiesAndNeverWritesToTheSource(t *testing.T) {
	_, mgr := newImportAgent(t)
	src := paperFixture(t)
	before := treeFingerprint(t, src)

	job, err := mgr.StartImport(ImportRequest{Path: src, Name: "Survie PSEU1"}, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if job.Mode != ImportCopy {
		t.Errorf("mode %q; copy is the default because the source may be live", job.Mode)
	}
	v := waitImport(t, job.ID, 20*time.Second)
	if v.State != ImportDone {
		t.Fatalf("import failed: %s", v.Error)
	}
	if v.ServerID == "" {
		t.Fatal("a finished import must report the server it created")
	}

	if after := treeFingerprint(t, src); !reflect.DeepEqual(before, after) {
		t.Errorf("the source directory changed during the import\nbefore %v\nafter  %v", before, after)
	}

	s := mgr.Get(v.ServerID)
	if s == nil {
		t.Fatal("the imported server is not in the manager")
	}
	if s.Port != 25566 || s.MaxPlayers != 40 || s.MOTD() != "Survie PSEU1" {
		t.Errorf("imported port=%d max=%d motd=%q; want the values from server.properties",
			s.Port, s.MaxPlayers, s.MOTD())
	}
	if s.Version != "unknown" {
		t.Errorf("version %q; paper.jar carries none, and inventing one from the template's list would be a guess", s.Version)
	}

	// The copy is a copy, not a fresh server directory.
	dstDir := mgr.serverDir(s)
	for _, rel := range []string{"world/level.dat", "world/region/r.0.0.mca", "plugins/EssentialsX.jar", "paper.jar"} {
		want, err := os.ReadFile(filepath.Join(src, rel))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(dstDir, rel))
		if err != nil {
			t.Fatalf("%s did not come across: %v", rel, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s differs from the source", rel)
		}
	}

	// Keys the panel has no schema for have to survive. writeProps rewrites the
	// whole file from s.Props the first time anyone saves a setting, so a key
	// dropped at import is a key deleted out of the operator's server.properties
	// later, with nothing on screen connecting the two.
	if got := s.Props["rcon.port"]; got != "25575" {
		t.Errorf("rcon.port = %q after import; unmodelled keys must be kept", got)
	}
	if err := mgr.writeProps(s); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(filepath.Join(dstDir, "server.properties"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"rcon.port=25575", "level-type=minecraft:normal", "server-port=25566"} {
		if !strings.Contains(string(written), want) {
			t.Errorf("saving settings dropped %q out of the imported server.properties", want)
		}
	}
}

// Adopt-in-place is the alternative for a world too big to duplicate. It must
// not copy, and deleting the panel entry must not take the operator's server
// with it.
func TestImportAdoptInPlaceLinksInsteadOfCopying(t *testing.T) {
	_, mgr := newImportAgent(t)
	src := fabricFixture(t)

	job, err := mgr.StartImport(ImportRequest{
		Path: src, Name: "BigTar's Cobblemon Server", Mode: ImportAdopt,
	}, "tester")
	if err != nil {
		t.Fatal(err)
	}
	v := waitImport(t, job.ID, 10*time.Second)
	if v.State != ImportDone {
		t.Fatalf("adopt failed: %s", v.Error)
	}
	if v.Copied != 0 {
		t.Errorf("adopt copied %d bytes; the whole point is that it copies nothing", v.Copied)
	}

	s := mgr.Get(v.ServerID)
	if s == nil {
		t.Fatal("the adopted server is not in the manager")
	}
	link := filepath.Join(mgr.dataDir, "servers", s.ID)
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("no server directory for the adopted server: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("adopt made a real directory; the source was duplicated after all")
	}
	if target, err := filepath.EvalSymlinks(link); err != nil || target != src {
		t.Fatalf("server directory points at %q, want %q (%v)", target, src, err)
	}

	// The panel's own file API has to see the operator's files through it.
	entries, err := mgr.ListFiles(s, "")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Name] = true
	}
	for _, want := range []string{"fabric-1.21.1.jar", "server.properties", "mods", "world"} {
		if !seen[want] {
			t.Errorf("the file manager cannot see %q in the adopted directory", want)
		}
	}

	// And a write through the panel lands in the operator's directory - that is
	// the difference from a copy, and it is the reason the UI has to be explicit
	// about which mode is running.
	if err := mgr.WriteFile(s, "panel-wrote-this.txt", "hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(src, "panel-wrote-this.txt")); err != nil {
		t.Errorf("a write to an adopted server did not reach the adopted directory: %v", err)
	}

	// Deleting the panel entry removes the link, never the server behind it.
	if err := mgr.Delete(s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(src, "fabric-1.21.1.jar")); err != nil {
		t.Fatalf("deleting the panel entry destroyed the operator's own server directory: %v", err)
	}
}

// Adopting one directory twice would give two panel entries one server, one
// port and one world.
func TestImportRefusesToAdoptTheSameDirectoryTwice(t *testing.T) {
	_, mgr := newImportAgent(t)
	src := fabricFixture(t)

	if _, err := mgr.StartImport(ImportRequest{Path: src, Name: "Cobblemon", Mode: ImportAdopt}, "tester"); err != nil {
		t.Fatal(err)
	}
	sc, err := mgr.ScanImport(src)
	if err != nil {
		t.Fatal(err)
	}
	if sc.AdoptedBy != "Cobblemon" {
		t.Errorf("adopted_by %q, want the server already linked to this directory", sc.AdoptedBy)
	}
	_, err = mgr.StartImport(ImportRequest{Path: src, Name: "Cobblemon 2", Mode: ImportAdopt, Port: 25599}, "tester")
	if err == nil {
		t.Fatal("adopting the same directory twice must be refused")
	}
}

// A copy that fills the panel's disk takes down every other server sharing it,
// so the refusal happens before the first byte rather than mid-copy.
func TestImportRefusesACopyThatWillNotFit(t *testing.T) {
	_, mgr := newImportAgent(t)
	src := paperFixture(t)

	restore := diskFree
	diskFree = func(string) (int64, error) { return 1 << 10, nil }
	defer func() { diskFree = restore }()

	sc, err := mgr.ScanImport(src)
	if err != nil {
		t.Fatal(err)
	}
	if sc.EnoughSpace {
		t.Fatal("a 1 KB disk cannot hold the copy")
	}
	_, err = mgr.StartImport(ImportRequest{Path: src, Name: "Survie"}, "tester")
	if err == nil {
		t.Fatal("a copy with nowhere to land must be refused")
	}
	if !strings.Contains(err.Error(), "in place") {
		t.Errorf("refusal %q should point at the mode that would work", err)
	}
	// Adopting needs no space, so it must still be allowed.
	if _, err := mgr.StartImport(ImportRequest{Path: src, Name: "Survie", Mode: ImportAdopt}, "tester"); err != nil {
		t.Errorf("adopt needs no free space but was refused: %v", err)
	}
}

// The HTTP surface end to end: scan, start, poll. A multi-gigabyte copy cannot
// block the request that started it.
func TestImportOverHTTPReturnsAJobAndCompletes(t *testing.T) {
	srv, mgr := newImportAgent(t)
	defer srv.Close()
	src := paperFixture(t)

	post := func(path string, body any) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		res, err := http.Post(srv.URL+path, "application/json", strings.NewReader(string(b)))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}

	code, scan := post("/api/import/scan", map[string]string{"path": src})
	if code != 200 {
		t.Fatalf("scan returned %d: %v", code, scan)
	}
	if scan["template"] != "paper" || scan["recognised"] != true {
		t.Fatalf("scan reported %v", scan)
	}

	code, job := post("/api/import", map[string]string{"path": src, "name": "Survie PSEU1", "mode": "copy"})
	if code != http.StatusAccepted {
		t.Fatalf("import returned %d: %v", code, job)
	}
	id, _ := job["id"].(string)
	if id == "" {
		t.Fatal("import must return a job to poll")
	}

	deadline := time.Now().Add(20 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		res, err := http.Get(srv.URL + "/api/import/" + id)
		if err != nil {
			t.Fatal(err)
		}
		_ = json.NewDecoder(res.Body).Decode(&last)
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("progress returned %d: %v", res.StatusCode, last)
		}
		if last["state"] != ImportRunning {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if last["state"] != ImportDone {
		t.Fatalf("import did not finish: %v", last)
	}
	sid, _ := last["server_id"].(string)
	if mgr.Get(sid) == nil {
		t.Fatalf("finished import did not create a server (%v)", last)
	}

	res, err := http.Get(srv.URL + "/api/import/nosuchjob")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Errorf("unknown job returned %d, want 404", res.StatusCode)
	}
}

// Scanning reads any directory the panel process can read, and importing
// creates a server. Both are admin acts, and an ungated route here is the same
// disclosure the six ungated GETs were.
func TestImportRoutesRequireAnAdminSession(t *testing.T) {
	srv, mgr := newImportAgent(t)
	defer srv.Close()

	if _, err := mgr.auth.CreateUser("admin", "correct-horse-battery", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct{ method, path string }{
		{"POST", "/api/import/scan"},
		{"POST", "/api/import"},
		{"GET", "/api/import/whatever"},
	} {
		req, _ := http.NewRequest(r.method, srv.URL+r.path, strings.NewReader("{}"))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s returned %d to an anonymous caller, want 401", r.method, r.path, res.StatusCode)
		}
	}

	// An operator is not enough either: import creates servers.
	s, err := mgr.auth.Login("admin", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.auth.CreateUser("op", "correct-horse-battery", RoleOperator); err != nil {
		t.Fatal(err)
	}
	opSess, err := mgr.auth.Login("op", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		token string
		want  int
	}{{opSess.Token, http.StatusForbidden}, {s.Token, http.StatusBadRequest}} {
		req, _ := http.NewRequest("POST", srv.URL+"/api/import/scan", strings.NewReader(`{"path":""}`))
		req.AddCookie(&http.Cookie{Name: "gss_session", Value: c.token})
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != c.want {
			t.Errorf("scan returned %d, want %d", res.StatusCode, c.want)
		}
	}
}

func hasWarning(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

// Forge's installer places `forge-<ver>.jar` next to the vanilla
// `minecraft_server.<ver>.jar` it runs on, and Fabric does the same. That is
// one server plus its dependency, but it looked identical to "two different
// server softwares in one directory" and every classic Forge modpack was
// refused. Found migrating a real RL Craft install, not by this suite.
func TestAModpackShippingItsVanillaJarIsStillRecognised(t *testing.T) {
	_, mgr := newImportAgent(t)

	cases := []struct {
		name         string
		files        map[string]string
		wantTemplate string
		wantJar      string
	}{
		{
			name: "forge 1.12 modpack",
			files: map[string]string{
				"forge-1.12.2-14.23.5.2860.jar": "PK\x03\x04 forge",
				"minecraft_server.1.12.2.jar":   "PK\x03\x04 vanilla",
				"server.properties":             "server-port=25572\n",
				"world/level.dat":               "level",
			},
			wantTemplate: "forge",
			wantJar:      "forge-1.12.2-14.23.5.2860.jar",
		},
		{
			name: "fabric modpack with the vanilla jar beside it",
			files: map[string]string{
				"fabric-server-launch.jar":    "PK\x03\x04 fabric",
				"minecraft_server.1.20.1.jar": "PK\x03\x04 vanilla",
				"server.properties":           "server-port=25580\n",
				"world/level.dat":             "level",
			},
			wantTemplate: "fabric",
			wantJar:      "fabric-server-launch.jar",
		},
	}

	for _, c := range cases {
		dir := mkTree(t, strings.ReplaceAll(c.name, " ", "-"), c.files)
		sc, err := mgr.ScanImport(dir)
		if err != nil {
			t.Fatalf("%s: scan: %v", c.name, err)
		}
		if !sc.Recognised {
			t.Errorf("%s: not recognised: %s", c.name, sc.Reason)
			continue
		}
		if sc.Template != c.wantTemplate {
			t.Errorf("%s: template = %q, want %q", c.name, sc.Template, c.wantTemplate)
		}
		if sc.Jar != c.wantJar {
			t.Errorf("%s: jar = %q, want %q (the loader, not the vanilla dependency)", c.name, sc.Jar, c.wantJar)
		}
	}
}

// Two genuine server softwares must still be refused: that is a directory the
// panel cannot make a safe guess about, and guessing would run the wrong one.
func TestTwoRealServerJarsAreStillAmbiguous(t *testing.T) {
	_, mgr := newImportAgent(t)
	dir := mkTree(t, "ambiguous", map[string]string{
		"paper-1.20.4.jar":  "PK\x03\x04 paper",
		"forge-1.20.1.jar":  "PK\x03\x04 forge",
		"server.properties": "server-port=25590\n",
		"world/level.dat":   "level",
	})
	sc, err := mgr.ScanImport(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if sc.Recognised {
		t.Errorf("paper + forge should stay ambiguous, got template %q", sc.Template)
	}
	if !strings.Contains(sc.Reason, "more than one server software") {
		t.Errorf("reason should explain the ambiguity, got %q", sc.Reason)
	}
}

// A Forge installer commonly leaves a plain "forge.jar" with no version in the
// name. The version is what selects the JRE, so an unknown one hands a 1.16.5
// pack a Java 21 image it cannot start on - and the vanilla server jar beside
// it names the version exactly.
func TestVersionIsRecoveredFromTheVanillaJar(t *testing.T) {
	_, mgr := newImportAgent(t)
	dir := mkTree(t, "pixelmon-like", map[string]string{
		"forge.jar":                   "PK\x03\x04 forge",
		"minecraft_server.1.16.5.jar": "PK\x03\x04 vanilla",
		"server.properties":           "server-port=25569\n",
		"world/level.dat":             "level",
	})
	sc, err := mgr.ScanImport(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if sc.Template != "forge" {
		t.Fatalf("template = %q, want forge", sc.Template)
	}
	if sc.Version != "1.16.5" {
		t.Fatalf("version = %q, want 1.16.5 recovered from the vanilla jar", sc.Version)
	}
	// And the whole point: that version must select a JRE the pack can run on.
	if got := imageForVersion("itzg/minecraft-server", sc.Version); got != "itzg/minecraft-server:"+imageJava8 {
		t.Errorf("image = %q; a 1.16.5 pack needs Java 8", got)
	}
}

// "paper.jar" carries no version in its name, which is the common case for a
// server that updates in place - so imports recorded "unknown" and the JRE was
// chosen by whatever the untagged image happened to ship. The jar itself knows:
// version.json in the archive root names the exact version.
func TestVersionIsReadFromInsideTheJar(t *testing.T) {
	_, mgr := newImportAgent(t)
	dir := t.TempDir()

	// A jar is a zip; build one carrying version.json, as Paper does.
	jar := filepath.Join(dir, "paper.jar")
	f, err := os.Create(jar)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("version.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(`{"id":"26.1.2","name":"26.1.2","world_version":4790}`)); err != nil {
		t.Fatal(err)
	}
	zw.Close()
	f.Close()

	if err := os.WriteFile(filepath.Join(dir, "server.properties"), []byte("server-port=25566\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "world"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "world", "level.dat"), []byte("lvl"), 0o644); err != nil {
		t.Fatal(err)
	}

	sc, err := mgr.ScanImport(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if sc.Version != "26.1.2" {
		t.Errorf("version = %q, want 26.1.2 read from the jar", sc.Version)
	}
	// And that version must now select a JRE rather than falling through.
	if _, ok := javaTagFor(sc.Version); !ok {
		t.Errorf("version %q still does not resolve a JRE", sc.Version)
	}
}

// A jar with no version.json, or one that is not a zip at all, must not break
// the scan - it falls back to the filename and the vanilla-jar hint.
func TestAJarWithoutVersionJsonStillScans(t *testing.T) {
	_, mgr := newImportAgent(t)
	dir := mkTree(t, "noversion", map[string]string{
		"paper.jar":         "not a zip at all",
		"server.properties": "server-port=25567\n",
		"world/level.dat":   "lvl",
	})
	sc, err := mgr.ScanImport(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if sc.Template != "paper" {
		t.Errorf("template = %q, want paper", sc.Template)
	}
	if sc.Version != "" {
		t.Errorf("version = %q, want empty for an unreadable jar", sc.Version)
	}
}
