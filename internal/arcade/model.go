package arcade

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// Status values a game server can hold. `failed` is deliberately distinct from
// `stopped`: a crash is not a deliberate stop, and the UI colours them apart.
const (
	StatusStopped  = "stopped"
	StatusStarting = "starting"
	StatusRunning  = "running"
	StatusStopping = "stopping"
	StatusFailed   = "failed"
)

// Runtime selects how a server is actually executed.
const (
	RuntimeSim    = "sim"    // in-process simulator, no images to pull
	RuntimeDocker = "docker" // real container
)

// Server is the unit the panel manages. Persisted to data/servers.json.
type Server struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Template string `json:"template"`
	Mark     string `json:"mark"`
	Game     string `json:"game"`
	Version  string `json:"version"`
	Runtime  string `json:"runtime"`
	Image    string `json:"image"`
	// LaunchJar names a server jar already present in the server directory,
	// set when a server is imported from another panel. When it is set the
	// container runs THAT jar instead of letting the image download its own.
	//
	// This is the difference between migrating a server and re-creating one
	// that looks like it. A modpack pins an exact loader build, and its mods
	// are compiled against that build; an image told only "forge, 1.12.2"
	// installs whatever it considers current for 1.12.2 and the pack stops
	// loading. The operator imported a server that worked, and it has to keep
	// working.
	LaunchJar string `json:"launch_jar,omitempty"`
	Port      int    `json:"port"`
	// Copied from the template at create time rather than looked up on every
	// start: a template can be edited, and a running server's published ports
	// have to keep matching the container that is actually up. Empty means the
	// Java default - one TCP port - which is what every server created before
	// these fields existed gets, and is correct for all of them.
	Protocols  []string          `json:"protocols,omitempty"`
	PortSpan   int               `json:"port_span,omitempty"`
	ExtraPorts []string          `json:"extra_ports,omitempty"`
	ReadyLog   string            `json:"ready_log,omitempty"`
	DataPath   string            `json:"data_path,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Args       []string          `json:"args,omitempty"`
	Console    string            `json:"console,omitempty"`

	MemoryMB   int     `json:"memory_mb"`
	CPU        float64 `json:"cpu"`
	DiskGB     int     `json:"disk_gb"`
	MaxPlayers int     `json:"max_players"`
	// BackupKeep is how many archives to keep for this server; 0 keeps every
	// one. Zero is the default deliberately: a panel upgrade must not delete an
	// operator's backups because a new field arrived with an opinion. Retention
	// is something you turn on, having decided how many rounds you want.
	BackupKeep int               `json:"backup_keep"`
	Props      map[string]string `json:"props"`
	CreatedAt  time.Time         `json:"created_at"`

	// Runtime state - persisted so a panel restart doesn't forget a crash.
	Status       string    `json:"status"`
	ExitCode     int       `json:"exit_code"`
	ExitReason   string    `json:"exit_reason"`
	Restarts     int       `json:"restarts"`
	FirstFailure time.Time `json:"first_failure"`
	StartedAt    time.Time `json:"started_at"`

	// Set on save when a change needs a restart to take effect.
	PendingRestart []string `json:"pending_restart"`

	mu      sync.Mutex
	proc    procHandle // whatever the runner needs to control this server
	players []Player
	cpuPct  float64
	memMB   int
}

type Player struct {
	Name     string `json:"name"`
	UUID     string `json:"uuid"`
	PingMS   int    `json:"ping_ms"`
	JoinedAt int64  `json:"joined_at"`
}

// procHandle is opaque to everything but the runner that created it.
type procHandle interface{ stop() }

// Line is one console line. Seq is monotonic per server and is what lets a
// client localise a gap, dedupe a replay and reconcile after a reconnect.
type Line struct {
	Seq    int64  `json:"seq"`
	TS     string `json:"ts"`
	Level  string `json:"level"`  // info | warn | error
	Source string `json:"source"` // server | panel | command
	Text   string `json:"text"`
}

// State reads Status under the lock. Status is written from runner goroutines
// (a container exiting, a boot completing) while HTTP handlers read it, so
// every access outside an existing critical section must go through here - the
// race detector found real races on the bare field.
func (s *Server) State() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Status
}

func (s *Server) MOTD() string {
	if v, ok := s.Props["motd"]; ok {
		return v
	}
	return "A Minecraft Server"
}

// Snapshot copies the mutable bits under lock for JSON encoding.
func (s *Server) Snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	var uptime int64
	if s.Status == StatusRunning && !s.StartedAt.IsZero() {
		uptime = int64(time.Since(s.StartedAt).Seconds())
	}

	players := make([]Player, len(s.players))
	copy(players, s.players)

	// Copied under the lock. Go panics *fatally* on a concurrent map
	// read/write - it is not recoverable - and reloadProps writes this map
	// whenever server.properties is edited.
	props := make(map[string]string, len(s.Props))
	for k, v := range s.Props {
		props[k] = v
	}

	// A stopped server has no player count. Reporting 0/20 asserts "nobody is
	// playing", which is a different fact from "not running".
	var pc any
	if s.Status == StatusRunning || s.Status == StatusStarting {
		pc = map[string]any{"online": len(players), "max": s.MaxPlayers}
	}

	var lastExit any
	if s.Status == StatusFailed {
		lastExit = map[string]any{
			"code": s.ExitCode, "reason": s.ExitReason, "restart_count": s.Restarts,
			"circuit_open": s.Restarts >= maxRestarts && time.Since(s.FirstFailure) < failWindow,
		}
	}

	return map[string]any{
		"id":          s.ID,
		"name":        s.Name,
		"template":    s.Template,
		"mark":        s.Mark,
		"game":        s.Game,
		"version":     s.Version,
		"runtime":     s.Runtime,
		"status":      s.Status,
		"uptime":      uptime,
		"players":     pc,
		"player_list": players,
		"cpu": map[string]any{
			"percent": round1(s.cpuPct), "limit_vcpu": s.CPU,
		},
		"memory": map[string]any{
			"used_mb": s.memMB, "limit_mb": s.MemoryMB,
			// The heap the JVM will actually be given. Reported rather than
			// left for the UI to recompute: the reserve rule lives in one
			// place, and a settings screen that showed a different number than
			// the container gets would be worse than showing none.
			"heap_mb": jvmHeapMB(s.MemoryMB),
		},
		"disk_gb": s.DiskGB,
		// So the console screen can refuse input up front rather than take a
		// command and report that it could not be delivered.
		"console":         consoleMode(s),
		"address":         map[string]any{"host": hostAddr, "port": s.Port},
		"motd":            s.MOTD(),
		"last_exit":       lastExit,
		"pending_restart": s.PendingRestart,
		"created_at":      s.CreatedAt,
		"settings":        props,
	}
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

// Template describes an installable game server. Display metadata lives here,
// not in the panel, so a new template needs no UI change to appear correctly.
type Template struct {
	Slug        string   `json:"slug"`
	Name        string   `json:"name"`
	Game        string   `json:"game"`
	Group       string   `json:"group"`
	Mark        string   `json:"mark"`
	Description string   `json:"description"`
	Recommended bool     `json:"recommended"`
	Maturity    string   `json:"maturity"`
	Image       string   `json:"image"`
	Versions    []string `json:"versions"`
	MemoryMB    int      `json:"memory_mb"`
	CPU         float64  `json:"cpu"`
	DiskGB      int      `json:"disk_gb"`
	MaxPlayers  int      `json:"max_players"`
	PortHint    int      `json:"port_hint"`

	// Protocols the game listens on, and how many consecutive ports it needs.
	//
	// Empty means {"tcp"} and one port, which is every Java-edition server and
	// the proxy. They are fields on the template rather than a switch in the
	// runner because that is the whole claim templates.go makes: a new game is
	// data, not code. Publishing was hardcoded to a single TCP port, so Bedrock,
	// Rust and Valheim - all UDP - installed, booted, reported healthy and could
	// not be connected to by anybody. Nothing in the panel could show that,
	// because from the panel's side nothing was wrong.
	Protocols []string `json:"protocols,omitempty"`
	PortSpan  int      `json:"port_span,omitempty"`

	// ExtraPorts are fixed ports this software listens on that have nothing to
	// do with its main one, as "<port>/<proto>".
	//
	// PortSpan cannot express them: it walks upwards from the server's own
	// port, and a plugin's listener is wherever that plugin's config says. The
	// case that found this is Geyser, which has been running on the deployed
	// Velocity since the migration - "Started Geyser on UDP port 19132" in its
	// own log - while the container published nothing but 25565/tcp. Loaded,
	// healthy, announcing itself, and unreachable by a single Bedrock client.
	// Nothing in the panel could show that, because from the panel's side the
	// proxy was fine.
	ExtraPorts []string `json:"extra_ports,omitempty"`

	// ReadyLog is a substring of the line this game prints when it is accepting
	// players. Empty falls back to the Java and proxy banners, which is what
	// every non-Java template was being judged by - so a Bedrock server that had
	// been up for ten minutes still showed "starting".
	ReadyLog string `json:"ready_log,omitempty"`

	// How to actually launch an image that is not one of itzg's.
	//
	// Every environment variable the runner set was an itzg convention -
	// EULA, TYPE, VERSION, MEMORY, MAX_PLAYERS, MOTD, SERVER_PORT - and the
	// bind mount always landed on /data. That is fine for the Minecraft images
	// and wrong for everything else: ryshe/terraria keeps its worlds in
	// /root/.local/share/Terraria/Worlds and takes its settings as command-line
	// flags, so the panel would have mounted an empty /data, passed it seven
	// variables it ignores, and produced a server with no world.
	//
	// So a template that is not an itzg image describes its own launch. These
	// four fields are what "add a game with no code changes" actually needs:
	//
	//	DataPath - where the bind mount goes inside the container
	//	Env      - image-specific environment
	//	Args     - command-line arguments after the image
	//	Console  - how, or whether, a command can be delivered
	//
	// Env values and Args are expanded for ${PORT}, ${MEMORY_MB},
	// ${MAX_PLAYERS}, ${MOTD} and ${DATA}, so a template can wire the panel's
	// own settings into whatever names the image expects.
	DataPath string            `json:"data_path,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Args     []string          `json:"args,omitempty"`
	Console  string            `json:"console,omitempty"`
}

// Console delivery modes a template can declare.
const (
	ConsoleRCON = "rcon"         // rcon-cli inside the container; the default
	ConsoleSend = "send-command" // writes to the server's stdin; no reply
	ConsoleNone = "none"         // output only - the panel cannot send anything
)

var templates = []Template{
	{
		Slug: "vanilla", Name: "Vanilla", Game: "minecraft-java", Group: "Playable Server",
		Mark: "vanilla", Description: "The basic Vanilla experience without plugins.",
		Maturity: "stable", Image: "itzg/minecraft-server", Versions: []string{"1.20.4", "1.20.1", "1.19.4"},
		MemoryMB: 2048, CPU: 2, DiskGB: 10, MaxPlayers: 20, PortHint: 25565,
	},
	{
		Slug: "spigot", Name: "Spigot", Game: "minecraft-java", Group: "Playable Server",
		Mark: "spigot", Description: "Most used modded Minecraft server software based on CraftBukkit.",
		Maturity: "stable", Image: "itzg/minecraft-server", Versions: []string{"1.20.4", "1.19.4"},
		MemoryMB: 3072, CPU: 2, DiskGB: 10, MaxPlayers: 20, PortHint: 25565,
	},
	{
		Slug: "paper", Name: "Paper", Game: "minecraft-java", Group: "Playable Server",
		Mark: "paper", Description: "High performance fork of Spigot with many features and performance improvements.",
		Recommended: true, Maturity: "stable", Image: "itzg/minecraft-server",
		Versions: []string{"1.20.4", "1.20.1", "1.19.4"},
		MemoryMB: 4096, CPU: 2, DiskGB: 10, MaxPlayers: 20, PortHint: 25565,
	},
	{
		Slug: "purpur", Name: "Purpur", Game: "minecraft-java", Group: "Playable Server",
		Mark: "purpur", Description: "Drop-in replacement for Paper servers designed for configurability and new, exciting gameplay features.",
		Maturity: "stable", Image: "itzg/minecraft-server", Versions: []string{"1.20.4", "1.19.4"},
		MemoryMB: 4096, CPU: 2, DiskGB: 10, MaxPlayers: 20, PortHint: 25565,
	},
	{
		Slug: "forge", Name: "Forge", Game: "minecraft-java", Group: "Playable Server",
		Mark: "forge", Description: "Drastically change the way how Minecraft looks and feels with mods.",
		Maturity: "stable", Image: "itzg/minecraft-server", Versions: []string{"1.20.1", "1.19.2"},
		MemoryMB: 6144, CPU: 2, DiskGB: 15, MaxPlayers: 20, PortHint: 25565,
	},
	{
		Slug: "fabric", Name: "Fabric", Game: "minecraft-java", Group: "Playable Server",
		Mark: "fabric", Description: "Fabric is a lightweight, experimental modding toolchain for Minecraft.",
		Maturity: "stable", Image: "itzg/minecraft-server", Versions: []string{"1.20.4", "1.20.1"},
		MemoryMB: 4096, CPU: 2, DiskGB: 12, MaxPlayers: 20, PortHint: 25565,
	},
	// Velocity is the only proxy we ship. Waterfall was dropped: PaperMC
	// deprecated it in favour of Velocity, so offering it would be pointing new
	// networks at a dead end. BungeeCord is still maintained but is the legacy
	// path, and PLAN.md §7 is explicit about shipping a focused set rather than
	// chasing Pterodactyl's breadth.
	{
		// Geyser is the reason for extra_ports. It is the standard way a Java
		// network accepts Bedrock clients, it is already on the deployed proxy,
		// and it listens on UDP 19132 regardless of what the proxy's own port
		// is. A second proxy on one host will now fail to start with a port
		// conflict rather than silently being the one nobody can reach on
		// Bedrock - which is the better of the two failures, and the only one
		// an operator can act on.
		Slug: "velocity", Name: "Velocity", Game: "proxy", Group: "Network Proxy",
		Mark: "proxy", Description: "Modern proxy that fronts several servers behind one address. The maintained successor to BungeeCord and Waterfall.",
		Recommended: true, Maturity: "stable", Image: "itzg/bungeecord", Versions: []string{"3.3.0"},
		MemoryMB: 1024, CPU: 1, DiskGB: 5, MaxPlayers: 200, PortHint: 25577,
		ExtraPorts: []string{"19132/udp"},
	},
	{
		Slug: "bedrock", Name: "Bedrock", Game: "minecraft-bedrock", Group: "Other",
		Mark: "bedrock", Description: "Multiplatform version of Minecraft from Mojang.",
		// LATEST, not a pin. The only version this template ever offered was
		// 1.20.71, and Mojang's CDN now 404s it - the template's single option
		// was a download that no longer exists, so every Bedrock server created
		// from it failed on first start. Proven version-specific: LATEST boots
		// on the same image. A pinned Bedrock version is a dead template on the
		// day Mojang prunes it, and Mojang prunes.
		Maturity: "preview", Image: "itzg/minecraft-bedrock-server", Versions: []string{"LATEST"},
		MemoryMB: 2048, CPU: 2, DiskGB: 10, MaxPlayers: 10, PortHint: 19132,
		Protocols: []string{"udp"}, ReadyLog: "Server started.",
	},
	{
		Slug: "terraria", Name: "Terraria", Game: "terraria", Group: "Playable Server",
		Mark: "vanilla", Description: "TShock server. Small worlds, small memory - the one that fits on a busy host.",
		Maturity: "preview", Image: "ryshe/terraria", Versions: []string{"latest"},
		// Two cores because the cost is all in world generation, which is
		// single-threaded but competes with everything else on the box: a
		// medium world took roughly ten minutes on one core during the live
		// test. Steady-state use is nothing - 416 MB and idle CPU with the
		// world loaded.
		MemoryMB: 1024, CPU: 2, DiskGB: 5, MaxPlayers: 8, PortHint: 7777,
		// Taken from a real boot on the deployed host, not guessed. Both
		// "Listening on port 7777" and TShock's own "Server started" appear;
		// this one is the earlier of the two and is the moment the server
		// actually accepts a connection, which is what this field means. It is
		// also Terraria's own line rather than TShock's, so it still holds if
		// the image is switched to one of ryshe's vanilla tags.
		ReadyLog: "Listening on port",
		// The image keeps worlds here, not in /data. A mount on the wrong path
		// does not fail - the server generates a world in its own empty
		// directory and runs - so the panel's files and backups would address a
		// tree the running game shares nothing with.
		DataPath: "/root/.local/share/Terraria/Worlds",
		// bootstrap.sh reads WORLD_FILENAME, and refuses to start unless either
		// that world already exists or -autocreate is passed. Both are set, so
		// a first start makes a world and every later start loads it.
		Env: map[string]string{"WORLD_FILENAME": "world.wld"},
		Args: []string{
			"-autocreate", "2", // 2 = medium; small worlds finish generating in seconds
			"-port", "${PORT}",
			"-maxplayers", "${MAX_PLAYERS}",
			"-worldname", "${MOTD}",
		},
		// TShock reads commands on the server process's own stdin. Containers
		// run detached and without -i - deliberately, so a panel restart is not
		// an outage - so there is no pipe to write to. Output still streams;
		// input is refused with a reason rather than accepted and dropped.
		Console: ConsoleNone,
	},
	{
		Slug: "rust", Name: "Rust", Game: "rust", Group: "Other",
		Mark: "rust", Description: "Wipes on a schedule - the backup job matters here more than anywhere else.",
		Maturity: "preview", Image: "didstopia/rust-server", Versions: []string{"latest"},
		MemoryMB: 8192, CPU: 4, DiskGB: 30, MaxPlayers: 100, PortHint: 28015,
		// Game traffic is UDP; the +1 port is RCON, which this image publishes
		// on TCP and is how its console is reached at all.
		Protocols: []string{"udp", "tcp"}, PortSpan: 2, ReadyLog: "Server startup complete",
	},
	{
		Slug: "valheim", Name: "Valheim", Game: "valheim", Group: "Other",
		Mark: "valheim", Description: "Dedicated server. Small player counts, heavy world saves.",
		Maturity: "preview", Image: "lloesche/valheim-server", Versions: []string{"latest"},
		MemoryMB: 4096, CPU: 2, DiskGB: 15, MaxPlayers: 10, PortHint: 2456,
		// Valheim wants three consecutive UDP ports, not one: 2456 is the game,
		// 2457 the query port Steam's server browser answers on. Publishing only
		// the first makes the server unlistable even when it is running.
		Protocols: []string{"udp"}, PortSpan: 3, ReadyLog: "Game server connected",
	},
}

// templateBySlug resolves against the on-disk template set (Phase 6), falling
// back to the compiled-in defaults before they have been written out.
func templateBySlug(slug string) *Template {
	if t := templateBySlugLoaded(slug); t != nil {
		return t
	}
	for i := range templates {
		if templates[i].Slug == slug {
			return &templates[i]
		}
	}
	return nil
}

// defaultProps is the server.properties surface the settings screen edits.
func defaultProps(t *Template, name string, port, maxPlayers int) map[string]string {
	return map[string]string{
		"motd":                 name,
		"pvp":                  "true",
		"hardcore":             "false",
		"allow-flight":         "false",
		"allow-nether":         "true",
		"spawn-animals":        "true",
		"spawn-monsters":       "true",
		"spawn-npcs":           "true",
		"force-gamemode":       "false",
		"difficulty":           "normal",
		"gamemode":             "survival",
		"view-distance":        "10",
		"simulation-distance":  "10",
		"spawn-protection":     "16",
		"online-mode":          "true",
		"white-list":           "false",
		"enable-command-block": "false",
		"max-players":          itoa(maxPlayers),
		"server-port":          itoa(port),
		"level-seed":           "",
	}
}

// propMeta drives the settings UI: label, help, type and - critically - when a
// change actually takes effect. This is template metadata, not panel code, so a
// new game doesn't need a UI change to describe its own settings.
type PropMeta struct {
	Key     string   `json:"key"`
	Label   string   `json:"label"`
	Help    string   `json:"help"`
	Type    string   `json:"type"` // bool | enum | int | string
	Group   string   `json:"group"`
	Options []string `json:"options,omitempty"`
	Unit    string   `json:"unit,omitempty"`
	Applies string   `json:"applies"` // immediate | next_restart | new_world_only
	Owner   string   `json:"owner"`   // game | panel
}

var propSchema = []PropMeta{
	{Key: "spawn-animals", Label: "Spawn animals", Type: "bool", Group: "Gameplay", Applies: "next_restart", Owner: "game"},
	{Key: "spawn-monsters", Label: "Spawn monsters", Type: "bool", Group: "Gameplay", Applies: "next_restart", Owner: "game",
		Help: "Hostile mobs spawn naturally."},
	{Key: "spawn-npcs", Label: "Spawn NPCs", Type: "bool", Group: "Gameplay", Applies: "next_restart", Owner: "game"},
	{Key: "hardcore", Label: "Hardcore mode", Type: "bool", Group: "Gameplay", Applies: "next_restart", Owner: "game",
		Help: "If enabled, players will be set to spectator mode if they die. Cannot be undone for players who have already died."},
	{Key: "allow-nether", Label: "Nether world", Type: "bool", Group: "Gameplay", Applies: "next_restart", Owner: "game",
		Help: "Allows players to travel to the Nether."},
	{Key: "pvp", Label: "PVP", Type: "bool", Group: "Gameplay", Applies: "immediate", Owner: "game",
		Help: "Players will be able to kill each other."},
	{Key: "allow-flight", Label: "Flight", Type: "bool", Group: "Gameplay", Applies: "next_restart", Owner: "game",
		Help: "Allows users to use flight on your server while in Survival mode, if they have a mod that provides flight."},
	{Key: "force-gamemode", Label: "Force Gamemode", Type: "bool", Group: "Gameplay", Applies: "immediate", Owner: "game",
		Help: "Force players to join in the default game mode."},
	{Key: "difficulty", Label: "Difficulty", Type: "enum", Group: "Gameplay", Applies: "immediate", Owner: "game",
		Options: []string{"peaceful", "easy", "normal", "hard"}},
	{Key: "gamemode", Label: "Gamemode", Type: "enum", Group: "Gameplay", Applies: "immediate", Owner: "game",
		Options: []string{"survival", "creative", "adventure", "spectator"}},
	{Key: "view-distance", Label: "View Distance", Type: "int", Group: "Gameplay", Applies: "immediate", Owner: "game",
		Unit: "chunks", Help: "Chunks sent to each player - the biggest single lever on CPU and bandwidth."},

	{Key: "level-seed", Label: "Level seed", Type: "string", Group: "World", Applies: "new_world_only", Owner: "game",
		Help: "Ignored for a world that already exists. Changing it does not regenerate the world."},
	{Key: "simulation-distance", Label: "Simulation distance", Type: "int", Group: "World", Applies: "immediate", Owner: "game",
		Unit: "chunks", Help: "Chunks that actually tick. Lower this before lowering view distance."},
	{Key: "spawn-protection", Label: "Spawn protection", Type: "int", Group: "World", Applies: "immediate", Owner: "game",
		Unit: "blocks", Help: "Blocks around spawn that non-operators cannot edit."},

	{Key: "server-port", Label: "Server port", Type: "int", Group: "Network", Applies: "next_restart", Owner: "panel",
		Help: "Container port mapping. Checked against the host before save, not at boot."},
	{Key: "max-players", Label: "Max players", Type: "int", Group: "Network", Applies: "immediate", Owner: "game",
		Help: "Applies immediately. Lowering it kicks nobody."},
	{Key: "online-mode", Label: "Online mode", Type: "bool", Group: "Network", Applies: "next_restart", Owner: "game",
		Help: "Verifies players against Mojang. Disable only behind a proxy that already does."},
	{Key: "white-list", Label: "Whitelist", Type: "bool", Group: "Network", Applies: "immediate", Owner: "game",
		Help: "Only listed players may join."},
	{Key: "enable-command-block", Label: "Command blocks", Type: "bool", Group: "Network", Applies: "next_restart", Owner: "game"},
	{Key: "motd", Label: "MOTD", Type: "string", Group: "Network", Applies: "next_restart", Owner: "game",
		Help: "The line shown in the player's multiplayer list."},
}

func propMetaFor(key string) *PropMeta {
	for i := range propSchema {
		if propSchema[i].Key == key {
			return &propSchema[i]
		}
	}
	return nil
}

// itoa delegates instead of hand-rolling the digit loop: taking the sign off
// with i = -i is a no-op at math.MinInt, so the loop saw a value that was still
// negative, emitted no digits at all and returned a bare "-".
func itoa(i int) string { return strconv.Itoa(i) }

// atoi keeps the contract its callers already guard on - anything it cannot use
// is 0, checked as `p > 0` or by a range test - and now applies it to values too
// large for an int as well. The hand-rolled n*10+digit wrapped around silently
// instead: "18446744073709577216" in a hand-edited server.properties came back
// as 25600, a plausible port that cleared ApplySettings' range check and was
// persisted as the server's own.
func atoi(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}
