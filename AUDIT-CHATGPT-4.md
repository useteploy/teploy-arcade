# teploy-arcade - Audit Pass 4 (2026-09-12)

## Verdict

**8 findings: 5 High, 3 Medium.** The Pass-3 lock-order repair is otherwise intact: I found no remaining `lifecycle -> fsMu` path; `Start`, `Delete`, `routeListChange`, clone and import follow the intended `fsMu -> lifecycle` ordering or avoid nesting entirely. `Stop` now does transfer its exclusive `fsMu` hold to the worker, `SetPassword` does both PBKDF2 derivations outside `Auth.mu` and revalidates the self-service snapshot before commit, and the `.part` backup path prevents a crash from exposing a partial archive under the final `.tar.gz` name.

The defects below are concrete gaps around those fixes: one leaked lifecycle lock, two remaining launch/port races, incomplete binding-set admission, broken interrupted-restore rollback, a queued-stop replay, loss of no-overwrite semantics in `.part` publication, and stale-login authority after password/user revocation.

## Findings

### 1. [HIGH] Clone persistence failure leaks the global lifecycle mutex

**File:** `internal/arcade/clone.go` — `Manager.StartClone`

After `m.lifecycle.Lock()`, the `m.Save()` error branch rolls back registration, releases the port, marks the job failed, and returns without `m.lifecycle.Unlock()`. A disk-full/read-only failure while persisting a completed clone therefore leaves `lifecycle` locked forever; later starts, deletes, stopped player-list edits, creates, imports and clones block behind it.

### 2. [HIGH] Settings can still change launch inputs across Docker argument construction

**Files:** `internal/arcade/manager.go` — `Manager.ApplySettings`, `Manager.SetResources`, `Manager.Start`; `internal/arcade/runner.go` — `dockerRunArgs`

`ApplySettings` calls `changeServerPort` before taking `s.fsMu.RLock()`. Interleave Start reading port 25565 in `publishArgs`, then a PATCH committing 25566, then Start reading `s.Port` again for `SERVER_PORT`: Docker publishes 25565 while the game listens on 25566. Separately, once `docker run -d` returns, a server may remain `starting`; `SetResources` can then change memory/CPU, sees only `running` as restart-pending, and reports the new limits as applied although the already-created container still has the old ones.

### 3. [HIGH] `portBinding` comparisons expand existing servers but not candidate servers

**Files:** `internal/arcade/manager.go` — `serverBindingsLocked`, `claimPort`, `NextFreePort`, `changeServerPort`, `claimStart`; `internal/arcade/files.go` — `validatePropsPort`; `internal/arcade/runner.go` — `publishArgs`

Existing servers are expanded to `PortSpan` and `ExtraPorts`, but candidates are still represented as only `{base/tcp, base/udp}`; `claimStart` compares only base-port equality. Example: Palworld A at 8211 and Palworld B at 8212 both need fixed `27015/udp`; B passes create and start admission, then Docker rejects the bind. Valheim/Rust candidate spans can likewise overlap an existing binding, and a high base can expand past 65535. Dynamic Geyser UDP bindings are also absent from the ledger entirely.

### 4. [HIGH] Interrupted-restore recovery can delete the previous world after failing to put it back

**Files:** `internal/arcade/manager.go` — `Manager.recoverInterruptedRestores`; `internal/arcade/backup.go` — `Manager.RestoreBackup`, `rollbackRestore`, `restoreHeld`

In-process rollback removes newly installed entries before restoring `staging/old`; boot recovery does not. If the panel dies after installing the new `world/` but before cleanup, startup calls `restoreHeld(old, dir)`. Renaming old `world/` over the already-present new directory can fail; `restoreHeld` ignores the error, recovery logs success, then `RemoveAll(staging)` deletes the old world. The lack of a durable phase/commit marker also means startup cannot distinguish a partial install from a completed restore whose staging cleanup was interrupted.

### 5. [MEDIUM] `server.properties` port publication is still split from port ownership

**Files:** `internal/arcade/files.go` — `Manager.WriteFile`, `validatePropsPort`, `reloadProps`; `internal/arcade/backup.go` — `Manager.RestoreBackup`; `internal/arcade/manager.go` — `changeServerPort`

Direct edits commit `s.Port` before `writeAtomicIn`; if the file write then fails (for example ENOSPC), the request fails but the model already moved to the new port while disk still contains the old one. Restore has the opposite gap: `validatePropsPort` checks and releases `m.mu` before the tree swap, so another request can claim the port before install; `reloadProps` then refuses the model change but leaves the restored conflicting `server.properties` on disk and the restore can still report success.

### 6. [MEDIUM] A queued second Stop can turn a clean stop into a failure

**Files:** `internal/arcade/manager.go` — `Manager.Stop`; `internal/arcade/runner.go` — `dockerRunner.Stop`

`Stop` checks only `stopped/failed` before acquiring `fsMu` and does not re-check after waiting. Stop A owns `fsMu` until Docker has stopped; Stop B arrives while status is `stopping`, passes the pre-check, and waits. After A finishes, B acquires `fsMu`, sets the already-stopped server back to `stopping`, and runs `docker stop` against a gone container. That error path can set the server to `failed` even though the first stop completed normally.

### 7. [MEDIUM] `.part` publication lost the backup no-overwrite guarantee

**File:** `internal/arcade/backup.go` — `Manager.CreateBackup`, `tarGz`

`CreateBackup` still retries on `os.ErrExist`, and `tarGz` says existing final names are refused, but the implementation opens only `dst + ".part"` and then calls `os.Rename(part, dst)`. On Unix, that rename replaces an existing regular `dst`; it does not return `ErrExist`. A repeated millisecond ID after a clock step/backward wall-time collision therefore silently replaces an older valid backup instead of retrying with a new ID.

### 8. [HIGH] `Auth.Login` can mint a session from credentials revoked while PBKDF2 was running

**File:** `internal/arcade/auth.go` — `Auth.Login`, `Auth.SetPassword`, `Auth.DeleteUser`

`Login` correctly copies salt/hash and hashes outside the mutex, but it never revalidates that snapshot before inserting the session. An old-password login can copy the old hash, then the owner changes the password and revokes sessions, then the in-flight PBKDF2 finishes and inserts a fresh 12-hour session from the revoked password. The same interleaving with `DeleteUser` can create a valid session for an account after the account and its prior sessions were deleted.
