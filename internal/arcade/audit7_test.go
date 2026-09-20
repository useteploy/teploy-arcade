package arcade

// Regression tests for the ChatGPT audit pass 7 (AUDIT-CHATGPT-7.md).
// Every test here names its finding; the house pattern is one file per pass.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// ---------------------------------------------------------- R01: no-auth bind

func TestR01NoAuthRequiresLiteralLoopback(t *testing.T) {
	for _, host := range []string{"", "0.0.0.0", "::", "192.168.1.10", "example.com"} {
		if got, err := canonicalNoAuthHost(host); err == nil {
			t.Errorf("canonicalNoAuthHost(%q) = %q, want refusal", host, got)
		}
	}
	for _, host := range []string{"localhost", "127.0.0.1", "127.9.8.7", "::1"} {
		got, err := canonicalNoAuthHost(host)
		if err != nil {
			t.Errorf("canonicalNoAuthHost(%q): %v", host, err)
			continue
		}
		if got != "127.0.0.1" && got != "127.9.8.7" && got != "::1" {
			t.Errorf("canonicalNoAuthHost(%q) = %q, not a normalised loopback literal", host, got)
		}
	}
}

// ------------------------------------------------- R02: one-snapshot auth gate

// No observation may cross the gate's decision while the first-account
// transition runs concurrently. The gate reads enforcement and setup as one
// snapshot, so there is no interleaving between "no users" and "setup no
// longer required" - on an Auth that --no-auth never touched, the
// combination (disabled, setup complete) is unreachable.
func TestR02GateNeverStraddlesTheFirstAccountTransition(t *testing.T) {
	a := NewAuth(t.TempDir())
	if err := a.Load(); err != nil {
		t.Fatal(err)
	}
	if err := a.BeginSetup(); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = a.CreateFirstUser("race-admin", "password123")
				_ = a.DeleteUser("race-admin")
			}
		}
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		enabled, setupRequired := a.gateState()
		if !enabled && !setupRequired {
			t.Fatal("gate observed enabled=false with setup complete and no --no-auth: the transition was straddled")
		}
	}
	close(stop)
	wg.Wait()
}

// -------------------------------------------------- R03: credential bounds

func TestR03CreationEnforcesLoginBounds(t *testing.T) {
	a := NewAuth(t.TempDir())
	if err := a.Load(); err != nil {
		t.Fatal(err)
	}
	if err := checkNewUser("ok-name", strings.Repeat("p", 1025), RoleAdmin); err == nil {
		t.Error("an oversized password is accepted at creation but refused at login")
	}
	if err := checkNewUser(strings.Repeat("n", 129), "password123", RoleAdmin); err == nil {
		t.Error("an oversized name is accepted at creation but refused at login")
	}
	if _, err := a.CreateFirstUser("bound", strings.Repeat("p", 1025)); err == nil {
		t.Error("CreateFirstUser accepted a password the login route refuses")
	}
	if _, err := a.CreateFirstUser("bound", "password123"); err != nil {
		t.Fatal(err)
	}
	if err := a.SetPassword("bound", "password123", strings.Repeat("p", 1025), "", true); err == nil {
		t.Error("SetPassword accepted a password the login route refuses")
	}
}

// ------------------------------------------------------- R05: audit caps

func TestR05AuditFieldsAreByteCappedAndUTF8Safe(t *testing.T) {
	big := strings.Repeat("é", 6000) // multibyte: 12000 bytes
	got := auditText(big, 128)
	if len(got) > 128+len(" [truncated]") {
		t.Fatalf("capped field is %d bytes", len(got))
	}
	if !strings.HasSuffix(got, " [truncated]") {
		t.Error("truncation is not visible in the audit record")
	}
	if got := auditText("clean", 128); got != "clean" {
		t.Errorf("short field was modified: %q", got)
	}

	a := NewAuth(t.TempDir())
	if err := a.Load(); err != nil {
		t.Fatal(err)
	}
	a.Append(AuditEntry{Actor: "x", Action: "console.command", Target: "s1",
		Detail: strings.Repeat("d", 1<<20)})
	entries := a.Audit(1)
	if len(entries) != 1 {
		t.Fatal("entry not retained")
	}
	if len(entries[0].Detail) > 4096+len(" [truncated]") {
		t.Fatalf("a megabyte of detail was retained: %d bytes", len(entries[0].Detail))
	}
}

// ------------------------------------------------- R15: FIFO-safe dir opens

func TestR15ListingAFifoPathDoesNotBlock(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]
	dir, err := mgr.ensureServerDir(s)
	if err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "not-a-dir")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip("mkfifo unavailable:", err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := mgr.ListFiles(s, "not-a-dir")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a FIFO was listed as a directory")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the listing blocked on a FIFO - the open precedes the type check")
	}
}

// ------------------------------------------- R18: unreadable held originals

func TestR18UnreadableHeldOriginalsKeepTheStagingTree(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root every directory is readable; the failure needs a non-root read refusal")
	}
	mgr := NewManager(t.TempDir(), NewHub())
	if err := mgr.Load(); err != nil {
		t.Fatal(err)
	}
	s := mgr.List()[0]
	dir := mgr.serverDir(s)

	staging := filepath.Join(dir, ".arcade-restore-crashed")
	held := filepath.Join(staging, "old")
	if err := os.MkdirAll(held, 0o0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(held, "world.dat"), []byte("only-copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Unreadable, not empty: the read must fail and the staging tree must
	// survive it.
	if err := os.Chmod(held, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(held, 0o700) })

	mgr.recoverInterruptedRestores()

	if _, err := os.Stat(staging); err != nil {
		t.Fatalf("the staging tree was deleted while its held originals were unreadable: %v", err)
	}
	// Restore readability before asserting on the file inside it.
	if err := os.Chmod(held, 0o700); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(held, "world.dat")); err != nil || string(b) != "only-copy" {
		t.Fatalf("the held original did not survive: %q %v", b, err)
	}
}

// -------------------------------------------------- R22: temp sweep scope

func TestR22SweepKeepsUserPartFiles(t *testing.T) {
	dir := t.TempDir()
	mine := filepath.Join(dir, ".arcade-tmp-abcd")
	userExport := filepath.Join(dir, "user-export.tar.gz.part")
	for _, p := range []string{mine, userExport} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sweepTempFiles(dir)
	if _, err := os.Stat(mine); !errors.Is(err, os.ErrNotExist) {
		t.Error("a panel temp file survived the sweep")
	}
	if _, err := os.Stat(userExport); err != nil {
		t.Error("a user-named .tar.gz.part was deleted at boot: a suffix is not ownership")
	}
}

// --------------------------------------------- R25: strict managed identity

func TestR25ManagedPortRefusesAmbiguity(t *testing.T) {
	if _, _, err := propsPortValue("server-port=25565\nserver-port=bad\n"); err == nil {
		t.Error("an invalid FINAL server-port was accepted")
	}
	if _, _, err := propsPortValue("server-port=25565\nserver-port=25566\n"); err == nil {
		t.Error("duplicate server-port keys were accepted")
	}
	if _, _, err := propsPortValue("server-port:25566\n"); err == nil {
		t.Error("a colon separator was read as absence")
	}
	p, ok, err := propsPortValue("motd=x\nserver-port=25566\n")
	if err != nil || !ok || p != 25566 {
		t.Errorf("a clean file failed: %d %v %v", p, ok, err)
	}
	if _, ok, err := propsPortValue("motd=x\n"); err != nil || ok {
		t.Errorf("absence must be absence: %v %v", ok, err)
	}
}

// -------------------------------------------------- R31: path containment

func TestR31RootAndSiblingPrefixes(t *testing.T) {
	if !relativeWithin("/", "/var/teploy-arcade") {
		t.Error("the filesystem root was not recognised as an ancestor (old // prefix bug)")
	}
	if relativeWithin("/data", "/data-other/world") {
		t.Error("a sibling prefix was accepted as containment")
	}
	if !relativeWithin("/data", "/data/world") || !relativeWithin("/data", "/data") {
		t.Error("real containment was refused")
	}
	if relativeWithin("/data/world", "/data") {
		t.Error("a child was accepted as an ancestor")
	}
}

func TestR31ImportRefusesTheFilesystemRoot(t *testing.T) {
	mgr := NewManager(t.TempDir(), NewHub())
	if err := mgr.Load(); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.importSource("/"); err == nil {
		t.Fatal("importing / passed admission: the copy would include the destination and grow until the disk filled")
	}
}

// ------------------------------------------------------ R44: memory units

func TestR44ParseMemKnowsEveryDockerUnit(t *testing.T) {
	cases := map[string]int{
		"512MiB / 4GiB":   512,
		"1.5GiB / 4GiB":   1536,
		"2048KiB / 4GiB":  2,
		"2GiB / 4GiB":     2048,
		"1.0TB / 2TB":     1 << 20,
		"524288B / 4GiB":  0,
		"3145728B / 4GiB": 3,
	}
	for in, want := range cases {
		if got := parseMem(in); got != want {
			t.Errorf("parseMem(%q) = %d MB, want %d", in, got, want)
		}
	}
	if got := parseMem("12 parsecs / 4GiB"); got != 0 {
		t.Errorf("an unrecognised unit produced %d instead of refusing", got)
	}
}

// ------------------------------------------------------ R46: Java floor

func TestR46JavaMemoryFloor(t *testing.T) {
	if err := checkJavaMemory("itzg/minecraft-server", 512); err == nil {
		t.Error("a 512 MB Java container passed: the heap would eat the whole limit")
	}
	if err := checkJavaMemory("itzg/minecraft-server", 0); err != nil {
		t.Error("0 means template default and must not be refused here")
	}
	if err := checkJavaMemory("ryshe/terraria", 512); err != nil {
		t.Error("a native game binary does not need the JVM reserve")
	}
	if got := jvmHeapMB(512); got >= 512 {
		t.Errorf("jvmHeapMB(512) = %d: the heap still equals the whole container", got)
	}
	_, mgr := newTestAgent(t)
	if _, err := mgr.Create("Tiny Java", "paper", "1.20.4", 0, 512, 0, RuntimeSim); err == nil {
		t.Error("Create accepted a 512 MB Java server")
	}
}

// ------------------------------------------------ R47/R49: launch geometry

func TestR47RustLaunchInjectsAFreshRCONSecret(t *testing.T) {
	s := &Server{Game: "rust", Image: "didstopia/rust-server", Port: 28015,
		Protocols: []string{"udp"}, PortSpan: 2, MemoryMB: 8192, CPU: 4, Env: map[string]string{}}
	args := strings.Join(dockerRunArgs(s, "gamepanel-r", "/srv/r", "fresh-secret"), " ")
	if !strings.Contains(args, "RUST_RCON_PASSWORD=fresh-secret") {
		t.Error("the per-launch secret was not injected; the image's known default would survive")
	}
	if strings.Contains(args, ":28016/tcp") {
		t.Error("the RCON TCP port is still published to the network")
	}
	if !strings.Contains(args, "-p 28015:28015/udp") {
		t.Error("the game UDP port was not published")
	}
}

func TestR49BedrockDeclaresItsSecondListener(t *testing.T) {
	tpl := templateBySlug("bedrock")
	if tpl == nil || tpl.PortSpan != 2 {
		t.Fatal("the bedrock template must declare its two-listener span")
	}
	if _, err := candidateBindings(65535, tpl.Protocols, tpl.PortSpan, tpl.ExtraPorts); err == nil {
		t.Error("a base of 65535 with a two-port span must be refused at admission")
	}
	if _, err := candidateBindings(65534, tpl.Protocols, tpl.PortSpan, tpl.ExtraPorts); err != nil {
		t.Errorf("a base of 65534 must be accepted: %v", err)
	}
}

// ------------------------------------------------------ R51: catalog identity

func TestR51DuplicateSlugsAndBadGeometryAreRefused(t *testing.T) {
	// LoadTemplates reads <dataDir>/templates and seeds it from the built-ins;
	// the malformed customs are written in beside them.
	dataDir := t.TempDir()
	dir := templatesDir(dataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.json", `{"slug":"dup","name":"A","image":"e","versions":["1"],"memory_mb":1024,"cpu":1}`)
	write("b.json", `{"slug":"dup","name":"B","image":"e","versions":["1"],"memory_mb":1024,"cpu":1}`)
	write("badproto.json", `{"slug":"badproto","name":"P","image":"e","versions":["1"],"memory_mb":1024,"cpu":1,"protocols":["sctp"]}`)
	write("badspan.json", `{"slug":"badspan","name":"S","image":"e","versions":["1"],"memory_mb":1024,"cpu":1,"port_span":40}`)
	write("badpath.json", `{"slug":"badpath","name":"D","image":"e","versions":["1"],"memory_mb":1024,"cpu":1,"data_path":"relative/path"}`)

	err := LoadTemplates(dataDir)
	if err == nil || !strings.Contains(err.Error(), "duplicate template slug") {
		t.Fatalf("duplicate slugs were not reported: %v", err)
	}
	if templateBySlug("dup") == nil {
		t.Error("the surviving duplicate was dropped from the catalog")
	}
	for _, slug := range []string{"badproto", "badspan", "badpath"} {
		if templateBySlug(slug) != nil {
			t.Errorf("template %s passed geometry validation", slug)
		}
	}
}

// ------------------------------------------------------ R64: digest fail-closed

func TestR64MalformedDigestFailsClosed(t *testing.T) {
	for _, bad := range []string{"not-a-sha", "aa", strings.Repeat("g", 64), strings.Repeat("0", 63)} {
		if _, err := ExpectedSHA256(bad); err == nil {
			t.Errorf("ExpectedSHA256(%q) was accepted", bad)
		}
	}
	if b, err := ExpectedSHA256(strings.Repeat("a", 64)); err != nil || len(b) != 32 {
		t.Errorf("a valid digest was refused: %v", err)
	}
	if b, err := ExpectedSHA256("  "); err != nil || b != nil {
		t.Errorf("blank must mean optional, not invalid: %v", err)
	}
}

func TestR64InstallRejectsBadDigestBeforeDownloading(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	var fetched bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetched = true
		w.WriteHeader(200)
	}))
	defer srv.Close()

	_, err := mgr.InstallPlugin(context.Background(), s, srv.URL+"/x.jar", "zzz-not-hex")
	if err == nil {
		t.Fatal("a malformed digest was accepted")
	}
	if fetched {
		t.Error("the download started before the digest was validated")
	}
}

// -------------------------------------------------- R35: unknown blocks rm -f

func TestR35UnknownContainerStateRefusesBlindRemove(t *testing.T) {
	mgr := NewManager(t.TempDir(), NewHub())
	if err := mgr.Load(); err != nil {
		t.Fatal(err)
	}
	s := mgr.List()[0]
	s.mu.Lock()
	s.Runtime = RuntimeDocker
	s.mu.Unlock()

	old := containerState
	t.Cleanup(func() { containerState = old })
	containerState = func(string) ContainerState { return ContainerUnknown }

	// A daemon hiccup used to read as "not running" and authorised
	// `docker rm -f` on a live game; unknown must refuse the blind remove.
	r := &dockerRunner{mgr: mgr}
	if err := r.Start(s, func(Line) {}); err == nil {
		t.Fatal("Start proceeded against an unobservable container")
	} else if !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("Start failed for the wrong reason: %v", err)
	}
}

// ------------------------------------------------ R28: console vs backup gate

func TestR28ConsoleCommandsAreRefusedInsideABackupWindow(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]
	if err := mgr.Start(s.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool { return s.State() == StatusRunning })

	// Hold the exclusive filesystem gate the way CreateBackup, RestoreBackup
	// and the clone worker do.
	s.fsMu.Lock()
	err := mgr.Send(s.ID, "save-on", "command", "test")
	s.fsMu.Unlock()
	if err == nil {
		t.Fatal("an external console command crossed the snapshot gate")
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	// Outside the window it goes through again.
	if err := mgr.Send(s.ID, "list", "command", "test"); err != nil {
		t.Fatalf("the gate did not release after the backup window: %v", err)
	}
}

// ---------------------------------------------------------- R09: MCP shape

func TestR09MCPRejectsTrailingDataAndBadIDs(t *testing.T) {
	srv, mgr := newTestAgent(t)
	defer srv.Close()
	tok, err := mgr.mcp.Issue("shape")
	if err != nil {
		t.Fatal(err)
	}

	post := func(body string) (int, string) {
		req, _ := http.NewRequest("POST", srv.URL+"/api/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		buf := make([]byte, 4096)
		n, _ := res.Body.Read(buf)
		return res.StatusCode, string(buf[:n])
	}

	// Two concatenated objects is one request too many.
	if _, body := post(`{"jsonrpc":"2.0","id":1,"method":"ping"}{"jsonrpc":"2.0","id":2,"method":"ping"}`); !strings.Contains(body, "exactly one JSON-RPC request") {
		t.Errorf("concatenated requests were not refused: %s", body)
	}
	// MCP forbids null IDs; a notification never gets a response body.
	if code, body := post(`{"jsonrpc":"2.0","id":null,"method":"ping"}`); code != http.StatusAccepted || body != "" {
		t.Errorf("null ID: status %d body %q, want 202 with no body", code, body)
	}
	// Fractional IDs are refused with the standard invalid-request code.
	if _, body := post(`{"jsonrpc":"2.0","id":1.5,"method":"ping"}`); !strings.Contains(body, "id must be a string or an integer") {
		t.Errorf("a fractional ID was accepted: %s", body)
	}
	// A real request still works end to end.
	if code, body := post(`{"jsonrpc":"2.0","id":7,"method":"ping"}`); code != 200 || strings.Contains(body, "error") {
		t.Errorf("a valid request failed: %d %s", code, body)
	}
}
