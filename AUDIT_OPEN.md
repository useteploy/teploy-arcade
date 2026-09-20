# Open audit items

Unresolved findings for this repository from the ChatGPT-led audit series (2026-09-09 through 2026-09-19, passes 1-7; register: teploy-neutron-lullmail expanded audit). Pass 7 triaged all 74 of its findings: code landed for 37 (several closing their safe half, with the deeper tail carried below), 0 refuted, and 37 deferred - most corroborating deferrals this register already tracks. Read this before treating related work as done; update it when you close, defer, or upstream-report an item.

Open items: 17 P2 improvements + 1 pre-existing P2 from pass 6, plus the pass-7 deferral tails listed in their section below.

## useteploy__teploy-arcade-03 - P2 - Open improvement (pre-existing)

**Make panel startup and shutdown own all spawned background workers**

- Kind: Improvement
- Evidence: Run launches metrics, sampling, guard, player-sync, scheduler and session-reaper goroutines before ListenAndServe, with no caller-visible cancellation/shutdown handle in this entry point.
- Impact: A bind failure or reuse of Run in tests/embedding can leave workers behind while the process remains alive; graceful panel restart policy is also not expressed here.
- Proposed fix: Accept an application context, bind the listener before starting long-lived workers, stop/join workers on errors and shutdown, and document which game containers intentionally survive the panel.
- Acceptance test: Force a port-bind failure and cancel a running panel; assert all panel-owned workers exit while the explicit game-runtime continuity policy is respected.
- Review commit: `8ff0ce6a6ab433cd72cb875e41891e6bba6e309d` (last reviewed 2026-09-10)
- Pass-6 note (A33): the IPv6 address-formatting half was fixed (net.JoinHostPort in Run and the banner). The worker-ownership rework stays deferred for the same reason as 2026-09-12: it is an entry-point API change and the embedding/test story must be decided first. A32's context-threading through task steps should land with it.


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

## ChatGPT audit pass 7 (2026-09-19, AUDIT-CHATGPT-7.md) - 37 of 74 findings closed in code, 37 deferred, 0 refuted

Reviewed against `329e67e73352aa7f972182ee02bd489d73234d8c` (all 74 findings verified against source first). Regression tests in `internal/arcade/audit7_test.go`. Upstream (teploy-cli-owned) defects: none - all 74 live in this repository.

### Fixed (37)

1. R01 HIGH empty no-auth host passed the loopback guard but bound wildcard - FIXED: canonicalNoAuthHost accepts only literal loopback addresses (localhost normalised, empty host refused) before any worker starts.
2. R02 HIGH auth-mode checks straddled first-account creation - FIXED: gate() reads enabled/setup-required as ONE snapshot under one lock (gateState); regression test hammers the transition.
3. R03 MED creation/reset validation disagreed with login limits - FIXED: 128-byte name / 1024-byte password upper bounds enforced at CreateUser, CreateFirstUser and SetPassword, matching authInputOK.
4. R05 MED audit entries could amplify memory (partial) - FIXED half: every field is byte-capped and UTF-8-safely truncated at the one sink every record passes through. Deferred half below (own mutex/rotated store).
5. R09 MED MCP HTTP/JSON-RPC accepted malformed requests (partial) - FIXED halves: strict Bearer parsing with an HTTP 401 + WWW-Authenticate at the transport boundary, exactly-one-JSON-value check, jsonrpc version check, MCP ID-shape validation (string/integer; null is a notification and never answered). Deferred half below.
6. R11 MED WebSocket connections lacked admission control - FIXED: a 64-connection lease acquired before every console upgrade (mirroring the SSE cap); a join onto a deleted server's tombstone room closes the socket promptly (Hub.Dead).
7. R13 HIGH recursive ownership repair followed symlinks - FIXED: chownTree sets ownership with Lchown (the link's own inode), never Chown through it.
8. R15 MED directory opens could block on a FIFO - FIXED: ListFiles, the plugin directory listing and the archiver's walk open directories with O_DIRECTORY|O_NONBLOCK before any type check.
9. R18 HIGH boot recovery discarded staging when reading held originals failed - FIXED: a read error on old/ is no longer the empty case; the staging tree (the only remaining copy) is retained with a loud log line.
10. R22 MED startup cleanup recognised an overly broad temp suffix - FIXED: the `.tar.gz.part` suffix rule is gone; only the panel's `.arcade-tmp-` prefix is swept.
11. R24 MED port changes lost restart warnings and later edits erased earlier ones - FIXED: ApplySettings snapshots the pre-edit props before the port moves, adds the live-port restart flag itself, and MERGES into PendingRestart instead of replacing it.
12. R25 HIGH properties parsing disagreed with Java (partial) - FIXED half: the managed server-port identity is strict - the FINAL occurrence must be a valid port, duplicate keys are refused, and a colon separator is refused as ambiguity. Deferred half below.
13. R26 HIGH settings advertised as immediate were file-only - FIXED: every file-only key's Applies metadata is now next_restart (whitelist included), with help text saying so; "immediate" returns only with a verified live adapter.
14. R27 HIGH clone dropped resume errors (partial) - FIXED half: the clone's save-on resume must succeed before registration/job.done; a failed resume fails the job with the copy rolled back and the console told to run save-on. Deferred half: the verified save-state ack contract (A06).
15. R28 HIGH raw console commands could undermine the backup gate - FIXED: Manager.Send (HTTP, WS, scheduler, MCP) refuses busy while the server's filesystem gate is held exclusively; the quiesce transport bypasses Manager.Send and is unaffected.
16. R29 MED tar header could describe a different size than the descriptor copied - FIXED: the header and the copy both come from the opened descriptor (hdr.Size = st.Size, io.CopyN); a short read is an error.
17. R31 HIGH string-prefix path checks mishandled roots and sibling prefixes - FIXED: segment-aware relativeWithin (filepath.Rel) for import containment ("/" is recognised as an ancestor; /data vs /data-other distinguished) and hostPathFor mount translation.
18. R35 HIGH docker transport errors read as proof of death (partial) - FIXED half: ContainerState tri-state (running/stopped/missing/unknown); Start refuses `docker rm -f` on unknown, Stop/Kill workers leave the state to the still-attached watchers on unknown, watchExit retries on unknown. Deferred half: the typed Engine-API inspector (A24).
19. R43 MED metrics sampler read s.CPU outside the lock - FIXED: the quota is copied under the same lock as the samples it scales.
20. R44 MED CPU percentages mixed units and memory parsing was incomplete - FIXED: docker's core-relative percentage is converted to percent-of-quota at ingestion (under the lock with the quota); parseMem handles B/KB/MB/GB/TB (IEC and SI) and refuses the unrecognised.
21. R46 MED the minimum Java heap could consume the whole container - FIXED: checkJavaMemory refuses <1024 MB effective memory on itzg-convention images (Create, SetResources, Import); jvmHeapMB never clamps the heap back up to the limit.
22. R47 HIGH the Rust template could expose the image's default RCON password - FIXED: the span is UDP-only so the RCON TCP port is no longer published, and every launch injects RUST_RCON_PASSWORD with the per-launch random secret after template env.
23. R48 HIGH Rust/Valheim did not describe their persistent paths (partial) - FIXED half: DataPath /steamcmd/rust and /config from the images' published contracts, port env wired. Deferred half: real-image persistence/launch contract tests and migration of pre-existing installs; templates stay preview.
24. R49 MED Bedrock configured a second listener without declaring the geometry - FIXED: PortSpan 2 (both UDP listeners reserved and published; base 65535 refused at admission).
25. R51 MED template validation missed catalog uniqueness (partial) - FIXED half: duplicate slugs are reported and the first kept; protocols, span bound and absolute DataPath validated before the catalog publishes. Deferred half: full launch-contract/capability validation.
26. R54 MED due tasks were silently dropped when slots were full (partial) - FIXED half: a skipped occurrence records its reason on the task (LastErr) instead of vanishing. Deferred half: the durable pending-occurrence queue.
27. R55 MED scheduler preview and loop disagreed on one-shots - FIXED: a pending one-shot whose moment passed today previews TOMORROW's occurrence - exactly what the loop fires; a completed one-shot previews none.
28. R57 MED editing a disabled task re-enabled it; toggles overwrote concurrent edits - FIXED: the dialog sends enabled only on create; the toggle PATCHes only {enabled}.
29. R58 MED a slower route request could overwrite a newer navigation - FIXED: router() takes a generation per navigation; only the newest may mount, stale candidates get gss:teardown.
30. R60 MED the Kick action inherited chat mode - FIXED: typed administrative actions pass mode explicitly; Kick validates the player name and always sends 'command'; ack IDs are collision-resistant.
31. R61 MED import UI could select adoption implicitly and retain a stale runtime - FIXED: copy stays selected when space is short (with the reason shown), adoption needs an explicit confirm, and a rescan resets mode/runtime to match the rebuilt controls.
32. R63 LOW the file UI hid directory truncation - FIXED: a truncated listing renders an explicit incompleteness notice.
33. R64 HIGH malformed plugin SHA-256 silently disabled verification - FIXED: ExpectedSHA256 refuses malformed digests BEFORE any download; the UI exposes the checksum field with format validation.
34. R66 LOW small UI affordances claimed unavailable behavior - FIXED: backup-note Cancel cancels; zero free bytes renders as zero (not unknown); the plugin screen's phantom upload claim removed; one in-flight guard for install (click and Enter).
35. R69 HIGH the release metadata expression rejected every image publication - FIXED: flavor latest=auto with semver tags (the action's own stable-vs-prerelease policy); the invalid `enable=!is_prerelease` raw rule removed.
36. R70 MED the Alpine runtime base was outside support - FIXED: runtime stage moved to alpine:3.24 (a supported branch); digest pinning stays with A40.
37. R74 LOW CLI default-path evaluation had side effects before flags decided - FIXED: the default data dir resolves after --version/--help return and only when -data was not given.

### Deferred, with rationale (37)

1. R04 - Login rate limiting / per-account session caps: corroborates A11; needs the idle-vs-active policy the reverted cap attempt documented, plus a per-origin limiter table design.
2. R05 (tail) - Audit store with its own synchronization and rotated append storage, plus a durable audit-health indication: the byte caps bound the amplification; the store split is a rework of Auth's locking.
3. R06 - http.CrossOriginProtection + mandatory JSON content type on mutations: corroborates A13; needs the TLS-termination/proxy decision so trusted origins are real.
4. R07 - Secure-cookie/public-origin configuration: corroborates A13's deployment half; deliberately not X-Forwarded-Proto.
5. R08 - Typed, scoped MCP tools replacing raw console: corroborates A27; a token/client migration decision.
6. R09 (tail) - Origin validation per the MCP transport spec and explicit protocol-version negotiation: the strictness that needs no product decision landed; the rest is a client-contract change.
7. R10 - 202 + durable operation IDs for long HTTP work: corroborates A14/A38; the async-operation resource is an API-shape change.
8. R12 - Strict JSON decoding (DisallowUnknownFields + one-value + content type) across mutation routes: needs the client migration first (the UI PATCHes stale full objects in places this pass fixed piecemeal).
9. R14 - Rooted, descriptor-based readers for import copying, player lists and Geyser config: a broadened os.Root refactor of paths that today use readSmall/copyFile by absolute name.
10. R16 - requireRegistered/editMu as a uniform invariant across every entry point, reads never mkdir: the pass-6 convention extended; a full entry-point sweep with barrier tests.
11. R17 - Publication-aware AtomicStateWrite (file+dir fsync outcomes) for private state: corroborates A10/A18; callers must handle Published before this lands.
12. R19 - Durable restore journal (manifest, phases, idempotent rollback): the R1/A07 generalization; crash-injection test harness included.
13. R20 - Persisted RecoveryRequired interlock blocking start/restore/clone/file mutations until resolved: depends on R19's journal being the thing that arms it.
14. R21 - Durable deletion tombstones + detach/delete intent split: corroborates A21; API-contract change with A35's dialog.
15. R23 - Candidate-then-commit transactions for ApplySettings/SetResources/Reorder/retention: corroborates A10's multi-resource half.
16. R27 (tail) - Verified save-state ack contract per template (command acceptance is not save state): A06 unchanged.
17. R30 - Cross-server shared-capacity disk reservations keyed by filesystem identity: corroborates A07's reservation-manager design.
18. R32 - Import parity with Create's invariants + frozen source selection (jar fingerprint, stopped-source policy): import admission redesign.
19. R33 - Clone freezes the source's complete launch configuration instead of re-deriving from the current template: LaunchSnapshot model.
20. R34 - Owned, durable, panic-safe import/clone jobs: folds into R10's operation model and A33's worker ownership.
21. R36 - Centralized bounded docker executor with per-op deadlines and shared daemon-health cache: corroborates A25.
22. R37 - Re-adoption rebuilds the live binding ledger from actual Docker PortBindings: the AppliedRuntime inspect-and-reserve design; needs the ownership labels.
23. R38 - One immutable LaunchPlan built once under the start gate (panel-visible read only, daemon path only for the mount): removes the second dynamic probe and the path-space mismatch.
24. R39 - Strict Geyser detection (enabled regular jar) and a reviewed bounded config parser returning errors, not defaults: needs the config-schema decision.
25. R40 - Cryptographically random server IDs + exclusive directory claim: the current scheme checks map+disk; the rework is an identity-format decision (deployed hosts embed these IDs in container names).
26. R41 - Log-follow reconnect with cursor/backoff and a tri-state readiness/health model: corroborates A26 plus a UI state contract.
27. R42 - Durable DesiredRunning + generation fencing for lifecycle intents: the lifecycle redesign shared with A32/A33.
28. R45 - Panel-scope vs daemon-host-scope capacity model: needs the remote-daemon deployment decision.
29. R48 (tail) - Real-image persistence/RCON/port contract tests per native template and migration of pre-existing installs (a template change does not move an existing container's world): the paths landed from the images' published contracts; verification needs boot tests.
30. R50 - Conservative template-seeding ownership (unknown ledger retains the active file; unique no-replace candidates; durable ledger writes): migration-policy decision.
31. R52 - Player-list raw-field preservation + a stopped-server identity contract: the ListEntry rewrite to RawMessage records plus an online/offline-mode adapter decision.
32. R53 - Source-marked player observations (null ping/UUID) and anchored log grammar: observability model change rippling into the UI.
33. R56 - Typed task steps with cancellation and lifecycle completion waits: corroborates A32; needs the application context from A33.
34. R59 - One client-side auth-state transition (401 coordinator, stopped reconnects, permission-derived disabling): a frontend rework of the api() wrapper and reconnect loops.
35. R62 - Optimistic concurrency (ETag/If-Match) for file and settings edits: API contract plus UI draft-preservation design.
36. R65 - HTTPS-only plugin downloads with audited exceptions and dialed-IP policy: corroborates A37's operator-trust decision.
37. R67/R68/R71/R72/R73 - Keyboard/focus/skip-link accessibility pass, configured public game-address policy, supply-chain pinning (A40), Run() worker ownership (A33/teploy-arcade-03), and an explicit corrupt-state recovery mode: all corroborate existing register items; unchanged rationale.

Verification (2026-09-19): gofmt clean, go vet clean, go test ./... clean (153s), go test -race ./... clean (259s), node test/routing.test.js 14/14, make build OK.

## ChatGPT audit pass 6 (2026-09-19, AUDIT-CHATGPT-6.md) - 42 of 43 findings closed in code, 17 deferred tails, 0 refuted

Reviewed against `dcfe671f707bb36fe541690fd27815b37c6435ae` (all 43 findings verified against source first). Regression tests in `internal/arcade/audit6_test.go`. Upstream (teploy-cli-owned) defects: none identified - all 43 findings live in this repository.

### Fixed (36)

1. A01 CRITICAL unclaimed panel served operational routes unauthenticated - FIXED: BeginSetup arms a setup gate; gate() answers 503 to every protected route while unclaimed (health/login/me/setup stay open); MCP dispatch refuses tokens while unclaimed; --no-auth now refuses to start on a non-loopback bind.
2. A02 CRITICAL boot recovery deleted originals evacuation never moved - FIXED: recovery removes only live entries whose names are held in old/ (provably replacements); unmoved originals stay; staging retained when the rollback cannot finish.
3. A03 restore rollback silently failed then deleted the only recovery copy - FIXED: restoreHeld/rollbackRestore propagate errors; RestoreBackup keeps staging when rollback is incomplete and says where the recovery data is.
4. A04 untar accepted an unverified gzip stream - FIXED: tar EOF drains gzip through its checksum (bounded tail), CopyN short-stream check, extract sync/close errors checked.
5. A05 starting server backed up without quiescing - FIXED: quiesceForBackup classifies states (starting/stopping refused; stopped/failed with a live container refused).
6. A06 (partial) live-backup trust and resume misreporting - FIXED halves: quiesce commands go through query() so a rejected command fails the backup; a failed save-on resume propagates into the response and console instead of "saves resumed". Deferred half below.
7. A07 (partial) ISIZE-based restore admission - FIXED half: extraction re-checks real free space per entry (the actual guard; ISIZE wraps). Deferred half below.
8. A08 archiver not root-confined, special files could block it - FIXED: tarGz walks and opens through a held os.Root, nonblocking regular-file opens, descriptor metadata, links skipped, special files refused.
9. A09 loaders accepted corrupt/null records - FIXED: unreadable users.json fails startup; invalid user records quarantine to an empty (setup-gated) set; Manager.Load drops null/no-id/duplicate server records; scheduler drops invalid tasks.
10. A10 (partial) in-memory mutation before persistence - FIXED for Auth (create/SetPassword/DeleteUser via commitUsersLocked), MCP tokens and scheduler tasks (persist-then-publish). Deferred half below.
11. A11 (partial) unbounded login work and audit fields - FIXED halves: credential length caps, 4-slot login semaphore, truncated claimed-name in audit. Deferred halves below.
12. A12 revoked sessions kept stream read access - FIXED: SSE reauthorizes on every frame and ping; the console writer reauthorizes every pass and closes on loss.
13. A13 (partial) cookie flags - FIXED half: Secure set when the request itself is TLS (setup + logout). Deferred halves below.
14. A14 deadline removal reopened slow-body/stalled-writer exhaustion - FIXED: createBackup parses its body before lifting deadlines (note capped at 4096); SSE writes carry rolling 20s deadlines; downloads use rolling 30s chunk deadlines; WS payload writes are 10s-bounded.
15. A15 shared fs locks did not serialize config transactions - FIXED: Server.editMu serializes WriteFile, ApplySettings, plugin toggle and install publication (lock order fsMu -> editMu; exclusive fsMu sections need none).
16. A16 properties identity/parsing disagreed - FIXED: root identity is the exact path "server.properties" (not basename); one last-wins parser (propsPortValue) feeds validation, WriteFile and reloadProps; reloadProps replaces the map wholesale so deleted keys die and new keys land.
17. A17 (partial) unbounded read/list work - FIXED halves: ReadFile enforces the cap on bytes consumed and regular files only; ListFiles reads one bounded page and reports truncation; downloads/reads refuse special files without blocking. Deferred half below.
18. A18 atomic replacement lost metadata/durability - FIXED: writeAtomicIn preserves the replaced file's permission bits, checks chown, syncs the parent directory; extraction restores archive permission bits (setuid/setgid/sticky stripped).
19. A19 port reservations leaked and shared display-name identity - FIXED: lease-based reservations (random lease ID + display label); whole-set release; release is atomic with registration across create/clone/import; no cross-release between same-named operations.
20. A20 live Server reads bypassed s.mu - FIXED: admission paths use the locking serverBindings wrapper (m.mu -> s.mu, documented order); serverMetrics, the resources audit line and Host() snapshot under the lock.
21. A21 (partial) Delete ignored cleanup/persistence failures - FIXED half: RemoveAll and Save failures are surfaced ("removed from the panel, but cleanup is incomplete"). Deferred half below.
22. A22 queued operations recreated deleted servers' directories - FIXED: requireRegistered re-validates registration at every file/plugin/backup entry point after gate acquisition.
23. A23 failed docker kill cancelled live supervision - FIXED: Kill cancels watchers only on success, under a 30s context (mirrors Stop).
24. A24 (partial) transport errors read as death, 137 always OOM - FIXED halves: watchExit inspects before reporting; a confirmed-running container means retry with backoff (supervision outlasts daemon hiccups); 137 says OOM only when the daemon's OOMKilled flag confirms it. Deferred half below.
25. A26 oversized/interrupted log lines silently ended the console - FIXED: readLogLine truncates in place and drains the overflow with a visible marker; stream-terminal errors are logged. (Reconnect deferred below.)
26. A28 duplicate MCP token names made revocation ambiguous - FIXED: duplicate normalized names refused at issue; Revoke removes every match; persist-then-publish.
27. A29 task creation was collision-prone and client-seeded - FIXED: create-only request shape, random unique IDs, caller pointer copied (never stored), run bookkeeping reset, requested enabled state preserved.
28. A30 task routes ignored the server ID - FIXED: Update/Delete/Run match (serverID, taskID) inside the scheduler lock; audits name the matched server.
29. A31 scheduler civil-time disagreement - FIXED: NextRun builds civil times with time.Date (no midnight+duration DST drift); the loop suppresses refires per local date, so the repeated fall-back hour cannot double-fire a task.
30. A34 player heads were opt-out - FIXED: headsEnabled is opt-in ('on'), storage failures read as off, avatar requests send no referrer, settings copy says off-by-default. Existing unset preferences are deliberately not converted to consent.
31. A35 delete confirmation understated the blast radius - FIXED: it now states the managed directory AND all panel backups are destroyed, that an adopted external directory survives, and that there is no undo.
32. A36 concurrent installs/toggles could overwrite a plugin - FIXED: publication and toggling use link-then-unlink (atomic no-replace) under editMu; the download runs before the gates and the existence checks are re-run inside the publication transaction.
33. A37 (partial) four-byte jar validation - FIXED half: full zip structural + per-entry CRC validation under a 512 MiB decompression budget, optional expected SHA-256. Deferred half below.
34. A38 (partial) install could land after the client timed out - FIXED half: InstallPlugin takes a context, downloads with it, and refuses to publish if it has died. Deferred half below.
35. A39 release publication was not gated by tests - FIXED: release and image jobs `needs: verify` which reuses the CI workflow (workflow_call); the `latest` image tag only moves on non-prereleases.
36. A41 archive assembly left partials and used a predictable temp name - FIXED: unique `.arcade-tmp-backup-*` temp (boot-swept prefix), deferred cleanup from creation, link-based publish plus parent-directory sync.
37. A42 creation validated supplied values, not effective ones - FIXED: unknown runtimes refused; effective memory/CPU (defaults included) re-checked against host fit before anything is created.
38. A43 NextFreePort could recommend an occupied/out-of-range port - FIXED: search capped at 65535, honest 0 on exhaustion; Create's port search walks the template's full binding geometry.

(Count note: entries marked "partial" landed their safe halves this pass and carry the remainder in the deferral list; A27, A31, A32, A33 and A25 also landed conservative improvements (namespace-stripped verbs, once-per-local-date firing, run slots, IPv6-safe bind, bounded kill/inspect) with their deeper reworks deferred. A40 is the only finding with no code change - it is a hardening program, tracked below.)

### Deferred, with rationale (17)

1. A06 - Verified live-snapshot ack contract: rcon delivery/rejection is now checked, but a per-template typed snapshot adapter that verifies save state (not command acceptance) is the real fix; needs a template-capability design. Until then the conservative failure paths refuse the backup.
2. A07 - Cross-server shared-capacity reservation manager (filesystem-identity keyed, mutex, deferred release): the per-entry free-space checks bound the destruction; strict concurrent-restore isolation needs the reservation service.
3. A10 - Multi-resource transactions for server state (servers.json + trees + model snapshots): a map-copy cannot make a multi-file operation atomic; wants the restore journal design (R1 in the report) generalized. The credential stores (the audit's named impact) are fixed.
4. A11 - Per-account session cap: REVERTED after implementation - evicting the oldest session conflicts with the tested invariant that a live session survives concurrent logins (TestALoginDoesNotDisturbLiveSessions). Needs an idle-vs-active policy (last-use tracking). Per-origin rate-limiter table also open; the semaphore bounds concurrency, not guessing frequency.
5. A13 - http.CrossOriginProtection middleware + mandatory application/json on mutation routes + explicit https/deployment config: needs a decision on how TLS is terminated (X-Forwarded-Proto is deliberately not trusted); bundling it with a reverse-proxy documentation pass.
6. A17 - Stable cursor pagination for directory listings (the truncated flag is honest but one page only).
7. A21 - Durable deletion tombstones + detach/delete API split (detach vs delete_managed_data vs delete_backups): an API-contract change that should land together with A35's proper confirmation dialog.
8. A24 - Structured docker tri-state supervision API (running/stopped/unknown as types, watcher re-registration): the retry loop landed; the typed API is a runner-interface change rippling through every caller.
9. A25 - Centralized bounded docker executor (cappedOutput + per-op deadlines): timeouts were added on the paths this pass touched; the single executor is a mechanical refactor best done alone.
10. A26 - Log-follow reconnect with cursor/backoff (the reader now survives oversized lines and reports terminal errors; automatic re-follow after daemon restarts is separate supervision work).
11. A27 - MCP typed tools replacing raw console send: the denylist now strips namespaces, but a denylist is not a capability boundary (the report says so itself); the durable fix is a typed, scoped toolset - a product-contract decision about which actions agents get.
12. A32 - Context threading through task steps + explicit run cancellation records: scheduled runs are now slot-bounded; cancellation wants the A33 worker-ownership rework so there is an application context to thread.
13. A33 - Run() worker ownership (pre-existing open item teploy-arcade-03 above; IPv6 formatting half fixed this pass).
14. A37 - HTTPS-only plugin downloads: HTTP mirrors are an operator-trust policy (the report agrees); enforcing https + an explicit exception flag is a deployment-contract change. Validation now proves integrity of whatever is downloaded; an expected digest can be supplied per request.
15. A38 - Async install/backup job resource (202 + operation id + idempotency): the sync path now respects the request context; the job resource is an API-shape change shared with A14's long-backup-handler note.
16. A40 - Supply-chain pinning (action SHAs, goreleaser version, base-image digests, govulncheck, browser suite): a hardening program, not a defect fix; needs reviewed pin choices made deliberately.
17. A34 (residual) - No behavioral JS test for the heads opt-in (node --check + routing test cover syntax/wiring only); a real browser suite is part of A40's program.

Verification (2026-09-19): gofmt clean, go vet clean, go test ./... clean (123s), go test -race ./... clean (178s), node test/routing.test.js 14/14, node --check on every frontend file, make build OK.

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
