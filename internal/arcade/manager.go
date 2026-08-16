package arcade

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Manager owns every server, the console hub, and persistence.
type Manager struct {
	mu      sync.RWMutex
	servers map[string]*Server
	order   []string

	hub     *Hub
	dataDir string

	metrics *Metrics
	backups *backupState
	auth    *Auth
	mcp     *mcpTokens
	sched   *Scheduler

	// Ports claimed by an import that has not registered its server yet. A
	// copy import runs for minutes on a background goroutine, so without a
	// reservation every concurrent import passes the same portOwner check and
	// they all land on one port.
	reservedPorts map[int]string

	sim    Runner
	docker Runner

	// lifecycle serialises the check-then-act pairs in Start and Delete. Each
	// reads a server's state and only then commits to it, and with no lock
	// across the pair both can pass their own check: the server is removed
	// while its runner is starting, and the orphaned goroutine goes on driving
	// a server nobody owns - publishing into a room that was just dropped and
	// saving state for an entry that no longer exists.
	//
	// One mutex for the whole process, not one per server. That is only
	// tolerable because it is never held across anything that can block: a
	// state read, a status flip and the small state save behind it. It used to
	// cover Start's docker preflight - `docker info`, `docker images`, and a
	// `docker pull` that runs for minutes on a first start - so a single
	// unresponsive daemon stalled starting, stopping and deleting every other
	// server on the host. Keep slow work out of claimStart.
	lifecycle sync.Mutex

	// SSE subscribers for panel-wide events (status changes, metrics).
	subMu sync.Mutex
	subs  map[chan []byte]struct{}

	// Host CPU is a rate, so it needs the previous reading to mean anything.
	// Directory sizes are walked on a slow ticker rather than per request: a
	// world is gigabytes, and the dashboard asks for every server at once.
	usageMu   sync.RWMutex
	lastCPU   cpuSample
	hostCPU   float64
	dirSizes  map[string]int64
	dirWalked time.Time
}

func NewManager(dataDir string, hub *Hub) *Manager {
	m := &Manager{
		servers: map[string]*Server{},
		hub:     hub,
		dataDir: dataDir,
		subs:    map[chan []byte]struct{}{},
		metrics: NewMetrics(),
		backups: &backupState{locked: map[string]bool{}},
		auth:    NewAuth(dataDir),
		mcp:     newMCPTokens(dataDir),
	}
	m.reservedPorts = map[int]string{}
	m.dirSizes = map[string]int64{}
	m.sched = newScheduler(dataDir, m)
	m.sim = &simRunner{mgr: m}
	m.docker = &dockerRunner{mgr: m}
	return m
}

func (m *Manager) runnerFor(s *Server) Runner {
	if s.Runtime == RuntimeDocker {
		return m.docker
	}
	return m.sim
}

// ------------------------------------------------------------- persistence

func (m *Manager) storePath() string { return filepath.Join(m.dataDir, "servers.json") }

func (m *Manager) Load() error {
	// 0o700 to match the data dir the binary creates: users.json,
	// mcp-tokens.json and audit.json live directly in here, and a
	// world-readable directory leaks the rest of the tree regardless of their
	// own 0o600.
	if err := os.MkdirAll(m.dataDir, 0o700); err != nil {
		return err
	}
	b, err := os.ReadFile(m.storePath())
	if os.IsNotExist(err) {
		m.seed()
		return m.Save()
	}
	if err != nil {
		return err
	}
	var list []*Server
	if err := json.Unmarshal(b, &list); err != nil {
		// Refusing to boot means nobody can reach any server, which is worse
		// than booting empty with the bad file preserved beside it.
		quarantine(m.storePath(), err)
		m.seed()
		return m.Save()
	}
	// What the file says was running is the only record of intent that
	// survives a reboot, and Load is about to overwrite it. Captured here so
	// resume() can put back what the host took down. See resume().
	wasUp := map[string]bool{}

	m.mu.Lock()
	for _, s := range list {
		// Nothing is running after a panel restart; don't claim otherwise.
		if s.Status == StatusRunning || s.Status == StatusStarting || s.Status == StatusStopping {
			wasUp[s.ID] = s.Status != StatusStopping
			s.Status = StatusStopped
		}
		if s.Props == nil {
			s.Props = map[string]string{}
		}
		m.servers[s.ID] = s
		m.order = append(m.order, s.ID)
	}
	m.mu.Unlock()

	m.backfillVersions()
	m.recoverAfterBoot(wasUp)
	return nil
}

// backfillVersions fills in a version for servers recorded as "unknown".
//
// Detection reads version.json out of the jar, which is where Paper, Purpur,
// Spigot and vanilla record the exact Minecraft version - `paper.jar` carries
// nothing in its name, and that is the normal case for a server that updates in
// place. But detection runs at import time and only at import time, so every
// server imported before that reader existed kept the "unknown" it was written
// with, forever, while the answer sat inside a jar on disk the whole time. The
// deployed fleet was four Paper servers reading "paper unknown" in their own
// header for exactly this reason.
//
// Only ever fills a blank. A version already recorded is the operator's or the
// importer's, and is not second-guessed by a jar that may since have been
// replaced. Nothing recomputes the image from this - Image is fixed when the
// server is created - so a backfill cannot move a running server onto a
// different JRE.
func (m *Manager) backfillVersions() {
	filled := 0
	for _, s := range m.List() {
		s.mu.Lock()
		unknown := s.Version == "" || strings.EqualFold(s.Version, "unknown")
		jar := s.LaunchJar
		s.mu.Unlock()
		if !unknown || jar == "" {
			continue
		}
		path := filepath.Join(m.serverDir(s), jar)
		v := versionFromJar(path)
		if v == "" {
			// A proxy is not a Minecraft build and ships no version.json, so
			// its own manifest is the honest source for what it is running.
			v = jarImplVersion(path)
		}
		if v == "" {
			continue
		}
		s.mu.Lock()
		s.Version = v
		s.mu.Unlock()
		log.Printf("%s: version was unknown, read %s from %s", s.Name, v, jar)
		filled++
	}
	if filled > 0 {
		_ = m.Save()
	}
}

// How long to keep waiting for the Docker daemon at startup, and how often to
// ask. The panel's unit is ordered After=docker.service, which is satisfied
// when that unit *starts*, not when the socket answers - so on a cold boot the
// panel can come up first and find nothing.
var (
	bootRecoveryWindow = 2 * time.Minute
	bootRecoveryPoll   = 3 * time.Second
)

// recoverAfterBoot re-adopts what is still running and restarts what the host
// took down, waiting for the daemon if it has to.
//
// Both halves fail closed when Docker is not answering: reconcile returns
// early and resume's Start refuses with "docker is not reachable". Run once at
// startup with no retry, that turns the exact scenario resume was built for -
// a host reboot - into the failure it was built to prevent, because after a
// reboot is precisely when the daemon is least likely to be up yet. It would
// have looked identical to the original bug: every server stopped, nothing
// explaining why.
func (m *Manager) recoverAfterBoot(wasUp map[string]bool) {
	// A healthy boot behaves exactly as before: synchronous, no goroutine.
	if dockerReachable() {
		m.reconcile()
		m.resume(wasUp)
		return
	}

	// Nothing to wait for. Notably this is every test and every simulator-only
	// run, so neither pays for a polling goroutine.
	docked := false
	for _, s := range m.List() {
		if s.Runtime == RuntimeDocker {
			docked = true
			break
		}
	}
	if !docked {
		return
	}

	go func() {
		defer recoverPanic("boot recovery")
		deadline := time.Now().Add(bootRecoveryWindow)
		for time.Now().Before(deadline) {
			time.Sleep(bootRecoveryPoll)
			if !dockerReachable() {
				continue
			}
			log.Printf("docker answered; re-adopting and resuming servers")
			m.reconcile()
			m.resume(wasUp)
			return
		}
		// Loudly: this is the state where the panel shows a fleet of stopped
		// servers and the operator has no idea one of them ever tried.
		log.Printf("docker did not answer within %s of startup; %d server(s) that were running "+
			"before the panel stopped were NOT restored, and no container was re-adopted",
			bootRecoveryWindow, len(wasUp))
	}()
}

// resume restarts servers that were running when the panel last saw them and
// whose containers are gone.
//
// Containers run with --rm and no restart policy, so a host reboot takes every
// server down and reconcile() finds nothing to re-adopt: the panel came back
// up with eight servers all stopped and no indication anything was wrong. On
// the deployed host that is exactly what happened - the box had been up 23
// hours and the containers 19, because someone had to notice and press Start.
//
// Deliberately restoring the previous state rather than a per-server autostart
// flag. A flag has to answer "is this a boot or an upgrade?", and gets it wrong
// in one direction or the other: it either resurrects a server you stopped on
// purpose an hour ago, or leaves one down after a reboot because nobody set it.
// The last saved status already carries the answer. A server that crash-loops
// saves itself as stopped and so is not tried again on the next boot.
func (m *Manager) resume(wasUp map[string]bool) {
	if len(wasUp) == 0 {
		return
	}
	var pending []*Server
	for _, s := range m.List() {
		// Docker only. Simulator servers are a development fixture; bringing
		// five of them back on every `go run` is noise, not recovery.
		if !wasUp[s.ID] || s.Runtime != RuntimeDocker || s.State() != StatusStopped {
			continue
		}
		pending = append(pending, s)
	}
	if len(pending) == 0 {
		return
	}

	// Off the boot path, and staggered: these are JVMs, and starting five at
	// once on a 4-vCPU host means all five take the hit of each other's world
	// load. The panel answers requests while they come up.
	go func() {
		for i, s := range pending {
			if i > 0 {
				time.Sleep(resumeStagger)
			}
			log.Printf("resuming %s, which was running before the panel stopped", s.Name)
			m.panelLine(s, "info", "Resuming - this server was running before the panel restarted.")
			if err := m.Start(s.ID); err != nil {
				log.Printf("could not resume %s: %v", s.Name, err)
				m.panelLine(s, "error", "Could not resume automatically: "+err.Error())
				continue
			}
			m.audit("panel", "server.resume", s.ID, s.Name)
		}
	}()
}

// Long enough that two Minecraft worlds are not loading at the same instant,
// short enough that a network of eight is back inside a couple of minutes.
var resumeStagger = 15 * time.Second

// reconcile re-adopts containers that outlived the panel. Load() optimistically
// marks everything stopped, which is right for the in-process simulator and
// wrong for Docker: those containers keep running across a panel restart.
func (m *Manager) reconcile() {
	dr, ok := m.docker.(*dockerRunner)
	if !ok || !dockerReachable() {
		return
	}
	for _, s := range m.List() {
		if s.Runtime != RuntimeDocker || !containerRunning(s.ID) {
			continue
		}
		emit := func(l Line) { m.emit(s, l) }
		if err := dr.Adopt(s, emit); err != nil {
			log.Printf("could not re-attach to %s: %v", s.Name, err)
			continue
		}
		log.Printf("re-attached to %s, which was still running", s.Name)
	}
}

func (m *Manager) Save() error {
	m.mu.RLock()
	list := make([]*Server, 0, len(m.order))
	for _, id := range m.order {
		if s, ok := m.servers[id]; ok {
			list = append(list, s)
		}
	}
	m.mu.RUnlock()

	// json.Marshal reads Status, StartedAt, Props and PendingRestart directly.
	// Runner goroutines write those, so the marshal has to happen with every
	// server held - otherwise persistence races state changes.
	for _, s := range list {
		s.mu.Lock()
	}
	b, err := json.MarshalIndent(list, "", "  ")
	for _, s := range list {
		s.mu.Unlock()
	}
	if err != nil {
		return err
	}
	return writeFileAtomic(m.storePath(), b, 0o644)
}

// seed gives a first run something to look at rather than an empty panel.
func (m *Manager) seed() {
	specs := []struct {
		name, tpl, ver string
		port           int
	}{
		{"My Purpur Server", "purpur", "1.20.4", 25565},
		{"Survie PSEU1", "paper", "1.20.4", 25566},
		{"Clone Wars", "forge", "1.20.1", 25567},
		{"Bungee Testing Server", "velocity", "3.3.0", 25577},
		{"Creative Flats", "vanilla", "1.20.4", 25571},
	}
	for _, sp := range specs {
		t := templateBySlug(sp.tpl)
		s := m.newServer(sp.name, t, sp.ver, sp.port, RuntimeSim)
		m.mu.Lock()
		m.servers[s.ID] = s
		m.order = append(m.order, s.ID)
		m.mu.Unlock()
		// seedServerFiles now reports a genuinely failed write instead of a
		// stat race, and a demo server with no eula.txt looks fine in the panel
		// until the operator tries to start it. Say so at boot.
		if err := m.seedServerFiles(s); err != nil {
			log.Printf("could not seed files for %q: %v", sp.name, err)
		}
	}
}

// ------------------------------------------------------------------ CRUD

var idSeq atomic.Int64

// newID returns an ID no existing server holds.
//
// A server's ID is not a label. It names the container (gamepanel-<id>), the
// server directory and the backup directory, so two servers sharing one is not
// a display bug - it is one world overwriting another and one server's backups
// filed under another's name.
//
// The old generator was `s<unix%100000><seq%100>` off a process-global counter,
// and every part of that was reachable: the counter restarts at zero with the
// panel, so the pair regenerates exactly after a restart within the same
// 100,000-second window; the increment was an unsynchronised ++ on a shared
// int64, so two concurrent creates could take the same value outright. Nothing
// then checked the result against the servers that already existed.
//
// Same shape, because the ID is embedded in paths and container names on every
// deployed host and must keep parsing as what it always was. What changed is
// that the counter is atomic, entropy replaces the wrap when it has to, and the
// candidate is checked against the map and the filesystem before it is handed
// out. Uniqueness is now verified rather than assumed.
func (m *Manager) newID() string {
	for attempt := 0; attempt < 50; attempt++ {
		n := idSeq.Add(1)
		var id string
		if attempt < 10 {
			id = fmt.Sprintf("s%d%02d", time.Now().Unix()%100000, n%100)
		} else {
			// The obvious names are taken; stop being predictable about it.
			id = fmt.Sprintf("s%d%s", time.Now().Unix()%100000, randomDigits(4))
		}
		if m.idTaken(id) {
			continue
		}
		return id
	}
	// 50 collisions in a row is not a busy panel, it is a broken assumption.
	// A long random ID is still a valid one, and losing a world is not.
	return "s" + randomDigits(12)
}

// idTaken checks both places an ID can already be in use. The map is the
// panel's own view; the directory catches an ID whose server was removed from
// the panel while its files were left behind, which `teploy-arcade` does on
// purpose for a delete that keeps data.
func (m *Manager) idTaken(id string) bool {
	m.mu.RLock()
	_, ok := m.servers[id]
	m.mu.RUnlock()
	if ok {
		return true
	}
	if _, err := os.Stat(filepath.Join(m.dataDir, "servers", id)); err == nil {
		return true
	}
	if _, err := os.Stat(m.backupDir(id)); err == nil {
		return true
	}
	return false
}

// randomDigits returns n cryptographically random decimal digits. Falls back to
// the counter rather than to a fixed string: a predictable fallback in an ID
// generator reintroduces exactly the collision it is there to avoid.
func randomDigits(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%0*d", n, idSeq.Add(1)%1e9)
	}
	out := make([]byte, n)
	for i, v := range b {
		out[i] = '0' + v%10
	}
	return string(out)
}

func (m *Manager) newServer(name string, t *Template, version string, port int, runtime string) *Server {
	if version == "" && len(t.Versions) > 0 {
		version = t.Versions[0]
	}
	s := &Server{
		ID:       m.newID(),
		Name:     name,
		Template: t.Slug,
		Mark:     t.Mark,
		Game:     t.Game,
		Version:  version,
		Runtime:  runtime,
		// Tagged by version, not left at :latest - see java.go. An old modpack
		// on a Java 21 image fails inside the game's log, where the panel
		// cannot explain it.
		Image:      imageForVersion(t.Image, version),
		Port:       port,
		Protocols:  t.Protocols,
		PortSpan:   t.PortSpan,
		ExtraPorts: t.ExtraPorts,
		ReadyLog:   t.ReadyLog,
		DataPath:   t.DataPath,
		Env:        t.Env,
		Args:       t.Args,
		Console:    t.Console,
		MemoryMB:   t.MemoryMB,
		CPU:        t.CPU,
		DiskGB:     t.DiskGB,
		MaxPlayers: t.MaxPlayers,
		Status:     StatusStopped,
		CreatedAt:  time.Now(),
	}
	s.Props = defaultProps(t, name, port, t.MaxPlayers)
	return s
}

// Bounds for the three resource fields a client can set. 0 means "use the
// template's value" for all three, so it is always accepted.
const (
	// Below 512 MB the JVM cannot start at all, and jvmHeapMB's floor would
	// hand the container an -Xmx larger than its own memory limit - the kernel
	// then kills the server mid-chunk-generation with nothing in the log that
	// points back at the number someone typed here.
	minServerMemMB = 512
	maxServerMemMB = 1 << 20 // 1 TB, well past any real host
	minServerCPU   = 0.1     // docker refuses --cpus below 0.01; 0.1 is the floor a game server is usable at
	maxServerCPU   = 256.0
)

func checkServerLimits(port, memMB int, cpu float64) error {
	if port != 0 && (port < 1 || port > 65535) {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	if memMB != 0 && (memMB < minServerMemMB || memMB > maxServerMemMB) {
		return fmt.Errorf("memory must be between %d and %d MB", minServerMemMB, maxServerMemMB)
	}
	// Rejected rather than clamped, and negatives rejected rather than ignored:
	// silently substituting a different limit than the one asked for is how an
	// operator ends up debugging a server that is not running with the
	// resources the panel says it has.
	if cpu != 0 && (cpu < minServerCPU || cpu > maxServerCPU) {
		return fmt.Errorf("cpu must be between %g and %g cores", minServerCPU, maxServerCPU)
	}
	return nil
}

// checkFitsHost refuses a limit the machine cannot honour.
//
// checkServerLimits above bounds a server against absolutes - 512 MB to 1 TB,
// 0.1 to 256 cores - which are the same numbers on a Raspberry Pi and a 512 GB
// server, so on any real host they permit a number that cannot work. Disk has
// been refused against the real machine since checkDiskSpace; memory and CPU
// were checked against nothing at all. The create wizard drew a warning bar
// when the total went over, and the Settings page did not even do that, so
// giving one server more memory than the box has was a valid request that
// succeeded, reported success, and was resolved by the OOM killer at the next
// restart.
//
// This refuses only what is impossible for a single server. Overcommitment
// across servers stays allowed and stays a warning: limits are caps rather than
// reservations, and a host deliberately running five 4 GB caps on 13 GB is an
// operations decision the panel has no business overruling. See hostCapacity
// for the sum it reports and the wizard's bar.
func checkFitsHost(memMB int, cpu float64) error {
	if memMB > 0 && hostMemMB > 0 && memMB > hostMemMB {
		return fmt.Errorf("this asks for %d MB and the host has %d MB in total", memMB, hostMemMB)
	}
	if cpu > 0 && hostCPUs > 0 && cpu > hostCPUs {
		return fmt.Errorf("this asks for %g cores and the host has %g", cpu, hostCPUs)
	}
	return nil
}

func (m *Manager) Create(name, tplSlug, version string, port, memMB int, cpu float64, runtime string) (*Server, error) {
	t := templateBySlug(tplSlug)
	if t == nil {
		return nil, fmt.Errorf("unknown template %q", tplSlug)
	}
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	// Ranges are checked here rather than in the HTTP handler because this is
	// the boundary every caller crosses. A handler-only check is one new call
	// site away from being no check at all, and these values do not stop at the
	// panel: they become `-p`, `--memory` and `--cpus` on a docker command line
	// and `server-port` in server.properties.
	if err := checkServerLimits(port, memMB, cpu); err != nil {
		return nil, err
	}
	if err := checkFitsHost(memMB, cpu); err != nil {
		return nil, err
	}
	if port == 0 {
		port = m.NextFreePort(t.PortHint)
	}
	// Claimed, not merely checked.
	//
	// portOwner walks the registered servers and nothing else, and Create does
	// not register its server until several steps later - a disk check, a seed,
	// a Save. Two creates arriving together therefore both passed, and an
	// import already holding a reservation was invisible to both: the panel
	// happily produced two servers on one port, and the second one to start
	// failed inside Docker with a bind error that named nothing the operator
	// could act on.
	//
	// Import and clone have claimed through reservedPorts since the concurrent
	// import bug; create was the path still doing it the old way, which meant
	// the reservation could only ever be half-honoured.
	if holder, ok := m.claimPort(port, name); !ok {
		return nil, fmt.Errorf("port %d is already used by %q", port, holder)
	}
	claimHeld := true
	defer func() {
		if claimHeld {
			m.releasePort(port)
		}
	}()
	if runtime != RuntimeDocker {
		runtime = RuntimeSim
	}
	// The same seam Start probes through, rather than a second direct call:
	// one question, one answer, and it is what makes the create path testable.
	if runtime == RuntimeDocker && !dockerReachable() {
		return nil, fmt.Errorf("docker is not reachable on this host")
	}
	if err := m.checkDiskSpace(t.DiskGB); err != nil {
		return nil, err
	}

	s := m.newServer(name, t, version, port, runtime)
	// A template's CPU figure is the panel's own suggestion, not the operator's
	// request, and every template ships one sized for a normal host. On a small
	// one - a 2-core VPS, which is a completely ordinary place to self-host this
	// - most of them exceed it, and docker refuses a --cpus above the core count
	// outright: "Range of CPUs is from 0.01 to 2.00, as there are only 2 CPUs
	// available". So the default would have produced a server that could not
	// start, on the machines least able to diagnose it.
	//
	// Capped rather than refused, and only when it came from the template.
	// checkServerLimits refuses rather than clamps on purpose - silently
	// substituting a limit an operator did not ask for is how they end up
	// debugging a server running on numbers the panel never showed them - but
	// that rule is about *their* number. Overruling our own suggestion to fit
	// their machine is not the same act, and a lower CPU quota costs speed
	// rather than correctness, which is why memory is still refused instead.
	if cpu == 0 && hostCPUs > 0 && s.CPU > hostCPUs {
		log.Printf("%s: template %s suggests %g cores and this host has %g; using %g",
			name, t.Slug, s.CPU, hostCPUs, hostCPUs)
		s.CPU = hostCPUs
	}
	if memMB > 0 {
		s.MemoryMB = memMB
	}
	if cpu > 0 {
		s.CPU = cpu
	}
	s.Props["max-players"] = itoa(s.MaxPlayers)
	s.Props["server-port"] = itoa(port)
	s.Props["motd"] = name

	m.mu.Lock()
	m.servers[s.ID] = s
	m.order = append(m.order, s.ID)
	delete(m.reservedPorts, port) // the server itself now holds the port
	m.mu.Unlock()
	claimHeld = false

	// Seeded by a process running as root, into a directory root just made, so
	// every file belongs to root - and the container runs as uid 1000 with no
	// --user flag, so it cannot write one of them. Handed over before anything
	// can start. Deferred so a half-seeded tree is handed over too, rather than
	// left as root's for someone to puzzle over.
	if s.Runtime == RuntimeDocker {
		defer chownTree(filepath.Join(m.dataDir, "servers", s.ID), containerRunUID, containerRunGID)
	}

	if err := m.seedServerFiles(s); err != nil {
		return nil, err
	}
	_ = m.Save()
	m.broadcastEvent("server.created", s.ID)
	return s, nil
}

// SetResources changes a server's memory and CPU allotment.
//
// These only reach the container as `--memory` and `--cpus` on the next
// `docker run`, so a running server keeps its current limits until it restarts
// - and says so, rather than letting the panel report a number the container is
// not actually held to.
//
// 0 means "leave this one alone", matching Create and StartImport.
func (m *Manager) SetResources(s *Server, memMB int, cpu float64) ([]string, error) {
	// Same bounds as every other way a server's resources get set. A limit that
	// only holds at creation is not a limit.
	if err := checkServerLimits(0, memMB, cpu); err != nil {
		return nil, err
	}
	if err := checkFitsHost(memMB, cpu); err != nil {
		return nil, err
	}
	if memMB == 0 && cpu == 0 {
		return nil, fmt.Errorf("nothing to change: pass memory_mb, cpu, or both")
	}

	s.mu.Lock()
	changed := []string{}
	if memMB > 0 && memMB != s.MemoryMB {
		s.MemoryMB = memMB
		changed = append(changed, "Memory")
	}
	if cpu > 0 && cpu != s.CPU {
		s.CPU = cpu
		changed = append(changed, "CPU")
	}
	running := s.Status == StatusRunning
	if len(changed) > 0 && running {
		// Merged rather than replaced: a settings change may already be waiting
		// on the same restart, and dropping it would tell the operator their
		// edit had taken effect.
		for _, c := range changed {
			if !contains(s.PendingRestart, c) {
				s.PendingRestart = append(s.PendingRestart, c)
			}
		}
		sort.Strings(s.PendingRestart)
	}
	pending := append([]string(nil), s.PendingRestart...)
	s.mu.Unlock()

	if err := m.Save(); err != nil {
		return nil, err
	}
	m.broadcastEvent("server.updated", s.ID)
	if !running {
		return nil, nil
	}
	return pending, nil
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// Reorder sets the order servers are listed and tabbed in.
//
// Order was creation order with no way to change it, which stops being a
// detail once someone runs more servers than fit on screen: the tab strip is
// the primary way you move between them, and you cannot put the one you watch
// all day first.
//
// Ids not mentioned keep their relative order and follow the ones that were,
// so a client holding a stale list cannot silently drop a server that was
// created while the operator was dragging. Unknown ids are ignored rather than
// rejected - a server deleted mid-drag should not fail the whole reorder.
func (m *Manager) Reorder(ids []string) error {
	m.mu.Lock()
	seen := make(map[string]bool, len(ids))
	next := make([]string, 0, len(m.order))
	for _, id := range ids {
		if _, ok := m.servers[id]; !ok || seen[id] {
			continue
		}
		seen[id] = true
		next = append(next, id)
	}
	for _, id := range m.order {
		if !seen[id] {
			next = append(next, id)
		}
	}
	m.order = next
	m.mu.Unlock()

	if err := m.Save(); err != nil {
		return err
	}
	m.broadcastEvent("servers.reordered", "")
	return nil
}

func (m *Manager) Delete(id string) error {
	// Held from the state check through the map mutation, so a Start cannot
	// slip between them and leave a runner attached to a deleted server.
	m.lifecycle.Lock()
	s := m.Get(id)
	if s == nil {
		m.lifecycle.Unlock()
		return fmt.Errorf("no such server")
	}
	if st := s.State(); st == StatusRunning || st == StatusStarting {
		m.lifecycle.Unlock()
		return fmt.Errorf("stop the server before deleting it")
	}
	m.mu.Lock()
	delete(m.servers, id)
	for i, x := range m.order {
		if x == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	m.mu.Unlock()

	// A server may still be stopping when it is deleted, and once the map entry
	// is gone nothing else will ever cancel its runner: the simulator would keep
	// ticking and republishing, which recreates the room DropRoom is about to
	// remove and leaks both for the life of the process.
	s.mu.Lock()
	proc := s.proc
	s.proc = nil
	s.mu.Unlock()
	m.lifecycle.Unlock()
	if proc != nil {
		proc.stop()
	}

	m.metrics.drop(id)
	m.hub.DropRoom(id)
	// Everything else that referred to this server is dropped here; its
	// schedule was the one thing that stayed behind, firing on a timer at a
	// server that no longer exists.
	if n := m.sched.DropServer(id); n > 0 {
		log.Printf("%s: removed %d scheduled task(s) along with the server", id, n)
	}
	_ = os.RemoveAll(filepath.Join(m.dataDir, "servers", id))
	_ = os.RemoveAll(m.backupDir(id))

	_ = m.Save()
	m.broadcastEvent("server.deleted", id)
	return nil
}

func (m *Manager) Get(id string) *Server {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.servers[id]
}

func (m *Manager) List() []*Server {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Server, 0, len(m.order))
	for _, id := range m.order {
		if s, ok := m.servers[id]; ok {
			out = append(out, s)
		}
	}
	return out
}

// claimPort reserves a port for an in-flight import, atomically with the check
// that nothing else holds it. Returns what holds it when the claim fails.
func (m *Manager) claimPort(port int, who string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range m.order {
		if s, ok := m.servers[id]; ok && s.Port == port {
			return s.Name, false
		}
	}
	if holder, taken := m.reservedPorts[port]; taken {
		return holder + " (import in progress)", false
	}
	m.reservedPorts[port] = who
	return "", true
}

func (m *Manager) releasePort(port int) {
	m.mu.Lock()
	delete(m.reservedPorts, port)
	m.mu.Unlock()
}

func (m *Manager) portOwner(port int) *Server {
	for _, s := range m.List() {
		if s.Port == port {
			return s
		}
	}
	return nil
}

func (m *Manager) NextFreePort(hint int) int {
	if hint == 0 {
		hint = 25565
	}
	used := map[int]bool{}
	for _, s := range m.List() {
		used[s.Port] = true
	}
	// Reservations count as used. A copy import runs for minutes before its
	// server is registered, and suggesting the port it is already holding sends
	// the operator straight into the collision the reservation exists to stop.
	m.mu.RLock()
	for p := range m.reservedPorts {
		used[p] = true
	}
	m.mu.RUnlock()
	for p := hint; p < hint+400; p++ {
		if !used[p] {
			return p
		}
	}
	return hint
}

// ------------------------------------------------------------- lifecycle

// dockerReachable and dockerImageLocal are the docker preflight Start runs.
// Vars rather than direct calls so a test can stand in for a daemon that never
// answers - which is the condition the lock scoping below exists to survive.
var (
	dockerReachable  = dockerAvailable
	dockerImageLocal = imagePresent
)

func (m *Manager) Start(id string) error {
	s := m.Get(id)
	if s == nil {
		return fmt.Errorf("no such server")
	}

	// The preflight runs before the lifecycle lock is taken, deliberately: it
	// shells out to the docker daemon up to three times and reads no panel
	// state at all. See the note on Manager.lifecycle for what holding the lock
	// across it did to every other server on the host.
	if s.Runtime == RuntimeDocker {
		if !dockerReachable() {
			return fmt.Errorf("docker is not reachable")
		}
		if !dockerImageLocal(s.Image) {
			m.pullImage(s)
		}
	}

	if err := m.claimStart(s); err != nil {
		return err
	}

	m.panelLine(s, "info", fmt.Sprintf("Starting %s (%s runtime) on port %d.", s.Name, s.Runtime, s.Port))

	emit := func(l Line) { m.emit(s, l) }
	if err := m.runnerFor(s).Start(s, emit); err != nil {
		m.fail(s, 1, err.Error())
		return err
	}
	return nil
}

// claimStart takes a server from "may start" to "starting" as one step, and is
// the whole of what the lifecycle lock protects. The flip to StatusStarting is
// the commit: Delete refuses a starting server and so does a second Start, so
// everything Start does afterwards is safe with the lock already released.
func (m *Manager) claimStart(s *Server) error {
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()

	// Looked up again under the lock. The preflight above can take minutes, and
	// Delete may have committed while it ran.
	if m.Get(s.ID) == nil {
		return fmt.Errorf("no such server")
	}
	switch s.State() {
	case StatusRunning, StatusStarting:
		return fmt.Errorf("already running")
	case StatusStopping:
		return fmt.Errorf("still stopping")
	}

	if m.circuitOpen(s) {
		return fmt.Errorf("this server failed %d times in a row; clear the failure count before starting it again",
			maxRestarts)
	}
	// The port is checked at create time, but server.properties can be edited
	// by hand. Catching it here beats letting Docker fail the bind.
	for _, other := range m.List() {
		if other.ID == s.ID || other.Port != s.Port {
			continue
		}
		if st := other.State(); st == StatusRunning || st == StatusStarting {
			return fmt.Errorf("port %d is already in use by %q", s.Port, other.Name)
		}
	}

	s.mu.Lock()
	s.PendingRestart = nil
	s.StartedAt = time.Now()
	s.mu.Unlock()

	m.setStatus(s, StatusStarting, 0, "")
	return nil
}

func (m *Manager) Stop(id string) error {
	s := m.Get(id)
	if s == nil {
		return fmt.Errorf("no such server")
	}
	if st := s.State(); st == StatusStopped || st == StatusFailed {
		return fmt.Errorf("not running")
	}
	m.setStatus(s, StatusStopping, 0, "")
	m.panelLine(s, "info", "Stopping the server gracefully.")

	go func() {
		defer recoverPanic("stop worker for " + s.ID)
		_ = m.runnerFor(s).Stop(s)
		if s.Runtime == RuntimeSim {
			time.Sleep(700 * time.Millisecond)
			m.stopped(s)
		}
	}()
	return nil
}

func (m *Manager) Kill(id string) error {
	s := m.Get(id)
	if s == nil {
		return fmt.Errorf("no such server")
	}
	m.panelLine(s, "warn", "Kill requested - the process is being terminated without a clean shutdown. Unsaved chunks are lost.")
	go func() {
		defer recoverPanic("kill worker for " + s.ID)
		_ = m.runnerFor(s).Kill(s)
		if s.Runtime == RuntimeSim {
			time.Sleep(200 * time.Millisecond)
			m.fail(s, 137, "killed")
		}
	}()
	return nil
}

// restartStopWait bounds how long a restart waits for the stop to land. A
// graceful `docker stop -t 45` can take the better part of a minute under load,
// so the bound is generous - but it has to exist, or a server that never
// finishes stopping leaves this poller running for the life of the process. A
// var rather than a const so the give-up path is reachable in a test.
var restartStopWait = 3 * time.Minute

func (m *Manager) Restart(id string) error {
	s := m.Get(id)
	if s == nil {
		return fmt.Errorf("no such server")
	}
	if st := s.State(); st == StatusStopped || st == StatusFailed {
		return m.Start(id)
	}
	m.panelLine(s, "info", "Restart requested.")
	go func() {
		defer recoverPanic("restart worker for " + s.ID)
		_ = m.Stop(id)
		// Start refuses a server that is still stopping, so waiting a fixed
		// time and then starting anyway turns a slow stop into a silent
		// stop-only: the operator's restart is accepted, logged nowhere they
		// can see, and never happens. Wait for the stop to actually land, and
		// say so on the console if it never does.
		deadline := time.Now().Add(restartStopWait)
		for {
			cur := m.Get(id)
			if cur == nil {
				return // deleted while it was stopping
			}
			if st := cur.State(); st == StatusStopped || st == StatusFailed {
				break
			}
			if time.Now().After(deadline) {
				m.panelLine(s, "error", "Restart abandoned: the server never finished stopping.")
				log.Printf("restart %s: never reached a stopped state", id)
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
		time.Sleep(400 * time.Millisecond)
		if err := m.Start(id); err != nil {
			m.panelLine(s, "error", "Restart failed: "+err.Error())
			log.Printf("restart %s: %v", id, err)
		}
	}()
	return nil
}

func (m *Manager) Send(id, cmd, mode, actor string) error {
	s := m.Get(id)
	if s == nil {
		return fmt.Errorf("no such server")
	}
	if s.State() != StatusRunning {
		return fmt.Errorf("server is not running")
	}
	line := cmd
	if mode == "say" {
		line = "say " + cmd
	}
	// Echo the command into the stream attributed to whoever ran it. This is
	// also what the audit log wants, so the actor travels with the command.
	m.emit(s, Line{Level: "info", Source: "command", Text: fmt.Sprintf("%s > %s", actor, cmd)})

	// A docker server answers over RCON, and that answer was being thrown away:
	// the runner captured it and used it only to build an error message. So
	// `list` on a real server echoed the command and printed nothing, while the
	// same command against the simulator printed its reply - a difference an
	// operator reads as a broken server, not as a missing feature.
	//
	// Only commands a person or a task ran. Internal quiescing calls
	// (save-off/save-all around a backup) go direct to the runner and stay
	// quiet, which is why this lives here rather than inside Send.
	if dr, ok := m.runnerFor(s).(*dockerRunner); ok {
		out, err := dr.query(s, line)
		if err != nil {
			return err
		}
		for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
			if strings.TrimSpace(l) == "" {
				continue
			}
			m.emit(s, Line{Level: classify(l), Source: "server", Text: l})
		}
		return nil
	}
	return m.runnerFor(s).Send(s, line)
}

// ---------------------------------------------------------- state changes

func (m *Manager) setStatus(s *Server, status string, code int, reason string) {
	s.mu.Lock()
	changed := s.Status != status
	s.Status = status
	if status == StatusRunning && s.StartedAt.IsZero() {
		s.StartedAt = time.Now()
	}
	if status == StatusFailed {
		s.ExitCode = code
		s.ExitReason = reason
	}
	s.mu.Unlock()

	if changed {
		_ = m.Save()
		m.broadcastEvent("server.status", s.ID)
		m.hub.PublishRaw(s.ID, map[string]any{"t": "status", "server": s.Snapshot()})
	}
}

func (m *Manager) stopped(s *Server) {
	s.mu.Lock()
	s.players = nil
	s.cpuPct = 0
	s.memMB = 0
	proc := s.proc
	s.proc = nil
	s.mu.Unlock()
	// Dropping the handle is not the same as cancelling the runner: the
	// simulator's goroutine only ends when its context does, and until then it
	// keeps rewriting cpu/memory and emitting console lines for a server the
	// panel is showing as stopped.
	if proc != nil {
		proc.stop()
	}
	m.panelLine(s, "info", "Server stopped.")
	m.setStatus(s, StatusStopped, 0, "")
	m.pushPlayers(s)
}

// maxRestarts is how many failures inside failWindow open the breaker. A
// server that dies instantly and is restarted forever pins a core and floods
// the console; PLAN.md §8 calls for a breaker and this is it.
const (
	maxRestarts = 5
	failWindow  = 10 * time.Minute
)

// circuitOpen reports whether a server has failed too often, too fast.
func (m *Manager) circuitOpen(s *Server) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Restarts >= maxRestarts && time.Since(s.FirstFailure) < failWindow
}

// ClearFailures closes the breaker. Without a way back a crash-looping server
// would be permanently unstartable, which is worse than the loop.
func (m *Manager) ClearFailures(s *Server, actor string) {
	s.mu.Lock()
	s.Restarts = 0
	s.FirstFailure = time.Time{}
	s.mu.Unlock()
	_ = m.Save()
	m.audit(actor, "server.clear_failures", s.ID, "")
	m.broadcastEvent("server.updated", s.ID)
}

func (m *Manager) fail(s *Server, code int, reason string) {
	s.mu.Lock()
	s.players = nil
	s.cpuPct = 0
	s.memMB = 0
	proc := s.proc
	s.proc = nil
	s.Restarts++
	if s.FirstFailure.IsZero() || time.Since(s.FirstFailure) > failWindow {
		s.FirstFailure = time.Now()
		s.Restarts = 1
	}
	tripped := s.Restarts >= maxRestarts
	s.mu.Unlock()

	// Same reason as stopped, and more visible here: the `crash` command reaches
	// this path with the simulator still running, so a "failed" server would go
	// on reporting live CPU, memory and console output.
	if proc != nil {
		proc.stop()
	}

	m.panelLine(s, "error", fmt.Sprintf("Server exited %d (%s).", code, reason))
	if tripped {
		m.panelLine(s, "error", fmt.Sprintf(
			"Stopped trying: %d failures in under %s. Fix the cause, then clear the failure count to start it again.",
			maxRestarts, failWindow))
		m.audit("system", "server.circuit_open", s.ID, reason)
	}
	m.setStatus(s, StatusFailed, code, reason)
	m.pushPlayers(s)
}

// processExited is the docker path's terminal handler.
func (m *Manager) processExited(s *Server, err error) {
	if s.State() == StatusStopping {
		m.stopped(s)
		return
	}
	// Exiting while still starting is a failed start even on exit 0: the game
	// can refuse a bad world, print the reason and quit cleanly.
	if err == nil && s.State() == StatusStarting {
		m.fail(s, 0, "exited during startup - see the console for the reason")
		return
	}
	code, reason := 0, "exited"
	if err != nil {
		code, reason = 1, err.Error()
		if ee, ok := err.(interface{ ExitCode() int }); ok {
			code = ee.ExitCode()
		}
		if code == 137 {
			reason = "OOM"
		}
		m.fail(s, code, reason)
		return
	}
	m.stopped(s)
}

func (m *Manager) trackPlayer(s *Server, name string, joined bool) {
	s.mu.Lock()
	if joined {
		found := false
		for _, p := range s.players {
			if p.Name == name {
				found = true
			}
		}
		if !found {
			s.players = append(s.players, Player{Name: name, UUID: fakeUUID(name), PingMS: 30, JoinedAt: time.Now().Unix()})
		}
	} else {
		for i, p := range s.players {
			if p.Name == name {
				s.players = append(s.players[:i], s.players[i+1:]...)
				break
			}
		}
	}
	s.mu.Unlock()
	m.pushPlayers(s)
}

func (m *Manager) pushPlayers(s *Server) {
	s.mu.Lock()
	players := make([]Player, len(s.players))
	copy(players, s.players)
	max := s.MaxPlayers
	s.mu.Unlock()
	m.hub.PublishRaw(s.ID, map[string]any{
		"t": "players", "players": players, "max": max,
	})
}

// emit sends a game line into the room.
func (m *Manager) emit(s *Server, l Line) {
	if l.Source == "ready" {
		l.Source = "server"
		l.Level = "ok"
	}
	m.hub.Publish(s.ID, l)
}

// panelLine injects a line the panel itself authored. Tagged so agent output is
// never confused with ours.
func (m *Manager) panelLine(s *Server, level, text string) {
	m.hub.Publish(s.ID, Line{Level: level, Source: "panel", Text: text})
}

// ---------------------------------------------------------------- events

// maxSubscribers bounds the SSE fan-out. Every subscriber costs a goroutine, a
// socket, a ticker and a 64-slot buffer held for as long as the client keeps
// the stream open, and nothing counted them: anyone who could reach /api/events
// could open them until the process ran out of memory or file descriptors.
//
// The ceiling is set where no real panel can reach it. A browser opens one
// stream per tab and will not hold more than about six connections to one host,
// so this is roughly ten operators' worth of tabs.
const maxSubscribers = 64

func (m *Manager) Subscribe() (chan []byte, error) {
	m.subMu.Lock()
	defer m.subMu.Unlock()
	if len(m.subs) >= maxSubscribers {
		return nil, fmt.Errorf("this panel already has %d event streams open; close a tab and try again", maxSubscribers)
	}
	ch := make(chan []byte, 64)
	m.subs[ch] = struct{}{}
	return ch, nil
}

func (m *Manager) Unsubscribe(ch chan []byte) {
	m.subMu.Lock()
	if _, ok := m.subs[ch]; ok {
		delete(m.subs, ch)
		close(ch)
	}
	m.subMu.Unlock()
}

func (m *Manager) broadcastEvent(kind, id string) {
	payload := mustJSON(map[string]any{"event": kind, "id": id, "servers": m.listSnapshot()})
	m.subMu.Lock()
	for ch := range m.subs {
		select {
		case ch <- payload:
		default: // a stalled SSE reader sheds updates rather than blocking state changes
		}
	}
	m.subMu.Unlock()
}

func (m *Manager) listSnapshot() []map[string]any {
	list := m.List()
	out := make([]map[string]any, 0, len(list))
	for _, s := range list {
		snap := s.Snapshot()
		// A real trace on the card, not a decorative squiggle. Thinned to 24
		// points because that is all a 200px sparkline can show.
		snap["spark"] = m.metrics.Series(s.ID, 15*time.Minute, 24)
		// What the server costs on disk - worlds and backups are the reason a
		// game host runs out of space, and it is per-server information the
		// operator cannot get anywhere else in the panel.
		snap["disk_mb"] = m.dirSizeMB(s.ID)
		out = append(out, snap)
	}
	return out
}

// metricsLoop pushes resource samples to the panel on a fixed cadence.
func (m *Manager) metricsLoop() {
	defer recoverPanic("metrics loop")
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for range t.C {
		m.sampleHostCPU()
		m.refreshDirSizes()
		if len(m.List()) == 0 {
			continue
		}
		m.broadcastEvent("metrics", "")
		for _, s := range m.List() {
			if s.State() == StatusRunning {
				m.hub.PublishRaw(s.ID, map[string]any{"t": "status", "server": s.Snapshot()})
			}
		}
	}
}

// Host reports the machine's allocation ledger. Allocated, not used: the
// overcommit check is about the sum of limits.
// sampleHostCPU turns two cumulative readings into a percentage. The first
// tick after start has nothing to compare against and reports nothing rather
// than a number derived from boot-time totals.
func (m *Manager) sampleHostCPU() {
	cur, ok := readCPUSample()
	if !ok {
		return
	}
	m.usageMu.Lock()
	prev := m.lastCPU
	m.lastCPU = cur
	if prev.total > 0 && cur.total > prev.total {
		busy := float64(cur.busy - prev.busy)
		total := float64(cur.total - prev.total)
		m.hostCPU = busy / total * 100
	}
	m.usageMu.Unlock()
}

// checkDiskSpace refuses a create the disk cannot physically hold.
//
// Disk gets different treatment from memory and CPU on purpose. Those are caps
// on a shared, reclaimable resource, which is why the dashboard reports usage
// rather than commitment - 22 vCPU allocated on a 4 vCPU box is normal and
// reporting it as a crisis was a bug. Disk is not reclaimable: a world that
// grows into its allowance keeps it, and running out is not slowness, it is
// every server on the host writing into a full filesystem at once.
//
// But the refusal is on free space, not on commitment. Commitment is the sum
// of template defaults, which is a number nobody chose: the deployed host has
// 87 GB committed against 99 GB while using 25 GB, so a commitment rule would
// refuse a 15 GB Forge server with 74 GB genuinely free. Refuse what cannot
// work; warn about what is merely promised - the host payload already carries
// that sum as disk.allocated_gb, and the create wizard warns from it.
//
// Nothing enforces the per-server figure at the filesystem layer. That wants
// XFS project quotas, and this host is ext4 inside an LXC, where they do not
// exist - so the panel promises only what it can keep: it will not hand out
// space the disk does not have, and it shows what each server uses against
// what it was given.
func (m *Manager) checkDiskSpace(wantGB int) error {
	if wantGB <= 0 || hostDiskGB <= 0 {
		return nil
	}
	free := hostDiskGB - diskUsedGB(m.dataDir)
	if wantGB > free {
		return fmt.Errorf(
			"this asks for %d GB and the host has %d GB free of %d GB; "+
				"delete a backup or an unused server first",
			wantGB, free, hostDiskGB)
	}
	return nil
}

// dirSizeMB is the on-disk size of a server's directory, refreshed on a slow
// ticker. Walking a multi-gigabyte world on every dashboard render would make
// the panel the most expensive thing on the host.
func (m *Manager) dirSizeMB(id string) int64 {
	m.usageMu.RLock()
	v := m.dirSizes[id]
	m.usageMu.RUnlock()
	return v
}

const dirSizeInterval = 2 * time.Minute

// refreshDirSizes walks every server directory. Called from the metrics loop,
// rate-limited by dirWalked.
func (m *Manager) refreshDirSizes() {
	m.usageMu.RLock()
	fresh := time.Since(m.dirWalked) < dirSizeInterval
	m.usageMu.RUnlock()
	if fresh {
		return
	}
	m.usageMu.Lock()
	m.dirWalked = time.Now()
	m.usageMu.Unlock()

	sizes := map[string]int64{}
	for _, s := range m.List() {
		var total int64
		root := m.serverDir(s)
		_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
			if err != nil {
				return nil // an unreadable corner should not abandon the total
			}
			if !info.IsDir() {
				total += info.Size()
			}
			return nil
		})
		sizes[s.ID] = total / (1 << 20)
	}
	m.usageMu.Lock()
	m.dirSizes = sizes
	m.usageMu.Unlock()
}

func (m *Manager) Host() map[string]any {
	var allocCPU float64
	var allocMem, disk int
	running := 0
	for _, s := range m.List() {
		allocCPU += s.CPU
		allocMem += s.MemoryMB
		disk += s.DiskGB
		if s.State() == StatusRunning {
			running++
		}
	}
	// Usage and commitment are different questions and were being conflated.
	// Summing every server's limit answers "if everything ran at once", which
	// on a host deliberately overcommitted (limits are caps, not reservations)
	// reads as a crisis - 22 vCPU allocated on a 4 vCPU box, next to "5 of 8
	// running". What an operator actually watches is what is in use now.
	m.usageMu.RLock()
	cpuPct := m.hostCPU
	m.usageMu.RUnlock()

	var players, usedMem int
	for _, s := range m.List() {
		if s.State() != StatusRunning {
			continue
		}
		snap := s.Snapshot()
		if mem, ok := snap["memory"].(map[string]any); ok {
			if u, ok := mem["used_mb"].(int); ok {
				usedMem += u
			}
		}
		if p, ok := snap["players"].(map[string]any); ok {
			if o, ok := p["online"].(int); ok {
				players += o
			}
		}
	}

	return map[string]any{
		"cpu": map[string]any{
			"allocated_vcpu": allocCPU, "total_vcpu": hostCPUs,
			"used_percent": round1(cpuPct),
		},
		"memory": map[string]any{
			"allocated_mb": allocMem, "total_mb": hostMemMB,
			"used_mb": memUsedMB(), "servers_mb": usedMem,
		},
		"disk": map[string]any{
			"allocated_gb": disk, "total_gb": hostDiskGB,
			"used_gb": diskUsedGB(m.dataDir),
		},
		"players": players,
		"servers": len(m.List()), "running": running,
		"docker": dockerAvailable(),
		"agent":  map[string]any{"version": agentVersion, "healthy": true},
	}
}

// SettingsView returns the schema joined to current values, grouped for the UI.
func (m *Manager) SettingsView(s *Server) []map[string]any {
	groups := []string{"Gameplay", "World", "Network"}
	byGroup := map[string][]map[string]any{}

	for _, meta := range propSchema {
		v, ok := s.Props[meta.Key]
		if !ok {
			continue
		}
		byGroup[meta.Group] = append(byGroup[meta.Group], map[string]any{
			"key": meta.Key, "label": meta.Label, "help": meta.Help,
			"type": meta.Type, "options": meta.Options, "unit": meta.Unit,
			"applies": meta.Applies, "owner": meta.Owner, "value": v,
		})
	}

	out := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		if len(byGroup[g]) > 0 {
			out = append(out, map[string]any{"group": g, "keys": byGroup[g]})
		}
	}
	return out
}

// ApplySettings writes changes and reports which need a restart.
func (m *Manager) ApplySettings(s *Server, changes map[string]string) ([]string, error) {
	var needRestart []string

	// Validate before taking the lock: portOwner walks every server and would
	// deadlock against a sibling's s.mu, and a rejected change must not have
	// half-applied.
	running := s.State() == StatusRunning
	newPort, newMax := 0, 0
	for k, v := range changes {
		meta := propMetaFor(k)
		if meta == nil {
			return nil, fmt.Errorf("unknown setting %q", k)
		}
		if k == "server-port" {
			p := atoi(v)
			if p < 1 || p > 65535 {
				return nil, fmt.Errorf("port %s is out of range", v)
			}
			if owner := m.portOwner(p); owner != nil && owner.ID != s.ID {
				return nil, fmt.Errorf("port %d is already used by %q", p, owner.Name)
			}
			newPort = p
		}
		if k == "max-players" {
			newMax = atoi(v)
		}
	}

	// Everything that touches s.Props happens under the lock. reloadProps
	// writes the same map from the file-manager path, and Go aborts the whole
	// process on a concurrent map read/write - it is not a recoverable panic.
	s.mu.Lock()
	for k, v := range changes {
		if s.Props[k] != v && propMetaFor(k).Applies == "next_restart" && running {
			needRestart = append(needRestart, propMetaFor(k).Label)
		}
		s.Props[k] = v
	}
	if newPort > 0 {
		s.Port = newPort
	}
	if newMax > 0 {
		s.MaxPlayers = newMax
	}
	sort.Strings(needRestart)
	s.PendingRestart = needRestart
	s.mu.Unlock()

	if err := m.Save(); err != nil {
		return nil, err
	}
	if err := m.writeProps(s); err != nil {
		return nil, err
	}
	m.broadcastEvent("server.updated", s.ID)

	if len(changes) > 0 && running {
		m.panelLine(s, "info", fmt.Sprintf("%d setting(s) saved from the panel.", len(changes)))
	}
	return needRestart, nil
}

// audit records a mutating action. Append-only, capped, persisted.
func (m *Manager) audit(actor, action, target, detail string) {
	if m.auth == nil {
		return
	}
	m.auth.Append(AuditEntry{
		TS: time.Now().Unix(), Actor: actor, Action: action,
		Target: target, Detail: detail,
	})
}

func runCmd(name string, args ...string) error {
	return execCommand(name, args...)
}

// pullImage fetches an image the host does not have, reporting progress into
// the server's own console.
//
// It used to background a `docker pull` and immediately run `docker run`, which
// pulls the same image itself when it is missing. Two pulls of the same image
// at once, neither of them visible: the console said "Pulling it now - this can
// take several minutes" and then printed nothing at all until the game booted.
// On a small image that is a few seconds; on CS2, which downloads its game
// through SteamCMD, it is long enough that the only honest reading of the panel
// is that it has hung. The first thing a new operator does is create a server,
// so this silence is the product's first impression.
//
// Synchronous and streamed. It is not slower - `docker run` was already waiting
// for exactly this - it is the same wait with the progress shown. Failures are
// logged rather than returned: docker run makes its own attempt, and its error
// is the one worth surfacing because it knows whether the container started.
func (m *Manager) pullImage(s *Server) {
	m.panelLine(s, "warn", fmt.Sprintf(
		"Image %s is not on this host yet. Downloading it now - a large game image can take a while, "+
			"and progress appears below.", s.Image))

	cmd := exec.Command("docker", "pull", s.Image)
	out, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("pull %s: %v", s.Image, err)
		return
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		log.Printf("pull %s: %v", s.Image, err)
		return
	}

	// Docker reports each layer on its own line, rewriting them as they
	// progress. Only the milestones are forwarded: the per-layer percentage
	// churn would be hundreds of lines a second into a 500-line ring buffer,
	// which would push the operator's actual server log out of it.
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 8*1024), 256*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "",
			strings.Contains(line, "Downloading"),
			strings.Contains(line, "Extracting"),
			strings.Contains(line, "Waiting"),
			strings.Contains(line, "Verifying"):
			continue
		}
		m.panelLine(s, "info", "docker pull: "+line)
	}
	if err := cmd.Wait(); err != nil {
		log.Printf("pull %s: %v", s.Image, err)
		m.panelLine(s, "warn", "The image download did not complete cleanly; starting anyway, "+
			"which will retry it.")
		return
	}
	m.panelLine(s, "info", "Image downloaded. Starting the server.")
}
