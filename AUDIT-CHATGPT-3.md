# teploy-arcade - Audit Pass 3 (2026-09-12)

## Verdict

**11 new findings:** 9 High, 2 Medium. No Critical finding met the Pass-3 bar.

The Pass-2 `saveMu` serialization itself holds: the persistence transaction is serialized from snapshot through rename, and I found no path that takes `saveMu` and then reaches back into `lifecycle`. The restore `new`/`old` split also fixes the ordinary returned-error rollback case, and the reserved `.arcade-tmp-` write path is correctly scoped. The new issues are in interactions around those fixes: two real `lifecycle -> fsMu` inversions, lifecycle paths still outside the filesystem transaction, non-atomic port/config publication, multi-port runtime bindings that the ledger does not model, and crash recovery gaps that normal Go error handling cannot execute.

Artifact: [AUDIT-CHATGPT-3.md](sandbox:/mnt/data/AUDIT-CHATGPT-3.md)

## Findings

### 1. [HIGH] Pass-2 introduced two `lifecycle -> fsMu` paths that deadlock against `Start`'s `fsMu -> lifecycle` order

**File:** `internal/arcade/manager.go` (`Manager.Start`, `Manager.claimStart`); `internal/arcade/players.go` (`Manager.routeListChange`, `Manager.writeList`); `internal/arcade/import.go` (`Manager.finishImport`)

**Problem:** `Start` now deliberately takes `s.fsMu` first and then enters `claimStart`, which takes the global `lifecycle` mutex. fileciteturn2file0L30-L58 But the stopped-player-list path takes the same two locks in the opposite order: `routeListChange` takes `lifecycle`, then its `edit()` reaches `writeList`, which takes `s.fsMu.RLock()`. fileciteturn2file1L116-L130 fileciteturn3file0L17-L36

A concrete two-request deadlock is therefore:

1. Request A edits a stopped whitelist and acquires `lifecycle` in `routeListChange`.
2. Request B starts that server, acquires `s.fsMu`, and blocks in `claimStart` waiting for `lifecycle`.
3. A reaches `writeList` and blocks waiting for `s.fsMu`.
4. Neither can progress. Because `lifecycle` is process-wide, unrelated starts/deletes/creates/import commits now queue behind the deadlock.

`finishImport` has the same reverse edge: it takes `lifecycle`, publishes the provisional server into `m.servers`, and only then calls `writeProps`, which takes `s.fsMu.RLock()`. fileciteturn2file2L161-L199 A concurrent server-list reader can observe that provisional ID; a `Start` on it takes `fsMu` and waits for `lifecycle`, while the import waits for `fsMu`.

**Fix:** make `fsMu -> lifecycle` the only legal order. Do not recursively take an `RWMutex` read lock from the edit helper; split held/unheld helpers. For import, write the properties before making the server visible, then use `lifecycle` only for registration-through-persistence.

```go
func (m *Manager) writeList(s *Server, l PlayerList, entries []ListEntry) error {
	s.fsMu.RLock()
	defer s.fsMu.RUnlock()
	return m.writeListHeld(s, l, entries)
}

func (m *Manager) routeListChange(
	s *Server, l PlayerList, editHeld func() error,
) (bool, error) {
	s.fsMu.RLock()              // canonical order: fsMu -> lifecycle
	defer s.fsMu.RUnlock()

	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()

	switch st := s.State(); st {
	case StatusRunning:
		return true, nil
	case StatusStopped, StatusFailed:
		return false, editHeld() // must use writeListHeld, not writeList
	default:
		return false, fmt.Errorf("server is %s; wait until it settles", st)
	}
}

func (m *Manager) finishImport(j *importJob, s *Server, sc *ImportScan, actor string) error {
	// s is still invisible, so no lifecycle lock is needed for this filesystem write.
	if err := m.writeProps(s); err != nil {
		return fmt.Errorf("write imported server.properties: %w", err)
	}

	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	// register -> Save -> rollback on failure
	// ...
	return nil
}
```

### 2. [HIGH] `Delete` accepts `stopping` and can remove a server while its Docker process is still alive

**File:** `internal/arcade/manager.go` (`Manager.Stop`, `Manager.Delete`); `internal/arcade/runner.go` (`dockerRunner.Stop`)

**Problem:** `Stop` changes the state to `stopping` and returns immediately while a goroutine runs the actual stop. fileciteturn2file4L285-L315 Docker stop is explicitly graceful and can spend up to 45 seconds letting the game flush chunks before it returns. fileciteturn3file4L318-L328 `Delete`, however, rejects only `running` and `starting`; its own comment explicitly permits a server that is still stopping, then removes the manager entry, cancels the process handle, and deletes the server and backup directories. fileciteturn2file3L217-L268

Concrete failure: an admin clicks Stop and immediately Delete. Delete sees `StatusStopping`, succeeds, removes the bind-mounted tree while `docker stop -t 45` is still waiting for the game to exit. If the Docker stop fails, its worker can set the orphaned `*Server` back to `running`, but the manager entry is already gone: a live listening container is now unmanaged. If the stop succeeds, the game performed its final flush against a tree the panel was concurrently deleting.

**Fix:** deletion must require a settled terminal state, not merely “not running”. Do not treat `stopping` as deletable.

```go
m.lifecycle.Lock()
defer m.lifecycle.Unlock()

switch st := s.State(); st {
case StatusStopped, StatusFailed:
	// safe to remove ownership of the tree
case StatusRunning, StatusStarting, StatusStopping:
	return fmt.Errorf("server is %s; wait until it is fully stopped before deleting it", st)
default:
	return fmt.Errorf("cannot delete server in state %q", st)
}
```

### 3. [HIGH] `Stop`/`Restart` can begin inside a quiesced backup or clone and reintroduce writes into the snapshot

**File:** `internal/arcade/backup.go` (`Manager.CreateBackup`); `internal/arcade/clone.go` (`Manager.StartClone`); `internal/arcade/manager.go` (`Manager.Stop`, `Manager.Restart`); `internal/arcade/runner.go` (`dockerRunner.Stop`)

**Problem:** backup and clone correctly hold `fsMu` exclusively for the entire snapshot transaction. Backup quiesces the running Minecraft server before archiving, and clone does the same before copying. fileciteturn7file0L11-L23 fileciteturn7file0L58-L76 fileciteturn7file1L98-L123 `Stop` does not participate in `fsMu` at all. fileciteturn2file4L285-L315 Its Docker implementation deliberately performs a graceful stop so the game can flush chunks. fileciteturn3file4L318-L328

Concrete failure: backup sends `save-off`, successfully flushes, and begins `tarGz`. While the tar walk is in progress, another request calls Stop. `docker stop` causes the game to execute its shutdown save/flush while the tar reader is walking region files. The snapshot invariant has been lost even though the backup had successfully quiesced the game. The same interleaving can produce a mixed-generation clone. `Restart` inherits the same problem through its Stop leg.

**Fix:** starting a stop transaction must be excluded by the same filesystem gate. The key invariant is that the state transition to `stopping` cannot happen until an existing backup/clone releases `fsMu`, and `fsMu` cannot be released until the stop operation that may write the tree has completed.

```go
func (m *Manager) Stop(id string) error {
	s := m.Get(id)
	if s == nil {
		return fmt.Errorf("no such server")
	}

	// This ownership may be handed to the worker; sync.RWMutex is not
	// goroutine-owned. A per-server operation mutex is also a clean design.
	s.fsMu.Lock()

	m.lifecycle.Lock()
	st := s.State()
	if st == StatusStopped || st == StatusFailed || st == StatusStopping {
		m.lifecycle.Unlock()
		s.fsMu.Unlock()
		return fmt.Errorf("not running")
	}
	m.setStatus(s, StatusStopping, 0, "")
	m.lifecycle.Unlock()

	go func() {
		defer s.fsMu.Unlock()
		defer recoverPanic("stop worker for " + s.ID)
		// existing runner.Stop + status handling
	}()
	return nil
}
```

### 4. [HIGH] Launch-affecting settings can mutate while `Start` is constructing Docker arguments, producing a self-contradictory container

**File:** `internal/arcade/manager.go` (`Manager.Start`, `Manager.ApplySettings`, `Manager.SetResources`); `internal/arcade/runner.go` (`dockerRunArgs`, `publishArgs`)

**Problem:** `Start` holds `fsMu` through `runner.Start`, but `ApplySettings` and `SetResources` mutate launch-affecting model fields without taking that gate. `ApplySettings` also classifies only `StatusRunning` as requiring a restart, so `StatusStarting` is treated like a stopped server. fileciteturn2file0L30-L49 fileciteturn3file1L57-L114 `SetResources` similarly changes memory/CPU first and records pending restart only if the state is exactly `running`. fileciteturn4file0L11-L56

`dockerRunArgs` reads the same fields at different points: it calls `publishArgs` first, then later reads `s.Port`, `s.MemoryMB`, and `s.CPU` again for environment variables and limits. fileciteturn3file3L192-L221 fileciteturn3file3L272-L283

Concrete port interleaving:

1. Start has set `StatusStarting`; `publishArgs` reads port 25565 and builds `-p 25565:25565/tcp`.
2. A settings PATCH sees `running == false` and changes `s.Port` to 25566. It will block only later when `writeProps` tries `fsMu`.
3. Start continues and emits `SERVER_PORT=25566`.
4. Docker launches a container publishing 25565 while the game listens on 25566. The panel reports the new port and no pending restart, but the server is unreachable.

The resource path has the same shape: a 2 GB -> 6 GB change during `starting` can be acknowledged with no pending restart after Docker already captured the 2 GB limit.

**Fix:** launch-affecting mutations must join the filesystem/start transaction before they inspect state or mutate the model. Split `writeProps` into a wrapper and a held helper so the settings path does not recursively reacquire the same `RWMutex`. Refuse transient states.

```go
func (m *Manager) ApplySettings(s *Server, changes map[string]string) ([]string, error) {
	s.fsMu.RLock()
	defer s.fsMu.RUnlock()

	if st := s.State(); st == StatusStarting || st == StatusStopping {
		return nil, fmt.Errorf("server is %s; wait until it settles before changing launch settings", st)
	}

	// validate + changeServerPort + mutate Props + Save
	// ...
	if err := m.writePropsHeld(s); err != nil {
		return nil, err
	}
	return needRestart, nil
}

func (m *Manager) writeProps(s *Server) error {
	s.fsMu.RLock()
	defer s.fsMu.RUnlock()
	return m.writePropsHeld(s)
}
```

Apply the same gate/state rule to `SetResources`.

### 5. [HIGH] The port ledger tracks only the base port, while Docker binds spans and fixed extra ports

**File:** `internal/arcade/manager.go` (`Manager.claimPort`, `Manager.NextFreePort`, `Manager.changeServerPort`, `Manager.claimStart`); `internal/arcade/runner.go` (`publishArgs`); `internal/arcade/model.go` (Rust, Valheim, Palworld, CS2 templates)

**Problem:** every manager-side ownership check records and compares only `s.Port`. `claimPort` and `NextFreePort` are base-port-only, as is `changeServerPort`; `claimStart` likewise compares only the base port of running/starting servers. fileciteturn4file4L294-L345 fileciteturn4file3L251-L279 But `publishArgs` binds every `(port, protocol)` in `PortSpan`, every `ExtraPorts` entry, and an optional Geyser UDP port. fileciteturn3file2L133-L179

This fails without any race. The shipped Valheim template binds three consecutive UDP ports. The first server at 2456 binds 2456-2458; `NextFreePort(2456)` sees only base 2456 as occupied and suggests 2457 for the second server, which then tries to bind 2457-2459 and Docker rejects it. Rust has the same overlap with its two-port span. Palworld always binds `27015/udp` as an extra port, so two Palworld servers—or Palworld plus CS2 at its default base—can pass every panel check and still collide at Docker. fileciteturn6file4L254-L262 fileciteturn6file4L288-L332 A base port of 65535 is also accepted even when `PortSpan` produces 65536 or 65537.

**Fix:** make the allocation unit the actual host binding `(port, protocol)`, not an integer base port. Derive the static binding set from the candidate base before create/import/clone/settings commit and before start; include dynamic Geyser bindings in the final start check.

```go
type hostBinding struct {
	Port  int
	Proto string
}

func staticBindings(s *Server, base int) ([]hostBinding, error) {
	span := max(1, s.PortSpan)
	var out []hostBinding
	for i := 0; i < span; i++ {
		p := base + i
		if p < 1 || p > 65535 {
			return nil, fmt.Errorf("port span from %d exceeds 65535", base)
		}
		for _, proto := range defaultProtocols(s.Protocols) {
			out = append(out, hostBinding{p, proto})
		}
	}
	for _, raw := range s.ExtraPorts {
		p, proto, ok := parseExtraPort(raw)
		if !ok || p < 1 || p > 65535 {
			return nil, fmt.Errorf("bad extra port %q", raw)
		}
		out = append(out, hostBinding{p, proto})
	}
	return dedupeBindings(out), nil
}

// reservedBindings map[hostBinding]string
// NextFreePort tries candidates until the whole binding set is disjoint.
```

### 6. [MEDIUM] `validatePropsPort` is a check-only preflight, so another request can claim the port before the new file is published

**File:** `internal/arcade/files.go` (`Manager.WriteFile`, `Manager.validatePropsPort`, `Manager.reloadProps`); `internal/arcade/manager.go` (`Manager.changeServerPort`); `internal/arcade/backup.go` (`Manager.RestoreBackup`)

**Problem:** the Pass-2 fix validates `server.properties` before publishing bytes, but `validatePropsPort` releases `m.mu` as soon as the check returns; it does not reserve the candidate. fileciteturn4file2L141-L180 `WriteFile` then performs the atomic file replacement and only afterward calls `reloadProps`. fileciteturn4file1L103-L120 `reloadProps` re-runs `changeServerPort`, but on conflict it only logs and repairs the in-memory property; it does not put the old bytes back on disk. fileciteturn4file2L197-L220

Concrete interleaving with servers A and B:

1. A saves `server.properties` with port 25570; `validatePropsPort` sees it free and returns.
2. B concurrently changes its port to 25570 through `changeServerPort`, which commits while holding `m.mu`.
3. A publishes its new `server.properties` containing 25570.
4. A's `reloadProps` now rejects 25570 because B owns it, but `WriteFile` still returns success. A's model says its old port while its on-disk file says 25570.

Restore has the same check-to-commit gap between staged validation and the later tree swap.

**Fix:** validation must acquire a lease that remains held through publication and model commit. This should use the full binding set from finding 5.

```go
type portLease struct {
	m         *Manager
	bindings  []hostBinding
	owner     string
	committed bool
}

func (m *Manager) reservePortChange(s *Server, base int) (*portLease, error) {
	bindings, err := staticBindings(s, base)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.bindingsFreeLocked(s, bindings); err != nil {
		return nil, err
	}
	m.reserveBindingsLocked(s.ID, bindings)
	return &portLease{m: m, bindings: bindings, owner: s.ID}, nil
}

// WriteFile/RestoreBackup:
// lease := reserve candidate
// defer lease.ReleaseUnlessCommitted()
// publish bytes/tree
// commit s.Port + Props while the lease is still held
// lease.Commit()
```

### 7. [HIGH] Clone still exposes a provisional server before persistence, so rollback can orphan a server another request already started

**File:** `internal/arcade/clone.go` (`Manager.StartClone`); `internal/arcade/manager.go` (`Manager.Start`)

**Problem:** Pass 2 put Create and `finishImport` behind `lifecycle` through persistence, but clone has a separate registration block that still inserts the destination into `m.servers`/`m.order`, releases `m.mu`, and only then calls `Save`. If `Save` fails, clone removes the registration and releases the port. fileciteturn5file0L17-L39 Nothing prevents `Start` from observing and claiming that provisional destination during the window.

Concrete failure: clone finishes copying, registers destination C, and enters a slow/failing `Save`. A concurrent list/MCP client sees C and starts it. Start commits C to `starting`; then clone's `Save` fails, so the clone goroutine removes C from the manager and releases its port without stopping the process. The new container is now an orphan, and the panel can allocate its still-bound port to another server.

**Fix:** factor the Create/import commit boundary into one helper and use it for clone as well. Clone already holds the source's `fsMu`, so acquiring `lifecycle` here follows the established `fsMu -> lifecycle` order.

```go
func (m *Manager) commitNewServer(s *Server) error {
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()

	m.mu.Lock()
	m.servers[s.ID] = s
	m.order = append(m.order, s.ID)
	delete(m.reservedPorts, s.Port)
	m.mu.Unlock()

	if err := m.Save(); err != nil {
		m.mu.Lock()
		delete(m.servers, s.ID)
		m.removeOrderLocked(s.ID)
		m.mu.Unlock()
		return err
	}
	return nil
}

// StartClone: copy + writeProps first, then commitNewServer(s), then announce done.
```

### 8. [HIGH] A crash during backup leaves a partial archive under the final `.tar.gz` name, and retention can later preserve it as if it were valid

**File:** `internal/arcade/backup.go` (`Manager.CreateBackup`, `tarGz`, `Manager.ListBackups`, `Manager.pruneBackups`)

**Problem:** `CreateBackup` passes the final `<id>.tar.gz` pathname directly to `tarGz`; `tarGz` creates that final name before it writes the first archive byte. Cleanup of a partial archive happens only after `tarGz` returns an error. fileciteturn5file1L79-L109 fileciteturn5file4L260-L332 A SIGKILL, host reboot, or power loss does not execute that error path, so the partially written file survives with a normal final name.

On the next process start, `ListBackups` accepts every `*.tar.gz` as a backup; the in-memory backup lock is gone. fileciteturn5file2L163-L193 After the next successful backup, retention sorts by recency and deletes everything beyond the keep count. fileciteturn5file3L219-L240 With `keep=2`, for example, valid A/B -> interrupted C -> restart -> valid D leaves D and corrupt C as the two newest entries, and prunes valid A/B.

**Fix:** archive to a panel-owned temporary pathname that `ListBackups` cannot mistake for a completed archive, then publish with one rename only after tar/gzip close and file sync succeed. The existing startup sweep already knows how to remove `.arcade-tmp-` files.

```go
suffix, err := randomHex(8)
if err != nil {
	return nil, err
}

tmp := filepath.Join(dir, ".arcade-tmp-"+suffix) // deliberately not .tar.gz
size, err := tarGz(src, tmp)
if err != nil {
	_ = os.Remove(tmp)
	return nil, friendlyFSError(err, "the backup archive")
}

// dst is the final timestamped .tar.gz name chosen while the per-server
// backup lock is held.
if err := os.Rename(tmp, dst); err != nil {
	_ = os.Remove(tmp)
	return nil, err
}
if err := syncDir(dir); err != nil {
	return nil, err
}
```

### 9. [HIGH] Restore rollback is transactional for returned errors but not for process death; no startup recovery exists for `staging/old`

**File:** `internal/arcade/backup.go` (`Manager.RestoreBackup`, `rollbackRestore`, `restoreHeld`); `internal/arcade/runtime.go` (`sweepTempFiles`)

**Problem:** the current `new`/`old` layout correctly fixes an in-process install failure, but its recovery exists only in ordinary Go control flow. Restore moves every live top-level entry into `staging/old`, then installs entries from `staging/new`; the cleanup is a `defer`. fileciteturn6file0L15-L78 A process death between those rename loops executes neither `rollbackRestore` nor the defer.

Concrete failure: restore has moved the live world, `server.properties`, and plugins into `.arcade-restore-*/old`, then the panel is SIGKILLed before all new entries are installed. On restart the server root is empty or partially restored except for the hidden staging directory. The startup sweep removes only files beginning `.arcade-tmp-`; it neither recognizes nor recovers `.arcade-restore-*` directories. fileciteturn6file1L112-L136 If the operator then starts the stopped server, the game can generate a fresh world/config into the partial root while the real previous world remains stranded under `old`.

**Fix:** make the restore transaction recoverable across process death. Write a durable phase marker before the first live rename, mark committed only after the restored properties/model have persisted, and recover every server directory—including resolved adopted directories—before serving lifecycle requests.

```go
// Inside staging:
// phase = "prepared"  -> archive extracted, live tree untouched
// phase = "swapping"  -> old tree may be in staging/old; rollback on boot
// phase = "committed" -> new tree + model persisted; cleanup only

if err := writePhase(staging, "swapping"); err != nil {
	return err
}
// fsync phase file + staging dir, then move live entries to old and install new.

if err := m.reloadPropsPersisted(s, restoredProps); err != nil {
	rollbackRestore(installed, held, dir)
	return err
}
if err := writePhase(staging, "committed"); err != nil {
	return err
}

// During startup, after Manager.Load and before workers/HTTP:
for _, s := range m.List() {
	dir, err := m.ensureServerDir(s) // resolves adopted-in-place trees too
	if err != nil {
		return err
	}
	if err := recoverInterruptedRestore(dir); err != nil {
		return err
	}
}
```

### 10. [MEDIUM] `reloadProps` still swallows persistence failure after publishing new `server.properties`

**File:** `internal/arcade/files.go` (`Manager.WriteFile`, `Manager.reloadProps`); `internal/arcade/backup.go` (`Manager.RestoreBackup`)

**Problem:** after `WriteFile` publishes a new `server.properties`, it calls `reloadProps`, but that function returns no error. Its final `m.Save()` failure is only logged. `WriteFile` then returns success. fileciteturn4file1L111-L120 fileciteturn4file2L183-L238 Restore does the same immediately before auditing `backup.restore` as successful. fileciteturn6file0L86-L93

Concrete failure: an adopted server lives on a healthy external volume, while the panel data volume containing `servers.json` is full or read-only. Editing `server.properties` succeeds on the external volume; `reloadProps` changes the in-memory port/settings; `Save` fails on the panel volume; the HTTP request still returns 200. After a panel restart, the old manager state comes back from `servers.json` while the external file still contains the new values. The operator received an explicit success for a state the panel cannot reproduce.

**Fix:** make property adoption return persistence errors and keep the old disk/model state available until the commit succeeds. Restore can still roll back at this point because `staging/old` has not been deleted yet.

```go
func (m *Manager) reloadProps(s *Server, content string) error {
	// parse + reserve/commit port + update model
	// ...
	return m.Save()
}

// WriteFile(server.properties):
oldBytes, err := readRootFile(r, name)
if err != nil && !errors.Is(err, os.ErrNotExist) {
	return err
}
oldModel := snapshotMutableServerConfig(s)

if err := writeAtomicIn(r, name, []byte(content), 0o644); err != nil {
	return err
}
if err := m.reloadProps(s, content); err != nil {
	restoreMutableServerConfig(s, oldModel)
	_ = writeAtomicIn(r, name, oldBytes, 0o644)
	return fmt.Errorf("properties were not committed: %w", err)
}
```

For restore, propagate the same error and call `rollbackRestore(installed, held, dir)` before returning.

### 11. [HIGH] A viewer can hold the global auth mutex across PBKDF2 and stall every authenticated request

**File:** `internal/arcade/auth.go` (`Auth.SetPassword`, `Auth.Login`, `Auth.Session`); `internal/arcade/api_ext.go` (`API.RoutesExt`, `API.setPassword`)

**Problem:** the password-change route is available to the `viewer` role for self-service. fileciteturn6file3L225-L228 `SetPassword` takes `a.mu` for writing before verifying the current password, then performs the 120,000-round PBKDF2 check—and, on success, the second PBKDF2 for the new password—while still holding that global mutex. fileciteturn6file2L162-L196 Every session lookup and auth gate needs the same `RWMutex`. The code's `Login` path already documents and fixes this exact lock-amplification shape by hashing outside the mutex. fileciteturn7file4L260-L294

Concrete failure: a legitimate low-privilege viewer sends repeated password-change requests for their own account with a wrong current password and any 8-character replacement. Each request holds the global auth write lock for one PBKDF2 duration. With a queue of such requests, normal API calls block at `Enabled`/`Session`/`MustChangePassword`; dashboards, lifecycle requests, SSE reconnects, and console authorization all stall even though the attacker has no operator privileges.

**Fix:** use the same copy-hash-relock pattern as `Login`. Hash both the presented current password and the replacement outside `a.mu`; when reacquiring the write lock, revalidate that the account's old salt/hash still match the snapshot before committing.

```go
func (a *Auth) SetPassword(name, current, next, keepToken string, byAdmin bool) error {
	if len(next) < 8 {
		return fmt.Errorf("password must be at least 8 characters")
	}
	key := strings.ToLower(name)

	a.mu.RLock()
	u, ok := a.users[key]
	if !ok {
		a.mu.RUnlock()
		return fmt.Errorf("no such user")
	}
	oldSalt, oldHash := u.Salt, u.Hash
	a.mu.RUnlock()

	if !byAdmin {
		got := hashPassword(current, oldSalt) // expensive work: no auth lock held
		if subtle.ConstantTimeCompare([]byte(oldHash), []byte(got)) != 1 {
			return fmt.Errorf("current password is incorrect")
		}
	}

	newSalt, err := randomHex(16)
	if err != nil {
		return err
	}
	newHash := hashPassword(next, newSalt) // also outside the lock

	a.mu.Lock()
	defer a.mu.Unlock()
	u, ok = a.users[key]
	if !ok || u.Salt != oldSalt || u.Hash != oldHash {
		return fmt.Errorf("password changed concurrently; retry")
	}
	u.Salt, u.Hash, u.MustChange = newSalt, newHash, byAdmin
	// revoke other sessions + persist
	return a.saveUsers()
}
```
