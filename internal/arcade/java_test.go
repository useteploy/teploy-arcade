package arcade

import (
	"strings"
	"testing"
	"time"
)

// The boundaries here are Mojang's. Getting one wrong is not a cosmetic bug: a
// server on too new a JRE fails inside the game's own log, where the panel
// cannot explain it, and the operator is left reading a Java stack trace about
// an unsupported class file version.
func TestJavaTagFollowsTheMinecraftVersion(t *testing.T) {
	cases := []struct{ version, want string }{
		// Java 8 era - everything up to and including 1.16.5.
		{"1.7.10", imageJava8},
		{"1.12.2", imageJava8}, // RL Craft
		{"1.16.5", imageJava8}, // Pixelmon
		// 1.17 moved the floor.
		{"1.17", imageJava17},
		{"1.17.1", imageJava17},
		{"1.19.4", imageJava17},
		{"1.20", imageJava17},
		{"1.20.4", imageJava17},
		// 1.20.5 moved it again.
		{"1.20.5", imageJava21},
		{"1.20.6", imageJava21},
		{"1.21", imageJava21},
		{"1.21.1", imageJava21}, // Cobblemon
		// Suffixed versions still resolve.
		{"1.20.4-pre1", imageJava17},
		{"1.21.1+build.3", imageJava21},
	}
	for _, c := range cases {
		got, ok := javaTagFor(c.version)
		if !ok {
			t.Errorf("javaTagFor(%q) could not resolve a tag", c.version)
			continue
		}
		if got != c.want {
			t.Errorf("javaTagFor(%q) = %q, want %q", c.version, got, c.want)
		}
	}
}

// An unreadable version keeps the default rather than guessing. Guessing an old
// JRE to protect old servers would break every new one.
func TestUnreadableVersionsKeepTheDefaultImage(t *testing.T) {
	for _, v := range []string{"", "garbage", "latest", "2.0", "snapshot-24w14a"} {
		if got := imageForVersion("itzg/minecraft-server", v); got != "itzg/minecraft-server" {
			t.Errorf("version %q produced %q; an unreadable version must not pick a JRE", v, got)
		}
	}
}

// An image the operator pinned themselves is never rewritten.
func TestAnExplicitImageIsLeftAlone(t *testing.T) {
	for _, img := range []string{
		"itzg/minecraft-server:java8",
		"itzg/minecraft-server:2024.1.0",
		"ghcr.io/example/custom-mc:v3",
		"itzg/bungeecord",
	} {
		if got := imageForVersion(img, "1.20.4"); got != img {
			t.Errorf("imageForVersion rewrote an explicit image %q to %q", img, got)
		}
	}
}

// The wiring that matters: a server created at an old version must actually
// carry the old-JRE image, not just have a helper that could compute one.
func TestOldServersAreCreatedOnAJavaEightImage(t *testing.T) {
	_, mgr := newTestAgent(t)

	old, err := mgr.Create("legacy pack", "forge", "1.12.2", 0, 2048, 2, RuntimeSim)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if old.Image != "itzg/minecraft-server:"+imageJava8 {
		t.Errorf("a 1.12.2 server got image %q; it cannot start on a modern JRE", old.Image)
	}

	modern, err := mgr.Create("current", "paper", "1.21.1", 0, 2048, 2, RuntimeSim)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if modern.Image != "itzg/minecraft-server:"+imageJava21 {
		t.Errorf("a 1.21.1 server got image %q", modern.Image)
	}

	// The proxy template uses a different image entirely and must be untouched.
	proxy, err := mgr.Create("proxy", "velocity", "3.3.0", 0, 1024, 1, RuntimeSim)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if proxy.Image != "itzg/bungeecord" {
		t.Errorf("the velocity image was rewritten to %q", proxy.Image)
	}
}

// An imported server must run the jar it arrived with. This asserts on the
// container command itself, because the whole point is what the image is told -
// a test that only checked the field would pass with the runner ignoring it.
func TestAnImportedServerRunsItsOwnJar(t *testing.T) {
	_, mgr := newImportAgent(t)
	dir := mkTree(t, "rlcraft", map[string]string{
		"forge-1.12.2-14.23.5.2860.jar": "PK\x03\x04 forge",
		"server.properties":             "server-port=25572\nmotd=RL Craft\n",
		"world/level.dat":               "level",
	})

	job, err := mgr.StartImport(ImportRequest{
		Path: dir, Name: "RL Craft", Mode: ImportCopy, Runtime: RuntimeDocker,
	}, "tester")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	waitImport(t, job.ID, 20*time.Second)

	var s *Server
	for _, c := range mgr.List() {
		if c.Name == "RL Craft" {
			s = c
		}
	}
	if s == nil {
		t.Fatal("the imported server is not in the panel")
	}
	if s.LaunchJar != "forge-1.12.2-14.23.5.2860.jar" {
		t.Errorf("LaunchJar = %q; the pinned loader build was lost", s.LaunchJar)
	}
	if s.Image != "itzg/minecraft-server:"+imageJava8 {
		t.Errorf("image = %q; a 1.12.2 pack cannot start on a modern JRE", s.Image)
	}

	args := dockerRunArgs(s, "test-container", "/srv/data", "test-rcon-secret")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "TYPE=CUSTOM") ||
		!strings.Contains(joined, "CUSTOM_SERVER=/data/forge-1.12.2-14.23.5.2860.jar") {
		t.Errorf("the container was not told to run the imported jar:\n%s", joined)
	}
	if strings.Contains(joined, "VERSION=") {
		t.Errorf("VERSION was still passed, so the image will fetch its own jar:\n%s", joined)
	}
}

// A panel restart must not be a server outage.
//
// Containers were started with `docker run --rm -i`, which makes the panel's
// pipe the container's stdin. A Minecraft console treats stdin EOF as "shut
// down", so when the panel exited - an upgrade, a config change, a reboot -
// every running server stopped itself cleanly, exit code 0, with nothing in the
// panel to explain it. Found on the first real deploy, not by this suite.
//
// This asserts on the container command, which is where the decision lives.
func TestContainersAreStartedDetachedSoTheyOutliveThePanel(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]
	s.MemoryMB, s.CPU = 2048, 2

	args := dockerRunArgs(s, "gamepanel-test", "/srv/data", "secret123")
	joined := strings.Join(args, " ")

	if !strings.Contains(joined, " -d ") && args[1] != "-d" {
		t.Errorf("container is not detached; a panel restart will stop it:\n%s", joined)
	}
	// -i is what tied the container's stdin to the panel in the first place.
	for _, a := range args {
		if a == "-i" || a == "--interactive" {
			t.Errorf("container still takes the panel's stdin (%q); stdin EOF on panel exit stops the server", a)
		}
	}
	// Detached means commands can only go over RCON, so RCON must be forced on:
	// an imported server.properties commonly carries enable-rcon=false, and the
	// console would then accept commands it could never deliver.
	if !strings.Contains(joined, "ENABLE_RCON=true") {
		t.Errorf("RCON is not enabled, so a detached container has no command channel:\n%s", joined)
	}
	if !strings.Contains(joined, "RCON_PASSWORD=secret123") {
		t.Errorf("no RCON password was set:\n%s", joined)
	}
	// The RCON port must NOT be published: it is reached via `docker exec`, and
	// publishing it would put a remote-console port on the network.
	if strings.Contains(joined, "25575:") {
		t.Errorf("the RCON port is published to the network:\n%s", joined)
	}
}

// The proxy image expects its server directory at /server, not /data. Mounting
// at the wrong path does not fail loudly - the image starts from its own empty
// working directory, writes a default config there and runs. A migrated
// Velocity came up healthy on BungeeCord's default port with none of the
// operator's velocity.toml, forwarding secret or plugins, while the imported
// directory sat unread at /data.
func TestProxyImagesGetTheirDirectoryAtTheRightPath(t *testing.T) {
	cases := []struct {
		image string
		want  string
	}{
		{"itzg/bungeecord", "/server"},
		{"itzg/minecraft-server", "/data"},
		{"itzg/minecraft-server:java8", "/data"},
		{"ghcr.io/example/custom", "/data"},
	}
	for _, c := range cases {
		if got := containerDataPath(c.image); got != c.want {
			t.Errorf("containerDataPath(%q) = %q, want %q", c.image, got, c.want)
		}
	}

	// And it must actually reach the container command.
	_, mgr := newTestAgent(t)
	proxy, err := mgr.Create("edge", "velocity", "3.3.0", 0, 1024, 1, RuntimeSim)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	args := strings.Join(dockerRunArgs(proxy, "gamepanel-x", "/srv/data", "sec"), " ")
	if !strings.Contains(args, "/srv/data:/server") {
		t.Errorf("proxy directory not mounted at /server:\n%s", args)
	}
	if strings.Contains(args, "/srv/data:/data") {
		t.Errorf("proxy directory mounted at /data, where the image will not read it:\n%s", args)
	}

	game, err := mgr.Create("world", "paper", "1.20.4", 0, 2048, 2, RuntimeSim)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	gargs := strings.Join(dockerRunArgs(game, "gamepanel-y", "/srv/data", "sec"), " ")
	if !strings.Contains(gargs, "/srv/data:/data") {
		t.Errorf("game server directory should still mount at /data:\n%s", gargs)
	}
}

// A proxy must run the jar it arrived with, for the same reason a modpack does.
// Told only "velocity", the image installed a 4.1.0 snapshot and velocitab
// refused to load against it - a plugin that had been running for months.
func TestAnImportedProxyRunsItsOwnJar(t *testing.T) {
	_, mgr := newTestAgent(t)
	p, err := mgr.Create("edge", "velocity", "3.3.0", 0, 1024, 1, RuntimeSim)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	p.LaunchJar = "velocity.jar"

	args := strings.Join(dockerRunArgs(p, "gamepanel-x", "/srv/data", "sec"), " ")
	if !strings.Contains(args, "TYPE=CUSTOM") {
		t.Errorf("proxy not told to run a custom jar:\n%s", args)
	}
	if !strings.Contains(args, "BUNGEE_JAR_FILE=/server/velocity.jar") {
		t.Errorf("proxy jar path wrong (must be under /server):\n%s", args)
	}
	if !strings.Contains(args, "CUSTOM_FAMILY=velocity") {
		t.Errorf("proxy family not set:\n%s", args)
	}
	if strings.Contains(args, "VERSION=") {
		t.Errorf("VERSION still passed, so the image will fetch its own jar:\n%s", args)
	}
	// The minecraft-server variable must NOT be used here; the proxy image
	// ignores it, which is how this went unnoticed.
	if strings.Contains(args, "CUSTOM_SERVER=") {
		t.Errorf("proxy given the game-server variable, which it ignores:\n%s", args)
	}

	// Every proxy imports as the "velocity" template, so the family has to come
	// from the jar or a BungeeCord install gets Velocity's config layout.
	for jar, want := range map[string]string{
		"velocity.jar":       "velocity",
		"waterfall-1.19.jar": "bungeecord",
		"BungeeCord.jar":     "bungeecord",
	} {
		if got := proxyFamily(jar); got != want {
			t.Errorf("proxyFamily(%q) = %q, want %q", jar, got, want)
		}
	}
}
