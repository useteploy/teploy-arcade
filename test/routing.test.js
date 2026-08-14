/* Frontend routing guard. Regression test for a real bug: `#/dashboard` and
 * `#/s/<id>/dashboard` share a route name, so the event feed re-rendered a
 * server's own dashboard as the global one every two seconds — the page
 * visibly navigated away from itself — and leaked that view's refresh timer
 * each time.
 *
 *   node test/routing.test.js
 */
const fs = require('fs');
const path = require('path');

const src = fs.readFileSync(
  path.join(__dirname, '..', 'cmd', 'teploy-arcade', 'frontend', 'app.js'), 'utf8');

const parseHash = eval('(' +
  src.match(/function parseHash\(\) \{[\s\S]*?\n\}/)[0].replace('function parseHash()', 'function ()') + ')');

// Must stay in step with the guard in connectEvents().
const isListView = (r) => !r.id && (r.name === 'servers' || r.name === 'dashboard');

// Every rail item must point at a route that actually exists, and light up on
// it. Regression test for a real bug: the Settings item carried
// data-nav="settings" but linked to #/host, so it never highlighted.
const html = fs.readFileSync(
  path.join(__dirname, '..', 'cmd', 'teploy-arcade', 'frontend', 'index.html'), 'utf8');

const railItems = [...html.matchAll(/data-nav="([^"]+)"\s+href="#\/([^"]*)"/g)]
  .map(([, nav, href]) => ({ nav, href }));

// Executes the shipped railActiveFor() rather than a copy of its rules. A
// hand-mirrored copy keeps passing against logic app.js no longer has, which
// is precisely the drift this file exists to catch.
const appSrc = fs.readFileSync(
  path.join(__dirname, '..', 'cmd', 'teploy-arcade', 'frontend', 'app.js'), 'utf8');
const railFn = appSrc.match(/^function railActiveFor\(nav, route\) \{[\s\S]*?^\}/m);
if (!railFn) {
  console.error('FAIL could not find railActiveFor() in app.js; the rail tests below are vacuous');
  process.exit(1);
}
const railActive = new Function(`${railFn[0]}; return railActiveFor;`)();

let navFailed = 0;
if (railItems.length < 4) {
  console.error(`FAIL only found ${railItems.length} rail items; expected at least 4`);
  navFailed++;
}
for (const { nav, href } of railItems) {
  global.location = { hash: '#/' + href };
  const route = parseHash();
  if (!railActive(nav, route)) {
    navFailed++;
    console.error(`FAIL rail item data-nav="${nav}" links to #/${href}` +
      ` which routes to "${route.name}" — it will never highlight`);
  }
}
if (!navFailed) console.log(`rail: ${railItems.length} items highlight on their own route`);

// `#/import` is panel-wide but is entered from the Servers page and exits onto a
// server, so renderRail deliberately keeps Servers lit for it. Asserted here
// because the rail loop above only walks routes a rail item links to, and
// nothing links to #/import.
{
  global.location = { hash: '#/import' };
  const r = parseHash();
  if (!railActive('servers', r)) {
    navFailed++;
    console.error('FAIL #/import lights no rail item — the rail blanks mid-import');
  }
  if (railActive('dashboard', r) || railActive('templates', r) || railActive('settings', r)) {
    navFailed++;
    console.error('FAIL #/import lights a rail item other than Servers');
  }
}

const cases = [
  ['#/',                   'servers',   null,     true ],
  ['#/dashboard',          'dashboard', null,     true ],
  ['#/servers',            'servers',   null,     true ],
  ['#/templates',          'templates', null,     false],
  ['#/import',             'import',    null,     false],
  ['#/s/abc123',           'console',   'abc123', false],
  ['#/s/abc123/dashboard', 'dashboard', 'abc123', false],
  ['#/s/abc123/console',   'console',   'abc123', false],
  ['#/s/abc123/players',   'players',   'abc123', false],
  ['#/s/abc123/scheduler', 'scheduler', 'abc123', false],
  ['#/s/abc123/plugins',   'plugins',   'abc123', false],
  ['#/s/abc123/settings',  'settings',  'abc123', false],
  ['#/settings',           'settings',  null,     false],
  ['#/host',               'host',      null,     false],
];

let failed = 0;
for (const [hash, name, id, list] of cases) {
  global.location = { hash };
  const got = parseHash();
  const gotList = isListView(got);
  if (got.name !== name || got.id !== id || gotList !== list) {
    failed++;
    console.error(`FAIL ${hash} -> ${got.name}/${got.id} listView=${gotList}` +
      ` (want ${name}/${id} listView=${list})`);
  }
}

// Every view the router reaches for must be registered by a script index.html
// loads *before* app.js. A view file that exists but was never script-tagged
// fails only when someone clicks the tab, as `viewX is not a function` in the
// console and a blank pane — no build, test or lint catches it. views.js has to
// come first because it is the file that creates window.extraViews; the rest
// Object.assign onto it.
const scripts = [...html.matchAll(/<script src="([^"]+\.js)"><\/script>/g)].map(([, s]) => s);
const beforeApp = scripts.slice(0, scripts.indexOf('app.js'));
let wireFailed = 0;
if (scripts.indexOf('app.js') < 0) {
  console.error('FAIL index.html does not load app.js');
  wireFailed++;
} else if (beforeApp[0] !== 'views.js') {
  console.error(`FAIL views.js must load first (creates window.extraViews); got ${beforeApp[0]}`);
  wireFailed++;
}
const registered = new Set();
for (const f of beforeApp) {
  const s = fs.readFileSync(path.join(__dirname, '..', 'cmd', 'teploy-arcade', 'frontend', f), 'utf8');
  for (const [, names] of s.matchAll(/window\.extraViews\s*=\s*\{([^}]*)\}/g))
    names.split(',').forEach((n) => registered.add(n.trim()));
  for (const [, names] of s.matchAll(/Object\.assign\(window\.extraViews,\s*\{([^}]*)\}/g))
    names.split(',').forEach((n) => registered.add(n.trim()));
}
const used = new Set([...src.matchAll(/window\.extraViews\.(\w+)/g)].map(([, n]) => n));
for (const name of used) {
  if (!registered.has(name)) {
    wireFailed++;
    console.error(`FAIL app.js calls window.extraViews.${name}, which no script before app.js registers`);
  }
}
if (!wireFailed) console.log(`views: ${used.size} extraViews entry points are loaded before app.js`);

// responsive.css must be the LAST stylesheet. A media query carries no extra
// specificity, so each rule in it ties with the rule it overrides and the later
// sheet wins on order alone. Written into styles.css first, every override of
// an app.css rule (.tiles, .panelbox .k, .statrow) lost silently — no error,
// no failing test, the layout below 900px simply did not move.
const sheets = [...html.matchAll(/<link rel="stylesheet" href="([^"]+\.css)">/g)].map(([, s]) => s);
let cssFailed = 0;
if (!sheets.includes('responsive.css')) {
  console.error('FAIL index.html does not load responsive.css');
  cssFailed++;
} else if (sheets[sheets.length - 1] !== 'responsive.css') {
  console.error(`FAIL responsive.css must be the last stylesheet; got ${sheets[sheets.length - 1]}` +
    ' — its media queries will lose to the sheets after it');
  cssFailed++;
} else {
  console.log(`stylesheets: ${sheets.length} loaded, responsive.css last`);
}

if (failed || navFailed || wireFailed || cssFailed) {
  console.error(`\n${failed}/${cases.length} routing cases failed, ${navFailed} rail items misconfigured,` +
    ` ${wireFailed} view scripts unwired, ${cssFailed} stylesheet-order problems`);
  process.exit(1);
}
console.log(`routing: ${cases.length} cases pass`);
