package arcade

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Plugins and mods: what is installed, what is switched on, and installing one
// from a URL the operator supplies.
//
// Deliberately local. There is no Modrinth/Spigot catalogue browser here: it
// would be the only feature in the panel that needs the internet to render, and
// republishing someone else's index carries licence questions this project has
// not answered. The screen says so rather than offering a button that does
// nothing.
//
// Enabling and disabling is by extension - "x.jar" <-> "x.jar.disabled" -
// because that is what every panel and every loader already understands, and
// because it is reversible. Deleting to disable loses the jar and the version
// that was working with it.

const (
	jarExt      = ".jar"
	disabledExt = ".jar.disabled"

	// A jar is a zip, so every real one starts with the local file header magic.
	zipMagic = "PK\x03\x04"
)

// maxPluginBytes caps one download. A var rather than a const so the oversize
// guard can be exercised without pushing 64 MB through a test.
var maxPluginBytes int64 = 64 << 20

type PluginEntry struct {
	Name    string `json:"name"` // display name, without the extension
	File    string `json:"file"` // the actual name on disk, which is what the API takes
	Size    int64  `json:"size"`
	Mod     int64  `json:"mod"`
	Enabled bool   `json:"enabled"`
}

// PluginView is the whole screen in one payload, the way PlayerLists is.
type PluginView struct {
	Supported bool          `json:"supported"`
	Dir       string        `json:"dir"`
	Kind      string        `json:"kind"` // plugin | mod
	Reason    string        `json:"reason,omitempty"`
	Running   bool          `json:"running"`
	Note      string        `json:"note"`
	Entries   []PluginEntry `json:"entries"`
}

// pluginDirFor picks the one directory this server's loader actually reads.
//
// Showing both plugins/ and mods/ would be showing the operator a directory
// their server ignores: a Fabric server never loads plugins/, and dropping a
// Bukkit jar in there produces no error anywhere - it simply does nothing, and
// the operator spends the evening looking for the reason.
func pluginDirFor(s *Server) (dir, kind string, err error) {
	switch s.Template {
	case "paper", "spigot", "purpur", "bukkit", "velocity":
		return "plugins", "plugin", nil
	case "forge", "fabric", "neoforge", "quilt":
		return "mods", "mod", nil
	}
	return "", "", fmt.Errorf("the %s template has no plugin or mod loader, so there is nothing here to manage"+
		" (Paper, Spigot and Purpur take plugins; Forge and Fabric take mods)", s.Template)
}

// pluginNote answers the question every one of these actions raises. A stopped
// server picks the change up on its next start, so claiming a restart is needed
// would be telling an operator to restart something that is not running.
func pluginNote(running bool) string {
	if running {
		return "The server is running: restart it for this to take effect."
	}
	return "This takes effect the next time the server starts."
}

// validPluginFile refuses anything that is not a bare jar file name.
//
// The check has to happen on the name itself, before it is joined onto the
// plugin directory: path.Join("plugins", "../server.properties") collapses to
// "server.properties", which resolve() then accepts as a perfectly legal path
// inside the server root. The traversal never reaches the sandbox check because
// the join has already consumed it.
func validPluginFile(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("a file name is required")
	}
	if name != filepath.Base(name) || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("%q must be a file name inside the plugin directory, not a path", name)
	}
	if !strings.HasSuffix(name, jarExt) && !strings.HasSuffix(name, disabledExt) {
		return fmt.Errorf("%q is not a .jar", name)
	}
	return nil
}

func pluginBase(file string) string {
	return strings.TrimSuffix(strings.TrimSuffix(file, disabledExt), jarExt)
}

func (m *Manager) PluginView(s *Server) (PluginView, error) {
	running := s.State() == StatusRunning
	v := PluginView{Running: running, Note: pluginNote(running), Entries: []PluginEntry{}}

	dir, kind, err := pluginDirFor(s)
	if err != nil {
		// Not an error the caller can act on - it is the answer for this server,
		// and the screen renders it instead of a failed request.
		v.Reason = err.Error()
		return v, nil
	}
	v.Supported, v.Dir, v.Kind = true, dir, kind

	entries, err := m.listPluginDir(s, dir)
	if err != nil {
		return v, err
	}
	v.Entries = entries
	return v, nil
}

func (m *Manager) listPluginDir(s *Server, dir string) ([]PluginEntry, error) {
	r, name, err := m.rooted(s, dir)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	d, err := r.Open(name)
	if os.IsNotExist(err) {
		// mods/ is only created by the first install. An empty screen is the
		// right answer; an error here is one the operator cannot act on.
		return []PluginEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer d.Close()
	ents, err := d.ReadDir(-1)
	if os.IsNotExist(err) {
		// mods/ is only created by the first install. An empty screen is the
		// right answer; an error here is one the operator cannot act on.
		return []PluginEntry{}, nil
	}
	if err != nil {
		return nil, err
	}

	out := []PluginEntry{}
	for _, e := range ents {
		// Directories in here are the plugins' own config folders, not plugins.
		if e.IsDir() {
			continue
		}
		name := e.Name()
		enabled := strings.HasSuffix(name, jarExt)
		if !enabled && !strings.HasSuffix(name, disabledExt) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, PluginEntry{
			Name:    pluginBase(name),
			File:    name,
			Size:    info.Size(),
			Mod:     info.ModTime().Unix(),
			Enabled: enabled,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

func (m *Manager) statPlugin(r *os.Root, name, file string, enabled bool) (PluginEntry, error) {
	info, err := r.Stat(name)
	if err != nil {
		return PluginEntry{}, err
	}
	return PluginEntry{
		Name: pluginBase(file), File: file, Size: info.Size(),
		Mod: info.ModTime().Unix(), Enabled: enabled,
	}, nil
}

// SetPluginEnabled renames between "x.jar" and "x.jar.disabled". enable is the
// state the caller wants, not a flip, so a double-clicked button cannot leave
// the plugin in the state the operator was trying to leave.
func (m *Manager) SetPluginEnabled(s *Server, file string, enable bool) (PluginEntry, error) {
	dir, _, err := pluginDirFor(s)
	if err != nil {
		return PluginEntry{}, err
	}
	if err := validPluginFile(file); err != nil {
		return PluginEntry{}, err
	}
	// Same rule as the file API: a rename during the quiesce window lands the
	// jar in the archive under a name that no longer exists on disk.
	if m.backupLocked(s.ID) {
		return PluginEntry{}, fmt.Errorf("a backup is in progress; writes are blocked until it finishes")
	}

	base := pluginBase(file)
	from, to := base+disabledExt, base+jarExt
	if !enable {
		from, to = base+jarExt, base+disabledExt
	}

	r, err := m.serverRoot(s)
	if err != nil {
		return PluginEntry{}, err
	}
	defer r.Close()
	nameFrom, err := cleanRel(path.Join(dir, from))
	if err != nil {
		return PluginEntry{}, err
	}
	nameTo, err := cleanRel(path.Join(dir, to))
	if err != nil {
		return PluginEntry{}, err
	}

	if _, err := r.Lstat(nameFrom); err != nil {
		if _, err := r.Lstat(nameTo); err == nil {
			return m.statPlugin(r, nameTo, to, enable) // already where the caller wants it
		}
		return PluginEntry{}, fmt.Errorf("%s is not installed", base+jarExt)
	}
	// os.Rename replaces the destination without a word, so a stale
	// "x.jar.disabled" beside a live "x.jar" would silently eat one of them.
	if _, err := r.Lstat(nameTo); err == nil {
		return PluginEntry{}, fmt.Errorf("%s already exists; remove it first", to)
	}
	if err := r.Rename(nameFrom, nameTo); err != nil {
		return PluginEntry{}, friendlyFSError(err, path.Join(dir, to))
	}
	return m.statPlugin(r, nameTo, to, enable)
}

// DeletePlugin removes one jar. Path handling is DeletePath's, not a second
// copy of it: that is where the sandbox check, the "not the server directory
// itself" refusal and the backup-window check already live.
func (m *Manager) DeletePlugin(s *Server, file string) error {
	dir, _, err := pluginDirFor(s)
	if err != nil {
		return err
	}
	if err := validPluginFile(file); err != nil {
		return err
	}
	rel := path.Join(dir, file)

	// DeletePath is os.RemoveAll underneath, which returns nil for a path that
	// was never there - so without this the panel cheerfully confirms deleting
	// a plugin that does not exist, and an operator reads that as "removed".
	if _, err := m.StatRel(s, rel); err != nil {
		return fmt.Errorf("no plugin named %q is installed", file)
	}
	return m.DeletePath(s, rel)
}

// checkDownloadScheme keeps a download on http(s).
//
// Without it, "file:///var/teploy-arcade/users.json" makes the panel read a
// local file with its own privileges and publish it inside the server
// directory, where the file manager hands it straight back - an arbitrary
// local-file read for anyone holding the operator role. The check is explicit
// rather than leaning on the default transport happening to refuse file://:
// what a transport supports is not a security boundary.
func checkDownloadScheme(u *url.URL) error {
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		if u.Host == "" {
			return fmt.Errorf("that URL has no host")
		}
		return nil
	}
	return fmt.Errorf("only http and https downloads are allowed, not %q", u.Scheme)
}

// InstallPlugin downloads one jar into the server's plugin directory.
//
// This is the only place the panel fetches something a user named, so every
// guard below is load-bearing and each says what it prevents. It does not stop
// an operator pointing the panel at a host on its own network - they are
// trusted with the console, which is strictly more powerful - but it does stop
// a link turning into an arbitrary write, an arbitrary local read, or a full
// disk.
func (m *Manager) InstallPlugin(s *Server, rawURL string) (PluginEntry, error) {
	dir, _, err := pluginDirFor(s)
	if err != nil {
		return PluginEntry{}, err
	}

	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return PluginEntry{}, fmt.Errorf("that is not a URL: %w", err)
	}
	if err := checkDownloadScheme(u); err != nil {
		return PluginEntry{}, err
	}

	// The extension is the only thing that says what the downloaded bytes will
	// be used as. Without this check any URL is an arbitrary write into the
	// server directory: a replacement server.properties, a start.sh, a
	// whitelist.json with the attacker on it.
	name := path.Base(u.Path)
	if err := validPluginFile(name); err != nil || !strings.HasSuffix(name, jarExt) {
		return PluginEntry{}, fmt.Errorf("the URL must point at a .jar file (it ends in %q)", path.Base(u.Path))
	}

	r, err := m.serverRoot(s)
	if err != nil {
		return PluginEntry{}, err
	}
	defer r.Close()
	dirName, err := cleanRel(dir)
	if err != nil {
		return PluginEntry{}, err
	}
	if err := r.MkdirAll(dirName, 0o755); err != nil {
		return PluginEntry{}, err
	}
	target, err := cleanRel(path.Join(dir, name))
	if err != nil {
		return PluginEntry{}, err
	}
	if m.backupLocked(s.ID) {
		return PluginEntry{}, fmt.Errorf("a backup is in progress; writes are blocked until it finishes")
	}
	// Refused rather than overwritten: an overwrite is indistinguishable from a
	// fresh install afterwards, and it destroys the version that was working
	// while the game may still have the old one mapped.
	for _, p := range []string{target, target + ".disabled"} {
		if _, err := r.Lstat(p); err == nil {
			return PluginEntry{}, fmt.Errorf("%s is already installed; delete it first", name)
		}
	}

	client := &http.Client{
		// Without a deadline the panel holds a goroutine and a socket open for
		// as long as the far end cares to stall - a free way to pin the process
		// with a link that never finishes.
		Timeout: 5 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			// Checking only the URL the operator typed checks the wrong URL: a
			// trusted https host can answer "302 Location: file:///etc/shadow",
			// and the client would fetch that with the panel's privileges and
			// publish it as a plugin.
			return checkDownloadScheme(req.URL)
		},
	}
	resp, err := client.Get(u.String())
	if err != nil {
		return PluginEntry{}, fmt.Errorf("could not download %s: %w", u.Redacted(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return PluginEntry{}, fmt.Errorf("%s returned %s", u.Redacted(), resp.Status)
	}
	if resp.ContentLength > maxPluginBytes {
		return PluginEntry{}, fmt.Errorf("that file is %d MB; the limit is %d MB",
			resp.ContentLength>>20, maxPluginBytes>>20)
	}

	// The LimitReader is the cap that counts. Content-Length above is a claim
	// the far end makes and can simply omit, and without a real ceiling one
	// pasted link fills the data volume - which is shared by every server on
	// this host and by the panel's own state files.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPluginBytes+1))
	if err != nil {
		return PluginEntry{}, fmt.Errorf("download of %s failed: %w", u.Redacted(), err)
	}
	if int64(len(body)) > maxPluginBytes {
		return PluginEntry{}, fmt.Errorf("that download is larger than the %d MB limit", maxPluginBytes>>20)
	}
	// A 200 that carries an HTML login page or an error document would otherwise
	// land as a plausible .jar, and the game then fails to boot with a stack
	// trace that points at the plugin rather than at the download.
	if !bytes.HasPrefix(body, []byte(zipMagic)) {
		return PluginEntry{}, fmt.Errorf("%s did not return a jar file", u.Redacted())
	}

	// Temp file then rename. A half-written jar is still a jar as far as the
	// loader is concerned: it would be opened on the next start and fail there
	// instead of here.
	if err := writeAtomicIn(r, target, body, 0o644); err != nil {
		return PluginEntry{}, friendlyFSError(err, path.Join(dir, name))
	}
	return m.statPlugin(r, target, name, true)
}

// -------------------------------------------------------------------- API

func (a *API) RoutesPlugins(mux *http.ServeMux) {
	auth := a.mgr.auth
	// Reading the list is a read; switching a jar on or off is driving the
	// server. Nothing here creates or destroys a server, so nothing is admin.
	mux.HandleFunc("GET /api/servers/{id}/plugins", auth.require(RoleViewer, a.getPlugins))
	mux.HandleFunc("POST /api/servers/{id}/plugins/toggle", auth.require(RoleOperator, a.togglePlugin))
	mux.HandleFunc("DELETE /api/servers/{id}/plugins", auth.require(RoleOperator, a.deletePlugin))
	mux.HandleFunc("POST /api/servers/{id}/plugins/install", auth.require(RoleOperator, a.installPlugin))
}

func (a *API) getPlugins(w http.ResponseWriter, r *http.Request) {
	s := a.server(w, r)
	if s == nil {
		return
	}
	v, err := a.mgr.PluginView(s)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, v)
}

// pluginResult is the shape every mutation answers with, so the UI never has to
// guess whether the change is live yet.
func pluginResult(s *Server, e PluginEntry) map[string]any {
	running := s.State() == StatusRunning
	return map[string]any{
		"plugin": e, "requires_restart": running, "note": pluginNote(running),
	}
}

func (a *API) togglePlugin(w http.ResponseWriter, r *http.Request) {
	s := a.server(w, r)
	if s == nil {
		return
	}
	var body struct {
		File   string `json:"file"`
		Enable bool   `json:"enable"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, err)
		return
	}
	e, err := a.mgr.SetPluginEnabled(s, body.File, body.Enable)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	action := "plugin.disable"
	if body.Enable {
		action = "plugin.enable"
	}
	a.mgr.audit(actorOf(r), action, s.ID, e.File)
	writeJSON(w, 200, pluginResult(s, e))
}

func (a *API) deletePlugin(w http.ResponseWriter, r *http.Request) {
	s := a.server(w, r)
	if s == nil {
		return
	}
	file := r.URL.Query().Get("file")
	if err := a.mgr.DeletePlugin(s, file); err != nil {
		writeErr(w, 400, err)
		return
	}
	a.mgr.audit(actorOf(r), "plugin.delete", s.ID, file)
	running := s.State() == StatusRunning
	writeJSON(w, 200, map[string]any{
		"ok": true, "requires_restart": running, "note": pluginNote(running),
	})
}

func (a *API) installPlugin(w http.ResponseWriter, r *http.Request) {
	s := a.server(w, r)
	if s == nil {
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, err)
		return
	}
	e, err := a.mgr.InstallPlugin(s, body.URL)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	a.mgr.audit(actorOf(r), "plugin.install", s.ID, e.File)
	writeJSON(w, 201, pluginResult(s, e))
}
