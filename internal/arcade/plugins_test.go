package arcade

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Plugins tab. The findings these cover are the ones a plugin screen actually
// gets wrong: showing a directory the server does not read, deleting instead of
// disabling, and treating a pasted URL as trustworthy input.

func newPluginServer(t *testing.T, m *Manager, template string) *Server {
	t.Helper()
	s, err := m.Create("test-"+template, template, "", 0, 0, 0, RuntimeSim)
	if err != nil {
		t.Fatalf("create %s server: %v", template, err)
	}
	return s
}

func pluginFixture(t *testing.T, m *Manager, s *Server, dir string, files map[string]string) string {
	t.Helper()
	abs := filepath.Join(m.serverDir(s), dir)
	if err := os.MkdirAll(abs, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(abs, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return abs
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// A disabled plugin is still installed. Listing only ".jar" would make a
// disabled plugin look deleted, and the operator would install a second copy.
func TestPluginListingReportsDisabledJarsAsInstalled(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := newPluginServer(t, mgr, "paper")

	dir := pluginFixture(t, mgr, s, "plugins", map[string]string{
		"EssentialsX.jar":        "PK\x03\x04essentials",
		"WorldEdit.jar.disabled": "PK\x03\x04worldedit",
		"config.yml":             "not a plugin",
	})
	// A plugin's own config directory sits beside the jars and is not a plugin.
	if err := os.MkdirAll(filepath.Join(dir, "EssentialsX"), 0o755); err != nil {
		t.Fatal(err)
	}

	v, err := mgr.PluginView(s)
	if err != nil {
		t.Fatalf("PluginView: %v", err)
	}
	if !v.Supported || v.Dir != "plugins" || v.Kind != "plugin" {
		t.Fatalf("paper should be supported as plugins/, got supported=%v dir=%q kind=%q",
			v.Supported, v.Dir, v.Kind)
	}
	if len(v.Entries) != 2 {
		t.Fatalf("listed %d entries, want 2 (the yml and the config directory are not plugins): %+v",
			len(v.Entries), v.Entries)
	}

	got := map[string]PluginEntry{}
	for _, e := range v.Entries {
		got[e.Name] = e
	}
	ess, ok := got["EssentialsX"]
	if !ok {
		t.Fatalf("EssentialsX missing from %+v", v.Entries)
	}
	if !ess.Enabled || ess.File != "EssentialsX.jar" {
		t.Errorf("EssentialsX: enabled=%v file=%q, want true/EssentialsX.jar", ess.Enabled, ess.File)
	}
	if ess.Size != int64(len("PK\x03\x04essentials")) || ess.Mod == 0 {
		t.Errorf("EssentialsX: size=%d mod=%d, want the real size and modified time", ess.Size, ess.Mod)
	}
	we, ok := got["WorldEdit"]
	if !ok {
		t.Fatalf("WorldEdit missing - a disabled plugin is still installed: %+v", v.Entries)
	}
	if we.Enabled || we.File != "WorldEdit.jar.disabled" {
		t.Errorf("WorldEdit: enabled=%v file=%q, want false/WorldEdit.jar.disabled", we.Enabled, we.File)
	}
}

// The directory has to come from the template. A Fabric server never reads
// plugins/, so listing it there would show jars the server silently ignores -
// the failure mode where nothing errors and nothing works.
func TestPluginDirectoryFollowsTheTemplate(t *testing.T) {
	_, mgr := newTestAgent(t)

	fabric := newPluginServer(t, mgr, "fabric")
	pluginFixture(t, mgr, fabric, "mods", map[string]string{"Sodium.jar": "PK\x03\x04sodium"})
	pluginFixture(t, mgr, fabric, "plugins", map[string]string{"Bukkit.jar": "PK\x03\x04bukkit"})

	v, err := mgr.PluginView(fabric)
	if err != nil {
		t.Fatalf("PluginView(fabric): %v", err)
	}
	if v.Dir != "mods" || v.Kind != "mod" {
		t.Fatalf("fabric resolved to dir=%q kind=%q, want mods/mod", v.Dir, v.Kind)
	}
	if len(v.Entries) != 1 || v.Entries[0].Name != "Sodium" {
		t.Fatalf("fabric listed %+v, want only the jar in mods/", v.Entries)
	}

	// Vanilla loads neither. Saying so is the answer, not an error: the screen
	// has to render something the operator can read.
	vanilla := newPluginServer(t, mgr, "vanilla")
	vv, err := mgr.PluginView(vanilla)
	if err != nil {
		t.Fatalf("PluginView(vanilla) returned an error instead of an explanation: %v", err)
	}
	if vv.Supported || vv.Reason == "" {
		t.Errorf("vanilla: supported=%v reason=%q, want unsupported with a reason", vv.Supported, vv.Reason)
	}
}

// Disabling must be reversible, which is the whole reason it is a rename and
// not a delete: the jar and its version survive being switched off.
func TestPluginToggleRoundTripsWithoutLosingTheJar(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := newPluginServer(t, mgr, "purpur")
	dir := pluginFixture(t, mgr, s, "plugins", map[string]string{"LuckPerms.jar": "PK\x03\x04luckperms"})

	off, err := mgr.SetPluginEnabled(s, "LuckPerms.jar", false)
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if off.Enabled || off.File != "LuckPerms.jar.disabled" {
		t.Fatalf("disable produced %+v, want the .jar.disabled name", off)
	}
	if _, err := os.Stat(filepath.Join(dir, "LuckPerms.jar")); !os.IsNotExist(err) {
		t.Errorf("LuckPerms.jar is still there after disabling; the loader would still read it")
	}
	body, err := os.ReadFile(filepath.Join(dir, "LuckPerms.jar.disabled"))
	if err != nil || string(body) != "PK\x03\x04luckperms" {
		t.Fatalf("the disabled jar lost its contents: %q, %v", body, err)
	}

	on, err := mgr.SetPluginEnabled(s, "LuckPerms.jar.disabled", true)
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !on.Enabled || on.File != "LuckPerms.jar" {
		t.Fatalf("enable produced %+v, want LuckPerms.jar", on)
	}
	if names := dirNames(t, dir); len(names) != 1 || names[0] != "LuckPerms.jar" {
		t.Errorf("after the round trip the directory holds %v, want exactly LuckPerms.jar", names)
	}

	v, err := mgr.PluginView(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Entries) != 1 || !v.Entries[0].Enabled {
		t.Errorf("listing after the round trip: %+v, want one enabled plugin", v.Entries)
	}
}

// The name is joined onto the plugin directory, and path.Join collapses ".."
// before resolve() ever sees the result - "../server.properties" becomes a
// legal path inside the server root. The refusal has to happen on the name.
func TestPluginDeleteRefusesATraversalPath(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := newPluginServer(t, mgr, "paper")
	props := filepath.Join(mgr.serverDir(s), "server.properties")
	if _, err := os.Stat(props); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	for _, bad := range []string{
		"../server.properties",
		"../../servers.json",
		"/etc/passwd",
		"sub/plugin.jar",
	} {
		if err := mgr.DeletePlugin(s, bad); err == nil {
			t.Errorf("DeletePlugin(%q) was accepted; it must be refused as a path, not a file name", bad)
		}
	}
	if _, err := os.Stat(props); err != nil {
		t.Fatalf("server.properties was deleted through the plugins API: %v", err)
	}

	// The ordinary case still works, or the guard is just a broken feature.
	pluginFixture(t, mgr, s, "plugins", map[string]string{"Vault.jar": "PK\x03\x04vault"})
	if err := mgr.DeletePlugin(s, "Vault.jar"); err != nil {
		t.Fatalf("deleting a real plugin: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mgr.serverDir(s), "plugins", "Vault.jar")); !os.IsNotExist(err) {
		t.Error("Vault.jar survived its own delete")
	}
}

func jarBytes(payload string) []byte { return append([]byte(zipMagic), []byte(payload)...) }

// The download is the one place the panel fetches something a user named, so
// each guard is tested for what it prevents: an arbitrary local read, an
// arbitrary write into the server directory, and a full disk.
func TestPluginInstallRefusesEverythingThatIsNotAJarDownload(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := newPluginServer(t, mgr, "paper")
	dir := filepath.Join(mgr.serverDir(s), "plugins")

	mux := http.NewServeMux()
	mux.HandleFunc("/good.jar", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(jarBytes("essentials"))
	})
	mux.HandleFunc("/payload.sh", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(jarBytes("shell"))
	})
	mux.HandleFunc("/login.jar", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>sign in to download</html>"))
	})
	mux.HandleFunc("/away.jar", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
	})
	mux.HandleFunc("/missing.jar", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	})
	// Declares a size far over the cap and never sends it: the early refusal is
	// what stops the panel pulling gigabytes before deciding it will not keep
	// them.
	mux.HandleFunc("/declared.jar", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "999999999")
		_, _ = w.Write(jarBytes("small"))
	})
	// Sends more than the cap without declaring anything, which is what a
	// hostile server would do. Chunked, so Content-Length cannot catch it.
	mux.HandleFunc("/flood.jar", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(jarBytes(strings.Repeat("A", 8192)))
	})
	origin := httptest.NewServer(mux)
	defer origin.Close()

	// Shrunk so the oversize guard can be exercised without moving 64 MB.
	orig := maxPluginBytes
	maxPluginBytes = 4096
	defer func() { maxPluginBytes = orig }()

	refused := []struct{ name, url, why string }{
		{"non-http scheme", "file:///etc/passwd",
			"file:// makes the panel read a local file and republish it in the server directory"},
		{"non-jar path", origin.URL + "/payload.sh",
			"any URL would otherwise be an arbitrary write into the server directory"},
		{"redirect off http", origin.URL + "/away.jar",
			"a trusted host can redirect to file:// and the client would follow it"},
		{"declared oversize", origin.URL + "/declared.jar",
			"one link would otherwise fill the volume every server shares"},
		{"undeclared oversize", origin.URL + "/flood.jar",
			"Content-Length is a claim the far end can simply omit"},
		{"not a jar at all", origin.URL + "/login.jar",
			"an HTML error page saved as a .jar fails at the game's next boot, not here"},
		{"error response", origin.URL + "/missing.jar",
			"a 404 body would land as a plugin"},
	}
	for _, c := range refused {
		if _, err := mgr.InstallPlugin(s, c.url); err == nil {
			t.Errorf("InstallPlugin(%s) succeeded: %s", c.name, c.why)
		}
		if names := dirNames(t, dir); len(names) != 0 {
			t.Errorf("after a refused install (%s) the directory holds %v; nothing may be left behind",
				c.name, names)
			for _, n := range names {
				_ = os.Remove(filepath.Join(dir, n))
			}
		}
	}

	e, err := mgr.InstallPlugin(s, origin.URL+"/good.jar")
	if err != nil {
		t.Fatalf("a real jar was refused: %v", err)
	}
	if !e.Enabled || e.File != "good.jar" {
		t.Errorf("installed %+v, want an enabled good.jar", e)
	}
	got, err := os.ReadFile(filepath.Join(dir, "good.jar"))
	if err != nil || !bytes.Equal(got, jarBytes("essentials")) {
		t.Fatalf("the installed jar is not what the server sent: %q, %v", got, err)
	}
	if names := dirNames(t, dir); len(names) != 1 {
		t.Errorf("the plugins directory holds %v; the temp file was not renamed away", names)
	}

	// A silent overwrite destroys the version that was working and is
	// indistinguishable from a fresh install afterwards.
	if _, err := mgr.InstallPlugin(s, origin.URL+"/good.jar"); err == nil {
		t.Error("installing over an existing plugin was allowed")
	}
}

// The routes have to be reachable and gated. A 404 here means RoutesPlugins is
// never called; a 200 means an anonymous caller can drive the panel.
func TestPluginRoutesAreWiredAndRequireASession(t *testing.T) {
	srv, mgr := newTestAgent(t)
	defer srv.Close()

	if _, err := mgr.auth.CreateUser("admin", "correct-horse-battery", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	id := mgr.List()[0].ID

	for _, r := range []struct{ method, path string }{
		{"GET", "/api/servers/" + id + "/plugins"},
		{"POST", "/api/servers/" + id + "/plugins/toggle"},
		{"DELETE", "/api/servers/" + id + "/plugins?file=x.jar"},
		{"POST", "/api/servers/" + id + "/plugins/install"},
	} {
		req, _ := http.NewRequest(r.method, srv.URL+r.path, strings.NewReader("{}"))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", r.method, r.path, err)
		}
		res.Body.Close()
		switch res.StatusCode {
		case http.StatusUnauthorized:
		case http.StatusNotFound:
			t.Errorf("%s %s returned 404: RoutesPlugins is not registered - Routes() must call a.RoutesPlugins(mux)",
				r.method, r.path)
		default:
			t.Errorf("%s %s returned %d to an anonymous caller, want 401", r.method, r.path, res.StatusCode)
		}
	}
}

// Every change here needs the server to come back for the loader to see it. The
// API has to say so, and it must not say so about a server that is not running.
func TestPluginChangesReportWhetherARestartIsNeeded(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := newPluginServer(t, mgr, "paper")
	pluginFixture(t, mgr, s, "plugins", map[string]string{"Vault.jar": "PK\x03\x04vault"})

	e, err := mgr.SetPluginEnabled(s, "Vault.jar", false)
	if err != nil {
		t.Fatal(err)
	}
	stopped := pluginResult(s, e)
	if stopped["requires_restart"] != false {
		t.Errorf("a stopped server was told to restart: %v", stopped)
	}
	if !strings.Contains(fmt.Sprint(stopped["note"]), "next time the server starts") {
		t.Errorf("stopped note is %q; it should say when the change lands", stopped["note"])
	}

	s.mu.Lock()
	s.Status = StatusRunning
	s.mu.Unlock()

	running := pluginResult(s, e)
	if running["requires_restart"] != true {
		t.Errorf("a running server was not told a restart is needed: %v", running)
	}
	if !strings.Contains(fmt.Sprint(running["note"]), "restart") {
		t.Errorf("running note is %q; it should name the restart", running["note"])
	}
}
