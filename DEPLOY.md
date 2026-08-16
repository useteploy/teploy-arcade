# Deploying Teploy Arcade

Arcade runs **natively as a systemd unit on a Docker host**, not in a container.

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
ExecStart=/usr/local/bin/teploy-arcade -host 0.0.0.0 -port 3457 -data /var/teploy-arcade
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

## Firewall

The panel publishes only its own port. Each game server publishes its own port
directly on the host, so open the ranges you intend to use:

```sh
ufw allow 3457/tcp            # the panel (or omit, if reached over a VPN)
ufw allow 25565:25600/tcp     # Minecraft Java
ufw allow 19132:19140/udp     # Bedrock
ufw allow 28015:28020/tcp     # Rust
```

## Running it in a container instead

Supported by the Dockerfile, but you must mount the socket and give the data
directory the *same path inside and out*:

```sh
docker run -d --name teploy-arcade \
  -p 3457:3457 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /var/teploy-arcade:/var/teploy-arcade \
  ghcr.io/useteploy/teploy-arcade:latest \
  -host 0.0.0.0 -port 3457 -data /var/teploy-arcade
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
