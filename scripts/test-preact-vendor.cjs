// The vendored Preact runtime: checksums, the import map, and web/js/ui.js as the only module that imports it.
'use strict';
const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');

const web = path.join(__dirname, '..', 'web');
const dir = path.join(web, 'vendor', 'preact');
const sums = fs.readFileSync(path.join(dir, 'SHA256SUMS'), 'utf8').trim().split('\n').map(line => line.split(/\s+/));
for (const [hash, file] of sums) assert.equal(crypto.createHash('sha256').update(fs.readFileSync(path.join(dir, file))).digest('hex'), hash, `${file} matches SHA256SUMS`);
assert.deepEqual(sums.map(([, file]) => file).sort(), fs.readdirSync(dir).filter(file => file !== 'SHA256SUMS' && file !== 'README.md').sort(), 'SHA256SUMS lists exactly the vendored files');

const page = fs.readFileSync(path.join(web, 'index.html'), 'utf8');
const maps = [...page.matchAll(/<script type="importmap">(.*?)<\/script>/gs)];
assert.equal(maps.length, 1, 'index.html has one import map');
assert.ok(page.indexOf('<script type="importmap">') < page.indexOf('<script type="module"'), 'the import map comes before the first module script');
const { imports } = JSON.parse(maps[0][1]);
assert.deepEqual(Object.keys(imports).sort(), ['htm', 'preact', 'preact/hooks']);
for (const [name, url] of Object.entries(imports)) {
  assert.match(url, /^\/vendor\/preact\/[\w.-]+\.mjs$/, `${name} maps to a vendored module`);
  assert.ok(fs.existsSync(path.join(web, url)), `${url} exists`);
}
// The hooks resolve Preact through the same map entry as ui.js, so both get the same copy.
assert.deepEqual([...fs.readFileSync(path.join(dir, 'hooks.mjs'), 'utf8').matchAll(/from\s*"([^"]+)"/g)].map(match => match[1]), ['preact']);

const modules = [];
const walk = folder => { for (const entry of fs.readdirSync(folder, { withFileTypes: true })) { const file = path.join(folder, entry.name); if (entry.isDirectory()) walk(file); else if (entry.name.endsWith('.js')) modules.push(file); } };
walk(path.join(web, 'js'));
const runtime = /\bfrom\s*['"](preact|preact\/hooks|htm)['"]|import\(\s*['"](preact|htm)/;
assert.deepEqual(modules.filter(file => runtime.test(fs.readFileSync(file, 'utf8'))).map(file => path.relative(web, file)), ['js/ui.js'], 'only web/js/ui.js imports Preact or htm');
console.log('preact vendor tests passed');
