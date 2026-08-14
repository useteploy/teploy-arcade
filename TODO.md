# TODO — what is not done

Everything known to be missing, rough, or deferred, in one place. Written at
v0.6.2, after the panel was deployed and a live 8-server network was migrated
onto it from another panel.

`BUGS.md` is the record of what was found and fixed. This is the record of what
has not been.

---

## Where things stand

| | |
|---|---|
| **Panel** | v0.6.2, systemd unit on a Debian 13 container, 12 GB / 4 vCPU / 50 GB |
| **Reachable** | `192.168.1.85:3457`, plain HTTP on the LAN, auth enforced |
| **Servers** | 8 migrated, 5 running: Velocity proxy + 4 Paper backends; 3 modpacks stopped |
| **Templates exercised** | velocity, paper, forge, fabric |
| **Templates never run** | vanilla, spigot, purpur, bedrock, rust, valheim |
| **Tests** | 175, race-clean |
| **Repo** | `github.com/useteploy/teploy-arcade` (private). Forgejo not yet created. |

Proven end to end: import from another panel, container lifecycle, detached
containers surviving a panel restart, console streaming, RCON commands,
backups of a live server, a real player connecting through the proxy and
hopping between backends.

---

## 1. Do these before decommissioning the old panel

The old installation is still the only copy of anything that is not backed up,
and it costs nothing sitting stopped.

- [ ] **Get the backups off the box.** All 8 servers have one backup (9.2 GB),
      but they sit on the same disk as the servers they protect. A disk failure
      takes both. Nothing about this is done until a copy exists elsewhere.
- [ ] **Verify a restore.** A backup that has never been restored is a
      hypothesis. Restore the smallest idle server end to end and confirm the
      world comes back.
- [ ] **Play each modpack once.** Pixelmon and RL Craft booted and loaded their
      mods, but no client has ever connected to them. Forge modpacks fail in
      ways that only appear with a player in the world.
- [x] ~~Confirm a real client can connect and hop between backends~~ — done.

## 2. Deployment debt

- [ ] **The container is on DHCP.** `192.168.1.85` is a lease. Every reference —
      the proxy's backend addresses, any DNS, bookmarks — breaks when it moves.
      Set a reservation or a static address.
- [ ] **No TLS.** The panel is plain HTTP. Fine on a trusted LAN, not fine the
      moment it is reachable from anywhere else. `teploy.yml` documents the
      Caddy + `tls: internal` path for a LAN hostname.
- [ ] **No off-LAN access.** Reaching the panel remotely means Tailscale on the
      container (needs an auth key) or a proxy in front.
- [ ] **Reclaim the old panel's memory.** Its container still reserves 14 GB
      while idle, on a host with 15.9 GB.
- [ ] **No monitoring.** Nothing watches whether the panel or the servers are
      up. The panel restarts on failure (`Restart=always`); nothing tells you it
      did.
- [ ] **Backups are manual.** `scheduled_backups` is declared `false` in
      capabilities. The scheduler can run `!backup` on a timer — no task exists
      yet, so in practice there is no backup schedule.

## 3. Known rough edges

Each of these is understood; none is fixed.

- [ ] **No first-run screen.** A fresh panel lands on an empty server list, and
      the one thing that must happen — claiming the panel — is buried in
      Settings, with its token in a log nobody would think to read. This caused
      real confusion during setup. It should take over the page when
      `needs_setup` is true.
- [ ] **No panel identity in the header.** Two instances on the same port are
      indistinguishable on screen. This cost a long debugging detour when a
      stray local panel on `localhost:3457` was mistaken for the real one.
      Show which host you are connected to.
- [ ] **Version reads "unknown" for jars that do not carry one.** `paper.jar`
      has no version in its filename, so imports record `unknown`. The exact
      version is inside the jar at `version.json` — read it. Today the JRE is
      picked correctly only because the untagged image happens to ship Java 25.
- [ ] **`javaTagFor` only understands `1.x` versions.** Minecraft moved to
      year-based versioning (`26.1.2`); anything not `1.x` falls through to the
      untagged image. Correct today by luck, not by rule.
- [ ] **Player avatars are gradient placeholders.** No head fetching.
- [ ] **Global settings are shallow.** Agent, users, audit log. No auto-update,
      autostart, or per-tab display toggles.
- [ ] **`spike/` is not gofmt-clean.** Three throwaway files from Phase 0 that
      `gofmt -l .` reports at the repo root.

## 4. Not built

- [ ] **Clone a server.** Import exists; clone does not.
- [ ] **Plugin catalogue.** Installing from a URL works. Browsing an index
      (Modrinth, Spigot) is a network dependency and a licensing question.
- [ ] **Disk quotas.** Declared per template, enforced nowhere. Needs XFS
      project quotas, and the `DiskGB` field is currently decoration.
- [ ] **Forced password change for admin-created users.** An admin sets, and
      therefore knows, another user's initial password. Needs a `must_change`
      flag, a change-password endpoint, a gate and UI. (Not needed for the first
      admin, who chooses their own.)
- [ ] **Postgres.** `PLAN.md` §3 named it the v1 default; state is still JSON
      files. Fine for one host; the accessory block in `teploy.yml` is the
      sketch of the swap.
- [ ] **Neutron-TS panel.** `PLAN.md` records the vanilla-JS panel as a
      deliberate deviation, and the port as still owed.

## 5. Blocked on another product

- [ ] **`teploy` cannot deploy this app.** Its `volumes:` maps a *name* to a
      container path and binds under `/deployments/<app>/volumes/<name>`, with
      keys validated against `^[a-z0-9][a-z0-9-]*[a-z0-9]$`. There is no way to
      express `/var/run/docker.sock` and no raw-docker-options escape hatch, so
      anything that talks to the Docker socket is undeployable by teploy —
      dashboards, CI runners, monitoring agents, this. Host bind mounts in
      teploy would close it. Until then, `DEPLOY.md` documents the systemd path
      that is actually in use, and `teploy.yml` is kept as the target shape
      (it validates).

## 6. Testing gaps

- [ ] **Six templates have never been run**: vanilla, spigot, purpur, bedrock,
      rust, valheim. Bedrock and Rust in particular use different images, ports
      and protocols, and the proxy/`/data` mount-path bug proved that per-image
      assumptions are exactly where this breaks.
- [ ] **Never tested with more than one panel user.** Roles exist and are
      enforced per route; no session has ever overlapped another.
- [ ] **No load test.** Four Paper servers idle. Nothing is known about the
      panel under a busy console, many viewers, or a large file listing.
- [ ] **Never run behind a reverse proxy in anger.** The WebSocket origin check
      has a test behind a real proxy, but the deployed panel is direct.

## 7. Deferred on purpose

Recorded so they are not rediscovered as oversights.

- **Multi-node.** Single-host is the product's wedge. Multi-node is where the
  heavier panels already live.
- **`kill` over MCP.** SIGKILL loses unsaved chunks; it stays a human decision.
- **Access-changing console commands over MCP** (`op`, `ban`, `whitelist`,
  `stop`). Console output is written by players and plugins, so a chat line
  asking for op must not be able to travel back out as an op command.
- **The `mcss_server_config.json` filename** remains in the import marker table
  as a file signature, beside `crafty_managed.txt` and `.pterodactyl`. The
  product name it belongs to appears nowhere. Removing it costs the "another
  control panel manages this directory" warning, which has caught real
  double-management.
- **L5's TOCTOU** is closed, not mitigated: file and plugin operations run
  against an `os.Root`, so confinement is enforced per path component by the
  syscall that performs the operation.
