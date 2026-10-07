/* Regression guard for the local test target. A failing frontend script must
 * fail make test, rather than masquerading as a missing Node installation.
 *
 * Each make invocation runs in an isolated temporary fixture with a fake Go
 * executable, tiny frontend scripts, and an allowlisted PATH. No real Go,
 * Docker, network calls, project tests, or dependency installs are involved.
 *
 *   node test/make-test.test.js
 */
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

const makefile = fs.readFileSync(path.join(__dirname, '..', 'Makefile'), 'utf8');
const make = (process.env.PATH || '').split(path.delimiter).map(directory =>
  path.resolve(directory, 'make')).find(candidate => {
  try {
    fs.accessSync(candidate, fs.constants.X_OK);
    return fs.statSync(candidate).isFile();
  } catch (_) {
    return false;
  }
});
assert.ok(make, 'make is required to verify the Makefile test targets');

const root = fs.mkdtempSync(path.join(os.tmpdir(), 'arcade-make-test-'));
const skipMessage = 'node not found, skipping frontend tests';
let checked = 0;

function run(name, { target = 'test', node = true, goStatus = 0,
  routingStatus = 0, uiStatus = 0 } = {}) {
  const directory = path.join(root, name);
  const bin = path.join(directory, 'bin');
  fs.mkdirSync(bin, { recursive: true });
  fs.mkdirSync(path.join(directory, 'test'));
  fs.writeFileSync(path.join(directory, 'Makefile'), makefile);
  // A same-named file must not prevent the phony frontend target running.
  fs.writeFileSync(path.join(directory, 'test-frontend'), '');
  fs.writeFileSync(path.join(bin, 'go'), '#!/bin/sh\n' +
    '[ "$*" = "test ./... -count=1" ] || exit 99\n' +
    'printf "fixture Go invoked\\n"\n' +
    `exit ${goStatus}\n`, { mode: 0o755 });
  if (node) fs.symlinkSync(process.execPath, path.join(bin, 'node'));
  for (const [filename, status] of [
    ['routing.test.js', routingStatus], ['ui-lifecycle.test.js', uiStatus],
  ]) {
    fs.writeFileSync(path.join(directory, 'test', filename),
      `console.log(${JSON.stringify('fixture ' + filename + ' invoked')});\n` +
      `process.exit(${status});\n`);
  }
  const result = spawnSync(make, [
    '--no-print-directory', 'SHELL=/bin/sh', 'COMMIT=fixture', 'DATE=fixture', target,
  ], {
    cwd: directory,
    // Deliberately omit inherited MAKEFLAGS, MAKEFILES and NODE_OPTIONS too.
    env: { PATH: bin, HOME: directory, LC_ALL: 'C' },
    encoding: 'utf8',
    timeout: 10000,
  });
  assert.ifError(result.error);
  assert.equal(result.signal, null, `${name}: make was terminated`);
  result.output = result.stdout + result.stderr;
  return result;
}

function check(name, options, verify) {
  const result = run(name, options);
  verify(result);
  checked++;
  console.log(`make: ${name} passed`);
}

try {
  check('routing-failure-propagates', { routingStatus: 23 }, result => {
    assert.notEqual(result.status, 0, result.output);
    assert.match(result.stdout, /fixture routing\.test\.js invoked/);
    assert.doesNotMatch(result.output, /fixture ui-lifecycle\.test\.js invoked/);
    assert.ok(!result.output.includes(skipMessage), result.output);
  });

  check('all-tests-pass', {}, result => {
    assert.equal(result.status, 0, result.output);
    assert.match(result.stdout, /fixture Go invoked[\s\S]*fixture routing\.test\.js invoked[\s\S]*fixture ui-lifecycle\.test\.js invoked/);
    assert.ok(!result.output.includes(skipMessage), result.output);
  });

  check('ui-failure-propagates', { uiStatus: 29 }, result => {
    assert.notEqual(result.status, 0, result.output);
    assert.match(result.stdout, /fixture routing\.test\.js invoked[\s\S]*fixture ui-lifecycle\.test\.js invoked/);
    assert.ok(!result.output.includes(skipMessage), result.output);
  });

  check('missing-node-skips-clearly', { node: false }, result => {
    assert.equal(result.status, 0, result.output);
    assert.match(result.stdout, /fixture Go invoked/);
    assert.ok(result.stdout.includes(skipMessage), result.output);
    assert.doesNotMatch(result.output, /fixture (?:routing|ui-lifecycle)\.test\.js invoked/);
  });

  check('go-failure-stops-frontend', { goStatus: 37 }, result => {
    assert.notEqual(result.status, 0, result.output);
    assert.match(result.stdout, /fixture Go invoked/);
    assert.doesNotMatch(result.output, /fixture (?:routing|ui-lifecycle)\.test\.js invoked/);
    assert.ok(!result.output.includes(skipMessage), result.output);
  });

  check('frontend-only-never-runs-go', { target: 'test-frontend' }, result => {
    assert.equal(result.status, 0, result.output);
    assert.doesNotMatch(result.output, /fixture Go invoked/);
    assert.match(result.stdout, /fixture routing\.test\.js invoked[\s\S]*fixture ui-lifecycle\.test\.js invoked/);
  });
} finally {
  fs.rmSync(root, { recursive: true, force: true });
}

console.log(`make: ${checked}/${checked} regression cases passed`);
