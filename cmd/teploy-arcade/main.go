// Command teploy-arcade is the Teploy Arcade panel: one static Go binary with
// an embedded frontend that manages game servers on a single host.
//
// Teploy installs it; the panel is the long-running runtime that manages game
// containers locally. See PLAN.md §9 for why those roles stay separate.
package main

import (
	"embed"
	"flag"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/useteploy/teploy-arcade/internal/arcade"
)

// frontendFS embeds the SPA shipped with the binary. Without this the binary
// breaks the moment it runs outside the source tree.
//
//go:embed all:frontend
var frontendFS embed.FS

// Set by goreleaser via -ldflags "-X main.version=..." (see .goreleaser.yaml).
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	var (
		host     = flag.String("host", "127.0.0.1", "HTTP server host")
		port     = flag.Int("port", 3457, "HTTP server port")
		dataDir  = flag.String("data", defaultDataDir(), "Data directory for servers, backups and users")
		noAuth   = flag.Bool("no-auth", false, "Disable authentication (development only)")
		dataHost = flag.String("data-host", "", "Host-side path matching -data, when the panel itself runs in a container")
		origins  = flag.String("origin", "", "Extra hostnames allowed to open the console socket, comma separated (for a proxied deploy)")
		showVer  = flag.Bool("version", false, "Print version and exit")

		// Provisioning the first admin here skips the first-run setup flow and
		// its token entirely, which is how most deployments should run: the
		// account arrives with the deploy rather than being claimed afterwards.
		// Mirrors TEPLOY_DASH_USER / TEPLOY_DASH_PASSWORD in teploy-dash.
		adminUser = flag.String("admin-user", "", "Username for the first admin (env TEPLOY_ARCADE_ADMIN_USER; default \"admin\")")
		adminPass = flag.String("admin-password", "", "Password for the first admin (env TEPLOY_ARCADE_ADMIN_PASSWORD). Prefer the env var: a flag is visible in ps")
	)
	flag.Parse()

	if *showVer {
		log.SetFlags(0)
		log.Printf("teploy-arcade %s (%s, %s)", version, commit, date)
		return
	}

	// Env wins only where the flag was not given. A password on a command line
	// is readable by any process on the host via ps, so the env var is the
	// documented route and the flag exists for scripted one-offs.
	if *adminUser == "" {
		*adminUser = os.Getenv("TEPLOY_ARCADE_ADMIN_USER")
	}
	if *adminPass == "" {
		*adminPass = os.Getenv("TEPLOY_ARCADE_ADMIN_PASSWORD")
	}

	web, err := fs.Sub(frontendFS, "frontend")
	if err != nil {
		log.Fatalf("embedded frontend: %v", err)
	}

	if err := arcade.Run(arcade.Config{
		Host:     *host,
		Port:     *port,
		DataDir:  *dataDir,
		Web:      web,
		Version:  version,
		Commit:   commit,
		NoAuth:   *noAuth,
		Origins:  splitList(*origins),
		DataHost: *dataHost,

		AdminUser:     *adminUser,
		AdminPassword: *adminPass,
	}); err != nil {
		log.Fatal(err)
	}
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// defaultDataDir matches teploy-dash's /var/<product> convention when that path
// is writable, and falls back to the working directory for a local run.
func defaultDataDir() string {
	const sys = "/var/teploy-arcade"
	// 0o700, not 0o755: this tree holds users.json, audit.json and
	// mcp-tokens.json. Their own modes are 0o600, but everything else written
	// under it lands at the umask default, so a world-readable parent hands any
	// other account on the host the server files, backups and template set.
	if err := os.MkdirAll(sys, 0o700); err == nil {
		return sys
	}
	wd, err := os.Getwd()
	if err != nil {
		return "data"
	}
	return filepath.Join(wd, "data")
}
