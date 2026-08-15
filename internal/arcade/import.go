package arcade

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Importing a game server directory that already exists on this host.
//
// The panel is never the first thing to have managed these directories: other
// control panels and hand-written systemd units got there first, and the one
// that got there first may still be running the server right now. So the
// default mode copies the tree and does not write a single byte into the
// original - two panels driving one directory is exactly the desync this
// project exists to remove.
//
// Adopt-in-place is offered because a 2.8 GB world is a real cost to duplicate,
// but it is a deliberate second choice and both the API and the UI say what it
// gives up.
//
// Detection reports what it is sure of and says "unrecognised" otherwise. A
// wrong guess here is not cosmetic - it picks the server software, and the
// operator would only find out when the game refused to boot.

const (
	ImportCopy  = "copy"
	ImportAdopt = "adopt"

	ImportRunning = "running"
	ImportDone    = "done"
	ImportFailed  = "failed"
)

const (
	// A scan stats every file under the path to report a size, and the real
	// directories this was built for run to 2.8 GB across hundreds of thousands
	// of region files. Bound it: an operator typing a path by mistake ("/" is
	// one keystroke away from a home directory) must not pin a request for
	// minutes. A bounded walk reports a partial size, which is honest; an
	// unbounded one reports nothing at all.
	scanBudget   = 8 * time.Second
	scanMaxFiles = 400_000

	copyBufBytes = 512 << 10

	// Headroom demanded beyond the copy itself. Filling the panel's own disk
	// takes down every other server on it, not just the import.
	importFreeMargin = 64 << 20

	// A finished job is only kept so the UI can read the result it was
	// polling for.
	importJobTTL = time.Hour
)

// ImportScan is what a host path turned out to be. Every field is something
// read off the disk, not inferred: `recognised` false with a `reason` is a
// valid, useful answer and the UI shows it rather than a guess.
type ImportScan struct {
	Path       string `json:"path"`
	Name       string `json:"suggested_name"`
	NameSource string `json:"name_source"`

	IsServer   bool   `json:"is_server"`
	Recognised bool   `json:"recognised"`
	Reason     string `json:"reason"`

	Template     string   `json:"template"`
	TemplateName string   `json:"template_name"`
	Proxy        bool     `json:"proxy"`
	Version      string   `json:"version"`
	Jar          string   `json:"jar"`
	Jars         []string `json:"jars"`

	Port       int    `json:"port"`
	PortSource string `json:"port_source"`
	MOTD       string `json:"motd"`
	MaxPlayers int    `json:"max_players"`

	World    string `json:"world"`
	HasWorld bool   `json:"has_world"`
	Mods     int    `json:"mods"`
	Plugins  int    `json:"plugins"`

	Markers   []string `json:"markers"`
	ManagedBy []string `json:"managed_by"`

	SizeBytes   int64  `json:"size_bytes"`
	SizeHuman   string `json:"size_human"`
	Files       int    `json:"files"`
	SizePartial bool   `json:"size_partial"`

	FreeBytes   int64 `json:"free_bytes"`
	EnoughSpace bool  `json:"enough_space"`

	PortTakenBy string `json:"port_taken_by"`
	AdoptedBy   string `json:"adopted_by"`

	Warnings    []string `json:"warnings"`
	RuntimeNote string   `json:"runtime_note"`

	props map[string]string
}

// ImportRequest is the body of POST /api/import. Everything except Path has a
// scanned default, so the common case is a path and a name.
type ImportRequest struct {
	Path     string  `json:"path"`
	Name     string  `json:"name"`
	Mode     string  `json:"mode"`
	Template string  `json:"template"`
	Version  string  `json:"version"`
	Port     int     `json:"port"`
	Runtime  string  `json:"runtime"`
	MemoryMB int     `json:"memory_mb"`
	CPU      float64 `json:"cpu"`
}

// ImportJob is the progress record the UI polls. A multi-gigabyte copy cannot
// be an HTTP request: the browser gives up, the operator presses the button
// again, and two copies race into the same directory.
type ImportJob struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Mode     string `json:"mode"`
	Name     string `json:"name"`
	State    string `json:"state"`
	Total    int64  `json:"total_bytes"`
	Copied   int64  `json:"copied_bytes"`
	Percent  int    `json:"percent"`
	Current  string `json:"current_file"`
	Skipped  int    `json:"skipped_links"`
	ServerID string `json:"server_id"`
	Error    string `json:"error"`
	Started  int64  `json:"started"`
	Finished int64  `json:"finished"`
}

// ---------------------------------------------------------------- detection

// jarFamilies maps a marker in a jar's file name onto the server software it
// identifies. Ordered, because several real names carry more than one marker:
// "neoforge" contains "forge", and a Purpur build is a Paper fork whose name
// says purpur. First match wins, so the more specific entry comes first.
var jarFamilies = []struct {
	marker, slug, warn string
	proxy              bool
}{
	{marker: "velocity", slug: "velocity", proxy: true},
	{marker: "waterfall", slug: "velocity", proxy: true,
		warn: "This is a Waterfall proxy. Waterfall is deprecated upstream, so the panel records it as Velocity - the settings that differ are in the proxy's own config file, which was imported unchanged."},
	{marker: "bungeecord", slug: "velocity", proxy: true,
		warn: "This is a BungeeCord proxy. The panel records it as Velocity - its own config file was imported unchanged."},
	{marker: "purpur", slug: "purpur"},
	{marker: "paper", slug: "paper"},
	{marker: "craftbukkit", slug: "spigot",
		warn: "This is a CraftBukkit jar. The panel records it as Spigot, which is the closest template it ships."},
	{marker: "spigot", slug: "spigot"},
	{marker: "fabric", slug: "fabric"},
	{marker: "quilt", slug: "fabric",
		warn: "This is a Quilt server. The panel records it as Fabric, which is the closest template it ships."},
	{marker: "neoforge", slug: "forge",
		warn: "This is a NeoForge server. The panel records it as Forge, which is the closest template it ships."},
	{marker: "forge", slug: "forge"},
	{marker: "minecraft_server", slug: "vanilla"},
}

// Files that name whoever managed this directory before the panel did. Saying
// so is not decoration: adopting in place a directory another panel is still
// driving gives two controllers one server, and they will fight over its port,
// its process and its world.
var managedMarkers = map[string]string{
	"crafty_managed.txt":      "Crafty Controller",
	"mcss_server_config.json": "another control panel",
	".pterodactyl":            "Pterodactyl",
}

// Files worth showing the operator as evidence for what this directory is.
var notableFiles = map[string]bool{
	"server.properties": true, "velocity.toml": true, "forwarding.secret": true,
	"eula.txt": true, "bukkit.yml": true, "spigot.yml": true, "paper.yml": true,
	"paper-global.yml": true, "start.sh": true, "run.sh": true, "ops.json": true,
	"whitelist.json": true, "banned-players.json": true,
}

// jarVersion picks the game version out of a jar's file name:
// fabric-1.21.1.jar, minecraft_server.1.20.4.jar,
// forge-1.12.2-14.23.5.2860.jar (the first match is the Minecraft version, the
// second is Forge's own build number - which is why this takes the first).
var jarVersion = regexp.MustCompile(`\d+\.\d+(?:\.\d+)?`)

// classifyJar names the server software a jar file is, or reports that it
// cannot. "server.jar" is the case that matters: Crafty and half the tutorials
// on the internet rename the jar to that, and it says nothing about what is
// inside. Guessing there would pick the wrong template silently.
func classifyJar(name string) (slug, warn string, proxy bool) {
	low := strings.ToLower(name)
	for _, f := range jarFamilies {
		if strings.Contains(low, f.marker) {
			return f.slug, f.warn, f.proxy
		}
	}
	return "", "", false
}

// ---------------------------------------------------------------- scanning

// importSource vets a host path before anything reads, copies or links it.
func (m *Manager) importSource(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("give the full path of the server directory to import")
	}
	// A relative path would resolve against the panel process's working
	// directory, which is not the directory the operator is looking at - and
	// the resulting import would silently be of something else.
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%q is not an absolute path; give the full path to the server directory", path)
	}
	abs := filepath.Clean(path)
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r
	}
	info, err := os.Stat(abs)
	if os.IsNotExist(err) {
		return "", fmt.Errorf("there is nothing at %s", abs)
	}
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is a file, not a server directory", abs)
	}

	data := m.dataDir
	if a, err := filepath.Abs(data); err == nil {
		data = a
	}
	if r, err := filepath.EvalSymlinks(data); err == nil {
		data = r
	}
	// Both directions are refused. A source inside the data directory is a
	// server this panel already manages; a source that *contains* the data
	// directory would have the copy copying itself, and the destination grows
	// until the disk is full.
	if abs == data || strings.HasPrefix(abs, data+string(os.PathSeparator)) {
		return "", fmt.Errorf("%s is inside the panel's own data directory, so it is already managed here", abs)
	}
	if strings.HasPrefix(data, abs+string(os.PathSeparator)) {
		return "", fmt.Errorf("%s contains the panel's data directory; copying it would copy the copy", abs)
	}
	return abs, nil
}

// ScanImport reports what a host path is, without changing anything in it.
func (m *Manager) ScanImport(path string) (*ImportScan, error) {
	abs, err := m.importSource(path)
	if err != nil {
		return nil, err
	}

	sc := &ImportScan{
		Path:       abs,
		Name:       filepath.Base(abs),
		NameSource: "the directory name",
		Markers:    []string{},
		ManagedBy:  []string{},
		Warnings:   []string{},
		Jars:       []string{},
		props:      map[string]string{},
		RuntimeNote: "The panel runs servers on the simulator or the itzg/minecraft-server image; " +
			"it does not launch the jar in this directory directly. The imported world, plugins and server.properties are what get used.",
	}

	ents, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		name := e.Name()
		low := strings.ToLower(name)
		switch {
		case !e.IsDir() && strings.HasSuffix(low, ".jar"):
			sc.Jars = append(sc.Jars, name)
		case notableFiles[low]:
			sc.Markers = append(sc.Markers, name)
		}
		if who, ok := managedMarkers[low]; ok {
			sc.ManagedBy = append(sc.ManagedBy, who)
			sc.Markers = append(sc.Markers, name)
		}
	}
	sc.Mods = countJars(filepath.Join(abs, "mods"))
	sc.Plugins = countJars(filepath.Join(abs, "plugins"))

	m.readServerConfig(sc, abs)
	m.identify(sc)
	m.measure(sc, abs)
	m.crossCheck(sc)
	return sc, nil
}

// readServerConfig pulls the port, MOTD, player cap and world name out of the
// files the server itself keeps them in.
func (m *Manager) readServerConfig(sc *ImportScan, abs string) {
	if content, ok := readSmall(filepath.Join(abs, "server.properties")); ok {
		sc.props = parseProperties(content)
		sc.Port = atoi(sc.props["server-port"])
		if sc.Port > 0 {
			sc.PortSource = "server.properties"
		}
		sc.MOTD = sc.props["motd"]
		sc.MaxPlayers = atoi(sc.props["max-players"])
	}

	level := sc.props["level-name"]
	if level == "" {
		level = "world"
	}
	// A directory called "world" with no level.dat in it is a leftover, not a
	// world. Saying "world found" about one would tell the operator their save
	// came across when it did not.
	if _, err := os.Stat(filepath.Join(abs, level, "level.dat")); err == nil {
		sc.World, sc.HasWorld = level, true
	}
}

// identify decides which template this directory is, from the jars in it.
func (m *Manager) identify(sc *ImportScan) {
	if len(sc.Jars) == 0 {
		sc.Reason = "there is no .jar file here, so this is not a server directory the panel can import"
		return
	}
	sc.IsServer = true

	type hit struct {
		jar, slug, warn string
		proxy           bool
	}
	var hits []hit
	seen := map[string]bool{}
	for _, jar := range sc.Jars {
		slug, warn, proxy := classifyJar(jar)
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true
		hits = append(hits, hit{jar: jar, slug: slug, warn: warn, proxy: proxy})
	}

	// A modded loader ships WITH the vanilla server jar it runs on: Forge's
	// installer drops `forge-<ver>.jar` next to `minecraft_server.<ver>.jar`,
	// and Fabric does the same. That is one server and its dependency, not two
	// servers, but it looks identical to the ambiguous case below - so every
	// classic Forge modpack was refused with "holds jars for more than one
	// server software", which is most of what anyone actually migrates.
	//
	// Only vanilla is dropped, and only when something else is present to run:
	// paper+forge stays ambiguous, because that genuinely is two servers.
	if len(hits) > 1 {
		modded := make([]hit, 0, len(hits))
		for _, x := range hits {
			if x.slug != "vanilla" {
				modded = append(modded, x)
			}
		}
		if len(modded) == 1 {
			dropped := ""
			for _, x := range hits {
				if x.slug == "vanilla" {
					dropped = x.jar
				}
			}
			hits = modded
			sc.Warnings = append(sc.Warnings, fmt.Sprintf(
				"%s is here too, which is the vanilla server %s runs on rather than a second server. The panel is importing this as %s.",
				dropped, hits[0].jar, hits[0].slug))
		}
	}

	switch {
	case len(hits) == 0:
		sc.Jar = sc.Jars[0]
		sc.Reason = fmt.Sprintf("%q does not say which server software it is, so the panel will not guess - choose one", sc.Jars[0])
		return
	case len(hits) > 1:
		// Two different server jars in one directory is usually an upgrade that
		// left the old one behind, but it can equally be a directory holding two
		// servers. Either way the panel cannot know which one runs.
		names := make([]string, 0, len(hits))
		for _, x := range hits {
			names = append(names, x.jar)
		}
		sc.Reason = fmt.Sprintf("this directory holds jars for more than one server software (%s), so the panel will not guess which one runs - choose one",
			strings.Join(names, ", "))
		return
	}

	h := hits[0]
	sc.Template, sc.Jar, sc.Proxy, sc.Recognised = h.slug, h.jar, h.proxy, true
	if h.warn != "" {
		sc.Warnings = append(sc.Warnings, h.warn)
	}
	if t := templateBySlug(sc.Template); t != nil {
		sc.TemplateName = t.Name
	}
	if v := jarVersion.FindString(strings.TrimSuffix(sc.Jar, filepath.Ext(sc.Jar))); v != "" {
		sc.Version = v
	}
	if sc.Version == "" {
		// The jar itself knows. Paper, Purpur, Spigot and vanilla all ship a
		// version.json in the archive root naming the exact Minecraft version,
		// which is more reliable than any filename convention - "paper.jar"
		// carries nothing at all, and that is the common case for a server that
		// auto-updates in place.
		if v := versionFromJar(filepath.Join(sc.Path, sc.Jar)); v != "" {
			sc.Version = v
		}
	}
	if sc.Version == "" {
		// A loader jar often carries no version - Forge installers frequently
		// leave plain "forge.jar" - but the vanilla server it runs on is named
		// for the exact Minecraft version, and it is sitting right there. This
		// is not cosmetic: the version is what picks the JRE, and an unknown
		// version means a 1.16.5 pack is handed a Java 21 image it cannot start
		// on, failing inside the game's log where the panel cannot explain it.
		for _, j := range sc.Jars {
			if !strings.HasPrefix(strings.ToLower(j), "minecraft_server") {
				continue
			}
			if v := jarVersion.FindString(strings.TrimSuffix(j, filepath.Ext(j))); v != "" {
				sc.Version = v
				sc.Warnings = append(sc.Warnings, fmt.Sprintf(
					"%q carries no version, so the panel read %s from %q, the vanilla server it runs on.",
					sc.Jar, v, j))
				break
			}
		}
	}

	// A proxy's port lives in its own config. Its server.properties, when it has
	// one at all, is a leftover from whatever the directory used to be, and
	// taking the port from that puts the panel's ledger on a port nothing is
	// listening on.
	if sc.Proxy {
		if content, ok := readSmall(filepath.Join(sc.Path, "velocity.toml")); ok {
			if p := velocityBind(content); p > 0 {
				sc.Port, sc.PortSource = p, "velocity.toml"
			}
		}
	}
}

// measure sizes the tree and works out whether a copy would fit.
func (m *Manager) measure(sc *ImportScan, abs string) {
	// measureTree reports *complete*, and SizePartial is its opposite. Assigning
	// one to the other made every fully-measured scan claim it had given up
	// early, so a four-file directory reported "at least 117 B" and the space
	// check below was reasoning about a number the UI had just called a floor.
	var complete bool
	sc.SizeBytes, sc.Files, complete = measureTree(abs)
	sc.SizePartial = !complete
	sc.SizeHuman = humanSize(sc.SizeBytes)
	if sc.SizePartial {
		sc.SizeHuman = "at least " + sc.SizeHuman
	}
	if free, err := diskFree(m.dataDir); err == nil {
		sc.FreeBytes = free
		sc.EnoughSpace = free >= sc.SizeBytes+importFreeMargin
	} else {
		// Unknown free space is not the same as none. Let the copy try and fail
		// with the filesystem's own error rather than refusing on a guess.
		sc.EnoughSpace = true
	}
}

// crossCheck compares what was found against the panel's own state and against
// what a server of this kind ought to have.
func (m *Manager) crossCheck(sc *ImportScan) {
	if !sc.IsServer {
		return
	}
	if name, source := serverNameFrom(sc.Path); name != "" {
		sc.Name, sc.NameSource = name, source
	}
	if sc.Port > 0 {
		if owner := m.portOwner(sc.Port); owner != nil {
			sc.PortTakenBy = owner.Name
			sc.Warnings = append(sc.Warnings, fmt.Sprintf(
				"port %d is already used by %q in this panel; pick a different port or the import is refused", sc.Port, owner.Name))
		}
	}
	if s := m.serverAdoptedFrom(sc.Path); s != nil {
		sc.AdoptedBy = s.Name
		sc.Warnings = append(sc.Warnings, fmt.Sprintf(
			"%q in this panel is already this exact directory, adopted in place", s.Name))
	}
	for _, who := range sc.ManagedBy {
		sc.Warnings = append(sc.Warnings, fmt.Sprintf(
			"%s manages this directory. Copy it rather than adopting it in place, or two panels will drive one server.", who))
	}
	if sc.Recognised && sc.Version == "" {
		sc.Warnings = append(sc.Warnings, fmt.Sprintf(
			"%q does not carry a version, so the panel records the version as unknown", sc.Jar))
	}
	if sc.IsServer && !sc.HasWorld && !sc.Proxy {
		sc.Warnings = append(sc.Warnings,
			"no world with a level.dat was found, so the game generates a new one the first time it starts")
	}
	if strings.Contains(strings.ToLower(sc.Jar), "installer") {
		sc.Warnings = append(sc.Warnings, fmt.Sprintf(
			"%q looks like an installer rather than the server itself", sc.Jar))
	}
	if content, ok := readSmall(filepath.Join(sc.Path, "eula.txt")); ok {
		if !strings.Contains(strings.ToLower(strings.ReplaceAll(content, " ", "")), "eula=true") {
			sc.Warnings = append(sc.Warnings, "eula.txt does not say eula=true; the game refuses to start until it does")
		}
	}
	if !sc.EnoughSpace {
		sc.Warnings = append(sc.Warnings, fmt.Sprintf(
			"copying needs %s and only %s is free on the panel's disk", humanSize(sc.SizeBytes+importFreeMargin), humanSize(sc.FreeBytes)))
	}
}

// versionFromJar reads the Minecraft version out of a server jar.
//
// version.json sits in the archive root and carries {"id": "26.1.2", ...}. This
// matters beyond cosmetics: the version selects the JRE, and a server recorded
// as "unknown" gets whatever the untagged image happens to ship.
//
// Deliberately tolerant - a jar that is not a zip, has no version.json, or has
// one that does not parse simply yields nothing and the caller falls back.
func versionFromJar(path string) string {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return ""
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name != "version.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return ""
		}
		// Bounded: this is an attacker-influenced archive, and the file we
		// want is a few hundred bytes.
		b, err := io.ReadAll(io.LimitReader(rc, 64<<10))
		rc.Close()
		if err != nil {
			return ""
		}
		var v struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if json.Unmarshal(b, &v) != nil {
			return ""
		}
		if v.ID != "" {
			return v.ID
		}
		return v.Name
	}
	return ""
}

// serverNameFrom recovers the name the previous panel gave this server, so an
// import does not silently rename a server to whatever directory it happens to
// live in. Keyed on the config filename another panel leaves behind - a file
// signature, the same way the markers above work.
func serverNameFrom(dir string) (string, string) {
	content, ok := readSmall(filepath.Join(dir, "mcss_server_config.json"))
	if !ok {
		return "", ""
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(content), &raw); err != nil {
		return "", ""
	}
	for k, v := range raw {
		switch strings.ToLower(strings.ReplaceAll(k, "_", "")) {
		case "name", "servername":
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s), "mcss_server_config.json"
			}
		}
	}
	return "", ""
}

// serverAdoptedFrom finds a panel server whose directory is a link to dir.
func (m *Manager) serverAdoptedFrom(dir string) *Server {
	for _, s := range m.List() {
		link := filepath.Join(m.dataDir, "servers", s.ID)
		if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		if target, err := filepath.EvalSymlinks(link); err == nil && target == dir {
			return s
		}
	}
	return nil
}

// ------------------------------------------------------------------- import

// StartImport validates everything it can synchronously, then does the slow
// part in the background and hands back a job to poll.
func (m *Manager) StartImport(req ImportRequest, actor string) (*ImportJob, error) {
	sc, err := m.ScanImport(req.Path)
	if err != nil {
		return nil, err
	}
	if !sc.IsServer {
		return nil, fmt.Errorf("%s", sc.Reason)
	}

	mode := req.Mode
	if mode == "" {
		mode = ImportCopy
	}
	if mode != ImportCopy && mode != ImportAdopt {
		return nil, fmt.Errorf("mode must be %q or %q", ImportCopy, ImportAdopt)
	}
	if mode == ImportAdopt && sc.AdoptedBy != "" {
		return nil, fmt.Errorf("%q already is this directory; adopting it twice would give two panel entries one server", sc.AdoptedBy)
	}

	slug := req.Template
	if slug == "" {
		slug = sc.Template
	}
	if slug == "" {
		return nil, fmt.Errorf("%s", sc.Reason)
	}
	tpl := templateBySlug(slug)
	if tpl == nil {
		return nil, fmt.Errorf("unknown template %q", slug)
	}

	port := req.Port
	if port == 0 {
		port = sc.Port
	}
	if port == 0 {
		port = m.NextFreePort(tpl.PortHint)
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("port %d is out of range", port)
	}
	// Checked against every server, not only the running ones: a stopped server
	// keeps its port in the panel's ledger, and letting two entries hold one
	// port turns into a failed bind the next time either is started.
	// Claimed, not merely checked: a copy import registers its server minutes
	// later on a background goroutine, so a bare check let concurrent imports
	// all pass and land on the same port.
	if holder, ok := m.claimPort(port, strings.TrimSpace(req.Name)); !ok {
		return nil, fmt.Errorf("port %d is already used by %q; give a different port to import this server", port, holder)
	}
	// Validation continues below and can still refuse the import. Hand the
	// claim back on every one of those paths, or a rejected import leaves the
	// port blocked until the panel restarts. Ownership passes to the import
	// itself at the point of no return, which clears this.
	claimHeld := true
	defer func() {
		if claimHeld {
			m.releasePort(port)
		}
	}()

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = sc.Name
	}
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}

	runtime := req.Runtime
	if runtime != RuntimeDocker {
		runtime = RuntimeSim
	}
	if runtime == RuntimeDocker && !dockerAvailable() {
		return nil, fmt.Errorf("docker is not reachable on this host")
	}

	version := req.Version
	if version == "" {
		version = sc.Version
	}
	if version == "" {
		// Better a version the panel admits it does not know than one it
		// invented from the template's default list.
		version = "unknown"
	}

	// Same bounds as Create. Import reaches the resource fields by a different
	// route, and a limit that only holds on one of the two ways to make a
	// server is not a limit.
	if err := checkServerLimits(port, req.MemoryMB, req.CPU); err != nil {
		return nil, err
	}

	s := m.newServer(name, tpl, version, port, runtime)
	// Run the jar this directory already has, not one matched by version.
	s.LaunchJar = sc.Jar
	if req.MemoryMB > 0 {
		s.MemoryMB = req.MemoryMB
	}
	if req.CPU > 0 {
		s.CPU = req.CPU
	}
	applyImportedProps(s, sc, port)

	dst := filepath.Join(m.dataDir, "servers", s.ID)
	if _, err := os.Lstat(dst); err == nil {
		return nil, fmt.Errorf("%s already exists; the panel will not import on top of it", dst)
	}
	if mode == ImportCopy && !sc.EnoughSpace {
		return nil, fmt.Errorf("copying %s needs %s free and only %s is left on the panel's disk; import it in place instead",
			sc.Path, humanSize(sc.SizeBytes+importFreeMargin), humanSize(sc.FreeBytes))
	}

	if mode == ImportAdopt {
		// One symlink, so it finishes inside the request. It still reports a job
		// so the UI has one flow for both modes rather than two.
		if err := adoptInPlace(dst, sc.Path); err != nil {
			return nil, err
		}
		job := newImportJob(sc, mode, name)
		claimHeld = false // the server now holds the port
		m.finishImport(job, s, sc, actor)
		view := job.view()
		return &view, nil
	}

	job := newImportJob(sc, mode, name)
	claimHeld = false // the goroutine owns the claim now: it releases on failure
	go func() {
		defer recoverPanic("import copy of " + sc.Path)
		if err := copyTree(sc.Path, dst, job); err != nil {
			// A half-copied tree is a server that boots on a truncated world.
			// Leave nothing rather than something that looks importable.
			_ = os.RemoveAll(dst)
			// Release the claim, or a failed import blocks that port until the
			// panel restarts.
			m.releasePort(port)
			job.fail(friendlyFSError(err, "the imported server"))
			return
		}
		m.finishImport(job, s, sc, actor)
	}()

	view := job.view()
	return &view, nil
}

// applyImportedProps folds the imported server.properties over the template
// defaults.
//
// Every scanned key is kept, including ones the panel has no schema for.
// writeProps rewrites the whole file from this map the first time anyone saves
// a setting, so a key dropped here is a key deleted out of the operator's own
// server.properties later - rcon credentials, level-type, resource packs -
// with nothing on screen to connect the two events.
//
// The server is not in the manager's map yet, so nothing else can see it and no
// lock is needed.
func applyImportedProps(s *Server, sc *ImportScan, port int) {
	for k, v := range sc.props {
		s.Props[k] = v
	}
	s.Props["server-port"] = itoa(port)
	s.Port = port
	if mp := atoi(s.Props["max-players"]); mp > 0 {
		s.MaxPlayers = mp
	}
}

// finishImport registers the imported server with the manager.
//
// Deliberately not Manager.Create: Create seeds a fresh tree - its own
// server.properties, an empty ops.json, eula.txt - straight over the files that
// were just imported. Registration is the only part of it an import wants.
func (m *Manager) finishImport(j *importJob, s *Server, sc *ImportScan, actor string) {
	m.mu.Lock()
	m.servers[s.ID] = s
	m.order = append(m.order, s.ID)
	delete(m.reservedPorts, s.Port) // the server itself now holds it
	m.mu.Unlock()

	if err := m.Save(); err != nil {
		log.Printf("imported %s but could not persist the server list: %v", s.Name, err)
	}
	// The panel's model now holds the chosen port, but the imported
	// server.properties still carries the source's. Left alone they disagree
	// until someone happens to save settings, and the game binds the old one.
	if err := m.writeProps(s); err != nil {
		log.Printf("imported %s but could not write server.properties: %v", s.Name, err)
	}

	m.audit(actor, "server.import", s.ID, fmt.Sprintf("%s from %s (%s, %s)",
		s.Name, sc.Path, j.mode(), humanSize(sc.SizeBytes)))
	m.broadcastEvent("server.created", s.ID)
	j.done(s.ID)
}

// adoptInPlace points the panel's server directory at the operator's own tree
// instead of copying it.
//
// The link is what keeps adoption honest about not moving anything. resolve()
// evaluates the root before it sandboxes a path, so the file manager reads and
// writes the real directory and still cannot escape it; and Delete removes the
// link by path, so deleting the panel entry leaves the operator's server
// exactly where it was.
func adoptInPlace(link, target string) error {
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return err
	}
	return os.Symlink(target, link)
}

// copyTree copies src into dst, reporting progress as it goes.
//
// Symlinks are skipped rather than followed, the same rule the backup archiver
// uses: a link in somebody else's server directory can point anywhere, and
// following one copies a tree from outside the import - or, pointed at an
// ancestor, loops until the disk is full.
func copyTree(src, dst string, j *importJob) error {
	// An import copies the directory as it found it: the operator pointed at
	// this tree and everything in it is theirs. Clone is the caller with
	// things to leave behind - see cloneSkip.
	return copyTreeFiltered(src, dst, j, nil)
}

// copyTreeFiltered copies src to dst, skipping what skip reports. A skipped
// directory is not descended into.
func copyTreeFiltered(src, dst string, j *importJob, skip func(rel string, d fs.DirEntry) bool) error {
	buf := make([]byte, copyBufBytes)
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if skip != nil && rel != "." && skip(rel, d) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			j.skip()
			return nil
		}
		target := filepath.Join(dst, rel)

		if d.IsDir() {
			perm := os.FileMode(0o755)
			if info, err := d.Info(); err == nil {
				perm = info.Mode().Perm()
			}
			return os.MkdirAll(target, perm)
		}
		if !d.Type().IsRegular() {
			j.skip()
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		j.setCurrent(rel)
		return copyFile(p, target, info.Mode().Perm(), buf, j)
	})
}

func copyFile(src, dst string, perm os.FileMode, buf []byte, j *importJob) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	// O_EXCL: the destination was verified empty before the copy started, so
	// anything already at this path means a second import is writing into the
	// same tree and the result would be two servers interleaved.
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	for {
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				out.Close()
				return werr
			}
			j.advance(int64(n))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			return rerr
		}
	}
	return out.Close()
}

// ---------------------------------------------------------------- job state

// Jobs live in memory only. There is nothing to resume after a restart - the
// server record is created only once every byte is across - and nothing to
// persist about a copy that no longer exists. Package-level for the same reason
// the loaded template set is: it is process-wide state with one owner.
var importJobs = struct {
	mu   sync.Mutex
	jobs map[string]*importJob
}{jobs: map[string]*importJob{}}

type importJob struct {
	mu     sync.Mutex
	pub    ImportJob
	copied atomic.Int64
}

func newImportJob(sc *ImportScan, mode, name string) *importJob {
	id, err := randomHex(8)
	if err != nil {
		// Falling back to a predictable id would let one operator poll another's
		// import; a clock-derived one is not guessable enough to matter here but
		// the failure is worth recording.
		log.Printf("import job id came from the clock, not crypto/rand: %v", err)
		id = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	j := &importJob{pub: ImportJob{
		ID: id, Path: sc.Path, Mode: mode, Name: name,
		State: ImportRunning, Total: sc.SizeBytes, Started: time.Now().Unix(),
	}}

	importJobs.mu.Lock()
	defer importJobs.mu.Unlock()
	cutoff := time.Now().Add(-importJobTTL).Unix()
	for key, old := range importJobs.jobs {
		v := old.view()
		if v.Finished > 0 && v.Finished < cutoff {
			delete(importJobs.jobs, key)
		}
	}
	importJobs.jobs[j.pub.ID] = j
	return j
}

func importJobByID(id string) (*importJob, bool) {
	importJobs.mu.Lock()
	defer importJobs.mu.Unlock()
	j, ok := importJobs.jobs[id]
	return j, ok
}

func (j *importJob) view() ImportJob {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := j.pub
	out.Copied = j.copied.Load()
	switch {
	case out.State == ImportDone:
		out.Percent = 100
	case out.Total > 0:
		out.Percent = int(out.Copied * 100 / out.Total)
		if out.Percent > 99 {
			out.Percent = 99 // 100% belongs to the server record existing, not to the last byte landing
		}
	}
	return out
}

func (j *importJob) mode() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.pub.Mode
}

// advance is the only progress call on the per-chunk path, so it stays off the
// mutex: a 2.8 GB copy makes six thousand of these per second.
func (j *importJob) advance(n int64) { j.copied.Add(n) }

func (j *importJob) setCurrent(rel string) {
	j.mu.Lock()
	j.pub.Current = rel
	j.mu.Unlock()
}

func (j *importJob) skip() {
	j.mu.Lock()
	j.pub.Skipped++
	j.mu.Unlock()
}

func (j *importJob) fail(err error) {
	j.mu.Lock()
	j.pub.State = ImportFailed
	j.pub.Error = err.Error()
	j.pub.Finished = time.Now().Unix()
	j.mu.Unlock()
}

func (j *importJob) done(serverID string) {
	j.mu.Lock()
	j.pub.State = ImportDone
	j.pub.ServerID = serverID
	j.pub.Current = ""
	j.pub.Finished = time.Now().Unix()
	j.mu.Unlock()
}

// ----------------------------------------------------------------- helpers

// readSmall reads a config file, ignoring anything too large to be one. A
// server directory can contain a 4 GB region file named like a config by
// accident; reading it into memory to look for "eula=true" would take the panel
// down with it.
func readSmall(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() > maxEditBytes {
		return "", false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// parseProperties reads key=value lines the way the game does: '#' comments and
// blank lines ignored, first '=' splits.
func parseProperties(content string) map[string]string {
	out := map[string]string{}
	for _, ln := range strings.Split(content, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") || strings.HasPrefix(ln, "!") {
			continue
		}
		k, v, ok := strings.Cut(ln, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

// velocityBind reads the listen port out of velocity.toml: bind = "0.0.0.0:25565".
func velocityBind(content string) int {
	for _, ln := range strings.Split(content, "\n") {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "bind") {
			continue
		}
		_, v, ok := strings.Cut(ln, "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if i := strings.LastIndex(v, ":"); i >= 0 {
			v = v[i+1:]
		}
		if p := atoi(v); p > 0 {
			return p
		}
	}
	return 0
}

func countJars(dir string) int {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".jar") {
			n++
		}
	}
	return n
}

// measureTree sums the regular files under root, bounded in both time and
// count. complete is false when it stopped early, which the caller reports as
// "at least" rather than pretending the number is the whole tree.
func measureTree(root string) (bytes int64, files int, complete bool) {
	deadline := time.Now().Add(scanBudget)
	complete = true
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		// One unreadable subdirectory (a plugin's cache owned by another user)
		// is not a reason to abandon the whole measurement.
		if err != nil {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		bytes += info.Size()
		files++
		if files >= scanMaxFiles || (files%2000 == 0 && time.Now().After(deadline)) {
			complete = false
			return filepath.SkipAll
		}
		return nil
	})
	return bytes, files, complete
}

// diskFree is a seam so the out-of-space refusal is reachable in a test; every
// production caller measures the real filesystem.
var diskFree = freeSpace

func freeSpace(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// -------------------------------------------------------------------- HTTP

// RoutesImport registers the import surface. All three are admin: a scan reads
// any directory on the host that the panel process can read, and an import
// creates a server - the same act createServer is gated for.
func (a *API) RoutesImport(mux *http.ServeMux) {
	auth := a.mgr.auth
	mux.HandleFunc("POST /api/import/scan", auth.require(RoleAdmin, a.importScan))
	mux.HandleFunc("POST /api/import", auth.require(RoleAdmin, a.importStart))
	mux.HandleFunc("GET /api/import/{job}", auth.require(RoleAdmin, a.importStatus))
	// A clone reports through the import job, so it belongs with them.
	mux.HandleFunc("POST /api/clone", auth.require(RoleAdmin, a.cloneStart))
}

func (a *API) cloneStart(w http.ResponseWriter, r *http.Request) {
	var req CloneRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	job, err := a.mgr.StartClone(req, actorOf(r))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 202, job)
}

func (a *API) importScan(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, err)
		return
	}
	sc, err := a.mgr.ScanImport(body.Path)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, sc)
}

func (a *API) importStart(w http.ResponseWriter, r *http.Request) {
	var req ImportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	job, err := a.mgr.StartImport(req, actorOf(r))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	// 202: the record does not exist yet for a copy, and the caller polls.
	writeJSON(w, 202, job)
}

func (a *API) importStatus(w http.ResponseWriter, r *http.Request) {
	j, ok := importJobByID(r.PathValue("job"))
	if !ok {
		writeErr(w, 404, fmt.Errorf("no such import job"))
		return
	}
	writeJSON(w, 200, j.view())
}
