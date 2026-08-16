/* views-plugins.js - the Plugins tab.
 *
 * What is installed, what is switched off, and installing one from a URL.
 * Enabling and disabling renames between "x.jar" and "x.jar.disabled", so a
 * plugin switched off is still on disk with its version intact.
 *
 * There is no catalogue browser on purpose, and the screen says so rather than
 * carrying a button that does nothing.
 */

async function viewPlugins(id) {
  const s = serverById(id);
  if (!s) return h(`<div class="content"><div class="empty">Server not found.</div></div>`);

  const root = h(`<div class="viewhost">
    ${serverHeader(s, 'plugins')}
    <div class="content">
      <div class="settings-head">
        <div>
          <h2 class="sect-title" id="plTitle">Plugins</h2>
          <p class="sect-sub" id="plSub">Reading the server's own plugin directory.</p>
        </div>
        <div class="spacer"></div>
        <span class="pill-warn" id="plRestart" style="display:none"><i class="ico ico-sm ico-restart"></i> Restart required</span>
      </div>
      <div style="padding:0 22px 26px">
        <div class="panelbox" style="margin:0 0 16px"><div id="plList"></div></div>
        <div class="panelbox" style="margin:0">
          <h3>Installing</h3>
          <div class="row"><span class="k">From a URL</span><span class="muted">Paste a direct link to a
            <span class="mono">.jar</span> in the bar below. The download is capped, must be http or https,
            and must actually be a jar.</span></div>
          <div class="row"><span class="k">Finding a plugin</span><span class="muted">Copy the download link
            from Modrinth, Hangar or SpigotMC and paste it below. The panel does not proxy those
            indexes, so the link you use is the one the author published.</span></div>
          <div class="row"><span class="k">Uploading a file</span><span class="muted">Use the Files tab -
            it writes into the same directory.</span></div>
        </div>
      </div>
    </div>
    <div class="addbar" id="plBar" style="display:none">
      <span class="sigil"><i class="ico ico-sm ico-download"></i></span>
      <input class="inp" id="plURL" placeholder="https://example.com/plugin.jar" autocomplete="off">
      <button type="button" class="btn btn-primary" id="plInstall">Install</button>
    </div>
  </div>`);

  wireHeaderActions(root, id);

  const list = $('#plList', root);
  let data = { entries: [] };

  // Only flagged once something has actually changed this visit. A permanent
  // banner would be noise, and whether it is needed at all is the agent's
  // answer, not the panel's - a stopped server needs no restart, it just starts
  // with the new set.
  //
  // display, not the hidden attribute: these elements carry their own `display`
  // from the stylesheet, which outranks the UA rule behind `hidden`.
  const flagRestart = (res) => {
    $('#plRestart', root).style.display = res.requires_restart ? '' : 'none';
    toast(res.note || 'Saved.');
  };

  const render = () => {
    $('#plTitle', root).textContent = data.kind === 'mod' ? 'Mods' : 'Plugins';
    $('#plBar', root).style.display = data.supported ? '' : 'none';

    if (!data.supported) {
      $('#plSub', root).textContent = 'Nothing to manage for this server type.';
      list.innerHTML = `<div class="row muted">${esc(data.reason || 'This server has no plugin loader.')}</div>`;
      return;
    }

    const noun = data.kind === 'mod' ? 'mod' : 'plugin';
    $('#plSub', root).innerHTML =
      `Reading <span class="mono">${esc(data.dir)}/</span> &middot; ${data.entries.length} ${noun}${data.entries.length === 1 ? '' : 's'} installed`;

    if (!data.entries.length) {
      list.innerHTML = `<div class="row muted">Nothing in <span class="mono">${esc(data.dir)}/</span> yet.</div>`;
      return;
    }

    list.innerHTML = data.entries.map((e) => `
      <div class="row" data-file="${esc(e.file)}">
        <i class="ico ico-sm ico-box" style="color:${e.enabled ? 'var(--lime)' : 'var(--t-2)'}"></i>
        <span style="flex:1;min-width:0">
          <div><b>${esc(e.name)}</b> ${e.enabled ? '' : '<span class="badge-mute">disabled</span>'}</div>
          <div class="muted mono" style="font-size:11px">${esc(e.file)}</div>
        </span>
        <span class="muted num" style="font-size:11.5px">${humanBytes(e.size)}</span>
        <span class="muted" style="font-size:11.5px;width:160px;text-align:right;white-space:nowrap">
          ${new Date(e.mod * 1000).toLocaleDateString()} ${new Date(e.mod * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}
        </span>
        <button type="button" class="btn btn-quiet btn-sm" data-toggle>${e.enabled ? 'Disable' : 'Enable'}</button>
        <button type="button" class="btn btn-ghost btn-sm btn-icon" data-rm title="Delete">
          <i class="ico ico-sm ico-trash" style="color:var(--offline)"></i></button>
      </div>`).join('');

    list.querySelectorAll('.row[data-file]').forEach((row) => {
      const file = row.dataset.file;
      const entry = data.entries.find((x) => x.file === file);
      row.querySelector('[data-toggle]').addEventListener('click', async (ev) => {
        const b = ev.currentTarget;
        b.disabled = true;
        try {
          const res = await api(`/api/servers/${id}/plugins/toggle`, {
            method: 'POST',
            body: JSON.stringify({ file, enable: !entry.enabled }),
          });
          flagRestart(res);
        } catch (e) { toast(e.message, 'err'); }
        load();
      });
      row.querySelector('[data-rm]').addEventListener('click', async () => {
        // Disabling is the reversible version of this, so say so before the
        // operator loses the jar and the version that worked with it.
        if (!confirm(`Delete ${entry.name}? This removes the jar.\n\nDisable it instead to switch it off and keep it.`)) return;
        try {
          const res = await api(`/api/servers/${id}/plugins?file=${encodeURIComponent(file)}`, { method: 'DELETE' });
          flagRestart(res);
        } catch (e) { toast(e.message, 'err'); }
        load();
      });
    });
  };

  const load = async () => {
    try {
      data = await api(`/api/servers/${id}/plugins`);
      render();
    } catch (e) {
      list.innerHTML = `<div class="row" style="color:var(--offline)">${esc(e.message)}</div>`;
    }
  };

  const install = async () => {
    const url = $('#plURL', root).value.trim();
    if (!url) return;
    const btn = $('#plInstall', root);
    btn.disabled = true;
    btn.textContent = 'Downloading…';
    try {
      const res = await api(`/api/servers/${id}/plugins/install`, {
        method: 'POST',
        body: JSON.stringify({ url }),
      });
      $('#plURL', root).value = '';
      toast(`Installed ${res.plugin.name}`);
      flagRestart(res);
    } catch (e) { toast(e.message, 'err'); }
    btn.disabled = false;
    btn.textContent = 'Install';
    load();
  };

  $('#plInstall', root).addEventListener('click', install);
  $('#plURL', root).addEventListener('keydown', (e) => { if (e.key === 'Enter') install(); });

  load();
  return root;
}

Object.assign(window.extraViews, { viewPlugins });
