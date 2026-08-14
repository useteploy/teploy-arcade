package arcade

import (
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"time"
)

// Run wires and serves the panel. Everything the binary needs is behind this
// one call, so cmd/teploy-arcade/main.go stays flag parsing and nothing else -
// the same shape as teploy-dash.
type Config struct {
	Host     string
	Port     int
	DataDir  string
	Web      fs.FS // embedded frontend
	Version  string
	Commit   string
	NoAuth   bool
	Origins  []string // extra hostnames allowed to open the console socket
	DataHost string   // host-side path matching DataDir, when containerised

	// Credentials to create the first admin with, when the panel has none.
	// Supplying them is how a deployment skips the first-run flow entirely -
	// the same role TEPLOY_DASH_PASSWORD plays for teploy-dash.
	AdminUser     string
	AdminPassword string
}

func Run(cfg Config) error {
	hostAddr = cfg.Host
	allowedOrigins = cfg.Origins
	// Disk is measured on the data directory, not "/", because that is where
	// worlds and backups actually land.
	hostMemMB = memTotalMB()
	hostDiskGB = diskTotalGB(cfg.DataDir)
	dataDirPath = cfg.DataDir
	dataHostPath = cfg.DataHost
	if cfg.Version != "" {
		agentVersion = cfg.Version
	}

	// 0o700, not 0o755: this tree holds users.json, mcp-tokens.json, audit.json
	// and every world. Those three files carry their own 0o600, but a
	// world-readable directory still hands any other account on the host the
	// world data, the backups and the full file listing.
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("data dir: %w", err)
	}
	sweepTempFiles(cfg.DataDir)

	// Templates load before state, so a server can resolve its template on load.
	if err := LoadTemplates(cfg.DataDir); err != nil {
		log.Printf("templates: %v", err)
	}

	hub := NewHub()
	mgr := NewManager(cfg.DataDir, hub)
	if err := mgr.auth.Load(); err != nil {
		return fmt.Errorf("load users: %w", err)
	}
	// Provision the first admin from startup credentials before deciding whether
	// a setup token is needed, so a provisioned panel never prints one.
	if created, err := mgr.auth.EnsureAdmin(cfg.AdminUser, cfg.AdminPassword); err != nil {
		return fmt.Errorf("create admin from startup credentials: %w", err)
	} else if created {
		log.Printf("created the first admin from startup credentials; setup is not required")
	}
	if cfg.NoAuth {
		mgr.auth.Disable()
	} else if err := mgr.auth.BeginSetup(); err != nil {
		return err
	}
	if err := mgr.Load(); err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	go mgr.metricsLoop()
	go mgr.sampleLoop()
	go mgr.guardLoop()
	go mgr.sched.loop()

	api := &API{mgr: mgr, hub: hub}
	mux := http.NewServeMux()
	api.Routes(mux)
	mux.Handle("/", noCache(http.FileServer(http.FS(cfg.Web))))

	go func() {
		defer recoverPanic("session reaper")
		t := time.NewTicker(30 * time.Minute)
		defer t.Stop()
		for range t.C {
			mgr.auth.reapSessions()
		}
	}()

	srv := newHTTPServer(fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		limitBodies(mgr.auth.attach(mux)))

	banner(cfg, mgr)
	return srv.ListenAndServe()
}

// newHTTPServer applies the connection timeouts. With only ReadHeaderTimeout
// set, a client that sent complete headers and then dribbled its body a byte at
// a time - or read the response a byte at a time - held a goroutine and a
// socket for as long as it cared to. MaxBytesReader bounds how *big* a body may
// be, never how slowly it may arrive.
//
// Constructed here rather than inline so a test can shrink the timeouts and
// still exercise the real wiring.
func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// clearStreamDeadlines lifts this connection's deadlines. Two handlers need it:
// /api/events, which is an SSE stream, and /ws/console, which is a WebSocket.
// Both stay open for hours by design.
//
// WriteTimeout is the one that breaks them. net/http arms it once, before the
// handler runs, as an absolute deadline on the connection - it is not an idle
// timer, and nothing re-arms it while a response is still being written. So a
// blanket WriteTimeout means every SSE write past the deadline fails and the
// panel silently stops updating; on the console it is worse, because the
// upgrade hijacks the connection and inherits the deadline, leaving a socket
// that looks connected while delivering no output and accepting no commands.
// The read deadline goes with it rather than depending on net/http having
// already cleared it for a request with no body.
//
// Called per connection, so the timeouts still apply to every ordinary request.
func clearStreamDeadlines(w http.ResponseWriter) {
	rc := http.NewResponseController(w)
	// A ResponseWriter with no deadline support has nothing to lift, and no
	// caller could do anything about it.
	_ = rc.SetWriteDeadline(time.Time{})
	_ = rc.SetReadDeadline(time.Time{})
}

func banner(cfg Config, mgr *Manager) {
	docker := "not reachable - servers run on the simulator"
	if dockerAvailable() {
		docker = "available - new servers can use the docker runtime"
	}
	authState := "open (no users yet - create one in Settings)"
	switch {
	case cfg.NoAuth:
		authState = "DISABLED by --no-auth (development only)"
	case mgr.auth.Enabled():
		authState = "enabled - sign in required"
	case cfg.Host != "127.0.0.1" && cfg.Host != "localhost":
		authState = "OPEN AND REACHABLE FROM THE NETWORK - create a user before exposing this"
	}

	fmt.Printf("\n  Teploy Arcade %s\n", agentVersion)
	fmt.Printf("  panel     http://%s:%d\n", cfg.Host, cfg.Port)
	fmt.Printf("  data      %s\n", cfg.DataDir)
	fmt.Printf("  docker    %s\n", docker)
	fmt.Printf("  auth      %s\n", authState)
	fmt.Printf("  templates %d\n", len(allTemplates()))
	fmt.Printf("  servers   %d\n", len(mgr.List()))

	// Getting this wrong does not fail loudly - it silently gives game
	// containers an empty directory - so say something before it happens.
	if inContainer() && dockerAvailable() && dataHostPath == "" {
		fmt.Printf("\n  WARNING: this panel is running in a container with the docker runtime\n")
		fmt.Printf("  available, but -data-host is not set. Sibling game containers would\n")
		fmt.Printf("  bind-mount host paths that do not exist, and start with empty worlds.\n")
		fmt.Printf("  Either bind the data directory at the SAME path on host and container,\n")
		fmt.Printf("  or pass -data-host <host path>.\n")
	}
	fmt.Println()
}

// maxBodyBytes bounds every request body. Without it a single POST can exhaust
// memory, and remembering the limit in each of a dozen handlers is a rule
// somebody eventually forgets. File writes are the largest legitimate body.
const maxBodyBytes = 8 << 20

func limitBodies(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Method != http.MethodGet {
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}
		h.ServeHTTP(w, r)
	})
}

// noCache keeps a browser from serving a stale panel during development.
func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}
