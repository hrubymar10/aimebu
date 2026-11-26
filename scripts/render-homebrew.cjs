#!/usr/bin/env node
// Render the tap formula from locally checked release archives.
'use strict';

const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');

const [tag, output] = process.argv.slice(2);
if (!tag || !output || !/^v\d+\.\d+\.\d+(?:-(?:alpha|beta|rc)\.\d+)?$/.test(tag)) {
  console.error('usage: scripts/render-homebrew.cjs <tag> <output.rb>');
  process.exit(2);
}

const version = tag.slice(1);
const root = path.resolve(__dirname, '..');
const dist = path.join(root, 'dist');
const checksums = new Map();
for (const line of fs.readFileSync(path.join(dist, 'checksums.txt'), 'utf8').trim().split('\n')) {
  const match = /^([0-9a-f]{64})  \*?(\S+)$/.exec(line.trimEnd());
  if (!match || checksums.has(match[2])) throw new Error(`bad or duplicate checksum line: ${line}`);
  checksums.set(match[2], match[1]);
}

let formula = fs.readFileSync(path.join(root, 'packaging/homebrew/aimebu.rb.tmpl'), 'utf8')
  .replaceAll('@VERSION@', version);
for (const osName of ['darwin', 'linux']) {
  for (const arch of ['arm64', 'amd64']) {
    const name = `aimebu_${version}_${osName}_${arch}.tar.gz`;
    const digest = crypto.createHash('sha256').update(fs.readFileSync(path.join(dist, name))).digest('hex');
    if (checksums.get(name) !== digest) throw new Error(`missing or mismatched checksum for ${name}`);
    const key = `@${osName.toUpperCase()}_${arch.toUpperCase()}`;
    formula = formula.replaceAll(`${key}_URL@`,
      `https://github.com/hrubymar10/aimebu/releases/download/${tag}/${name}`);
    formula = formula.replaceAll(`${key}_SHA256@`, digest);
  }
}
if (/@[A-Z_0-9]+@/.test(formula)) throw new Error('unreplaced formula placeholder');
fs.mkdirSync(path.dirname(output), { recursive: true });
fs.writeFileSync(output, formula);
