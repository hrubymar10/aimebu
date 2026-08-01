#!/usr/bin/env node
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'frontend', 'app.js'), 'utf8');

function extractFunction(name) {
  const start = source.indexOf(`function ${name}`);
  assert.notStrictEqual(start, -1, `${name} not found`);
  const bodyStart = source.indexOf('{', start);
  let depth = 0;
  for (let i = bodyStart; i < source.length; i++) {
    if (source[i] === '{') depth++;
    if (source[i] === '}') depth--;
    if (depth === 0) return source.slice(start, i + 1);
  }
  throw new Error(`${name} did not terminate`);
}

const context = {
  esc: value => String(value || '').replace(/&/g, '&amp;').replace(/"/g, '&quot;'),
};
vm.createContext(context);
vm.runInContext([
  extractFunction('agentSpeedMeta'),
  extractFunction('agentSpeedBadgeHTML'),
].join('\n'), context);

const underSampled = context.agentSpeedBadgeHTML({
  kind: 'ai', harness: 'codex', generation_samples_ms: [1000, 2000], generation_ms: 1500,
});
assert(!underSampled.includes('agent-speed-icon'), 'under three samples renders no icon');
assert(underSampled.includes('not enough samples yet'), 'under-sampled tooltip explains absence');

const unsupported = context.agentSpeedBadgeHTML({ kind: 'ai', harness: 'vibe' });
assert(!unsupported.includes('agent-speed-icon'), 'unsupported harness renders no neutral icon');
assert(unsupported.includes('not measured for vibe'), 'unsupported tooltip names the harness');

const missingMedian = context.agentSpeedBadgeHTML({
  kind: 'ai', harness: 'codex', generation_samples_ms: [1000, 2000, 3000], generation_ms: 0,
});
assert(!missingMedian.includes('agent-speed-icon'), 'non-positive missing median fails closed');
assert(missingMedian.includes('not enough samples yet'), 'missing median explains absent icon');

const fast = context.agentSpeedBadgeHTML({
  kind: 'ai', harness: 'pi', generation_samples_ms: [1000, 2000, 3000], generation_ms: 14999,
});
assert(fast.includes('agent-speed-fast'), 'under 15s uses lightning band');

const average = context.agentSpeedBadgeHTML({
  kind: 'ai', harness: 'claude-code', generation_samples_ms: [15000, 23000, 60000], generation_ms: 23000,
});
assert(average.includes('agent-speed-average'), '15-60s uses average band');
assert(average.includes('23s median over 3 turns'), 'measured tooltip reports median and sample count');

const slow = context.agentSpeedBadgeHTML({
  kind: 'ai', harness: 'codex', generation_samples_ms: [60001, 70000, 80000], generation_ms: 60001,
});
assert(slow.includes('agent-speed-slow'), 'over 60s uses snail band');
assert.strictEqual(context.agentSpeedBadgeHTML({ kind: 'human', harness: 'codex' }), '', 'humans never render a speed slot');

console.log('frontend agent speed tests passed');
