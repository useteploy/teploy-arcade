/* views-import.js - Import an existing server directory.
 *
 * Two decisions this screen exists to make honest, because both are expensive
 * to get wrong: what the directory actually is - the panel says "unrecognised"
 * and asks, rather than picking a template for you - and whether to copy it or
 * manage it where it stands. The second one can damage somebody else's live
 * server, so it is a deliberate choice with its consequence written beside it
 * rather than a checkbox.
 */

const IMPORT_MODES = [
  {
    key: 'copy', icon: 'download', title: 'Copy into the panel', rec: '(recommended)',
    text: 'The panel copies the directory into its own data folder and manages the copy. ' +
      'The original is never written to, so a server another panel is still running keeps running.',
  },
  {
    key: 'adopt', icon: 'import', title: 'Adopt where it is',
    text: 'Nothing is copied, so a multi-gigabyte world costs no disk. The panel then writes into ' +
      'that directory - settings, files, player lists - so anything else managing it has to be stopped first. ' +
      'Deleting the server here removes the panel entry only, never the directory.',
  },
];

async function viewImport() {
  let templates = [];
  try { templates = (await api('/api/templates')).templates; } catch (e) { toast(e.message, 'err'); }

  const root = h(`<div class="content">
    <div class="page-head">
      <h1 class="page-title">Import a server</h1>
      <div class="link-actions"><a href="#/servers">Back to servers</a></div>
    </div>

    <div class="panelbox">
      <h3>Where is it</h3>
      <div class="row">
        <span class="k">Full path on this host</span>
        <input class="inp mono" id="impPath" style="width:420px" placeholder="/srv/minecraft/survival" autocomplete="off">
        <button class="btn btn-primary btn-sm" id="impScan"><i class="ico ico-sm ico-search"></i> Scan</button>
      </div>
      <div class="row">
        <span class="k"></span>
        <span class="muted" style="font-size:11.5px">A scan reads that directory and changes nothing in it.</span>
      </div>
    </div>

    <div id="impResult"></div>
  </div>`);

  const out = $('#impResult', root);
  let scan = null, mode = 'copy', runtime = 'sim', timer = null;

  const stopPolling = () => { if (timer) { clearInterval(timer); timer = null; } };
  root.addEventListener('gss:teardown', stopPolling);

  const markFor = (slug) => {
    const t = templates.find((x) => x.slug === slug);
    return t ? t.mark : 'vanilla';
  };
  const row = (k, v) => `<div class="row"><span class="k">${esc(k)}</span><span>${v}</span></div>`;

  const doScan = async () => {
    const path = $('#impPath', root).value.trim();
    if (!path) { toast('Give the full path to the server directory.', 'warn'); return; }
    const btn = $('#impScan', root);
    btn.disabled = true;
    btn.innerHTML = `<span class="spin"></span> Scanning…`;
    out.innerHTML = '';
    try {
      scan = await api('/api/import/scan', { method: 'POST', body: JSON.stringify({ path }) });
      renderScan();
    } catch (e) {
      scan = null;
      out.innerHTML = `<div class="panelbox"><div class="row" style="color:var(--offline)">${esc(e.message)}</div></div>`;
    }
    btn.disabled = false;
    btn.innerHTML = `<i class="ico ico-sm ico-search"></i> Scan`;
  };

  const renderScan = () => {
    const s = scan;

    if (!s.is_server) {
      out.innerHTML = `<div class="panelbox">
        <h3>Not a server directory</h3>
        <div class="row"><span style="color:var(--amber)">${esc(s.reason)}</span></div>
        <div class="row"><span class="muted mono" style="font-size:11.5px">${esc(s.path)}</span></div>
      </div>`;
      return;
    }

    // The identity line is the whole point of the scan: it either names the
    // software or says plainly that it will not guess.
    const identity = s.recognised
      ? `<span class="gm gm-sm gm-${esc(markFor(s.template))}"></span>
         <b>${esc(s.template_name || s.template)}</b>
         ${s.version ? `<span class="mono">${esc(s.version)}</span>` : '<span class="badge-mute">version unknown</span>'}
         ${s.proxy ? '<span class="badge-mute">proxy</span>' : ''}
         <span class="muted mono" style="font-size:11.5px">${esc(s.jar)}</span>`
      : `<span style="color:var(--amber)">Unrecognised.</span> <span class="muted">${esc(s.reason)}</span>`;

    const warnings = (s.warnings || []).map((w) =>
      `<div class="row"><div class="warnbox" style="flex:1"><i class="ico ico-sm ico-warning"></i><span>${esc(w)}</span></div></div>`).join('');

    out.innerHTML = `
      <div class="panelbox">
        <h3>What was found</h3>
        ${row('Server software', identity)}
        ${row('Port', s.port
          ? `${s.port} <span class="muted" style="font-size:11.5px">from ${esc(s.port_source)}</span>
             ${s.port_taken_by ? `<span style="color:var(--amber)">already used here by ${esc(s.port_taken_by)}</span>` : ''}`
          : '<span class="muted">none found</span>')}
        ${row('MOTD', s.motd ? esc(s.motd) : '<span class="muted">none</span>')}
        ${row('Max players', s.max_players || '<span class="muted">not set</span>')}
        ${row('World', s.has_world
          ? `${esc(s.world)} <span class="muted" style="font-size:11.5px">has a level.dat</span>`
          : (s.proxy ? '<span class="muted">none - a proxy has no world</span>' : '<span class="muted">none found</span>'))}
        ${s.mods ? row('Mods', `${s.mods} jars`) : ''}
        ${s.plugins ? row('Plugins', `${s.plugins} jars`) : ''}
        ${row('Size on disk', `${esc(s.size_human)} <span class="muted" style="font-size:11.5px">in ${s.files} files</span>`)}
        ${s.markers && s.markers.length
          ? row('Files found', `<span class="mono muted" style="font-size:11.5px">${esc(s.markers.join('   '))}</span>`) : ''}
        ${s.managed_by && s.managed_by.length ? row('Managed by', esc(s.managed_by.join(', '))) : ''}
      </div>

      ${warnings ? `<div class="panelbox"><h3>Worth knowing first</h3>${warnings}</div>` : ''}

      <div class="panelbox">
        <h3>How to import it</h3>
        <div class="row" style="display:block">
          <div class="tpl-grid" style="grid-template-columns:repeat(2,minmax(0,1fr))" id="impModes">
            ${IMPORT_MODES.map((m) => `
              <div class="tpl" data-mode="${m.key}">
                <i class="ico ico-lg ico-${m.icon}" style="color:var(--t-1)"></i>
                <div>
                  <div class="tn">${esc(m.title)} ${m.rec ? `<span class="rec">${esc(m.rec)}</span>` : ''}</div>
                  <div class="td">${esc(m.text)}</div>
                </div>
              </div>`).join('')}
          </div>
        </div>
        <div class="row" id="modeNote"></div>
      </div>

      <div class="panelbox">
        <h3>The server this creates</h3>
        <div class="row">
          <span class="k">Name</span>
          <input class="inp" id="impName" style="width:280px" value="${esc(s.suggested_name || '')}">
          <span class="muted" style="font-size:11.5px">from ${esc(s.name_source || 'the directory name')}</span>
        </div>
        <div class="row">
          <span class="k">Server software</span>
          <select class="inp" id="impTemplate" style="width:220px">
            ${s.recognised ? '' : '<option value="" selected>Choose one…</option>'}
            ${templates.map((t) =>
              `<option value="${esc(t.slug)}" ${t.slug === s.template ? 'selected' : ''}>${esc(t.name)}</option>`).join('')}
          </select>
          <span class="muted" style="font-size:11.5px">${s.recognised
            ? 'Detected from the jar.' : 'Not detected - the panel will not guess this one.'}</span>
        </div>
        <div class="row">
          <span class="k">Port</span>
          <input class="inp mono" id="impPort" style="width:110px" value="${s.port || ''}">
          <span class="muted" style="font-size:11.5px">${s.port_taken_by
            ? 'Change this - the scanned port is already in use in this panel.'
            : 'Read from the server’s own config.'}</span>
        </div>
        <div class="row">
          <span class="k">Runtime</span>
          <div class="seg2" style="width:260px" id="impRuntime">
            <button data-rt="sim" class="on">Simulator</button>
            <button data-rt="docker">Docker</button>
          </div>
        </div>
        <div class="row">
          <span class="k"></span>
          <span class="muted" style="font-size:11.5px;line-height:1.5">${esc(s.runtime_note)}</span>
        </div>
        <div class="row">
          <span class="k"></span>
          <span class="spacer"></span>
          <button class="btn btn-primary" id="impGo"><i class="ico ico-sm ico-import"></i> Import</button>
        </div>
      </div>

      <div class="panelbox" id="impProgress" style="display:none">
        <h3>Importing</h3>
        <div class="row">
          <span class="k" id="impState">Starting…</span>
          <span class="spacer"></span>
          <span class="muted mono" id="impBytes" style="font-size:11.5px"></span>
        </div>
        <div class="row" style="display:block">
          <div class="bar"><i id="impBar" style="width:0%"></i></div>
          <div class="muted mono" id="impCurrent" style="font-size:11px;margin-top:7px;min-height:14px"></div>
        </div>
      </div>`;

    const setMode = (m) => {
      mode = m;
      out.querySelectorAll('#impModes .tpl').forEach((el) =>
        el.classList.toggle('is-picked', el.dataset.mode === m));
      // Said again next to the button, in the terms that matter for this
      // directory: its size, its path, and who else is holding it.
      $('#modeNote', out).innerHTML = m === 'copy'
        ? `<span class="muted">${esc(s.size_human)} will be copied. <span class="mono">${esc(s.path)}</span> is left exactly as it is.</span>`
        : `<span style="color:var(--amber)">The panel will manage <span class="mono">${esc(s.path)}</span> directly${
            s.managed_by && s.managed_by.length
              ? ` - ${esc(s.managed_by.join(' and '))} still manages it, so stop that first` : ''}.</span>`;
    };
    out.querySelectorAll('#impModes .tpl').forEach((el) =>
      el.addEventListener('click', () => setMode(el.dataset.mode)));
    // A copy that will not fit is not a default worth offering.
    setMode(s.enough_space ? 'copy' : 'adopt');

    out.querySelectorAll('#impRuntime button').forEach((b) =>
      b.addEventListener('click', () => {
        runtime = b.dataset.rt;
        out.querySelectorAll('#impRuntime button').forEach((x) => x.classList.toggle('on', x === b));
      }));

    $('#impGo', out).addEventListener('click', start);
  };

  const start = async () => {
    const template = $('#impTemplate', out).value;
    if (!template) { toast('Choose which server software this is.', 'warn'); return; }

    const btn = $('#impGo', out);
    btn.disabled = true;
    btn.innerHTML = `<span class="spin"></span> Importing…`;
    const idle = () => {
      btn.disabled = false;
      btn.innerHTML = `<i class="ico ico-sm ico-import"></i> Import`;
    };

    let job;
    try {
      job = await api('/api/import', {
        method: 'POST',
        body: JSON.stringify({
          path: scan.path,
          name: $('#impName', out).value.trim(),
          mode, template, runtime,
          port: parseInt($('#impPort', out).value || '0', 10) || 0,
        }),
      });
    } catch (e) { toast(e.message, 'err'); idle(); return; }

    $('#impProgress', out).style.display = '';
    renderJob(job);

    // Polled, not pushed: the copy is the one thing here that can run for
    // minutes, and the panel's event feed carries server state - this is not a
    // server yet.
    timer = setInterval(async () => {
      let j;
      try { j = await api(`/api/import/${encodeURIComponent(job.id)}`); }
      catch (e) { stopPolling(); toast(e.message, 'err'); idle(); return; }
      renderJob(j);
      if (j.state === 'running') return;
      stopPolling();
      if (j.state !== 'done') { toast(j.error || 'The import failed.', 'err'); idle(); return; }
      toast(`Imported ${j.name}`);
      // Refresh the list before navigating: the new server reaches the browser
      // over the event feed, and landing on its page before that arrives shows
      // "server not found" for a server that imported perfectly.
      try {
        const d = await api('/api/servers');
        state.servers = d.servers;
        state.host = d.host;
      } catch { /* the feed will catch up */ }
      location.hash = `#/s/${j.server_id}/files`;
    }, 700);
  };

  const renderJob = (j) => {
    $('#impBar', out).style.width = `${j.percent || 0}%`;
    $('#impState', out).textContent = j.state === 'done' ? 'Done'
      : j.state === 'failed' ? 'Failed'
        : j.mode === 'adopt' ? 'Linking…' : 'Copying…';
    $('#impBytes', out).textContent = j.total_bytes
      ? `${humanBytes(j.copied_bytes)} of ${humanBytes(j.total_bytes)}`
      : '';
    $('#impCurrent', out).textContent = j.error || j.current_file || '';
  };

  $('#impScan', root).addEventListener('click', doScan);
  $('#impPath', root).addEventListener('keydown', (e) => { if (e.key === 'Enter') doScan(); });
  setTimeout(() => $('#impPath', root).focus(), 40);
  return root;
}

Object.assign(window.extraViews, { viewImport });
