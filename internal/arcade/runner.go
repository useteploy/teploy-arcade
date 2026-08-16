package arcade

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
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

// replaySettle is how long the adopt path waits for the 200-line tail to finish
// replaying before asking the game who is really on.
const replaySettle = 3 * time.Second

// RCON connection churn. Every query the panel makes - and every command an
// operator types, since those go the same way - opens a connection, runs, and
// closes, and the game logs all three steps:
//
//	Thread RCON Client /0:0:0:0:0:0:0:1 started
//	[Essentials] Rcon issued server command: /list
//	Thread RCON Client /0:0:0:0:0:0:0:1 shutting down
//
// The two thread lines describe the transport, never the server, and are noise
// to whoever is reading the console no matter who caused them. They are dropped
// from the stream. The middle line is kept when an operator typed the command -
// that is the echo of their own action - and dropped when the panel asked on
// its own behalf, which is what queryQuiet marks.
// afterLogPrefix strips a "[HH:MM:SS INFO]: " style prefix so a pattern can be
// anchored on what the server actually said.
func afterLogPrefix(line string) string {
	if i := strings.Index(line, "]: "); i >= 0 {
		return strings.TrimSpace(line[i+3:])
	}
	return strings.TrimSpace(line)
}

var rconThreadRe = regexp.MustCompile(`^Thread RCON Client .* (started|shutting down)$`)

var rconIssuedRe = regexp.MustCompile(`Rcon issued server command:`)

var joinRe = regexp.MustCompile(`(\w{3,16}) (joined|left) the game`)

// "X joined the game" is a chat broadcast, and a plugin can cancel it.
//
// The deployed Lobby does exactly that. Its log carries the arrival:
//
//	[03:17:58 INFO]: UUID of player Steve_Example is 00000000-...
//	[03:18:06 INFO]: Steve_Example[/192.168.1.160:46714] logged in with entity id 9 at (...)
//
// and then, on the way out, "Steve_Example left the game" - so the panel could
// only ever see people leave. A player standing in that world was invisible to
// the sidebar from the moment they arrived until the moment they quit, and the
// removal for someone never added is a silent no-op, so nothing looked wrong.
//
// These two lines are written by the server itself rather than broadcast to
// chat, so no plugin suppresses them and no resource pack translates them. They
// are matched in addition to the broadcast, never instead of it: trackPlayer
// dedupes an arrival by name and a removal for an absent player does nothing,
// so a server that prints both is counted once.
var (
	loginRe    = regexp.MustCompile(`(\w{3,16})\[/[^\]]*\] logged in with entity id`)
	lostConnRe = regexp.MustCompile(`(\w{3,16}) lost connection:`)
)

// A proxy does not say "joined the game" - it never runs a world, so nobody
// joins one. Velocity says this instead:
//
//	[connected player] Someone (/192.168.1.160:50545) has connected
//	[connected player] Someone (/192.168.1.160:50545) has disconnected
//
// The proxy is the front door: every player on the network connects to it, and
// most of them never touch a backend's console. So the one server an operator
// watches to see who is on was the one server that tracked nobody, and its
// Players sidebar sat empty through a full session.
//
// Anchored on the "[connected player]" tag deliberately. The same log also
// carries
//
//	[server connection] Someone -> Lobby has connected
//
// which is the player being handed to a backend, not a second player arriving.
// Matching loosely counts one player twice, and counts them out again when they
// hop between backends.
var proxyJoinRe = regexp.MustCompile(`\[connected player\] (\w{3,16}) \([^)]*\) has (connected|disconnected)`)

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

// serverDataPath is containerDataPath with the template's own answer preferred.
//
// The image-name rule only ever knew about itzg's two layouts. ryshe/terraria
// keeps worlds in /root/.local/share/Terraria/Worlds, and a bind mount landing
// on /data instead does not fail - the image starts from its own empty
// directory, generates a world there, and runs. The panel's file manager and
// backups then address a directory the running server shares nothing with,
// which is exactly the silent, total failure the proxy's /server mount was
// found to cause.
func serverDataPath(s *Server) string {
	if s.DataPath != "" {
		return s.DataPath
	}
	return containerDataPath(s.Image)
}

// usesItzgConventions reports whether the image takes the Minecraft images'
// environment: EULA, TYPE, VERSION, MEMORY, MAX_PLAYERS, MOTD, SERVER_PORT.
//
// Every one of those was sent to every container unconditionally. They are
// harmless noise to another image, but they are also the only way the panel had
// of telling a server its port or its player cap - so a non-itzg game got none
// of its settings, and the panel had no way to express them.
func usesItzgConventions(image string) bool { return strings.HasPrefix(image, "itzg/") }

// expandVars substitutes the panel's own settings into a template's env and
// args, so a template can wire them into whatever names its image expects
// rather than the runner having to know them.
func expandVars(s *Server, in string) string {
	r := strings.NewReplacer(
		"${PORT}", itoa(s.Port),
		// Bedrock is why this exists: its IPv6 listener needs a port of its own,
		// and the only sensible one to give it is the next.
		"${PORT_PLUS_1}", itoa(s.Port+1),
		"${MEMORY_MB}", itoa(s.MemoryMB),
		"${MAX_PLAYERS}", itoa(s.MaxPlayers),
		"${MOTD}", s.MOTD(),
		"${DATA}", serverDataPath(s),
	)
	return r.Replace(in)
}

// templateLaunchArgs is the env and command line for an image that describes
// its own launch. Sorted so the command a test asserts on, and the one an
// operator reads in `docker inspect`, are stable rather than map-ordered.
func templateLaunchArgs(s *Server) (env []string, cmd []string) {
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, "-e", k+"="+expandVars(s, s.Env[k]))
	}
	for _, a := range s.Args {
		cmd = append(cmd, expandVars(s, a))
	}
	return env, cmd
}

// publishArgs builds the -p flags for a server.
//
// This was one hardcoded `-p port:port`, which is TCP, which is correct for
// every Java-edition server and the proxy and for nothing else the panel ships.
// Bedrock, Rust and Valheim all speak UDP: they installed, booted, went
// "healthy", and no client could reach them - the panel was publishing a TCP
// port nothing was listening on. Nothing looked wrong from the panel side,
// which is why it survived a template audit.
//
// Protocols and span come off the server, which copied them from its template,
// so adding a game with a different transport stays a data change.
func publishArgs(s *Server, dir string) []string {
	protos := s.Protocols
	if len(protos) == 0 {
		protos = []string{"tcp"}
	}
	span := s.PortSpan
	if span < 1 {
		span = 1
	}
	// A span bounded so a bad template cannot ask the daemon for ten thousand
	// port bindings.
	if span > 16 {
		span = 16
	}
	out := make([]string, 0, len(protos)*span*2)
	seen := map[string]bool{}
	add := func(port int, proto string) {
		key := fmt.Sprintf("%d/%s", port, proto)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, "-p", fmt.Sprintf("%d:%d/%s", port, port, proto))
	}
	for i := 0; i < span; i++ {
		for _, proto := range protos {
			add(s.Port+i, proto)
		}
	}
	// Fixed listeners that have nothing to do with the main port - Geyser's
	// Bedrock socket is the case this exists for. Bad entries are skipped
	// rather than failing the start: a typo in a template must not be the
	// reason a working server will not come up.
	for _, e := range s.ExtraPorts {
		port, proto, ok := parseExtraPort(e)
		if !ok {
			log.Printf("%s: ignoring unparseable extra_ports entry %q", s.Name, e)
			continue
		}
		add(port, proto)
	}
	// Geyser is a plugin and its port belongs to the server that has it
	// installed, whichever that is - a proxy or a Paper server. See geyserPort.
	if port, ok := geyserPort(dir); ok {
		add(port, "udp")
	}
	return out
}

// geyserPort finds the Bedrock port of a Geyser install, if there is one.
//
// Geyser is a plugin, not a server, and it can sit on a proxy or on a Paper
// server. Its port therefore belongs to whatever has the plugin installed, not
// to a template - putting 19132 in the Velocity template would have every proxy
// claim a Bedrock port whether or not it runs Geyser, and made a second proxy
// on the host fail over a plugin it does not have.
//
// So the port follows the plugin. The plugins directory is on disk and readable
// before the container starts, which is exactly when the publish flags are
// decided.
//
// Reading its config rather than assuming 19132: the port is configurable, and
// a panel that published the default while Geyser listened elsewhere would
// reproduce the original bug with more steps. The default is only used when the
// plugin is clearly present but its config is not readable yet - which is the
// normal state on the very first boot, before Geyser has written one.
func geyserPort(dir string) (int, bool) {
	if dir == "" {
		return 0, false
	}
	plugins := filepath.Join(dir, "plugins")
	ents, err := os.ReadDir(plugins)
	if err != nil {
		return 0, false
	}
	found := false
	var cfg string
	for _, e := range ents {
		name := strings.ToLower(e.Name())
		if !strings.HasPrefix(name, "geyser") {
			continue
		}
		found = true
		if e.IsDir() {
			cfg = filepath.Join(plugins, e.Name(), "config.yml")
		}
	}
	if !found {
		return 0, false
	}
	if cfg == "" {
		return geyserDefaultPort, true
	}
	b, err := os.ReadFile(cfg)
	if err != nil {
		return geyserDefaultPort, true
	}
	// A deliberately small YAML read: find the `bedrock:` block and take the
	// first `port:` indented under it. Pulling in a YAML parser to read one
	// integer out of one optional file is not a trade worth making, and a
	// mis-read falls back to the default rather than failing a start.
	inBedrock := false
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || trimmed == "" {
			continue
		}
		indented := line != strings.TrimLeft(line, " \t")
		if !indented {
			inBedrock = strings.HasPrefix(trimmed, "bedrock:")
			continue
		}
		if !inBedrock {
			continue
		}
		if rest, ok := strings.CutPrefix(trimmed, "port:"); ok {
			if p := atoi(strings.TrimSpace(rest)); p > 0 && p <= 65535 {
				return p, true
			}
			break
		}
	}
	return geyserDefaultPort, true
}

const geyserDefaultPort = 19132

// parseExtraPort reads "19132/udp". The protocol may be omitted and defaults to
// tcp, matching docker's own -p behaviour.
func parseExtraPort(spec string) (int, string, bool) {
	spec = strings.TrimSpace(spec)
	proto := "tcp"
	if i := strings.LastIndex(spec, "/"); i >= 0 {
		proto = strings.ToLower(strings.TrimSpace(spec[i+1:]))
		spec = spec[:i]
	}
	if proto != "tcp" && proto != "udp" {
		return 0, "", false
	}
	port := atoi(strings.TrimSpace(spec))
	if port < 1 || port > 65535 {
		return 0, "", false
	}
	return port, proto, true
}

// isReady reports whether a console line says this server is accepting players.
//
// Three banners, in the order they were needed. The Java one has always been
// here; the proxy prints "Done (1.43s)!" with no help suffix, so a Velocity that
// was up and listening showed "starting" forever until that was added. A
// template can now name its own, which is what every non-Java game needs -
// they are judged by Java's banner otherwise, never print it, and stay
// "starting" for their whole uptime.
func isReady(s *Server, text string) bool {
	if s.ReadyLog != "" && strings.Contains(text, s.ReadyLog) {
		return true
	}
	if s.Game == "proxy" && strings.Contains(text, "Done (") && strings.Contains(text, ")!") {
		return true
	}
	return strings.Contains(text, `For help, type "help"`)
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
	}
	args = append(args, publishArgs(s, mountPath)...)

	// An image that describes its own launch gets that and nothing else. The
	// Minecraft variables below are not neutral for it - EULA and MEMORY mean
	// nothing to Terraria, and SERVER_PORT would be the panel claiming to have
	// set a port it has not.
	if !usesItzgConventions(s.Image) {
		env, cmd := templateLaunchArgs(s)
		args = append(args, env...)
		args = append(args,
			"--memory", fmt.Sprintf("%dm", s.MemoryMB),
			"--cpus", fmt.Sprintf("%.2f", s.CPU),
			"-v", fmt.Sprintf("%s:%s", mountPath, serverDataPath(s)),
			s.Image,
		)
		return append(args, cmd...)
	}

	args = append(args,
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
		"-e", "RCON_PASSWORD="+rconSecret,
	)

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
		"-v", fmt.Sprintf("%s:%s", mountPath, serverDataPath(s)),
	)

	// A template may add to the standard block, or override part of it - later
	// -e wins in docker. Bedrock needs this: the panel sets SERVER_PORT, and the
	// image's IPv6 listener defaults to 19133 regardless, so the moment a
	// Bedrock server lands on 19133 - which is exactly where NextFreePort puts
	// the second one - the two collide inside the container and the game exits
	// with "Port [19133] may be in use by another process". Proven on the host:
	// SERVER_PORT=19133 alone exits; adding SERVER_PORT_V6=19134 boots and
	// reports "IPv4 supported, port: 19133 / IPv6 supported, port: 19134".
	env, _ := templateLaunchArgs(s)
	args = append(args, env...)

	args = append(args, s.Image)
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
var containerRunning = func(id string) bool {
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

	if !adopted {
		go r.watchReady(ctx, s, name)
	}

	if adopted {
		r.mgr.setStatus(s, StatusRunning, 0, "")
		r.mgr.panelLine(s, "info",
			"Re-attached to a container that was already running - the panel restarted, this server never stopped.")

		// Who is online is in-memory state, and the panel just lost it. The
		// tail replays the last 200 lines, which rebuilds recent arrivals and
		// nothing older - so a player who joined an hour before the restart
		// disappears from the sidebar while still standing in the world, and
		// one whose join replayed but whose departure scrolled past appears
		// though they left. Ask the game instead. Backgrounded: it shells into
		// the container, and adoption should not wait on it.
		//
		// After a settle, not immediately: the tail replay is running in
		// another goroutine and re-adds whoever it sees, so a reconcile that
		// wins the race gets overwritten by history it was there to correct.
		// The game's answer has to be the last word, not the first.
		go func() {
			time.Sleep(replaySettle)
			r.mgr.reconcilePlayers(s)
		}()
	}
	return nil
}

// readyFallbackFor is how long a container may run without ever printing the
// line its template says means "ready" before the panel stops believing the
// template. A var rather than a const so a test does not have to wait it out.
var readyFallbackFor = 4 * time.Minute

// watchReady stops a wrong ready marker from stranding a working server in
// "starting" forever.
//
// Ready detection is a substring match against one line a template names, and
// that line is a guess until somebody boots the game and reads its log. Five of
// the templates here have never been booted, and an operator can add their own -
// so "the template's ready_log is wrong" is a permanent condition of the design,
// not a bug to be fixed once.
//
// Its failure mode was the worst available: the container runs, the game is
// listening, players can connect, and the panel says "starting" indefinitely.
// Every action keyed off status then misbehaves, and the operator has no way to
// tell this apart from a genuine hang.
//
// So after a grace period long enough for a slow first boot - a world being
// generated, a modpack loading, a Steam download finishing - a container that is
// still running is called running, and the panel says plainly that it never saw
// the line it was told to expect. That names the template as the thing to fix
// rather than leaving the server looking broken.
func (r *dockerRunner) watchReady(ctx context.Context, s *Server, name string) {
	defer recoverPanic("ready watchdog for " + s.ID)
	select {
	case <-ctx.Done():
		return
	case <-time.After(readyFallbackFor):
	}
	if s.State() != StatusStarting {
		return
	}
	// Only for a container that is genuinely up. One that died is the exit
	// watcher's business, and calling it running would be a different lie.
	if !containerRunning(s.ID) {
		return
	}
	s.mu.Lock()
	want := s.ReadyLog
	s.mu.Unlock()
	if want == "" {
		want = `the default Minecraft banner (For help, type "help")`
	} else {
		want = fmt.Sprintf("%q", want)
	}
	r.mgr.panelLine(s, "warn", fmt.Sprintf(
		"Still running after %s and this server never printed %s, which is the line its template says means ready. "+
			"Treating it as running. If it is in fact up, that template's ready_log is wrong - the correct line is in the console above.",
		readyFallbackFor, want))
	r.mgr.setStatus(s, StatusRunning, 0, "")
}

// watchExit reports the container stopping. `docker logs -f` exiting is not a
// reliable signal (it also ends if the daemon hiccups), so block on
// `docker wait`, which yields the real exit code.
func (r *dockerRunner) watchExit(ctx context.Context, s *Server, name string) {
	defer recoverPanic("exit watcher for " + s.ID)

	out, err := exec.CommandContext(ctx, "docker", "wait", name).Output()
	if err != nil {
		if ctx.Err() != nil {
			// We tore it down ourselves, so the kill is not a container
			// failure - but "we cancelled" is not the same as "someone else
			// will report this". Stop cancels immediately after `docker stop`
			// returns, and that cancel kills this `docker wait` before it can
			// print the exit code. Nothing else moves the server on, so it sat
			// in "stopping" permanently: Start refuses it ("still stopping"),
			// Kill returns 200 and changes nothing, and reconcile skips it
			// because its container is not running. The only way out was a
			// panel restart, which clears the status on load.
			//
			// If the container is gone, the server has stopped. Say so.
			if !containerRunning(s.ID) {
				r.mgr.processExited(s, nil)
			}
			return
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

		// Transport churn is never worth showing. The middle line - the command
		// echo - is kept when an operator typed it and dropped when the panel
		// asked on its own behalf.
		if body := afterLogPrefix(text); rconThreadRe.MatchString(body) {
			continue
		}
		if rconIssuedRe.MatchString(text) && s.panelIsQuerying() {
			continue
		}

		src := "server"
		if m := joinRe.FindStringSubmatch(text); m != nil {
			src = "player"
			r.mgr.trackPlayer(s, m[1], m[2] == "joined")
		} else if m := proxyJoinRe.FindStringSubmatch(text); m != nil {
			src = "player"
			r.mgr.trackPlayer(s, m[1], m[2] == "connected")
		} else if m := loginRe.FindStringSubmatch(text); m != nil {
			src = "player"
			r.mgr.trackPlayer(s, m[1], true)
		} else if m := lostConnRe.FindStringSubmatch(text); m != nil {
			src = "player"
			r.mgr.trackPlayer(s, m[1], false)
		}
		if !ready && isReady(s, text) {
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

	if consoleMode(s) == ConsoleNone {
		return fmt.Errorf(
			"%s does not accept console commands through the panel - its server reads them on "+
				"its own stdin, and containers run detached so a panel restart is not an outage",
			s.Template)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Containers are started detached so that a panel restart does not stop
	// them, which means the panel never holds their stdin - every command goes
	// in through the image's own console tool.
	name := containerPrefix + "-" + s.ID
	out, err := exec.Command("docker", "exec", name, consoleTool(s.Image), cmd).CombinedOutput()
	if err != nil {
		return fmt.Errorf("the command could not be delivered to the server console: %s",
			strings.TrimSpace(string(out)))
	}
	return nil
}

// consoleTool names the binary inside an image that takes a console command.
//
// `rcon-cli` was hardcoded, and it is not universal: the Bedrock image has no
// such binary, because Bedrock's server speaks no RCON at all. Its image ships
// `send-command`, which writes to the server process's stdin through a named
// pipe. So every console command sent to a Bedrock server failed with "not
// found in $PATH" - the console was decoration on that template.
//
// Matched on the image rather than the template slug because an imported server
// keeps the image it arrived on and can carry any slug the importer guessed.
func consoleTool(image string) string {
	if strings.HasPrefix(image, "itzg/minecraft-bedrock-server") {
		return "send-command"
	}
	return "rcon-cli"
}

// consoleMode is how a command reaches this particular server.
//
// A template may declare it; otherwise the image decides, which is the rule
// that has always applied. "none" is a real answer and a necessary one: TShock
// takes its commands on the server process's own stdin, and the panel runs
// every container detached without -i precisely so that a panel restart is not
// a server outage. There is no pipe to write to, and pretending otherwise gives
// an operator a console that accepts input and drops it.
func consoleMode(s *Server) string {
	if s.Console != "" {
		return s.Console
	}
	if consoleTool(s.Image) == "send-command" {
		return ConsoleSend
	}
	return ConsoleRCON
}

// hasRCON reports whether this image can answer a question, not just take an
// order. send-command writes to stdin and returns nothing, so a caller that
// needs the game's reply - reconcilePlayers is the only one - must not use it.
func hasRCON(image string) bool { return consoleTool(image) == "rcon-cli" }

// query runs an RCON command and returns what the game said, for the callers
// that need the answer rather than the delivery. Send discards it: a console
// command's reply reaches the operator through the log stream, so returning it
// there would print everything twice.
func (r *dockerRunner) query(s *Server, cmd string) (string, error) {
	s.mu.Lock()
	p, _ := s.proc.(*dockerProc)
	s.mu.Unlock()
	if p == nil {
		return "", fmt.Errorf("server is not running")
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	name := containerPrefix + "-" + s.ID
	// Mark the window before the exec, so the lines the game logs about this
	// connection are attributed to the panel rather than shown to the operator
	// as something that happened on their server.
	s.mu.Lock()
	s.quietRCONUntil = time.Now().Add(rconQuietWindow)
	s.mu.Unlock()
	out, err := exec.Command("docker", "exec", name, consoleTool(s.Image), cmd).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// rconQuietWindow is how long after a panel-initiated query its own console
// churn is suppressed. Generous relative to a local `docker exec`, and short
// enough that an operator's own command a second later is still echoed.
const rconQuietWindow = 3 * time.Second

// panelIsQuerying reports whether the panel itself caused the RCON traffic
// being logged right now.
func (s *Server) panelIsQuerying() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Now().Before(s.quietRCONUntil)
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
