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
	"strconv"
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
//
// The ".arcade-tmp-" prefix is what the boot sweep keys on: sweeping any name
// containing ".tmp" deleted legitimate game files that happened to carry it.
func writePropsFileGuard(r *os.Root, name, content string) error {
	return writeAtomicIn(r, name, []byte(content), 0o644)
}

func writeAtomicIn(r *os.Root, name string, data []byte, perm os.FileMode) error {
	dir := path.Dir(name)
	if dir == "." {
		dir = ""
	}
	suffix, err := randomHex(8)
	if err != nil {
		return err
	}
	tmp := path.Join(dir, ".arcade-tmp-"+suffix)

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
	// Same handover as writeFileAtomic, and this is the path that actually
	// writes server.properties: saving one setting from the panel replaced the
	// file with a root-owned inode, and the container - uid 1000 - then died on
	// AccessDeniedException before the world loaded. Done through the Root so
	// the confinement still holds; a chown by absolute path would step outside
	// the sandbox this function exists to keep.
	preserveOwnerIn(r, tmp, name)

	if err := r.Rename(tmp, name); err != nil {
		cleanup()
		return err
	}
	return nil
}

// Seam, so the handover is testable without being root.
var rootChown = func(r *os.Root, name string, uid, gid int) error { return r.Chown(name, uid, gid) }

// preserveOwnerIn is preserveOwner for a Root-confined write: the file being
// replaced decides, and a file that does not exist yet inherits its directory.
func preserveOwnerIn(r *os.Root, tmp, name string) {
	if geteuid() != 0 {
		return
	}
	uid, gid, ok := rootOwner(r, name)
	if !ok {
		dir := path.Dir(name)
		if dir == "." {
			dir = "."
		}
		uid, gid, ok = rootOwner(r, dir)
	}
	if !ok || (uid == 0 && gid == 0) {
		return
	}
	_ = rootChown(r, tmp, uid, gid)
}

func rootOwner(r *os.Root, name string) (int, int, bool) {
	fi, err := r.Stat(name)
	if err != nil {
		return 0, 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
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
	// Shared hold on the filesystem gate from before the backup check through
	// the completed write: the check alone was check-then-act, and a backup
	// could open its archive window between the check and the rename.
	s.fsMu.RLock()
	defer s.fsMu.RUnlock()

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
	// server.properties carries the panel's port identity. Order: snapshot
	// the old bytes, write the new file, THEN commit the port. If the port
	// commit loses a race for the port, the old file is restored so disk and
	// model never disagree; the previous shape committed the port first and
	// left the model moved when a disk failure dropped the write.
	if path.Base(name) == "server.properties" {
		if err := m.validatePropsPort(s, content); err != nil {
			return err
		}
		oldBytes, hadOld := func() ([]byte, bool) {
			if f, err := r.Open(name); err == nil {
				defer f.Close()
				b, rerr := io.ReadAll(io.LimitReader(f, maxEditBytes))
				if rerr == nil {
					return b, true
				}
			}
			return nil, false
		}()
		restoreOld := func() {
			if hadOld {
				_ = writeAtomicIn(r, name, oldBytes, 0o644)
			} else {
				_ = r.Remove(name)
			}
		}
		s.mu.Lock()
		current := s.Port
		s.mu.Unlock()
		for _, ln := range strings.Split(content, "\n") {
			ln = strings.TrimSpace(ln)
			if ln == "" || strings.HasPrefix(ln, "#") {
				continue
			}
			if k, v, ok := strings.Cut(ln, "="); ok && strings.TrimSpace(k) == "server-port" {
				if p, cerr := strconv.Atoi(strings.TrimSpace(v)); cerr == nil && p > 0 && p != current {
					if err := m.changeServerPort(s, p); err != nil {
						return err
					}
					// The write must succeed for the commit to stand; on
					// failure the old file is restored and the model reverts.
					if werr := writePropsFileGuard(r, name, content); werr != nil {
						// put the file back and revert the model
						restoreOld()
						_ = m.changeServerPort(s, current)
						return friendlyFSError(werr, rel)
					}
					return m.reloadProps(s, content)
				}
				break
			}
		}
	}
	if err := writeAtomicIn(r, name, []byte(content), 0o644); err != nil {
		return friendlyFSError(err, rel)
	}

	// server.properties is the panel's own model too - keep them in step rather
	// than letting the file and the settings screen disagree.
	if path.Base(name) == "server.properties" {
		return m.reloadProps(s, content)
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
	s.fsMu.RLock()
	defer s.fsMu.RUnlock()

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
	// The file API's other mutations refuse writes during a backup window;
	// mkdir used to skip the check entirely, so a directory could appear
	// under the archiver's feet mid-walk.
	s.fsMu.RLock()
	defer s.fsMu.RUnlock()

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
	// A mutation of the server tree like any other: held shared on the
	// filesystem gate so it cannot land inside a backup's archive window.
	// Callers already inside an fsMu read section must use writePropsHeld -
	// recursive RLock deadlocks against a queued writer.
	s.fsMu.RLock()
	defer s.fsMu.RUnlock()
	return m.writePropsHeld(s)
}

func (m *Manager) writePropsHeld(s *Server) error {
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

// validatePropsPort checks a properties file's server-port the way
// changeServerPort would, without committing anything: paths about to publish
// new properties bytes (a restore's staged archive) call this first so a
// conflict is refused BEFORE the tree is swapped, not after.
func (m *Manager) validatePropsPort(s *Server, content string) error {
	for _, ln := range strings.Split(content, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		k, v, ok := strings.Cut(ln, "=")
		if !ok || strings.TrimSpace(k) != "server-port" {
			continue
		}
		p, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || p < 1 || p > 65535 {
			return fmt.Errorf("server-port %q is out of range", v)
		}
		s.mu.Lock()
		current := s.Port
		s.mu.Unlock()
		if p == current {
			return nil
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		protos, span, extras := func() ([]string, int, []string) {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.Protocols, s.PortSpan, s.ExtraPorts
		}()
		candidate, cerr := candidateBindings(p, protos, span, extras)
		if cerr != nil {
			return cerr
		}
		for _, id := range m.order {
			other, ok := m.servers[id]
			if !ok || other.ID == s.ID {
				continue
			}
			if bindingConflict(serverBindingsLocked(other), candidate) {
				other.mu.Lock()
				name := other.Name
				other.mu.Unlock()
				return fmt.Errorf("port %d is already used by %q", p, name)
			}
		}
		if _, held := m.reservedPorts[p]; held {
			return fmt.Errorf("port %d is reserved by an import or create in progress", p)
		}
		return nil
	}
	return nil
}

// reloadProps parses an edited server.properties back into the panel's model so
// the settings screen and the file never drift. The error is the persistence
// failure: the file HAS landed at that point, and the caller must be able to
// tell the operator the panel could not record it.
func (m *Manager) reloadProps(s *Server, content string) error {
	parsed := map[string]string{}
	for _, ln := range strings.Split(content, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		if k, v, ok := strings.Cut(ln, "="); ok {
			parsed[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}

	// Every port adoption goes through the manager-level transaction: the
	// old shape assigned s.Port directly here, letting an edited or restored
	// file reintroduce duplicate port ownership or an out-of-range value the
	// settings path had just been fixed to refuse.
	if raw, ok := parsed["server-port"]; ok {
		s.mu.Lock()
		current := s.Port
		s.mu.Unlock()
		p, convErr := strconv.Atoi(raw)
		var err error
		if convErr != nil || p < 1 || p > 65535 {
			err = fmt.Errorf("server-port %q is out of range", raw)
		} else if p != current {
			err = m.changeServerPort(s, p)
		}
		if err != nil {
			// The refused value never reaches the model either: Props and
			// Port must not disagree with each other.
			log.Printf("%s: refusing the edited server-port from server.properties: %v", s.ID, err)
			s.mu.Lock()
			s.Props["server-port"] = itoa(s.Port)
			s.mu.Unlock()
			delete(parsed, "server-port")
		}
	}

	s.mu.Lock()
	for k, v := range parsed {
		if _, known := s.Props[k]; known {
			s.Props[k] = v
		}
	}
	if mp := atoi(parsed["max-players"]); mp > 0 {
		s.MaxPlayers = mp
	}
	s.mu.Unlock()
	// Swallowing this leaves the panel's model and servers.json disagreeing
	// about the port until the next save, and nobody ever finds out why the
	// edit came back on restart.
	if err := m.Save(); err != nil {
		log.Printf("server.properties for %s was applied in memory but not persisted: %v", s.ID, err)
		return fmt.Errorf("the file was saved, but the panel could not record the change: %w", err)
	}
	return nil
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
