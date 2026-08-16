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
| **Panel** | v0.30.1, systemd unit on a Debian 13 LXC, 13 GB / 4 vCPU / 100 GB |
| **Reachable** | `192.168.1.85:3457`, plain HTTP on the LAN, auth enforced |
| **Servers** | 8 migrated, 5 running: Velocity proxy + 4 Paper backends; 3 modpacks stopped |
| **Templates exercised** | velocity, paper, forge, fabric, vanilla, spigot, purpur, terraria, bedrock |
| **Templates never run** | bedrock, rust, valheim — all three now publish UDP and know their own ready banner, none has been booted since |
| **Templates** | 14 |
| **Tests** | 222 Go + a frontend guard, race-clean |
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
- [ ] **Backups are manual** — on this panel. The *feature* was never missing:
      `scheduler.go` has dispatched `!backup [note]` to `CreateBackup` since it
      was written, the Scheduler tab documents it, and a task running it
      produces a real archive with its note (verified end to end). What was
      wrong was `scheduled_backups: false` in capabilities, which since the
      dashboard started reading capabilities meant the panel advertised one of
      its own working features as "not built" on its front page. Now `true`,
      asserted against the behaviour rather than against a copy of the flag.
      What remains is genuinely operational: **no task exists on the deployed
      panel**, so there is still no schedule. The only backups are the frozen
      `pre-Crafty-decommission` set from 2026-08-14.

## 3. Known rough edges

- [x] ~~The proxy never showed anyone in its Players sidebar~~ — reported live,
      and the panel's own logs proved it. A proxy runs no world, so nothing
      ever "joins the game" in its log, and `joinRe` was the only thing feeding
      the player list. Velocity says `[connected player] Name (/ip:port) has
      connected` instead. Every player on the network connects to the proxy —
      it is the front door and the console you would naturally watch — so the
      one server that should always know who is on was the only one that never
      did. Matched on the `[connected player]` tag specifically: the same log
      carries `[server connection] Name -> Lobby has connected`, which is the
      handoff to a backend, and counting it would double every player and
      remove them again on every backend hop.

- [x] ~~Who is online was lost on every panel restart~~ — the tail replays the
      last 200 lines and rebuilds recent arrivals only, so a player who joined
      an hour before a restart vanished from the sidebar while still standing
      in the world. The panel now asks the game itself over RCON when it
      re-adopts a container. Written twice: the first parser accepted only
      vanilla's `There are 2 of a max of 20 players online: names`, and this
      fleet answers `There are 0 out of maximum 20 players online.` wrapped in
      ANSI colour with EssentialsX appending its own line — so it matched
      nothing and shipped dead until it was run against the real host. The
      test strings are now copied from that host. A reply the parser does not
      recognise leaves the console-derived list alone rather than replacing it
      with a guess, which is also what happens on the proxy: Velocity has no
      RCON at all.

- [x] ~~A backend's Players sidebar was empty for a player standing in its
      world~~ — reported live and root-caused on the deployed host. Two defects
      hiding each other: Lobby cancels the `joined the game` broadcast (a plugin
      can), so the only join evidence was `logged in with entity id`, which the
      panel did not read; and the RCON fallback refused EssentialsX's two-line
      answer, so it was dead for every case with anyone in it. Both fixed, plus
      a 60s reconcile so a missed announcement self-corrects instead of lasting
      the session. See BUGS.md.

- [x] ~~Backups had no free-space check~~ — the one path writing gigabytes with
      nothing checking there was room, on a filesystem it shares with every live
      world by design. It now refuses against the worst case the way create,
      clone and import already did, and restore estimates from the gzip trailer.

- [x] ~~Backups had no retention~~ — a scheduled backup grew unbounded: 9.2 GB
      per full round on a 100 GB disk is about ten rounds. Per-server "keep last
      N" on the Backups screen, default 0 (keep everything) so an upgrade
      deletes nobody's archives, and the prune runs only after a new backup has
      actually landed.

- [x] ~~Settings could give a server more memory than the host has~~ —
      `SetResources` validated against absolutes (512 MB to 1 TB) and nothing
      else, so the request succeeded, reported success, and was resolved by the
      OOM killer at the next restart. This is the path that broke Lobby.

- [x] ~~Server IDs came from a counter that reset each restart~~ — and named the
      container, the directory and the backups. The counter is atomic now and
      the candidate is checked against the map and the filesystem before it is
      handed out.

- [x] ~~Deleting a server orphaned its scheduled tasks~~ — they stayed in
      `tasks.json` and kept firing at a server that no longer exists, invisibly,
      because every task screen is reached through a server.

- [x] ~~`Create` bypassed the port reservation import and clone use~~ — two
      creates could both pass the check, and an in-flight import's reservation
      was invisible to both. `NextFreePort` now honours reservations too.

- [x] ~~The frontend was role-blind outside settings~~ — a viewer saw Start,
      Stop, Kill, Delete and the console input, and every one returned 403. Not
      insecure, dishonest.

- [x] ~~Login held the auth mutex through 120,000 PBKDF2 rounds~~ — the same
      mutex `Session()` takes on every authenticated request, so one sign-in
      stalled every request in flight.

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
- [x] ~~...and every server imported before that reader existed kept its
      "unknown" forever~~ — detection ran at import time and only at import
      time, so four deployed Paper servers read "paper unknown" in their own
      header while `version.json` sat in a jar on disk saying 26.1.2. Filled in
      on load now, blanks only. A proxy ships no version.json, so its manifest's
      `Implementation-Version` answers instead — which is the build a plugin
      refuses to load against, so it is the one worth showing.
- [x] ~~`javaTagFor` only understands `1.x` versions~~ — year-based releases
      (major >= 20) now take the newest known JRE explicitly, rather than
      falling through to whatever the untagged image happens to ship.
- [x] ~~Player avatars are gradient placeholders~~ — heads now load from
      mc-heads.net, drawn on top of the gradient so a head that never arrives
      leaves the panel as it was. By name rather than UUID, because the panel
      mints an offline UUID for tracked players and a UUID lookup would return
      the default skin for everybody. Off-switch in Panel settings → Appearance,
      and a banned *IP* row is never sent to the service as if it were a name.
- [ ] **Global settings are shallow.** Agent, users, audit log. No auto-update,
      autostart, or per-tab display toggles.
- [x] ~~`spike/` is not gofmt-clean~~ — formatted; `gofmt -l .` is clean
      repo-wide.

## 4. Not built

- [ ] **No UI for adding a template.** A template is one JSON file in
      `data/templates/` and can now describe a game the panel has never heard of
      - image, versions, protocols, port span, extra ports, data path, env,
      args, console, ready log. Adding Terraria took zero code, which is the
      test of whether that is real. But it takes SSH and a text editor, so it is
      a capability the operator has and a *user* of the panel does not.
      Deliberately deferred 2026-08-15 after being offered.
- [ ] **Backups have no per-template excludes.** Steam-based games (Palworld,
      Project Zomboid, Rust, 7 Days to Die, CS2) download the game itself into
      the same directory as the saves, so a backup would archive 20 GB of game
      install alongside a 200 MB world - and with the free-space guard and
      retention now in place, that turns one backup into a disk event. This
      blocks any Steam-based template being honest, and is why none was added.
      Deliberately deferred 2026-08-15 after being offered.
- [ ] **A first start on a missing image looks like a hang.** Start backgrounds
      a `docker pull` and then runs `docker run`, which pulls again itself and
      blocks - for minutes on a multi-GB image - while the panel line says only
      that a pull is happening. No progress, no size, no estimate.

- [x] ~~Clone a server~~ — `POST /api/clone`, reached from the wizard's Clone
      Existing tab and the Servers page link, both of which used to toast "not
      built yet". It reuses import's copy, progress job, port claim and
      free-space refusal rather than growing a second set, and adds the part
      import does not need: what must **not** come across. `session.lock`
      (a lock describing another process), logs and crash reports, the RCON
      credentials the image generates per container, and the `crafty_managed`
      /`.pterodactyl` markers — copied, those turn "another panel manages this
      directory" into a warning about a panel that has never seen the clone.
      A running source is quiesced exactly as a backup quiesces it, so the
      copy is not read mid-write. The clone inherits the source's image and
      launch jar rather than re-matching by version, which is what stops a
      cloned modpack landing on a loader its mods were not built against.
- [x] ~~Disk quotas~~ — as far as this host allows, and no further. The stated
      fix was XFS project quotas; the box is **ext4 inside an LXC**, where they
      do not exist, so that route was never available. What the panel can
      promise, it now does: a create is refused when the disk cannot physically
      hold the template's allowance, and every server shows real usage against
      its allowance (amber at 90%, and past 100% the figure goes amber too).
      The refusal is on **free space, not commitment** — the deployed host has
      87 GB committed of 99 GB while using 25 GB, so a commitment rule would
      have refused a 15 GB Forge server with 74 GB free. Over-commitment is
      instead a warning in the create wizard, beside the existing CPU and
      memory ones, and the Create button disables itself with the numbers when
      the agent would refuse. `disk_quota` stays `false` in capabilities: there
      is still no filesystem-level enforcement, and saying otherwise would be
      the same lie in the other direction.
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

- [x] ~~Bedrock has never been run~~ — booted on the host 2026-08-15. Two
      results: `Server started.` is confirmed as its ready line, and it found a
      real defect. The panel sets SERVER_PORT while the image's IPv6 listener
      defaults to 19133 regardless, so the *second* Bedrock server on a host -
      which NextFreePort puts on 19133 - collided with itself inside the
      container and exited with "Port [19133] may be in use by another process".
      Fixed by letting a template add env on the itzg path too.
- [ ] **CS2's ready line is a guess.** Every other `ready_log` in this repo was
      copied from a real boot; CS2's was not, because booting it downloads about
      30 GB. If it is wrong the server runs correctly and the panel shows
      "starting" forever - annoying rather than dangerous, and a one-line fix
      the first time anybody boots one.
- [ ] **Four templates have never been run**: rust, valheim, palworld, cs2. Palworld cannot be, on this host - it asks for 16 GB and the panel
      correctly refuses it on 13. Rust and Valheim can now, the host is x86_64. Vanilla,
      spigot and purpur now pass end to end. The other three were exercised far
      enough to prove the panel could not have run them: Bedrock's only offered
      version 404s on Mojang's CDN, no template published UDP at all, and
      `rcon-cli` was hardcoded but does not exist in the Bedrock image. All
      three are fixed and none has been booted since — Rust and Valheim cannot
      be, on this host: both images are amd64-only.
- [ ] **Never tested with more than one panel user.** Roles exist and are
      enforced per route; no session has ever overlapped another.
- [ ] **No load test.** Four Paper servers idle. Nothing is known about the
      panel under a busy console, many viewers, or a large file listing.
- [ ] **Never run behind a reverse proxy in anger.** The WebSocket origin check
      has a test behind a real proxy, but the deployed panel is direct. One
      concrete hazard was found and fixed unprompted: the console socket sent
      nothing in either direction while a server was quiet, so any proxy with an
      idle timeout - 60s is a common default - would close it. It now pings
      every 25s. The rest of the proxy path is still untested.

## 7. Deferred on purpose

Recorded so they are not rediscovered as oversights.

- **Multi-node.** Single-host is the product's wedge. Multi-node is where the
  heavier panels already live.

- **Postgres as the store.** `PLAN.md` §3 named it the v1 default. JSON files
  are the right answer for a single host: no accessory to run, no migration to
  get wrong, and the whole state is greppable when something goes strange. The
  accessory block in `teploy.yml` stays as the sketch if that ever changes.

- **Porting the panel to Neutron-TS.** Checked against the siblings rather than
  assumed: **dash** is vanilla JS + Alpine embedded in its Go binary, the same
  shape as this; **observe** and **ship** are Neutron apps embedded as `dist`.
  So arcade is not the odd one out, and its selling point — one static binary,
  no build step, one Go dependency — is bought with exactly this decision. The
  trigger that should reverse it: a shared Teploy UI kit landing in Neutron
  that dash and observe both consume. Then arcade follows, once, with them.

- **Plugin catalogue.** Installing from a URL already works. Browsing an index
  adds a network dependency and a licensing question for a step that is one
  copy-paste today.
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
- [x] ~~`Template.DiskGB` is decoration~~ — it is now the number the create
      check reads and the number each server's disk usage is shown against. See
      §4 for what is and is not enforced, and why a hard quota is not available
      on this host.

### 8f. Not a problem, checked and clear

Recorded so a future pass does not spend time re-deriving them:

- Every endpoint the UI calls exists on the Go side; no drift in either
  direction.
- The `_ = c.Write(...)` results on the console socket are deliberately
  discarded — best-effort writes to a socket that may already be gone.
- The bare `catch {}` blocks in the frontend are all on paths where the
  metrics feed re-renders a moment later, and each carries a comment saying so.
