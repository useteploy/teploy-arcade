# Deploying Teploy Arcade

The recommended deployment runs **natively as a systemd unit on a Docker host**. Container deployment is also supported when host bind paths are mapped correctly.

That is a deliberate choice, not a shortcut. The panel creates each game server
as a container on the host's Docker daemon. If the panel is itself a container,
it is a *sibling* of the servers it manages, and every bind mount it asks for is
resolved by the daemon on the host rather than inside the panel's filesystem —
so the panel's idea of `/var/teploy-arcade/servers/abc` and the daemon's have to
be made to agree, by hand, forever. Run it natively and they simply agree.

It also means the panel needs no Docker socket mount, because it *is* on the
Docker host.

## Install

On a host with Docker installed:

```sh
# 1. Build for the target (static, no cgo)
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=$(git describe --tags --always 2>/dev/null || echo dev)" \
    -o teploy-arcade ./cmd/teploy-arcade

# 2. Ship it
scp teploy-arcade root@HOST:/usr/local/bin/teploy-arcade
ssh root@HOST 'chmod +x /usr/local/bin/teploy-arcade && install -d -m 0700 /var/teploy-arcade'
```

Then the unit, at `/etc/systemd/system/teploy-arcade.service`:

```ini
[Unit]
Description=Teploy Arcade game server panel
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

[Service]
Type=simple
ExecStart=/usr/local/bin/teploy-arcade -host 127.0.0.1 -port 3457 -data /var/teploy-arcade -public-origin https://panel.example.com -game-host games.example.com
Restart=always
RestartSec=3
User=root

[Install]
WantedBy=multi-user.target
```

```sh
systemctl daemon-reload && systemctl enable --now teploy-arcade
curl -fsS http://127.0.0.1:3457/api/health
```

## The first admin

**Provision it with the deploy** and there is no first-run flow at all:

```sh
TEPLOY_ARCADE_ADMIN_USER=admin \
TEPLOY_ARCADE_ADMIN_PASSWORD='...' \
  teploy-arcade -host 0.0.0.0 -port 3457 -data /var/teploy-arcade
```

or in the systemd unit:

```ini
Environment=TEPLOY_ARCADE_ADMIN_USER=admin
Environment=TEPLOY_ARCADE_ADMIN_PASSWORD=...
```

The account is created on first boot, `auth` is enforced immediately, and no
setup token is ever printed. This is the same role `TEPLOY_DASH_PASSWORD` plays
for teploy-dash, and it is the path most deployments should take. There are
matching `-admin-user` / `-admin-password` flags, but prefer the env vars: a
password on a command line is readable by any process on the host via `ps`.

These credentials **bootstrap** a panel; they do not override one. On a panel
that already has accounts they are ignored, so leaving them in a unit file does
not reset anyone's password on the next restart.

### If you did not provision one

The panel starts unclaimed and opens first-run setup, gated by a token so a
reachable panel cannot be claimed by whoever finds it first — admin here means
creating containers as root on the host. The token is written to the log, never
to an HTTP response:

```sh
journalctl -u teploy-arcade | grep "Bootstrap token"
```

Paste it into the setup form with the username and password you want. Valid
**30 minutes**; restart the panel for a fresh one.

## TLS, game addresses and session policy

Terminate TLS at a trusted reverse proxy (for example Caddy) and proxy the panel to `127.0.0.1:3457`. Preserve WebSocket upgrades and SSE streaming. Set `-public-origin https://panel.example.com` (or `TEPLOY_ARCADE_PUBLIC_ORIGIN`) to the exact HTTPS origin reached by browsers. This explicitly sets secure session cookies for proxy deployments; Arcade does not trust arbitrary `X-Forwarded-Proto` headers. Serve the browser only through that origin. Direct TLS requests also set secure cookies.

Set `-game-host games.example.com` (or `TEPLOY_ARCADE_GAME_HOST`) to the player-facing DNS name or IP, without a scheme or port. Raw IPv6 literals are supported and displayed with brackets. This is independent of the panel URL and bind address. If unset, the UI shows the configured port and asks for a game host instead of inventing a reachable endpoint from a wildcard listener. Loopback game hosts are usable only by local players.

Sessions are bounded to 32 per account and 4096 globally. Admission refuses another login at the cap rather than evicting existing live sessions. Sign out unused sessions; expiry reclaims slots. On session expiry/revocation the browser closes its streams and dialogs and returns to sign-in.

## Firewall

The panel publishes only its own port. Each game server publishes its own port
directly on the host, so open the ranges you intend to use:

```sh
ufw allow 443/tcp             # TLS reverse proxy; panel stays on loopback
ufw allow 25565:25600/tcp     # Minecraft Java
ufw allow 19132:19140/udp     # Bedrock
ufw allow 28015:28020/udp     # Rust game/query UDP; open configured RCON TCP separately
```

## Running it in a container instead

Supported by the Dockerfile, but you must mount the socket and give the data
directory the *same path inside and out*:

```sh
docker run -d --name teploy-arcade \
  -p 127.0.0.1:3457:3457 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /var/teploy-arcade:/var/teploy-arcade \
  ghcr.io/useteploy/teploy-arcade:latest \
  -host 0.0.0.0 -port 3457 -data /var/teploy-arcade \
  -public-origin https://panel.example.com -game-host games.example.com
```

The image is published by the release workflow on a `v*` tag, so it exists from
the first tagged release onward and not before. Building it yourself is one
command if you are ahead of a release:

```sh
docker build -t teploy-arcade --build-arg VERSION=dev .
```

If the paths cannot match, pass `-data-host` with the host-side path so the
panel can translate. Without one of those two, every game server bind-mounts an
empty directory and boots a blank world.

Mounting the socket gives the panel effective root on the host — the same trust
model as Pterodactyl's Wings and Dokploy. Do not expose it without auth.

## Why not `teploy up`

teploy cannot express a host bind mount: `volumes:` maps a *name* to a container
path and binds it under `/deployments/<app>/volumes/<name>`, with keys validated
against `^[a-z0-9][a-z0-9-]*[a-z0-9]$`. There is no way to mount
`/var/run/docker.sock`, and no raw-docker-options escape hatch. See the comments
in `teploy.yml` — the file is kept as the target shape, and validates, but is
not the deploy path today. This is a teploy gap, not an Arcade one, and it
affects anything that talks to the Docker socket.

## Migrating from Crafty Controller

Arcade's importer reads a server directory directly and detects its software,
version, jar, port, MOTD and world.

1. Stop the server in Crafty (or pick one already stopped).
2. Copy its directory to the Arcade host. Crafty names directories by UUID; the
   friendly names are in its SQLite DB at
   `crafty-4/app/config/db/crafty.sqlite`, table `servers`.
3. `POST /api/import/scan` with the path to see what Arcade makes of it.
4. `POST /api/import` with `mode: "copy"`.

Use **copy**, not **adopt**, while Crafty still has the server registered.
Adopting in place gives two panels one server, and they will fight over its
port, its process and its world. Arcade detects `crafty_managed.txt` and says so.

Imported servers run the jar they arrived with (`TYPE=CUSTOM`), not one matched
by version — a modpack pins an exact loader build and its mods are compiled
against it. The JRE is chosen from the Minecraft version, so a 1.12.2 pack gets
Java 8 rather than failing inside the game's log on a modern JVM.

A Forge modpack ships the vanilla `minecraft_server.<ver>.jar` alongside its
loader jar; that is the server Forge runs on, not a second server, and the
importer treats it that way. When the loader jar carries no version of its own
(plain `forge.jar` is common), the version is read from that vanilla jar —
which is what makes the JRE selectable.

## Restarting the panel

Safe. Containers are started detached, so they outlive the panel process; on
boot the panel re-attaches to anything still running and says so on the console.

This was not always true. Containers used to run attached with `-i`, which made
the panel's pipe the container's stdin, and a Minecraft console treats stdin EOF
as "shut down" — so every panel restart stopped every server cleanly, with exit
code 0 and nothing to explain it. Console commands go over RCON as a result,
which the panel forces on per container (`ENABLE_RCON=true` with a generated
password, on an unpublished port reached only via `docker exec`).

## Changing a server's memory or CPU

```sh
curl -X PATCH http://HOST:3457/api/servers/<id> \
  -H 'Content-Type: application/json' \
  -d '{"memory_mb": 6144, "cpu": 3}'
```

Admin only, and bounded by the same limits as create and import. `0` (or an
omitted field) leaves that one alone. These reach the container as `--memory`
and `--cpus` on the next `docker run`, so a running server keeps its current
limits until it restarts — the response returns `pending_restart` saying so
rather than reporting a number the container is not actually held to.


## Trusted plugin mirrors

Plugin downloads use public HTTPS destinations and a pinned resolved public IP for each request. Redirect targets are checked too. If your deployment deliberately trusts a private mirror, set `TEPLOY_ARCADE_ALLOW_PRIVATE_PLUGIN_HOSTS=1`. If it also requires unencrypted HTTP, set `TEPLOY_ARCADE_ALLOW_HTTP_PLUGINS=1`. These are operator-wide trust exceptions; leave both unset for public installations. Supply an expected SHA-256 when available, and install only plugins you trust to execute inside the game runtime.

## Candidate acceptance and deployment gates

Build and test the exact revision before copying the binary. Run `make build`, `make vet`, `make test`, `make race`, the frontend syntax check, and the pinned govulncheck command in README.md. CI also builds and scans its exact candidate image and checks the embedded frontend/CLI/setup gate. A release workflow publishes only after the reusable verification workflow passes, and stable tags alone move `latest`.

Before promotion, use isolated disposable game fixtures to check create/start/stop/restart, daemon interruption and panel re-adoption, backup/restore, and storage/permission/cancellation failure paths. Verify keyboard-only navigation, skip link without route change, modal Escape/focus return, role-disabled controls, session revocation and player-head opt-in in a real browser. Do not use existing game worlds for destructive tests. Preserve any recovery staging reported after a restore failure.

Deployment-specific checks remain necessary: confirm the proxy serves HTML on document requests and forwards SSE/WebSocket traffic, cookies are Secure through HTTPS, anonymous operations are refused, and game ports/protocols are reachable from the intended player network. Confirm the published archive checksum and container digest before installing. Publishing a tag or deploying to a real host is a separate promotion step; local validation does not create those artifacts.

Release tags are immutable version identities. The publisher refuses a version that already has a GitHub release, checks remote tag identity, and publishes/scans the image only after binary publication succeeds. Retry a failed new release before it creates a release; after publication begins, use a fresh version for a correction. Never move a previously published tag to a different commit. A historical release with mixed provenance needs a fresh coherent release rather than silent asset replacement.

The supported Docker endpoint is a local Unix socket. Remote Docker daemons are refused for launch/adoption because Arcade cannot verify that remote bind paths and capacity describe its local filesystem. Host filesystem/capacity metrics describe the panel host (`panel_host` scope). For Geyser, use an enabled regular plugin jar and one bounded scalar `bedrock.port` mapping; turn `clone-remote-port` off and remove malformed or duplicate YAML port mappings before admission.

## Async operation migration

Backup creation, plugin install and restore clients can request `Prefer: respond-async` together with a unique `Idempotency-Key` (1–128 characters). The browser uses this contract. The agent returns 202 with an operation ID and Location `/api/servers/{id}/operations/{job}`. Poll that resource until `done` or `failed`; a failed status read retries the same operation ID rather than posting the action again. Reusing a key with changed request content is refused. Legacy clients without Prefer retain synchronous behavior.

Job metadata is durable under the private data directory's `jobs/`, capped at 1000 records and 64 KiB per record. Verified terminal metadata expires after one hour; active jobs are never evicted for admission. A panel restart marks interrupted jobs failed instead of replaying potentially destructive work, and preserves partial world/source bytes. Corrupt or oversized registry records stop recovery with an actionable error and remain intact for inspection. Archive reviewed metadata to resolve a registry-capacity error; never delete a world to free a metadata slot. If a submission response is lost before the browser receives an ID, the UI reports uncertainty and refuses blind resubmission. Inspect panel state before starting another action.

Host telemetry carries a measurement scope, source, sample time and CPU staleness. Missing or stale usage is shown as unknown. Host-visible `/proc` memory usage is labeled separately from a panel cgroup/process limit and is not drawn as a percentage of that unrelated limit. Docker creation and resource forms use measured Docker-daemon CPU/memory capacity, which can differ from the panel process quota. When daemon capacity is unknown, the UI labels it unknown instead of falling back to the panel quota. Configured resource commitment remains separate from measured usage.
