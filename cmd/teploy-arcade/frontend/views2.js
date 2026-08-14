/* views2.js - Players, Scheduler, and the server dashboard rebuilt to match
 * Dashboard layout: stat tiles, a Server Statistics panel with a time
 * scale, three stacked bands (CPU area, RAM area, players bar chart) with
 * right-aligned coloured readouts, and a players sidebar.
 */

// ------------------------------------------------------- server dashboard

function areaBand(samples, pick, color, fill, max) {
  const w = 600, hgt = 74;
  if (samples.length < 2) {
    return `<svg viewBox="0 0 ${w} ${hgt}" preserveAspectRatio="none" class="band">
      <line x1="0" y1="${hgt - 2}" x2="${w}" y2="${hgt - 2}" stroke="#3a3a3a"/></svg>`;
  }
  const step = w / (samples.length - 1);
  const y = (v) => hgt - 3 - (v / max) * (hgt - 10);
  const pts = samples.map((s, i) => `${(i * step).toFixed(1)},${y(pick(s)).toFixed(1)}`);
  return `<svg viewBox="0 0 ${w} ${hgt}" preserveAspectRatio="none" class="band">
    <path d="M${pts[0]} L${pts.join(' L')} L${w},${hgt} L0,${hgt} Z" fill="${fill}"/>
    <polyline points="${pts.join(' ')}" fill="none" stroke="${color}" stroke-width="1.6"
      vector-effect="non-scaling-stroke"/>
  </svg>`;
}

// Players are drawn as a bar chart, not a line - discrete counts read better
// as bars, and it distinguishes the band at a glance.
function barBand(samples, pick, color, max) {
  const w = 600, hgt = 74;
  if (!samples.length) {
    return `<svg viewBox="0 0 ${w} ${hgt}" preserveAspectRatio="none" class="band">
      <line x1="0" y1="${hgt - 2}" x2="${w}" y2="${hgt - 2}" stroke="#3a3a3a"/></svg>`;
  }
  const slot = w / samples.length;
  const bw = Math.max(1.5, Math.min(6, slot * 0.5));
  const bars = samples.map((s, i) => {
    const v = pick(s);
    const h = v <= 0 ? 1.5 : Math.max(2, (v / max) * (hgt - 10));
    const x = i * slot + (slot - bw) / 2;
    return `<rect x="${x.toFixed(1)}" y="${(hgt - 3 - h).toFixed(1)}" width="${bw.toFixed(1)}"
      height="${h.toFixed(1)}" fill="${color}" opacity="${v <= 0 ? 0.28 : 1}"/>`;
  }).join('');
  return `<svg viewBox="0 0 ${w} ${hgt}" preserveAspectRatio="none" class="band">${bars}</svg>`;
}

async function viewServerDashboard(id) {
  const s = serverById(id);
  if (!s) return h(`<div class="content"><div class="empty">Server not found.</div></div>`);

  const root = h(`<div class="viewhost">
    ${serverHeader(s, 'dashboard')}
    <div class="split">
      <div class="content">
        <div class="statrow" id="statRow"></div>
        <div class="statspanel">
          <div class="statspanel-head">
            <span class="t">Server Statistics</span>
            <span class="spacer"></span>
            <span class="scale-label">Time Scale</span>
            <span class="scales" id="scales">
              ${['1m', '5m', '30m', '1h', '4h'].map((wnd) =>
                `<span class="scale ${wnd === '5m' ? 'on' : ''}" data-w="${wnd}">${wnd}</span>`).join('')}
            </span>
          </div>
          <div id="bands"><div class="empty"><span class="spin"></span></div></div>
        </div>
      </div>
      <aside class="aside">
        <div class="aside-head"><i class="ico ico-sm ico-users"></i> Players Online
          <span class="n" id="dashCount">-</span></div>
        <div class="aside-list" id="dashPlayers"></div>
      </aside>
    </div>
  </div>`);

  wireHeaderActions(root, id);
  let win = '5m';

  const load = async () => {
    const cur = serverById(id) || s;
    const running = cur.status === 'running';

    // Three tiles: CPU, RAM, UPTIME.
    $('#statRow', root).innerHTML = `
      <div class="stat">
        <span class="glyph" style="background:#5b5bd6"><i class="ico ico-lg ico-cpu"></i></span>
        <span class="t"><span class="k" style="color:#8f8ff0">CPU</span>
          <span class="v">${running ? cur.cpu.percent : 0} %</span></span>
      </div>
      <div class="stat">
        <span class="glyph" style="background:#2f86d6"><i class="ico ico-lg ico-memory"></i></span>
        <span class="t"><span class="k" style="color:#68b6ef">RAM</span>
          <span class="v">${running ? cur.memory.used_mb : 0} MB</span></span>
      </div>
      <div class="stat">
        <span class="glyph" style="background:#2bb673"><i class="ico ico-lg ico-clock"></i></span>
        <span class="t"><span class="k" style="color:#5fd39b">UPTIME</span>
          <span class="v">${running ? fmtUptime(cur.uptime) : '-'}</span></span>
      </div>`;

    const players = cur.player_list || [];
    $('#dashCount', root).textContent = cur.players ? `${cur.players.online} / ${cur.players.max}` : '-';
    $('#dashPlayers', root).innerHTML = players.length
      ? players.map((p) => {
          const hue = [...p.name].reduce((a, c) => a + c.charCodeAt(0), 0) % 360;
          return `<div class="player">
            <span class="skin" style="background:linear-gradient(150deg,hsl(${hue} 32% 46%),hsl(${hue} 32% 28%))"></span>
            <span><div class="pn">${esc(p.name)}</div><div class="pm">${p.ping_ms} ms</div></span>
          </div>`;
        }).join('')
      : `<div style="padding:14px 9px;color:var(--t-2);font-size:12px">Nobody online.</div>`;

    try {
      const d = await api(`/api/servers/${id}/metrics?window=${win}`);
      const sm = d.samples;
      const cpuMax = Math.max(10, ...sm.map((x) => x.cpu)) * 1.15;
      const memMax = Math.max(1, d.limit_mb);
      const plMax = Math.max(4, ...sm.map((x) => x.players)) * 1.2;
      const last = sm.length ? sm[sm.length - 1] : { cpu: 0, mem_mb: 0, players: 0 };

      $('#bands', root).innerHTML = `
        <div class="bandrow">
          ${areaBand(sm, (x) => x.cpu, '#6f6fe0', 'rgba(90,90,190,.32)', cpuMax)}
          <div class="bandlbl" style="color:#8f8ff0">${last.cpu} % CPU</div>
        </div>
        <div class="bandrow">
          ${areaBand(sm, (x) => x.mem_mb, '#3ba0e8', 'rgba(45,95,140,.42)', memMax)}
          <div class="bandlbl" style="color:#68b6ef">${last.mem_mb} MB RAM</div>
        </div>
        <div class="bandrow">
          ${barBand(sm, (x) => x.players, '#e8615c', plMax)}
          <div class="bandlbl" style="color:#e8615c">${last.players} PLAYERS</div>
        </div>`;
    } catch (e) {
      $('#bands', root).innerHTML = `<div class="empty">${esc(e.message)}</div>`;
    }
  };

  $('#scales', root).querySelectorAll('.scale').forEach((b) =>
    b.addEventListener('click', () => {
      win = b.dataset.w;
      $('#scales', root).querySelectorAll('.scale').forEach((x) => x.classList.toggle('on', x === b));
      load();
    }));

  load();
  const timer = setInterval(load, 5000);
  root.addEventListener('gss:teardown', () => clearInterval(timer));
  return root;
}

// ------------------------------------------------------------ players tab

const PLAYER_LISTS = [
  { key: 'whitelist', label: 'Whitelist', add: 'Add username or UUID' },
  { key: 'ops', label: 'Operators', add: 'Add username or UUID' },
  { key: 'banned', label: 'Banned', add: 'Ban a username' },
  { key: 'banned-ips', label: 'Banned IPs', add: 'Ban an IP address' },
];

async function viewPlayers(id) {
  const s = serverById(id);
  if (!s) return h(`<div class="content"><div class="empty">Server not found.</div></div>`);

  const root = h(`<div class="viewhost">
    ${serverHeader(s, 'players')}
    <div class="content">
      <div class="plist-head">
        <span class="subtabs" id="subtabs">
          ${PLAYER_LISTS.map((l, i) =>
            `<span class="subtab ${i === 0 ? 'on' : ''}" data-list="${l.key}">${l.label}
              <span class="cnt" data-cnt="${l.key}"></span></span>`).join('')}
        </span>
        <span class="spacer"></span>
        <span class="muted" id="liveNote"></span>
        <button class="btn btn-quiet btn-sm" id="editRaw"><i class="ico ico-sm ico-terminal"></i> Switch to File Editor</button>
      </div>
      <div style="padding:0 22px 20px">
        <div class="panelbox" style="margin:0"><div id="plist"></div></div>
      </div>
    </div>
    <div class="addbar">
      <span class="sigil"><i class="ico ico-sm ico-plus"></i></span>
      <input class="inp" id="addName" placeholder="Add username or UUID" autocomplete="off">
      <input class="inp" id="addReason" placeholder="Reason (bans only)" style="max-width:220px;display:none">
      <button class="btn btn-primary" id="addBtn">Add</button>
    </div>
  </div>`);

  wireHeaderActions(root, id);
  let active = 'whitelist';
  let data = {};

  const render = () => {
    const entries = data[active] || [];
    const err = data[active + '_error'];
    const isBan = active === 'banned' || active === 'banned-ips';

    PLAYER_LISTS.forEach((l) => {
      const el = root.querySelector(`[data-cnt="${l.key}"]`);
      if (el) el.textContent = (data[l.key] || []).length || '';
    });

    $('#addName', root).placeholder = PLAYER_LISTS.find((l) => l.key === active).add;
    $('#addReason', root).style.display = isBan ? '' : 'none';

    $('#liveNote', root).textContent = data.running
      ? 'Server is running — changes go through the game console so they apply live.'
      : 'Server is stopped — changes are written straight to the file.';

    const list = $('#plist', root);
    if (err) {
      list.innerHTML = `<div class="row" style="color:var(--offline)">${esc(err)}</div>`;
      return;
    }
    if (!entries.length) {
      list.innerHTML = `<div class="row muted">Nothing on the ${esc(active.replace('-', ' '))} list.</div>`;
      return;
    }

    // A multi-column grid of avatar + name + delete.
    list.innerHTML = `<div class="pgrid">${entries.map((e) => {
      const who = e.ip || e.name || '(unknown)';
      const hue = [...who].reduce((a, c) => a + c.charCodeAt(0), 0) % 360;
      return `<div class="pcell">
        <span class="skin" style="background:linear-gradient(150deg,hsl(${hue} 32% 46%),hsl(${hue} 32% 28%))"></span>
        <span class="pn" title="${esc(e.reason || '')}">${esc(who)}</span>
        ${e.level ? `<span class="badge-mute">lvl ${e.level}</span>` : ''}
        <span class="spacer"></span>
        <button class="btn btn-ghost btn-sm btn-icon" data-rm="${esc(who)}" title="Remove">
          <i class="ico ico-sm ico-trash" style="color:var(--offline)"></i></button>
      </div>`;
    }).join('')}</div>`;

    list.querySelectorAll('[data-rm]').forEach((b) =>
      b.addEventListener('click', async () => {
        try {
          await api(`/api/servers/${id}/players?list=${active}&name=${encodeURIComponent(b.dataset.rm)}`,
            { method: 'DELETE' });
          toast(`Removed ${b.dataset.rm}`);
          setTimeout(load, data.running ? 700 : 0);
        } catch (e) { toast(e.message, 'err'); }
      }));
  };

  const load = async () => {
    try { data = await api(`/api/servers/${id}/players`); render(); }
    catch (e) { $('#plist', root).innerHTML = `<div class="row" style="color:var(--offline)">${esc(e.message)}</div>`; }
  };

  $('#subtabs', root).querySelectorAll('.subtab').forEach((t) =>
    t.addEventListener('click', () => {
      active = t.dataset.list;
      $('#subtabs', root).querySelectorAll('.subtab').forEach((x) => x.classList.toggle('on', x === t));
      render();
    }));

  $('#editRaw', root).addEventListener('click', () => {
    const file = { whitelist: 'whitelist.json', ops: 'ops.json',
      banned: 'banned-players.json', 'banned-ips': 'banned-ips.json' }[active];
    window.extraViews.openEditor(id, file, load);
  });

  const add = async () => {
    const name = $('#addName', root).value.trim();
    if (!name) return;
    try {
      await api(`/api/servers/${id}/players`, {
        method: 'POST',
        body: JSON.stringify({ list: active, name, reason: $('#addReason', root).value.trim() }),
      });
      $('#addName', root).value = '';
      $('#addReason', root).value = '';
      toast(`Added ${name}`);
      setTimeout(load, data.running ? 700 : 0);
    } catch (e) { toast(e.message, 'err'); }
  };
  $('#addBtn', root).addEventListener('click', add);
  $('#addName', root).addEventListener('keydown', (e) => { if (e.key === 'Enter') add(); });

  load();
  return root;
}

// ---------------------------------------------------------- scheduler tab

async function viewScheduler(id) {
  const s = serverById(id);
  if (!s) return h(`<div class="content"><div class="empty">Server not found.</div></div>`);

  const root = h(`<div class="viewhost">
    ${serverHeader(s, 'scheduler')}
    <div class="content">
      <div class="settings-head">
        <div>
          <h2 class="sect-title">Scheduler</h2>
          <p class="sect-sub">Tasks run at a 24-hour clock time on the host. Steps are separated by <span class="mono">;</span></p>
        </div>
        <div class="spacer"></div>
        <button class="btn btn-primary btn-sm" id="newTask"><i class="ico ico-sm ico-plus"></i> Create Task</button>
      </div>
      <div style="padding:0 22px 26px">
        <div class="panelbox" style="margin:0 0 16px"><div id="tasks"></div></div>
        <div class="panelbox" style="margin:0">
          <h3>Actions you can use besides console commands</h3>
          <div class="row"><span class="k"><code>!restart</code></span><span class="muted">Restart the server. The reason a scheduler exists.</span></div>
          <div class="row"><span class="k"><code>!backup [note]</code></span><span class="muted">Take a backup, with the quiesce cycle.</span></div>
          <div class="row"><span class="k"><code>!stop</code> / <code>!start</code></span><span class="muted">Stop or start the server.</span></div>
          <div class="row"><span class="k"><code>!wait &lt;seconds&gt;</code></span><span class="muted">Pause between steps, up to 900s.</span></div>
          <div class="row"><span class="k">Example</span><span class="muted mono">say Restarting in 60s; !wait 60; !backup nightly; !restart</span></div>
        </div>
      </div>
    </div>
  </div>`);

  wireHeaderActions(root, id);

  const load = async () => {
    try {
      const d = await api(`/api/servers/${id}/tasks`);
      const list = $('#tasks', root);
      if (!d.tasks.length) {
        list.innerHTML = `<div class="row muted">No tasks. A nightly restart is the usual first one.</div>`;
        return;
      }
      list.innerHTML = d.tasks.map((t) => `
        <div class="row" data-tid="${esc(t.id)}">
          <span class="tgl ${t.enabled ? 'is-on' : ''}" data-toggle title="${t.enabled ? 'Enabled' : 'Disabled'}"></span>
          <span style="width:170px;flex:none">
            <div><b>${esc(t.name)}</b></div>
            <div class="muted" style="font-size:11px">${t.repeat ? 'daily' : 'once'} at ${esc(t.time)}</div>
          </span>
          <span class="mono muted" style="font-size:11.5px;flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap"
            title="${esc(t.commands)}">${esc(t.commands)}</span>
          <span class="muted" style="font-size:11.5px;width:150px;text-align:right">
            ${t.enabled && t.next_run ? 'next ' + new Date(t.next_run * 1000).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }) : '&mdash;'}
          </span>
          <span class="muted" style="font-size:11.5px;width:60px;text-align:right">${t.runs} run${t.runs === 1 ? '' : 's'}</span>
          <button class="btn btn-quiet btn-sm" data-run>Run now</button>
          <button class="btn btn-ghost btn-sm btn-icon" data-edit title="Edit"><i class="ico ico-sm ico-sliders"></i></button>
          <button class="btn btn-ghost btn-sm btn-icon" data-del title="Delete"><i class="ico ico-sm ico-trash"></i></button>
        </div>
        ${t.last_err ? `<div class="row" style="padding-top:0;border-top:0;color:var(--offline);font-size:11.5px">
          <span class="k"></span><span>last run failed: ${esc(t.last_err)}</span></div>` : ''}
      `).join('');

      list.querySelectorAll('.row[data-tid]').forEach((row) => {
        const tid = row.dataset.tid;
        const task = d.tasks.find((x) => x.id === tid);
        row.querySelector('[data-toggle]').addEventListener('click', async () => {
          try {
            await api(`/api/servers/${id}/tasks/${tid}`, {
              method: 'PATCH',
              body: JSON.stringify({ ...task, enabled: !task.enabled }),
            });
            load();
          } catch (e) { toast(e.message, 'err'); }
        });
        row.querySelector('[data-run]').addEventListener('click', async (ev) => {
          const b = ev.currentTarget;
          b.disabled = true; b.textContent = 'Running…';
          try { await api(`/api/servers/${id}/tasks/${tid}/run`, { method: 'POST' }); toast('Task ran'); }
          catch (e) { toast(e.message, 'err'); }
          load();
        });
        row.querySelector('[data-edit]').addEventListener('click', () => taskDialog(id, task, load));
        row.querySelector('[data-del]').addEventListener('click', async () => {
          if (!confirm(`Delete task "${task.name}"?`)) return;
          try { await api(`/api/servers/${id}/tasks/${tid}`, { method: 'DELETE' }); load(); }
          catch (e) { toast(e.message, 'err'); }
        });
      });
    } catch (e) {
      $('#tasks', root).innerHTML = `<div class="row" style="color:var(--offline)">${esc(e.message)}</div>`;
    }
  };

  $('#newTask', root).addEventListener('click', () => taskDialog(id, null, load));
  load();
  return root;
}

// Create Task dialog: name, commands, time, repeat.
function taskDialog(serverId, task, onSaved) {
  const editing = !!task;
  const modal = h(`<div class="scrim">
    <div class="modal" style="width:min(660px,100%)">
      <div class="modal-bar"><i class="ico ico-sm ico-clock"></i> ${editing ? 'Edit Task' : 'Create Task'}
        <span class="spacer"></span><span class="strip-btn" id="x"><i class="ico ico-sm ico-close"></i></span></div>
      <div class="modal-banner">
        <h2>${editing ? 'Edit this task' : 'Create a new Task'}</h2>
        <p>Runs at a certain time (24H notation). E.g. 17:00 or 9:45.</p>
      </div>
      <div class="modal-body">
        <div class="field" style="margin-bottom:16px">
          <label style="display:block;font-size:13.5px;margin-bottom:7px">Task Name</label>
          <input class="inp" id="tName" value="${esc(task ? task.name : '')}" placeholder="Nightly restart">
        </div>
        <div class="field" style="margin-bottom:6px">
          <label style="display:block;font-size:13.5px;margin-bottom:7px">Commands</label>
          <input class="inp mono" id="tCmd" value="${esc(task ? task.commands : '')}"
            placeholder="say Restarting in 60s; !wait 60; !restart">
        </div>
        <div class="help" style="margin-bottom:16px">
          Multiple steps separated by <span class="mono">;</span> — plain text goes to the game console,
          <span class="mono">!restart</span> <span class="mono">!backup</span> <span class="mono">!stop</span>
          <span class="mono">!start</span> <span class="mono">!wait</span> are panel actions.
        </div>
        <div class="field" style="margin-bottom:16px">
          <label style="display:block;font-size:13.5px;margin-bottom:7px">Time</label>
          <div class="row-flex">
            <input class="inp mono" id="tTime" value="${esc(task ? task.time : '04:00:00')}" style="width:150px">
            <span class="help" style="margin:0">Define a time in the 24h format. (hh:mm:ss)</span>
          </div>
        </div>
        <label class="cbx ${!task || task.repeat ? 'is-on' : 'is-off'}" id="tRepeat">
          <span class="box"><i class="ico ico-check"></i></span> Repeat
        </label>
        <div class="help" style="margin-left:29px">Loop this task daily.</div>
      </div>
      <div class="modal-foot">
        <div class="spacer"></div>
        <button class="btn btn-quiet" id="back">Back</button>
        <button class="btn btn-primary" id="save">Save</button>
      </div>
    </div>
  </div>`);

  $('#modalHost').appendChild(modal);
  const close = () => modal.remove();
  $('#x', modal).addEventListener('click', close);
  $('#back', modal).addEventListener('click', close);

  const rep = $('#tRepeat', modal);
  rep.addEventListener('click', (e) => {
    e.preventDefault();
    const on = !rep.classList.contains('is-on');
    rep.classList.toggle('is-on', on); rep.classList.toggle('is-off', !on);
  });

  $('#save', modal).addEventListener('click', async () => {
    const body = {
      name: $('#tName', modal).value.trim(),
      commands: $('#tCmd', modal).value.trim(),
      time: $('#tTime', modal).value.trim(),
      repeat: rep.classList.contains('is-on'),
      enabled: true,
    };
    try {
      if (editing) {
        await api(`/api/servers/${serverId}/tasks/${task.id}`, { method: 'PATCH', body: JSON.stringify(body) });
      } else {
        await api(`/api/servers/${serverId}/tasks`, { method: 'POST', body: JSON.stringify(body) });
      }
      close();
      toast(editing ? 'Task updated' : 'Task created');
      onSaved();
    } catch (e) { toast(e.message, 'err'); }
  });

  setTimeout(() => $('#tName', modal).focus(), 30);
}

Object.assign(window.extraViews, { viewPlayers, viewScheduler, viewServerDashboard });
