package arcade

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// Run wires and serves the panel. Everything the binary needs is behind this
// one call, so cmd/teploy-arcade/main.go stays flag parsing and nothing else -
// the same shape as teploy-dash.
type Config struct {
	Host           string
	Port           int
	DataDir        string
	Web            fs.FS // embedded frontend
	Version        string
	Commit         string
	NoAuth         bool
	Origins        []string // extra hostnames allowed to open the console socket
	DataHost       string   // host-side path matching DataDir, when containerised
	PublicOrigin   string   // explicit browser origin when TLS terminates at a trusted proxy
	PublicGameHost string   // game-client hostname, independent of the panel listener

	// Credentials to create the first admin with, when the panel has none.
	// Supplying them is how a deployment skips the first-run flow entirely -
	// the same role TEPLOY_DASH_PASSWORD plays for teploy-dash.
	AdminUser     string
	AdminPassword string
}

func Run(cfg Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return RunContext(ctx, cfg)
}

// RunContext binds before loading/recovering state or launching any workers.
func RunContext(ctx context.Context, cfg Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cfg.NoAuth {
		host, err := canonicalNoAuthHost(cfg.Host)
		if err != nil {
			return err
		}
		cfg.Host = host
	}
	if cfg.PublicOrigin != "" {
		u, err := url.Parse(cfg.PublicOrigin)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("public-origin must be an absolute HTTPS origin without a path")
		}
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
	if err != nil {
		return err
	}
	defer ln.Close()
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
	hostDiskGB = diskTotalGB(cfg.DataDir)
	sweepTempFiles(cfg.DataDir)

	// Templates load before state, so a server can resolve its template on load.
	if err := LoadTemplates(cfg.DataDir); err != nil {
		return fmt.Errorf("load templates: %w", err)
	}

	engineInspector.RLock()
	previousClient, previousBase := engineInspector.client, engineInspector.base
	engineInspector.RUnlock()
	defer func() {
		engineInspector.Lock()
		engineInspector.client, engineInspector.base = previousClient, previousBase
		engineInspector.Unlock()
	}()
	hub := NewHub()
	mgr := NewManager(cfg.DataDir, hub)
	mgr.cancel()
	mgr.ctx, mgr.cancel = context.WithCancel(ctx)
	mgr.publicOrigin, mgr.publicGameHost = cfg.PublicOrigin, cfg.PublicGameHost
	defer mgr.Close()
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
		// --no-auth is a development switch, and the container default binds
		// 0.0.0.0. Refusing the combination is the only honest reading of
		// "no authentication" on a network interface: on this panel it means
		// anyone reachable can create containers as root on the host.
		//
		// R01 (audit pass 7): the old predicate accepted the EMPTY host as
		// loopback, but net.JoinHostPort("", port) is a wildcard listener -
		// every interface, not loopback. "localhost" was trusted to resolve
		// to loopback as well. Only a literal loopback address passes now,
		// normalised, before any worker starts.
		host, err := canonicalNoAuthHost(cfg.Host)
		if err != nil {
			return err
		}
		cfg.Host = host
		mgr.auth.Disable()
	} else if err := mgr.auth.BeginSetup(); err != nil {
		return err
	}
	probeCtx, probeCancel := context.WithTimeout(ctx, 10*time.Second)
	engineErr := configureEngineInspector(probeCtx)
	probeCancel()
	if engineErr != nil {
		return engineErr
	}
	if err := mgr.Load(); err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	mgr.own(mgr.metricsLoop)
	mgr.own(mgr.sampleLoop)
	mgr.own(mgr.guardLoop)
	mgr.own(mgr.playerSyncLoop)
	mgr.own(mgr.sched.loop)
	mgr.own(func() {
		timer := time.NewTicker(time.Minute)
		defer timer.Stop()
		for {
			select {
			case <-mgr.ctx.Done():
				return
			case <-timer.C:
				if err := mgr.reapImportJobs(); err != nil {
					storageUncertain.Store(true)
					log.Printf("operation metadata reaping failed: %v", err)
				}
			}
		}
	})

	api := &API{mgr: mgr, hub: hub}
	mux := http.NewServeMux()
	api.Routes(mux)
	mux.Handle("/", noCache(http.FileServer(http.FS(cfg.Web))))

	mgr.own(func() {
		defer recoverPanic("session reaper")
		t := time.NewTicker(30 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-mgr.ctx.Done():
				return
			case <-t.C:
			}
			mgr.auth.reapSessions()
		}
	})

	srv := newHTTPServer(net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		mutationPolicy(cfg.PublicOrigin, limitBodies(mgr.auth.attach(mux))))

	banner(cfg, mgr)
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			mgr.cancel()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := srv.Shutdown(shutdownCtx); err != nil {
				_ = srv.Close()
			}
		case <-done:
		}
	}()
	err = srv.Serve(ln)
	close(done)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// canonicalNoAuthHost is the address --no-auth may bind: a literal loopback
// address, nothing else. The empty string is a wildcard listener and
// "localhost" is a name whose resolution is not the panel's to trust, so both
// are normalised or refused rather than passed through to net.Listen.
func canonicalNoAuthHost(host string) (string, error) {
	if host == "localhost" {
		host = "127.0.0.1"
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.Unmap().IsLoopback() {
		return "", fmt.Errorf("--no-auth refuses to start on %q: it is a development switch and only a literal loopback address (127.0.0.1, ::1, 127.x.x.x) may carry it; the empty host is a wildcard bind, not loopback", host)
	}
	return ip.Unmap().String(), nil
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
	authState := "closed until first-run setup creates an admin (setup token is in this log)"
	switch {
	case cfg.NoAuth:
		authState = "DISABLED by --no-auth (development only)"
	case mgr.auth.Enabled():
		authState = "enabled - sign in required"
	}

	fmt.Printf("\n  Teploy Arcade %s\n", agentVersion)
	fmt.Printf("  panel     http://%s\n", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
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
