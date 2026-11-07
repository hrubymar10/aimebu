// The accessibility and phone-layout audits and the Settings -> Harnesses smoke, which scripts/test-ui.cjs used to run before its own steps. They need a built binary and Chromium but not the smoke's server, so they run beside it.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const net = require('node:net');
const { spawn, spawnSync } = require('node:child_process');
const { start, isolatedEnv, binary } = require('./ui-smoke-lib.cjs');

async function main() {
    // Names, focus rings and button classes are hard failures. Contrast is a report: v2 ships v1's palette (the owner's decision), whose muted text
    // and light themes are under WCAG AA in places; a run prints how many, and `node scripts/a11y-audit.cjs --strict` still enforces it.
    const audit = spawnSync(process.execPath, [path.join(__dirname, 'a11y-audit.cjs')], { env: isolatedEnv, encoding: 'utf8' });
    assert.equal(audit.status, 0, `accessibility audit failed:\n${audit.stdout}${audit.stderr}`);
    console.log(`a11y contrast (report only, v1 palette): ${audit.stdout.split('\n').filter(line => line.startsWith('== Contrast failures')).map(line => line.replace('== Contrast failures ', '')).join(' · ') || 'none'}`);
    const phone = spawnSync(process.execPath, [path.join(__dirname, 'phone-audit.cjs')], { env: isolatedEnv, encoding: 'utf8' });
    assert.equal(phone.status, 0, `phone layout audit failed:\n${phone.stdout}${phone.stderr}`);
    // Settings → Harnesses: model editing, role defaults, the sync with Settings → Roles, the stale-save refusal and the phone width, on a scratch server of its own.
    const harnesses = spawnSync(process.execPath, [path.join(__dirname, 'harness-settings.cjs')], { env: { ...isolatedEnv, HARNESS_BINARY: binary }, encoding: 'utf8' });
    assert.equal(harnesses.status, 0, `harness settings smoke failed:\n${harnesses.stdout}${harnesses.stderr}`);
  console.log('UI smoke part audits passed');
}
main().catch(error => { console.error(error); process.exitCode = 1; });
