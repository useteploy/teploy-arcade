package arcade

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// Phase 5: the file API.
//
// Every operation runs against an *os.Root - a directory handle for the
// server's own directory - so confinement is enforced by the kernel, one path
// component at a time, by the same syscall that performs the operation.
//
// This replaced a resolve-then-operate model: check the path string, then make
// the syscall on it. That has an unavoidable gap between the check and the use,
// and a game server's directory is writable by the game process and its
// plugins, so a directory component could be swapped for a symlink inside the
// gap and the operation would land outside the tree the check had approved.
// There is no such gap here: os.Root re-derives each component from the held
// handle and refuses one that leaves the root, so there is no window to win.
//
// cleanRel still applies the panel's own *policy* on top (refuse "..", refuse
// absolute paths) - that is about being predictable to an operator, not about
// security, and is documented as such on the function.

const (
	maxEditBytes = 2 << 20 // 2 MB - above this the browser editor is the wrong tool
	maxListItems = 2000
)

var errOutsideRoot = errors.New("path escapes the server directory")

// friendlyFSError turns a syscall into something an operator can act on, and
// keeps the panel's internal paths (and the .tmp file it writes through) out
// of the message.
func friendlyFSError(err error, rel string) error {
	switch {
	case errors.Is(err, syscall.ENOSPC):
		return fmt.Errorf("the disk is full, so %s was not saved (the previous version is untouched)", rel)
	case errors.Is(err, syscall.EROFS):
		return fmt.Errorf("the filesystem is read-only, so %s was not saved", rel)
	case errors.Is(err, syscall.EACCES), errors.Is(err, os.ErrPermission):
		return fmt.Errorf("permission denied writing %s", rel)
	case errors.Is(err, syscall.EDQUOT):
		return fmt.Errorf("the disk quota is exhausted, so %s was not saved", rel)
	case errors.Is(err, syscall.ELOOP):
		return fmt.Errorf("%s was not saved: something in the server directory has left a symbolic link in the way", rel)
	}
	return fmt.Errorf("could not write %s: %w", rel, fsCause(err))
}

// fsCause strips the filesystem path off an os error, leaving the reason.
//
// The default branch above used to wrap the error whole, which undid the
// sentence directly above it: *os.PathError and *os.LinkError both carry the
// path the syscall was made on, and for a write that path is the randomly-named
// temp file this package writes through - deleted before the message is
// rendered, never anything the operator asked for, and the internal layout of
// the data directory besides. Unwrapping to the cause keeps errors.Is working
// on the errno for callers while the path stays inside.
func fsCause(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) && pe.Err != nil {
		return pe.Err
	}
	var le *os.LinkError
	if errors.As(err, &le) && le.Err != nil {
		return le.Err
	}
	return err
}

// serverDir is the on-disk home for one server. Bind-mounted into the container
// for the docker runtime, so the file manager works identically for both.
func (m *Manager) serverDir(s *Server) string {
	p := filepath.Join(m.dataDir, "servers", s.ID)
	// Resolved because an adopted-in-place server's directory is a link to the
	// operator's own tree: filepath.Walk will not descend a link root, so an
	// unresolved path here makes a backup report success over an empty archive.
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func (m *Manager) ensureServerDir(s *Server) (string, error) {
	dir := m.serverDir(s)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// cleanRel applies the panel's policy to a user-supplied path and returns a
// name to use with an *os.Root.
//
// This is policy, not the sandbox. os.Root is the sandbox: it refuses anything
// that leaves the root regardless of what this function lets through. What this
// adds is predictability - traversal is *refused* rather than resolved, even
// when it would land back inside the root. "world/../server.properties" is a
// legal path that os.Root would happily serve, but someone who types it is
// usually working from a mistaken idea of where they are, and a quiet success
// at a different path than they meant is worse than an error.
func cleanRel(rel string) (string, error) {
	rel = strings.ReplaceAll(rel, `\`, "/")
	rel = strings.TrimPrefix(rel, "./")

	// Only "" and "/" address the server root itself; any other absolute path
	// is a caller mistake or an attack, never a legitimate request.
	if rel == "" || rel == "/" {
		return ".", nil
	}
	if strings.HasPrefix(rel, "/") {
		return "", errOutsideRoot
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return "", errOutsideRoot
		}
	}
	c := path.Clean(rel)
	if c == "." || c == "/" {
		return ".", nil
	}
	return c, nil
}

// serverRoot opens the server's directory as a handle. Callers must Close it.
//
// Every file operation opens its own root rather than caching one on the
// Manager: a cached handle keeps referencing the original directory after an
// adopted server's tree is moved or replaced, which is exactly the confusion
// the panel is supposed to save an operator from.
func (m *Manager) serverRoot(s *Server) (*os.Root, error) {
	dir, err := m.ensureServerDir(s)
	if err != nil {
		return nil, err
	}
	return os.OpenRoot(dir)
}

// rootedPath is the (root, name) pair every operation below works from.
func (m *Manager) rooted(s *Server, rel string) (*os.Root, string, error) {
	name, err := cleanRel(rel)
	if err != nil {
		return nil, "", err
	}
	r, err := m.serverRoot(s)
	if err != nil {
		return nil, "", err
	}
	return r, name, nil
}

// writeAtomicIn writes through a uniquely-named temp file beside the target and
// renames, so a failure leaves the previous file intact rather than truncated.
//
// The unique name is load-bearing twice over: a fixed "<name>.tmp" is a path a
// plugin can symlink ahead of the write, and the rename afterwards republishes
// that link under a name the panel trusts; and two concurrent writers to one
// file both used it, so one removed the other's temp and the loser failed with
// ENOENT. O_CREATE|O_EXCL refuses an existing path of any kind, symlink
// included, which closes both.
func writeAtomicIn(r *os.Root, name string, data []byte, perm os.FileMode) error {
	dir := path.Dir(name)
	if dir == "." {
		dir = ""
	}
	suffix, err := randomHex(8)
	if err != nil {
		return err
	}
	tmp := path.Join(dir, "."+path.Base(name)+"."+suffix+".tmp")

	f, err := r.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	cleanup := func() { _ = r.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		f.Close()
		cleanup()
		return err
	}
	// Flushed and closed before the rename, and both errors checked: a
	// discarded Close is how a truncated file gets published under a name the
	// operator believes is complete.
	if err := f.Sync(); err != nil {
		f.Close()
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return err
	}
	if err := r.Rename(tmp, name); err != nil {
		cleanup()
		return err
	}
	return nil
}

type FileEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
	Mod   int64  `json:"mod"`
	Text  bool   `json:"text"`  // safe to open in the browser editor
	Limit bool   `json:"limit"` // too big to edit
}

var textExt = map[string]bool{
	".properties": true, ".txt": true, ".json": true, ".yml": true, ".yaml": true,
	".toml": true, ".cfg": true, ".conf": true, ".log": true, ".md": true,
	".sh": true, ".sk": true, ".csv": true, ".ini": true, ".xml": true, "": false,
}

func isTextFile(name string, size int64) bool {
	if size > maxEditBytes {
		return false
	}
	return textExt[strings.ToLower(filepath.Ext(name))]
}

func (m *Manager) ListFiles(s *Server, rel string) ([]FileEntry, error) {
	r, name, err := m.rooted(s, rel)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	d, err := r.Open(name)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	ents, err := d.ReadDir(-1)
	if err != nil {
		return nil, err
	}

	out := make([]FileEntry, 0, len(ents))
	for i, e := range ents {
		if i >= maxListItems {
			break
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		rp := filepath.ToSlash(filepath.Join(strings.TrimPrefix(rel, "/"), e.Name()))
		out = append(out, FileEntry{
			Name: e.Name(), Path: rp, Dir: e.IsDir(),
			Size: info.Size(), Mod: info.ModTime().Unix(),
			Text:  !e.IsDir() && isTextFile(e.Name(), info.Size()),
			Limit: !e.IsDir() && info.Size() > maxEditBytes,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

func (m *Manager) ReadFile(s *Server, rel string) (string, error) {
	r, name, err := m.rooted(s, rel)
	if err != nil {
		return "", err
	}
	defer r.Close()

	f, err := r.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	// Type and size come from the open descriptor rather than a second stat of
	// the path, which could by then describe a different file.
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory", rel)
	}
	if info.Size() > maxEditBytes {
		return "", fmt.Errorf("%s is %d KB; files above %d KB are download-only",
			rel, info.Size()/1024, maxEditBytes/1024)
	}
	b, err := io.ReadAll(f)
	return string(b), err
}

func (m *Manager) WriteFile(s *Server, rel, content string) error {
	r, name, err := m.rooted(s, rel)
	if err != nil {
		return err
	}
	defer r.Close()

	// Refuse writes while a backup is quiescing the world, so a snapshot can
	// never catch a half-written file (PLAN.md §8).
	if m.backupLocked(s.ID) {
		return fmt.Errorf("a backup is in progress; writes are blocked until it finishes")
	}
	if dir := path.Dir(name); dir != "." {
		if err := r.MkdirAll(dir, 0o755); err != nil {
			return friendlyFSError(err, rel)
		}
	}
	if err := writeAtomicIn(r, name, []byte(content), 0o644); err != nil {
		return friendlyFSError(err, rel)
	}

	// server.properties is the panel's own model too - keep them in step rather
	// than letting the file and the settings screen disagree.
	if path.Base(name) == "server.properties" {
		m.reloadProps(s, content)
	}
	return nil
}

// StatRel reports on a path inside the server directory, resolved through the
// same sandbox as every other file operation.
func (m *Manager) StatRel(s *Server, rel string) (os.FileInfo, error) {
	r, name, err := m.rooted(s, rel)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return r.Stat(name)
}

func (m *Manager) DeletePath(s *Server, rel string) error {
	r, name, err := m.rooted(s, rel)
	if err != nil {
		return err
	}
	defer r.Close()
	if name == "." {
		return fmt.Errorf("refusing to delete the server directory itself")
	}
	if m.backupLocked(s.ID) {
		return fmt.Errorf("a backup is in progress; writes are blocked until it finishes")
	}
	return r.RemoveAll(name)
}

func (m *Manager) MkDir(s *Server, rel string) error {
	r, name, err := m.rooted(s, rel)
	if err != nil {
		return err
	}
	defer r.Close()
	if name == "." {
		return nil // the server directory already exists
	}
	return r.MkdirAll(name, 0o755)
}

// OpenForDownload hands back a reader the HTTP layer streams out.
func (m *Manager) OpenForDownload(s *Server, rel string) (io.ReadCloser, string, int64, error) {
	r, name, err := m.rooted(s, rel)
	if err != nil {
		return nil, "", 0, err
	}
	// The root is only needed to open the file; the descriptor that comes back
	// is independent of it, so the handle does not have to outlive this call.
	defer r.Close()

	f, err := r.Open(name)
	if err != nil {
		return nil, "", 0, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, "", 0, err
	}
	if info.IsDir() {
		f.Close()
		return nil, "", 0, fmt.Errorf("%s is a directory", rel)
	}
	return f, path.Base(name), info.Size(), nil
}

// writeProps materialises server.properties from the panel's model.
func (m *Manager) writeProps(s *Server) error {
	r, err := m.serverRoot(s)
	if err != nil {
		return err
	}
	defer r.Close()
	// Under the lock: reloadProps writes this map from another goroutine.
	s.mu.Lock()
	keys := make([]string, 0, len(s.Props))
	props := make(map[string]string, len(s.Props))
	for k, v := range s.Props {
		keys = append(keys, k)
		props[k] = v
	}
	s.mu.Unlock()
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("#Minecraft server properties\n")
	b.WriteString("#Managed by teploy-arcade. Edits here are read back by the panel.\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, props[k])
	}
	// Not a plain write: a truncated server.properties is a server the game
	// refuses to boot, and every configured setting gone with it.
	return writeAtomicIn(r, "server.properties", []byte(b.String()), 0o644)
}

// reloadProps parses an edited server.properties back into the panel's model so
// the settings screen and the file never drift.
func (m *Manager) reloadProps(s *Server, content string) {
	s.mu.Lock()
	for _, ln := range strings.Split(content, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		k, v, ok := strings.Cut(ln, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if _, known := s.Props[k]; known {
			s.Props[k] = v
		}
	}
	if p := atoi(s.Props["server-port"]); p > 0 {
		s.Port = p
	}
	if mp := atoi(s.Props["max-players"]); mp > 0 {
		s.MaxPlayers = mp
	}
	s.mu.Unlock()
	// Swallowing this leaves the panel's model and servers.json disagreeing
	// about the port until the next save, and nobody ever finds out why the
	// edit came back on restart.
	if err := m.Save(); err != nil {
		log.Printf("server.properties for %s was applied in memory but not persisted: %v", s.ID, err)
	}
}

// seedServerFiles gives a new server a plausible tree so the file manager has
// something real to browse before the game has ever run.
func (m *Manager) seedServerFiles(s *Server) error {
	r, err := m.serverRoot(s)
	if err != nil {
		return err
	}
	defer r.Close()

	if err := m.writeProps(s); err != nil {
		return err
	}
	// Only directories the game is happy to find empty. No `world/` - the game
	// creates it, and a placeholder in there is worse than nothing (see below).
	for _, sub := range []string{"plugins", "logs"} {
		if err := r.MkdirAll(sub, 0o755); err != nil {
			return err
		}
	}
	// O_EXCL rather than stat-then-write: an existing file must be left exactly
	// as the operator has it, and a symlink sitting in its place must not
	// redirect the seed write out of the server directory. A failure here is
	// reported rather than swallowed - a server whose eula.txt never landed
	// will not boot, and the reason belongs in the create response, not in the
	// game's log an hour later.
	var werr error
	write := func(rel, content string) {
		if werr != nil {
			return
		}
		f, err := r.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			if !os.IsExist(err) {
				werr = err
			}
			return
		}
		if _, err := f.WriteString(content); err != nil {
			f.Close()
			werr = err
			return
		}
		if err := f.Close(); err != nil {
			werr = err
		}
	}
	// Every seeded file must be a *valid* instance of its format, or absent.
	// A placeholder that merely looks plausible in the file browser will be
	// parsed for real by the game: a stub world/level.dat made Paper exit with
	// "World files may be corrupted. Shutting down." on first boot.
	write("eula.txt", "#By changing the setting below to TRUE you are indicating your agreement to the EULA.\neula=true\n")
	write("ops.json", "[]\n")
	write("whitelist.json", "[]\n")
	write("banned-players.json", "[]\n")
	write("banned-ips.json", "[]\n")
	return werr
}
