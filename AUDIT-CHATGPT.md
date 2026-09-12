# teploy-arcade - Audit (2026-09-12)

## Summary

| # | Severity | File | Issue |
|---:|---|---|---|
| 1 | Critical | `internal/arcade/manager.go`, `internal/arcade/model.go`, `internal/arcade/runner.go` | Remaining unlocked `Server.Props` reads can race settings/file edits and crash the whole Go process |
| 2 | High | `internal/arcade/backup.go` | Live backup ignores failures from `save-off` / `save-all flush` and can archive a world while it is still being written |
| 3 | High | `internal/arcade/backup.go`, `internal/arcade/files.go`, `internal/arcade/plugins.go` | Backup write exclusion is check-then-act, so file/plugin mutations can race directly into an in-progress archive |
| 4 | High | `internal/arcade/backup.go` | Restore is not transactional: adopted trees can fail with cross-device renames and partial installs can leave a mixed old/new world |
| 5 | High | `internal/arcade/import.go` | Import reports success even when `servers.json` or the imported `server.properties` could not be persisted |
| 6 | High | `internal/arcade/mcp.go` | Every unauthenticated MCP token guess runs 120,000-round PBKDF2, enabling cheap CPU exhaustion |
| 7 | High | `internal/arcade/runner.go`, `internal/arcade/manager.go` | Docker stop failures are discarded, the watcher is cancelled anyway, and a live container can be left unmanaged in `stopping` state |
| 8 | Medium | `internal/arcade/manager.go` | `Create` publishes a server before seeding succeeds and ignores persistence failure, producing ghost/non-durable servers |
| 9 | Medium | `internal/arcade/manager.go` | Concurrent `server-port` edits are not reserved atomically and can assign one port to multiple servers/imports |
| 10 | Medium | `internal/arcade/api_ext.go` | Task PATCH treats omitted booleans as `false`, so a partial edit silently disables/non-repeats a task |
| 11 | Low | `internal/arcade/scheduler.go` | Invalid `!wait` arguments parse as zero seconds and can turn a delayed restart into an immediate restart |

## Findings

### 1. [CRITICAL] Remaining unlocked `Server.Props` reads can crash the panel

- File: `internal/arcade/manager.go` (`SettingsView`), `internal/arcade/model.go` (`MOTD`), `internal/arcade/runner.go` (`simRunner.run`, `expandVars` / `dockerRunArgs`)
- Problem: `reloadProps` and `ApplySettings` mutate `s.Props` while holding `s.mu`, but several readers still access the map without that lock. `SettingsView` does `s.Props[meta.Key]` directly; `MOTD` directly reads `s.Props["motd"]`; and the simulator boot path reads `s.Props["gamemode"]`. A concurrent `PATCH /settings` or file-manager write of `server.properties` can therefore overlap a settings GET or server start. Go's concurrent map read/write failure is fatal to the process and cannot be recovered by the project's panic wrappers. This is a new remaining site of the map-race class despite the earlier fixes to `Snapshot`, `writeProps`, MCP and `ApplySettings`.
- Fix: make every map read use a locked snapshot. Avoid simply adding locking inside `MOTD`, because `Snapshot` currently calls it while already holding `s.mu`.

```go
// model.go

func (s *Server) motdLocked() string {
	if v, ok := s.Props["motd"]; ok {
		return v
	}
	return "A Minecraft Server"
}

func (s *Server) MOTD() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.motdLocked()
}

func (s *Server) Prop(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Props[key]
}

func (s *Server) Snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	// ...existing snapshot construction...

	return map[string]any{
		// ...
		"motd": s.motdLocked(),
		// ...
	}
}
```

```diff
// manager.go - SettingsView

 func (m *Manager) SettingsView(s *Server) []map[string]any {
+    s.mu.Lock()
+    props := make(map[string]string, len(s.Props))
+    for k, v := range s.Props {
+        props[k] = v
+    }
+    s.mu.Unlock()
+
     groups := []string{"Gameplay", "World", "Network"}
     byGroup := map[string][]map[string]any{}

     for _, meta := range propSchema {
-        v, ok := s.Props[meta.Key]
+        v, ok := props[meta.Key]
         if !ok {
             continue
         }
```

```diff
// runner.go - simulator boot

- {130 * time.Millisecond, func() {
-     info("Default game type: %s", strings.ToUpper(s.Props["gamemode"]))
- }},
+ {130 * time.Millisecond, func() {
+     info("Default game type: %s", strings.ToUpper(s.Prop("gamemode")))
+ }},
```

### 2. [HIGH] Live backups proceed even when the game was never quiesced

- File: `internal/arcade/backup.go` (`Manager.CreateBackup`)
- Problem: for a running server, `CreateBackup` sends `save-off` and `save-all flush` but discards both errors, sleeps 1.5 seconds, and starts archiving regardless. If RCON is unavailable, the server is still starting/stopping, the console implementation does not support those commands, or the game is one of the supported non-Java servers for which Minecraft's save commands are meaningless, the backup is taken while the game can still be writing world files. The resulting archive can contain a torn or internally inconsistent world while the API reports a successful quiesced backup. The same source already treats inability to deliver these commands as fatal when cloning, so backup currently provides weaker consistency than clone.
- Fix: abort a live backup unless its quiesce sequence is known and every required command succeeds. For games without a supported quiesce protocol, require the server to be stopped until a game-specific implementation exists.

```go
func (m *Manager) quiesceForBackup(s *Server) (resume func(), err error) {
	if s.State() != StatusRunning {
		return func() {}, nil
	}

	// Only use commands whose semantics this panel actually knows.
	if s.Game != "minecraft-java" {
		return nil, fmt.Errorf(
			"live backups are not quiesce-safe for %s; stop the server before backing it up",
			s.Game,
		)
	}

	r := m.runnerFor(s)

	if err := r.Send(s, "save-off"); err != nil {
		return nil, fmt.Errorf("could not pause world saves: %w", err)
	}

	resume = func() {
		if err := r.Send(s, "save-on"); err != nil {
			log.Printf("%s: could not resume world saves after backup: %v", s.Name, err)
		}
	}

	if err := r.Send(s, "save-all flush"); err != nil {
		resume()
		return nil, fmt.Errorf("could not flush the world before backup: %w", err)
	}

	time.Sleep(1500 * time.Millisecond)
	return resume, nil
}
```

```diff
 func (m *Manager) CreateBackup(s *Server, note, actor string) (*Backup, error) {
     // ...

-    wasRunning := s.State() == StatusRunning
-
-    if wasRunning {
-        m.panelLine(s, "info", "Backup starting - pausing world saves and flushing to disk.")
-        _ = m.runnerFor(s).Send(s, "save-off")
-        _ = m.runnerFor(s).Send(s, "save-all flush")
-        time.Sleep(1500 * time.Millisecond)
-    }
-
-    defer func() {
-        if wasRunning {
-            _ = m.runnerFor(s).Send(s, "save-on")
-            m.panelLine(s, "info", "Backup finished - world saves resumed.")
-        }
-    }()
+    resume, err := m.quiesceForBackup(s)
+    if err != nil {
+        return nil, err
+    }
+    defer resume()

     // archive only after successful quiesce
```

### 3. [HIGH] Backup/file exclusion has a TOCTOU window

- File: `internal/arcade/backup.go` (`lockBackup`), `internal/arcade/files.go` (`WriteFile`, `DeletePath`, `MkDir`), `internal/arcade/plugins.go` (`SetPluginEnabled`, `DeletePlugin`, `InstallPlugin`)
- Problem: file mutations call `backupLocked`, release the backup-state mutex, and only then perform the filesystem operation. `CreateBackup` can acquire its backup lock immediately after that check and begin walking the tree while the previously-approved write/rename/delete is still executing. This defeats the stated invariant that file writes are refused for the whole archive window. `MkDir` is worse: it has no backup check at all. An archive can therefore see a temporary atomic-write file, old/new versions inconsistently, a jar during rename, or a directory created after traversal began.
- Fix: use an actual per-server read/write filesystem gate. Backups/restores take it exclusively for their entire filesystem window; ordinary file/plugin mutations hold a shared lock from before the backup-state check through the completed mutation.

```go
// model.go

type Server struct {
	// ...
	mu sync.Mutex

	// fsMu serialises snapshot/restore against mutations of the server tree.
	// Mutations take RLock; backup/restore take Lock.
	fsMu sync.RWMutex

	proc procHandle
	// ...
}
```

```diff
// backup.go

 func (m *Manager) CreateBackup(s *Server, note, actor string) (*Backup, error) {
+    s.fsMu.Lock()
+    defer s.fsMu.Unlock()
+
     if !m.lockBackup(s.ID) {
         return nil, fmt.Errorf("a backup is already running for this server")
     }
     defer m.unlockBackup(s.ID)
     // ...
 }

 func (m *Manager) RestoreBackup(s *Server, backupID, actor string) error {
+    s.fsMu.Lock()
+    defer s.fsMu.Unlock()
+
     // existing restore
 }
```

```diff
// files.go

 func (m *Manager) WriteFile(s *Server, rel, content string) error {
+    s.fsMu.RLock()
+    defer s.fsMu.RUnlock()
+
     // existing operation
 }

 func (m *Manager) DeletePath(s *Server, rel string) error {
+    s.fsMu.RLock()
+    defer s.fsMu.RUnlock()
+
     // existing operation
 }

 func (m *Manager) MkDir(s *Server, rel string) error {
+    s.fsMu.RLock()
+    defer s.fsMu.RUnlock()
+
     // existing operation
 }
```

The same `RLock` must surround plugin enable/disable/install/delete operations, not merely their initial `backupLocked` test.

### 4. [HIGH] Restore rollback can leave a mixed world, and adopted trees can fail across filesystems

- File: `internal/arcade/backup.go` (`RestoreBackup`, `restoreHeld`)
- Problem: restore extracts to `m.dataDir` and then moves the live tree's entries into `staging/.previous` using `os.Rename`. An adopted-in-place server can reside on a different filesystem, in which case the first rename fails with `EXDEV`, so restore can never work for that otherwise-supported layout. Separately, once new staged entries start moving into the live directory, a later rename failure calls `restoreHeld` without removing already-installed new entries. Restoring an old file with the same name then fails because the new destination already exists, leaving a mixture of the old and new worlds despite the error message saying the previous world was put back.
- Fix: stage on the same filesystem as the target and explicitly remove every newly-installed top-level entry before moving held entries back.

```go
func rollbackRestore(installed []string, held, dir string) {
	for _, name := range installed {
		_ = os.RemoveAll(filepath.Join(dir, name))
	}
	restoreHeld(held, dir)
}

func (m *Manager) RestoreBackup(s *Server, backupID, actor string) error {
	// ...validation and stopped-state checks...

	// Keep the staging transaction on the target filesystem. A random hidden
	// child works even when an adopted tree's parent is not writable.
	staging, err := os.MkdirTemp(dir, ".arcade-restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	extracted := filepath.Join(staging, "new")
	held := filepath.Join(staging, "old")
	if err := os.MkdirAll(extracted, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(held, 0o700); err != nil {
		return err
	}

	if err := untarGz(archive, extracted); err != nil {
		return fmt.Errorf("restore failed, the live world was not touched: %w", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == filepath.Base(staging) {
			continue
		}
		if err := os.Rename(
			filepath.Join(dir, e.Name()),
			filepath.Join(held, e.Name()),
		); err != nil {
			restoreHeld(held, dir)
			return fmt.Errorf("could not clear the live world, nothing was changed: %w", err)
		}
	}

	var installed []string
	staged, err := os.ReadDir(extracted)
	if err != nil {
		restoreHeld(held, dir)
		return err
	}
	for _, e := range staged {
		name := e.Name()
		if err := os.Rename(
			filepath.Join(extracted, name),
			filepath.Join(dir, name),
		); err != nil {
			rollbackRestore(installed, held, dir)
			return fmt.Errorf("restore failed part way; the previous world was restored: %w", err)
		}
		installed = append(installed, name)
	}

	return nil
}
```

### 5. [HIGH] Import reports success after persistence/configuration failures

- File: `internal/arcade/import.go` (`finishImport`, `StartImport`)
- Problem: `finishImport` registers the new server, then logs and ignores failure from both `m.Save()` and `m.writeProps(s)`, audits the import, broadcasts `server.created`, and marks the job done. If `servers.json` cannot be written, the UI reports a completed import that disappears at restart. If `server.properties` cannot be rewritten, the panel records the chosen port while the imported game continues to bind its old port; the next start can collide with another server or expose a service at a different address than the UI reports. `StartClone` already rolls back these exact failure classes, making the import path inconsistent with the safer clone path.
- Fix: make `finishImport` return an error and undo registration on any failure before the job is marked complete. Copy imports should delete the copied destination on failure; adopted imports should only remove the panel-created symlink, never the external target.

```go
func (m *Manager) finishImport(
	s *Server,
	sc *ImportScan,
	actor string,
) error {
	m.mu.Lock()
	m.servers[s.ID] = s
	m.order = append(m.order, s.ID)
	delete(m.reservedPorts, s.Port)
	m.mu.Unlock()

	rollbackRegistration := func() {
		m.mu.Lock()
		delete(m.servers, s.ID)
		for i, id := range m.order {
			if id == s.ID {
				m.order = append(m.order[:i], m.order[i+1:]...)
				break
			}
		}
		m.mu.Unlock()
	}

	if err := m.writeProps(s); err != nil {
		rollbackRegistration()
		return fmt.Errorf("could not write imported server.properties: %w", err)
	}

	if err := m.Save(); err != nil {
		rollbackRegistration()
		return fmt.Errorf("could not persist imported server: %w", err)
	}

	m.audit(actor, "server.import", s.ID,
		fmt.Sprintf("%s from %s", s.Name, sc.Path))
	m.broadcastEvent("server.created", s.ID)
	return nil
}
```

```diff
// copy worker

- m.finishImport(job, s, sc, actor)
+ if err := m.finishImport(s, sc, actor); err != nil {
+     _ = os.RemoveAll(dst)
+     m.releasePort(port)
+     job.fail(err)
+     return
+ }
+ job.done(s.ID)
```

For adopt mode, the error path should use `os.Remove(dst)` to remove only the symlink.

### 6. [HIGH] MCP authentication is an unauthenticated PBKDF2 CPU amplifier

- File: `internal/arcade/mcp.go` (`mcpTokens.Issue`, `mcpTokens.Check`)
- Problem: MCP bearer tokens are already high-entropy random values, but each request hashes the supplied token with `hashPassword`, which runs PBKDF2-HMAC-SHA256 for 120,000 iterations. `Check` executes before a caller is authenticated, so anyone who can reach `/api/mcp` can submit many random bearer tokens and force one expensive PBKDF2 computation per request. Parallel requests can consume all CPU even though no token is ever close to correct. The `mcpToken.Hash` comment says SHA-256, but the implementation uses the password KDF. Password stretching is valuable for low-entropy human passwords; it is unnecessary for a random 192-bit bearer secret.
- Fix: hash MCP tokens once with plain SHA-256 and compare the fixed-size digest in constant time. Existing PBKDF2 hashes need a one-time migration or token reissue.

```go
import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
)

func hashMCPToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func (t *mcpTokens) Issue(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("name is required")
	}
	suffix, err := randomHex(24)
	if err != nil {
		return "", err
	}
	raw := "tpa_" + suffix

	t.mu.Lock()
	defer t.mu.Unlock()
	t.toks = append(t.toks, mcpToken{
		Name:    name,
		Hash:    hashMCPToken(raw),
		Created: time.Now().Unix(),
	})
	return raw, t.save()
}

func (t *mcpTokens) Check(raw string) bool {
	if raw == "" {
		return false
	}

	h := hashMCPToken(raw)

	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now().Unix()
	for i := range t.toks {
		if subtle.ConstantTimeCompare(
			[]byte(t.toks[i].Hash),
			[]byte(h),
		) != 1 {
			continue
		}

		if now-t.toks[i].LastUse >= lastUseResolution {
			t.toks[i].LastUse = now
			if err := t.save(); err != nil {
				log.Printf("mcp: could not record token use: %v", err)
			}
		}
		return true
	}
	return false
}
```

### 7. [HIGH] Failed Docker stop is reported as accepted and disconnects the panel from a still-running container

- File: `internal/arcade/runner.go` (`dockerRunner.Stop`), `internal/arcade/manager.go` (`Manager.Stop`)
- Problem: `dockerRunner.Stop` explicitly discards the result of `docker stop`, calls `cancelProc` regardless, and returns `nil`. `Manager.Stop` has already changed status to `stopping` and its background worker also ignores the runner error. If Docker is unavailable or `docker stop` otherwise fails, the game container can remain alive while the panel cancels its `docker logs`, `docker wait`, and stats supervision. No exit event arrives to advance the state, leaving a live unmanaged game shown indefinitely as `stopping`. A later panel restart may recover it, but until then console/status supervision has been intentionally severed.
- Fix: return the Docker error and do not cancel the process watchers unless the stop request succeeded. If the stop worker fails, restore the observable state and report the failure.

```diff
// runner.go

 func (r *dockerRunner) Stop(s *Server) error {
     name := containerPrefix + "-" + s.ID
-    _ = exec.Command("docker", "stop", "-t", "45", name).Run()
+    if out, err := exec.Command("docker", "stop", "-t", "45", name).CombinedOutput(); err != nil {
+        return fmt.Errorf("docker stop failed: %w: %s",
+            err, strings.TrimSpace(string(out)))
+    }
     r.cancelProc(s)
     return nil
 }
```

```diff
// manager.go

 go func() {
     defer recoverPanic("stop worker for " + s.ID)
-    _ = m.runnerFor(s).Stop(s)
+    if err := m.runnerFor(s).Stop(s); err != nil {
+        m.panelLine(s, "error", "Stop failed: "+err.Error())
+
+        if s.Runtime == RuntimeDocker && containerRunning(s.ID) {
+            m.setStatus(s, StatusRunning, 0, "")
+        } else {
+            m.setStatus(s, StatusFailed, 1, err.Error())
+        }
+        return
+    }
     if s.Runtime == RuntimeSim {
         time.Sleep(700 * time.Millisecond)
         m.stopped(s)
     }
 }()
```

The same rule should be applied to `Kill`: an asynchronous operation must not discard the runner error and then imply success.

### 8. [MEDIUM] `Create` can leave a ghost server after returning an error and can acknowledge a non-durable create

- File: `internal/arcade/manager.go` (`Manager.Create`)
- Problem: `Create` inserts the new server into `m.servers` and `m.order` and releases its reserved port before `seedServerFiles` runs. If seeding fails, `Create` returns an error to the API but leaves the server registered in memory. The client is told creation failed, yet a later list request can show the server and the port remains occupied by it. On the success path, the subsequent `m.Save()` error is discarded, so a server can be returned with HTTP 201 even though it will vanish at the next process restart. The clone path already correctly rolls back a registration when persistence fails.
- Fix: seed before exposing the server in the manager, then register and require persistence to succeed; roll back the map and tree if the save fails.

```go
func (m *Manager) Create(
	name, tplSlug, version string,
	port, memMB int,
	cpu float64,
	runtime string,
) (*Server, error) {
	// ...existing validation and port claim...

	s := m.newServer(name, t, version, port, runtime)
	if memMB > 0 {
		s.MemoryMB = memMB
	}
	if cpu > 0 {
		s.CPU = cpu
	}

	s.Props["max-players"] = itoa(s.MaxPlayers)
	s.Props["server-port"] = itoa(port)
	s.Props["motd"] = name

	dir := filepath.Join(m.dataDir, "servers", s.ID)
	if err := m.seedServerFiles(s); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}

	if s.Runtime == RuntimeDocker {
		chownTree(dir, containerRunUID, containerRunGID)
	}

	m.mu.Lock()
	m.servers[s.ID] = s
	m.order = append(m.order, s.ID)
	delete(m.reservedPorts, port)
	m.mu.Unlock()
	claimHeld = false

	if err := m.Save(); err != nil {
		m.mu.Lock()
		delete(m.servers, s.ID)
		for i, id := range m.order {
			if id == s.ID {
				m.order = append(m.order[:i], m.order[i+1:]...)
				break
			}
		}
		m.mu.Unlock()
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("could not persist new server: %w", err)
	}

	m.broadcastEvent("server.created", s.ID)
	return s, nil
}
```

### 9. [MEDIUM] Port changes are check-then-set and ignore in-flight reservations

- File: `internal/arcade/manager.go` (`ApplySettings`, `portOwner`, `claimPort`)
- Problem: `ApplySettings` validates a new `server-port` with `portOwner` and later updates `s.Port` under only the server mutex. Two concurrent settings requests for different stopped servers can both observe a free port and both commit it. `portOwner` also knows nothing about `reservedPorts`, so a port held by a long-running import or clone can simultaneously be assigned to an existing server through Settings. The conflict is eventually caught only when starts are attempted, leaving persisted panel state with duplicate port ownership.
- Fix: reserve the candidate port through the same manager-wide mechanism used by create/import/clone, keeping the reservation until the setting has been persisted and releasing the server's old ownership only through the registered server record.

```go
func (m *Manager) changeServerPort(s *Server, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("port %d is out of range", port)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, id := range m.order {
		other := m.servers[id]
		if other == nil || other.ID == s.ID {
			continue
		}
		other.mu.Lock()
		taken := other.Port == port
		name := other.Name
		other.mu.Unlock()
		if taken {
			return fmt.Errorf("port %d is already used by %q", port, name)
		}
	}

	if holder, ok := m.reservedPorts[port]; ok {
		return fmt.Errorf("port %d is reserved by %q", port, holder)
	}

	s.mu.Lock()
	s.Port = port
	s.Props["server-port"] = itoa(port)
	s.mu.Unlock()
	return nil
}
```

`ApplySettings` should call this atomic manager-level operation instead of performing `portOwner` and the write as separate steps.

### 10. [MEDIUM] Task PATCH clears omitted boolean fields

- File: `internal/arcade/api_ext.go` (`updateTask`)
- Problem: `updateTask` decodes a PATCH request directly into `Task`. String fields are conditionally copied only when non-empty, but `Repeat` and `Enabled` are assigned unconditionally. Their Go zero value is `false`, so a caller sending a legitimate partial PATCH such as `{"name":"Nightly reboot"}` silently changes a repeating enabled task into a disabled one-shot task. The method is explicitly PATCH and the surrounding implementation already treats strings as optional, so the boolean behavior violates the endpoint's own partial-update semantics.
- Fix: decode optional booleans as pointers and update only fields actually supplied.

```go
func (a *API) updateTask(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     *string `json:"name"`
		Commands *string `json:"commands"`
		Time     *string `json:"time"`
		Repeat   *bool   `json:"repeat"`
		Enabled  *bool   `json:"enabled"`
	}

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, err)
		return
	}

	t, err := a.mgr.sched.Update(r.PathValue("tid"), func(t *Task) {
		if body.Name != nil {
			t.Name = *body.Name
		}
		if body.Commands != nil {
			t.Commands = *body.Commands
		}
		if body.Time != nil {
			t.Time = *body.Time
		}
		if body.Repeat != nil {
			t.Repeat = *body.Repeat
		}
		if body.Enabled != nil {
			t.Enabled = *body.Enabled
		}
	})
	if err != nil {
		writeErr(w, 400, err)
		return
	}

	a.mgr.audit(actorOf(r), "task.update", r.PathValue("id"), t.Name)
	writeJSON(w, 200, t)
}
```

### 11. [LOW] Invalid `!wait` text silently becomes a zero-second wait

- File: `internal/arcade/scheduler.go` (`Scheduler.step`)
- Problem: `!wait` parses its argument with `atoi`, whose documented contract is to return zero for invalid input. Zero is a valid wait duration, so `!wait sixty`, `!wait 1.5`, or any typo passes validation and executes immediately. A scheduled sequence such as `say Restarting in 60 seconds; !wait sixty; !restart` therefore restarts immediately after announcing a delay. `parseClock` already received a strict parser specifically to eliminate the same class of silent conversion.
- Fix: parse scheduler durations with `strconv.Atoi` and reject malformed input explicitly.

```diff
+ import "strconv"

 case "wait":
     d := 5
     if len(fields) > 1 {
-        d = atoi(fields[1])
+        var err error
+        d, err = strconv.Atoi(fields[1])
+        if err != nil {
+            return fmt.Errorf("wait must be a whole number of seconds")
+        }
     }
     if d < 0 || d > 900 {
         return fmt.Errorf("wait must be between 0 and 900 seconds")
     }
     time.Sleep(time.Duration(d) * time.Second)
     return nil
```
