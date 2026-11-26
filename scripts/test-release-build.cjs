#!/usr/bin/env node
// Build and check the four release archives, checksums, version, and formula.
'use strict';

const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const cp = require('node:child_process');

const [tag, flag] = process.argv.slice(2);
if (!tag || !/^v\d+\.\d+\.\d+(?:-(?:alpha|beta|rc)\.\d+)?$/.test(tag) ||
    (flag !== undefined && flag !== '--verify-only')) {
  console.error('usage: scripts/test-release-build.cjs <tag> [--verify-only]');
  process.exit(2);
}
const version = tag.slice(1);
const root = path.resolve(__dirname, '..');
const dist = path.join(root, 'dist');
function run(command, args, options = {}) {
  const result = cp.spawnSync(command, args, { cwd: root, encoding: 'utf8', ...options });
  if (result.error || result.status !== 0) {
    throw new Error(`${command} ${args.join(' ')} failed: ${result.error || result.stderr}`);
  }
  return result.stdout;
}
if (flag !== '--verify-only') run(path.join(root, 'scripts/release-build.sh'), [tag], { stdio: 'inherit' });

const expected = new Set();
for (const targetOS of ['darwin', 'linux']) {
  for (const arch of ['arm64', 'amd64']) expected.add(`aimebu_${version}_${targetOS}_${arch}.tar.gz`);
}
const actual = fs.readdirSync(dist).filter(name => name.startsWith(`aimebu_${version}_`) && name.endsWith('.tar.gz'));
if (actual.length !== 4 || actual.some(name => !expected.has(name))) throw new Error(`wrong archive set: ${actual}`);

const checksums = new Map();
for (const line of fs.readFileSync(path.join(dist, 'checksums.txt'), 'utf8').trim().split('\n')) {
  const match = /^([0-9a-f]{64})  (aimebu_\S+\.tar\.gz)$/.exec(line.trimEnd());
  if (!match || checksums.has(match[2])) throw new Error(`bad or duplicate checksum line: ${line}`);
  checksums.set(match[2], match[1]);
}
if (checksums.size !== 4 || [...expected].some(name => !checksums.has(name))) throw new Error('wrong checksum set');

const unpack = fs.mkdtempSync(path.join(os.tmpdir(), 'aimebu-archives-'));
try {
  for (const name of expected) {
    const archive = path.join(dist, name);
    const digest = crypto.createHash('sha256').update(fs.readFileSync(archive)).digest('hex');
    if (digest !== checksums.get(name)) throw new Error(`checksum mismatch: ${name}`);
    const names = run('tar', ['-tzf', archive]).trim().split('\n');
    if (names.length !== 1 || names[0] !== 'aimebu') throw new Error(`bad archive contents: ${name}`);
    const target = path.join(unpack, name);
    fs.mkdirSync(target);
    run('tar', ['-xzf', archive, '-C', target]);
    const binary = path.join(target, 'aimebu');
    if (!(fs.statSync(binary).mode & 0o111)) throw new Error(`binary is not executable: ${name}`);
    const magic = Buffer.alloc(4);
    const fd = fs.openSync(binary, 'r');
    try { fs.readSync(fd, magic, 0, 4, 0); } finally { fs.closeSync(fd); }
    if (name.includes('_linux_') && !magic.equals(Buffer.from([0x7f, 0x45, 0x4c, 0x46]))) {
      throw new Error(`not ELF: ${name}`);
    }
    if (name.includes('_darwin_') && !['cffaedfe', 'feedfacf'].includes(magic.toString('hex'))) {
      throw new Error(`not Mach-O: ${name}`);
    }
  }
  const arch = { arm64: 'arm64', x64: 'amd64' }[process.arch];
  if (['darwin', 'linux'].includes(process.platform) && arch) {
    const name = `aimebu_${version}_${process.platform}_${arch}.tar.gz`;
    const binary = path.join(dist, 'aimebu');
    fs.copyFileSync(path.join(unpack, name, 'aimebu'), binary);
    fs.chmodSync(binary, 0o755);
    const printed = run(binary, ['version']).trim();
    if (printed !== `aimebu ${tag}`) throw new Error(`wrong embedded version: ${printed}`);
  }
} finally {
  fs.rmSync(unpack, { recursive: true, force: true });
}

const formula = path.join(dist, 'aimebu.rb');
run(process.execPath, [path.join(root, 'scripts/render-homebrew.cjs'), tag, formula]);
const rendered = fs.readFileSync(formula, 'utf8');
if (!rendered.includes(`version "${version}"`) || rendered.includes(`version "${tag}"`)) {
  throw new Error(`formula version must omit the v prefix: ${version}`);
}
for (const name of expected) {
  if (!rendered.includes(name) || !rendered.includes(checksums.get(name))) {
    throw new Error(`formula missing URL or checksum: ${name}`);
  }
}
console.log('release archives, checksums, embedded version, and formula: passed');
