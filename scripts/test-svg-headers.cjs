// No SVG in the repository carries an icon catalogue's download header ("Uploaded to: SVG Repo ...", "Generator: SVG Repo Mixer Tools"):
// the icons are used under their licences, which web/vendor/README.md records, and the header is only noise in the files.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const root = path.join(__dirname, '..');
const skip = new Set(['.git', 'node_modules', 'tmp']);
const svgs = [];
(function walk(dir) {
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    if (skip.has(entry.name)) continue;
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) walk(full); else if (entry.isFile() && /\.svg$/i.test(entry.name)) svgs.push(full);
  }
})(root);
assert.ok(svgs.length > 20, `found ${svgs.length} SVG files`);
const offenders = svgs.filter(file => /svg\s*repo|svgrepo|Mixer Tools|Uploaded to:/i.test(fs.readFileSync(file, 'utf8'))).map(file => path.relative(root, file));
assert.deepEqual(offenders, [], `SVG files that carry a catalogue header: ${offenders.join(', ')}`);
// The check bites: a file with the header would be caught.
assert.ok(/svg\s*repo|svgrepo|Mixer Tools|Uploaded to:/i.test('<?xml version="1.0"?><!-- Uploaded to: SVG Repo, www.svgrepo.com, Generator: SVG Repo Mixer Tools -->'));
console.log(`svg header test passed (${svgs.length} files)`);
