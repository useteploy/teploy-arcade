# Teploy Arcade — a desktop-grade game server panel, web-native

Concept: the best game-server management UX is still a Windows desktop
application, and the web panels that exist trade that polish for reach. Build a
modern **web-based** panel that does not make the trade: desktop-grade
interaction density, delivered as a single binary. Reference Pterodactyl and
Crafty Controller (both OSS) where useful. Position as the
Ghost-that-also-packs-containers.

> This is the **master plan** — concept, architecture, and build order in one
> place. Framework bugs/gaps found while building are tracked in
> `NEUTRON-ISSUES.md`. Fast-moving build status, when it diverges from this plan,
> wins.
>
> **Stack decision (locked 2026-08-12):** dogfood **Neutron on both sides** —
> **Neutron-TS** for the panel, **Neutron-Go** for the agent, **Nucleus** for
> storage. Teploy is the install path. Rationale + gating checks in §3 and §11.

---

## 1. Why this exists

The current landscape has a clear gap:

| Panel | Stack | UX | Packing model | Weakness |
|---|---|---|---|---|
| **Pterodactyl** | PHP + Go (Wings) | Criticized, dated | Multi-container per node + cgroup quotas | Heavy, hosting-provider-shaped, UI lags |
| **Crafty Controller 4** | Python | Decent web UI | Single-host, simpler Docker | MC-focused, smaller ecosystem |
| **Desktop panels** | Windows desktop | **Best GUI/UX** | Process-level, no real isolation | Windows-only, desktop-only |
| **Ghost (Hayden Bleasel)** | Next.js + Bun agent | Beautiful, modern | One VM per game (no packing) | Expensive at scale, no quotas |

Nobody currently owns: **desktop-grade UX + multi-container packing + modern
stack + web-deployable.** That's the wedge.

## 2. Core idea

- Web panel with desktop-grade UX — an operations-dense layout designed for
  people who watch a console all day.
- **Agent in Neutron-Go** (not "plain Go" — see §3), packs multiple Docker
  containers per host with real cgroup CPU/disk limits.
- One-`teploy-up` deploy, BYO host.
- NOT trying to beat Ptero on game coverage — ship with a focused set (Minecraft
  Java + Bedrock, Rust, maybe Valheim/CS2).

---

## 3. Stack (locked)

| Layer | Choice | Notes |
|---|---|---|
| Panel UI | **Neutron-TS** | Dashboard patterns + realtime console view (Neutron-TS has its own `REALTIME_PLAN.md`). Dogfood the TS side. |
| Agent | **Neutron-Go** | `neutron` (HTTP control API) + `neutronrealtime` (WS console hub) + `neutronauth` (apikey/RBAC) + `neutronjobs` (backup/restart scheduling). Docker/cgroups/SFTP are domain code on top. |
| Storage (v1 default) | **Postgres** via `nucleus-go` (pgwire) | Relational metadata (users/servers/templates/audit). Zero ceremony, rock-solid, proven in Phase 0.1. `nucleus-go` auto-detects it. |
| Storage (dogfood/upgrade) | **Nucleus** via `nucleus-go` | Same client/DSN. Adds `nucleus/ts` (metrics) + `nucleus/kv` (live config) — both gated off on plain Postgres. Pre-production + known pgwire rough edges (NI-004/005/007); promote to default once those close + it soaks. |
| Realtime | WS console (browser → **agent direct**) + SSE activity feed | Panel is thin; does not proxy the console stream (see §6). |
| Install / distribution | **Teploy** | Provisions the box, starts Nucleus + agent, fronts the panel with Caddy. |

### Why Neutron on the agent too (revised from the earlier "plain Go" instinct)

Initial instinct was to keep the agent "close to Teploy's metal, framework helps
little." After reading `Neutron/go/`, that was wrong (see `NEUTRON-ISSUES.md`
NI-001 — the README's "planned" status is stale; there are 112 real Go files with
tests). An agent's *plumbing* is HTTP routes + WS console hub + SSE + state
persistence + auth + scheduled jobs — and Neutron-Go ships **all** of those,
implemented and `net/http`-compatible. The only bespoke pieces (Docker lifecycle,
cgroup limits, SFTP) are domain logic no framework covers anyway. Neutron-Go is
agnostic (stdlib net/http, bring-your-own WS lib, modular data-model imports), so
it won't fight the bespoke parts.

### Teploy is the *installer*, the agent is the *runtime* — distinct roles

Important distinction this plan hinges on:

- **Teploy** = the outside-in provisioner (SSH, file-state, no DB, Caddy,
  template registry, accessories). It gets a box ready and installs things. It
  is *not* the long-running manager.
- **Game panel agent** = a long-running on-host **Neutron-Go** process that
  manages game containers locally (start/stop, console, quotas, backups).

So "Teploy for game servers" = Teploy provisions the box and installs the agent +
panel (one `teploy up`), then hands off to the agent at runtime. The agent never
becomes part of Teploy; it's its own binary.

---

## 4. Architecture

```
                      ┌───────────────────────────┐
                      │  Panel (Neutron-TS, web)  │
                      │   auth, UI, CRUD, graphs  │
                      └───────────┬───────────────┘
                          HTTPS   │   (CRUD/metadata + SSE activity)
                      (control)   ▼
   Browser  ◄─────────────────────────────────────►  Panel  ◄──── Nucleus (pgwire) ────┐
       │   signed token (Ed25519, Ghost-style)                                            │ metadata +
       │   direct WS for console                                                          │ metrics +
       └──────────────────────►  Agent (Neutron-Go)  ◄──────────────────────────────────►  timeseries
                                   │
                                   ├─ HTTP control API (neutron router)
                                   ├─ Console hub: 1 room per game server (neutronrealtime)
                                   ├─ Docker lifecycle, cgroup quotas  (domain code)
                                   ├─ SFTP/file API, backup/restore     (domain code + neutronjobs)
                                   └─ apikey auth (neutronauth) — panel↔agent trust
```

Three planes:

- **Control plane:** Browser → Panel → Agent over HTTPS (CRUD, server config,
  start/stop). Panel persists metadata to Nucleus and forwards commands to the
  agent.
- **Data plane (console):** Browser → **Agent directly** over WS, using a
  short-lived signed token minted by the panel (Ed25519, the Ghost protocol
  shape). The panel never proxies console bytes — that keeps the panel thin and
  avoids double-hop latency / backpressure.
- **Activity plane:** Panel → Browser over SSE (deploys, starts/stops, alerts).

## 5. Data model (Nucleus / Postgres)

Minimal v1:

- `users` — id, email, password_hash, role. RBAC via `neutronauth`.
- `api_keys` — id, user_id, key_hash, scopes. Panel↔agent trust + browser→agent
  console tokens are derived here.
- `game_servers` — id, owner_id, template_id, name, container_id, status,
  cpu_limit, ram_limit, disk_limit, bind_port, created_at.
- `templates` — id, slug, name, game, image, default_env, startup_cmd, default
  resource limits. Our "egg" equivalent (see §8).
- `audit_log` — actor_id, action, target, ts, payload. Append-only.
- Timeseries (`nucleus/ts`): per-server CPU/mem/disk samples, retention-bounded.
  **Nucleus-only** — until Nucleus is the default, use a plain Postgres metrics
  table (see §11).
- KV (`nucleus/kv`): live per-server runtime flags + console ring-buffer
  pointers (see §6). **Nucleus-only** — Postgres default uses a small
  `server_runtime` table or JSONB instead.

## 6. Realtime — console streaming (the hardest part)

Per game server, one `neutronrealtime` room. Flow:

1. Browser requests console → panel returns a signed token bound to
   `server_id` + expiry.
2. Browser opens `wss://agent/api/servers/:id/console?token=…`.
3. Agent validates token (Ed25519), subscribes the socket to room `server:<id>`.
4. Agent runs a goroutine streaming the container's stdout/stderr
   (`docker logs --follow` / attach) → `hub.Broadcast("server:<id>", line)`.
5. Inbound from browser = **commands** → routed to container stdin / `docker
   exec` (NOT broadcast to other viewers — this is `NEUTRON-ISSUES.md` NI-002).

Backpressure & reconnect:

- The hub's `trySend` already drops on full buffer (correct for console — never
  block the broadcaster). NI-002 stress-test must confirm this holds under MC/Rust
  console flood before it's load-bearing.
- On (re)connect, the agent replays the last N lines from an in-memory ring
  buffer keyed in `nucleus/kv`, so a refresh doesn't blank the console.

## 7. Game config templates ("eggs")

Ptero's egg library is a network-effect moat we can't clone (license +
community). v1 ships a **focused, our-own-format** library (see §12 open
question: adopt-Ptero-egg-format vs. clean-slate — leaning clean-slate, but
defer the call until after the reference MC Java template is built so we know
the shape we need). Each template declares: image, startup command, env schema,
default + overrideable resource limits, exposed ports, a healthcheck.

Bootstrap set (~5–10, Phase 6): Minecraft Java, Minecraft Bedrock, Rust,
Valheim, CS2.

## 8. The hard parts (agent edge cases) — and where each lands

These only surface in production; reading Wings source saves discovery, not
operational pain. Mapping to where they're handled in this architecture:

- **Container OOM / restart loops** — agent supervisor (neutronjobs retry +
  exponential backoff), with a max-restart circuit breaker that flips
  `game_servers.status` to `failed` and fires an alert.
- **Broken image pulls** — pre-flight pull in the control API; surface a typed
  error to the panel instead of a hung start.
- **SFTP file locking during backup** — backup job (neutronjobs cron) takes a
  container quiesce/pause, snapshots the volume, then resumes. File API refuses
  writes during the snapshot window.
- **Console backpressure on disconnect** — covered by hub drop-on-full + ring
  buffer (§6); verify in NI-002 stress test.
- **Resource-limit race conditions** — start/stop/cgroup-update serialized
  through a per-server command queue; no concurrent mutations.

## 9. Distribution — Teploy integration

- One `teploy.yml` declares: panel service + agent service + Nucleus accessory.
  `teploy up` provisions the box (Teploy harden, network, Tailscale), starts
  Nucleus, starts the agent (connected to Nucleus over local pgwire), starts the
  panel, fronts it all with Caddy with TLS.
- Game templates are installable via Teploy's existing `template`/`registry`
  commands — same Umbrel/CasaOS-style flow, but the installed "app" is a game
  server definition the agent then manages.
- BYO host = any box Teploy can SSH to.

## 10. Build phases

Each phase has a definition-of-done. Don't start the next until the prior's DoD
holds. Tests are non-negotiable per phase (Neutron-Go convention: `Querier` +
transaction-rollback tests, no mocks for DB code).

### Phase 0 — De-risk spikes  *(gate; ~3–5 days)*
- **0.1** Confirm Nucleus packaging: agent ↔ Nucleus over local pgwire, Nucleus
  as a Teploy accessory. Verify `nucleus-go` also talks to stock Postgres (the
  fallback path actually works). *DoD: agent connects to both, runs a migration,
  reads/writes a row.* **✅ DONE 2026-08-12 — see below.**

  **Result — DoD met on BOTH targets** (spike at `spike/phase0/`, same code, two
  DSNs): connect + auto-detect + ping + migrate + insert + select + list all
  green on Postgres 16.14 *and* Nucleus 0.1.1. nucleus-go is real and the
  pgwire dual-target design works. But the spike **inverted the storage default**
  (see §3, §11): Postgres is the safer v1 production default (zero ceremony,
  proven); Nucleus is the dogfood/upgrade target (works, gives us `kv`/`ts`, but
  pre-production + known pgwire rough edges). Both switchable by DSN — no lock-in.
  Bugs/friction found (all in `NEUTRON-ISSUES.md`): **NI-004** `time.Time` scan
  broken on both targets (worked around w/ string); **NI-005** Nucleus standalone
  needs 3 env vars; **NI-006** neutron-go isn't `go get`-able + `replace` breaks
  on space paths (symlink workaround); **NI-007** query path unconditionally
  uses Nucleus's degraded simple-protocol/string-scan even on Postgres.
- **0.2** Stress-test `neutronrealtime` hub under a fake MC console flood
  (`NEUTRON-ISSUES.md` NI-002). *DoD: measured backpressure, no goroutine leaks,
  drop rate acceptable.* **✅ DONE 2026-08-12 — see below.**

  **Result — Hub is production-sound for the console path** (spike at
  `spike/phase0-2/`, drives the Hub directly, no WS transport). Ceiling
  **1.1M broadcasts/sec** (0.18% drop at saturation — backpressure shedding
  working). At the realistic operating point — 5 kHz console flood (heavy MC
  startup) — **0% drops with 10 *or* 100 concurrent viewers.** Slow-consumer
  isolation holds: one stalled viewer never blocks the broadcaster (0.9µs/broadcast),
  fast viewers still get 100%, stalled viewer capped exactly at buffer, no panic.
  No goroutine leak (200 conns → all drain goroutines exit on `Unregister`).
  This clears the §8 "console backpressure on disconnect" risk. One finding:
  **NI-008** — the Hub sheds silently with no per-Conn drop counter, so the UI
  can't show "N lines dropped" without app-level tracking (the notice is in
  `VISUAL-BRIEF.md` §3); small upstream fix.
- **0.3** Pick WS lib (nhooyr vs gorilla), write the Upgrader, resolve inbound
  routing (custom handler or framework fix per NI-002/NI-003).
  *DoD: browser ↔ agent echo demo with command-routing shape.*
  **✅ DONE 2026-08-12 — see below. Phase 0 gate now COMPLETE.**

  **Result.** WS lib: **`github.com/coder/websocket`** (the maintained nhooyr
  successor — ctx-native, fits Neutron-Go's context-first style). Upgrader
  written (`coderConn` adapter to `neutronrealtime.WebSocketConn`, ~15 lines —
  confirms NI-003's fix is trivial). **Command-routing handler built** at
  `spike/phase0-3/main.go::consoleHandler` — this is the NI-002 reference impl:
  inbound → `onCommand` callback (NOT broadcast), outbound = normal room fanout.
  Automated two-client test (`routing_test.go`) **passes**: client A sends a
  command, client B never sees the raw command (routing works), both receive the
  `executed:` broadcast, fanout confirmed. Browser demo (`web/index.html`)
  served at `/`, WS at `/ws/console`. WS upgrade works cleanly through Neutron's
  router. One finding: **NI-009** — `errInterceptor` double-writes headers when
  a mounted handler writes its own status (harmless log warning on the 426 path,
  but the interceptor's write-guard is incomplete).

  ---
  **Phase 0 complete.** Storage de-risked (0.1, both targets), realtime
  backpressure proven (0.2), WS transport + command routing proven (0.3). The
  Phase 0 gate is cleared. Findings logged: NI-004…NI-009. Ready for Phase 1
  (agent skeleton + HTTP control API + Docker lifecycle). Phase 1 is the natural
  sync point for Claude's visual data-needs list (`VISUAL-BRIEF.md` §5) — it
  shapes the API endpoints — but is **not blocked** on it; build proceeds from
  §5's data model and adjusts when the list lands.

  **The list has landed (2026-08-12): `VISUAL-HANDBACK.md` §4**, with the five
  mockups in `mockups/`. Read it before freezing the Phase 1 HTTP surface and
  the Phase 2 WS message shapes — it adds required fields that are cheap now and
  expensive later (per-line `seq`, nullable `players`, CPU percent + its
  denominator, `last_exit`, `command_ack`), and it pushes display metadata
  (group, help text, restart-required) into the template schema, which bears on
  §12 open question 2.

### Phase 1 — Agent skeleton  *(Neutron-Go app)*
- Schema + migrations for §5 tables.
- HTTP control API: create/list/start/stop/restart/delete a server.
- Docker lifecycle for **one** game (Minecraft Java, the reference template).
- *DoD: `curl` the agent, start an MC server, it accepts connections.*
  **DoD MET FOR REAL 2026-08-12** — a live Paper 1.20.4 container booted through
  the panel, published :25599, generated all three dimensions, took console
  commands and stopped gracefully. Three bugs only a live run surfaced, all
  fixed with tests: (1) the bind-mount path was passed unresolved, which Docker
  rejects on macOS where `/tmp` and `/var` are symlinks into `/private` — fatal
  for the default data dir on a Mac; (2) `seedServerFiles` wrote a placeholder
  `world/level.dat` that made Paper exit with "World files may be corrupted";
  (3) the JVM heap was set equal to the container memory limit, so the kernel
  killed the server during chunk generation — heap now reserves max(512 MB, 25%).
  **~DONE 2026-08-12 — `agent/`, see `agent/README.md`.** Control API is up
  (`/api/servers`, start/stop/restart/kill/command, settings GET+PATCH, SSE
  events). Docker runner is real: `docker run -i` with our own stdin pipe,
  `--memory`/`--cpus` as hard limits, a named volume per server. **Two
  deviations, both deliberate:** storage is a JSON file rather than Postgres via
  `nucleus-go` (zero-setup so the thing runs; NI-006 makes the swap a chore
  today — it's a `Store` implementation away), and a **simulator runtime** was
  added alongside docker so the panel is usable without pulling a 500 MB image.
  Every server records which runtime it uses and the UI shows it.

### Phase 2 — Console streaming  *(the realtime core)*
- `GET /api/servers/:id/console` WS endpoint, signed-token auth, one room/server.
- Container stdout/stderr → room fanout; inbound → container stdin/exec.
- Ring-buffer replay on connect; reconnect survives a page refresh.
- *DoD: two browser tabs see the same console live; typing in one runs a command;
  refresh restores history.*
  **~DONE 2026-08-12.** `GET /ws/console?server=<id>`, one room per server,
  500-line ring buffer replayed on connect. DoD met and covered by tests in
  `agent/console_test.go`. Beyond the DoD: every line carries a monotonic per-room
  `seq`; each socket counts its own drops and the UI marks the gap inline where
  output was lost; commands are acked so a dead socket can't swallow one
  silently. **Deviation: no signed-token auth** — panel and agent are one
  process here, so the browser talks to a single origin. The Ed25519
  browser→agent-direct flow in §6 is still the multi-process design.

### Phase 3 — Panel UI  *(Neutron-TS)*
- Login (session via `neutronauth`), server list, create-from-template, start/stop.
- Live console view (WS direct to agent).
- Establish the core interaction patterns (layouts, the console as the hero).
- *DoD: end-to-end — log in, create an MC server, start it, watch the console.*
  **~DONE 2026-08-12, minus login.** Server list, create-from-template dialog
  with a live host-budget check, start/stop/restart/kill, live console, working
  settings with restart-required flags. The interaction patterns: icon rail,
  server tab strip, per-software marks,
  Stop/Kill/Restart, section tabs, console footer checkboxes, sparkline cards.
  **Two deviations:** no login (`neutronauth` not wired; binds to loopback), and
  the panel is **vanilla JS served by the agent**, not Neutron-TS. The views map
  1:1 onto the components named in the mockups, so the port is mechanical — but
  it is a port, and it is still owed.

### Phase 4 — Packing & quotas  *(the moat)*
- cgroup CPU + RAM limits per container; disk quota per volume.
- Multiple containers on one host; resource-isolation correctness.
- Resource graphs via `nucleus/ts` (CPU/mem/disk, retention-bounded).
- *DoD: run 3 servers, cap each, show live graphs, verify one can't OOM the host.*
  **~PARTIAL 2026-08-12.** Live graphs are real: sampled every 5s into a bounded
  ring, per-server CPU/memory/players over 5m/15m/1h, plus a host aggregate.
  A guard loop warns in the console when a server sits above 92% of its memory
  ceiling, *before* the kernel says it with a SIGKILL. Limits are enforced by
  docker's own `--memory`/`--cpus`, which is real cgroup enforcement.
  **Not done: disk quota per volume** (needs XFS project quotas or a loop
  device; docker has no portable equivalent), and the "verify one can't OOM the
  host" chaos test is asserted by construction rather than by an actual run.
  Metrics are in-memory only - `nucleus/ts` would take this over.

### Phase 5 — File manager + backups
- File browser over the agent (SFTP-backed or a web file API).
- Backup/restore: scheduled (neutronjobs cron) + on-demand, with the quiesce
  window from §8.
- *DoD: browse/edit `server.properties`, take a backup, restore it, server boots.*
  **✅ DONE 2026-08-12.** File browser with a text editor, download, delete,
  mkdir - every path resolved against the server's own directory and re-checked
  after symlink evaluation, with traversal *refused* rather than clamped
  (`TestFilePathsCannotEscapeServerDir`). Editing `server.properties` by hand
  reloads the panel's model, so the file and the settings screen cannot drift.
  Backups are tar.gz with the §8 quiesce window: `save-off` + `save-all flush`,
  archive, `save-on`, with file writes blocked for the duration and saves
  resumed even if the archive fails. Restore keeps the old tree until the new
  one lands. **Not done: scheduled backups** - that is the `neutronjobs` cron
  half, and the Scheduler tab is still marked v1.1.

### Phase 6 — Template library
- Lock the egg format (resolve §12 open question).
- Build ~5–10 templates (MC Java/Bedrock, Rust, Valheim, CS2).
- Template registry via Teploy `template`/`registry`.
- *DoD: install a new game from the registry, no code changes required.*
  **~DONE 2026-08-12, minus the registry.** Templates are now data:
  `data/templates/*.json`, seeded from the built-ins on first run, validated on
  load, with a malformed file skipped and reported rather than fatal. Dropping
  a file in and restarting adds a game with no code change - that half of the
  DoD is covered by `TestTemplatesLoadFromDiskAndValidate`. 11 templates ship.
  **This resolves §12 open question 2: clean-slate, not Ptero's egg format.**
  The reason is concrete now that the UI exists - what the panel needs from a
  template is *display* metadata (group, description, recommended, maturity,
  per-field help) that Ptero's format simply does not carry, and adopting it
  would import an installer-script model plus licence questions for no gain.
  **Not done: a remote registry** to install from; templates are local files.

### Phase 7 — Teploy distribution
- Author the `teploy.yml` (panel + agent + Nucleus accessory + Caddy).
- End-to-end one-command install on a clean box.
- *DoD: `teploy up` on a fresh VPS → working panel + agent, TLS, no manual steps.*
  **~PARTIAL 2026-08-12.** `agent/Dockerfile` (multi-stage, static binary,
  alpine + docker-cli) and `teploy.yml` are written, with the Caddy settings the
  console actually needs - websockets on, buffering off, a 3600s read timeout,
  or the console drops every 60s. Game port ranges are opened up front so a new
  server needs no redeploy. **Not verified: it has never been run on a real
  VPS.** Treat the file as a reviewed draft, not a proven deploy. The Nucleus
  accessory is commented out because storage is still a JSON file.

### Phase 8 — Harden
- Drive through every edge case in §8 with adversarial tests.
- Audit log surface in the panel.
- Multi-user RBAC (admin / owner / viewer).
- *DoD: chaos scripts (kill -9 the container, yank the network, fill the disk)
  all recover or fail clearly.*
  **~PARTIAL 2026-08-12.** Auth, RBAC and the audit log are done: PBKDF2-SHA256
  (120k iterations, per-user salt, constant-time compare, stdlib only),
  httpOnly session cookies, three roles enforced on every mutating route *and*
  on the console socket, first-admin setup flow, and an append-only audit log
  surfaced in the panel. The last admin cannot be deleted. Auth stays off until
  a user exists, so a loopback run needs no ceremony, and the startup banner
  shouts if the agent is bound to a non-loopback address with no users.
  **Not done: the chaos scripts.** Edge cases from §8 that are handled -
  restart loops (restart counter + failed state), broken image pulls (surfaced
  as a console warning), backup file locking, console backpressure. Not handled:
  a max-restart circuit breaker, disk-full behaviour, and network partition.

## 11. Gating open questions (resolve early, not at the end)

1. **Nucleus packaging for the agent** (Phase 0.1). Nucleus is real (294k LOC
   Rust, pgwire, MVCC, single-node working, pre-production), and `nucleus-go`
   speaks pgwire so Postgres is a clean fallback — but confirm the actual
   packaging path (Teploy accessory vs co-shipped binary) and that single-node
   Nucleus holds for the panel's metadata load. **This is the one true gate.**
   **✅ RESOLVED 2026-08-12 (Phase 0.1).** Packaging path = Nucleus container as
   a Teploy accessory, agent connects over local pgwire (proven). Both targets
   round-trip. Evidence **inverted the default**: **Postgres is the v1
   production default** (zero friction, proven), **Nucleus is the dogfood/
   upgrade target** (works but pre-production; needs NI-004/005/007 closed +
   soak before it's the default). This is a strategic call — the whole bet
   is Nucleus replacing Postgres — so the *desire* to default to Nucleus is
   legitimate, but the *evidence* says ship Postgres first and promote Nucleus
   as the bugs close. Revisit the default at each phase gate. KV/ts metrics
   stores are Nucleus-only; until Nucleus is default, implement metrics/config
   against plain Postgres (JSONB + a metrics table) so we don't build a
   Nucleus-only feature we then can't run on the v1 default.
2. **Egg format: adopt Ptero's vs. clean-slate** (Phase 6). Ptero compatibility
   would enable migration but imports legacy complexity and license constraints.
   Decide after the reference MC template reveals the shape we actually need.
3. **Monetization model.** Game-server users expect free (Ptero is free).
   Options: hosted SaaS, paid pro tier, Teploy-bundled. Deferred
   past v1; don't let it shape architecture now.

## 12. Out of scope for v1 (deliberately deferred)

- **Multi-node** (Pterodactyl shape: one panel → many agents on many boxes).
  v1 is single-host (one machine, many containers) — covers ~90% of self-hosters
  and is where the wedge against single-host panels is. Multi-node is where Ptero-level
  complexity lives; revisit as v2.
- **Hosting-provider features** — node registration, distributed scheduling,
  cross-node file management, per-tenant billing. All multi-node concerns.
- **Game coverage breadth** — the 5–10 in Phase 6 are the v1 set; community
  templates come after the format is public.

## 13. OSS reference (read, don't lift wholesale)

- **Pterodactyl Wings** (`github.com/pterodactyl/wings`) — agent architecture,
  console streaming over websocket, cgroup limits, SFTP file management,
  container lifecycle. Verify license before any reuse beyond reading.
- **Pterodactyl Panel** (`github.com/pterodactyl/panel`) — DB schema shape
  (servers/users/nodes/eggs). Most is hosting-provider complexity we don't need.
- **Crafty Controller 4** (`gitlab.com/crafty-controller/crafty-4`) — single-host
  patterns and MC-specific UX flows. Check license.
- **Ghost** (`github.com/haydenbleasel/ghost`) — Ed25519 signed long-poll agent
  protocol; we borrow the token shape for browser→agent console auth.

## 14. License

Panel license TBD (open question). Until decided, treat as private.
