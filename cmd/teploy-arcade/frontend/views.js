/* views.js - phases 4-8 screens: graphs, files, backups, users, audit, login.
 * Loaded after app.js; hooks into the same router via window.extraViews.
 */

// ------------------------------------------------------------------ charts

// Minimal line chart. No library: one path, one area, an axis label.
function lineChart(samples, pick, opts = {}) {
  const w = 600, hgt = opts.height || 90;
  if (!samples.length) {
    return `<svg viewBox="0 0 ${w} ${hgt}" preserveAspectRatio="none" style="width:100%;height:${hgt}px">
      <line x1="0" y1="${hgt - 1}" x2="${w}" y2="${hgt - 1}" stroke="#3a3a3a"/></svg>`;
  }
  const vals = samples.map(pick);
  const max = Math.max(opts.min || 1, ...vals) * 1.12;
  const stepX = samples.length > 1 ? w / (samples.length - 1) : w;
  const y = (v) => hgt - 4 - (v / max) * (hgt - 10);

  const pts = vals.map((v, i) => `${(i * stepX).toFixed(1)},${y(v).toFixed(1)}`);
  const line = pts.join(' ');
  const area = `M${pts[0]} L${pts.join(' L')} L${w},${hgt} L0,${hgt} Z`;
  const color = opts.color || '#6f6fe0';
  const fill = opts.fill || 'rgba(111,111,224,.16)';

  return `<svg viewBox="0 0 ${w} ${hgt}" preserveAspectRatio="none" style="width:100%;height:${hgt}px">
    <path d="${area}" fill="${fill}"/>
    <polyline points="${line}" fill="none" stroke="${color}" stroke-width="1.6"
      vector-effect="non-scaling-stroke"/>
  </svg>`;
}

function chartCard(title, samples, pick, unit, opts) {
  const latest = samples.length ? pick(samples[samples.length - 1]) : 0;
  const peak = samples.length ? Math.max(...samples.map(pick)) : 0;
  return `<div class="chartcard">
    <div class="chart-head">
      <span class="k">${esc(title)}</span>
      <span class="spacer"></span>
      <span class="v"><b>${latest}</b> ${esc(unit)}</span>
      <span class="muted" style="font-size:11.5px">peak ${peak}</span>
    </div>
    ${lineChart(samples, pick, opts)}
  </div>`;
}

// ------------------------------------------------------------- files view

async function viewFiles(id) {
  const s = serverById(id);
  if (!s) return h(`<div class="content"><div class="empty">Server not found.</div></div>`);

  const root = h(`<div class="viewhost">
    ${serverHeader(s, 'files')}
    <div class="content">
      <div class="settings-head">
        <div>
          <h2 class="sect-title">Files</h2>
          <p class="sect-sub" id="crumbs">/</p>
        </div>
        <div class="spacer"></div>
        <button type="button" class="btn btn-quiet btn-sm" id="newFolder"><i class="ico ico-sm ico-plus"></i> New folder</button>
        <button type="button" class="btn btn-quiet btn-sm" id="upDir" style="margin-left:8px"><i class="ico ico-sm ico-up"></i> Up</button>
      </div>
      <div style="padding:0 22px 26px">
        <div class="panelbox" style="margin:0"><div id="fileList"></div></div>
      </div>
    </div>
  </div>`);

  wireHeaderActions(root, id);

  let cwd = '';
  const list = $('#fileList', root);

  const load = async () => {
    list.innerHTML = `<div class="row"><span class="spin"></span></div>`;
    $('#crumbs', root).textContent = '/' + cwd;
    try {
      const data = await api(`/api/servers/${id}/files?path=${encodeURIComponent(cwd)}`);
      if (!data.entries.length) {
        list.innerHTML = `<div class="row muted">This folder is empty.</div>`;
        return;
      }
      list.innerHTML = data.entries.map((e) => `
        <div class="row filerow" data-path="${esc(e.path)}" data-dir="${e.dir}" data-text="${e.text}">
          <i class="ico ico-sm ico-${e.dir ? 'folder' : 'book'}" style="color:${e.dir ? 'var(--lime)' : 'var(--t-2)'}"></i>
          <span class="fname">${esc(e.name)}</span>
          <span class="spacer"></span>
          <span class="muted num" style="font-size:11.5px">${e.dir ? '' : humanBytes(e.size)}</span>
          <span class="muted nowrap" style="font-size:11.5px;width:160px;text-align:right">${new Date(e.mod * 1000).toLocaleDateString()} ${new Date(e.mod * 1000).toLocaleTimeString([], {hour:'2-digit',minute:'2-digit'})}</span>
          <span class="rowacts">
            ${e.dir ? '' : `<button type="button" class="btn btn-ghost btn-sm btn-icon" data-dl title="Download"><i class="ico ico-sm ico-download"></i></button>`}
            <button type="button" class="btn btn-ghost btn-sm btn-icon" data-rm title="Delete"><i class="ico ico-sm ico-trash"></i></button>
          </span>
        </div>`).join('');

      list.querySelectorAll('.filerow').forEach((row) => {
        const path = row.dataset.path;
        row.querySelector('.fname').addEventListener('click', () => {
          if (row.dataset.dir === 'true') { cwd = path; load(); }
          else if (row.dataset.text === 'true') openEditor(id, path, load);
          else toast('This file is binary or too large to edit. Download it instead.', 'warn');
        });
        const dl = row.querySelector('[data-dl]');
        if (dl) dl.addEventListener('click', () => {
          window.location = `/api/servers/${id}/download?path=${encodeURIComponent(path)}`;
        });
        row.querySelector('[data-rm]').addEventListener('click', async () => {
          if (!confirm(`Delete ${path}? This cannot be undone.`)) return;
          try {
            await api(`/api/servers/${id}/file?path=${encodeURIComponent(path)}`, { method: 'DELETE' });
            toast('Deleted'); load();
          } catch (e) { toast(e.message, 'err'); }
        });
      });
    } catch (e) {
      list.innerHTML = `<div class="row" style="color:var(--offline)">${esc(e.message)}</div>`;
    }
  };

  $('#upDir', root).addEventListener('click', () => {
    if (!cwd) return;
    cwd = cwd.includes('/') ? cwd.slice(0, cwd.lastIndexOf('/')) : '';
    load();
  });
  $('#newFolder', root).addEventListener('click', async () => {
    const name = prompt('Folder name');
    if (!name) return;
    try {
      await api(`/api/servers/${id}/mkdir`, {
        method: 'POST',
        body: JSON.stringify({ path: cwd ? `${cwd}/${name}` : name }),
      });
      load();
    } catch (e) { toast(e.message, 'err'); }
  });

  load();
  return root;
}

function humanBytes(n) {
  if (n >= 1 << 30) return (n / (1 << 30)).toFixed(1) + ' GB';
  if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + ' MB';
  if (n >= 1 << 10) return (n / (1 << 10)).toFixed(0) + ' KB';
  return n + ' B';
}

async function openEditor(id, path, onSaved) {
  let data;
  try { data = await api(`/api/servers/${id}/file?path=${encodeURIComponent(path)}`); }
  catch (e) { toast(e.message, 'err'); return; }

  const modal = h(`<div class="scrim">
    <div class="modal" style="width:min(980px,100%);height:min(80vh,100%)">
      <div class="modal-bar"><i class="ico ico-sm ico-book"></i> <span class="mono">${esc(path)}</span>
        <span class="spacer"></span><span class="strip-btn" id="x"><i class="ico ico-sm ico-close"></i></span></div>
      <div class="modal-body" style="padding:0;flex:1;min-height:0;display:flex">
        <textarea id="ed" spellcheck="false"></textarea>
      </div>
      <div class="modal-foot">
        <span class="note" id="edNote">${data.content.split('\n').length} lines</span>
        <div class="spacer"></div>
        <button type="button" class="btn btn-quiet" id="cancel">Cancel</button>
        <button type="button" class="btn btn-primary" id="save"><i class="ico ico-sm ico-check"></i> Save</button>
      </div>
    </div>
  </div>`);

  $('#modalHost').appendChild(modal);
  const ta = $('#ed', modal);
  ta.value = data.content;
  const close = () => modal.remove();
  $('#x', modal).addEventListener('click', close);
  $('#cancel', modal).addEventListener('click', close);
  ta.addEventListener('input', () => {
    $('#edNote', modal).textContent = `${ta.value.split('\n').length} lines - unsaved`;
  });
  $('#save', modal).addEventListener('click', async () => {
    try {
      await api(`/api/servers/${id}/file`, {
        method: 'PUT', body: JSON.stringify({ path, content: ta.value }),
      });
      toast(`Saved ${path}`);
      close();
      if (onSaved) onSaved();
    } catch (e) { toast(e.message, 'err'); }
  });
  setTimeout(() => ta.focus(), 30);
}

// ----------------------------------------------------------- backups view

async function viewBackups(id) {
  const s = serverById(id);
  if (!s) return h(`<div class="content"><div class="empty">Server not found.</div></div>`);

  const root = h(`<div class="viewhost">
    ${serverHeader(s, 'backups')}
    <div class="content">
      <div class="settings-head">
        <div>
          <h2 class="sect-title">Backups</h2>
          <p class="sect-sub">Saves are paused and flushed before the archive is taken, and file writes are blocked for the window.</p>
        </div>
        <div class="spacer"></div>
        <button type="button" class="btn btn-primary btn-sm" id="mkBackup"><i class="ico ico-sm ico-download"></i> Back up now</button>
      </div>
      <div style="padding:0 22px 26px">
        <div class="panelbox" style="margin:0"><div id="bkList"></div></div>
      </div>
    </div>
  </div>`);

  wireHeaderActions(root, id);
  const list = $('#bkList', root);

  const load = async () => {
    list.innerHTML = `<div class="row"><span class="spin"></span></div>`;
    try {
      const data = await api(`/api/servers/${id}/backups`);
      if (!data.backups.length) {
        list.innerHTML = `<div class="row muted">No backups yet.</div>`;
        return;
      }
      list.innerHTML = data.backups.map((b) => `
        <div class="row" data-bid="${esc(b.id)}">
          <i class="ico ico-sm ico-box" style="color:var(--t-2)"></i>
          <span class="mono" style="font-size:12px">${esc(b.id)}</span>
          <span class="muted">${esc(b.note || '')}</span>
          <span class="spacer"></span>
          <span class="muted num" style="font-size:11.5px">${humanBytes(b.size)}</span>
          <span class="muted" style="font-size:11.5px">${new Date(b.created * 1000).toLocaleString()}</span>
          <button type="button" class="btn btn-quiet btn-sm" data-restore>Restore</button>
          <button type="button" class="btn btn-ghost btn-sm btn-icon" data-del title="Delete"><i class="ico ico-sm ico-trash"></i></button>
        </div>`).join('');

      list.querySelectorAll('.row[data-bid]').forEach((row) => {
        const bid = row.dataset.bid;
        row.querySelector('[data-restore]').addEventListener('click', async () => {
          if (!confirm(`Restore ${bid}?\n\nThe current world is replaced. The server must be stopped.`)) return;
          try { await api(`/api/servers/${id}/backups/${bid}/restore`, { method: 'POST' }); toast('Restored'); }
          catch (e) { toast(e.message, 'err'); }
        });
        row.querySelector('[data-del]').addEventListener('click', async () => {
          if (!confirm(`Delete backup ${bid}?`)) return;
          try { await api(`/api/servers/${id}/backups/${bid}`, { method: 'DELETE' }); toast('Deleted'); load(); }
          catch (e) { toast(e.message, 'err'); }
        });
      });
    } catch (e) {
      list.innerHTML = `<div class="row" style="color:var(--offline)">${esc(e.message)}</div>`;
    }
  };

  $('#mkBackup', root).addEventListener('click', async () => {
    const note = prompt('Note for this backup (optional)') || '';
    const btn = $('#mkBackup', root);
    btn.disabled = true;
    btn.innerHTML = `<span class="spin"></span> Backing up…`;
    try { await api(`/api/servers/${id}/backups`, { method: 'POST', body: JSON.stringify({ note }) }); toast('Backup created'); }
    catch (e) { toast(e.message, 'err'); }
    btn.disabled = false;
    btn.innerHTML = `<i class="ico ico-sm ico-download"></i> Back up now`;
    load();
  });

  load();
  return root;
}

// ------------------------------------------------------------- panel admin

async function viewAdmin() {
  const root = h(`<div class="content"><div class="empty"><span class="spin"></span></div></div>`);
  let me, users = [], audit = [];
  try {
    me = await api('/api/me');
    if ((me.user && me.user.role === 'admin') || me.unclaimed) {
      try { users = (await api('/api/users')).users; } catch {}
    }
    audit = (await api('/api/audit?limit=80')).entries;
  } catch (e) { /* audit needs a session when auth is on */ }

  const host = state.host;
  // An unclaimed panel has no accounts, so nobody is an admin - but everyone
  // can do everything, which is what the admin affordances need to reflect.
  const isAdmin = !!(me && (me.unclaimed || (me.user && me.user.role === 'admin')));

  root.innerHTML = `
    <div class="page-head"><h1 class="page-title">Panel settings</h1></div>

    <div class="panelbox">
      <h3>Agent</h3>
      <div class="row"><span class="k">Version</span><span>${host ? esc(host.agent.version) : '-'}</span></div>
      <div class="row"><span class="k">Docker</span><span>${host && host.docker ? 'reachable' : 'not reachable'}</span></div>
      <div class="row"><span class="k">Host CPU</span><span>${host ? host.cpu.total_vcpu : '-'} vCPU</span></div>
      <div class="row"><span class="k">Signed in as</span><span>${
        me && me.user ? `${esc(me.user.name)} <span class="badge-mute">${esc(me.user.role)}</span>`
        : me && me.unclaimed ? `<span style="color:var(--amber)">nobody - this panel has no accounts yet</span>`
        : 'not signed in'
      }</span></div>
    </div>

    <div class="panelbox">
      <h3>Appearance</h3>
      <div class="row">
        <span class="k">Accent</span>
        <span class="swatches" id="themeSwatches">
          <span class="swatch" data-theme=""       style="background:#95c83d" title="Arcade green"></span>
          <span class="swatch" data-theme="ocean"  style="background:#3fa9f5" title="Ocean"></span>
          <span class="swatch" data-theme="violet" style="background:#9b7bf0" title="Violet"></span>
          <span class="swatch" data-theme="amber"  style="background:#e0a33c" title="Amber"></span>
          <span class="swatch" data-theme="rose"    style="background:#ef6f8b" title="Rose"></span>
          <span class="swatch" data-theme="cyan"    style="background:#22d3ee" title="Cyan"></span>
          <span class="swatch" data-theme="crimson" style="background:#e23b4a" title="Crimson"></span>
          <span class="swatch" data-theme="indigo"  style="background:#6366f1" title="Indigo"></span>
        </span>
        <span class="spacer"></span>
        <span class="muted" style="font-size:11.5px">Applies immediately, stored in this browser.</span>
      </div>
      <div class="row">
        <span class="k">Background</span>
        <span class="bgswatches" id="bgSwatches">
          <span class="bgswatch" data-bg="plain"      title="Plain"></span>
          <span class="bgswatch" data-bg="waves"      title="Waves"></span>
          <span class="bgswatch" data-bg="grid"       title="Grid"></span>
          <span class="bgswatch" data-bg="dots"       title="Dots"></span>
          <span class="bgswatch" data-bg="scanlines"  title="Scanlines"></span>
          <span class="bgswatch" data-bg="aurora"     title="Aurora"></span>
          <span class="bgswatch" data-bg="neon"       title="Neon dusk"></span>
        </span>
        <span class="spacer"></span>
        <span class="muted" style="font-size:11.5px">Waves, Grid, Dots and Scanlines follow the accent; Aurora and Neon are fixed palettes.</span>
      </div>
      <div class="row">
        <span class="k">Surface</span>
        <div class="seg2" style="width:300px" id="contrastSeg">
          <button type="button" data-contrast="dim">Dim</button>
          <button type="button" data-contrast="">Default</button>
          <button type="button" data-contrast="high">High contrast</button>
        </div>
      </div>
    </div>

    <div class="panelbox">
      <h3>Access</h3>
      ${me && me.needs_setup ? `
        <div class="row"><span class="k">Status</span>
          <span style="color:var(--amber)">No users yet - the panel is open to anyone who can reach it.</span></div>
        <div class="row"><span class="k">Setup token</span>
          <span class="spacer"></span>
          <input class="inp" id="suToken" placeholder="from the panel's log" style="width:330px">
        </div>
        <div class="row"><span class="k"></span>
          <span class="spacer"></span>
          <span class="muted" style="font-size:12px">Printed at startup: <code>journalctl -u teploy-arcade | grep &quot;Bootstrap token&quot;</code>. Valid 30 minutes; restart the panel for a fresh one.</span>
        </div>
        <div class="row"><span class="k">Create first admin</span>
          <span class="spacer"></span>
          <input class="inp" id="suName" placeholder="username" style="width:150px">
          <input class="inp" id="suPass" type="password" placeholder="password (8+)" style="width:170px">
          <button type="button" class="btn btn-primary btn-sm" id="suBtn">Create</button>
        </div>` : `
        <div class="row"><span class="k">Status</span><span>Sign-in required.</span></div>
        ${users.map((u) => `<div class="row">
            <span class="k">${esc(u.name)}</span>
            <span class="badge-mute">${esc(u.role)}</span>
            <span class="spacer"></span>
            ${isAdmin ? `<button type="button" class="btn btn-ghost btn-sm btn-icon" data-deluser="${esc(u.name)}"><i class="ico ico-sm ico-trash"></i></button>` : ''}
          </div>`).join('')}
        ${isAdmin ? `<div class="row">
          <span class="k">Add user</span>
          <span class="spacer"></span>
          <input class="inp" id="nuName" placeholder="username" style="width:140px">
          <input class="inp" id="nuPass" type="password" placeholder="password" style="width:150px">
          <select class="inp" id="nuRole" style="width:110px">
            <option value="viewer">viewer</option>
            <option value="operator">operator</option>
            <option value="admin">admin</option>
          </select>
          <button type="button" class="btn btn-sm" id="nuBtn">Add</button>
        </div>` : ''}
        <div class="row"><span class="k"></span><span class="spacer"></span>
          <button type="button" class="btn btn-quiet btn-sm" id="logoutBtn">Sign out</button></div>`}
    </div>

    <div class="panelbox">
      <h3>Audit log</h3>
      ${audit.length ? audit.map((e) => `<div class="row">
          <span class="muted" style="width:150px;flex:none;font-size:11.5px">${new Date(e.ts * 1000).toLocaleString()}</span>
          <span style="width:110px;flex:none">${esc(e.actor)}</span>
          <span class="mono" style="font-size:12px;width:190px;flex:none">${esc(e.action)}</span>
          <span class="muted" style="font-size:12px">${esc(e.target)} ${esc(e.detail)}</span>
        </div>`).join('') : '<div class="row muted">Nothing recorded yet.</div>'}
    </div>

    <div class="panelbox">
      <h3>Console commands (simulator)</h3>
      <div class="row"><span class="k"><code>help</code> / <code>list</code></span><span class="muted">The usual operator commands.</span></div>
      <div class="row"><span class="k"><code>flood</code></span><span class="muted">5000 lines at once - shows backpressure and the dropped-line marker.</span></div>
      <div class="row"><span class="k"><code>crash</code></span><span class="muted">Force an OOM exit to reach the failed state.</span></div>
    </div>`;

  // Appearance is a browser preference, not panel state - it is deliberately
  // not persisted server-side, so two operators can differ.
  const applyTheme = (theme) => {
    if (theme) document.documentElement.setAttribute('data-theme', theme);
    else document.documentElement.removeAttribute('data-theme');
    try { localStorage.setItem('arcade-theme', theme || ''); } catch {}
    root.querySelectorAll('#themeSwatches .swatch').forEach((s) =>
      s.classList.toggle('on', (s.dataset.theme || '') === (theme || '')));
  };
  const applyContrast = (c) => {
    if (c) document.documentElement.setAttribute('data-contrast', c);
    else document.documentElement.removeAttribute('data-contrast');
    try { localStorage.setItem('arcade-contrast', c || ''); } catch {}
    root.querySelectorAll('#contrastSeg button').forEach((b) =>
      b.classList.toggle('on', (b.dataset.contrast || '') === (c || '')));
  };

  const applyBg = (bg) => {
    if (bg && bg !== 'plain') document.documentElement.setAttribute('data-bg', bg);
    else document.documentElement.setAttribute('data-bg', 'plain');
    try { localStorage.setItem('arcade-bg', bg || 'plain'); } catch {}
    root.querySelectorAll('#bgSwatches .bgswatch').forEach((s) =>
      s.classList.toggle('on', s.dataset.bg === (bg || 'plain')));
  };

  let curTheme = '', curContrast = '', curBg = 'plain';
  try {
    curTheme = localStorage.getItem('arcade-theme') || '';
    curContrast = localStorage.getItem('arcade-contrast') || '';
    curBg = localStorage.getItem('arcade-bg') || 'plain';
  } catch {}
  applyTheme(curTheme);
  applyContrast(curContrast);
  applyBg(curBg);

  root.querySelectorAll('#themeSwatches .swatch').forEach((s) =>
    s.addEventListener('click', () => applyTheme(s.dataset.theme)));
  root.querySelectorAll('#contrastSeg button').forEach((b) =>
    b.addEventListener('click', () => applyContrast(b.dataset.contrast)));
  root.querySelectorAll('#bgSwatches .bgswatch').forEach((s) =>
    s.addEventListener('click', () => applyBg(s.dataset.bg)));

  const su = $('#suBtn', root);
  if (su) su.addEventListener('click', async () => {
    try {
      await api('/api/setup', {
        method: 'POST',
        body: JSON.stringify({
          name: $('#suName', root).value,
          password: $('#suPass', root).value,
          token: $('#suToken', root).value.trim(),
        }),
      });
      toast('Admin created - auth is now enforced');
      router(true);
    } catch (e) { toast(e.message, 'err'); }
  });

  const nu = $('#nuBtn', root);
  if (nu) nu.addEventListener('click', async () => {
    try {
      await api('/api/users', {
        method: 'POST',
        body: JSON.stringify({
          name: $('#nuName', root).value, password: $('#nuPass', root).value,
          role: $('#nuRole', root).value,
        }),
      });
      toast('User added'); router(true);
    } catch (e) { toast(e.message, 'err'); }
  });

  root.querySelectorAll('[data-deluser]').forEach((b) =>
    b.addEventListener('click', async () => {
      if (!confirm(`Delete user ${b.dataset.deluser}?`)) return;
      try { await api(`/api/users/${encodeURIComponent(b.dataset.deluser)}`, { method: 'DELETE' }); router(true); }
      catch (e) { toast(e.message, 'err'); }
    }));

  const lo = $('#logoutBtn', root);
  if (lo) lo.addEventListener('click', async () => {
    await api('/api/logout', { method: 'POST' });
    location.reload();
  });

  return root;
}

// ------------------------------------------------------------------ login

function viewLogin() {
  const root = h(`<div class="loginwrap">
    <form class="loginbox">
      <div class="row-flex" style="gap:11px;margin-bottom:18px">
        <span class="gm gm-purpur"></span>
        <div><div style="font-size:16px;font-weight:600">teploy-arcade</div>
        <div class="muted" style="font-size:12px">Sign in to the panel</div></div>
      </div>
      <div class="field" style="margin-bottom:12px">
        <label style="display:block;font-size:12px;color:var(--t-1);margin-bottom:6px">Username</label>
        <input class="inp" id="lgName" autocomplete="username">
      </div>
      <div class="field" style="margin-bottom:16px">
        <label style="display:block;font-size:12px;color:var(--t-1);margin-bottom:6px">Password</label>
        <input class="inp" id="lgPass" type="password" autocomplete="current-password">
      </div>
      <button type="submit" class="btn btn-primary" id="lgBtn" style="width:100%;justify-content:center">Sign in</button>
      <div id="lgErr" style="color:var(--offline);font-size:12px;margin-top:12px"></div>
    </form>
  </div>`);

  const submit = async (ev) => {
    ev.preventDefault();
    try {
      await api('/api/login', {
        method: 'POST',
        body: JSON.stringify({ name: $('#lgName', root).value, password: $('#lgPass', root).value }),
      });
      location.reload();
    } catch (e) { $('#lgErr', root).textContent = e.message; }
  };
  root.querySelector('.loginbox').addEventListener('submit', submit);
  setTimeout(() => $('#lgName', root).focus(), 40);
  return root;
}

window.extraViews = { viewFiles, viewBackups, viewAdmin, viewLogin, openEditor };
