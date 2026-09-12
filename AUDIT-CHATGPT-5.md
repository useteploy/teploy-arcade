# teploy-arcade - Audit Pass 5 (2026-09-12)

## Verdict

**5 findings: 4 High, 1 Medium. This is not a clean bill.** The Pass-4 clone-lock repair is present (`StartClone` now defers the lifecycle unlock once acquired), the queued-Stop recheck is under the filesystem gate, backup publication now uses link-then-unlink so an existing final archive still yields `EEXIST`, and `Login` revalidates the password snapshot under `Auth.mu` before minting the session. `SetResources` also correctly treats `starting` as restart-pending. fileciteturn1file2L392-L420 fileciteturn2file4L551-L594 fileciteturn3file2L250-L259 fileciteturn3file1L198-L212 fileciteturn9file0L35-L60

The remaining defects are concrete residuals in the newest port/admission and restore changes. I found no new defect in the deferred clone unlock itself, the link-based archive publication, the queued second-Stop fix, or Login's stale-credential revalidation. This was a source-level confirmation sweep of the supplied attachment; I did not re-run the Go 1.26 build/race gates in this sandbox.

## Findings

### 1. [HIGH] A live `server-port` change releases the port the existing container is still bound to

**Files/functions:** `internal/arcade/manager.go` — `Manager.ApplySettings`, `Manager.changeServerPort`, `Manager.claimStart`; `internal/arcade/runner.go` — `dockerRunner.Start` / `dockerRunArgs`.

`ApplySettings` immediately commits the desired port into `s.Port`, and the binding ledger derives ownership from that field. For a running server, however, Docker's published host port cannot change until the container is recreated. Concrete sequence: server A is running on 25565; PATCH changes A to 25566 and correctly reports a restart requirement; A's live container is still bound to 25565, but the ledger now says A owns 25566. Creating server B on 25565 therefore passes admission, and `claimStart` also sees no conflict; `docker run` for B then fails because A's existing container still owns 25565. The panel has released a live binding before the restart that actually releases it. fileciteturn4file1L114-L155 fileciteturn1file1L129-L196 fileciteturn2file4L525-L548

The `starting` window is worse: `ApplySettings` snapshots `running := s.State() == StatusRunning` before taking `fsMu`, so it excludes `StatusStarting` and can also become stale while waiting for `Start`'s exclusive hold. `dockerRunner.Start` returns after `docker run -d` and attachment while the server is still `starting`; a queued settings request can then move the port after the container was created from the old launch inputs and return with no restart requirement at all. fileciteturn1file1L129-L183 fileciteturn3file0L22-L72

### 2. [HIGH] Full binding-set admission is still not reserved atomically, and clone/import still use base-only claims

**Files/functions:** `internal/arcade/manager.go` — `candidateBindings`, `claimPortBindings`, `changeServerPort`; `internal/arcade/clone.go` — `Manager.StartClone`; `internal/arcade/import.go` — `Manager.StartImport`; `internal/arcade/files.go` — `validatePropsPort`.

`candidateBindings` now expands spans/extras, but `claimPortBindings` records only `cand[0].port` in `reservedPorts`. Two concurrent Valheim creates demonstrate the gap: A requests base 2456 and has candidate UDP bindings 2456-2458, but reserves only 2456; before A registers, B requests base 2457 with bindings 2457-2459, checks 2457/2458/2459 against the reservation map, never sees A's 2456 entry, and also succeeds. Both records can then be registered with overlapping bindings. `changeServerPort` and `validatePropsPort` likewise test only `reservedPorts[newBase]`, not every candidate member. fileciteturn1file0L15-L74 fileciteturn4file1L130-L155 fileciteturn2file3L348-L374

Clone and import have an additional bypass: after resolving a template they still call `claimPort`, which constructs a one-port TCP candidate. A direct, non-racy reproduction is cloning a Valheim server to base port 65535: `StartClone` accepts the base and reserves only 65535, while the cloned server inherits Valheim's three-port UDP span; later Start attempts to publish 65535, 65536 and 65537 and Docker rejects the out-of-range bindings. The full candidate validator that would reject this is never called on the clone path. fileciteturn1file2L264-L320 fileciteturn2file2L255-L278 fileciteturn7file0L28-L35

### 3. [HIGH] Geyser's runtime-derived UDP port is still absent from the admission ledger

**Files/functions:** `internal/arcade/manager.go` — `serverBindingsLocked`, `Manager.claimStart`; `internal/arcade/runner.go` — `publishArgs`, `geyserPort`.

The ledger expands only `Protocols`, `PortSpan` and static `ExtraPorts`, while `publishArgs` independently inspects the server tree and adds Geyser's configured/default Bedrock UDP port at launch. Concrete sequence: Paper A and Velocity B have distinct Java/proxy ports, both contain Geyser configured for 19132/udp, and A is already running. Starting B passes `claimStart` because neither static binding set contains 19132; `publishArgs` then adds 19132/udp for B and Docker rejects the bind because A already published it. This is the exact dynamic-binding case called out in Pass 4, and it remains outside the ledger. fileciteturn4file0L14-L55 fileciteturn2file4L525-L539 fileciteturn2file1L160-L197

### 4. [HIGH] Interrupted-restore boot recovery removes the not-yet-installed entries, not the entries already installed

**Files/functions:** `internal/arcade/backup.go` — `Manager.RestoreBackup`, `rollbackRestore`; `internal/arcade/manager.go` — `Manager.recoverInterruptedRestores`.

During restore, each successful rename removes that top-level entry from `staging/new` and places it in the live directory; the in-process path therefore keeps an explicit `installed` list for rollback. Boot recovery has no such list and instead enumerates what is still in `staging/new`, then removes those names from the live directory. Those are precisely the entries that had **not** been installed when the process died. fileciteturn2file0L41-L65 fileciteturn1file4L624-L641

Concrete crash: old `world/` and `server.properties` have been moved to `staging/old`; new `world/` has been renamed into the live directory; the process dies before new `server.properties` is moved. On boot, `staging/new` contains only `server.properties`, so recovery removes live `server.properties` (there is none), leaves the new `world/` in place, then `restoreHeld` cannot rename old `world/` over the non-empty new `world/`. That rename error is ignored, after which recovery deletes the staging directory and with it the only remaining copy of the old world. The `.committed` marker does not protect this pre-commit crash window. fileciteturn2file0L26-L65 fileciteturn1file4L618-L641

The marker is also written before ownership repair and `reloadProps`; a process death immediately after marker creation is treated on boot as a completed restore even though those post-install steps have not run. fileciteturn2file0L60-L85

### 5. [MEDIUM] `server.properties` port publication is still split from global port ownership

**Files/functions:** `internal/arcade/files.go` — `Manager.WriteFile`, `validatePropsPort`, `reloadProps`; `internal/arcade/backup.go` — `Manager.RestoreBackup`; `internal/arcade/manager.go` — `Manager.changeServerPort`.

The direct-edit path's comment says "write the new file, THEN commit the port", but the implementation calls `changeServerPort` first and writes the file second. Its failure rollback is another independent `changeServerPort` call whose error is discarded. Concrete interleaving: A moves its model from 25565 to 25566; before the file write fails on A's filesystem, B claims 25565; A restores the old file and tries to revert the model to 25565, but that revert now fails against B's reservation and is ignored. The request returns the filesystem error with A's disk back on 25565 and its model still on 25566. fileciteturn1file3L495-L542

Restore retains the opposite check-then-commit gap. It calls `validatePropsPort`, releases `m.mu`, then performs the tree swap; another create can claim that staged port before `reloadProps` eventually calls `changeServerPort`. If that happens, `reloadProps` logs the rejected port and deletes it from the parsed model update but does not rewrite the restored `server.properties` or propagate the port conflict as an error. The restore can therefore return success with the restored file naming a port now owned by another server while the panel model keeps the old port. fileciteturn4file2L270-L276 fileciteturn2file3L324-L438
