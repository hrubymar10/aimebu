// The limit guard's wording and validation are pure browser-module code (no DOM), so this runs them in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'limit-guard-format.js'), 'utf8');
const context = {};
vm.createContext(context);
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { GUARD_TOOLS, guardSentence, guardHealthRow, heldTools, guardProblem };', context);
const { guardSentence, guardHealthRow, heldTools, guardProblem } = context.api;

const view = (state, percent, extra = {}) => ({ held: state === 'soft' || state === 'hard', result: { state, profile: 'main', percent, window: 'weekly', soft: 90, hard: 95 }, ...extra });

assert.equal(guardSentence('Claude', view('soft', 91)), 'Claude (main) is at 91% of its weekly window: new turns are held, running ones finish.');
assert.match(guardSentence('Codex', view('hard', 96.4)), /^Codex \(main\) is at 96% of its weekly window: new turns are held until it resets or the account is switched\.$/);
assert.equal(guardSentence('Claude', view('ok', 40)), 'Claude (main) is at 40% of its weekly window.');
assert.equal(guardSentence('Claude', view('off', null)), 'The limit guard is off for Claude.');
assert.equal(guardSentence('Claude', view('unknown', null)), 'Claude: no clear usage reading yet, nothing is held.');
assert.equal(guardSentence('Claude', undefined), 'Claude: no clear usage reading yet, nothing is held.');

// The Health list only shows a tool with a clear reading.
assert.equal(guardHealthRow('claude', 'Claude', view('off', null)), null);
assert.equal(guardHealthRow('claude', 'Claude', view('unknown', null)), null);
const row = guardHealthRow('claude', 'Claude', view('soft', 92));
assert.deepEqual({ tool: row.tool, state: row.state, held: row.held, percent: row.percent, soft: row.soft, hard: row.hard }, { tool: 'claude', state: 'soft', held: true, percent: 92, soft: 90, hard: 95 });
assert.equal(guardHealthRow('claude', 'Claude', view('ok', 10)).held, false);

// Only held tools count toward the Health badge.
assert.equal(heldTools({ tools: { claude: view('soft', 91), codex: view('ok', 5) } }), 1);
assert.equal(heldTools({ tools: { claude: view('hard', 99), codex: view('soft', 91) } }), 2);
assert.equal(heldTools(null), 0);

// A draft is whole numbers 1..100 with soft below hard, for the tool and for each override.
assert.equal(guardProblem({ soft: 90, hard: 95, profiles: {} }), '');
assert.match(guardProblem({ soft: 95, hard: 95, profiles: {} }), /soft threshold must be below/);
assert.match(guardProblem({ soft: 0, hard: 95, profiles: {} }), /whole numbers from 1 to 100/);
assert.match(guardProblem({ soft: 90.5, hard: 95, profiles: {} }), /whole numbers/);
assert.match(guardProblem({ soft: NaN, hard: 95, profiles: {} }), /whole numbers/);
assert.match(guardProblem({ soft: 90, hard: 101, profiles: {} }), /whole numbers/);
assert.match(guardProblem({ soft: 90, hard: 95, profiles: { small: { soft: 80, hard: 70 } } }), /^small: the soft threshold/);
assert.equal(guardProblem({ soft: 90, hard: 95, profiles: { small: { soft: 60, hard: 70 } } }), '');
console.log('limit guard format test passed');
