# Teploy Arcade

A single-process game-server panel with an embedded browser UI, Docker lifecycle management, console streaming, files, backups, players, scheduled tasks, imports, clones, and scoped MCP access. State is stored locally as JSON; metrics history is in memory.

## Run

```sh
make build
./bin/teploy-arcade -data ./data -game-host games.example.com
# Open http://127.0.0.1:3457 and complete the token-gated first-admin form.
# Development simulator only, on a literal loopback address:
./bin/teploy-arcade -data ./demo-data -no-auth
```

Go 1.26 and Node 22 are used in CI. The frontend is plain JavaScript embedded in the binary, so it runs outside the source tree without a separate frontend build. Fresh state seeds demonstration servers and local templates; demonstrations use the simulator. Select Docker when creating a real server. Docker must be available to the panel process, with the data path visible at the same host path or translated by `-data-host`.

The first run is locked until the administrator uses the bootstrap token printed in the process log, or provisions an administrator through the environment. `-no-auth` is limited to literal loopback addresses. Do not delete the state directory to repair a problem: it contains managed server trees, credentials, backups and panel configuration. See [DEPLOY.md](DEPLOY.md) for installation and recovery precautions.

## Features and deliberate constraints

- Three roles: viewers read, operators manage routine lifecycle/files/plugins/tasks, administrators create/import/clone/delete, restore backups, allocate resources, manage users and issue tokens.
- Docker servers survive panel restarts and are reconciled on boot. The simulator is for demonstrations, not a substitute for a game process.
- Imports support copying a stopped directory or explicitly adopting it in place. Imported Minecraft jars use the custom-jar runtime and a version-appropriate JRE. Do not adopt a directory another panel still manages.
- Backup, plugin-install and restore actions use durable async operation IDs and idempotency keys in the browser. Closing a view stops status polling without cancelling accepted panel work. Interrupted operations recover as failed after restart and are never automatically replayed. Status-read retries poll the same job; an ambiguous lost submission response is shown explicitly.
- Clone jobs copy an existing server with status/progress polling. Closing the dialog stops browser polling; accepted jobs remain owned by the panel.
- Scheduled tasks support console steps and `!start`, `!stop`, `!restart`, `!backup`, `!wait` at a local civil time, with optional daily repetition.
- Plugin/mod installs use an operator-supplied URL, structural ZIP validation and optional SHA-256. There is no remote plugin catalogue. Public HTTPS endpoints are the default; trusted mirror exceptions are documented in DEPLOY.md.
- File and settings editors save against the ETag of their original draft. A concurrent change returns a conflict and retains the draft; comparison shows the saved version separately, and reloading requires explicit discard confirmation.
- Player observations identify log/RCON/simulation provenance. Unknown latency or UUID is displayed as a dash rather than invented values.
- Player heads are off by default. Explicit opt-in sends player names to mc-heads.net from the browser with no referrer; disabling them retains local gradient avatars.
- MCP tokens grant selected existing server IDs and typed actions. New tokens default to server reads; lifecycle/backup and host reads are explicit capabilities. Raw console, kill, delete, restore and user management are excluded. Legacy unscoped tokens must be reissued. Tokens are shown once and stored hashed.
- Memory and CPU limits are enforced by Docker. Disk allocation is a declared limit; no portable per-directory filesystem quota is enforced. Metrics history is lost on panel restart. Templates remain local JSON files.

## API

Requests use the same-origin session cookie. JSON mutations require `Content-Type: application/json`; a missing session returns 401, insufficient role 403, and unclaimed setup state 503. Bootstrap endpoints and health remain accessible before sign-in.

| Route | Purpose |
|---|---|
| `GET /api/health`, `GET /api/me` | Health and session/setup state |
| `POST /api/login`, `/api/logout`, `/api/setup` | Authentication and first-admin setup |
| `GET /api/host`, `/api/templates`, `/api/servers` | Host, templates and server inventory |
| `POST /api/servers`, `DELETE /api/servers/{id}` | Admin create/delete |
| `POST /api/servers/{id}/start`, `/stop`, `/restart`, `/kill`, `/command` | Lifecycle and console; kill requires admin |
| `GET`, `PATCH /api/servers/{id}/settings` | Settings schema/values and updates |
| `PATCH /api/servers/{id}` | Admin memory/CPU allocation; reports restart-pending limits |
| `GET /api/servers/{id}/files`, `/file`, `/download`; `PUT`, `DELETE /file`; `POST /mkdir` | Files below the server root |
| `GET`, `POST /api/servers/{id}/backups`; `POST /backups/{bid}/restore`; `DELETE /backups/{bid}` | Backup inventory/create/restore/delete |
| `GET`, `POST /api/servers/{id}/tasks`; `PATCH`, `DELETE /tasks/{tid}`; `POST /tasks/{tid}/run` | Scheduled task management |
| `POST /api/import/scan`, `/api/import`, `/api/clone`; `GET /api/import/{job}` | Import and clone jobs |
| `GET`, `POST /api/mcp-tokens`; `DELETE /api/mcp-tokens/{name}` | Admin scoped-token management |
| `GET /api/servers/{id}/operations/{job}` | Scoped async backup/plugin/restore status |
| `GET /api/events`, `/ws/console?server={id}` | Session-authorized SSE and WebSocket streams |

The WebSocket down protocol carries `replay`, `line`, `dropped`, `status`, `players`, `command_ack`; up carries `command`. Each room has bounded replay with monotonic sequence numbers and per-connection drop accounting. Commands route to the game, not to other viewers.

Backups quiesce supported running games before archiving, and restore requires a stopped server. Accepted commands alone do not prove a durable game save; unsupported live snapshot contracts must be refused or handled by stopping the game. Restore retains recovery staging when rollback cannot complete. Read the failure response and preserve that staging until recovery finishes.

## Verify and release

```sh
make build
make vet
make test
make race
for script in cmd/teploy-arcade/frontend/*.js; do node --check "$script"; done
go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...
```

Tagged releases depend on CI, including race/frontend checks and a scanned candidate container. Actions and base images are pinned by immutable identifiers; GoReleaser is pinned and verifies dependency manifests without running `go mod tidy`. Dependabot proposes updates. Real game runtime, browser keyboard/session lifecycle, restore failure and deployed TLS acceptance must be exercised on the candidate before promotion; a passing source test is not proof of a deployed service.

[DEPLOY.md](DEPLOY.md) describes native systemd and container deployment. [AUDIT_OPEN.md](AUDIT_OPEN.md) tracks unresolved defects and acceptance gates. Historical PLAN/TODO and audit reports describe earlier checkpoints and may differ from current code.
