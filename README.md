# Teploy Arcade

One static Go binary with an embedded frontend: manages game servers on a single
host and serves the panel that drives them. Teploy installs it; it is the
long-running runtime, not part of the CLI (`PLAN.md` §9).

Covers `PLAN.md` **phases 1-8** to varying depth — see "What is and isn't done"
at the bottom, and the per-phase notes in `PLAN.md` itself.

## Run

```sh
make build && ./bin/teploy-arcade      # http://127.0.0.1:3457
go run ./cmd/teploy-arcade -port 9000  # different port
go run ./cmd/teploy-arcade -host 0.0.0.0
go run ./cmd/teploy-arcade -no-auth    # development only
```

Layout follows teploy-dash: `cmd/teploy-arcade` for the binary, `internal/` for
everything else, frontend embedded via `//go:embed` so the binary works outside
its source tree, `make build|test|vet|clean`, and goreleaser passing
`main.version/commit/date`.

One Go dependency, nothing to configure. State lands in `/var/teploy-arcade`
when writable and `./data` otherwise; delete it to start over. First run seeds
five servers so the panel isn't empty.

```sh
make test                # 20 tests, ~20s
```

On first run the panel is **open** — no users, loopback only, no ceremony. Create
the first admin in **Settings → Access** and auth switches on from that moment.
The startup banner shouts if the agent is bound to a non-loopback address with no
users configured.

## Runtimes

Every server records which runtime it uses, and the panel shows it. Nothing
pretends to be a container when it isn't.

| | What it is | When to use it |
|---|---|---|
| **`sim`** | In-process simulator. Emits a realistic Minecraft log, tracks players, answers console commands. | Default. Starts in ~2.5s with nothing to pull — this is what makes the panel usable immediately. |
| **`docker`** | **Verified end to end 2026-08-12** against a real Paper 1.20.4 server: boot, world generation, console commands reaching stdin, a 166 MB live-world backup with the save-off/save-on cycle confirmed in the game's own log, and a graceful stop that saved chunks. Real container: `docker run -i` with our own stdin pipe, `--memory` and `--cpus` as hard limits, the panel's own server directory bind-mounted at `/data` so files and backups work identically for both runtimes. | Actual game servers. The first start pulls the image (`itzg/minecraft-server` is ~500 MB). |

Pick the runtime in the create dialog. Docker is offered only when the daemon is
reachable.

## Console commands the simulator understands

`help`, `list`, `say <msg>`, `whitelist add|remove <player>`, `kick <player>`,
`time set <day|night>`, `weather <clear|rain>`, `stop`

Two exist purely to make failure states reachable on demand:

- **`flood`** — pushes 5000 lines at once. Watch the dropped-line marker appear
  inline in the stream: that count comes from the socket's own drop counter, not
  a guess in the browser.
- **`crash`** — forces an OOM exit so the failed state, exit code and restart
  count are reachable without waiting for a real crash.

## Roles

| Role | Can |
|---|---|
| `viewer` | Read everything, watch the console. Cannot type in it. |
| `operator` | Start/stop/restart/kill, run console commands, edit settings and files, take backups. |
| `admin` | The above, plus create/delete servers, restore backups, manage users. |

Enforced on every mutating route *and* on the console socket — a viewer holding a
session cannot start a server by POSTing directly, and cannot send a command over
the WebSocket. Passwords are PBKDF2-HMAC-SHA256, 120k iterations, per-user salt,
constant-time compare, standard library only.

## MCP

`POST /api/mcp` — hand-rolled JSON-RPC 2.0, same shape as `teploy-dash/internal/mcp`
so a client attached to both behaves identically. Bearer tokens are separate from
panel sessions and are minted at `/api/mcp-tokens` (admin only), stored hashed and
shown once.

Tools are namespaced `arcade_*`, not `teploy_*`: dash owns that prefix, both
servers can be attached at once, and "server" means a deploy target there but a
game server here — an unprefixed name would actively mislead a model holding both
toolsets.

```
arcade_list_servers    arcade_get_server     arcade_console_tail
arcade_send_command    arcade_lifecycle      arcade_list_backups
arcade_create_backup   arcade_host_status
```

The tool surface is deliberately narrower than the HTTP API: reads, the lifecycle
verbs and backups. No delete, no restore, no user management. `kill` is refused
with a reason — it is SIGKILL and loses unsaved chunks, so it stays a human
decision. Everything an agent does is attributed to actor `mcp` in the audit log.

## API

```
GET    /api/host                        host allocation ledger
GET    /api/templates                   templates + next free port
GET    /api/servers                     list + host
POST   /api/servers                     create                       (admin)
GET    /api/servers/{id}
DELETE /api/servers/{id}                                             (admin)
POST   /api/servers/{id}/start|stop|restart|kill|command             (operator)
GET    /api/servers/{id}/settings       schema joined to current values
PATCH  /api/servers/{id}/settings       returns requires_restart[]   (operator)
GET    /api/servers/{id}/metrics        ?window=5m|15m|1h
GET    /api/metrics                     host aggregate
GET    /api/servers/{id}/files          ?path=
GET    /api/servers/{id}/file           ?path=          read
PUT    /api/servers/{id}/file           write                        (operator)
DELETE /api/servers/{id}/file           ?path=                       (operator)
POST   /api/servers/{id}/mkdir                                       (operator)
GET    /api/servers/{id}/download       ?path=
GET    /api/servers/{id}/backups
POST   /api/servers/{id}/backups        create (quiesced)            (operator)
POST   /api/servers/{id}/backups/{bid}/restore                       (admin)
DELETE /api/servers/{id}/backups/{bid}                               (admin)
GET    /api/auth/me
POST   /api/auth/login | /api/auth/logout | /api/auth/setup
GET    /api/users | POST /api/users | DELETE /api/users/{name}       (admin)
GET    /api/audit                       append-only action log
GET    /api/events                      SSE: status transitions + metrics
GET    /ws/console?server={id}          WebSocket console
```

## Templates are data, not code

`data/templates/*.json`, seeded from the built-ins on first run. Drop a file in,
restart, and the game appears in the create dialog — no code change. A malformed
file is skipped and reported rather than fatal.

The format is clean-slate rather than Pterodactyl's egg JSON. That is `PLAN.md`
§12 question 2, and the answer got easy once the UI existed: what the panel needs
from a template is *display* metadata — group, description, recommended flag,
maturity, per-field help — which Ptero's format does not carry. Adopting it would
import an installer-script model and licence questions for no gain.

## Backups

`save-off` → `save-all flush` → wait → archive → `save-on`. File writes are
refused for the whole window, so a snapshot can never catch a half-written chunk
(`PLAN.md` §8). Saves resume even if the archive fails — a server left with
`save-off` is a far worse outcome than a failed backup. Restore keeps the old
tree until the new one lands, and refuses to run while the server is up.

The WS protocol is the one written up in `VISUAL-HANDBACK.md` §4.4. Down:
`replay`, `line`, `dropped`, `status`, `players`, `command_ack`. Up: `command`.

Three properties it holds, each with a test in `console_test.go`:

- **Inbound commands route to the game's stdin, never to other viewers**
  (`NEUTRON-ISSUES.md` NI-002). `TestCommandRoutesToServerNotPeers` proves a
  peer never receives the raw command but does receive its output.
- **Every socket counts its own dropped lines** (NI-008). The hub here does what
  `neutronrealtime` doesn't: `Conn.Dropped()`. Without it the "N lines dropped"
  notice would be a lie. `TestBackpressureIsCountedAndReported` asserts nothing
  vanishes unaccounted — delivered + dropped == published.
- **Every line carries a monotonic per-room `seq`**, so a client can localise a
  gap and dedupe a replay. Cheap now, impossible to retrofit.

## Feature coverage

| Area | Status |
|---|---|
| **Players** | ✅ Built — whitelist / operators / banned / banned IPs, sub-tabs, add bar, file-editor switch. Changes route through the game console while running and straight to the file while stopped, so the game never overwrites an edit; while a server is `starting` or `stopping` the edit is refused rather than written, because the game owns the files in both transient states. |
| **Scheduler** | ✅ Built — named tasks at a 24h time, daily repeat, run-now. Adds panel actions beyond plain console commands: `!restart`, `!backup`, `!stop`, `!start`, `!wait`, so the nightly restart is expressible. |
| **Dashboard** | ✅ Built — CPU/RAM/UPTIME tiles, Server Statistics panel, 1m/5m/30m/1h/4h time scale, CPU area + RAM area + players bar chart with right-aligned readouts, players sidebar. |
| **Plugins** | ✅ Built, minus a catalogue browser — lists the jars in the one directory the template actually reads (`plugins/` for Paper/Spigot/Purpur/Velocity, `mods/` for Forge/Fabric/NeoForge/Quilt, an explanation for Vanilla and Bedrock), enable/disable by renaming to `.jar.disabled`, delete, and install from a URL. No Modrinth/Spigot index: that is a network dependency and a licensing question, so the URL bar is the whole install surface. |
| **Import an existing server** | ✅ Built — scans a directory on the host, identifies the template from the jar name, reads the port from `velocity.toml` for proxies and `server.properties` otherwise, warns when another panel is still pointed at the same tree, then either copies it in or adopts it in place by symlink. It does not launch a bare jar: the panel runs the simulator or `itzg/minecraft-server`, and the scan says so rather than implying otherwise. |
| **Clone an existing server** | Not built |

Global settings are deliberately shallow for now — the agent, users and the
audit log. Auto-update, autostart and per-tab display toggles are not built.

Player avatars are gradient placeholders rather than fetched player heads.

## Audit findings (2026-08-12)

A deliberate bug-hunting pass, after two earlier passes each turned up real
defects. Everything below was found and fixed:

| Found | Why it mattered |
|---|---|
| **Cross-origin WebSocket accepted** — `InsecureSkipVerify` disabled the Origin check | WebSockets bypass CORS, so that check was the only defence. Any page the operator visited could open a console socket and run commands on their servers. |
| **Data races on `Server.Status`** | Runner goroutines write it while HTTP handlers read it. Found by `-race`, not by reading. All reads now go through a locked `State()`. |
| **`Save()` marshalled without locks** | `json.Marshal` reads every field while runners write them; persistence raced state changes. |
| **Per-server dashboard navigated away from itself** | `#/dashboard` and `#/s/<id>/dashboard` share a route name, so the event feed re-rendered the global dashboard over it every 2s, leaking a timer each time. |
| **Console rooms leaked on delete** | Each holds a 500-line ring buffer, kept for the life of the process. |
| **No crash-loop breaker** | A server dying instantly was restarted forever. Now opens after 5 failures in 10 minutes, with an explicit clear. |
| **Port conflicts only checked at create** | A hand-edited `server.properties` let two servers fight over a port until Docker failed the bind. |
| **Tab strip hid the open server** | Opening the 6th server showed a strip with no active tab. |

Second pass, on failure recovery and resource bounds:

| Found | Why it mattered |
|---|---|
| **A corrupt state file stopped the panel booting** | One bad byte in `servers.json` and nobody could reach any server. Bad files are now quarantined beside the original and the panel starts, loudly. |
| **A panic in any background goroutine killed the process** | One bad scheduled task or runner would take down the panel and every server's management with it. Contained per goroutine. |
| **Unbounded request bodies** | A single POST could exhaust memory. Capped at 8 MB by middleware, reported as 413 rather than a decode failure. |
| **Unbounded console lines** | The ring holds 500 lines and fans each to every viewer; modded servers really do print megabyte stack dumps on one line. Capped at 8 KB with an explicit truncation marker. |
| **Expired sessions were never reaped** | They were only dropped when looked up again, so the map grew for the process lifetime. |

Third pass, on the dimensions that only show up under stress or in a deploy:

| Found | Why it mattered |
|---|---|
| **Containers outlived the panel, invisibly** | After a panel restart the server showed as stopped while its container ran. Pressing Start force-removed a *live* server with no graceful save. Fires on every redeploy or reboot. The panel now reconciles with Docker on boot and re-adopts. |
| **`docker attach` was the wrong adoption mechanism** | It does not exit cleanly and would wedge shutdown. Adoption streams via `docker logs -f --tail 200` and sends commands over RCON, the channel Crafty and Pterodactyl use. |
| **The first adoption fix broke exit detection** | An adopted container stopping was never noticed; the server hung in "stopping" forever. Replaced with `docker wait`. |
| **Bind mounts broke when the panel was containerised** | `docker run -v` resolves on the *host*, so a containerised panel gave game servers paths that do not exist. Docker creates them empty — the game boots a blank world while the file manager shows the real one. Silent. Fixed with `-data-host`, an identical-path bind mount in `teploy.yml`, and a startup warning. |
| **Filesystem errors leaked internal paths** | A full disk reported the panel's `.tmp` path. Now names the cause and the file the operator asked for. |

`make race` runs the detector; `make test` includes the frontend routing cases.

## What is and isn't done

Done: control API, console streaming, panel UI, resource history and graphs,
file manager, backups with the quiesce window, templates as data, auth with
three roles, audit log, Dockerfile and `teploy.yml`.

Not done, and worth knowing before trusting this anywhere real:

- **Storage is a JSON file**, not Postgres via `nucleus-go`. `PLAN.md` §3 locks
  Postgres as the v1 default; that swap is a `Store` implementation away and was
  skipped so this runs with zero setup. NI-006 (neutron-go isn't `go get`-able
  and its `replace` breaks on paths containing spaces) makes it a chore today.
- **The panel is vanilla JS, not Neutron-TS.** Views map onto the components
  named in `mockups/`, so the port is mechanical — but it is still owed.
- **`teploy.yml` has never been run on a real VPS.** Reviewed draft, not a
  proven deploy.
- **No disk quota per volume.** Needs XFS project quotas or a loop device;
  docker has no portable equivalent. The number in the UI is the declared limit,
  not an enforced one.
- **No scheduled backups.** That is the `neutronjobs` cron half; the Scheduler
  tab is still marked v1.1.
- **No plugin catalogue.** Plugins install from a URL you supply; nothing here
  browses Modrinth or SpigotMC.
- **No max-restart circuit breaker**, no disk-full handling, no network-partition
  behaviour. `PLAN.md` §8's chaos scripts were not written.
- **No remote template registry** — templates are local files.
- **The console token flow in `PLAN.md` §6** (Ed25519, browser→agent direct) is
  not implemented, because the panel and agent are one process here and the
  browser talks to a single origin. It matters again the moment they split.
- **Metrics are in-memory only** and are lost on restart. `nucleus/ts` would
  take this over.
