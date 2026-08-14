package arcade

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
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
	m.mu.Lock()
	for _, s := range list {
		// Nothing is running after a panel restart; don't claim otherwise.
		if s.Status == StatusRunning || s.Status == StatusStarting || s.Status == StatusStopping {
			s.Status = StatusStopped
		}
		if s.Props == nil {
			s.Props = map[string]string{}
		}
		m.servers[s.ID] = s
		m.order = append(m.order, s.ID)
	}
	m.mu.Unlock()

	m.reconcile()
	return nil
}

// reconcile re-adopts containers that outlived the panel. Load() optimistically
// marks everything stopped, which is right for the in-process simulator and
// wrong for Docker: those containers keep running across a panel restart.
func (m *Manager) reconcile() {
	dr, ok := m.docker.(*dockerRunner)
	if !ok || !dockerAvailable() {
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

var idSeq int64

func nextID() string {
	idSeq++
	return fmt.Sprintf("s%d%02d", time.Now().Unix()%100000, idSeq%100)
}

func (m *Manager) newServer(name string, t *Template, version string, port int, runtime string) *Server {
	if version == "" && len(t.Versions) > 0 {
		version = t.Versions[0]
	}
	s := &Server{
		ID:       nextID(),
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
	if port == 0 {
		port = m.NextFreePort(t.PortHint)
	}
	if used := m.portOwner(port); used != nil {
		return nil, fmt.Errorf("port %d is already used by %q", port, used.Name)
	}
	if runtime != RuntimeDocker {
		runtime = RuntimeSim
	}
	if runtime == RuntimeDocker && !dockerAvailable() {
		return nil, fmt.Errorf("docker is not reachable on this host")
	}

	s := m.newServer(name, t, version, port, runtime)
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
	m.mu.Unlock()

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
			m.panelLine(s, "warn", fmt.Sprintf(
				"Image %s is not present locally. Pulling it now - this can take several minutes.", s.Image))
			go func() {
				_ = runCmd("docker", "pull", s.Image)
			}()
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
	return map[string]any{
		"cpu":     map[string]any{"allocated_vcpu": allocCPU, "total_vcpu": hostCPUs},
		"memory":  map[string]any{"allocated_mb": allocMem, "total_mb": hostMemMB},
		"disk":    map[string]any{"allocated_gb": disk, "total_gb": hostDiskGB},
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
