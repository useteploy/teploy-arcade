/* Execute the shipped modal controllers with deterministic DOM/API/timer seams.
 * No browser, backend, network, external packages, or real timers are used.
 * Browser layout/focus/keyboard acceptance remains a separate check.
 *
 *   node test/ui-lifecycle.test.js
 */
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const runTest = require('node:test');
// A lifecycle regression may leave a fixture promise unresolved. Fail that
// case instead of hanging the entire frontend gate.
const test = (name, body) => runTest(name, { timeout: 2000 }, body);

function sourceFunction(file, name) {
  const src = fs.readFileSync(path.join(__dirname, '..', 'cmd', 'teploy-arcade', 'frontend', file), 'utf8');
  const match = src.match(new RegExp(`^(?:async )?function ${name}\\([^]*?^\\}`, 'm'));
  assert.ok(match, `Shipped ${name} must exist; do not replace it with a test copy`);
  return match[0];
}

class Element {
  constructor() {
    this.listeners = new Map();
    this.elements = new Map();
    this.children = [];
    this.value = '';
    this.disabled = false;
    this.hidden = true;
    this.removed = false;
    this.style = {};
    this.innerHTML = '';
    this.textContent = '';
    this.focused = false;
    const classes = new Set();
    this.classList = {
      contains: name => classes.has(name),
      toggle: (name, on) => on ? classes.add(name) : classes.delete(name),
    };
  }
  addEventListener(name, listener) {
    if (!this.listeners.has(name)) this.listeners.set(name, new Set());
    this.listeners.get(name).add(listener);
  }
  removeEventListener(name, listener) { this.listeners.get(name)?.delete(listener); }
  async emit(name, event = {}) {
    for (const listener of [...(this.listeners.get(name) || [])]) await listener(event);
  }
  // Deliberately bypass native disabled-button dispatch to test the shared
  // submission guard against queued click/keyboard activation callbacks.
  activate() { return this.emit('click', { currentTarget: this, preventDefault() {} }); }
  appendChild(child) { this.children.push(child); }
  remove() { this.removed = true; }
  focus() { this.focused = true; }
  querySelector(selector) {
    assert.ok(this.elements.has(selector), `Missing element ${selector}`);
    return this.elements.get(selector);
  }
}

function fixture() {
  const host = new Element(), window = new Element();
  const document = new Element();
  document.elements.set('#modalHost', host);
  const requests = [], timers = new Map(), toasts = [], modals = [];
  let nextTimer = 1;
  const initialServers = [{ id: 's1', name: 'Fixture', template: 'paper', version: '1.20.4' }];
  const context = {
    window, AbortController,
    state: { servers: initialServers, host: { original: true } },
    location: { hash: '#/s/s1/scheduler' },
    $: (selector, root = document) => root.querySelector(selector),
    h: html => {
      const modal = new Element();
      for (const match of html.matchAll(/<([a-z]+)\b([^>]*\bid="([^"]+)"[^>]*)>/g)) {
        const el = new Element();
        el.value = match[2].match(/\bvalue="([^"]*)"/)?.[1] || '';
        for (const cls of (match[2].match(/\bclass="([^"]*)"/)?.[1] || '').split(' ')) {
          if (cls) el.classList.toggle(cls, true);
        }
        modal.elements.set(`#${match[3]}`, el);
      }
      if (modal.elements.has('#clGo')) {
        modal.querySelector('#clGo').innerHTML = '<i class="ico ico-sm ico-layers"></i> Clone';
        modal.querySelector('#clSrc').value = 's1';
      }
      modals.push(modal);
      return modal;
    },
    esc: value => String(value ?? ''), humanBytes: String,
    toast: (...args) => toasts.push(args),
    setTimeout: (callback, delay) => { const id = nextTimer++; timers.set(id, { callback, delay }); return id; },
    clearTimeout: id => timers.delete(id),
    // Support the old source too, so regression tests fail on behavior instead
    // of failing only because the fixture lacks a replaced timer API.
    setInterval: (callback, delay) => { const id = nextTimer++; timers.set(id, { callback, delay, interval: true }); return id; },
    clearInterval: id => timers.delete(id),
    api: (url, options = {}) => new Promise((resolve, reject) => {
      // Abort intentionally does not settle the fake request. This lets tests
      // deliver late responses despite abort, as can happen after body decode.
      requests.push({ url, ...options, resolve, reject });
    }),
  };
  vm.createContext(context);
  vm.runInContext(sourceFunction('views.js', 'openClone') + '\n' + sourceFunction('views2.js', 'taskDialog'), context);
  return {
    context, requests, timers, toasts, modals, initialServers,
    clone() { context.openClone('s1'); return modals.at(-1); },
    task(task = null, onSaved = () => {}) { context.taskDialog('s1', task, onSaved); return modals.at(-1); },
    tick(delay) {
      const entry = [...timers].find(([, timer]) => delay === undefined || timer.delay === delay);
      assert.ok(entry, `Expected a timer${delay === undefined ? '' : ` for ${delay}ms`}`);
      const [id, timer] = entry;
      if (!timer.interval) timers.delete(id);
      return timer.callback();
    },
    async navigate(hash) { context.location.hash = hash; await window.emit('hashchange'); },
    hashListeners() { return window.listeners.get('hashchange')?.size || 0; },
  };
}
const get = (modal, selector) => modal.querySelector(selector);
const done = id => ({ state: 'done', name: `Copy ${id}`, server_id: id, percent: 100 });
const microtasks = async () => { await Promise.resolve(); await Promise.resolve(); };
async function startClone(f, modal = f.clone(), id = 'job1') {
  const submission = get(modal, '#clGo').activate();
  f.requests.at(-1).resolve({ id });
  await submission;
  return modal;
}
async function completePoll(f, poll, id = 's2') {
  f.requests.at(-1).resolve(done(id));
  await microtasks();
  const refresh = f.requests.at(-1);
  assert.equal(refresh.url, '/api/servers');
  refresh.resolve({ servers: [{ id }], host: { updated: true } });
  await poll;
}

for (const selector of ['#clCancel', '#cx']) {
  test(`clone ${selector} before POST settles aborts and ignores late success`, async () => {
    const f = fixture(), modal = f.clone(), originalHash = f.context.location.hash;
    const pending = get(modal, '#clGo').activate();
    const repeated = get(modal, '#clGo').activate();
    assert.equal(f.requests.length, 1, 'No duplicate clone submission');
    await repeated;
    await get(modal, selector).activate();
    assert.equal(f.requests[0].signal.aborted, true);
    f.requests[0].resolve({ id: 'late-job' });
    await pending;
    await get(modal, '#clGo').activate();
    assert.equal(f.requests.length, 1);
    assert.equal(f.timers.size, 0);
    assert.equal(f.context.location.hash, originalHash);
    assert.equal(f.toasts.length, 0);
    assert.equal(f.hashListeners(), 0);
    assert.equal(modal.removed, true);
  });
}

test('clone Cancel removes a scheduled poll before it sends a request', async () => {
  const f = fixture(), modal = await startClone(f);
  assert.equal(f.timers.size, 1);
  await get(modal, '#clCancel').activate();
  assert.equal(f.timers.size, 0);
  assert.equal(f.requests.length, 1);
});

for (const outcome of ['success', 'error']) {
  test(`clone ignores late poll ${outcome} after dismissal`, async () => {
    const f = fixture(), modal = await startClone(f);
    const poll = f.tick(700), request = f.requests.at(-1);
    await get(modal, '#clCancel').activate();
    await f.navigate('#/settings');
    if (outcome === 'success') request.resolve(done('late')); else request.reject(new Error('late failure'));
    await poll;
    assert.equal(request.signal.aborted, true);
    assert.equal(f.context.location.hash, '#/settings');
    assert.equal(f.context.state.servers, f.initialServers);
    assert.equal(f.requests.length, 2, 'No refresh after a stale status response');
    assert.equal(f.timers.size, 0);
    assert.equal(f.toasts.length, 0);
  });
}

test('clone reopening is isolated from the dismissed dialog POST', async () => {
  const f = fixture(), oldModal = f.clone();
  const oldSubmit = get(oldModal, '#clGo').activate(), oldPost = f.requests[0];
  await get(oldModal, '#clCancel').activate();
  const newModal = await startClone(f, f.clone(), 'new-job');
  oldPost.resolve({ id: 'old-job' });
  await oldSubmit;
  assert.equal(f.timers.size, 1);
  assert.equal(newModal.removed, false);
  const poll = f.tick(700);
  assert.equal(f.requests.at(-1).url, '/api/import/new-job');
  await completePoll(f, poll, 'new-server');
  assert.equal(f.context.location.hash, '#/s/new-server/dashboard');
  assert.equal(newModal.removed, true);
  assert.equal(f.toasts.length, 1);
});

test('clone close during server refresh ignores its late state and navigation', async () => {
  const f = fixture(), modal = await startClone(f);
  const poll = f.tick(700);
  f.requests.at(-1).resolve(done('late'));
  await microtasks();
  const refresh = f.requests.at(-1);
  assert.equal(refresh.url, '/api/servers');
  await get(modal, '#cx').activate();
  await f.navigate('#/templates');
  refresh.resolve({ servers: [{ id: 'late' }], host: { late: true } });
  await poll;
  assert.equal(refresh.signal.aborted, true);
  assert.equal(f.context.state.servers, f.initialServers);
  assert.equal(f.context.location.hash, '#/templates');
  assert.equal(f.toasts.length, 0);
});

test('clone navigation away and back disposes ownership even when the hash matches again', async () => {
  const f = fixture(), modal = await startClone(f), hash = f.context.location.hash;
  const poll = f.tick(700), pending = f.requests.at(-1);
  await f.navigate('#/settings');
  await f.navigate(hash);
  pending.resolve(done('late'));
  await poll;
  assert.equal(modal.removed, true);
  assert.equal(f.hashListeners(), 0);
  assert.equal(f.context.location.hash, hash);
  assert.equal(f.toasts.length, 0);
});

test('clone guards a newer hash before its hashchange event is delivered', async () => {
  const f = fixture();
  await startClone(f);
  const poll = f.tick(700);
  f.context.location.hash = '#/settings';
  f.requests.at(-1).resolve(done('late'));
  await poll;
  assert.equal(f.context.location.hash, '#/settings');
  assert.equal(f.requests.length, 2);
  assert.equal(f.toasts.length, 0);
  await f.navigate('#/settings');
  assert.equal(f.hashListeners(), 0);
});

test('clone slow polls never overlap; running schedules exactly one next read', async () => {
  const f = fixture();
  await startClone(f, f.clone(), 'job /1');
  const poll = f.tick(700);
  assert.equal(f.requests.at(-1).url, '/api/import/job%20%2F1');
  assert.equal(f.timers.size, 0, 'No interval can start another read while one is pending');
  f.requests.at(-1).resolve({ state: 'running', percent: 42 });
  await poll;
  assert.equal(f.timers.size, 1);
  assert.equal(f.requests.length, 2);
  assert.equal(get(f.modals[0], '#clBar').style.width, '42%');
});

test('clone POST failure restores controls for a deliberate retry', async () => {
  const f = fixture(), modal = f.clone();
  const first = get(modal, '#clGo').activate();
  f.requests[0].reject(new Error('request rejected'));
  await first;
  assert.equal(get(modal, '#clGo').disabled, false);
  assert.match(get(modal, '#clGo').innerHTML, /Clone/);
  assert.deepEqual(f.toasts, [['request rejected', 'err']]);
  get(modal, '#clName').value = 'retry name';
  await startClone(f, modal, 'retry-job');
  assert.equal(JSON.parse(f.requests[1].body).name, 'retry name');
  assert.equal(get(modal, '#clGo').disabled, true);
  assert.equal(f.timers.size, 1);
});

test('clone status failure retries the same job without a second create', async () => {
  const f = fixture(), modal = await startClone(f);
  const poll = f.tick(700);
  f.requests.at(-1).reject(new Error('status unavailable'));
  await poll;
  assert.equal(get(modal, '#clGo').disabled, false);
  assert.equal(get(modal, '#clGo').innerHTML, 'Retry status');
  assert.equal(f.timers.size, 0);
  await get(modal, '#clGo').activate();
  await get(modal, '#clGo').activate();
  const retried = f.tick(700);
  assert.equal(f.requests.at(-1).url, '/api/import/job1');
  assert.equal(f.requests.filter(request => request.method === 'POST').length, 1);
  await completePoll(f, retried);
  assert.equal(f.context.location.hash, '#/s/s2/dashboard');
});

test('clone terminal failure permits a new clone rather than retrying a failed job', async () => {
  const f = fixture(), modal = await startClone(f);
  const poll = f.tick(700);
  f.requests.at(-1).resolve({ state: 'failed', error: 'copy failed' });
  await poll;
  assert.equal(get(modal, '#clGo').disabled, false);
  assert.match(get(modal, '#clGo').innerHTML, /Clone/);
  await startClone(f, modal, 'job2');
  assert.equal(f.requests.filter(request => request.method === 'POST').length, 2);
});

test('clone successful completion refreshes state, navigates once, and tears down', async () => {
  const f = fixture(), modal = await startClone(f);
  const poll = f.tick(700);
  await completePoll(f, poll);
  assert.deepEqual(f.context.state.servers, [{ id: 's2' }]);
  assert.deepEqual(f.context.state.host, { updated: true });
  assert.equal(f.context.location.hash, '#/s/s2/dashboard');
  assert.deepEqual(f.toasts, [['Cloned Copy s2']]);
  assert.equal(modal.removed, true);
  assert.equal(f.hashListeners(), 0);
  assert.equal(f.timers.size, 0);
});

test('clone completed job remains usable if the optional server refresh fails', async () => {
  const f = fixture();
  await startClone(f);
  const poll = f.tick(700);
  f.requests.at(-1).resolve(done('s2'));
  await microtasks();
  f.requests.at(-1).reject(new Error('refresh failed'));
  await poll;
  assert.equal(f.context.location.hash, '#/s/s2/dashboard');
  assert.equal(f.toasts.length, 1);
  assert.equal(f.timers.size, 0);
});

test('task create repeated activation sends one request and one success callback', async () => {
  const f = fixture();
  let saved = 0;
  const modal = f.task(null, () => saved++), save = get(modal, '#save');
  get(modal, '#tName').value = '  Nightly backup  ';
  get(modal, '#tCmd').value = ' !backup ';
  const first = save.activate();
  const repeated = save.activate();
  assert.equal(f.requests.length, 1);
  await repeated;
  assert.equal(save.disabled, true);
  assert.equal(save.textContent, 'Saving…');
  assert.equal(f.requests[0].method, 'POST');
  assert.equal(f.requests[0].url, '/api/servers/s1/tasks');
  assert.deepEqual(JSON.parse(f.requests[0].body), {
    name: 'Nightly backup', commands: '!backup', time: '04:00:00', repeat: true, enabled: true,
  });
  f.requests[0].resolve({});
  await first;
  await save.activate();
  assert.equal(f.requests.length, 1);
  assert.equal(saved, 1);
  assert.equal(f.toasts.length, 1);
  assert.equal(modal.removed, true);
  assert.equal(f.hashListeners(), 0);
  assert.equal(f.timers.size, 0);
});

test('task edit repeated activation preserves disabled state and sends one PATCH', async () => {
  const f = fixture();
  const modal = f.task({ id: 't1', name: 'old', commands: '!backup', time: '04:00:00', repeat: false, enabled: false });
  const first = get(modal, '#save').activate();
  const repeated = get(modal, '#save').activate();
  assert.equal(f.requests.length, 1);
  await repeated;
  assert.equal(f.requests[0].method, 'PATCH');
  assert.equal(f.requests[0].url, '/api/servers/s1/tasks/t1');
  const body = JSON.parse(f.requests[0].body);
  assert.equal('enabled' in body, false);
  assert.equal(body.repeat, false);
  f.requests[0].resolve({});
  await first;
});

test('task error restores Save and a retry reads current fields once', async () => {
  const f = fixture();
  let saved = 0;
  const modal = f.task(null, () => saved++), save = get(modal, '#save');
  const first = save.activate();
  f.requests[0].reject(new Error('invalid task'));
  await first;
  assert.equal(save.disabled, false);
  assert.equal(save.textContent, 'Save');
  assert.equal(modal.removed, false);
  assert.equal(saved, 0);
  get(modal, '#tName').value = 'Fixed name';
  const retry = save.activate();
  const repeated = save.activate();
  assert.equal(f.requests.length, 2);
  await repeated;
  assert.equal(JSON.parse(f.requests[1].body).name, 'Fixed name');
  f.requests[1].resolve({});
  await retry;
  assert.equal(saved, 1);
  assert.deepEqual(f.toasts, [['invalid task', 'err'], ['Task created']]);
});

for (const selector of ['#back', '#x']) {
  for (const outcome of ['success', 'error']) {
    test(`task ${selector} ignores late ${outcome} and does not steal focus`, async () => {
      const f = fixture();
      let saved = 0;
      const modal = f.task(null, () => saved++), pending = get(modal, '#save').activate();
      await get(modal, selector).activate();
      assert.equal(f.requests[0].signal.aborted, true);
      if (outcome === 'success') f.requests[0].resolve({}); else f.requests[0].reject(new Error('late error'));
      await pending;
      await get(modal, '#save').activate();
      assert.equal(f.requests.length, 1);
      assert.equal(saved, 0);
      assert.equal(f.toasts.length, 0);
      assert.equal(f.timers.size, 0);
      assert.equal(get(modal, '#tName').focused, false);
      assert.equal(f.hashListeners(), 0);
    });
  }
}

test('task reopening isolates old completion from the new modal and callback', async () => {
  const f = fixture();
  let oldSaved = 0, newSaved = 0;
  const oldModal = f.task(null, () => oldSaved++), oldSave = get(oldModal, '#save').activate();
  await get(oldModal, '#back').activate();
  const newModal = f.task(null, () => newSaved++), newSave = get(newModal, '#save').activate();
  f.requests[0].resolve({});
  await oldSave;
  assert.equal(newModal.removed, false);
  assert.equal(get(newModal, '#save').disabled, true);
  assert.equal(oldSaved, 0);
  assert.equal(newSaved, 0);
  f.requests[1].resolve({});
  await newSave;
  assert.equal(newSaved, 1);
  assert.equal(f.toasts.length, 1);
});

test('task navigation away and back ignores completion for the disposed view', async () => {
  const f = fixture();
  let saved = 0;
  const modal = f.task(null, () => saved++), hash = f.context.location.hash;
  const pending = get(modal, '#save').activate();
  await f.navigate('#/settings');
  await f.navigate(hash);
  f.requests[0].resolve({});
  await pending;
  assert.equal(saved, 0);
  assert.equal(modal.removed, true);
  assert.equal(f.toasts.length, 0);
  assert.equal(f.hashListeners(), 0);
});

test('task focus timer runs only while the dialog still owns its route', () => {
  const f = fixture(), modal = f.task();
  f.tick(30);
  assert.equal(get(modal, '#tName').focused, true);
  const second = f.task();
  f.context.location.hash = '#/settings';
  f.tick(30);
  assert.equal(get(second, '#tName').focused, false);
});
