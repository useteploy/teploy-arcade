# BUGS.md — teploy-arcade audit findings

## 2026-08-15 — the Players sidebar was empty for a player who was standing in the world

Reported live: a player connects and never appears in the sidebar. Root-caused
against the deployed fleet rather than reasoned about, and it was two separate
defects that happened to hide each other.

**The join line the panel waits for is one a plugin can cancel, and Lobby's
does.** `X joined the game` is a chat broadcast. Lobby's own log carries the
arrival as

    [03:17:58 INFO]: UUID of player Steve_Example is 00000000-...
    [03:18:06 INFO]: Steve_Example[/192.168.1.160:46714] logged in with entity id 9 at (...)

and never prints the broadcast — but it *does* print `Steve_Example left the
game` on the way out. So the panel could only ever see people leave a world it
had never seen them enter, and removing a player who was never added is a silent
no-op. Nothing looked wrong from either side. Fixed by also matching the two
lines the server writes itself — `logged in with entity id` and
`lost connection:` — which no plugin suppresses and no resource pack translates.

**The fallback that should have caught it refused every answer that had anyone
in it.** `reconcilePlayers` asks the game over RCON and parses the reply.
EssentialsX, which this fleet runs, answers on two lines:

    There are 1 out of maximum 20 players online.
    default: Steve_Example

and the parser confined names to the count's own line — a deliberate choice, to
stop `Error: There's no one online in this group!` yielding a player called
"There's". It worked, and it also meant the reconcile was dead for every case
that mattered. It now reads the whole reply and uses the count as the check:
names are accepted only when there are exactly as many as the server said, so a
stray word refuses the answer rather than corrupting the list.

Two more things came out of chasing it, both now fixed:

- **The reconcile only ran on adopt, racing the log replay it was meant to
  correct.** It is on a 60s ticker now, and the adopt call waits for the tail to
  finish first. An announcement can always be missed — a cancelled broadcast, a
  shed line, a 200-line replay window — so the game itself gets the last word on
  a timer rather than once.
- **The proxy was being asked a question it can never answer.** Velocity speaks
  no RCON, so `rcon-cli` in the bungeecord image returns "authentication failed"
  every time. That was one `docker exec` per tick, forever, for nothing. Proxies
  are excluded now; their list comes from their own console, which is the one
  place a connect is always announced.

## 2026-08-15 — saving a setting made a server unstartable

Reported live: memory changed on Lobby, restart pressed, and the container died
before the world loaded.

    java.nio.file.AccessDeniedException: /data/server.properties
    [init] [ERROR] Failed to update server.properties
    panel Server exited 1 (exited).

The panel runs as root; the game runs as uid 1000. An atomic write replaces a
file with a **new inode owned by whoever wrote it**, so one settings save handed
root ownership of `server.properties` to a file the container has to write. The
write itself succeeded, so nothing in the panel noticed. On the deployed host it
was visible only as one file out of eight owned by `0:0`, timestamped at the
minute the setting was saved.

Fixed by handing the replacement back to the owner it is replacing - the file
being replaced decides, and a file that does not exist yet inherits its
directory. The panel's own state (`servers.json`, `users.json`, `audit.json`)
lives in a root-owned directory and so is left alone by the same rule.

Two things this taught, both worth keeping:

1. **The first fix changed nothing, and the test passed.** `writeFileAtomic` was
   the obvious place and it is not the path `server.properties` takes: writeProps
   goes through `writeAtomicIn`, which writes inside an `os.Root` for
   confinement. The unit test exercised the helper I had fixed rather than the
   route the panel actually takes, so it went green while the deployed binary
   still produced a root-owned file. Re-running the real reproduction on the host
   is what caught it - `before: 1000:1000 / after: 0:0`.
2. **The same defect was waiting in every path that writes a whole tree.**
   Creating a server seeds it as root into a root-owned directory, so a new
   docker server would have failed exactly the same way on its first start; and
   clone, copy-import and restore all produce trees owned by root. Those now hand
   the tree over too - to the source's owner where one exists, and to the image's
   uid otherwise.


**Date:** 2026-08-13
**Scope:** `internal/arcade/*.go`, `internal/mcp/*.go`, `cmd/teploy-arcade/main.go`
**Status (2026-08-13, fifth pass):** all 44 findings closed. L5 was the last one
and was the only one left standing as a mitigation; it is now fixed outright by
moving the file API onto `os.Root`. Every finding below now
carries its own **Status:** line — the earlier summary counts drifted from the
code twice, and a per-finding marker is the only version that cannot. Every fix
carries a regression test, and each new test was mutation-checked: the fix was
reverted to confirm the test actually fails without it.

Earlier passes ran a parallel fix run followed by an adversarial review that
re-derived every claim from source and ran the gates itself; what it caught is below.

**The review caught three things worth remembering, because they are the failure modes
this kind of work actually has:**

1. **A fix that stopped one file short of its own class.** `writeFileAtomic` was moved
   to a unique temp name; `writeAtomicNoFollow` kept the shared `<file>.tmp`, so two
   concurrent settings saves still raced (26 of 32 reproduced failing with ENOENT).
   Both now share one implementation.
2. **C5 fixed at the sites BUGS.md named, not at the site that shared the defect.**
   `Snapshot`, `writeProps` and MCP were fixed; `ApplySettings` still read and wrote
   `s.Props` unlocked — a *fatal* map race, not a recoverable panic. Now under the lock,
   with `TestApplySettingsDoesNotRaceReloadProps` driving all three goroutines.
3. **A regression test of mine went vacuous.** `TestStateFilesAreWrittenAtomically`
   checked for a literal `<file>.tmp`; once temps got random suffixes the assertion
   could no longer fail. It globs now. This is the exact failure the review phase exists
   to catch, and it was in the file written to prove the Criticals stayed fixed.

One test was deliberately re-aimed: `TestWriteFileRefusesAPlantedTempSymlink` became
`TestWriteFileIgnoresAPlantedTempSymlink`. Refusing the write was safe but was itself a
denial of service (planting `notes.txt.tmp` blocked all writes to `notes.txt`); with a
random temp name the planted path cannot be targeted, so the write now succeeds *and*
stays sandboxed. The assertions that matter — nothing outside the tree is touched, the
published file is not a symlink — are unchanged.

**Status (2026-08-13, third pass):** the four largest remaining items are closed —
H8's subscriber cap, M9's HTTP timeouts, M13's `DeleteBackup` lock and M14's
decompression-bomb cap — along with M10, L4, L8, L9 and L10. Two features landed
in the same run (Plugins, Import), and integrating them surfaced a third class of
defect worth recording, because neither feature's own suite could see it:

- **Both features registered their routes only inside their own tests.** Each
  suite built a fresh mux, called `RoutesPlugins`/`RoutesImport` on it and
  passed; the mux the binary actually serves had neither, so every new endpoint
  was a 404 in the shipping build with 140 green tests behind it.
  `TestPluginsAndImportAreOnTheShippingMux` now drives `Routes()` — the entry
  point `app.go` calls — instead of a hand-built one.
- **A 404-only check could not have caught it.** `POST /api/servers/{id}/{action}`
  matches the *path* of `GET`/`DELETE /api/servers/{id}/plugins`, so an
  unregistered plugin route answers **405**, not 404. The first version of that
  test passed on two of the seven missing routes.
- **`size_partial` was returned inverted.** `measureTree` reports *complete*;
  the field is its opposite, and the two were wired straight across, so every
  fully-measured scan called itself a floor ("at least 117 B" for four files).
  Found by running the binary, not by the suite — no test asserted on the field.

**Fourth pass (2026-08-13).** The remaining items were re-checked against source
one by one rather than against the running total above, which had drifted: most
of the "still open" set was already fixed, and two that the total counted as
closed were not. What actually needed work:

- **L3 was fixed too broadly first.** Refusing to create a room on `Publish`
  stopped the leak and also emptied the console: the ring fills *before* a
  viewer connects, so replay-on-connect, `Tail()` and MCP `arcade_console_tail`
  all went blank. Seven pre-existing tests caught it. The room is tombstoned
  now, so the leak closes without the buffer going with it.
- **H6 was half-fixed.** `actor` was overwritten by the session user with auth
  on, and trusted verbatim with auth off — the audit log is the one place a
  forged name matters most.
- **M17 was fixed in the wrong place.** The port range was checked in the HTTP
  handler; memory and CPU were not checked at all, and `StartImport` reached
  those fields without passing the handler. The bound now sits at the two
  boundaries every caller crosses.

Same pattern as the three lessons above, a fourth time: **a fix stops at the
site the report named, not at every site that shares the defect.**

Four tests were also re-aimed at the property they were supposed to guard,
after mutation-testing showed they could not fail:

- `TestCraftedBackupIDsCannotReachOutsideTheBackupDirectory` — the old version
  asserted only "restore returned an error", which a crafted id does anyway
  because no such archive exists. It passed with **both** guards deleted. It
  now plants a real archive where the traversal resolves to.
- `TestCapabilitiesAgreeWithTheRegisteredRoutes` — asserted route and flag
  *agree*, so deleting a feature from both sides passed. It now asserts each
  shipped feature is advertised **and** served.
- `test/routing.test.js` — held a hand-copy of `renderRail`'s rules, so it
  passed forever against logic `app.js` no longer had. It now extracts and runs
  the shipped `railActiveFor()`.
- `TestDeletedServerReleasesItsConsoleRoom` — asserted the map entry was gone
  (a mechanism) rather than the ring being freed and unrebuildable (the
  property). The tombstone satisfies the property and not the mechanism.

**Fifth pass (2026-08-13) — L5, the last one.** L5 was the only finding standing
as a mitigation rather than a fix, because eliminating it needs handle-relative
I/O. Go 1.24 shipped exactly that, and this project is on 1.26, so there was no
reason left to leave it open.

The file and plugin APIs now run every operation against an `*os.Root` opened on
the server's directory. Confinement is enforced by the kernel per path
component, by the same syscall that performs the operation, so the
check-then-use gap does not exist. Removed with it: `resolve`, `evalDeepest`,
`openNoFollow` and `writeAtomicNoFollow`, all of which existed to narrow a
window that is now closed.

**One behaviour deliberately changed.** `openNoFollow` refused *any* final
symlink; `os.Root` follows a symlink that stays inside the root and refuses one
that leaves. Following an internal link cannot escape by definition, and
refusing it broke nothing an operator wanted, so the stricter rule was not worth
carrying. `TestOpenNoFollowRefusesASymlink` tested that helper directly and went
with it, replaced by `TestSymlinksOutOfTheServerDirectoryAreRefused`, which
asserts the property at the API: links out are refused for read, write, delete
and list; links that stay inside still work.

The fix is verified by racing the defect rather than by inspection.
`TestPathOperationsCannotBeRacedOutOfTheServerDirectory` runs four goroutines
through the file API while a fifth flips `plugins` between a real directory and
a symlink pointing outside. Under the old resolve-then-operate model it plants a
file outside the server directory in under 0.3s. Under `os.Root` it never does.

**Sixth pass (2026-08-13) — the first real deploy.** The panel was installed on a
container host and a live server from another panel was migrated onto it. Six defects surfaced
that 159 passing tests did not, and every one of them needed real hardware or
real data:

- **Host capacity was hardcoded.** `hostMemMB = 16384` and `hostDiskGB = 200`
  were mockup-era constants never wired to anything. On a 4 GB container the panel
  claimed 16 GB, and `app.js` uses that figure for the over-allocation warning -
  so it would have approved four times the memory the box had. Now read from
  `/proc/meminfo` and the cgroup limit, whichever is smaller, and `statfs` on
  the data dir. An unmeasurable value reports 0 and the UI says "unknown"
  rather than inventing one.
- **A panel restart stopped every running server.** Containers were started with
  `docker run --rm -i`, making the panel's pipe the container's stdin; a
  Minecraft console treats stdin EOF as "shut down", so every upgrade or reboot
  stopped the servers cleanly with exit code 0 and nothing to explain it. The
  first two diagnoses (signal forwarding, then the systemd cgroup) were both
  wrong and were disproved by experiment before any code changed. Containers are
  detached now and commands go over RCON, which `Adopt` already did.
- **`/api/login` had no test at all.** Every test called `auth.Login()` directly.
  A misplaced edit put the setup-token gate inside `login`, breaking every
  sign-in, and the whole suite still passed. The most security-relevant route in
  the panel was untested end to end.
- **A fresh panel was claimable by anyone who could reach it.** No token gated
  `/api/setup`, so whoever arrived first became admin - and admin here means
  creating containers as root. Now gated by a bootstrap token, matching
  teploy-dash's design exactly.
- **Every classic Forge modpack was refused on import.** Forge's installer puts
  `forge-<ver>.jar` next to the vanilla `minecraft_server.<ver>.jar` it runs on;
  the scanner read that as two competing server softwares and would not guess.
  That is one server and its dependency. Vanilla is now dropped when exactly one
  other software is present; paper+forge stays ambiguous.
- **A loader jar with no version left the JRE unselectable.** Forge often leaves
  a plain `forge.jar`. The version picks the Java image, so an unknown one hands
  a 1.16.5 pack a Java 21 image it cannot start on. The version is now recovered
  from the vanilla jar beside it.

Two more were fixed pre-emptively because the migration would have hit them:
every Java template pinned `itzg/minecraft-server` untagged (Java 21), so a
1.12.2 pack could not start at all; and an imported server downloaded a fresh
jar for its version instead of running the one it arrived with, which for a
modpack means a loader build its mods were not compiled against.

Verified on hardware: Cobblemon (Fabric 1.21.1, Java 21), RL Craft (Forge
1.12.2, Java 8, 161 mods) and Pixelmon (Forge 1.16.5, Java 8) all imported from
Crafty, booted, and answered RCON. A panel restart under a running server left
it running.

**Also fixed in this pass:** nothing could change a server's memory or CPU after
creation (`PATCH /api/servers/{id}` was a 404), so RL Craft needed a hand-edited
`servers.json` and a panel restart to get 6 GB. `SetResources` now applies the
same bounds as Create and StartImport, and reports `pending_restart` when the
server is running, because the limits only reach the container on the next
`docker run`.

**Seventh pass (2026-08-14) — what the dashboard was actually measuring.** Three
issues, all found by an operator looking at a panel running eight real servers:

- **Host figures were commitment, not usage.** Every "allocated" number summed
  each server's configured limit regardless of whether it was running, so a
  deliberately overcommitted host read as a crisis: *22 / 4 vCPU* and
  *30.5 / 12 GB*, sitting next to "5 of 8 running". Limits are caps, not
  reservations, so exceeding the host is normal. The dashboard now leads with
  what is in use - host CPU from `/proc/stat` deltas, memory from
  `MemAvailable`, disk from `statfs` on the data directory - and keeps
  commitment in its own clearly-labelled panel, because it is still what a new
  server is checked against.
- **Per-server cost was invisible.** No screen answered "what is this server
  costing me". The dashboard now carries a table: CPU, memory against its
  limit, on-disk size, players and status, with start/stop/restart per row.
  Directory sizes are walked on a two-minute ticker rather than per request -
  a world is gigabytes and the dashboard asks for every server at once.
- **The tab strip hid servers.** It capped at five and put the rest behind a
  "+N" that was a bare `<span>` with no handler, so past the cap they were not
  reachable from the strip at all. Every server has a tab now and the strip
  scrolls; tabs also drag to reorder, which persists.

The drag felt unreliable for a reason worth recording: the metrics feed
redraws the strip every two seconds, and rebuilding the element under the
pointer cancels an in-flight drag. Redraws are deferred while dragging.

Fixes are in `internal/arcade/critical_test.go`, one test per finding:

| | Fix | Test |
|---|---|---|
| C1 | `Conn.sendMu` makes the closed-check and the send one atomic step; the atomic flag alone could not. | `TestFanoutNeverSendsOnAClosedChannel` |
| C2 | All 9 read endpoints now `auth.require(RoleViewer, …)`. Only `/api/health`, `/api/me`, `/api/login`, `/api/logout`, `/api/setup` stay open. | `TestEveryStatefulRouteRequiresASession` — enumerates routes, so a future ungated one fails the build |
| C3 | `writeFileAtomic` (temp + rename) for `users.json`, and every other state file. | `TestStateFilesAreWrittenAtomically` |
| C4 | `Append` marshals and writes inside `a.mu`; errors logged, not discarded. | `TestAuditAppendsAreNotLost` |
| C5 | `Snapshot` copies `Props` under the lock and exposes it as `settings`; `writeProps` builds its key list under the lock; MCP reads the snapshot. | `TestSettingsSnapshotDoesNotRaceReload` |
| C6 | Token store is atomic, and `LastUse` is debounced to 60s instead of rewriting on every request. | `TestMCPTokenUseIsDebounced` |

Verified end to end: before `/api/setup` the panel is open; after it, anonymous
callers get 401 on every stateful route while a session works and the login page
still loads.
**Method:** Three parallel audits (concurrency core / API+auth+data / files+backup+MCP) reading every file in full, followed by spot-verification of the top four Criticals against source (marked ✔ below).

## Severity legend
- **Critical** — crash, data loss, auth bypass, sandbox escape. Fix before exposing the panel to anything wider than localhost.
- **High** — likely in normal use; race / leak / injection under reachable conditions.
- **Medium** — needs specific timing or a privileged actor to trigger.
- **Low** — theoretical, minor, or cosmetic-but-real.

## Tally
| Severity | Count |
|----------|------:|
| Critical | 6 |
| High | 11 |
| Medium | 17 |
| Low | 10 |
| **Total** | **44** |

Plus 5 items flagged **Needs verification** (depend on code outside the audited file).

---

# Critical

### C1. ✔ Send-on-closed-channel race in Hub fanout panics the whole process
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `internal/arcade/hub.go:79-88` (`trySend`), `:70-75` (`Close`), `:134-143` (`Publish` sends outside lock), `:148-161` (`PublishRaw`); same shape; `:180-195` (`DropRoom`)
- **Verified:** yes — `trySend` checks `closed.Load()` then sends on `c.Send`; `Publish` releases `r.mu` before the send loop, so `Leave`/`DropRoom` can `close(c.Send)` in the gap.
- **Why:** A runner producing console output while a viewer disconnects (or a server is deleted) hits `panic: send on closed channel`. Some publish paths recover (`simRunner.run`, `dockerRunner.stream`), but several do **not** — `Manager.Stop`/`Kill`/`Restart` goroutines (`manager.go:396/412/431`), the `cmd.Wait` handler (`runner.go:536`), and the sim command goroutines (`runner.go:317/323/333`). In those the panic escapes and kills the panel — the exact failure the project's `recoverPanic` convention exists to prevent.
- **Fix direction:** Make `trySend` atomic with `Close` — either publish under `r.mu` (hold the lock across the fanout), give `Conn` its own mutex that `Close` takes, or `recover` inside `trySend`. The atomic-flag-before-close pattern alone is insufficient.

### C2. ✔ Six GET endpoints registered without `auth.require` — full unauthenticated disclosure
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `internal/arcade/api.go:24-29`
- **Verified:** yes — the six GET handlers wire bare (`a.getHost`, `a.listServers`, `a.getServer`, `a.getSettings`, `a.events`), while the POST/DELETE/PATCH handlers six lines below all wrap with `auth.require(...)`.
- **Why:** In a configured panel (`auth.Enabled() == true`) an unauthenticated client still gets: the full server list (names, ports, MOTD, player counts, status, exit reasons via `Snapshot`); per-server settings incl. `level-seed`, `server-port`, `online-mode`, whitelist state; and the live SSE feed broadcasting every status transition + metric sample. The asymmetric gating of `/api/servers/{id}/players` (gated, `api_ext.go:40`) vs `/api/servers/{id}/settings` (ungated) confirms oversight, not policy.
- **Fix direction:** Wrap all six with `auth.require(RoleViewer, ...)` the moment the first admin exists.

### C3. ✔ Non-atomic `users.json` write can brick auth → panel boots open → `/api/setup` takeover
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `internal/arcade/auth.go:110-117` (`saveUsers` = bare `os.WriteFile`), `:89-108` (`Load` → `quarantine` on parse fail), `:355-360` (`require` short-circuits to allow when `!Enabled()`), `api_ext.go:321-344` (`setup`)
- **Verified:** yes — `saveUsers` is truncate-then-write with no temp+rename. Same pattern: `Append` (`auth.go:316`), `writeList` (`players.go:124`).
- **Why:** A crash / OOM-kill / power loss / disk-full between truncate and complete write leaves `users.json` empty or partial. On next boot `Load` fails to parse → `quarantine` moves it aside → `a.users` empty → `enabled = len(users) > 0` is false → every `require(...)` short-circuits to allow. Anyone reaching `/api/setup` becomes the first admin; without setup, every mutating endpoint is open. The author flags this in a comment (`auth.go:93-95`) and treats it as acceptable — it is not.
- **Fix direction:** Write `users.json.tmp` then `os.Rename` (atomic on POSIX); refuse to boot if a quarantine marker from a prior run exists.

### C4. ✔ Audit `Append` writes the file outside the lock → lost entries on concurrent appends
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `internal/arcade/auth.go:305-317`
- **Verified:** yes — snapshot taken under `a.mu` (306-313), then `MarshalIndent` + `os.WriteFile` run lock-free (315-316), and the error is discarded.
- **Why:** Interleave A-snapshot → B-snapshot → B-write → A-write and the file ends up as A's older snapshot, silently dropping B's entry. The audit log is the only record of who did what (no DB); this lost-write is silent and the race detector will not catch it. Also non-atomic, compounding C3's exposure.
- **Fix direction:** Move marshal+write inside `a.mu.Lock()`, or serialize writes through a single goroutine fed by a channel. Use temp+rename.

### C5. Unlocked `s.Props` map read races `reloadProps` write → `fatal: concurrent map read and map write`
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `internal/arcade/mcp.go:131` (`snap["settings"] = s.Props` outside the lock taken by `Snapshot()`); `internal/arcade/files.go:292-303` (`writeProps` ranges `s.Props` unlocked); writer `internal/arcade/files.go:309-333` (`reloadProps` writes under `s.mu`)
- **Why:** Go panics fatally on concurrent map read+write (not recoverable). An MCP `arcade_get_server` call, or a settings save running `writeProps`, reading `s.Props` the instant a `server.properties` edit triggers `reloadProps` → process crash.
- **Fix direction:** Copy `s.Props` into the snapshot under `s.mu` (or inside `Snapshot()` itself); have `writeProps` build its key list under `s.mu`.

### C6. `mcp-tokens.json` rewritten non-atomically on every MCP request → mass token loss on crash
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `internal/arcade/mcp.go:47-50` (`save` = bare `os.WriteFile`), called from `:75` inside `Check` on every successful auth; load+quarantine at `:39-43`
- **Why:** Every authenticated MCP call updates `LastUse` and calls `save()`, overwriting the live file (truncate-then-write). A crash mid-write leaves a partial file → next boot `json.Unmarshal` fails → `quarantine` → **every MCP bearer token destroyed**, forcing an admin to re-issue them all. Because the write happens on every request, the corruption window is hit continuously under agent load.
- **Fix direction:** Temp+rename; skip the `LastUse` rewrite unless the value changed (debounce).

---

# High

### H1. `metricsLoop` is the only background loop missing `recoverPanic`
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `internal/arcade/manager.go:673-686`. Siblings all recover: `sampleLoop` (`metrics.go:111`), `guardLoop` (`metrics.go:143`), `scheduler.loop` (`scheduler.go:179`), session reaper (`app.go:67`).
- **Why:** Any panic on the 2s tick propagates to `runtime` and crashes the process. The asymmetry is the smell.
- **Fix direction:** `defer recoverPanic("metrics loop")` as the first line.

### H2. Adopted containers never cancel their context — `pollStats` leaks forever
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `internal/arcade/runner.go:564-598` (`Adopt`); `dockerProc.stop()` defined `:414` but never called; `dockerRunner.Stop` (`:675-680`) and `Kill` (`:682-684`) never call `p.cancel()`
- **Why:** In the `Start` path the `cmd.Wait` goroutine (`:536-540`) calls `cancel()` after `Wait` returns, so `pollStats`'s `ctx.Done()` fires. In the `Adopt` path there is no `cmd.Wait` goroutine, and none of `stream`/`watchExit`/`Stop`/`Kill` call `cancel()`. When an adopted container stops, `pollStats` (`:626`) keeps ticking every 3s forever, shelling out to `docker stats <name>` against a gone container. One leaked goroutine + one wasted `exec` per 3s, per adopted-then-stopped server, for the life of the process. The `Adopt` `exec.Cmd` is also never `Wait`ed (unreaped process).
- **Fix direction:** Have `watchExit` (or a new `cmd.Wait` goroutine in `Adopt`) call `cancel()`; have `dockerRunner.Stop`/`Kill` call `p.stop()` like `simRunner.Stop` does.

### H3. `pollStats` goroutine missing `recoverPanic`
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `internal/arcade/runner.go:626-651` (spawned `:534` Start, `:591` Adopt)
- **Why:** Locks `s.mu` every tick, dereferences `s`, parses docker output. Sibling goroutines `watchExit` and `stream` recover; this one doesn't. A nil `s` or helper panic crashes the panel.
- **Fix direction:** `defer recoverPanic("docker stats poller for " + s.ID)`.

### H4. `cmd.Wait` handler goroutine missing `recoverPanic`
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `internal/arcade/runner.go:536-540`
- **Why:** `go func() { err := cmd.Wait(); cancel(); r.mgr.processExited(s, err) }()` — no recover. `processExited` cascades through `stopped`/`fail`/`Save`/`broadcastEvent`/`pushPlayers`. If it panics, the panic escapes and kills the panel; if before `cancel()`, the Start-path `pollStats` also leaks (compounds H2). This is the single most active goroutine in the docker lifecycle — fires on every container exit.
- **Fix direction:** `defer recoverPanic("docker exit handler for " + s.ID)`.

### H5. WebSocket role check frozen at upgrade time — demoted/deleted users keep console access
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `internal/arcade/api.go:342-351` (WS read loop); `auth.go:273-299` (`DeleteUser` wipes tokens from the map but cannot revoke an in-flight upgrade)
- **Why:** The `*Session` is placed in `r`'s context by `Auth.attach` at upgrade time and captured for the connection's life. `DeleteUser` removes future tokens but the open WS still holds the live `*Session`, so `sess != nil` keeps passing and `sess.Role` is whatever they had at upgrade. A deleted admin retains full console command access until they happen to disconnect.
- **Fix direction:** Re-resolve the session from the token on each inbound message (or a short ticker) and close the socket when it no longer resolves / role dropped.

### H6. `serverAction` "command" trusts client-supplied `Actor` — audit/console forgery
- **Status:** FIXED (this pass) — the client-supplied `actor` field is gone. It was overwritten by the session user when auth was on, but written verbatim into the audit log when auth was off.
- **Where:** `internal/arcade/api.go:145-154`
- **Why:** The "command" case decodes `{Text, Mode, Actor}` and passes `body.Actor` straight to `m.Send(...)` without overriding from the session — unlike the console WS reader (`:338-351`) which deliberately overrides `actor = sess.User`. An operator can POST `{"text":"stop","actor":"another_op"}` and the action is attributed to the victim. The follow-up audit row records an empty detail string, so the command text is never logged under the right name — making forgery hard to detect.
- **Fix direction:** Drop `Actor` from this body shape; always derive from `actorOf(r)`, mirroring the console path.

### H7. Game-command injection via newline in player-list `reason`
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `internal/arcade/players.go:128-158` (`gameCommand`), `:160-182` (`AddToList` validates `who` at `:165` only, never `reason`)
- **Why:** `gameCommand` for `ban`/`ban-ip` returns `"ban " + who + " " + reason` with `reason` taken verbatim and unvalidated. Most game consoles (Minecraft included) treat `\n` as a command separator. `reason = "cheating\nop attacker\nwhitelist off"` executes three commands: the ban, then `op attacker`, then `whitelist off`. Operators are trusted with `ban`, not with arbitrary op-elevation.
- **Fix direction:** Strip `\r` and `\n` from `who` and `reason` before composing, or reject if either contains a line break.

### H8. SSE `/api/events` unauthenticated and unbounded — info leak + connection-exhaustion DoS
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `internal/arcade/api.go:208-245`, registered without `auth.require` at `:29`
- **Why:** Compounds C2 with a DoS: an unauthenticated attacker opens thousands of SSE connections (each holds a goroutine, a channel, a Ticker) and pins server resources. No `WriteTimeout` (see M-items) so these live until the client closes them.
- **Fix direction:** `auth.require(RoleViewer, ...)` + a per-IP/per-session cap on subscribers.

### H9. `writeProps` writes `server.properties` non-atomically (inconsistent with the file API)
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `internal/arcade/files.go:304` (bare `os.WriteFile`); the safe pattern is at the same file `:214-222`
- **Why:** A crash during `writeProps` truncates/corrupts `server.properties`. Minecraft refuses to boot on a malformed file (or resets to defaults) — the operator loses the entire configured settings surface. The tmp+rename in `WriteFile` exists precisely to prevent this; `writeProps` bypasses it.
- **Fix direction:** Route `writeProps` through the same write-temp-then-rename used at `:214`.

### H10. `WriteFile` writes to `abs+".tmp"` which is never symlink-resolved — defeats the documented sandbox
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `internal/arcade/files.go:214-215`; security model stated at `:17-19`; resolve logic `:66-106`
- **Why:** `resolve()` validates and `EvalSymlinks`-checks `abs`, but `abs+".tmp"` is written without any check and `os.WriteFile` follows symlinks. A symlink planted at `<serverdir>/<name>.tmp` (by a plugin, the game process, or anything running as the panel user — exactly the threat the file header claims to stop) sends the write to an arbitrary location. `os.Rename(tmp, abs)` then renames the *symlink*, so the real path becomes a dangling/outside symlink. Arbitrary file write outside the data dir.
- **Fix direction:** Re-run the sandbox check on the `.tmp` path, or open with `O_CREATE|O_WRONLY|O_NOFOLLOW` and reject if it exists as non-regular.

### H11. Nil-pointer deref in MCP `Lifecycle` after a concurrent server deletion
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `internal/arcade/mcp.go:178` (`b.m.Get(id).State()` with no nil check); the helper that exists for this (`:115-121`) is not used here
- **Why:** After `Start/Stop/Restart` returns nil, the success message calls `b.m.Get(id).State()` directly. If another admin deletes the server in between, `Get` returns nil and `.State()` panics. http recovers the goroutine, but the MCP client gets a broken response *after the action took effect* — a retrying agent issues a duplicate start/stop.
- **Fix direction:** Reuse `b.server(id)`, or store the `*Server` from the initial lookup and reuse it.

---

# Medium

### M1. `fail()` doesn't cancel the simulator — orphaned goroutine keeps mutating state
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `internal/arcade/manager.go:526-549` (`fail`); reached via `crash` console command (`runner.go:333`) and `processExited`
- **Why:** `fail` sets `s.proc = nil` and bumps `Restarts` but never calls the `simProc`'s `cancel()`. The `simRunner.run` select never sees `ctx.Done()`, so it keeps ticking: every 2s it rewrites `s.cpuPct`/`s.memMB`, every 12s `ambient` may emit console lines. A "failed" server shows live CPU/mem and new console output; status and reality diverge — exactly the sync/desync problem this project exists to solve. On delete, the orphaned goroutine is never cancelled → goroutine + room leak.
- **Fix direction:** `fail` (and `stopped`) should call `s.proc.stop()` before clearing `s.proc`. No-op for docker (cmd already gone); the missing `cancel()` for sim.

### M2. `Manager.Stop`/`Kill`/`Restart` worker goroutines missing `recoverPanic`
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `manager.go:396-402` (Stop), `:412-419` (Kill), `:431-443` (Restart)
- **Why:** Each spawns a goroutine that calls `m.stopped`/`m.fail` → `m.panelLine` → `m.hub.Publish` → `trySend`, which can panic on the C1 close-race. Without recover, that panic kills the panel from a code path reachable from an HTTP handler's `go`-statement — the request itself looks fine to the caller.
- **Fix direction:** `defer recoverPanic(...)` at the top of each closure.

### M3. Simulator `stop`/`flood`/`crash` command goroutines missing `recoverPanic`
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `runner.go:317`, `:323-327`, `:333`
- **Why:** Each is bare `go ...` reaching `m.emit`/`m.panelLine`/`m.fail` → `hub.Publish` → `trySend`. Same C1 exposure during a viewer disconnect / server delete. The `flood` goroutine is especially exposed — 5000 emits in a tight loop widen the race window.
- **Fix direction:** Wrap each in a closure with `defer recoverPanic(...)`.

### M4. `Scheduler.Run` is not re-entrant — manual + scheduled run can double-execute
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `scheduler.go:219-262` (`Run`); concurrent callers are the loop (`:208-213`) and the HTTP "Run now" handler
- **Why:** No per-task lock. The de-dupe guard in `loop` (`:201`) reads `LastRun` under `RLock` *before* spawning; `LastRun` is only written back inside `Update` at the *end* of `Run` (`:242-253`). Wide window. Operator clicks "Run now" while the scheduler already has the task due (or twice, or two browsers): two `Run` goroutines execute concurrently. For `!restart`/`!stop` that's two concurrent lifecycle calls on the same server; for `say X; !wait 60; !restart` the streams interleave. If `Run`'s final `Update` is skipped (task deleted mid-run), `LastRun` never writes and the next tick re-fires.
- **Fix direction:** Per-task mutex (or CAS on a `running` flag) at the top of `Run`; or have `loop` write a provisional `LastRun` under lock before spawning.

### M5. `Manager.Delete` TOCTOU vs `Start` — can delete a server out from under a live runner
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `manager.go:256-282` (`Delete`, state check `:261`, map mutation `:264-272`); counterpart `manager.go:329-383` (`Start`)
- **Why:** Neither holds `m.mu` across its state check → transition. Concurrent `Delete` + `Start` on the same server both pass their state check (state is `Stopped`/`Failed`), both commit. `Delete` removes `s` and calls `DropRoom`/`RemoveAll`; `Start`'s goroutines keep running against the orphaned `s`, calling `m.hub.Publish(s.ID, …)` (recreating the dropped room), `m.setStatus` → `m.Save`. For sim the runner goroutine is never cancelled (Delete doesn't call `Stop`) → leaks forever. Same shape as M1.
- **Fix direction:** Hold `m.mu` (or a per-server lifecycle mutex) across check+transition in both; have `Delete` call the runner's stop path before removing.

### M6. `Scheduler.Run` ignores the return error of its final `Update` — `LastRun` can silently fail to persist
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `scheduler.go:242-253` (`_, _ = sc.Update(...)`)
- **Why:** `Update` (`:118-132`) re-validates `t.Time` with `parseClock` *after* the callback and returns error on failure — mutation applied in-memory but error returned. Two failure modes: (1) `Time` concurrently edited to garbage between `Get` and `Update` → `parseClock` fails → `Update` errors → `Run` ignores → `LastRun`/`Runs` not updated, one-shots not disabled, next tick re-fires. (2) `os.WriteFile` fails (disk full/permissions) → in-memory `LastRun` set but unpersisted → restart re-fires. Nightly `!restart` becomes a noisy self-repeating bug; one-shot `!backup` can re-run an expensive backup.
- **Fix direction:** Don't ignore the error (log; surface in `LastErr`), or split `Update` so `LastRun`/`Runs`/`Enabled` mutation validates/persists independently of the `Time` re-validation.

### M7. `Restart` polls for 60s then unconditionally calls `Start` — silently no-ops on docker slow-stop
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `manager.go:431-443`
- **Why:** The poll loop waits up to `120 * 500ms = 60s` for `Stopped`/`Failed`, sleeps 400ms, then calls `m.Start(id)` regardless of whether the wait succeeded. `docker stop -t 45` can exceed 60s under load; `Start` then rejects `StatusStopping` with `"still stopping"` (`:337-339`), `Restart` logs and gives up — server is stopped-only, not restarted, with no UI signal. Operator's stated intent silently lost.
- **Fix direction:** Loop until `Start` succeeds or a hard deadline; or have `Stop` complete synchronously (docker path already gets completion via `processExited`).

### M8. `/api/setup` TOCTOU allows a race winner to create a parallel admin during initial onboarding
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `api_ext.go:321-344`; `auth.go:191-205` (`CreateUser`)
- **Why:** `setup` checks `auth.Enabled()` (`:322`) without `a.mu`; `CreateUser` later takes `a.mu` and only checks *name* collision, not whether `enabled` flipped mid-flight, and sets `enabled = true` only after the insert (`:201-203`). Two concurrent setup requests with different usernames both pass the gate, both succeed. On an internet-exposed first-run panel, an attacker who knows the URL can race the operator and get their own admin. Limited to the pre-setup window.
- **Fix direction:** Take `a.mu` for the whole check-then-create, or `sync.Once` keyed on first-user creation.

### M9. HTTP server has `ReadHeaderTimeout` only — slow-body / slow-write DoS
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `internal/arcade/app.go:75-83`
- **Why:** `srv` sets `ReadHeaderTimeout: 10s` but no `ReadTimeout`, `WriteTimeout`, or `IdleTimeout`. `MaxBytesReader` (`:128`) bounds body *size* at 8 MB but not body *rate*. Slowloris-style slow-body sends tie up a goroutine indefinitely. Combined with H8 (unbounded SSE) this is a low-effort DoS.
- **Fix direction:** Set `ReadTimeout` and `IdleTimeout` to tens of seconds; exempt streaming handlers via per-handler `http.TimeoutHandler` or a separate `*http.Server` for `/api/events` and `/ws/console`.

### M10. TOCTOU between `s.State()` check and file write in player-list mutations
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `players.go:169-182` (`AddToList`), `:220-233` (`RemoveFromList`)
- **Why:** Reads `s.State()` to decide between "send the game command" and "edit the file directly". The module comment (`:14-23`) explicitly warns that writing the file under a running server is silent data loss. Between the `StatusRunning` check and the file write inside `applyListAdd`, the runner can flip state — producing exactly the warned-about failure (game overwrites the edit on its next save, change silently vanishes). No lock held across check-then-act.
- **Fix direction:** Take a manager-level lock that the runner's status transition also takes, so check-and-write is atomic w.r.t. start/stop.

### M11. `randHex` ignores `crypto/rand.Read` error — predictable tokens on entropy failure
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `auth.go:174-178`; consumed by `randHex(16)` for salts (`:197`) and `randHex(32)` for session tokens (`:220`)
- **Why:** `_, _ = rand.Read(b)` discards the error; on failure `b` is zero-filled → salt is 32 hex zeros, token is 64 hex zeros. If `crypto/rand` ever fails (entropy starvation, misconfigured container with no `/dev/urandom`, fd exhaustion), every new session gets the same all-zeros token and every new user gets the same salt → identical password hashes → one known password unlocks every account. Low probability, silent failure, catastrophic outcome.
- **Fix direction:** Propagate the error; fail user-create/login rather than emit a deterministic token.

### M12. MCP `SendCommand` runs arbitrary game-console text with no allowlist → prompt-injection-driven privilege escalation
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `internal/arcade/mcp.go:150-159` (`SendCommand`); tool surface `internal/mcp/tools.go:127-135`; read-back `mcp.go:135-148` (`ConsoleTail`)
- **Why:** `arcade_send_command` forwards any non-empty `text` verbatim to the game console; the agent is told to interpret the result via `arcade_console_tail`. An attacker who can influence console output (chat, log line, plugin stdout, MOTD) can embed tool-injection text the agent reads and acts on — `op <attacker>`, `stop`, `ban <player>`, whitelist changes. The token authenticates the *caller*; the *command* is unvalidated and the agent relays whatever an injected prompt says.
- **Fix direction:** Allowlist of safe commands, or a human-confirmation step for privileged verbs (`op`, `deop`, `stop`, `ban`, `whitelist`).

### M13. `DeleteBackup` ignores the backup lock — concurrent delete silently destroys a backup being written
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `backup.go:205-214` (no `backupLocked`/`lockBackup`); compare `CreateBackup` `:99-103`
- **Why:** On Unix, removing a file that `CreateBackup` is still writing unlinks the directory entry while the writer keeps writing to the orphaned inode. `tarGz` finishes and reports a size, `CreateBackup` audits `backup.create` as success, but no archive is left on disk — `ListBackups` never shows it. The operator believes a successful backup exists when it does not.
- **Fix direction:** `DeleteBackup` takes (or checks) `lockBackup(s.ID)` like `CreateBackup`/`RestoreBackup`.

### M14. `untarGz` has no decompression/size bound → disk-exhaustion bomb on restore
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `backup.go:326` (`io.Copy(out, tr)` with no limit); switch `:313-333`
- **Why:** Restore copies each tar entry with unbounded `io.Copy`. `tar.Reader` honors `hdr.Size` but neither `hdr.Size` nor the running total is validated before writing. A crafted archive (or huge world) expands without limit during restore, filling the data dir's volume — taking down the panel and every server sharing it. Zip-slip guard at `:308-311` is correct; there is no zip-bomb guard.
- **Fix direction:** Cap each `hdr.Size` and cumulative bytes written; abort if either exceeds a ceiling.

### M15. MCP token hash compared with `==` instead of constant-time compare
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `mcp.go:73` (`if t.toks[i].Hash == h`); the password path does it right at `auth.go:217` (`subtle.ConstantTimeCompare`)
- **Why:** Token validation compares PBKDF2-derived hex strings with plain `==`, which short-circuits on the first differing byte — timing side-channel. The panel already bothers with constant-time comparison + a decoy hash for user passwords; the bearer-token path is the inconsistency. Exploitable on low-jitter links (LAN/Tailscale).
- **Fix direction:** `subtle.ConstantTimeCompare` on the raw bytes.

### M16. Path-traversal surface in file/backup/download handlers — **Needs verification**
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `api_ext.go:115-207` (`listFiles`/`readFile`/`writeFile`/`deleteFile`/`mkdir`/`download` pass user `path` straight into `m.ListFiles`/`ReadFile`/`WriteFile`/`DeletePath`/`MkDir`/`OpenForDownload`); backup `bid` at `:241-263`
- **Why:** No handler-layer sanitization. Whether traversal is possible depends entirely on the `files.go`/`backup.go`/`manager.go` implementations. H10 confirms the `.tmp` write path bypasses `resolve()`; this finding covers the residual surface on read/delete/download.
- **Fix direction:** Confirm `files.go` does `filepath.Rel` + rejects `..`/absolute, and that `RestoreBackup` validates extraction paths (zip-slip — partly addressed at `backup.go:308-311`). If unguarded anywhere, this is Critical.

### M17. `createServer` does not validate `Port`/`MemoryMB`/`CPU`/`Runtime` ranges — **Needs verification**
- **Status:** FIXED (this pass) — `checkServerLimits` bounds port, memory and CPU at `Manager.Create` **and** `StartImport`, not in the HTTP handler where it only covered one of the two ways to make a server. `TestCheckServerLimits`, `TestCreateRefusesOutOfRangeResources`, `TestImportRefusesOutOfRangeResources`.
- **Where:** `api.go:88-115` (`createServer`); depends on `Manager.Create` (`manager.go`, outside the per-file audit)
- **Why:** Body decodes raw `int`/`float64`/`string` and forwards with no handler-level check. Negative ports, `Port > 65535`, negative memory, `Runtime: "sh"` all flow into `Create`. Negative `Port` may survive into docker binding or template substitution; an unknown `Runtime` may select an unintended `runnerFor(s)` path.
- **Fix direction:** Confirm `Manager.Create` rejects out-of-range; if not, add an allow-list at the handler (`runtime ∈ {sim, docker}`, `1 ≤ port ≤ 65535`, `memory > 0`).

---

# Low

### L1. `parseClock` accepts garbage as midnight
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `scheduler.go:66-80` via `atoi` at `model.go:369-387`
- **Why:** `atoi` returns `0` for any non-numeric input rather than erroring. `"foo:bar"` parses as `(0,0,0)` — a valid midnight. A typo'd task time in `tasks.json` silently fires at midnight.
- **Fix direction:** `atoi` returns `(int, error)` (or digit-only check in `parseClock`).

### L2. `dockerRunner.Start` early-return leaks the context's cancel func
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `runner.go:466-475`; `ctx, cancel := context.WithCancel(...)` then early `return` at `:473` when `containerRunning(s.ID)`
- **Why:** `lostcancel` — `go vet` flags it. Tiny per-call leak against `context.Background()`; not a crash.
- **Fix direction:** `defer cancel()` immediately after creation, or `cancel()` before the early return.

### L3. `Hub.room` materializes empty rooms on stale access — minor unbounded map growth
- **Status:** FIXED (this pass) — `DropRoom` tombstones the room instead of deleting the entry, so a late `Publish` from a leaked runner goroutine cannot rebuild a 500-line ring. Get-or-create stays, because the ring fills before any viewer connects — that is what replay depends on. `TestDeletedServerReleasesItsConsoleRoom`, `TestHubDoesNotMaterialiseRoomsForServersThatAreGone`.
- **Where:** `hub.go:47-62` (used by `Publish`, `PublishRaw`, `Tail`, `Viewers`, `Leave`)
- **Why:** `room(id)` is get-or-create. `DropRoom` deletes, but a stray `Publish(s.ID, …)` from a leaked runner goroutine (H2, M1) re-creates a 500-line ring buffer that lives forever. Multiplier on the High-severity leaks — they become memory leaks, not just CPU.
- **Fix direction:** `room()` returns `(r, ok)` so callers can skip create; or fix the upstream leaks.

### L4. Backup IDs are second-resolution → same-second backup silently overwritten
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `backup.go:133` (`id := fmt.Sprintf("%s-%s", time.Now().Format("20060102-150405"), s.ID)`)
- **Why:** Two backups within the same second for the same server produce identical IDs; the second `os.Create` truncates the first archive and its `.note`. Per-server lock blocks *concurrent* backups but a quick backup → delete → backup can collide.
- **Fix direction:** Append a short random suffix or unix millis.

### L5. TOCTOU window between `resolve` and the subsequent filesystem op
- **Status:** FIXED — the resolve-then-operate model is gone. Every file and plugin operation now runs against an `*os.Root` (`openat2` with `RESOLVE_BENEATH` on Linux), so confinement is enforced by the kernel, one path component at a time, by the same syscall that performs the operation. There is no check-then-use gap left to race. `cleanRel` still refuses `..` and absolute paths, but as *policy* (predictability for an operator), not as the sandbox. Verified by racing a swapping directory symlink against the file API: `TestPathOperationsCannotBeRacedOutOfTheServerDirectory` plants a file outside the server directory within 0.3s under the old model and never under this one. Also `TestPluginInstallCannotEscapeThroughASwappedDirectory`, `TestSymlinksOutOfTheServerDirectoryAreRefused`.
- **Where:** `files.go:66-106` (`resolve` runs `EvalSymlinks`), then ops at `:192`, `:215`, `:247`, `:279`
- **Why:** A symlink can be created in the gap between the `EvalSymlinks` check and the open, letting a follow-symlink op escape the root. Requires an attacker who can win the race and plant a symlink inside the server dir. H10 is the most exploitable instance; this covers the residual race on read/delete/download.
- **Fix direction:** Open with `O_NOFOLLOW` on the final component and `fstat`-check the fd, or re-resolve immediately before each mutating op.

### L6. `reloadProps` calls `m.Save()` unconditionally — atomicity **Needs verification**
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `files.go:332` (`_ = m.Save()`)
- **Why:** Every `server.properties` edit triggers a full manager state save. If `Manager.Save` persists `servers.json` with bare `os.WriteFile` rather than temp+rename, every properties edit risks corrupting the *entire* servers state on crash — same class as C6 but far worse blast radius (all servers, not just tokens).
- **Fix direction:** Verify `Manager.Save` uses temp+rename; if not, apply the C6 fix.

### L7. `OpenForDownload` returns an open `*os.File` — leak risk **Needs verification**
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `files.go:263-284` (returns `f` at `:283`)
- **Why:** If the HTTP download handler fails to `Close` the reader on every exit path (errors mid-stream, client disconnect, write timeout), the fd leaks; repeated downloads exhaust the process FD limit.
- **Fix direction:** Confirm the HTTP layer `defer`s `Close` unconditionally on the returned reader.

### L8. `atoi` silently overflows on long numeric props
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `model.go:369-387`. `n*10 + digit` with no overflow check; returns `0` on any non-digit (`"3.5"`→3, `"-5"`→−5).
- **Why:** A JSON `"99999999999"` produces a wraparound int persisted as the server's port if consumed by `ApplySettings` for `server-port`.
- **Fix direction:** `strconv.Atoi` (overflows loudly) or cap explicitly.

### L9. `itoa` does not handle `math.MinInt`
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `model.go:350-367`. `i = -i` overflows for `MinInt`, producing garbage digits.
- **Why:** Reachable only if a `MinInt`-valued port/max-players ever reaches `itoa`. Unlikely, unverified.
- **Fix direction:** `strconv.Itoa`.

### L10. Default data dir created `0o755`
- **Status:** FIXED — verified against source 2026-08-13.
- **Where:** `cmd/teploy-arcade/main.go:84-87`
- **Why:** World-readable directory. `users.json` itself is `0o600` (`auth.go:116`) so hashes are protected, but anything else written under default umask is world-readable. Mild disclosure on multi-tenant hosts.
- **Fix direction:** `0o700` or `0o750`.

---

# Cross-cutting / systemic themes

These recur across multiple findings and are worth fixing as a class rather than one-by-one:

1. **Inconsistent `recoverPanic`.** The project has an explicit convention ("a panic in any background goroutine killed the process") and a `recoverPanic` helper. It is applied in 8 of ~14 spawned-goroutine sites and missing from 6 — H1, H3, H4, M2, M3. The C1 close-race becomes process-killing *specifically* because several unrecovered goroutines are publish paths. Backfilling the missing recovers is the defense-in-depth that matches the stated design; fixing C1 is necessary regardless.

2. **Non-atomic JSON persistence.** Bare `os.WriteFile` over a live file (truncate-then-write) appears in: `saveUsers` (C3), `Append` (C4), `save` MCP tokens (C6), `writeProps` (H9), and possibly `Manager.Save` (L6). Every one is a crash-away from corrupting the file it owns. A single `atomicWrite(path, bytes)` helper (write `path.tmp` → `os.Rename`) would retire the whole class.

3. **Missing cancellation on lifecycle paths.** The docker `Adopt` path (H2) and the sim `fail()`/`Delete` paths (M1, M5) don't cancel the runner's context, leaving goroutines (and their `pollStats`/ambient loops) running against orphaned or "failed" servers. This is the sync/desync problem the project exists to solve, reproduced inside the panel itself.

4. **Handler-layer trust.** `serverAction` trusts `Actor` (H6); `gameCommand` trusts `reason` (H7); `SendCommand` trusts arbitrary text (M12); GET handlers trust that auth is enforced elsewhere (C2). The pattern is "the lower layer will validate" — but in several cases it doesn't, and the handler is the right place for an explicit check.

5. **Auth enforcement is opt-in per route.** `Auth.attach` only stashes the session; only `auth.require` enforces. Six routes forgot to call it (C2), and the WS role check is frozen at upgrade time (H5). A default-deny mux (auth required unless explicitly waived) would prevent the next forgotten route.

---

## Items still needing verification (consolidated)
- M16 — `files.go` traversal guards on read/delete/download paths.
- M17 — `Manager.Create` input validation.
- L6 — `Manager.Save` atomicity (if non-atomic, escalate to Critical).
- L7 — download handler `Close`s the reader on all paths.

These four depend on `manager.go` (`Save`, `Create`) and the `files.go` op bodies, which were not line-verified in this pass beyond the two confirmed Criticals (C2, C1) and the four confirmed in source (C3, C4). Recommend a focused read of `Manager.Save` and the `files.go` mutating ops before triage.

---

## Suggested fix order (if/when fixing)
1. C2 (auth on GET routes) + C3 (atomic users.json) + H8 (SSE auth/cap) — closes the anonymous-access hole and the takeover-on-corruption hole together.
2. C1 (trySend race) — backfill the missing recovers (H1/H3/H4/M2/M3) at the same time; they're one-line each and shield every publish path.
3. C5/C6 (map race, MCP token atomicity) + H9 (writeProps atomicity) — one `atomicWrite` helper retires most.
4. H7 (newline injection) + H6 (Actor forgery) + H10 (`.tmp` symlink) — small, high-value security fixes.
5. H2/M1/M5 (cancellation leaks) — the sync/desync class.
6. Everything else.
