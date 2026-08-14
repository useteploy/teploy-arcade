# Neutron Issues — found while building teploy-arcade

This file tracks **bugs, gaps, and improvement ideas for the Neutron framework
itself** (both the TS and Go sides, and Nucleus), discovered while building this
product. These are **upstream contribution candidates**, not this project's own
bugs. This project's own TODOs live in the plan (`PLAN.md`) and normal commit
messages.

Because teploy-arcade dogfoods Neutron hard (TS panel + Go agent + Nucleus
storage + realtime console streaming + auth + jobs), it is a deliberate stress
test. Every sharp edge we hit goes here so the framework gets better from real
use, not from guessing.

## How to add an entry

1. Append at the bottom, next free ID. Don't renumber.
2. One entry per discrete issue. If you find three things in one session, write
   three entries.
3. Keep "Observed" strictly factual with `file:line` evidence — no speculation.
   Put interpretation in "Impact" and "Expected".
4. Severity guide:
   - **blocker** — can't ship this product without it; must fix or fork.
   - **major** — works around exists but is ugly, slow, or unsafe in production.
   - **minor** — papercut; nice to fix, low urgency.
   - **doc** — code is fine, docs lie or are missing (still costly — cost me a
     wrong architecture call on this very project).
5. When an issue is fixed upstream, mark `Status: fixed` with the commit/PR and
   the date. Don't delete the entry — it's a useful changelog of dogfood wins.

---

## NI-001 — go/README.md status line is stale (doc rot → wrong conclusions)

- **Date:** 2026-08-12
- **Component:** `Neutron/go/README.md`, `Neutron/go/ARCHITECTURE.md`
- **Severity:** doc
- **Observed:** `go/README.md:274` says `Status: Planned — not yet implemented.`
  while the tree actually contains 112 `.go` files with tests across `neutron/`,
  `nucleus/`, `neutronrealtime/`, `neutronauth/`, `neutronjobs/`, `neutroncache/`,
  `neutroncli/`, `neutronmcp/`. `ARCHITECTURE.md` is framed as a forward-looking
  "Architecture Plan" for code that already exists.
- **Expected:** README status reflects reality (implemented modules, test counts,
  what's stubbed vs. production-ready). A stale "planned" status on a built SDK
  is worse than no status — it actively discourages dogfooding.
- **Impact:** Led to an initial (wrong) recommendation to avoid Neutron-Go for
  the agent and use "plain Go". Wasted a decision round. Anyone evaluating
  Neutron-Go from the README alone would walk away.
- **Status:** open
- **Links:** —

## NI-002 — `WebSocketHandler` default broadcasts client messages to everyone

- **Date:** 2026-08-12
- **Component:** `Neutron/go/neutronrealtime/websocket.go`
- **Severity:** major
- **Observed:** `WebSocketHandler` (`websocket.go:81`) and
  `WebSocketHandlerWithRoom` (`websocket.go:141`) both broadcast every message
  received from a client to all connections (or the whole room) via
  `BroadcastAll` / `Broadcast(room, msg)`. There is no handler variant that
  routes inbound client messages to a callback instead of echoing them to peers.
- **Expected:** For most non-chat use cases (agent control, console command
  input, signalling), inbound client messages should go to application code, not
  to other subscribers. A console panel sends a *command* that the agent must
  execute — it must NOT be echoed to every other viewer's socket. Current API
  forces every consumer to either reimplement the handler or build a workaround.
- **Impact:** The game panel's console input path cannot use the stock handler.
  We need inbound → agent-exec, outbound → room-fanout. Will likely have to write
  a custom handler on top of `Hub`, which is fine but means the framework's
  flagship realtime handler doesn't fit the flagship realtime use case.
- **Suggested fix:** Add `WebSocketHandlerFunc(hub, room, upgrader, onMessage func([]byte))`
  (inbound → callback; no auto-fanout), and keep the broadcast variants for
  chat-room use. Or split: transport (upgrade + register + lifecycle) is generic;
  inbound routing policy should always be caller-supplied.
- **Status:** open
- **Links:** `PLAN.md` § Realtime architecture. **Reference implementation now
  exists** at `spike/phase0-3/main.go::consoleHandler` (Phase 0.3) — this is the
  exact shape the upstream fix should take; an automated two-client test
  (`routing_test.go`) proves the routing property.

## NI-003 — `Upgrader` is bring-your-own with no default

- **Date:** 2026-08-12
- **Component:** `Neutron/go/neutronrealtime/websocket.go`
- **Severity:** minor
- **Observed:** `Upgrader` is a function type the caller must implement against
  `nhooyr.io/websocket`, `gorilla/websocket`, etc. No default is shipped.
- **Expected:** Agnosticism is good (don't pin a WS lib), but a ready-made
  `nhooyr.io/websocket` upgrader in a `neutronrealtime/ws/nhooyr` subpackage —
  opt-in, separate import — would save every user 30 lines of boilerplate and
  guarantee the origin-check + lifecycle is correct on day one.
- **Impact:** Minor friction at agent bootstrap. We'll write it once; offering it
  upstream removes the work for every future consumer.
- **Status:** open
- **Links:** depends on resolution of NI-002 (the default upgrader should pair
  with the inbound-callback handler shape)

## NI-004 — `parseTimeValue` cannot scan TIMESTAMPTZ as `time.Time` (both targets)

- **Date:** 2026-08-12
- **Component:** `Neutron/go/nucleus/sql.go` (`parseTimeValue`, lines 231–248)
- **Severity:** blocker
- **Observed:** Any struct field of type `time.Time` fails to scan. Phase 0.1
  spike INSERT...RETURNING a `created_at TIMESTAMPTZ` column failed:
  `parse time "2026-08-13 02:56:38.833736+00": unrecognized time format`.
  `parseTimeValue` handles only epoch-millis, RFC3339, and RFC3339Nano. But
  **both** Postgres and Nucleus return timestamps in Postgres's native text
  format — `2026-08-13 03:00:28.171023+00` (space separator, `+00` short
  offset) — which none of the three parsers match. Confirmed on Postgres 16.14
  and Nucleus 0.1.1.
- **Expected:** Scanning a `TIMESTAMPTZ`/`TIMESTAMP` column into a `time.Time`
  field works out of the box on the documented primary target (Postgres) and on
  Nucleus. Every schema has `created_at`/`updated_at`; this breaks essentially
  every typed model.
- **Impact:** Forced the spike to scan `created_at` as `string` to prove the
  data path. Any real agent model with `time.Time` fields is blocked until fixed.
- **Suggested fix:** Add Postgres formats to `parseTimeValue`
  (`2006-01-02 15:04:05.999999-07` + `2006-01-02 15:04:05.999999Z07:00` +
  short-offset variants). Better long-term: when `Features.IsNucleus == false`,
  bypass the string intermediary entirely and let pgx decode into typed fields
  natively (see also NI-007).
- **Status:** open
- **Links:** `PLAN.md` §3, Phase 0.1 spike
  (`teploy-arcade/spike/phase0/main.go`)

## NI-005 — Nucleus standalone startup demands cluster + replication auth ceremony

- **Date:** 2026-08-12
- **Component:** Nucleus server (`ghcr.io/neutron-build/nucleus:latest`)
- **Severity:** major
- **Observed:** A single-node container refuses to start with only
  `NUCLEUS_PASSWORD` set. Two sequential refusals:
  1. `Refusing to start with non-loopback cluster transport and no
     NUCLEUS_CLUSTER_TOKEN. Set … or NUCLEUS_ALLOW_INSECURE_CLUSTER=1 for
     development.`
  2. After (1): same for `NUCLEUS_REPLICATION_TOKEN` /
     `NUCLEUS_ALLOW_INSECURE_REPLICATION=1`.
  Working standalone run requires **three** env vars: `NUCLEUS_PASSWORD` +
  `NUCLEUS_ALLOW_INSECURE_CLUSTER=1` + `NUCLEUS_ALLOW_INSECURE_REPLICATION=1`.
  Also: default pgwire user/db is `nucleus` (not `postgres`) — undocumented in
  the deploy README, discovered only via repo grep.
- **Expected:** Single-node is the documented primary release target
  (`Neutron/nucleus/DATABASE_COMPLETION.md`: "single-node first … must not block
  an honest single-node release"). A single-node dev/run should start with just
  a password (or a single `NUCLEUS_STANDALONE=1` shortcut that implies the
  loopback insecure defaults). Replication/cluster auth ceremony belongs to the
  distributed gate, not the standalone path.
- **Impact:** Teploy accessory definition for Nucleus must carry the two
  insecure flags + correct user/db — friction for the "just works one
  `teploy up`" UX this product depends on. Also every new dev hit this.
- **Status:** open
- **Links:** `PLAN.md` §9 (Teploy distribution), §11 gating 1

## NI-006 — Neutron-Go module is not `go get`-able; local `replace` breaks on space-containing paths

- **Date:** 2026-08-12
- **Component:** `Neutron/go/go.mod`, `Neutron/go/README.md`
- **Severity:** major
- **Observed:** `go.mod` declares `module github.com/neutron-dev/neutron-go`,
  but the README documents `github.com/neutron-build/nucleus-go` /
  `neutron-go`. The declared path `github.com/neutron-dev/neutron-go` is not
  published (no such resolvable module), so `go get` fails and every consumer
  must use a filesystem `replace`. That `replace` path is whitespace-tokenized
  in `go.mod`, so it **breaks on any path containing a space** — e.g.
  a workspace path containing spaces. Phase 0.1 required a
  `/tmp/neutron-go-src` symlink workaround to compile at all. Also
  `ARCHITECTURE.md` still describes the abandoned per-module `go.mod` split
  (`nucleus-go/kv/go.mod`, etc.); it is in fact one mono `go.mod` (extends the
  doc-rot theme of NI-001).
- **Expected:** A consumer can `go get github.com/<org>/neutron-go` and import
  it with no `replace`, no symlink, regardless of workspace path. Either
  publish under the declared `neutron-dev` path, or align the module path with
  the real repo (`neutron-build`) and publish there.
- **Impact:** Every external consumer (this project is the first real one) hits
  this on day one. The space-path problem specifically affects this
  workspace layout.
- **Status:** open
- **Links:** `PLAN.md` Phase 0.1; workaround documented in
  `spike/phase0/go.mod`

## NI-007 — SQL query path is unconditionally degraded to Nucleus's pgwire workarounds, even on Postgres

- **Date:** 2026-08-12
- **Component:** `Neutron/go/nucleus/sql.go`
- **Severity:** major
- **Observed:** Every `Query`/`QueryOne`/`Exec` forces
  `pgx.QueryExecModeSimpleProtocol` (sql.go:35-37, 67-69, 94-96), and `scanRow`
  scans **all** columns as nullable strings then re-parses into Go types
  (sql.go:145-153). Comments state why: Nucleus's extended protocol returns
  wrong results for COUNT/SUM, types all params as TEXT (breaks BIGINT
  arithmetic), and sends binary format indicators with text data. But the
  client applies these workarounds unconditionally — no branch on
  `Features.IsNucleus`. So on plain Postgres you still pay: no prepared
  statements, no native typed decoding, slower + lossier scans.
- **Expected:** Branch on `IsNucleus`. On Postgres: use the extended protocol
  (default) + pgx's native field decoding (fast, type-safe, no string
  intermediary). On Nucleus: keep the current workaround path. The
  auto-detect (`client.go:213-240`) already exists — the query path just
  doesn't use it.
- **Impact:** Postgres path (our v1 safe default, see `PLAN.md` §3) inherits
  Nucleus's degraded protocol for no benefit. Performance + type-fidelity left
  on the table on the target we'll actually run in production first. Also makes
  NI-004 worse: native pgx time decoding would handle TIMESTAMPTZ correctly
  with zero custom parsing.
- **Status:** open
- **Links:** compounds NI-004; relevant to `PLAN.md` §3 storage decision

## NI-008 — `Conn` keeps no dropped-message count; backpressure is invisible to consumers

- **Date:** 2026-08-12
- **Component:** `Neutron/go/neutronrealtime/hub.go` (`Conn.trySend`, lines 20-30)
- **Severity:** major
- **Observed:** `trySend` silently drops messages when the Send buffer is full
  (`select { case c.Send <- msg: default: }`). There is no counter incremented
  on the drop path and no `Conn.Dropped()` accessor. Phase 0.2 stress confirmed
  shedding works correctly (0% drops at realistic 5 kHz × 100 viewers; 0.18%
  only at the 1.1M-msg/s saturation ceiling), but consumers cannot tell *whether*
  or *how many* messages were dropped — they have to diff produced-vs-received
  themselves, which they can't do for an inbound-only socket.
- **Expected:** A console UI must be able to tell the user "N lines dropped due
  to backpressure" (see `VISUAL-BRIEF.md` §3 — the dropped-lines notice is a
  required console behavior). That requires the Conn to count its own drops.
  Suggested: an `atomic.Int64 dropped` on `Conn`, incremented in `trySend`'s
  default branch, exposed via `Conn.Dropped() int64` (and reset on read, or
  monotonic with a `ResetDrops()`).
- **Impact:** The flagship realtime use case (console streaming) needs visible
  backpressure to be honest with the user. Without this, either the UI can't
  show the notice, or the app re-implements delivery tracking above the Hub
  (duplicating what the Conn already knows). Also affects any "did I miss
  anything?" reconnect reconciliation.
- **Status:** open
- **Links:** `PLAN.md` §6 (ring-buffer replay), §10 Phase 0.2;
  `VISUAL-BRIEF.md` §3 (dropped-lines notice)

## NI-009 — `errInterceptor` double-writes headers when a mounted handler writes its own status

- **Date:** 2026-08-12
- **Component:** `Neutron/go/neutron/router.go` (`errInterceptor.WriteHeader`, line 285)
- **Severity:** minor
- **Observed:** When an `http.Handler` mounted via `Router.Handle` writes a
  response status itself (e.g. the `coder/websocket` upgrader writing `426
  Upgrade Required` for a non-WebSocket request), the stdlib logs:
  `http: superfluous response.WriteHeader call from
  github.com/neutron-dev/neutron-go/neutron.(*errInterceptor).WriteHeader
  (router.go:285)`. The errInterceptor attempts a `WriteHeader` after the
  underlying handler already did. Repro: Phase 0.3 spike, `curl
  http://localhost:8080/ws/console` against a mounted WS handler.
- **Expected:** A mounted raw `http.Handler` owns its own response. The
  errInterceptor should not write a status after the handler has already
  written one — it should check `headerWritten` (or equivalent) before
  writing, the same way `http.ResponseController`/standard wrappers do.
- **Impact:** Observed effect is just a log warning + the *correct* 426 still
  goes out, so functionally harmless for the WS case. But it signals the
  error-interceptor's header-write guard is incomplete, which could mask or
  corrupt handler-written responses in other paths. Worth tightening.
- **Status:** open
- **Links:** `PLAN.md` §10 Phase 0.3 (reproducer at `spike/phase0-3/`)

---
