package arcade

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

// maxLineBytes caps one console line. The ring buffer holds 500 of them and
// fans each out to every viewer, so an unbounded line is an unbounded ring and
// an unbounded write to every socket. Modded servers do print megabyte stack
// dumps on one line.
const maxLineBytes = 8192

func truncateLine(s string) string {
	if len(s) <= maxLineBytes {
		return s
	}
	return s[:maxLineBytes] + fmt.Sprintf(" … [truncated, %d bytes total]", len(s))
}

// recoverPanic keeps one bad goroutine from taking the panel down with it. A
// panic in a runner or a scheduled task would otherwise kill the process, and
// with it every server's management - the blast radius is completely out of
// proportion to the fault.
func recoverPanic(what string) {
	if r := recover(); r != nil {
		log.Printf("panic in %s: %v\n%s", what, r, debug.Stack())
	}
}

// writeFileAtomic writes via a temp file and renames, so a crash, an OOM kill
// or a full disk can never leave a truncated state file behind.
//
// This matters more than it looks: a half-written users.json fails to parse on
// boot, gets quarantined, leaves the user set empty - and an empty user set
// means auth.Enabled() is false, which makes every require() short-circuit to
// allow. A non-atomic write to that one file is an availability bug that
// becomes an authentication bypass.
//
// The temp name is unique per call, not path+".tmp". Manager.Save has eight
// callers across the runner and HTTP goroutines with no lock between them, and
// a shared temp name makes them destroy each other: both write it, the first
// rename consumes it, and the second fails ENOENT having saved nothing. Most of
// those callers discard the error, so the state change was simply lost.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// CreateTemp always makes 0600; the caller's perm is the mode the file must
	// end up with, and after the rename there is no second chance to set it.
	if err := f.Chmod(perm); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// sweepTempFiles removes temp files stranded by a crash mid-write.
//
// Unique temp names fixed a collision and a symlink escape, but traded a
// self-overwriting "<file>.tmp" for litter that nothing cleaned up - and for
// the per-server lists it accumulated inside the directory the file manager
// shows the operator. Called once at boot, when nothing is mid-write.
func sweepTempFiles(root string) {
	var removed int
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if i := strings.Index(d.Name(), ".tmp"); i > 0 {
			if os.Remove(p) == nil {
				removed++
			}
		}
		return nil
	})
	if removed > 0 {
		log.Printf("cleaned up %d temp file(s) left by an interrupted write", removed)
	}
}

// quarantine moves an unreadable state file aside and reports the new path, so
// a corrupt file never blocks boot and is never silently destroyed either.
func quarantine(path string, cause error) string {
	dst := fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
	if err := os.Rename(path, dst); err != nil {
		log.Printf("could not quarantine %s: %v", filepath.Base(path), err)
		return ""
	}
	log.Printf("WARNING: %s was unreadable (%v). Moved to %s and continuing with an empty set.",
		filepath.Base(path), cause, filepath.Base(dst))
	return dst
}

var (
	agentVersion = "0.1.0"
	hostAddr     = "127.0.0.1"
	hostCPUs     = float64(runtime.NumCPU())
	// Measured at startup from the running host (see hostcap.go). 0 means
	// "could not determine", which the UI reports as unknown rather than
	// substituting a plausible-looking figure.
	hostMemMB  = 0
	hostDiskGB = 0

	// Extra hostnames permitted to open the console socket, beyond same-origin.
	// Empty means same-origin only.
	allowedOrigins []string

	// dataHostPath is the host-side path that corresponds to the panel's own
	// data directory. It only matters when the panel itself runs in a
	// container: `docker run -v A:B` is resolved by the daemon on the HOST, so
	// bind mounts for sibling game containers must use host paths. Get this
	// wrong and Docker silently creates an empty directory - the game boots a
	// blank world while the file manager shows the real one.
	dataHostPath string
	dataDirPath  string
)

// hostPathFor maps a path inside this process onto the equivalent host path,
// so sibling containers bind-mount the right directory.
func hostPathFor(p string) string {
	if dataHostPath == "" || dataDirPath == "" || dataHostPath == dataDirPath {
		return p
	}
	if strings.HasPrefix(p, dataDirPath) {
		return dataHostPath + strings.TrimPrefix(p, dataDirPath)
	}
	return p
}

// inContainer reports whether this process is itself containerised.
func inContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	return false
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"error":"encode failed"}`)
	}
	return b
}

func execCommand(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}
