# teploy-arcade - Audit Pass 2 (2026-09-12)

## Verdict

**10 new findings:** 1 Critical, 5 High, 3 Medium, 1 Low.

Re-checked the current implementations of `fsMu` gating and lock ordering, backup quiescing, lifecycle interaction with backup/restore, `changeServerPort` versus persistence, `Create`/`finishImport` rollback paths, SHA-256 MCP token migration, `updateTask` pointer decoding, strict `!wait` parsing, clone snapshotting, and in-directory restore staging.

The SHA-256 MCP token path and strict `!wait` parser did not produce an additional finding: current MCP authentication rejects legacy unprefixed hashes and compares the new fixed-format hash in constant time, while `!wait` now uses `strconv.Atoi` and rejects malformed values. fileciteturn22file0L25-L42 fileciteturn19file1L14-L30

The findings below exclude the 11 Pass 1 findings recorded as fixed.

## Findings

### 1. [HIGH] Concurrent `Save` calls can deadlock after a reorder and can overwrite newer state

**File:** `internal/arcade/manager.go` (`Manager.Save`, `Manager.Reorder`)

**Problem:** `Save` snapshots `m.order`, releases `m.mu`, then locks every server mutex in that snapshot's order. `Reorder` can change that order before starting another `Save`. With servers A and B, one save can hold A while waiting for B using `[A,B]`, while a post-reorder save holds B while waiting for A using `[B,A]`: a permanent mutex cycle. fileciteturn17file2L180-L203 fileciteturn17file3L224-L244

There is a second independent persistence race: the server mutexes are released before `writeFileAtomic`. Two saves can therefore marshal different generations and race their final renames; the older snapshot can be the last rename and silently become `servers.json`.

**Fix:** Serialize the entire persistence transaction—from taking the manager snapshot through the final atomic rename—with a dedicated mutex.

```go
type Manager struct {
	mu     sync.RWMutex
	saveMu sync.Mutex

	servers map[string]*Server
	order   []string
	// ...
}

func (m *Manager) Save() error {
	m.saveMu.Lock()
	defer m.saveMu.Unlock()

	m.mu.RLock()
	list := make([]*Server, 0, len(m.order))
	for _, id := range m.order {
		if s := m.servers[id]; s != nil {
			list = append(list, s)
		}
	}
	m.mu.RUnlock()

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
```

The `saveMu` must cover the initial snapshot as well as the write; locking only the rename still allows an older snapshot to overwrite a newer mutation later.

### 2. [CRITICAL] `Start` and `Delete` bypass the new filesystem transaction gate, so backup/restore can cross a lifecycle transition

**File:** `internal/arcade/backup.go` (`Manager.CreateBackup`, `Manager.RestoreBackup`, `Manager.quiesceForBackup`); `internal/arcade/manager.go` (`Manager.Start`, `Manager.Delete`)

**Problem:** backup and restore now hold `s.fsMu` exclusively, but `Start` does not acquire that gate at all. A backup that observes a stopped server can therefore decide no quiesce is necessary and then have `Start` launch the game while `tarGz` is walking the tree. More seriously, `RestoreBackup` checks `Stopped`/`Failed` **before** acquiring `fsMu`; `Start` can commit after that check and the restore can then rename files underneath a live game. fileciteturn16file0L10-L30 fileciteturn16file0L42-L57 fileciteturn16file3L224-L244

`Delete` is outside `fsMu` as well. A stopped server can be deleted while its backup or restore owns the exclusive filesystem gate; for an adopted server, restore has already resolved the external target and can continue modifying that operator-owned directory after the panel entry/symlink has been deleted. fileciteturn12file4L140-L159 fileciteturn5file0L22-L46

**Fix:** add a per-server lifecycle/filesystem transaction mutex and make lifecycle operations that change ownership of the tree participate in it. Also classify transient states explicitly rather than treating every non-running state as safe.

```go
type Server struct {
	// ...
	opMu sync.Mutex
	fsMu sync.RWMutex
}

func (m *Manager) Start(id string) error {
	s := m.Get(id)
	if s == nil {
		return fmt.Errorf("no such server")
	}

	// Docker preflight can remain outside opMu.

	s.opMu.Lock()
	defer s.opMu.Unlock()

	if err := m.claimStart(s); err != nil {
		return err
	}
	if err := m.runnerFor(s).Start(s, func(l Line) { m.emit(s, l) }); err != nil {
		m.fail(s, 1, err.Error())
		return err
	}
	return nil
}

func (m *Manager) quiesceForBackup(s *Server) (func(), error) {
	switch s.State() {
	case StatusStopped, StatusFailed:
		return func() {}, nil
	case StatusStarting, StatusStopping:
		return nil, fmt.Errorf("wait until the server finishes %s before backing it up", s.State())
	case StatusRunning:
		// existing save-off / save-all flush logic
	default:
		return nil, fmt.Errorf("cannot back up a server in state %q", s.State())
	}
}
```

`CreateBackup`, `RestoreBackup`, and `Delete` should hold the same `s.opMu`; the restore state check must move inside that transaction. Define one lock order and use it consistently.

### 3. [HIGH] The new `fsMu` coverage still misses clone snapshots and stopped player-list writes

**File:** `internal/arcade/clone.go` (`Manager.StartClone`); `internal/arcade/players.go` (`Manager.writeList`, `Manager.routeListChange`)

**Problem:** `StartClone` never takes `src.fsMu`. For a running source it sets the old `backupLocked` flag, but the new `writeProps` path relies on `fsMu` rather than that flag; for a stopped source it does not even set `backupLocked`. The actual `copyTreeFiltered` therefore remains able to overlap a settings write or, for a stopped source, a restore/file mutation, producing a clone assembled from different generations of the source tree. fileciteturn14file0L8-L35 fileciteturn15file0L27-L50

The Players API is another missed mutator. When stopped, `routeListChange` executes its edit directly, and `writeList` writes the JSON file without `fsMu`; that write can therefore run inside a backup/restore/clone transaction despite the new gate. fileciteturn16file2L197-L212 fileciteturn13file3L130-L143

**Fix:** make clone an exclusive snapshot transaction and make the player-list file writer a normal shared mutation.

```go
// clone.go
go func() {
	defer recoverPanic("clone of " + src.Name)

	src.fsMu.Lock()
	defer src.fsMu.Unlock()

	if !m.lockBackup(src.ID) {
		m.releasePort(port)
		job.fail(fmt.Errorf("another snapshot operation is already running"))
		return
	}
	defer m.unlockBackup(src.ID)

	resume, err := m.quiesceForBackup(src)
	if err != nil {
		m.releasePort(port)
		job.fail(err)
		return
	}
	defer resume()

	if err := copyTreeFiltered(srcDir, dst, job, cloneSkip); err != nil {
		// existing cleanup
		return
	}

	// ...
}()
```

```go
// players.go
func (m *Manager) writeList(s *Server, l PlayerList, entries []ListEntry) error {
	s.fsMu.RLock()
	defer s.fsMu.RUnlock()

	if m.backupLocked(s.ID) {
		return fmt.Errorf("a snapshot operation is in progress; writes are blocked until it finishes")
	}

	// existing marshal + atomic write
}
```

### 4. [HIGH] The new Create/import rollback can remove a server another request already started

**File:** `internal/arcade/manager.go` (`Manager.Create`); `internal/arcade/import.go` (`Manager.finishImport`); `internal/arcade/manager.go` (`Manager.Start`)

**Problem:** both rollback fixes publish the new `Server` into `m.servers`/`m.order` **before** the fallible persistence step. The code comment in `Create` says the server is invisible until persisted, but registration occurs immediately before `m.Save()`. If another caller lists the server and starts it during that window, then `Save` fails, rollback deletes the manager entry and filesystem tree without stopping the process that just claimed the server. fileciteturn17file0L45-L85

`finishImport` has the same shape, with an even larger window because `writeProps` also occurs after registration and before persistence. fileciteturn17file1L127-L157

**Fix:** keep the provisional registration behind the existing lifecycle mutex until persistence either commits or rolls back. `Start`/`Delete` already synchronize their critical transition through that mutex.

```go
m.lifecycle.Lock()
defer m.lifecycle.Unlock()

m.mu.Lock()
m.servers[s.ID] = s
m.order = append(m.order, s.ID)
delete(m.reservedPorts, s.Port)
m.mu.Unlock()

if err := m.writeProps(s); err != nil { // finishImport only
	rollbackRegistration()
	return err
}

if err := m.Save(); err != nil {
	rollbackRegistration()
	return err
}

// Only after this point may Start/Delete observe a committed registration.
```

Apply the same commit boundary in `Create`. A per-server `creating`/`committed` state would also work, but rollback must never assume the provisional object could not have acquired a process.

### 5. [HIGH] Failed adopt-in-place import rolls back the symlink but not the modification it already made to the operator's source tree

**File:** `internal/arcade/import.go` (`Manager.StartImport`, `Manager.finishImport`)

**Problem:** adopt mode points the panel path directly at `sc.Path`. `finishImport` then calls `writeProps`, so the external operator-owned `server.properties` is rewritten with the selected panel port. If the following `m.Save()` fails, registration is rolled back and `StartImport` removes only the panel symlink—but the external source file remains modified even though the API reports that the import failed. fileciteturn15file1L104-L115 fileciteturn15file2L154-L167

This can break the server under its previous controller on its next start, particularly when the operator deliberately chose a different port during import.

**Fix:** snapshot the exact external file before adoption and restore it on every post-write failure.

```go
propsPath := filepath.Join(sc.Path, "server.properties")

oldProps, readErr := os.ReadFile(propsPath)
oldExists := readErr == nil
if readErr != nil && !os.IsNotExist(readErr) {
	return nil, readErr
}

oldPerm := os.FileMode(0o644)
if oldExists {
	if fi, err := os.Stat(propsPath); err == nil {
		oldPerm = fi.Mode().Perm()
	}
}

if err := adoptInPlace(dst, sc.Path); err != nil {
	return nil, err
}

if err := m.finishImport(job, s, sc, actor); err != nil {
	if oldExists {
		_ = writeFileAtomic(propsPath, oldProps, oldPerm)
	} else {
		_ = os.Remove(propsPath)
	}
	_ = os.Remove(dst) // panel-owned symlink only
	job.fail(err)
	return nil, err
}
```

The restore of the external file should occur before returning the failed import.

### 6. [MEDIUM] Restore now stages on the server filesystem but still checks free space on the panel filesystem

**File:** `internal/arcade/backup.go` (`Manager.RestoreBackup`)

**Problem:** the EXDEV fix correctly moved staging inside the resolved server directory. For an adopted server that directory can be on another filesystem entirely. The free-space guard still calls `diskFree(m.dataDir)` **before** resolving `dir`, so it measures the panel's disk even though the restored archive is about to expand on the adopted server's disk. fileciteturn14file1L49-L66

A large panel disk plus a nearly-full adopted disk therefore passes the preflight and proceeds to exhaust the wrong filesystem. The opposite configuration can incorrectly refuse a restore that actually has ample space.

**Fix:** resolve the server directory first and measure the filesystem where staging will actually be created.

```go
dir, err := m.ensureServerDir(s)
if err != nil {
	return err
}

if need := uncompressedSize(archive, st.Size()); need > 0 {
	if free, err := diskFree(dir); err == nil && free < need+importFreeMargin {
		return fmt.Errorf(
			"restoring this backup needs about %s free and only %s is left on the server's filesystem",
			humanSize(need+importFreeMargin),
			humanSize(free),
		)
	}
}

staging, err := os.MkdirTemp(dir, ".arcade-restore-")
```

### 7. [MEDIUM] Restore reserves `.previous` inside the extracted archive namespace and silently drops a legitimate top-level entry with that name

**File:** `internal/arcade/backup.go` (`Manager.RestoreBackup`)

**Problem:** the archive is extracted directly into `staging`. Restore then creates `staging/.previous` for the held live world, and the install loop explicitly skips the `.previous` entry. A perfectly valid server tree containing a top-level `.previous` directory is included by backups, but on restore that name collides with the transaction's private bookkeeping and is never installed. If the archived `.previous` is a regular file, creating the holding directory fails instead. fileciteturn4file1L45-L68 fileciteturn5file3L241-L255

**Fix:** separate extracted data and transaction bookkeeping into sibling directories whose names are never part of the archive namespace.

```go
staging, err := os.MkdirTemp(dir, ".arcade-restore-")
if err != nil {
	return err
}
defer os.RemoveAll(staging)
stagingName := filepath.Base(staging)

extracted := filepath.Join(staging, "new")
held := filepath.Join(staging, "old")

if err := os.Mkdir(extracted, 0o700); err != nil {
	return err
}
if err := os.Mkdir(held, 0o700); err != nil {
	return err
}

if err := untarGz(archive, extracted); err != nil {
	return fmt.Errorf("restore failed, the live world was not touched: %w", err)
}

// Move live entries into held, skipping only stagingName.

staged, err := os.ReadDir(extracted)
if err != nil {
	rollbackRestore(nil, held, dir)
	return err
}
for _, e := range staged {
	if err := os.Rename(
		filepath.Join(extracted, e.Name()),
		filepath.Join(dir, e.Name()),
	); err != nil {
		// rollback
	}
}
```

No filename from the archive then doubles as restore metadata.

### 8. [MEDIUM] `reloadProps` bypasses the new atomic port-change path

**File:** `internal/arcade/files.go` (`Manager.reloadProps`, `Manager.WriteFile`); `internal/arcade/backup.go` (`Manager.RestoreBackup`); `internal/arcade/manager.go` (`Manager.changeServerPort`)

**Problem:** `changeServerPort` now correctly checks registered servers and `reservedPorts` under `m.mu`, but it is used only by Settings. `reloadProps` still parses `server-port` and assigns `s.Port` directly under `s.mu`, with no conflict check and no upper bound. fileciteturn13file4L161-L183 fileciteturn18file2L57-L75

That bypass is reachable both from an operator saving `server.properties` through the file API and from a successful backup restore. An edited/restored properties file can therefore reintroduce duplicate persisted port ownership—or set a value above 65535—despite the Pass 1 atomic-port fix. fileciteturn18file0L8-L17 fileciteturn18file1L38-L44

**Fix:** make every path that adopts a `server-port` use one manager-level transaction. Do not assign `s.Port` directly in `reloadProps`.

```go
func (m *Manager) reloadProps(s *Server, content string) error {
	next := parseProperties(content)

	if raw, ok := next["server-port"]; ok {
		p, err := strconv.Atoi(raw)
		if err != nil || p < 1 || p > 65535 {
			return fmt.Errorf("server-port %q is out of range", raw)
		}

		s.mu.Lock()
		oldPort := s.Port
		s.mu.Unlock()

		if p != oldPort {
			if err := m.changeServerPort(s, p); err != nil {
				return err
			}
		}
	}

	s.mu.Lock()
	for k, v := range next {
		if k == "server-port" {
			continue // committed by changeServerPort
		}
		if _, known := s.Props[k]; known {
			s.Props[k] = v
		}
	}
	if mp, err := strconv.Atoi(next["max-players"]); err == nil && mp > 0 {
		s.MaxPlayers = mp
	}
	s.mu.Unlock()

	return m.Save()
}
```

`WriteFile` must validate/reserve the candidate port **before** publishing the new `server.properties` bytes, and restore must validate the staged properties before replacing the live tree; otherwise an error from this helper merely leaves disk and memory disagreeing.

### 9. [LOW] The `updateTask` pointer fix now permits empty names and empty command programs

**File:** `internal/arcade/api_ext.go` (`API.updateTask`); `internal/arcade/scheduler.go` (`Scheduler.Add`, `Scheduler.Update`)

**Problem:** changing PATCH fields to pointers correctly preserved omitted booleans, but it also changed the string behavior: an explicitly supplied `""` is now assigned. Creation rejects blank names and blank commands, while `Scheduler.Update` validates only the clock. `PATCH {"commands":""}` therefore succeeds and leaves an enabled scheduled task whose run contains zero executable steps and is recorded as a successful run. fileciteturn14file3L119-L139 fileciteturn14file4L149-L168 fileciteturn15file4L249-L267

**Fix:** centralize task validation and run it after every update mutation before saving.

```go
func validateTask(t *Task) error {
	if strings.TrimSpace(t.Name) == "" {
		return fmt.Errorf("a task name is required")
	}
	if strings.TrimSpace(t.Commands) == "" {
		return fmt.Errorf("at least one command is required")
	}
	_, err := parseClock(t.Time)
	return err
}

func (sc *Scheduler) Update(id string, fn func(*Task)) (*Task, error) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	for _, t := range sc.tasks {
		if t.ID != id {
			continue
		}

		before := *t
		fn(t)

		if err := validateTask(t); err != nil {
			*t = before
			return nil, err
		}

		cp := *t
		return &cp, sc.save()
	}
	return nil, fmt.Errorf("no such task")
}
```

`Add` should call the same validator so the two write paths cannot drift again.

### 10. [HIGH] Startup temp cleanup deletes arbitrary game files whose legitimate name contains `.tmp`

**File:** `internal/arcade/runtime.go` (`sweepTempFiles`); `internal/arcade/app.go` (`Run`)

**Problem:** `sweepTempFiles` recursively walks the **entire panel data directory** and deletes every non-directory whose basename contains `.tmp` at any position after the first character. It does not verify that the file matches either temp-file naming scheme generated by Arcade. fileciteturn14file2L82-L103

`Run` executes this sweep at every startup before server state is loaded. A plugin, mod, game, imported server, or operator is therefore free to have a legitimate `cache.tmp`, `world.tmp.dat`, `config.tmp.json`, etc.; Arcade silently deletes it on the next panel restart. fileciteturn10file1L133-L148

**Fix:** give panel-owned temporary files an unambiguous reserved prefix and sweep only that prefix.

```go
const arcadeTempPrefix = ".arcade-tmp-"

// writeFileAtomic
f, err := os.CreateTemp(filepath.Dir(path), arcadeTempPrefix+"*")

// writeAtomicIn
suffix, err := randomHex(8)
if err != nil {
	return err
}
tmp := path.Join(dir, arcadeTempPrefix+suffix)
```

```go
func sweepTempFiles(root string) {
	var removed int
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), arcadeTempPrefix) {
			if os.Remove(p) == nil {
				removed++
			}
		}
		return nil
	})
	if removed > 0 {
		log.Printf("cleaned up %d panel temp file(s) left by an interrupted write", removed)
	}
}
```

Do not identify panel-owned files by a generic extension/sub-string shared with arbitrary server data.
