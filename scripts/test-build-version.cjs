// What the header's version says, and that an unreadable answer leaves it alone. Runs the module source in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'build-version.js'), 'utf8');
const context = {};
vm.createContext(context);
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { versionLabel, showBuildVersion };', context);
const { versionLabel, showBuildVersion } = context.api;

assert.deepEqual({ ...versionLabel({ version: 'aimebu v2.0.0-3-gabc123-dirty', go_version: 'go1.26.1' }) }, { text: 'v2.0.0-3-gabc123-dirty', title: 'v2.0.0-3-gabc123-dirty · go1.26.1' }, 'the program name is dropped, the toolchain is the tooltip');
assert.deepEqual({ ...versionLabel({ version: 'AIMEBU  v1' }) }, { text: 'v1', title: 'v1' }, 'the prefix goes whatever its case or spacing; no toolchain, no extra tooltip text');
assert.deepEqual({ ...versionLabel({ version: 'aimebu v2 0123456789ab 2026-10-27T00:00:00Z dirty' }) }, { text: 'v2 0123456789ab 2026-10-27T00:00:00Z dirty', title: 'v2 0123456789ab 2026-10-27T00:00:00Z dirty' });
for (const empty of [null, undefined, {}, { version: '' }, { version: '   ' }, { version: 3 }, { version: 'aimebu ' }, 'v1']) assert.equal(versionLabel(empty), null, `nothing to show for ${JSON.stringify(empty)}`);

(async () => {
  const header = () => ({ textContent: 'v2', title: 'Version' });
  let element = header();
  await showBuildVersion(async path => { assert.equal(path, '/api/buildinfo'); return { version: 'aimebu v2.1.0', go_version: 'go1.26.1' }; }, element);
  assert.deepEqual(element, { textContent: 'v2.1.0', title: 'v2.1.0 · go1.26.1' }, 'the header shows what the server answered');
  for (const answer of [async () => { throw new Error('offline'); }, async () => ({}), async () => null]) {
    element = header();
    await showBuildVersion(answer, element);
    assert.deepEqual(element, header(), 'an unreadable answer leaves the served text');
  }
  await showBuildVersion(async () => ({ version: 'x' }), null); // no header in the page: nothing to do, nothing thrown
  assert.equal(await showBuildVersion(async () => ({ version: 'aimebu v2.1.0' }), header()), 'aimebu v2.1.0', 'the version string is handed back as the server states it');
  assert.equal(await showBuildVersion(async () => { throw new Error('offline'); }, header()), '', 'an unreadable answer hands back nothing');
  console.log('build version test passed');
})().catch(error => { console.error(error); process.exit(1); });
