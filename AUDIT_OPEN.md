# Open audit items

Unresolved findings for this repository from the ChatGPT-led audit series (2026-09-09 through 2026-09-11, passes 1-5; register: teploy-neutron-lullmail expanded audit). Every P0/P1 finding has been fixed and verified; the items below are the remaining P2/P3 tail plus one item needing validation. Fields are quoted from the audit register; line references point at the review commits listed per item where recorded.

Open items: 1 P2 improvement (1 total)

## useteploy__teploy-arcade-03 - P2 - Open improvement

**Make panel startup and shutdown own all spawned background workers**

- Kind: Improvement
- Evidence: Run launches metrics, sampling, guard, player-sync, scheduler and session-reaper goroutines before ListenAndServe, with no caller-visible cancellation/shutdown handle in this entry point.
- Impact: A bind failure or reuse of Run in tests/embedding can leave workers behind while the process remains alive; graceful panel restart policy is also not expressed here.
- Proposed fix: Accept an application context, bind the listener before starting long-lived workers, stop/join workers on errors and shutdown, and document which game containers intentionally survive the panel.
- Acceptance test: Force a port-bind failure and cancel a running panel; assert all panel-owned workers exit while the explicit game-runtime continuity policy is respected.
- Review commit: `8ff0ce6a6ab433cd72cb875e41891e6bba6e309d` (last reviewed 2026-09-10)


## Resolution log (2026-09-12)

- teploy-arcade-01, -02: FIXED - see audit commit (fan-out moved under r.mu; trySend stays nonblocking; DropRoom records absent-room tombstones; Join refuses dead rooms with tests).
- teploy-arcade-03: DEFERRED (design) - Run()'s worker-lifecycle ownership (context, pre-bind listener, join-on-shutdown) is an entry-point API change; decide the embedding/test story first.

## ChatGPT audit pass (2026-09-12, AUDIT-CHATGPT.md) - all 11 findings verified against source and fixed

1. CRITICAL unlocked Props reads (SettingsView, MOTD, sim boot) - FIXED: motdLocked/Prop locked accessors; Snapshot uses motdLocked; SettingsView copies under lock; sim boot reads via Prop. (players.go white-list read was already locked - audit false positive there.)
2. HIGH live backup ignores quiesce failures - FIXED: quiesceForBackup aborts unless save-off/save-all flush both succeed; non-minecraft-java live backups refused (stop first), matching clone's fail-closed behavior.
3. HIGH backup/file TOCTOU - FIXED: Server.fsMu RWMutex gate; backups/restores exclusive for their whole window; WriteFile/DeletePath/MkDir/writeProps/SetPluginEnabled/InstallPlugin hold shared through their mutation.
4. HIGH restore not transactional + EXDEV on adopted trees - FIXED: staging created inside the target dir (same filesystem); installed entries tracked and removed before restoreHeld on mid-install failure (rollbackRestore).
5. HIGH import ignores persistence failures - FIXED: finishImport errors and rolls back registration; copy failures remove the copied tree and release the port; adopt failures remove only the panel's symlink.
6. HIGH MCP PBKDF2 CPU amplifier - FIXED: bearer tokens hashed with plain SHA-256 (sha256: prefix marker), constant-time compare; legacy PBKDF2-stored tokens no longer authenticate (logged at load; reissue).
7. HIGH docker stop failure discarded - FIXED: dockerRunner.Stop reports the docker error and only cancels watchers on success; Stop/Kill workers restore running/failed state and surface the error on the console instead of wedging in stopping.
8. MEDIUM Create ghost server + ignored Save - FIXED: seed before registration; Save failure rolls back registration and the tree.
9. MEDIUM port check-then-set - FIXED: changeServerPort is one atomic manager-level operation (registered servers + reservedPorts under m.mu); ApplySettings moves the port before applying any other setting.
10. MEDIUM PATCH clears omitted booleans - FIXED: updateTask decodes pointer fields and only assigns supplied ones.
11. LOW !wait atoi silent zero - FIXED: strict strconv.Atoi with an explicit error.

Verification: go build, go vet, full go test ./... and go test -race ./internal/arcade all clean (2026-09-12).

## ChatGPT audit pass 2 (2026-09-12, AUDIT-CHATGPT-2.md) - all 10 findings verified and fixed

1. HIGH concurrent Save deadlock + stale rename - FIXED: Manager.saveMu serialises snapshot through atomic rename.
2. CRITICAL Start/Delete bypass fsMu; restore state-check raced Start - FIXED: Start holds fsMu around claim+launch; Delete takes fsMu before lifecycle; RestoreBackup locks fsMu BEFORE the state check. Lock order: fsMu -> lifecycle.
3. HIGH clone + player-list writes missed the fs gate - FIXED: clone is an exclusive snapshot transaction (fsMu + quiesceForBackup); writeList holds fsMu shared.
4. HIGH Create/import rollback could delete a started server - FIXED: provisional registration through persistence now runs under the lifecycle mutex.
5. HIGH failed adopt import left the operator's server.properties modified - FIXED: external file snapshotted before adoption and restored on every post-write failure.
6. MEDIUM restore measured the panel's disk - FIXED: server dir resolved first, diskFree(dir).
7. MEDIUM staging/.previous collided with the archive namespace - FIXED: extracted data and held entries live in staging/new and staging/old siblings.
8. MEDIUM reloadProps bypassed changeServerPort - FIXED: reloadProps commits port changes through changeServerPort and refuses (model AND disk stay consistent); WriteFile and RestoreBackup validate the staged port before any bytes land.
9. LOW pointer PATCH allowed blank name/commands - FIXED: validateTask enforced by both Add and Update with rollback.
10. HIGH boot sweep deleted any file containing .tmp - FIXED: panel temps carry the reserved ".arcade-tmp-" prefix (writeFileAtomic + writeAtomicIn); the sweep removes only that prefix.

Verification: go build, go vet, go test ./..., go test -race ./internal/arcade - all clean (2026-09-12).

## ChatGPT audit pass 3 (2026-09-12, AUDIT-CHATGPT-3.md) - 10 fixed, 1 refuted

1. HIGH lifecycle->fsMu deadlocks (routeListChange, finishImport) - FIXED: fsMu->lifecycle is the only legal order; writeList/writeProps split into held variants; finishImport and clone write properties BEFORE lifecycle registration.
2. HIGH Delete accepts stopping - REFUTED: deliberate tested behavior (TestDeleteCancelsARunnerThatIsStillStopping) - delete during stop cancels the runner instead of orphaning it; the stop worker + docker daemon finish the stop regardless of the entry.
3. HIGH Stop/Restart inside a quiesced backup - FIXED: Stop holds fsMu for the whole stop (worker releases), so the shutdown save cannot land inside a backup/clone window.
4. HIGH settings racing docker args - FIXED: ApplySettings and SetResources run inside an fsMu read section; Start's exclusive window cannot interleave.
5. HIGH ledger ignores spans/extras - FIXED: portBinding model (base..span + extra ports with protocols) compared in claimPort, changeServerPort, validatePropsPort and NextFreePort.
6. MEDIUM validatePropsPort TOCTOU - FIXED: WriteFile commits the port via changeServerPort BEFORE the file bytes land; reloadProps then sees port==current.
7. HIGH clone provisional exposure - FIXED: registration->Save under lifecycle, properties written first (same shape as import).
8. HIGH partial archive under final name - FIXED: tarGz assembles under .part and renames after fsync; boot sweep removes stray .part files.
9. HIGH restore not crash-atomic - FIXED: recoverInterruptedRestores at boot puts held ("old") entries back and drops untouched staging trees.
10. MEDIUM reloadProps swallows Save failure - FIXED: returns the error; WriteFile surfaces it; RestoreBackup logs it (tree already installed).
11. HIGH auth mutex across PBKDF2 - FIXED: copy-hash-relock (both derivations outside the mutex, snapshot revalidated under the write lock).

Verification: go build, go vet, go test ./... and go test -race - all clean (2026-09-12).

## ChatGPT audit pass 4 (2026-09-12, AUDIT-CHATGPT-4.md) - all 8 fixed

1. HIGH clone lifecycle leak - FIXED: defer unlock (the error branch returned past the manual unlock).
2. HIGH settings across docker args - FIXED: the port move runs inside ApplySettings' fsMu read section; SetResources also treats `starting` as restart-pending.
3. HIGH candidates not expanded - FIXED: candidateBindings() validates range and expands span/extras for Create (from template geometry), changeServerPort and validatePropsPort (from the server's own geometry); claimStart compares full binding sets; claimPortBindings checks reservations against every candidate member.
4. HIGH boot recovery could destroy the old world - FIXED: restores mirror the in-process rollback (remove staging/new entries from the live dir, then restoreHeld), and a .committed marker distinguishes interrupted installs from completed restores whose cleanup was interrupted.
5. MEDIUM port publication split - FIXED: WriteFile snapshots the old bytes, writes the new file, then commits the port; any failure restores the old file and reverts the model.
6. MEDIUM queued second Stop - FIXED: state re-checked under fsMu; a queued stop of an already-stopped server is an idempotent success.
7. MEDIUM .part rename lost no-overwrite - FIXED: link-then-unlink publication, so an existing final name fails with EEXIST and the re-stamp retry works.
8. HIGH Login stale-credential mint - FIXED: the salt/hash snapshot is revalidated under the write lock before the session is inserted.

Verification: go build, go vet, go test ./..., go test -race - all clean (2026-09-12).

## ChatGPT audit pass 5 (2026-09-12, AUDIT-CHATGPT-5.md) - all 5 fixed

1. HIGH live port change released the container's binding - FIXED: Server.BindPort records the port the live container was created with (set in claimStart, cleared in stopped/fail); serverBindings returns the UNION of live and desired binding sets during a transition, so the old host port stays held until the restart that actually recreates the container. ApplySettings' restart-pending snapshot is read under fsMu after any wait and includes `starting`.
2. HIGH reservations/admissions base-only - FIXED: every candidate member (span + extras) is written to reservedPorts; releasePort drops the holder's whole reservation; clone and import claim with their template's full geometry (candidateBindings), so a Valheim clone at 65535 is refused at admission; changeServerPort/validatePropsPort check every candidate member against reservations.
3. HIGH Geyser dynamic port absent - FIXED: dynamicBindings() probes the server tree (Geyser's Bedrock UDP) at claimStart OUTSIDE the manager lock and stores it on Server.DynamicBindings; serverBindings and the claimStart conflict check include it while the container is live.
4. HIGH boot recovery removed the wrong entries - FIXED: at swap time every live entry had been moved to old/, so recovery now removes ALL live entries (except staging) and then restores old/ - the installed set, not staging/new's not-yet-installed remainder. The .committed marker is written LAST, after ownership repair and props reload; a marker write failure logs loudly instead of failing a finished restore into a boot-time undo.
5. MEDIUM port publication split - FIXED: WriteFile writes the new file FIRST and commits the port SECOND (a failed write never moves the model; a failed commit only restores the old bytes - the silently-dropped model revert is gone). RestoreBackup republishes the model's port via writePropsHeld when the restored file's port was refused, so a raced restore can no longer report success with disk naming another server's port.

Verification: go build, go vet, go test ./..., go test -race - all clean (2026-09-12).
