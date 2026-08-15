/* Teploy Arcade panel
 *
 * Views: servers, console, settings, dashboard, templates, host settings.
 * Live data: SSE (/api/events) for the list, WebSocket (/ws/console) per server.
 * No framework, no build step - this maps 1:1 onto the Neutron-TS components
 * formalised in Phase 3.
 */

// ------------------------------------------------------------------ helpers

const $ = (sel, el = document) => el.querySelector(sel);
const h = (html) => { const t = document.createElement('template'); t.innerHTML = html.trim(); return t.content.firstElementChild; };
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

function fmtUptime(sec) {
  if (!sec) return '-';
  const d = Math.floor(sec / 86400), hh = Math.floor((sec % 86400) / 3600);
  const mm = Math.floor((sec % 3600) / 60), ss = sec % 60;
  if (d) return `${d}d ${hh}h`;
  if (hh) return `${hh}h ${mm}m`;
  if (mm) return `${mm}m ${ss}s`;
  return `${ss}s`;
}

function fmtMB(mb) { return mb >= 1024 ? (mb / 1024).toFixed(1) + ' GB' : mb + ' MB'; }

// ------------------------------------------------------------ player heads

// Avatars were gradient placeholders derived from the name. The gradient is
// kept and is still what you see first: it is the background the head is drawn
// on, so a head that never arrives leaves the panel exactly as it was rather
// than a broken-image icon in every row.
//
// Heads come from an external service, which is why this is a switch and why
// the switch says so. The panel itself never makes the request - the browser
// does - so a panel with no route to the internet is unaffected, and no player
// name leaves the LAN unless an operator turns this on.
//
// By name, not by UUID, deliberately: the panel mints an offline UUID for
// tracked players, so a UUID lookup would return the default skin for
// everybody. A name that has no premium account still returns a default head,
// which is the correct answer rather than a failure.
const HEADS_KEY = 'arcade.heads';
const HEADS_URL = 'https://mc-heads.net/avatar';

function headsEnabled() { return localStorage.getItem(HEADS_KEY) !== 'off'; }

function skinHue(name) {
  return [...name].reduce((a, c) => a + c.charCodeAt(0), 0) % 360;
}

// skinPlaceholder is the gradient on its own, for a row that is not a player -
// a banned IP has no head and must never be sent to the head service as if it
// were a name.
function skinPlaceholder(seed, cls) {
  const hue = skinHue(seed);
  return `<span class="${cls || 'skin'}" style="background:linear-gradient(150deg,hsl(${hue} 32% 46%),hsl(${hue} 32% 28%))"></span>`;
}

function skinMarkup(name, cls) {
  const hue = skinHue(name);
  const bg = `background:linear-gradient(150deg,hsl(${hue} 32% 46%),hsl(${hue} 32% 28%))`;
  const span = `<span class="${cls || 'skin'}" style="${bg}">`;
  if (!headsEnabled()) return `${span}</span>`;
  // onerror removes the img rather than swapping a placeholder in: the gradient
  // underneath is already the placeholder.
  return `${span}<img src="${HEADS_URL}/${encodeURIComponent(name)}/32" alt="" loading="lazy" onerror="this.remove()"></span>`;
}

// "paper unknown" was the header on four deployed servers. The word is a
// non-answer printed where a fact goes, and it reads as a broken server rather
// than as a version the panel could not determine. The software name alone is
// true and complete; the version is added when there is one.
function softwareLabel(s) {
  const v = (s.version || '').trim();
  if (!v || v.toLowerCase() === 'unknown') return esc(s.template);
  return `${esc(s.template)} ${esc(v)}`;
}

const STATUS_LABEL = {
  running: 'Online', stopped: 'Offline', starting: 'Starting…',
  stopping: 'Stopping…', failed: 'Failed',
};
const STATUS_CLASS = {
  running: 'on', stopped: 'off', starting: 'starting',
  stopping: 'starting', failed: 'off',
};

async function api(path, opts = {}) {
  const res = await fetch(path, {
    headers: { 'Content-Type': 'application/json' },
    ...opts,
  });
  let body = null;
  try { body = await res.json(); } catch { /* no body */ }
  if (!res.ok) throw new Error((body && body.error) || `${res.status} ${res.statusText}`);
  return body;
}

function toast(msg, kind = '') {
  const icon = kind === 'err' ? 'warning' : kind === 'warn' ? 'warning' : 'check';
  const el = h(`<div class="toast ${kind}"><i class="ico ico-sm ico-${icon}"></i><span>${esc(msg)}</span></div>`);
  $('#toasts').appendChild(el);
  setTimeout(() => { el.style.opacity = '0'; setTimeout(() => el.remove(), 220); }, kind === 'err' ? 6000 : 3200);
}

// --------------------------------------------------------------- app state

const state = {
  servers: [],
  host: null,
  templates: [],
  route: { name: 'servers', id: null },
  console: null, // live console controller
};

const serverById = (id) => state.servers.find((s) => s.id === id);

// -------------------------------------------------------------- shell bits

// Which rail item lights up for a route. Kept as a pure function, and named in
// test/routing.test.js, which extracts this exact source and runs it - a test
// holding its own copy of these rules passes forever against the old ones.
function railActiveFor(nav, route) {
  // Anything scoped to a server belongs under Servers, including its own
  // dashboard - otherwise opening a server's graphs moves the rail selection.
  if (route.id) return nav === 'servers';
  // `#/host` is the old URL for panel settings; keep it lighting the same item.
  // `#/import` is reached from the Servers page and lands back on a server, so
  // it stays under Servers rather than blanking the rail mid-flow.
  const name = route.name === 'host' ? 'settings'
    : route.name === 'import' ? 'servers' : route.name;
  return nav === name;
}

// The panel reports 0 for a capacity it could not measure. Rendering that as
// "/ 0 GB" with a full red bar invents a crisis; rendering it as unknown says
// what is actually true. Every consumer of a total goes through here.
const capKnown = (total) => typeof total === 'number' && total > 0;
const capPct = (used, total) => capKnown(total) ? Math.min(100, used / total * 100) : 0;
const capOver = (used, total) => capKnown(total) && used > total;

function renderRail() {
  document.querySelectorAll('.rail-item').forEach((el) => {
    el.classList.toggle('is-active', !!railActiveFor(el.dataset.nav, state.route));
  });
  // Name the panel you are connected to. location.host is the honest source:
  // it is what the browser actually reached, so a second instance cannot claim
  // to be the first.
  const rh = $('#railHost');
  if (rh) {
    const h = location.hostname === 'localhost' || location.hostname === '127.0.0.1'
      ? 'local' : location.hostname;
    rh.textContent = h;
    rh.title = `Connected to ${location.host}`;
    rh.classList.toggle('is-local', h === 'local');
  }

  if (state.host) {
    $('#railVer').textContent =
      state.host.agent.version === 'dev' ? 'dev' : 'v' + state.host.agent.version;

    // A coloured square with no label is a riddle. Say what it is and what it
    // means, and link somewhere that explains further.
    const dockerOK = state.host.docker;
    const d = $('#dockerStat');
    d.querySelector('.dot').className = 'dot ' + (dockerOK ? 'dot-on' : '');
    d.classList.toggle('is-off', !dockerOK);
    d.title = dockerOK
      ? 'Docker is reachable — servers can use the container runtime'
      : 'Docker is not reachable — servers can only use the simulator runtime';
  }
}

let tabDragging = false;

function renderTabstrip() {
  const strip = $('#tabstrip');
  const cur = state.route.id;

  // The metrics feed redraws this every two seconds. Rebuilding the strip
  // mid-drag destroys the element under the pointer, which cancels the drag and
  // drops the indicator - the whole interaction felt unreliable for that reason
  // alone. Defer the redraw; dragend renders once at the end.
  if (tabDragging) return;

  // Every server gets a tab. This used to cap at five with a "+3" that was a
  // bare <span> - so with more servers than the cap, the ones past it were not
  // reachable from the strip at all and you had to go via the Servers list.
  // The strip scrolls instead; the point of it is that every server is one
  // click away.
  const open = state.servers;
  if (!open.length) { strip.innerHTML = ''; return; }

  strip.innerHTML = open.map((s) => {
    const cls = STATUS_CLASS[s.status] || 'off';
    const pc = s.players ? `<span class="pc">${s.players.online}/${s.players.max}</span>` : '';
    return `<a class="stab ${s.id === cur ? 'is-active' : ''}" href="#/s/${s.id}/console"
        draggable="true" data-sid="${esc(s.id)}" title="${esc(s.name)}">
        <span class="gm gm-xs gm-${esc(s.mark)}"></span>
        <span class="nm">${esc(s.name)}</span>
        ${pc}
        <span class="st ${cls}"><span class="dot dot-${cls === 'on' ? 'on' : cls === 'starting' ? 'starting' : 'off'}"></span> ${STATUS_LABEL[s.status]}</span>
      </a>`;
  }).join('');

  wireTabDrag(strip);
  // Keep the server you are looking at in view when the strip scrolls.
  const active = strip.querySelector('.stab.is-active');
  if (active) active.scrollIntoView({ block: 'nearest', inline: 'nearest' });
  markTabOverflow(strip);
  strip.onscroll = () => markTabOverflow(strip);
}

// Fade the right edge only while there is something past it, so the strip says
// "there is more" without spending height on a permanent affordance.
function markTabOverflow(strip) {
  const wrap = $('#tabstripWrap');
  if (!wrap) return;
  const more = strip.scrollWidth - strip.clientWidth - strip.scrollLeft > 4;
  wrap.classList.toggle('has-overflow', more);
}

// Drag to reorder. The order is the operator's, and it persists: the tab strip
// is how you move between servers, so being unable to put the one you watch
// all day first is a real limitation once there are more than a handful.
function wireTabDrag(strip) {
  let dragId = null;

  strip.querySelectorAll('.stab').forEach((el) => {
    el.addEventListener('dragstart', (e) => {
      dragId = el.dataset.sid;
      tabDragging = true;
      el.classList.add('is-dragging');
      e.dataTransfer.effectAllowed = 'move';
      // Firefox needs data set or the drag never starts.
      e.dataTransfer.setData('text/plain', dragId);
    });

    el.addEventListener('dragend', () => {
      el.classList.remove('is-dragging');
      strip.querySelectorAll('.stab').forEach((x) => x.classList.remove('drop-before', 'drop-after'));
      dragId = null;
      tabDragging = false;
      renderTabstrip();
    });

    el.addEventListener('dragover', (e) => {
      if (!dragId || el.dataset.sid === dragId) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = 'move';
      // Which half you are over decides which side it lands on.
      const box = el.getBoundingClientRect();
      const after = e.clientX > box.left + box.width / 2;
      el.classList.toggle('drop-after', after);
      el.classList.toggle('drop-before', !after);
    });

    el.addEventListener('dragleave', () => el.classList.remove('drop-before', 'drop-after'));

    el.addEventListener('drop', async (e) => {
      e.preventDefault();
      e.stopPropagation();
      if (!dragId || el.dataset.sid === dragId) return;
      const box = el.getBoundingClientRect();
      const after = e.clientX > box.left + box.width / 2;

      const ids = state.servers.map((s) => s.id);
      const from = ids.indexOf(dragId);
      if (from < 0) return;
      ids.splice(from, 1);
      let to = ids.indexOf(el.dataset.sid);
      if (to < 0) return;
      if (after) to += 1;
      ids.splice(to, 0, dragId);

      // Reorder locally first so the strip does not jump while the request is
      // in flight, then let the server's answer be authoritative.
      const byId = Object.fromEntries(state.servers.map((s) => [s.id, s]));
      state.servers = ids.map((id) => byId[id]).filter(Boolean);
      tabDragging = false;
      renderTabstrip();

      try {
        const res = await api('/api/servers/order', {
          method: 'POST',
          body: JSON.stringify({ order: ids }),
        });
        if (res && res.servers) state.servers = res.servers;
      } catch (err) {
        toast(err.message, 'err');
      }
      renderTabstrip();
    });
  });
}

// server header shared by console + settings views
function serverHeader(s, tab) {
  const cls = STATUS_CLASS[s.status] || 'off';
  const running = s.status === 'running';
  const busy = s.status === 'starting' || s.status === 'stopping';

  const actions = running || busy
    ? `<button type="button" class="btn btn-stop" data-act="stop" ${busy ? 'disabled' : ''}><i class="ico ico-sm ico-stop"></i> Stop</button>
       <button type="button" class="btn btn-kill" data-act="kill"><i class="ico ico-sm ico-power"></i> Kill</button>
       <button type="button" class="btn btn-restart" data-act="restart" ${busy ? 'disabled' : ''}><i class="ico ico-sm ico-restart"></i> Restart</button>`
    : `<button type="button" class="btn btn-start" data-act="start"><i class="ico ico-sm ico-play"></i> Start</button>
       <button type="button" class="btn btn-quiet" data-act="delete"><i class="ico ico-sm ico-trash"></i> Delete</button>`;

  const meta = [
    `<span><i class="ico ico-sm ico-network"></i> <span class="mono val">${esc(s.address.host)}:${s.address.port}</span></span>`,
    `<span><i class="ico ico-sm ico-flask"></i> ${softwareLabel(s)}</span>`,
    s.players ? `<span><i class="ico ico-sm ico-users"></i> <span class="val">${s.players.online}/${s.players.max}</span> players</span>` : '',
    running ? `<span><i class="ico ico-sm ico-clock"></i> up <span class="val">${fmtUptime(s.uptime)}</span></span>` : '',
    running ? `<span><i class="ico ico-sm ico-cpu"></i> <span class="val">${s.cpu.percent}%</span> of ${s.cpu.limit_vcpu} vCPU</span>` : '',
    running ? `<span><i class="ico ico-sm ico-memory"></i> <span class="val">${fmtMB(s.memory.used_mb)}</span> / ${fmtMB(s.memory.limit_mb)}</span>` : '',
    s.last_exit ? `<span style="color:var(--offline)"><i class="ico ico-sm ico-warning"></i> exit ${s.last_exit.code} &middot; ${esc(s.last_exit.reason)} &middot; ${s.last_exit.restart_count} restarts</span>` : '',
    s.last_exit && s.last_exit.circuit_open
      ? `<span class="pill-warn">Stopped retrying &mdash; <a data-act="clear-failures" style="text-decoration:underline;cursor:pointer">clear failures</a></span>` : '',
    `<span><i class="ico ico-sm ico-tag"></i> <span class="motd">${esc(s.motd)}</span></span>`,
  ].filter(Boolean).join('');

  const t = (name, label, soon) =>
    `<a class="tab ${tab === name ? 'is-active' : ''} ${soon ? 'is-soon' : ''}" ${soon ? '' : `href="#/s/${s.id}/${name}"`}>${label}</a>`;

  return `
    <div class="srv-head">
      <div class="srv-id">
        <span class="gm gm-lg gm-${esc(s.mark)}"></span>
        <div><h1 class="srv-name">${esc(s.name)}</h1></div>
        <span class="srv-state ${cls}"><span class="dot dot-${cls === 'on' ? 'on' : cls === 'starting' ? 'starting' : 'off'}"></span> ${STATUS_LABEL[s.status]}</span>
        <span class="badge-mute" style="margin-left:4px">${esc(s.runtime)}</span>
        <div class="srv-actions">${actions}</div>
      </div>
      <div class="srv-meta">${meta}</div>
      <div class="tabs">
        ${t('dashboard', 'Dashboard')}
        ${t('console', 'Console')}
        ${t('players', 'Players')}
        ${t('files', 'Files')}
        ${t('backups', 'Backups')}
        ${t('scheduler', 'Scheduler')}
        ${t('plugins', 'Plugins')}
        ${t('settings', 'Settings')}
        <div class="spacer"></div>
        <div class="tabs-icon folder" title="Server folder"><i class="ico ico-folder"></i></div>
        <div class="tabs-icon" title="Options"><i class="ico ico-gear"></i></div>
      </div>
    </div>`;
}

function wireHeaderActions(root, id) {
  root.querySelectorAll('[data-act]').forEach((btn) => {
    btn.addEventListener('click', async () => {
      const act = btn.dataset.act;
      if (act === 'delete') {
        if (!confirm('Delete this server? Its state is removed from the panel.')) return;
        try { await api(`/api/servers/${id}`, { method: 'DELETE' }); toast('Server deleted'); location.hash = '#/servers'; }
        catch (e) { toast(e.message, 'err'); }
        return;
      }
      if (act === 'kill' && !confirm('Kill sends SIGKILL. The game will not save first and unsaved chunks are lost.\n\nStop instead unless it is unresponsive.')) return;
      btn.disabled = true;
      try { await api(`/api/servers/${id}/${act}`, { method: 'POST' }); }
      catch (e) { toast(e.message, 'err'); btn.disabled = false; }
    });
  });
}

// ------------------------------------------------------------ servers view

// Host tiles, shared by the Servers page and the Dashboard.
//
// These were duplicated and drifted: one page was corrected to show measured
// usage while the other kept summing every server's configured limit, so the
// same host read as "7% CPU" on one screen and "22 / 4 vCPU" in red on the
// other. Limits are caps, not reservations - the sum only matters if every
// server runs at once, which is a planning question, not a live one.
function hostTiles(host) {
  if (!host) return '';
  const bar = (used, total) => capKnown(total)
    ? `<div class="bar"><i class="${used / total > 0.9 ? 'warn' : ''}" style="width:${capPct(used, total)}%"></i></div>` : '';
  const cpuPct = host.cpu.used_percent != null ? host.cpu.used_percent : null;

  return `<div class="tiles">
    <div class="tile">
      <div class="k"><i class="ico ico-sm ico-layers"></i> Servers</div>
      <div class="v">${host.running}<small> / ${host.servers} running</small></div>
    </div>
    <div class="tile">
      <div class="k"><i class="ico ico-sm ico-cpu"></i> Host CPU</div>
      <div class="v">${cpuPct == null ? '-' : cpuPct + '<small> %</small>'}</div>
      ${cpuPct == null ? '' : bar(cpuPct, 100)}
    </div>
    <div class="tile">
      <div class="k"><i class="ico ico-sm ico-memory"></i> Host memory</div>
      <div class="v">${(host.memory.used_mb / 1024).toFixed(1)}<small> / ${capKnown(host.memory.total_mb) ? (host.memory.total_mb / 1024).toFixed(0) + ' GB' : 'unknown'}</small></div>
      ${bar(host.memory.used_mb, host.memory.total_mb)}
    </div>
    <div class="tile">
      <div class="k"><i class="ico ico-sm ico-box"></i> Disk</div>
      <div class="v">${host.disk.used_gb}<small> / ${capKnown(host.disk.total_gb) ? host.disk.total_gb + ' GB' : 'unknown'}</small></div>
      ${bar(host.disk.used_gb, host.disk.total_gb)}
    </div>
    <div class="tile">
      <div class="k"><i class="ico ico-sm ico-flask"></i> Runtime</div>
      <div class="v" style="font-size:15px;padding-top:4px">${host.docker ? 'Docker + simulator' : 'Simulator only'}</div>
    </div>
  </div>`;
}

function viewServers() {
  const hostBar = hostTiles(state.host);

  const cards = state.servers.map(serverCard).join('');

  const body = state.servers.length ? `<div class="cards">${cards}</div>` : `
    <div class="empty">
      <div>
        <div class="big">No servers yet</div>
        <div>Create one from a template - it takes a few seconds on the simulator runtime.</div>
        <button type="button" class="btn btn-primary" data-open-create><i class="ico ico-sm ico-plus"></i> Create new Server</button>
      </div>
    </div>`;

  const root = h(`<div class="content">
    <div class="page-head">
      <h1 class="page-title">Servers</h1>
      <div class="link-actions">
        <a data-open-create>Create new Server</a><span class="div">|</span>
        <a data-import>Import</a><span class="div">|</span>
        <a data-clone>Clone</a>
      </div>
    </div>
    ${hostBar}
    ${body}
  </div>`);

  root.querySelectorAll('[data-open-create]').forEach((a) => a.addEventListener('click', openCreate));
  root.querySelectorAll('[data-import]').forEach((a) =>
    a.addEventListener('click', () => { location.hash = '#/import'; }));
  root.querySelectorAll('[data-clone]').forEach((a) =>
    a.addEventListener('click', () => window.extraViews.openClone()));

  root.querySelectorAll('.card').forEach((card) => {
    const id = card.dataset.id;
    card.querySelectorAll('[data-act]').forEach((btn) => {
      btn.addEventListener('click', async (ev) => {
        ev.preventDefault(); ev.stopPropagation();
        const act = btn.dataset.act;
        if (act === 'console') { location.hash = `#/s/${id}/console`; return; }
        if (act === 'kill' && !confirm('Kill sends SIGKILL. Unsaved chunks are lost.')) return;
        btn.disabled = true;
        try { await api(`/api/servers/${id}/${act}`, { method: 'POST' }); }
        catch (e) { toast(e.message, 'err'); btn.disabled = false; }
      });
    });
  });

  return root;
}

function sparkline(s) {
  // Real history from the agent's sampler, not a decorative squiggle.
  // Falls back to a flat baseline when a server has never run - drawing a
  // synthetic wiggle there would be inventing data.
  const pts = (s.spark || []).filter((x) => x.cpu > 0 || x.mem_mb > 0);
  const flat = `<line x1="0" y1="31" x2="200" y2="31" stroke="#3a3a3a" stroke-width="1.6"/>`;
  if (pts.length < 2) return { cpuLine: flat, memArea: flat };

  const stepX = 200 / (pts.length - 1);
  const cpuMax = Math.max(12, ...pts.map((x) => x.cpu)) * 1.15;
  const memMax = Math.max(1, s.memory.limit_mb);
  const y = (v, max) => 32 - (v / max) * 28;

  const cpuPts = pts.map((x, i) => `${(i * stepX).toFixed(1)},${y(x.cpu, cpuMax).toFixed(1)}`).join(' ');
  const memPts = pts.map((x, i) => `${(i * stepX).toFixed(1)},${y(x.mem_mb, memMax).toFixed(1)}`);

  return {
    cpuLine: `<polyline points="${cpuPts}" fill="none" stroke="#6f6fe0" stroke-width="1.6" vector-effect="non-scaling-stroke"/>`,
    memArea: `<path d="M${memPts[0]} L${memPts.join(' L')} L200,34 L0,34 Z" fill="#2a4560"/>
       <path d="M${memPts[0]} L${memPts.join(' L')}" fill="none" stroke="#3ba0e8" stroke-width="1.6" vector-effect="non-scaling-stroke"/>`,
  };
}

function serverCard(s) {
  const cls = STATUS_CLASS[s.status] || 'off';
  const running = s.status === 'running';
  const busy = s.status === 'starting' || s.status === 'stopping';
  const { cpuLine, memArea } = sparkline(s);

  const sub = [
    `<span><i class="ico ico-flask"></i> ${softwareLabel(s)}</span>`,
    s.players ? `<span><i class="ico ico-users"></i> <span class="num">${s.players.online} / ${s.players.max}</span></span>` : `<span><i class="ico ico-port"></i> ${s.address.port}</span>`,
    running ? `<span><i class="ico ico-clock"></i> ${fmtUptime(s.uptime)}</span>` : '',
    s.last_exit ? `<span style="color:var(--offline)">exit ${s.last_exit.code} &middot; ${esc(s.last_exit.reason)}</span>` : '',
  ].filter(Boolean).join('');

  const actions = running || busy
    ? `<button type="button" class="btn btn-sm" data-act="console"><i class="ico ico-sm ico-terminal"></i> Console</button>
       <button type="button" class="btn btn-sm btn-stop" data-act="stop" ${busy ? 'disabled' : ''}><i class="ico ico-sm ico-stop"></i> Stop</button>
       <button type="button" class="btn btn-sm btn-restart btn-icon" data-act="restart" title="Restart" ${busy ? 'disabled' : ''}><i class="ico ico-sm ico-restart"></i></button>`
    : `<button type="button" class="btn btn-sm" data-act="console"><i class="ico ico-sm ico-terminal"></i> Console</button>
       <button type="button" class="btn btn-sm btn-start" data-act="start"><i class="ico ico-sm ico-play"></i> Start</button>`;

  const statusExtra = s.status === 'failed'
    ? `<div class="st off" style="display:flex;align-items:center;gap:6px"><i class="ico ico-sm ico-warning"></i> Failed</div>`
    : `<div class="st ${cls}">${STATUS_LABEL[s.status]}</div>`;

  return `<div class="card ${busy ? 'is-busy' : ''} ${s.status === 'failed' ? '' : ''}" data-id="${s.id}"
      ${s.status === 'failed' ? 'style="border-color:#5a3330"' : ''}>
    <div class="card-head">
      <span class="gm gm-${esc(s.mark)}"></span>
      <span class="t">
        <div class="nm">${esc(s.name)}</div>
        ${statusExtra}
      </span>
      <a class="card-menu" href="#/s/${s.id}/settings" title="Settings"><i class="ico ico-sm ico-dots"></i></a>
    </div>
    <div class="card-sub">${sub}</div>
    <div class="card-chart">
      <svg viewBox="0 0 200 34" preserveAspectRatio="none">${cpuLine}</svg>
      <div class="lbl"><b>${running ? s.cpu.percent : 0}</b> % CPU</div>
    </div>
    <div class="card-chart">
      <svg viewBox="0 0 200 34" preserveAspectRatio="none">${memArea}</svg>
      <div class="lbl"><b>${running ? s.memory.used_mb : 0}</b> MB RAM</div>
    </div>
    <div class="card-act">${actions}</div>
  </div>`;
}

// ------------------------------------------------------------ console view

function viewConsole(id) {
  const s = serverById(id);
  if (!s) return h(`<div class="content"><div class="empty">Server not found.</div></div>`);

  const root = h(`<div class="viewhost">
    ${serverHeader(s, 'console')}
    <div class="console-wrap">
      <div class="wsbar">
        <span class="conn down" id="connPill"><span class="dot"></span> Connecting</span>
        <span class="sep">|</span>
        <span id="replayNote">-</span>
        <span class="spacer"></span>
        <label class="consearch">
          <i class="ico ico-sm ico-search"></i>
          <input id="findInput" placeholder="Filter lines" autocomplete="off" spellcheck="false"
                 aria-label="Filter console lines">
          <span class="n" id="findCount" hidden></span>
          <span class="chip chip-x" id="findClear" hidden title="Clear filter">&times;</span>
        </label>
        <span class="chip" id="clearBtn"><i class="ico ico-sm ico-close"></i> Clear</span>
        <span class="chip" id="dlBtn"><i class="ico ico-sm ico-download"></i> Save log</span>
      </div>
      <div class="split">
        <div class="stream" id="stream"></div>
        <aside class="aside">
          <div class="aside-head"><i class="ico ico-sm ico-users"></i> Players <span class="n" id="pCount">0 / 0</span></div>
          <div class="aside-list" id="playerList"></div>
        </aside>
      </div>
      <div class="cmd">
        <div class="cmd-field" id="cmdField">
          <span class="sigil">&gt;</span>
          <input id="cmdInput" placeholder="Type a command, or 'help'" autocomplete="off" spellcheck="false">
          <span class="hint"><kbd class="hintkey">&uarr;</kbd> history</span>
        </div>
        <label class="cbx is-off" id="chatToggle"><span class="box"><i class="ico ico-check"></i></span> Chat mode</label>
        <label class="cbx is-on" id="scrollToggle"><span class="box"><i class="ico ico-check"></i></span> Auto scroll</label>
      </div>
    </div>
  </div>`);

  wireHeaderActions(root, id);
  state.console = new ConsoleController(root, id);
  return root;
}

class ConsoleController {
  constructor(root, id) {
    this.root = root;
    this.id = id;
    this.stream = $('#stream', root);
    this.input = $('#cmdInput', root);
    this.chat = false;
    this.autoscroll = true;
    this.unread = 0;
    this.history = [];
    this.histIdx = -1;
    this.seq = 0;
    this.retries = 0;
    this.closed = false;
    this.jumpEl = null;

    $('#chatToggle', root).addEventListener('click', (e) => {
      e.preventDefault();
      this.chat = !this.chat;
      $('#chatToggle', root).classList.toggle('is-on', this.chat);
      $('#chatToggle', root).classList.toggle('is-off', !this.chat);
      $('#cmdField', root).classList.toggle('chat', this.chat);
      this.input.placeholder = this.chat ? 'Say something to everyone on the server' : "Type a command, or 'help'";
      this.input.focus();
    });

    $('#scrollToggle', root).addEventListener('click', (e) => {
      e.preventDefault();
      this.setAutoscroll(!this.autoscroll);
      if (this.autoscroll) this.toBottom();
    });

    // Filtering hides lines rather than dropping them: the console is still
    // streaming underneath, and clearing the box has to bring everything back.
    this.filter = '';
    const findInput = $('#findInput', root);
    const findCount = $('#findCount', root);
    const findClear = $('#findClear', root);

    const applyFilter = () => {
      this.filter = findInput.value.trim().toLowerCase();
      let shown = 0, total = 0;
      this.stream.querySelectorAll('.ln').forEach((ln) => {
        total++;
        const hit = !this.filter || (ln.dataset.q || '').includes(this.filter);
        ln.hidden = !hit;
        if (hit) shown++;
      });
      const on = !!this.filter;
      findCount.hidden = !on;
      findClear.hidden = !on;
      findCount.textContent = on ? `${shown} / ${total}` : '';
      this.stream.classList.toggle('is-filtered', on);
      if (on) this.toBottom();
    };

    findInput.addEventListener('input', applyFilter);
    findInput.addEventListener('keydown', (e) => {
      if (e.key === 'Escape') { findInput.value = ''; applyFilter(); findInput.blur(); }
    });
    findClear.addEventListener('click', () => { findInput.value = ''; applyFilter(); findInput.focus(); });

    // cmd/ctrl+F inside the console filters it rather than opening the
    // browser's find, which cannot see lines that scrolled out of the ring.
    root.addEventListener('keydown', (e) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'f') {
        e.preventDefault();
        findInput.focus();
        findInput.select();
      }
    });

    $('#clearBtn', root).addEventListener('click', () => {
      this.stream.innerHTML = '';
      this.sys('Console cleared in this browser. Server-side history is untouched.');
    });

    $('#dlBtn', root).addEventListener('click', () => this.download());

    // pause-on-scroll: leaving the bottom pauses, returning resumes
    this.stream.addEventListener('scroll', () => {
      const atBottom = this.stream.scrollHeight - this.stream.scrollTop - this.stream.clientHeight < 24;
      if (!atBottom && this.autoscroll) this.setAutoscroll(false);
      if (atBottom && !this.autoscroll) this.setAutoscroll(true);
    });

    this.input.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') { this.send(); e.preventDefault(); }
      else if (e.key === 'ArrowUp') {
        if (this.histIdx < this.history.length - 1) { this.histIdx++; this.input.value = this.history[this.history.length - 1 - this.histIdx]; }
        e.preventDefault();
      } else if (e.key === 'ArrowDown') {
        if (this.histIdx > 0) { this.histIdx--; this.input.value = this.history[this.history.length - 1 - this.histIdx]; }
        else { this.histIdx = -1; this.input.value = ''; }
        e.preventDefault();
      }
    });

    this.connect();
    setTimeout(() => this.input.focus(), 40);
  }

  destroy() {
    this.closed = true;
    if (this.ws) { try { this.ws.close(); } catch {} }
    clearTimeout(this.retryTimer);
  }

  setConn(kind, text) {
    const pill = $('#connPill', this.root);
    if (!pill) return;
    pill.className = `conn ${kind}`;
    pill.innerHTML = `<span class="dot"></span> ${esc(text)}`;
    const dead = kind !== 'live';
    $('#cmdField', this.root).classList.toggle('dead', dead);
    this.input.disabled = dead;
    this.input.placeholder = dead
      ? "Reconnecting - commands can't be sent right now"
      : (this.chat ? 'Say something to everyone on the server' : "Type a command, or 'help'");
  }

  connect() {
    if (this.closed) return;
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const ws = new WebSocket(`${proto}//${location.host}/ws/console?server=${encodeURIComponent(this.id)}`);
    this.ws = ws;

    ws.onopen = () => { this.retries = 0; this.setConn('live', 'Live'); };

    ws.onmessage = (ev) => {
      let m; try { m = JSON.parse(ev.data); } catch { return; }
      this.handle(m);
    };

    ws.onclose = () => {
      if (this.closed) return;
      this.retries++;
      const wait = Math.min(8000, 500 * Math.pow(2, Math.min(4, this.retries)));
      this.setConn('retry', `Reconnecting - attempt ${this.retries}`);
      this.sys(`Console socket closed. Retry ${this.retries} in ${(wait / 1000).toFixed(0)}s - the server itself is unaffected.`);
      this.retryTimer = setTimeout(() => this.connect(), wait);
    };

    ws.onerror = () => { /* close handler drives reconnect */ };
  }

  handle(m) {
    switch (m.t) {
      case 'replay': {
        this.stream.innerHTML = '';
        m.lines.forEach((l) => this.appendLine(l, true));
        if (m.count) {
          this.stream.appendChild(h(`<div class="replay">restored the last ${m.count} of ${m.buffer_capacity} buffered lines &middot; live below</div>`));
        }
        $('#replayNote', this.root).innerHTML =
          m.count ? `restored <b>${m.count}</b> lines &middot; buffer holds <b>${m.buffer_capacity}</b>` : 'no buffered history yet';
        this.seq = m.seq || 0;
        // The replay carries the server snapshot, and it is the first thing to
        // arrive - so the input is settled before anyone can type into it.
        this.applyConsoleMode(m.server);
        this.toBottom(true);
        break;
      }
      case 'line':
        this.appendLine(m.line);
        break;
      case 'dropped':
        this.gap(m.count, m.ts);
        break;
      case 'status':
        this.onStatus(m.server);
        break;
      case 'players':
        this.renderPlayers(m.players, m.max);
        break;
      case 'command_ack':
        if (!m.accepted) toast(m.error || 'Command was not accepted', 'err');
        break;
    }
  }

  // A game whose server reads commands on its own stdin cannot be driven from
  // here - containers run detached, so there is no pipe to write to. Say that
  // on the input itself rather than accepting a command and reporting that it
  // could not be delivered.
  applyConsoleMode(s) {
    if (!this.input || !s || s.console !== 'none') return;
    this.input.disabled = true;
    this.input.classList.add('is-denied');
    this.input.placeholder = `${s.template} takes commands in its own console, not through the panel`;
  }

  onStatus(s) {
    this.applyConsoleMode(s);
    const i = state.servers.findIndex((x) => x.id === s.id);
    if (i >= 0) state.servers[i] = s; else state.servers.push(s);
    renderTabstrip();
    // refresh the header without tearing down the stream
    const head = $('.srv-head', this.root);
    if (head) {
      const fresh = h(serverHeader(s, 'console'));
      head.replaceWith(fresh);
      wireHeaderActions(this.root, this.id);
    }
  }

  appendLine(l, quiet) {
    const cls = {
      warn: 'ln-warn', error: 'ln-error', ok: 'ln-ok',
    }[l.level] || '';
    const srcCls = l.source === 'player' ? 'ln-player'
      : l.source === 'command' ? 'ln-cmd'
      : l.source === 'panel' ? 'ln-sys' : '';
    const isTrace = l.level === 'error' && /^\s+(at |\.\.\.)/.test(l.text);

    const lvl = l.source === 'panel' ? '&mdash;'
      : l.source === 'command' ? 'CMD'
      : (l.level === 'ok' ? 'INFO' : l.level.toUpperCase());

    const tag = l.source === 'panel' ? '<span class="tagp">panel</span> ' : '';
    const el = h(`<div class="ln ${isTrace ? 'ln-trace' : srcCls || cls}">
        <span class="ts">${esc(l.ts)}</span><span class="lv">${lvl}</span><span>${tag}${esc(l.text)}</span>
      </div>`);

    // Store the searchable text on the element so filtering does not have to
    // re-derive it, and apply the current filter to a line arriving while a
    // filter is active - otherwise new output ignores the filter.
    el.dataset.q = (l.text + ' ' + (l.source || '')).toLowerCase();
    if (this.filter && !el.dataset.q.includes(this.filter)) el.hidden = true;

    this.stream.appendChild(el);
    this.trim();
    if (this.autoscroll) this.toBottom();
    else if (!quiet) this.bumpUnread();
  }

  sys(text) {
    this.appendLine({ ts: new Date().toTimeString().slice(0, 8), level: 'info', source: 'panel', text });
  }

  gap(count, ts) {
    const el = h(`<div class="gap">
        <i class="ico ico-sm ico-warning"></i>
        ${count} line${count === 1 ? '' : 's'} dropped
        <span class="why">&mdash; output arrived faster than this browser could take it</span>
        <span class="spacer"></span>
        <span class="mono" style="color:var(--t-2);font-weight:400">${esc(ts)}</span>
      </div>`);
    this.stream.appendChild(el);
    if (this.autoscroll) this.toBottom();
  }

  trim() {
    // keep the DOM bounded; the agent's ring buffer is the real history
    const max = 1200;
    while (this.stream.childElementCount > max) this.stream.firstElementChild.remove();
  }

  setAutoscroll(on) {
    this.autoscroll = on;
    const t = $('#scrollToggle', this.root);
    t.classList.toggle('is-on', on);
    t.classList.toggle('is-off', !on);
    if (on) { this.unread = 0; this.removeJump(); }
  }

  bumpUnread() {
    this.unread++;
    if (!this.jumpEl) {
      this.jumpEl = h(`<div class="jump"><i class="ico ico-sm ico-pause" style="color:var(--amber)"></i>
        Paused &mdash; <span class="n">0</span> new lines
        <span class="go"><i class="ico ico-sm ico-arrowdown"></i> Jump to latest</span></div>`);
      this.jumpEl.addEventListener('click', () => { this.setAutoscroll(true); this.toBottom(); });
      this.stream.parentElement.appendChild(this.jumpEl);
    }
    $('.n', this.jumpEl).textContent = this.unread;
  }

  removeJump() { if (this.jumpEl) { this.jumpEl.remove(); this.jumpEl = null; } }

  toBottom(force) {
    if (!this.autoscroll && !force) return;
    this.stream.scrollTop = this.stream.scrollHeight;
    this.unread = 0;
    this.removeJump();
  }

  renderPlayers(players, max) {
    $('#pCount', this.root).textContent = `${players.length} / ${max}`;
    const list = $('#playerList', this.root);
    if (!players.length) {
      list.innerHTML = `<div style="padding:14px 9px;color:var(--t-2);font-size:12px">Nobody online.</div>`;
      return;
    }
    list.innerHTML = players.map((p) => {
      return `<div class="player">
        ${skinMarkup(p.name)}
        <span><div class="pn">${esc(p.name)}</div><div class="pm">${p.ping_ms} ms</div></span>
        <span class="pa">
          <button type="button" class="btn btn-ghost btn-sm btn-icon" title="Kick" data-kick="${esc(p.name)}"><i class="ico ico-sm ico-close"></i></button>
        </span>
      </div>`;
    }).join('');
    list.querySelectorAll('[data-kick]').forEach((b) =>
      b.addEventListener('click', () => this.sendRaw(`kick ${b.dataset.kick}`)));
  }

  send() {
    const text = this.input.value.trim();
    if (!text) return;
    this.history.push(text);
    this.histIdx = -1;
    this.input.value = '';
    this.sendRaw(text);
  }

  sendRaw(text) {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) { toast('Not connected', 'err'); return; }
    this.ws.send(JSON.stringify({
      t: 'command', id: String(Date.now()), text,
      mode: this.chat ? 'say' : 'command', actor: 'panel',
    }));
    this.setAutoscroll(true);
    this.toBottom(true);
  }

  download() {
    const text = [...this.stream.querySelectorAll('.ln')]
      .map((el) => [...el.children].map((c) => c.textContent).join(' ')).join('\n');
    const blob = new Blob([text], { type: 'text/plain' });
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = `${this.id}-console.log`;
    a.click();
    URL.revokeObjectURL(a.href);
  }
}

// ----------------------------------------------------------- settings view

async function viewSettings(id) {
  const root = h(`<div class="viewhost"><div class="empty"><span class="spin"></span></div></div>`);
  let data;
  try { data = await api(`/api/servers/${id}/settings`); }
  catch (e) { root.innerHTML = `<div class="empty">${esc(e.message)}</div>`; return root; }

  const s = data.server;
  const changes = {};

  const ctl = (k) => {
    const meta = k;
    if (meta.type === 'bool') {
      return `<label class="cbx ${meta.value === 'true' ? 'is-on' : 'is-off'}" data-key="${esc(meta.key)}" data-type="bool">
          <span class="box"><i class="ico ico-check"></i></span> ${esc(meta.label)}
          <span class="pkey">${esc(meta.key)}</span>
          ${meta.applies === 'next_restart' ? '<span class="restart-flag" hidden><i class="ico ico-sm ico-restart"></i> Restart required</span>' : ''}
          ${meta.owner === 'panel' ? '<span class="badge-mute">panel-owned</span>' : ''}
        </label>
        ${meta.help ? `<div class="rd">${esc(meta.help)}</div>` : ''}`;
    }
    if (meta.type === 'enum') {
      return `<label style="display:flex;align-items:center;gap:8px;font-size:13px">${esc(meta.label)}
          <span class="pkey">${esc(meta.key)}</span>
          ${meta.applies === 'next_restart' ? '<span class="restart-flag" hidden><i class="ico ico-sm ico-restart"></i> Restart required</span>' : ''}
        </label>
        <div class="ctl"><select class="inp" data-key="${esc(meta.key)}" data-type="enum">
          ${meta.options.map((o) => `<option ${o === meta.value ? 'selected' : ''}>${esc(o)}</option>`).join('')}
        </select></div>
        ${meta.help ? `<div class="rd">${esc(meta.help)}</div>` : ''}`;
    }
    return `<label style="display:flex;align-items:center;gap:8px;font-size:13px">${esc(meta.label)}
        <span class="pkey">${esc(meta.key)}</span>
        ${meta.applies === 'next_restart' ? '<span class="restart-flag" hidden><i class="ico ico-sm ico-restart"></i> Restart required</span>' : ''}
        ${meta.applies === 'new_world_only' ? '<span class="restart-flag"><i class="ico ico-sm ico-warning"></i> New world only</span>' : ''}
        ${meta.owner === 'panel' ? '<span class="badge-mute">panel-owned</span>' : ''}
      </label>
      <div class="ctl"><input class="inp ${meta.type === 'int' ? 'mono' : ''}" data-key="${esc(meta.key)}" data-type="${meta.type}" value="${esc(meta.value)}"></div>
      ${meta.help ? `<div class="rd">${esc(meta.help)}</div>` : ''}`;
  };

  const groups = data.groups.map((g) => `
    <div class="group-title">${esc(g.group)}</div>
    ${g.keys.map((k) => `<div class="setrow" data-row="${esc(k.key)}">${ctl(k)}</div>`).join('')}
  `).join('');

  root.innerHTML = `
    ${serverHeader(s, 'settings')}
    <div class="content">
      <div class="settings-head">
        <div>
          <h2 class="sect-title">Server Settings</h2>
          <p class="sect-sub">You are editing the <span class="mono">server.properties</span> file.</p>
        </div>
        <div class="spacer"></div>
        <div class="link-actions"><a data-raw>Edit server.properties manually</a></div>
      </div>
      <div style="padding:0 22px 26px;max-width:900px">
        <div class="panelbox" id="resBox">
          <h3>Resources</h3>
          <div class="row">
            <span class="k">Max memory</span>
            <span class="spacer"></span>
            <input class="inp mono" id="resMem" value="${s.memory.limit_mb}" style="width:110px">
            <span class="muted">MB</span>
            <span class="muted" style="font-size:12px;margin-left:10px">JVM heap gets ${s.memory.heap_mb} MB; the rest is headroom the JVM needs outside the heap.</span>
          </div>
          <div class="row">
            <span class="k">CPU</span>
            <span class="spacer"></span>
            <input class="inp mono" id="resCpu" value="${s.cpu.limit_vcpu}" style="width:110px">
            <span class="muted">vCPU</span>
          </div>
          <div class="row">
            <span class="muted" style="font-size:12px">These are the container's own limits, applied on the next start. Changing them while the server runs does not resize it.</span>
            <span class="spacer"></span>
            <button type="button" class="btn btn-primary btn-sm" id="resSave">Save resources</button>
          </div>
        </div>
        ${groups}
        <div class="savebar" id="savebar" hidden>
          <span class="pill-warn"><i class="ico ico-sm ico-warning"></i> <span id="chCount">0 changes</span></span>
          <span class="n" id="chNote"></span>
          <span class="spacer"></span>
          <button type="button" class="btn btn-quiet" id="discardBtn">Discard</button>
          <button type="button" class="btn" id="saveBtn">Save</button>
          <button type="button" class="btn btn-primary" id="saveRestartBtn"><i class="ico ico-sm ico-restart"></i> Save and restart</button>
        </div>
      </div>
    </div>`;

  wireHeaderActions(root, id);
  root.querySelector('[data-raw]').addEventListener('click', () =>
    window.extraViews.openEditor(id, 'server.properties', () => router(true)));

  // Resource limits live on the server record, not in server.properties, so
  // they save separately from the properties save bar below.
  const resBtn = $('#resSave', root);
  if (resBtn) resBtn.addEventListener('click', async () => {
    const mem = parseInt($('#resMem', root).value, 10) || 0;
    const cpu = parseFloat($('#resCpu', root).value) || 0;
    resBtn.disabled = true;
    try {
      const res = await api(`/api/servers/${id}`, {
        method: 'PATCH',
        body: JSON.stringify({ memory_mb: mem, cpu }),
      });
      const pend = res && res.pending_restart;
      toast(pend && pend.length
        ? 'Saved - restart the server for the new limits to apply'
        : 'Resources saved');
      await refreshServers();
    } catch (e) { toast(e.message, 'err'); }
    resBtn.disabled = false;
  });

  const meta = {};
  data.groups.forEach((g) => g.keys.forEach((k) => { meta[k.key] = k; }));

  const mark = (key, val) => {
    const orig = meta[key].value;
    if (String(val) === String(orig)) delete changes[key];
    else changes[key] = String(val);

    const row = root.querySelector(`[data-row="${key}"]`);
    row.classList.toggle('changed', key in changes);
    const flag = row.querySelector('.restart-flag[hidden], .restart-flag:not([hidden])');
    if (flag && meta[key].applies === 'next_restart') flag.hidden = !(key in changes);

    const n = Object.keys(changes).length;
    const bar = $('#savebar', root);
    bar.hidden = n === 0;
    $('#chCount', root).textContent = `${n} change${n === 1 ? '' : 's'}`;
    const restartNeeded = Object.keys(changes).filter((k) => meta[k].applies === 'next_restart').map((k) => meta[k].label);
    $('#chNote', root).innerHTML = restartNeeded.length
      ? `<b>${restartNeeded.map(esc).join('</b>, <b>')}</b> take effect on the next restart.`
      : 'All changes apply immediately.';
  };

  root.querySelectorAll('.cbx[data-key]').forEach((el) => {
    el.addEventListener('click', (e) => {
      e.preventDefault();
      const on = !el.classList.contains('is-on');
      el.classList.toggle('is-on', on);
      el.classList.toggle('is-off', !on);
      mark(el.dataset.key, on);
    });
  });
  root.querySelectorAll('select[data-key], input[data-key]').forEach((el) => {
    el.addEventListener('input', () => mark(el.dataset.key, el.value));
  });

  $('#discardBtn', root).addEventListener('click', () => router(true));

  const save = async (restart) => {
    try {
      const res = await api(`/api/servers/${id}/settings`, { method: 'PATCH', body: JSON.stringify(changes) });
      toast(res.requires_restart.length
        ? `Saved. ${res.requires_restart.join(', ')} need a restart.`
        : 'Saved.');
      if (restart) { await api(`/api/servers/${id}/restart`, { method: 'POST' }); toast('Restarting…'); }
      router(true);
    } catch (e) { toast(e.message, 'err'); }
  };
  $('#saveBtn', root).addEventListener('click', () => save(false));
  $('#saveRestartBtn', root).addEventListener('click', () => save(true));

  return root;
}

// ------------------------------------------------------------ create modal

async function openCreate() {
  let data;
  try { data = await api('/api/templates'); } catch (e) { toast(e.message, 'err'); return; }
  state.templates = data.templates;

  let picked = data.templates.find((t) => t.recommended) || data.templates[0];
  let runtime = data.docker ? 'sim' : 'sim';

  const groups = [...new Set(data.templates.map((t) => t.group))];

  const modal = h(`<div class="scrim">
    <div class="modal" style="width:min(1130px,100%);height:min(706px,100%)">
      <div class="modal-bar"><span class="gm gm-xs gm-${esc(picked.mark)}" id="barMark"></span> Add Server
        <span class="spacer"></span><span class="strip-btn" id="closeX"><i class="ico ico-sm ico-close"></i></span>
      </div>
      <div class="modal-banner">
        <h2>Create a new Server</h2>
        <p>What type of server would you like to create?</p>
      </div>
      <div class="modal-tabs">
        <div class="tab is-active">Create New</div>
        <div class="tab" id="tabImport">Import Server</div>
        <div class="tab" id="tabClone">Clone Existing</div>
      </div>
      <div class="modal-body" style="padding:0;display:flex;flex-direction:column;min-height:0">
        <div class="wizgrid">
          <div style="padding:18px 20px;overflow:auto" id="catalog">
            ${groups.map((g) => `
              <div class="group-label">${esc(g)}</div>
              <div class="tpl-grid">
                ${data.templates.filter((t) => t.group === g).map((t) => `
                  <div class="tpl ${t.slug === picked.slug ? 'is-picked' : ''}" data-slug="${esc(t.slug)}">
                    <span class="gm gm-${esc(t.mark)}"></span>
                    <div>
                      <div class="tn">${esc(t.name)}
                        ${t.recommended ? '<span class="rec">(recommended for new servers)</span>' : ''}
                        ${t.maturity === 'preview' ? '<span class="badge-mute">preview</span>' : ''}
                      </div>
                      <div class="td">${esc(t.description)}</div>
                    </div>
                  </div>`).join('')}
              </div>`).join('')}
          </div>
          <aside class="wiz-side">
            <h4>Configure</h4>
            <div class="field" style="margin-bottom:12px">
              <label style="display:block;font-size:12px;color:var(--t-1);margin-bottom:6px">Server name</label>
              <input class="inp" id="fName" value="${esc(picked.name)} Server">
            </div>
            <div class="form-row" style="margin-bottom:12px">
              <div class="field">
                <label style="display:block;font-size:12px;color:var(--t-1);margin-bottom:6px">Version</label>
                <select class="inp" id="fVersion"></select>
              </div>
              <div class="field">
                <label style="display:block;font-size:12px;color:var(--t-1);margin-bottom:6px">Port</label>
                <input class="inp mono" id="fPort" value="${data.next_free_port}">
              </div>
            </div>
            <div class="form-row" style="margin-bottom:14px">
              <div class="field">
                <label style="display:block;font-size:12px;color:var(--t-1);margin-bottom:6px">Memory (MB)</label>
                <input class="inp mono" id="fMem" value="${picked.memory_mb}">
              </div>
              <div class="field">
                <label style="display:block;font-size:12px;color:var(--t-1);margin-bottom:6px">CPU (vCPU)</label>
                <input class="inp mono" id="fCpu" value="${picked.cpu}">
              </div>
            </div>

            <h4>Runtime</h4>
            <div class="seg2" style="margin-bottom:8px">
              <button type="button" id="rtSim" class="on">Simulator</button>
              <button type="button" id="rtDocker" ${data.docker ? '' : 'disabled'}>Docker</button>
            </div>
            <div class="muted" style="font-size:11.5px;line-height:1.5" id="rtNote">
              Runs in-process. Starts instantly, no image to pull - use it to drive the panel.
            </div>

            <h4 style="margin-top:18px">Host after this server</h4>
            <div id="budget" style="font-size:12px;color:var(--t-1)"></div>
          </aside>
        </div>
      </div>
      <div class="modal-foot">
        <span class="note">Nothing is created until you press Create.</span>
        <div class="spacer"></div>
        <label class="cbx is-on" id="startNow"><span class="box"><i class="ico ico-check"></i></span> Start immediately</label>
        <button type="button" class="btn btn-quiet" id="cancelBtn">Cancel</button>
        <button type="button" class="btn btn-primary" id="createBtn"><i class="ico ico-sm ico-plus"></i> Create</button>
      </div>
    </div>
  </div>`);

  $('#modalHost').appendChild(modal);
  const close = () => modal.remove();
  $('#closeX', modal).addEventListener('click', close);
  $('#cancelBtn', modal).addEventListener('click', close);
  modal.addEventListener('click', (e) => { if (e.target === modal) close(); });
  $('#tabImport', modal).addEventListener('click', () => { close(); location.hash = '#/import'; });
  $('#tabClone', modal).addEventListener('click', () => { close(); window.extraViews.openClone(); });

  const startNow = $('#startNow', modal);
  startNow.addEventListener('click', (e) => {
    e.preventDefault();
    const on = !startNow.classList.contains('is-on');
    startNow.classList.toggle('is-on', on); startNow.classList.toggle('is-off', !on);
  });

  const setRuntime = (rt) => {
    runtime = rt;
    $('#rtSim', modal).classList.toggle('on', rt === 'sim');
    $('#rtDocker', modal).classList.toggle('on', rt === 'docker');
    $('#rtNote', modal).textContent = rt === 'sim'
      ? 'Runs in-process. Starts instantly, no image to pull - use it to drive the panel.'
      : `Real container from ${picked.image}. The first start pulls the image, which can take several minutes.`;
  };
  $('#rtSim', modal).addEventListener('click', () => setRuntime('sim'));
  $('#rtDocker', modal).addEventListener('click', () => setRuntime('docker'));

  const budget = () => {
    const host = state.host;
    if (!host) return;
    const mem = parseInt($('#fMem', modal).value || '0', 10);
    const cpu = parseFloat($('#fCpu', modal).value || '0');
    const newMem = host.memory.allocated_mb + mem, newCpu = host.cpu.allocated_vcpu + cpu;
    const memOver = capOver(newMem, host.memory.total_mb), cpuOver = capOver(newCpu, host.cpu.total_vcpu);

    // Disk is the one the agent will actually refuse on, and it refuses on
    // free space rather than on commitment - so the two are shown as two
    // different things. Over-committed is normal; not fitting is fatal.
    const disk = (picked && picked.disk_gb) || 0;
    const newDisk = host.disk.allocated_gb + disk;
    const diskOver = capOver(newDisk, host.disk.total_gb);
    const freeGB = Math.max(0, host.disk.total_gb - host.disk.used_gb);
    const wontFit = capKnown(host.disk.total_gb) && disk > freeGB;
    $('#budget', modal).innerHTML = `
      <div class="row-flex" style="justify-content:space-between;margin-bottom:5px">
        <span>CPU</span><span class="num"><b style="color:var(--t-0)">${newCpu.toFixed(1)}</b> / ${capKnown(host.cpu.total_vcpu) ? host.cpu.total_vcpu + ' vCPU' : 'unknown'}</span>
      </div>
      <div class="bar" style="margin-bottom:12px"><i class="${cpuOver ? 'warn' : ''}" style="width:${capPct(newCpu, host.cpu.total_vcpu)}%"></i></div>
      <div class="row-flex" style="justify-content:space-between;margin-bottom:5px">
        <span>Memory</span><span class="num"><b style="color:var(--t-0)">${(newMem / 1024).toFixed(1)}</b> / ${capKnown(host.memory.total_mb) ? (host.memory.total_mb / 1024).toFixed(0) + ' GB' : 'unknown'}</span>
      </div>
      <div class="bar" style="margin-bottom:12px"><i class="${memOver ? 'warn' : ''}" style="width:${capPct(newMem, host.memory.total_mb)}%"></i></div>
      <div class="row-flex" style="justify-content:space-between;margin-bottom:5px">
        <span>Disk</span><span class="num"><b style="color:var(--t-0)">${newDisk}</b> / ${capKnown(host.disk.total_gb) ? host.disk.total_gb + ' GB' : 'unknown'}${capKnown(host.disk.total_gb) ? ` <span class="muted">(${freeGB} GB free)</span>` : ''}</span>
      </div>
      <div class="bar"><i class="${diskOver ? 'warn' : ''}" style="width:${capPct(newDisk, host.disk.total_gb)}%"></i></div>
      ${wontFit ? `<div class="warnbox"><i class="ico ico-sm ico-warning"></i>
        <span>This will be refused: the template asks for ${disk} GB and only ${freeGB} GB is free. Delete a backup or an unused server first.</span></div>` : ''}
      ${(memOver || cpuOver || diskOver) ? `<div class="warnbox"><i class="ico ico-sm ico-warning"></i>
        <span>Overcommitted. Allowed &mdash; limits are ceilings, not reservations &mdash; but if every server peaks at once the kernel picks the loser${diskOver ? ', and disk is the one nobody gets back' : ''}.</span></div>` : ''}`;

    // The agent will refuse this create, and the sidebar that says so is below
    // the fold. Say it on the button you were about to press.
    const create = $('#createBtn', modal);
    create.disabled = wontFit;
    create.title = wontFit
      ? `Needs ${disk} GB; only ${freeGB} GB is free on the host.`
      : '';
  };

  const applyPick = (t) => {
    picked = t;
    modal.querySelectorAll('.tpl').forEach((el) => el.classList.toggle('is-picked', el.dataset.slug === t.slug));
    $('#barMark', modal).className = `gm gm-xs gm-${t.mark}`;
    $('#fName', modal).value = `${t.name} Server`;
    $('#fMem', modal).value = t.memory_mb;
    $('#fCpu', modal).value = t.cpu;
    $('#fVersion', modal).innerHTML = t.versions.map((v) => `<option>${esc(v)}</option>`).join('');
    setRuntime(runtime);
    budget();
  };

  modal.querySelectorAll('.tpl').forEach((el) =>
    el.addEventListener('click', () => applyPick(data.templates.find((t) => t.slug === el.dataset.slug))));
  ['fMem', 'fCpu'].forEach((id) => $('#' + id, modal).addEventListener('input', budget));
  applyPick(picked);

  $('#createBtn', modal).addEventListener('click', async () => {
    const btn = $('#createBtn', modal);
    btn.disabled = true;
    try {
      const s = await api('/api/servers', {
        method: 'POST',
        body: JSON.stringify({
          name: $('#fName', modal).value.trim(),
          template: picked.slug,
          version: $('#fVersion', modal).value,
          port: parseInt($('#fPort', modal).value, 10) || 0,
          memory_mb: parseInt($('#fMem', modal).value, 10) || 0,
          cpu: parseFloat($('#fCpu', modal).value) || 0,
          runtime,
          start: startNow.classList.contains('is-on'),
        }),
      });
      close();
      toast(`Created ${s.name}`);
      location.hash = `#/s/${s.id}/console`;
    } catch (e) { toast(e.message, 'err'); btn.disabled = false; }
  });
}

// -------------------------------------------------------- simple info views

function viewTemplates() {
  const rows = state.templates.length ? state.templates : [];
  return h(`<div class="content">
    <div class="page-head"><h1 class="page-title">Templates</h1>
      <div class="link-actions"><a>${rows.length || '…'} built in</a></div></div>
    <div class="panelbox">
      <h3>Bundled templates</h3>
      ${rows.map((t) => `<div class="row">
          <span class="gm gm-sm gm-${esc(t.mark)}"></span>
          <b>${esc(t.name)}</b>
          <span class="muted">${esc(t.group)}</span>
          <span class="spacer"></span>
          <span class="muted mono">${esc(t.image)}</span>
          <span class="muted">${t.memory_mb} MB &middot; ${t.cpu} vCPU</span>
        </div>`).join('') || '<div class="row muted">Loading…</div>'}
    </div>
    <div class="panelbox">
      <h3>Registry</h3>
      <div class="row"><span class="k">Source</span><span class="muted">Loaded from <span class="mono">data/templates/*.json</span>. Drop a file in and restart to add a game &mdash; no code change.</span></div>
      <div class="row"><span class="k">Remote registry</span><span class="muted">Not built. Templates are local files.</span></div>
    </div>
  </div>`);
}

// What the agent says it cannot do, rather than a list hand-maintained in the
// UI. /api/capabilities exists precisely so a client can decide what to offer,
// and the only client that ships was ignoring it - so this list could drift out
// of step with the agent and nobody would notice.
const CAP_LABELS = {
  scheduled_backups: ['Scheduled backups', 'The scheduler can run <code>!backup</code> on a timer; there is no built-in schedule.'],
  disk_quota: ['Disk quotas (hard)', 'No filesystem-level quota &mdash; ext4 inside an LXC has no project quotas. The panel shows usage against each allowance and refuses a create the disk cannot hold.'],
  plugins: ['Plugin management', 'List, enable, disable, delete and install from a URL.'],
  import: ['Import an existing server', 'Scan a directory and copy or adopt it.'],
  files: ['File manager', 'Browse and edit files inside a server directory.'],
  backups: ['Backups', 'Pause saves, flush, archive, resume.'],
  metrics: ['Metrics', 'CPU, memory and player history.'],
  audit: ['Audit log', 'Who did what.'],
};

// Disk used against the allowance the template gave the server. Nothing
// enforces that allowance - ext4 inside an LXC has no project quotas - so this
// is the whole of what "soft limit" means: you can see it being passed. The
// bar warns at 90% and reads full past 100%, which is a state the panel allows
// and the filesystem eventually will not.
function diskCell(s) {
  if (!s.disk_mb) return '<span class="muted">—</span>';
  const limitMB = (s.disk_gb || 0) * 1024;
  if (!limitMB) return `<span class="mono">${fmtMB(s.disk_mb)}</span>`;
  const pct = Math.min(100, Math.round((s.disk_mb / limitMB) * 100));
  const over = s.disk_mb >= limitMB;
  return `<div class="bar bar-sm"><i class="${over || pct >= 90 ? 'warn' : ''}" style="width:${pct}%"></i></div>
    <span class="${over ? 'num' : 'muted'}" ${over ? 'style="color:var(--amber)"' : ''}>${fmtMB(s.disk_mb)} / ${s.disk_gb} GB</span>`;
}

function notBuiltRows() {
  const caps = state.caps;
  if (!caps) return '<div class="row muted">Asking the agent&hellip;</div>';

  const off = Object.entries(caps)
    .filter(([, on]) => !on)
    .map(([k]) => CAP_LABELS[k] || [k, '']);

  // Things the agent has no flag for, but which are still not built. Kept
  // separate so it is obvious which list is authoritative.
  const known = [
    ['Plugin catalogue', 'Browse a plugin index &mdash; installing from a URL already works.'],
  ];

  return [...off, ...known]
    .map(([k, d]) => `<div class="row"><span class="k">${esc(k)}</span><span class="muted">${d}</span></div>`)
    .join('') || '<div class="row muted">Everything the agent advertises is built.</div>';
}

function viewDashboard() {
  const host = state.host;
  const running = state.servers.filter((s) => s.status === 'running');
  const players = host && typeof host.players === 'number'
    ? host.players
    : running.reduce((a, s) => a + (s.players ? s.players.online : 0), 0);

  // Host figures come from the host itself now. They used to be the sum of
  // every server's configured limit, which answers "if everything ran at once"
  // - on a deliberately overcommitted box that reads as 22 vCPU on a 4 vCPU
  // host, sitting next to "5 of 8 running". Commitment is still shown, below
  // and clearly labelled, because it is what a new server is checked against.
  return h(`<div class="content">
    <div class="page-head"><h1 class="page-title">Dashboard</h1></div>

    ${hostTiles(host)}

    <div class="panelbox">
      <h3>Servers</h3>
      <div class="tablewrap">
      <table class="dtable">
        <thead><tr>
          <th>Server</th><th>Actions</th><th>CPU</th><th>Memory</th>
          <th>Disk</th><th>Players</th><th>Status</th>
        </tr></thead>
        <tbody>
        ${state.servers.map((s) => {
          const up = s.status === 'running';
          const cpu = up ? s.cpu.percent : 0;
          const memU = up ? s.memory.used_mb : 0;
          const memL = s.memory.limit_mb || 0;
          const cls = STATUS_CLASS[s.status] || 'off';
          return `<tr>
            <td><span class="gm gm-xs gm-${esc(s.mark)}"></span>
                <a href="#/s/${s.id}/console">${esc(s.name)}</a>
                <div class="muted mono" style="font-size:11px">:${s.address.port}</div></td>
            <td class="dact">
              ${up
                ? `<button type="button" class="btn btn-ghost btn-sm btn-icon" data-dact="stop" data-sid="${esc(s.id)}" title="Stop"><i class="ico ico-sm ico-stop"></i></button>
                   <button type="button" class="btn btn-ghost btn-sm btn-icon" data-dact="restart" data-sid="${esc(s.id)}" title="Restart"><i class="ico ico-sm ico-restart"></i></button>`
                : `<button type="button" class="btn btn-ghost btn-sm btn-icon" data-dact="start" data-sid="${esc(s.id)}" title="Start"><i class="ico ico-sm ico-play"></i></button>`}
            </td>
            <td><div class="bar bar-sm"><i style="width:${Math.min(100, cpu)}%"></i></div>
                <span class="muted">${up ? cpu + '%' : '—'}</span></td>
            <td><div class="bar bar-sm"><i style="width:${capPct(memU, memL)}%"></i></div>
                <span class="muted">${up ? fmtMB(memU) + ' / ' + fmtMB(memL) : '—'}</span></td>
            <td>${diskCell(s)}</td>
            <td class="mono">${s.players ? s.players.online + ' / ' + s.players.max : '—'}</td>
            <td><span class="st ${cls}"><span class="dot dot-${cls === 'on' ? 'on' : cls === 'starting' ? 'starting' : 'off'}"></span> ${STATUS_LABEL[s.status]}</span></td>
          </tr>`;
        }).join('') || '<tr><td colspan="7" class="muted">No servers.</td></tr>'}
        </tbody>
      </table>
      </div>
    </div>

    <div class="panelbox">
      <h3>Committed</h3>
      <div class="row"><span class="k">CPU</span><span class="spacer"></span>
        <span class="mono">${host ? host.cpu.allocated_vcpu : '-'} / ${host ? host.cpu.total_vcpu : '-'} vCPU</span></div>
      <div class="row"><span class="k">Memory</span><span class="spacer"></span>
        <span class="mono">${host ? (host.memory.allocated_mb / 1024).toFixed(1) : '-'} / ${host && capKnown(host.memory.total_mb) ? (host.memory.total_mb / 1024).toFixed(0) : '?'} GB</span></div>
      <div class="row"><span class="muted" style="font-size:12px">Every server's configured limit added up, running or not. Limits are caps rather than reservations, so exceeding the host is normal &mdash; it only matters if they all run at once.</span></div>
    </div>

    <div class="panelbox">
      <h3>Not built yet</h3>
      ${notBuiltRows()}
    </div>
  </div>`);
}

// Pull the server list again after an action, so the row updates without
// waiting for the next metrics tick.
async function refreshServers() {
  try {
    const data = await api('/api/servers');
    state.servers = data.servers;
    state.host = data.host;
    renderTabstrip();
  } catch { /* the metrics feed will catch up */ }
}

// Row actions on the dashboard table.
function wireDashboardActions(root) {
  root.querySelectorAll('[data-dact]').forEach((b) => {
    b.addEventListener('click', async (e) => {
      e.preventDefault();
      const act = b.dataset.dact, id = b.dataset.sid;
      b.disabled = true;
      try {
        await api(`/api/servers/${id}/${act}`, { method: 'POST' });
        toast(`${act} requested`);
      } catch (err) { toast(err.message, 'err'); }
      await refreshServers();
    });
  });
}

// ------------------------------------------------------------------ router

function parseHash() {
  const hash = location.hash.replace(/^#\/?/, '');
  const parts = hash.split('/').filter(Boolean);
  if (!parts.length) return { name: 'servers', id: null };
  if (parts[0] === 's' && parts[1]) return { name: parts[2] || 'console', id: parts[1] };
  return { name: parts[0], id: null };
}

async function router(force) {
  const next = parseHash();
  const same = next.name === state.route.name && next.id === state.route.id;
  state.route = next;

  if (state.console) { state.console.destroy(); state.console = null; }

  const host = $('#view');
  if (host.firstElementChild) host.firstElementChild.dispatchEvent(new CustomEvent('gss:teardown'));
  let el;

  switch (next.name) {
    case 'console': el = viewConsole(next.id); break;
    // Panel settings and a server's settings share a name; the id separates
    // them, the same way it does for dashboard.
    case 'settings':
      el = next.id ? await viewSettings(next.id) : await window.extraViews.viewAdmin();
      break;
    case 'files': el = await window.extraViews.viewFiles(next.id); break;
    case 'backups': el = await window.extraViews.viewBackups(next.id); break;
    case 'players': el = await window.extraViews.viewPlayers(next.id); break;
    case 'scheduler': el = await window.extraViews.viewScheduler(next.id); break;
    case 'plugins': el = await window.extraViews.viewPlugins(next.id); break;
    case 'import': el = await window.extraViews.viewImport(); break;
    case 'dashboard':
      el = next.id ? await window.extraViews.viewServerDashboard(next.id) : viewDashboard();
      if (!next.id) wireDashboardActions(el);
      break;
    case 'templates':
      if (!state.templates.length) { try { state.templates = (await api('/api/templates')).templates; } catch {} }
      el = viewTemplates();
      break;
    case 'host': el = await window.extraViews.viewAdmin(); break; // legacy URL
    default: el = viewServers(); break;
  }

  host.innerHTML = '';
  host.appendChild(el);
  applyRole(host);
  renderRail();
  renderTabstrip();
  void same; void force;
}

// ------------------------------------------------------------------- roles

// The API has enforced roles since RBAC landed; the UI never learned about
// them anywhere except Panel settings. A viewer therefore saw a complete
// control surface - Start, Stop, Kill, Delete, the console input, Back up now -
// and every one of them returned 403 on click. The panel was not insecure, it
// was dishonest: it offered work it knew would be refused, and the operator had
// to discover their own permissions one error toast at a time.
//
// Selector-driven rather than an attribute on every button, because these
// controls are built as HTML strings in seven files. One table here is a place
// to look; two hundred attributes are not. Anything added later that is not in
// the table simply behaves as it does today, which is the safe direction to
// fail - the server still refuses it.
const ROLE_RANK = { viewer: 1, operator: 2, admin: 3 };

const GUARDED = [
  [2, [
    '[data-act="start"]', '[data-act="stop"]', '[data-act="restart"]',
    '[data-dact="start"]', '[data-dact="stop"]', '[data-dact="restart"]',
    '[data-act="clear-failures"]',
    '#cmdInput', '#mkBackup', '#newFolder', '#save', '#saveBtn', '#saveRestartBtn',
    '#addBtn', '#newTask', '#plInstall', '#createBtn',
    '[data-run]', '[data-edit]', '[data-toggle]', '[data-rm]',
    '[data-need="operator"]',
  ]],
  [3, [
    '[data-act="kill"]', '[data-act="delete"]',
    '[data-restore]', '[data-del]',
    '#impScan', '#impGo', '#clGo',
    '[data-need="admin"]',
  ]],
];

function myRoleRank() {
  const me = state.me;
  if (!me) return 0;
  // An unclaimed panel has no accounts, so nobody is an admin and everybody can
  // do everything - the same rule viewAdmin applies to its own controls.
  if (me.unclaimed) return 3;
  return ROLE_RANK[me.user && me.user.role] || 0;
}

function applyRole(root) {
  const rank = myRoleRank();
  if (rank >= 3) return;
  for (const [need, selectors] of GUARDED) {
    if (rank >= need) continue;
    const label = need === 3 ? 'an admin' : 'an operator';
    for (const el of root.querySelectorAll(selectors.join(','))) {
      if (el.dataset.denied) continue;
      el.dataset.denied = '1';
      el.disabled = true;
      el.classList.add('is-denied');
      el.title = `Only ${label} can do this`;
    }
  }
}

// Views re-render their own lists - a file listing after a delete, the task
// table after a run, the backup list after one lands - and those replacements
// never pass through the router. Guarding only at mount would leave a viewer
// with live controls the moment anything refreshed, which is the same bug with
// a delay. One observer covers every path, present and future.
function watchRoleSurface() {
  if (myRoleRank() >= 3) return;
  const host = document.getElementById('view');
  if (!host || typeof MutationObserver === 'undefined') return;
  let queued = false;
  new MutationObserver(() => {
    if (queued) return;
    queued = true;
    queueMicrotask(() => { queued = false; applyRole(host); });
  }).observe(host, { childList: true, subtree: true });
}

// -------------------------------------------------------------- live feed

function connectEvents() {
  const es = new EventSource('/api/events');
  es.onmessage = (ev) => {
    let m; try { m = JSON.parse(ev.data); } catch { return; }
    if (m.servers) {
      state.servers = m.servers;
      renderTabstrip();
      // Only *list-level* views re-render from the feed. The check must include
      // `!state.route.id`: `#/s/<id>/dashboard` and `#/dashboard` share a route
      // name, so without it the feed replaced a server's own dashboard with the
      // global one every two seconds, and leaked that view's refresh timer each
      // time. Server-scoped views own their own refresh.
      const listView = !state.route.id &&
        (state.route.name === 'servers' || state.route.name === 'dashboard');
      if (listView) {
        const host = $('#view');
        const prev = host.firstElementChild;
        const keepScroll = prev ? prev.scrollTop : 0;
        if (prev) prev.dispatchEvent(new CustomEvent('gss:teardown'));
        host.innerHTML = '';
        const el = state.route.name === 'servers' ? viewServers() : viewDashboard();
        if (state.route.name === 'dashboard') wireDashboardActions(el);
        host.appendChild(el);
        el.scrollTop = keepScroll;
      }
    }
  };
  es.onerror = () => { /* EventSource retries on its own */ };
}

async function boot() {
  try {
    state.me = await api('/api/me');
  } catch { state.me = null; }

  // Needs a session (RoleViewer), so this fails for an unauthenticated boot -
  // which is fine, because the dashboard that renders it is behind sign-in too.
  // Caught rather than awaited-and-thrown so a capabilities outage never blocks
  // the panel loading.
  try {
    state.caps = (await api('/api/capabilities')).features;
  } catch { state.caps = null; }

  // An unclaimed panel has exactly one thing to do. Show that instead of an
  // empty server list with the real task buried in Settings.
  if (state.me && state.me.needs_setup) {
    document.querySelector('.app').style.display = 'none';
    document.body.appendChild(window.extraViews.viewSetup());
    return;
  }

  // An account still on a password an admin chose is refused every other
  // route by the API, so there is nothing else to show it.
  if (state.me && state.me.must_change && state.me.user) {
    document.querySelector('.app').style.display = 'none';
    document.body.appendChild(window.extraViews.viewForcePassword(state.me.user.name));
    return;
  }

  try {
    const data = await api('/api/servers');
    state.servers = data.servers;
    state.host = data.host;
  } catch (e) {
    if (String(e.message).includes('sign in')) {
      document.querySelector('.app').style.display = 'none';
      document.body.appendChild(window.extraViews.viewLogin());
      return;
    }
    $('#view').innerHTML = `<div class="empty"><div><div class="big">Can't reach the agent</div><div>${esc(e.message)}</div></div></div>`;
    return;
  }
  window.addEventListener('hashchange', () => router());
  watchRoleSurface();
  await router();

  // ?nolive renders one static frame and opens no long-lived connections, so a
  // headless screenshot terminates instead of hanging on the event stream.
  if (new URLSearchParams(location.search).has('nolive')) return;

  // keep host stats fresh for the budget maths
  setInterval(async () => { try { state.host = await api('/api/host'); } catch {} }, 5000);
  connectEvents();
}

boot();
