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
| **Panel** | v0.12.0, systemd unit on a Debian 13 LXC, 13 GB / 4 vCPU / 100 GB |
| **Reachable** | `192.168.1.85:3457`, plain HTTP on the LAN, auth enforced |
| **Servers** | 8 migrated, 5 running: Velocity proxy + 4 Paper backends; 3 modpacks stopped |
| **Templates exercised** | velocity, paper, forge, fabric |
| **Templates never run** | vanilla, spigot, purpur, bedrock, rust, valheim |
| **Tests** | 186 Go + a frontend guard, race-clean |
| **Repo** | `Tyler/teploy-arcade` on Forgejo (`origin`) + `useteploy/teploy-arcade` on GitHub (`github`), both private |

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
- [x] ~~Reclaim the old panel's memory~~ — the container was decommissioned
      (fresh `vzdump` taken first, verified, restorable). ~60 GB of disk and its
      14 GB reservation released; arcade raised to 13 GB and its disk to 100 GB.
- [x] ~~Servers did not come back after a host reboot~~ — not previously
      recorded, and worse than most of what was. Containers run with `--rm` and
      no restart policy, so a reboot takes every server down; `reconcile()` only
      re-adopts containers that are *still running*, and found none. The panel
      came back reporting eight stopped servers and no reason why. The evidence
      was on the deployed host the whole time: 23 hours of uptime against 19
      hours of container uptime, the gap being how long it took a human to
      notice. `Manager.resume()` now restarts what the host took down, staggered
      15s apart, and deliberately restores the *previous state* rather than
      reading a per-server autostart flag - the last saved status already knows
      whether you stopped that server on purpose.

- [ ] **The host is over-committed on memory, and is at its ceiling now.** The
      five running servers carry limits summing to 17.5 GB on a 13 GB LXC, and
      actual usage sits at ~11.9 GB — 92%. Limits are caps rather than
      reservations, which is why the panel deliberately reports usage instead of
      commitment (`manager.go`), so this is an operations decision, not a panel
      bug: either raise the container, or lower the four 4 GB caps. It is the
      reason no sixth server was started on this box during testing.

- [ ] **No monitoring.** Nothing watches whether the panel or the servers are
      up. The panel restarts on failure (`Restart=always`); nothing tells you it
      did.
- [ ] **Backups are manual.** `scheduled_backups` is declared `false` in
      capabilities. The scheduler can run `!backup` on a timer — no task exists
      yet, so in practice there is no backup schedule.

## 3. Known rough edges

Each of these is understood; none is fixed.

- [x] ~~No first-run screen~~ — an unclaimed panel now takes over the page:
      it says it has no account, explains why the token exists and where to get
      it, and takes the username and password. Verified end to end on a scratch
      instance.
- [x] ~~No panel identity in the header~~ — the rail shows the host you are
      connected to, taken from `location.hostname` so a second instance cannot
      claim to be the first. A local instance is called out in amber, since
      that is the one mistaken for production.
- [x] ~~Version reads "unknown" for jars that do not carry one~~ — the scan
      now reads `version.json` from inside the jar, which is where Paper,
      Purpur, Spigot and vanilla all record the exact Minecraft version.
- [x] ~~`javaTagFor` only understands `1.x` versions~~ — year-based releases
      (major >= 20) now take the newest known JRE explicitly, rather than
      falling through to whatever the untagged image happens to ship.
- [ ] **Player avatars are gradient placeholders.** No head fetching.
- [ ] **Global settings are shallow.** Agent, users, audit log. No auto-update,
      autostart, or per-tab display toggles.
- [x] ~~`spike/` is not gofmt-clean~~ — formatted; `gofmt -l .` is clean
      repo-wide.

## 4. Not built

- [ ] **Clone a server.** Import exists; clone does not.
- [ ] **Plugin catalogue.** Installing from a URL works. Browsing an index
      (Modrinth, Spigot) is a network dependency and a licensing question.
- [ ] **Disk quotas.** Declared per template, enforced nowhere. Needs XFS
      project quotas, and the `DiskGB` field is currently decoration.
- [x] ~~Forced password change for admin-created users~~ — and the larger gap
      behind it: there was no way to change a password **at all**. No route, no
      field, no UI. Nobody could rotate their own, an admin who created an
      account knew its password for the life of the install, and the first
      admin's was fixed unless someone hand-edited `users.json`.
      `POST /api/users/{name}/password` now serves both cases: your own needs
      the current password, an admin setting someone else's does not and arms
      `must_change`. A flagged account is refused every route except that one -
      enforced in `require()`, not in the UI, since the point is that the
      admin's copy stops working. Changing a password drops that account's
      other sessions and keeps the caller's. Existing accounts are unflagged, so
      the upgrade locks nobody out; verified against the live panel.
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

---

## 8. Audit findings (v0.6.2)

A mechanical pass over the whole tree — cross-referencing what is defined
against what is used, rather than reading for impressions. Nothing here is
fixed; it is all recorded so it can be picked up deliberately.

### 8a. Missing UI for features that exist

- [x] ~~Per-server memory and CPU had no UI~~ — a Resources panel on each
      server's Settings page now edits both, showing the JVM heap the container
      will actually be given (reported by the agent, not recomputed).
- [x] ~~MCP tokens have no UI~~ — Settings now has an "Agent access (MCP)"
      panel: list, create and revoke. The token is rendered once in a
      selectable field with a copy button rather than a toast, since only its
      hash is stored and it cannot be recovered. Lifecycle verified end to end,
      including that a revoked token stops authenticating.
- [x] ~~`/api/capabilities` is never read by the panel's own client~~ — the
      dashboard's "Not built yet" list is now driven by what the agent
      advertises, instead of a list hand-maintained in the UI that could drift
      out of step with it.
- [ ] **Global `/api/metrics` is unused by the UI.** Per-server metrics are
      consumed; the host-wide series is not.
- [x] ~~No console search or filter~~ — a filter box in the console bar, with
      a live match count, Escape to clear, and cmd/ctrl+F bound to it. Lines are
      hidden rather than dropped, and output arriving while a filter is active
      respects it. The browser's own find cannot do this: the stream is a
      bounded ring, so anything scrolled out is not in the DOM.

### 8b. Accessibility — currently none

- [x] ~~Zero `aria-` attributes~~ — landmarks on the rail, tab strip and main
      region, and toasts are a polite live region so they are announced.
- [x] ~~Zero `:focus-visible` rules~~ — focus rings on every control, the
      console scroll region, and a skip link (the icon rail is first in tab
      order on every page, so without one a keyboard user walks the whole rail
      to reach content). `:focus-visible` not `:focus`, so a mouse click does
      not leave a ring behind — which is why the blunt version gets deleted.
- [x] ~~`<button>` elements with no `type`~~ — 56 given `type="button"`; the
      login form's submit button kept `type="submit"` deliberately.

### 8c. Responsive — currently none

- [x] ~~Zero media queries~~ — three breakpoints in a new `responsive.css`,
      measured rather than eyeballed: a CDP script loaded every route at 360,
      390, 768, 1024 and 1280 and reported any element sticking out past its
      container, ignoring the strips that are meant to scroll. Below 1180 the
      card and template grids halve; below 900 the wizard's Configure column,
      the players list and the settings rows stack, and the four stat blocks
      wrap; below 640 the 84px icon rail becomes a top strip, gutters drop to
      13px, dialogs go full-bleed, and the command footer takes its own row.
      Wide screens are provably untouched — every rule is inside a `max-width`
      query, and 1280 measures identically to before.
      Two things that cost time and are worth keeping: **a media query adds no
      specificity**, so the first version of this — written into `styles.css` —
      lost every override to `app.css`, which loads after it, and did nothing
      at all while looking correct. `responsive.css` therefore loads last, and
      `test/routing.test.js` now fails if it stops being last. And **inline
      styles cannot be overridden**, so the wizard's two-column grid and the
      dashboard table both had to move out of `style="..."` into a class
      before a breakpoint could reach them.

### 8d. Styling consistency

- [ ] **63 distinct `padding` declarations, on no scale.** 9px, 10px, 11px,
      12px, 13px, 14px all appear, chosen ad hoc. A small spacing scale
      (4/8/12/16) and tokens would collapse most of them and is the single
      biggest source of the "not quite even" feel.
- [ ] **119 distinct hex colours in CSS**, well beyond the token set at the top
      of `styles.css`, plus **18 more hardcoded inline in JS**. Those inline
      ones cannot follow a theme, which undercuts the theme switcher.
- [ ] **175 inline `style="..."` attributes across the JS views.** They bypass
      the stylesheet entirely, so they are invisible to theming and to any
      future spacing scale.

### 8e. Dead code

- [x] ~~14 dead CSS definitions~~ — all removed.
- [ ] **`Template.DiskGB` is decoration.** It is stored, summed into the
      "committed" figure and shown, but never enforced anywhere — the same gap
      as the `disk_quota: false` capability, from the other end.

### 8f. Not a problem, checked and clear

Recorded so a future pass does not spend time re-deriving them:

- Every endpoint the UI calls exists on the Go side; no drift in either
  direction.
- The `_ = c.Write(...)` results on the console socket are deliberately
  discarded — best-effort writes to a socket that may already be gone.
- The bare `catch {}` blocks in the frontend are all on paths where the
  metrics feed re-renders a moment later, and each carries a comment saying so.
