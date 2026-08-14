package arcade

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math/rand"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Runner executes a game server. Two implementations:
//
//   - dockerRunner: the real thing. `docker run -d`, output followed with
//     `docker logs -f`, commands delivered over RCON.
//   - simRunner: an in-process fake that emits a realistic Minecraft log and
//     answers console commands, so the panel is usable on a laptop without
//     pulling a 500 MB image first.
//
// Which one a server uses is recorded per-server and shown in the UI. Nothing
// pretends to be a container when it isn't.
type Runner interface {
	Start(s *Server, emit func(Line)) error
	Stop(s *Server) error
	Kill(s *Server) error
	Send(s *Server, cmd string) error
}

// ---------------------------------------------------------------- simulator

type simRunner struct{ mgr *Manager }

type simProc struct {
	cancel context.CancelFunc
	in     chan string
	done   chan struct{}
}

func (p *simProc) stop() { p.cancel() }

var simNames = []string{
	"RockDigger", "spicesw", "Thoughts3rased", "filliravaz", "AlxTray",
	"Leonito", "Felux137", "Fireblade115", "quartzfox", "mossyCobble",
}

func (r *simRunner) Start(s *Server, emit func(Line)) error {
	ctx, cancel := context.WithCancel(context.Background())
	p := &simProc{cancel: cancel, in: make(chan string, 16), done: make(chan struct{})}
	s.mu.Lock()
	s.proc = p
	s.players = nil
	s.mu.Unlock()

	go r.run(ctx, s, p, emit)
	return nil
}

func (r *simRunner) run(ctx context.Context, s *Server, p *simProc, emit func(Line)) {
	defer close(p.done)
	defer recoverPanic("simulator for " + s.ID)

	info := func(f string, a ...any) { emit(Line{Level: "info", Text: fmt.Sprintf(f, a...)}) }
	warn := func(f string, a ...any) { emit(Line{Level: "warn", Text: fmt.Sprintf(f, a...)}) }

	boot := []struct {
		d time.Duration
		f func()
	}{
		{250 * time.Millisecond, func() {
			warn(`[Pufferfish] To enable additional optimizations, add "--add-modules=jdk.incubator.vector" to your startup flags, BEFORE the "-jar".`)
		}},
		{120 * time.Millisecond, func() { info("Starting minecraft server version %s", s.Version) }},
		{140 * time.Millisecond, func() { info("Loading properties") }},
		{130 * time.Millisecond, func() { info("Default game type: %s", strings.ToUpper(s.Props["gamemode"])) }},
		{160 * time.Millisecond, func() { info("Generating keypair") }},
		{150 * time.Millisecond, func() { info("Starting Minecraft server on *:%d", s.Port) }},
		{140 * time.Millisecond, func() { info("Using default channel type") }},
		{200 * time.Millisecond, func() { info("Preparing level \"world\"") }},
		{260 * time.Millisecond, func() { info("Preparing start region for dimension minecraft:overworld") }},
		{180 * time.Millisecond, func() { info("Time elapsed: %d ms", 120+rand.Intn(120)) }},
		{200 * time.Millisecond, func() { info("Preparing start region for dimension minecraft:the_nether") }},
		{150 * time.Millisecond, func() { info("Time elapsed: %d ms", 60+rand.Intn(60)) }},
		{170 * time.Millisecond, func() { info("Preparing start region for dimension minecraft:the_end") }},
		{140 * time.Millisecond, func() { info("Time elapsed: %d ms", 40+rand.Intn(50)) }},
		{220 * time.Millisecond, func() { info("Running delayed init tasks") }},
	}

	start := time.Now()
	for _, step := range boot {
		select {
		case <-ctx.Done():
			return
		case <-time.After(step.d):
		}
		step.f()
	}

	emit(Line{Level: "info", Source: "ready",
		Text: fmt.Sprintf("Done (%.3fs)! For help, type \"help\"", time.Since(start).Seconds())})

	r.mgr.setStatus(s, StatusRunning, 0, "")

	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	slow := time.NewTicker(12 * time.Second)
	defer slow.Stop()

	baseMem := float64(s.MemoryMB) * 0.28

	for {
		select {
		case <-ctx.Done():
			return

		case cmd := <-p.in:
			r.handleCommand(s, cmd, emit)

		case <-tick.C:
			// resource jitter, bounded by the server's own limit
			s.mu.Lock()
			load := float64(len(s.players))*4.5 + 6 + rand.Float64()*9
			s.cpuPct = clamp(load, 0, 100)
			baseMem += (rand.Float64() - 0.35) * 24
			baseMem = clampF(baseMem, float64(s.MemoryMB)*0.18, float64(s.MemoryMB)*0.92)
			s.memMB = int(baseMem)
			s.mu.Unlock()

		case <-slow.C:
			r.ambient(s, emit)
		}
	}
}

// ambient produces the background chatter a live server generates: joins,
// leaves, chat, tick reports, the occasional warning.
func (r *simRunner) ambient(s *Server, emit func(Line)) {
	s.mu.Lock()
	n := len(s.players)
	s.mu.Unlock()

	switch {
	case n < 3 || rand.Intn(100) < 40:
		name := simNames[rand.Intn(len(simNames))]
		s.mu.Lock()
		dup := false
		for _, p := range s.players {
			if p.Name == name {
				dup = true
			}
		}
		if !dup && len(s.players) < s.MaxPlayers {
			s.players = append(s.players, Player{
				Name: name, UUID: fakeUUID(name),
				PingMS: 15 + rand.Intn(90), JoinedAt: time.Now().Unix(),
			})
		}
		s.mu.Unlock()
		if !dup {
			emit(Line{Level: "info", Source: "player", Text: fmt.Sprintf("UUID of player %s is %s", name, fakeUUID(name))})
			emit(Line{Level: "info", Source: "player", Text: fmt.Sprintf("%s joined the game", name)})
			r.mgr.pushPlayers(s)
		}

	case rand.Intn(100) < 25 && n > 0:
		s.mu.Lock()
		i := rand.Intn(len(s.players))
		name := s.players[i].Name
		s.players = append(s.players[:i], s.players[i+1:]...)
		s.mu.Unlock()
		emit(Line{Level: "info", Source: "player", Text: fmt.Sprintf("%s left the game", name)})
		r.mgr.pushPlayers(s)

	case rand.Intn(100) < 30 && n > 0:
		s.mu.Lock()
		name := s.players[rand.Intn(len(s.players))].Name
		s.mu.Unlock()
		chat := []string{
			"anyone got spare iron", "check the shulker by spawn", "brb food",
			"who broke the farm", "nether portal is up", "lag spike?",
			"gg", "im at bedrock", "trading hall done",
		}
		emit(Line{Level: "info", Text: fmt.Sprintf("<%s> %s", name, chat[rand.Intn(len(chat))])})

	case rand.Intn(100) < 12:
		emit(Line{Level: "warn", Text: fmt.Sprintf(
			"Can't keep up! Is the server overloaded? Running %dms or %d ticks behind",
			1200+rand.Intn(5000), 20+rand.Intn(90))})

	default:
		s.mu.Lock()
		cpu := s.cpuPct
		s.mu.Unlock()
		emit(Line{Level: "info", Text: fmt.Sprintf(
			"[spark] Tick monitor: average tick %.1fms over the last 60s", 18+cpu*0.6)})
	}
}

// handleCommand answers the console commands a Minecraft operator actually uses.
func (r *simRunner) handleCommand(s *Server, cmd string, emit func(Line)) {
	f := strings.Fields(cmd)
	if len(f) == 0 {
		return
	}
	switch strings.ToLower(f[0]) {
	case "help", "?":
		emit(Line{Text: "--- Showing help page 1 of 1 ---"})
		for _, h := range []string{
			"/list - list online players", "/say <msg> - broadcast a message",
			"/whitelist <add|remove> <player>", "/kick <player> [reason]",
			"/time set <day|night>", "/weather <clear|rain>", "/stop - stop the server",
		} {
			emit(Line{Text: h})
		}

	case "list":
		s.mu.Lock()
		names := make([]string, 0, len(s.players))
		for _, p := range s.players {
			names = append(names, p.Name)
		}
		max := s.MaxPlayers
		s.mu.Unlock()
		emit(Line{Text: fmt.Sprintf("There are %d of a max of %d players online: %s",
			len(names), max, strings.Join(names, ", "))})

	case "say":
		emit(Line{Text: fmt.Sprintf("[Server] %s", strings.Join(f[1:], " "))})

	case "whitelist":
		if len(f) >= 3 && f[1] == "add" {
			r.applyList(s, ListWhitelist, true, f[2], "", emit,
				fmt.Sprintf("Added %s to the whitelist", f[2]))
		} else if len(f) >= 3 && f[1] == "remove" {
			r.applyList(s, ListWhitelist, false, f[2], "", emit,
				fmt.Sprintf("Removed %s from the whitelist", f[2]))
		} else {
			entries, _ := r.mgr.readList(s, ListWhitelist)
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name)
			}
			if len(names) == 0 {
				emit(Line{Text: "There are no whitelisted players"})
			} else {
				emit(Line{Text: fmt.Sprintf("There are %d whitelisted players: %s",
					len(names), strings.Join(names, ", "))})
			}
		}

	case "op", "deop":
		if len(f) >= 2 {
			add := f[0] == "op"
			verb := "Made %s a server operator"
			if !add {
				verb = "Made %s no longer a server operator"
			}
			r.applyList(s, ListOps, add, f[1], "", emit, fmt.Sprintf(verb, f[1]))
		}

	case "ban", "pardon":
		if len(f) >= 2 {
			add := f[0] == "ban"
			reason := strings.Join(f[2:], " ")
			verb := "Banned %s: %s"
			msg := fmt.Sprintf(verb, f[1], orDefault(reason, "Banned by an operator"))
			if !add {
				msg = fmt.Sprintf("Unbanned %s", f[1])
			}
			r.applyList(s, ListBanned, add, f[1], reason, emit, msg)
			if add {
				r.mgr.trackPlayer(s, f[1], false)
			}
		}

	case "ban-ip", "pardon-ip":
		if len(f) >= 2 {
			add := f[0] == "ban-ip"
			msg := fmt.Sprintf("Banned IP %s", f[1])
			if !add {
				msg = fmt.Sprintf("Unbanned IP %s", f[1])
			}
			r.applyList(s, ListBannedIPs, add, f[1], strings.Join(f[2:], " "), emit, msg)
		}

	case "kick":
		if len(f) >= 2 {
			s.mu.Lock()
			for i, p := range s.players {
				if strings.EqualFold(p.Name, f[1]) {
					s.players = append(s.players[:i], s.players[i+1:]...)
					break
				}
			}
			s.mu.Unlock()
			emit(Line{Text: fmt.Sprintf("Kicked %s from the game", f[1])})
			emit(Line{Level: "info", Source: "player", Text: fmt.Sprintf("%s left the game", f[1])})
			r.mgr.pushPlayers(s)
		}

	case "time":
		if len(f) >= 3 {
			emit(Line{Text: fmt.Sprintf("Set the time to %s", f[2])})
		}

	case "weather":
		if len(f) >= 2 {
			emit(Line{Text: fmt.Sprintf("Set the weather to %s", f[1])})
		}

	case "stop":
		emit(Line{Text: "Stopping the server"})
		// Every one of these reaches hub.Publish, so a viewer disconnecting at
		// the wrong moment used to take the panel down from a bare `go`
		// statement with nobody left to recover it.
		go func() {
			defer recoverPanic("simulator stop for " + s.ID)
			_ = r.mgr.Stop(s.ID)
		}()

	case "flood":
		// Deliberate backpressure test: floods the room so the dropped-line
		// counter and the console's gap marker can be seen doing their job.
		emit(Line{Level: "warn", Text: "flooding console with 5000 lines"})
		go func() {
			defer recoverPanic("simulator flood for " + s.ID)
			for i := 1; i <= 5000; i++ {
				emit(Line{Text: fmt.Sprintf("[flood] line %d of 5000 - backpressure test", i)})
			}
		}()

	case "crash":
		// Forces the failure path so the failed state is reachable on demand.
		emit(Line{Level: "error", Text: "java.lang.OutOfMemoryError: Java heap space"})
		emit(Line{Level: "error", Text: "\tat net.minecraft.server.MinecraftServer.tickChildren(MinecraftServer.java:1579)"})
		go func() {
			defer recoverPanic("simulator crash for " + s.ID)
			r.mgr.fail(s, 137, "OOM")
		}()

	default:
		emit(Line{Level: "error", Text: fmt.Sprintf("Unknown or incomplete command, see below for error%s<--[HERE]", cmd)})
	}
}

// applyList makes the simulator's list commands change the same files a real
// server would, so the Players screen agrees with the console.
func (r *simRunner) applyList(s *Server, l PlayerList, add bool, who, reason string, emit func(Line), okMsg string) {
	var err error
	if add {
		err = r.mgr.applyListAdd(s, l, who, reason)
	} else {
		err = r.mgr.applyListRemove(s, l, who)
	}
	if err != nil {
		emit(Line{Level: "error", Text: err.Error()})
		return
	}
	emit(Line{Text: okMsg})
	r.mgr.broadcastEvent("players.changed", s.ID)
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func (r *simRunner) Stop(s *Server) error {
	s.mu.Lock()
	p, _ := s.proc.(*simProc)
	s.mu.Unlock()
	if p == nil {
		return nil
	}
	p.stop()
	return nil
}

func (r *simRunner) Kill(s *Server) error { return r.Stop(s) }

func (r *simRunner) Send(s *Server, cmd string) error {
	s.mu.Lock()
	p, _ := s.proc.(*simProc)
	s.mu.Unlock()
	if p == nil {
		return fmt.Errorf("server is not running")
	}
	select {
	case p.in <- cmd:
		return nil
	case <-time.After(time.Second):
		return fmt.Errorf("server did not accept the command")
	}
}

// ------------------------------------------------------------------- docker

// containerPrefix is the deploy identity, and it is deliberately NOT the
// product name.
//
// Renaming code is a ripgrep. Renaming things that already exist on a running
// box - containers, volumes, databases - is not: it means stopping servers,
// migrating data, and breaking anyone whose scripts reference the old names.
// So the marketing name is free to change (Teploy Arcade, Arcade, whatever) while
// this stays put forever. Same call as Akiroo's deploy identity, which kept
// `akiroo-lite` through a rename for exactly this reason.
const containerPrefix = "gamepanel"

type dockerRunner struct{ mgr *Manager }

// dockerProc is the panel's handle on a container it is watching - the
// `docker logs -f` child, not the container itself. Deliberately no stdin: the
// container is detached and the daemon owns it, so commands go over RCON.
type dockerProc struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	mu     sync.Mutex
}

func (p *dockerProc) stop() { p.cancel() }

var mcLevel = regexp.MustCompile(`^\[?\d{2}:\d{2}:\d{2}\]?\s*\[[^\]]*/(INFO|WARN|ERROR|FATAL)\]?`)

// jvmHeapMB sizes the Java heap *below* the container's memory limit.
//
// Setting -Xmx equal to the cgroup limit is a guaranteed kill: the JVM needs
// room beyond the heap for metaspace, thread stacks, code cache, direct byte
// buffers and GC structures. When the heap grows toward its max the process
// RSS crosses the limit and the kernel takes it - which it did here, during
// chunk generation on a 2 GB server.
//
// Reserve the larger of 512 MB or 25%, which is the rule of thumb the itzg
// image's own docs use.
func jvmHeapMB(limitMB int) int {
	reserve := limitMB / 4
	if reserve < 512 {
		reserve = 512
	}
	heap := limitMB - reserve
	if heap < 512 {
		heap = 512 // below this Minecraft will not start at all
	}
	return heap
}

// classify assigns a level in the agent, not the browser: every game formats
// differently, and putting the regex in the UI means a UI change per template.
func classify(text string) string {
	if m := mcLevel.FindStringSubmatch(text); m != nil {
		switch m[1] {
		case "WARN":
			return "warn"
		case "ERROR", "FATAL":
			return "error"
		}
		return "info"
	}
	up := strings.ToUpper(text)
	switch {
	case strings.Contains(up, "ERROR") || strings.Contains(up, "EXCEPTION") ||
		strings.HasPrefix(strings.TrimSpace(text), "at "):
		return "error"
	case strings.Contains(up, "WARN"):
		return "warn"
	}
	return "info"
}

var joinRe = regexp.MustCompile(`(\w{3,16}) (joined|left) the game`)

// dockerRunArgs builds the container command for a server. Extracted from Start
// so a test can assert on what the image is actually told: the interesting
// decisions (which jar, which JRE, which limits) all live in these arguments,
// and a test holding its own copy of them would drift.
// proxyFamily picks the config family itzg/bungeecord should apply.
//
// The importer records every proxy under the "velocity" template because that
// is the closest one the panel ships, so the template slug cannot answer this -
// a BungeeCord or Waterfall jar imported that way would be handed Velocity's
// config layout. The jar's own name is the honest source.
func proxyFamily(jar string) string {
	if strings.Contains(strings.ToLower(jar), "velocity") {
		return "velocity"
	}
	return "bungeecord"
}

// containerDataPath is where an image expects its server directory.
//
// Not universally /data. itzg/minecraft-server uses /data; itzg/bungeecord -
// the proxy image - uses /server. Mounting at the wrong path does not fail: the
// image simply starts from its own empty working directory instead, generates a
// default config there, and runs. A migrated Velocity therefore came up
// perfectly healthy on BungeeCord's default port with none of the operator's
// velocity.toml, forwarding.secret or plugins - the imported directory sat at
// /data, read by nobody, while the proxy ran out of an anonymous volume.
//
// Silent and total: the panel's file manager and backups all address the
// directory it mounted, so everything looks right from the panel while the
// running server shares none of it.
func containerDataPath(image string) string {
	if strings.HasPrefix(image, "itzg/bungeecord") {
		return "/server"
	}
	return "/data"
}

func dockerRunArgs(s *Server, name, mountPath, rconSecret string) []string {
	args := []string{
		// Detached, and deliberately NOT -i.
		//
		// An attached `docker run -i` makes the panel's pipe the container's
		// stdin, and a Minecraft console treats stdin EOF as "shut down". So
		// when the panel exited - an upgrade, a config change, a reboot - every
		// running server read EOF and stopped itself, cleanly and silently,
		// with exit code 0. A panel restart must not be a server outage.
		//
		// Detached means the daemon owns the container's lifetime. Console
		// input then goes over RCON, which is why it is forced on below.
		"run", "-d", "--rm", "--name", name,
		"-p", fmt.Sprintf("%d:%d", s.Port, s.Port),
		"-e", "EULA=TRUE",
		// Forced on rather than left to the image default, because an imported
		// server brings its own server.properties: a directory adopted from
		// another panel commonly has enable-rcon=false, and without this the
		// console would silently accept commands it could never deliver.
		//
		// The port is deliberately not published - RCON is reached only via
		// `docker exec`, so it is not exposed to the network at all, and the
		// password never has to leave the container.
		"-e", "ENABLE_RCON=true",
		"-e", "RCON_PASSWORD=" + rconSecret,
	}

	// An imported server runs the jar it arrived with. TYPE/VERSION tells the
	// image to fetch its own, which for a modpack means a loader build its mods
	// were not compiled against - the pack then fails to load and the panel has
	// silently replaced the server it was asked to move.
	//
	// Restricted to the Java-edition image: the proxy image takes a different
	// set of variables, and guessing at it would trade one silent substitution
	// for another.
	switch {
	case s.LaunchJar != "" && strings.HasPrefix(s.Image, "itzg/minecraft-server"):
		args = append(args,
			"-e", "TYPE=CUSTOM",
			"-e", "CUSTOM_SERVER="+containerDataPath(s.Image)+"/"+s.LaunchJar,
		)

	case s.LaunchJar != "" && strings.HasPrefix(s.Image, "itzg/bungeecord"):
		// The proxy image takes a different set of variables for the same idea.
		// It matters for the same reason: told only "velocity", it installed a
		// 4.1.0 snapshot, and a plugin the operator had been running for months
		// refused to load against it ("Your Velocity build version (#20) is not
		// supported"). Their own jar is the build their plugins were chosen for.
		args = append(args,
			"-e", "TYPE=CUSTOM",
			"-e", "CUSTOM_FAMILY="+proxyFamily(s.LaunchJar),
			"-e", "BUNGEE_JAR_FILE="+containerDataPath(s.Image)+"/"+s.LaunchJar,
		)

	default:
		args = append(args,
			"-e", "TYPE="+strings.ToUpper(s.Template),
			"-e", "VERSION="+s.Version,
		)
	}

	args = append(args,
		"-e", fmt.Sprintf("MEMORY=%dM", jvmHeapMB(s.MemoryMB)),
		"-e", fmt.Sprintf("MAX_PLAYERS=%d", s.MaxPlayers),
		"-e", "MOTD="+s.MOTD(),
		"-e", fmt.Sprintf("SERVER_PORT=%d", s.Port),
		// real limits, not advisory ones
		"--memory", fmt.Sprintf("%dm", s.MemoryMB),
		"--cpus", fmt.Sprintf("%.2f", s.CPU),
		// bind the panel's own directory, so the file manager and backups
		// see exactly what the container sees
		"-v", fmt.Sprintf("%s:%s", mountPath, containerDataPath(s.Image)),
		s.Image,
	)

	return args
}

func (r *dockerRunner) Start(s *Server, emit func(Line)) error {
	name := containerPrefix + "-" + s.ID
	// Only clear a *dead* leftover. Force-removing a running container here
	// would kill a live server with no graceful save - the panel calls this
	// "Start", so the operator would have no idea they just did that.
	if containerRunning(s.ID) {
		return fmt.Errorf("a container for this server is already running; the panel will re-attach to it")
	}
	_ = exec.Command("docker", "rm", "-f", name).Run()

	// The context is created only once this call is committed to starting a
	// container. Created above the guard, the early return walks away from a
	// cancel func nobody will ever call.
	ctx, cancel := context.WithCancel(context.Background())

	// Resolve symlinks before handing the path to docker. On macOS /tmp and
	// /var are symlinks into /private, and the daemon refuses the unresolved
	// form with "statfs ...: no such file or directory" - which is fatal for
	// the default data dir (/var/teploy-arcade) on a Mac.
	mountPath := r.mgr.serverDir(s)
	if resolved, err := filepath.EvalSymlinks(mountPath); err == nil {
		mountPath = resolved
	}
	// If the panel is itself in a container, the daemon resolves this path on
	// the host, not in here.
	mountPath = hostPathFor(mountPath)

	secret, err := randomHex(16)
	if err != nil {
		cancel()
		return fmt.Errorf("could not generate an RCON secret: %w", err)
	}
	args := dockerRunArgs(s, name, mountPath, secret)

	// `docker run -d` returns as soon as the container is created, so this call
	// owns nothing afterwards - which is the entire point.
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		cancel()
		return fmt.Errorf("docker run: %w: %s", err, strings.TrimSpace(string(out)))
	}

	s.mu.Lock()
	s.players = nil
	s.mu.Unlock()

	// From here the wiring is identical to re-attaching after a panel restart,
	// because the situation is identical: a container the daemon owns, that this
	// process is merely watching.
	cancel() // the run has finished; attach installs its own context
	return r.attach(s, emit, false)
}

// containerRunning reports whether this server's container is alive right now,
// regardless of what the panel's persisted state claims.
func containerRunning(id string) bool {
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}",
		containerPrefix+"-"+id).Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// Adopt re-attaches to a container that outlived the panel.
//
// Containers do not die when the panel does - a redeploy, a crash or a reboot
// leaves the game running. Without this the panel reports the server stopped,
// and pressing Start force-removes a live server with no graceful save.
//
// Output comes from `docker logs -f`, not `docker attach`: attach holds the
// container's stdin and does not exit cleanly, which hangs the panel on
// shutdown. Commands go over RCON - the same channel Crafty and Pterodactyl
// use, and the reason itzg images ship rcon-cli.
func (r *dockerRunner) Adopt(s *Server, emit func(Line)) error {
	if !containerRunning(s.ID) {
		return fmt.Errorf("no running container for %s", s.ID)
	}
	return r.attach(s, emit, true)
}

// attach follows a running container's output, watches for its exit and keeps
// its stats polled. Shared by Start and Adopt: once a container is detached,
// starting one and finding one already running differ only in how much history
// the console wants and whether the server is known to be ready.
func (r *dockerRunner) attach(s *Server, emit func(Line), adopted bool) error {
	ctx, cancel := context.WithCancel(context.Background())
	name := containerPrefix + "-" + s.ID

	logArgs := []string{"logs", "-f", name}
	if adopted {
		// Give the console some history back; the in-memory ring buffer died
		// with the previous process. On a fresh start there is no history to
		// recover and the container's whole log is new output.
		logArgs = []string{"logs", "-f", "--tail", "200", name}
	}
	cmd := exec.CommandContext(ctx, "docker", logArgs...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("docker logs: %w", err)
	}

	p := &dockerProc{cmd: cmd, cancel: cancel} // stdin nil: commands go via RCON
	s.mu.Lock()
	s.proc = p
	s.mu.Unlock()

	go func() {
		r.stream(s, stdout, emit, adopted)
		// `docker logs -f` is our own child process. Nothing else waits on it,
		// so without this it sits as a zombie until the panel exits - one per
		// adopted server.
		_ = cmd.Wait()
	}()
	go r.pollStats(ctx, s, name)
	go func() {
		// Nothing else cancels this context: the adopt path has no cmd.Wait
		// handler to do it, and until it happens pollStats keeps ticking every
		// 3s against a container that is gone, forever.
		defer cancel()
		r.watchExit(ctx, s, name)
	}()

	if adopted {
		r.mgr.setStatus(s, StatusRunning, 0, "")
		r.mgr.panelLine(s, "info",
			"Re-attached to a container that was already running - the panel restarted, this server never stopped.")
	}
	return nil
}

// watchExit reports the container stopping. `docker logs -f` exiting is not a
// reliable signal (it also ends if the daemon hiccups), so block on
// `docker wait`, which yields the real exit code.
func (r *dockerRunner) watchExit(ctx context.Context, s *Server, name string) {
	defer recoverPanic("exit watcher for " + s.ID)

	out, err := exec.CommandContext(ctx, "docker", "wait", name).Output()
	if err != nil {
		if ctx.Err() != nil {
			return // we tore it down ourselves; the kill is not a container failure
		}
		r.mgr.processExited(s, err)
		return
	}
	// An exit code that actually arrived is authoritative even if the context
	// has since been cancelled: Stop cancels right after `docker stop` returns,
	// and dropping the report in that window would strand the server in
	// "stopping" with nothing left to move it on.
	code := atoi(strings.TrimSpace(string(out)))
	if code == 0 {
		r.mgr.processExited(s, nil)
		return
	}
	reason := "exited"
	if code == 137 {
		reason = "OOM"
	}
	r.mgr.fail(s, code, reason)
}

func (r *dockerRunner) pollStats(ctx context.Context, s *Server, name string) {
	// Labelled with name rather than s.ID on purpose: a defer's argument is
	// evaluated where the defer is written, so building the label from s would
	// panic before the recover is installed - which is the very fault this line
	// exists to contain.
	defer recoverPanic("docker stats poller for " + name)

	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			out, err := exec.Command("docker", "stats", "--no-stream",
				"--format", "{{.CPUPerc}} {{.MemUsage}}", name).Output()
			if err != nil {
				continue
			}
			f := strings.Fields(string(out))
			if len(f) < 2 {
				continue
			}
			cpu := parseFloatPrefix(strings.TrimSuffix(f[0], "%"))
			mem := parseMem(f[1])
			s.mu.Lock()
			s.cpuPct = cpu
			s.memMB = mem
			s.mu.Unlock()
		}
	}
}

// stream turns container output into console lines. alreadyReady is set when
// adopting: the "Done" banner scrolled past before we attached.
func (r *dockerRunner) stream(s *Server, stdout io.Reader, emit func(Line), alreadyReady bool) {
	defer recoverPanic("docker stream for " + s.ID)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	ready := alreadyReady
	for sc.Scan() {
		text := sc.Text()
		src := "server"
		if m := joinRe.FindStringSubmatch(text); m != nil {
			src = "player"
			r.mgr.trackPlayer(s, m[1], m[2] == "joined")
		}
		// A proxy prints "Done (1.43s)!" with no help suffix, so the Minecraft
		// line never arrives and the panel reported "starting" forever - for a
		// server that was up and listening. Narrowed to proxies so the stricter
		// Minecraft match keeps its meaning everywhere else.
		if !ready && s.Game == "proxy" && strings.Contains(text, "Done (") && strings.Contains(text, ")!") {
			ready = true
			r.mgr.setStatus(s, StatusRunning, 0, "")
		}
		if !ready && strings.Contains(text, `For help, type "help"`) {
			ready = true
			r.mgr.setStatus(s, StatusRunning, 0, "")
		}
		emit(Line{Level: classify(text), Source: src, Text: text})
	}
}

func (r *dockerRunner) Stop(s *Server) error {
	name := containerPrefix + "-" + s.ID
	// graceful: let the game flush chunks before the container goes away
	_ = exec.Command("docker", "stop", "-t", "45", name).Run()
	r.cancelProc(s)
	return nil
}

func (r *dockerRunner) Kill(s *Server) error {
	err := exec.Command("docker", "kill", containerPrefix+"-"+s.ID).Run()
	r.cancelProc(s)
	return err
}

// cancelProc tears down the goroutines attached to this server's container, the
// way simRunner.Stop does for the simulator.
//
// It runs *after* the docker command, never before: the context kills our own
// `docker run` / `docker logs` client, and cancelling first would strand the
// container mid-shutdown with no flush.
func (r *dockerRunner) cancelProc(s *Server) {
	s.mu.Lock()
	p, _ := s.proc.(*dockerProc)
	s.mu.Unlock()
	if p != nil {
		p.stop()
	}
}

func (r *dockerRunner) Send(s *Server, cmd string) error {
	s.mu.Lock()
	p, _ := s.proc.(*dockerProc)
	s.mu.Unlock()
	if p == nil {
		return fmt.Errorf("server is not running")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Always RCON. Containers are started detached so that a panel restart does
	// not stop them, which means the panel never holds their stdin - there is no
	// longer a "normal" path and an "adopted" path, only this one.
	name := containerPrefix + "-" + s.ID
	out, err := exec.Command("docker", "exec", name, "rcon-cli", cmd).CombinedOutput()
	if err != nil {
		return fmt.Errorf("the command could not be delivered over RCON: %s",
			strings.TrimSpace(string(out)))
	}
	return nil
}

// ------------------------------------------------------------------ helpers

func clamp(v, lo, hi float64) float64 { return clampF(v, lo, hi) }
func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func parseFloatPrefix(s string) float64 {
	var whole, frac float64
	var fracDiv float64 = 1
	seenDot := false
	for _, c := range s {
		if c == '.' {
			seenDot = true
			continue
		}
		if c < '0' || c > '9' {
			break
		}
		if seenDot {
			fracDiv *= 10
			frac += float64(c-'0') / fracDiv
		} else {
			whole = whole*10 + float64(c-'0')
		}
	}
	return whole + frac
}

// parseMem turns docker's "1.234GiB / 4GiB" into megabytes.
func parseMem(s string) int {
	part := strings.SplitN(s, "/", 2)[0]
	part = strings.TrimSpace(part)
	v := parseFloatPrefix(part)
	up := strings.ToUpper(part)
	switch {
	case strings.Contains(up, "GIB"), strings.Contains(up, "GB"):
		return int(v * 1024)
	case strings.Contains(up, "KIB"), strings.Contains(up, "KB"):
		return int(v / 1024)
	}
	return int(v)
}

func fakeUUID(seed string) string {
	h := uint32(2166136261)
	for _, c := range seed {
		h = (h ^ uint32(c)) * 16777619
	}
	return fmt.Sprintf("%08x-%04x-4%03x-8%03x-%012x", h, h&0xffff, h&0xfff, (h>>4)&0xfff, uint64(h)*7919)
}

func dockerAvailable() bool {
	return exec.Command("docker", "info").Run() == nil
}

func imagePresent(image string) bool {
	out, err := exec.Command("docker", "images", "-q", image).Output()
	return err == nil && len(strings.TrimSpace(string(out))) > 0
}
