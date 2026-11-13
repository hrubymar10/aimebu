// The agent cards' pure rules (web/js/pages/agents/model.js), run in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'agents', 'model.js'), 'utf8');
const context = vm.createContext({ Date, Number, Math, JSON, Map, Intl, Object, Array, String });
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { reconcile, stateBadge, stateDot, stateOf, activityOf, warmOf, dotOf, keepAliveOf, switchStatusOf, grantsOf, contextOf, liveOf, actionsOf, quickActionsOf, profileRows, outputStatus, outputFailure };', context);
const m = context.api;
const plain = value => JSON.parse(JSON.stringify(value));

// The class of the state badge and of the dot before the name: v1's where v1 has the state, v2's own for the rest (amber when paused or waiting for a
// decision, red when dead).
assert.deepEqual(['idle', 'working', 'paused', 'attention', 'error', 'stopped'].map(m.stateBadge), ['idle', 'thinking', 'paused', 'attention', 'error', 'stopped']);
assert.deepEqual(['idle', 'working', 'paused', 'attention', 'error', 'stopped', 'offline', 'unknown'].map(m.stateDot), ['active', 'active', 'stale', 'stale', 'dead', 'dead', 'dead', 'offline']);

// A refresh keeps the record it holds for an unchanged agent, and the whole map when nothing changed or moved.
const a = { id: 'a', name: 'ada', health_flags: [] }, b = { id: 'b', name: 'bert' };
const held = new Map([['a', a], ['b', b]]);
assert.equal(m.reconcile(held, [{ id: 'a', name: 'ada', health_flags: [] }, { id: 'b', name: 'bert' }]), held);
const changed = m.reconcile(held, [{ id: 'a', name: 'ada', health_flags: [] }, { id: 'b', name: 'bea' }]);
assert.notEqual(changed, held); assert.equal(changed.get('a'), a); assert.notEqual(changed.get('b'), b); assert.equal(changed.get('b').name, 'bea');
const moved = m.reconcile(held, [{ id: 'b', name: 'bert' }, { id: 'a', name: 'ada', health_flags: [] }]);
assert.deepEqual([...moved.keys()], ['b', 'a']); assert.equal(moved.get('a'), a); assert.equal(moved.get('b'), b);
assert.deepEqual([...m.reconcile(held, [{ id: 'a', name: 'ada', health_flags: [] }]).keys()], ['a']);
assert.deepEqual([...m.reconcile(held, [...held.values(), { id: 'c' }]).keys()], ['a', 'b', 'c']);
assert.equal(m.reconcile(new Map(), []).size, 0);

// The state words, the dot and the age's starting point.
const at = '2026-01-01T10:00:00Z';
const state = patch => plain(m.stateOf({ desired: 'active', observed: 'idle', last_activity_at: at, ...patch }));
assert.deepEqual(state({}), { name: 'idle', kind: 'idle', title: 'Idle; waiting for new work.', since: at, observed: 'Idle' });
assert.deepEqual(state({ observed: 'in_turn', activity_at: '2026-01-01T10:05:00Z' }), { name: 'working', kind: 'working', title: 'Working on a turn.', since: '2026-01-01T10:05:00Z', observed: 'Working' });
assert.equal(state({ observed: 'in_turn' }).since, at, 'a working agent without a turn start counts from its last activity');
assert.equal(state({ desired: 'stopped', observed: 'stopped' }).since, '', 'a stopped agent has no state age');
assert.deepEqual([state({ desired: 'stopped', observed: 'stopped' }).name, state({ desired: 'stopped', observed: 'in_turn' }).name, state({ observed: 'interrupting' }).name, state({ desired: 'not_accepting' }).name, state({ observed: 'lost' }).name, state({ boundary_pending: true }).name],
  ['stopped', 'stopping', 'stopping', 'paused', 'lost', 'awaiting approval']);
assert.deepEqual([state({ observed: 'lost' }).kind, state({ boundary_pending: true }).kind, state({ desired: 'stopped', observed: 'stopped' }).kind], ['error', 'paused', 'stopped']);
assert.equal(state({ recovery_block: true, last_exit_evidence: 'The process vanished', observed: 'lost' }).title, 'The previous process is unconfirmed; recovery is needed.\nThe process vanished');
assert.equal(state({ last_activity_at: '' }).since, '');
// An idle agent that will not start is not "idle": it needs a decision (orange), or the limit guard holds its tool's new turns (orange, with why).
const needs = state({ recovery_block: true, last_exit_evidence: 'OAuth session expired' });
assert.deepEqual([needs.name, needs.kind], ['needs decision', 'attention']);
assert.match(needs.title, /^Needs your decision: .*decide what to do in Inbox\.\nOAuth session expired$/);
assert.equal(needs.why.includes('OAuth'), false, 'the sentence beside the state is the decision, the evidence stays in the title');
const guardHold = { code: 'limit_guard', text: 'claude session 95%', detail: 'The limit guard is holding new claude turns.' };
const guardHeld = state({ hold: guardHold });
assert.deepEqual([guardHeld.name, guardHeld.kind, guardHeld.title, guardHeld.why], ['held: claude session 95%', 'attention', guardHold.detail, guardHold.detail]);
assert.equal(m.activityOf({ hold: guardHold }, guardHeld).text, guardHold.detail, 'the card says why nothing happens');
assert.equal(m.activityOf({ recovery_block: true }, needs).text, needs.why);
assert.equal(state({ recovery_block: true, hold: guardHold }).name, 'needs decision', 'a decision outranks the hold');
// Only a free idle agent shows either: a working one is working, a stopped or paused one is that, a lost one is lost (red), and one waiting for approval waits for it.
for (const patch of [{ observed: 'in_turn' }, { desired: 'stopped', observed: 'stopped' }, { desired: 'not_accepting' }, { observed: 'lost' }, { boundary_pending: true }]) {
  const other = state({ ...patch, recovery_block: true, hold: guardHold });
  assert.notEqual(other.kind, 'attention', JSON.stringify(patch));
  assert.equal(other.why, undefined, JSON.stringify(patch));
}
assert.equal(state({ observed: 'lost' }).kind, 'error', 'a lost process is still red');
assert.equal(m.activityOf({}, state({})).text, '', 'an idle agent that has done nothing has no line (the pill says idle, and for how long)');
// Warm mode: an idle agent says it is warm (and for how many more minutes) or sleeping; it says nothing when warm mode does not apply, or the agent is working.
const nowMs = Date.parse('2026-11-08T16:00:00Z');
assert.equal(m.warmOf({ observed: 'idle', warm: { state: 'warm', sleeps_at: '2026-11-08T16:04:10Z' } }, nowMs).text, 'Warm · sleeps in 5 min');
assert.equal(m.warmOf({ observed: 'idle', warm: { state: 'warm', sleeps_at: '2026-11-08T16:00:20Z' } }, nowMs).text, 'Warm · sleeps in 1 min', 'a window almost over still says one minute, never zero');
assert.equal(m.warmOf({ observed: 'idle', warm: { state: 'warm', sleeps_at: 'garbage' } }, nowMs).text, 'Warm');
assert.equal(m.warmOf({ observed: 'idle', warm: { state: 'sleeping' } }, nowMs).text, 'Sleeping');
assert.equal(m.warmOf({ observed: 'in_turn', warm: { state: 'warm', sleeps_at: '2026-11-08T16:04:10Z' } }, nowMs), null);
assert.equal(m.warmOf({ observed: 'idle' }, nowMs), null);
assert.equal(m.activityOf({ observed: 'idle', warm: { state: 'sleeping' } }, state({ observed: 'idle' })).text, 'Sleeping', 'the card shows it where an idle agent has nothing else to say');

assert.equal(m.activityOf({ woken_by: 'reply', activity: 'tool inspect' }, state({})).text, 'Woken by reply · tool inspect');
assert.equal(m.activityOf({}, state({ observed: 'in_turn' })).text, 'Working…');
assert.equal(m.activityOf({}, state({})).text, '');
assert.equal(m.activityOf({ woken_by: 'reply' }, state({})).text, 'Woken by reply', 'no dangling separator when it only says it was woken');
assert.deepEqual(plain(m.switchStatusOf({ boundary_pending: true }, String)), { hidden: false, text: 'Folder or machine approval needed · Open Inbox', inbox: true });
assert.deepEqual(plain(m.switchStatusOf({ switch: { state: 'failed', tool: 'claude', model: 'opus', error: 'no room' } }, String)), { hidden: false, text: 'Switch failed: claude · opus — no room', inbox: false });
assert.deepEqual(plain(m.switchStatusOf({ switch: { state: 'draining', tool: 'claude', model: 'opus' } }, tool => tool.toUpperCase())), { hidden: false, text: 'Finishing current turn: CLAUDE · opus', inbox: false });
assert.deepEqual(plain(m.switchStatusOf({ switch: { state: 'draining', handoff_status: 'pending', tool: 'claude', model: 'opus' } }, String)), { hidden: false, text: 'Handing off: claude · opus', inbox: false });
assert.deepEqual(plain(m.switchStatusOf({ switch: { state: 'starting', handoff_status: 'automatic', tool: 'claude', model: 'opus' } }, String)), { hidden: false, text: 'Starting new session: claude · opus', inbox: false });
assert.equal(m.switchStatusOf({}, String).hidden, true);
assert.deepEqual(plain(m.grantsOf({ boundary_grants: [{ machine_id: 'm1', path: '/a' }, { machine_id: 'm2', path: '/b' }] })), { hidden: false, text: 'Approved: m1 · /a · Approved: m2 · /b' });
assert.equal(m.grantsOf({}).hidden, true);

// The Memory bar and its title.
assert.deepEqual(plain(m.contextOf(undefined)), { known: false, width: '0%', value: '—', title: 'Context — / — tokens · Input — · Output — · Cost so far —' });
const usage = plain(m.contextOf({ tokens_in: 5000, tokens_out: 1200, context_used: 150000, context_window: 200000, cost_so_far: 0.35, cost_estimated: true }));
assert.deepEqual([usage.known, usage.width, usage.value], [true, '75%', '75%']);
assert.equal(usage.title, 'Context 150,000 / 200,000 tokens · Input 5,000 · Output 1,200 · Cost so far $0.35 (includes estimates)');
assert.equal(m.contextOf({ context_used: 300, context_window: 200 }).width, '100%', 'the bar never passes its end');
assert.match(m.contextOf({ cost_so_far: 0.004 }).title, /\$0\.0040/);
assert.deepEqual(plain(m.liveOf({ observed: 'in_turn', activity: 'tool inspect', activity_at: at })), { at, label: 'tool' });
assert.deepEqual(plain(m.liveOf({ observed: 'idle', activity_at: at })), { at: '', label: 'working' });

// Every action says what it does, or why it cannot.
const reasons = (agent, options) => Object.fromEntries(plain(m.actionsOf(agent, options)).map(action => [action.name, action.disabled ? action.hint : '']));
assert.deepEqual(reasons({ desired: 'active' }), { message: '', 'act-pause': '', 'act-stop': '', reactivate: 'Start is unavailable: it is already active.', 'start-fresh': 'Start fresh is unavailable: stop this agent first.', remove: 'Remove is unavailable: stop it first.', switch: '' });
assert.equal(reasons({ desired: 'stopped', observed: 'stopped' })['start-fresh'], '');
assert.equal(reasons({ desired: 'stopped', observed: 'stopped', role_deleted: true })['start-fresh'], 'Start fresh is unavailable: its role was deleted.');
// Remove is for a stopped agent only (nothing may still run for it), and says so otherwise.
assert.equal(reasons({ desired: 'stopped', observed: 'stopped' }).remove, '');
assert.equal(reasons({ desired: 'stopped', observed: 'interrupting' }).remove, 'Remove is unavailable: stop it first.');
assert.equal(reasons({ desired: 'not_accepting', observed: 'idle' }).remove, 'Remove is unavailable: stop it first.');
assert.equal(reasons({ desired: 'stopped' })['act-pause'], 'Pause is unavailable: it is stopped; start it first.');
assert.equal(reasons({ desired: 'not_accepting' })['act-pause'], 'Pause is unavailable: it is already paused.');
assert.equal(reasons({ desired: 'stopped' })['act-stop'], 'Stop is unavailable: it is already stopped.');
assert.equal(reasons({ desired: 'active', health_flags: [{ code: 'out_of_quota' }] }).reactivate, '', 'a quota-held agent can be reactivated');
assert.deepEqual(plain(m.quickActionsOf({ desired: 'active', health_flags: [{ code: 'out_of_quota' }] })), ['start', 'pause', 'stop'], 'the collapsed card offers the enabled quota restart');
assert.equal(reasons({ desired: 'stopped', role_deleted: true }).reactivate, 'Start is unavailable: its role was deleted.');
assert.equal(reasons({ desired: 'stopped', boundary_pending: true }).reactivate, 'Start is unavailable: it is waiting for folder or machine approval.');
assert.equal(reasons({ desired: 'stopped', writer_blocked_by: { holder: 'alice@test' } }).reactivate, '', 'legacy writer data does not block starting');
assert.equal(plain(m.stateOf({ desired: 'stopped', observed: 'stopped' })).why, undefined);
assert.equal(reasons({ desired: 'active' }, { lifecycle: 'archived' }).switch, 'Switch is unavailable: reopen the room first.');
assert.equal(reasons({ desired: 'active', recovery_block: true }).switch, 'Switch is unavailable: resolve the recovery item first.');
assert.equal(reasons({ desired: 'active', switch: { state: 'draining' } }).switch, 'Switch is unavailable: a switch is already in progress.');
assert.equal(reasons({ desired: 'active', switch: { state: 'failed' } }).switch, '');
assert.equal(reasons({ desired: 'active', switch: { state: 'started' } }).switch, '', 'a completed switch does not block the next one');
assert.deepEqual(reasons({ desired: 'active' }, { down: 'Agent controls are unavailable' }).message, '');
assert.equal(reasons({ desired: 'active' }, { down: 'Agent controls are unavailable' })['act-stop'], 'Stop is unavailable: Agent controls are unavailable.');
const [message, pause] = plain(m.actionsOf({ desired: 'active' }));
assert.deepEqual([message.title, pause.title, pause.label, pause.hint], ['Send this agent a private message', 'Pause: finish the current turn, then accept no new work', 'Pause', '']);

// The profile's rows: the card's facts first, the profile read's after; empty values are left out.
const harness = tool => ({ codex: 'Codex' }[tool] || tool), relative = value => `rel(${value})`;
const rows = (agent, profile) => plain(m.profileRows(agent, profile, { harness, relative }));
const base = { id: 'a1', tool: 'codex', variant: 'native', model: 'gpt-6-sol', role_key: 'worker', role_emoji: '🔧', desired: 'active', observed: 'in_turn', last_activity_at: at };
assert.deepEqual(rows(base, null).map(row => row[0]), ['Runtime', 'Agent ID']);
assert.deepEqual(rows(base, null).slice(0, 2).map(row => row[1]), ['gpt-6-sol · Codex/native', 'a1']);
assert.equal(rows({ ...base, variant: 'docker' }, null)[0][1], 'gpt-6-sol · Codex/docker');
assert.equal(rows({ ...base, variant: 'codex' }, null)[0][1], 'gpt-6-sol · Codex', 'a variant that is the tool itself adds nothing');
// The state is not a row: the card's pill is the one status line, so Status, Presence and Last seen are not repeated.
assert.deepEqual(['Status', 'Presence', 'Last seen'].filter(label => rows(base, null).some(row => row[0] === label)), []);
const full = rows(base, { native_session_id: 's1', resume_command: 'codex resume s1', cwd: '/w', branch: 'main', machine_name: 'Host', launcher_name: 'Codex', project_name: 'Alpha', repo_url: 'git@x', spawned_by: 'owner', spawned_at: at, resumed_from: 'old-bot', turns_today: 1, cost_today: 0.004, cost_today_estimated: true });
assert.equal(rows(base, { native_session_id: 's1' }).some(row => row[0] === 'Resume'), false, 'an unconfirmed session has no resume action');
assert.deepEqual(full.map(row => row[0]), ['Runtime', 'Session', 'Resume', 'CWD', 'Branch', 'Machine', 'Launcher', 'Project', 'Repo', 'Spawned', 'Continues', 'Last 24 h', 'Agent ID']);
assert.deepEqual(full.find(row => row[0] === 'Resume'), ['Resume', 'codex resume s1', { mono: true, copy: true }]);
assert.equal(full.find(row => row[0] === 'Last 24 h')[1], '1 turn · $0.0040 estimated');
assert.equal(full.find(row => row[0] === 'Spawned')[1], `owner · rel(${at})`);

assert.equal(m.outputStatus({ text: '' }), 'No output has been captured for this agent yet.');
assert.equal(m.outputStatus({ text: 'x', truncated: true }), 'Earlier output is not shown.');
assert.equal(m.outputStatus({ text: 'x' }), '');
assert.equal(m.outputFailure({ code: 'no_output', message: 'x' }), 'No output has been captured for this agent yet.');
assert.equal(m.outputFailure({ message: 'The machine is offline' }), 'The machine is offline');
assert.equal(m.outputFailure({}), 'The output could not be read.');
console.log('agents model: ok');

// Keep-alive on a card: the interval and what a stretch of pings costs, and a warning when the last ping failed or missed the cache.
const keepLine = keep => m.keepAliveOf({ keep_alive: keep });
assert.equal(m.keepAliveOf({}), null);
assert.equal(keepLine({ interval_minutes: 50, pings_per_stretch: 28 }).text, 'Cache kept warm every 50 min');
assert.equal(keepLine({ interval_minutes: 50, pings_per_stretch: 28, estimate_usd: 1.26 }).text, 'Cache kept warm every 50 min, up to 28 pings after its last turn ≈ $1.3');
assert.equal(keepLine({ interval_minutes: 50, pings_per_stretch: 28, estimate_usd: 0.04 }).text, 'Cache kept warm every 50 min, up to 28 pings after its last turn ≈ $0.04');
assert.equal(keepLine({ interval_minutes: 50, pings_per_stretch: 28, estimate_usd: 0.002 }).text.endsWith('≈ <$0.01'), true);
assert.equal(keepLine({ interval_minutes: 50, pings_per_stretch: 28, last_ok: true }).warning, '');
assert.match(keepLine({ interval_minutes: 50, pings_per_stretch: 28, last_ok: false, last_error: 'no key' }).warning, /^The last ping failed: no key\.$/);
assert.match(keepLine({ interval_minutes: 50, pings_per_stretch: 28, last_ok: true, missed: true }).warning, /did not find the cache/);
// The one status dot: green and cold, green and warm, red offline, orange when it is not known.
{
  const dot = (agent, at = nowMs) => plain(m.dotOf(agent, m.stateOf(agent), at));
  const idle = { desired: 'active', observed: 'idle', tool: 'claude' };
  assert.equal(dot({ ...idle, warm: { state: 'warm', sleeps_at: '2026-11-08T16:04:10Z' } }).kind, 'warm');
  assert.equal(dot({ ...idle, warm: { state: 'warm', sleeps_at: '2026-11-08T16:04:10Z' } }).title, 'Online \u00b7 warm, the next answer skips the start (\u22485 min left)');
  assert.match(dot({ ...idle, warm: { state: 'warm', sleeps_at: '2026-11-08T16:00:20Z' } }).title, /\(\u22481 min left\)$/, 'a window almost over still says one minute, never zero');
  assert.deepEqual(dot({ ...idle, warm: { state: 'warm', sleeps_at: 'garbage' } }), { kind: 'warm', title: 'Online \u00b7 warm, the next answer skips the start' }, 'an end that cannot be read is still warm, without minutes');
  assert.deepEqual(dot({ ...idle, warm: { state: 'sleeping' } }), { kind: 'cold', title: 'Online \u00b7 cold, the next answer starts the agent' });
  assert.equal(dot({ ...idle }).kind, 'cold', 'warm mode off or not applicable: cold, the next answer starts the agent');
  assert.equal(dot({ ...idle, warm: undefined }).kind, 'cold', 'a missing first supervisor report leaves the dot green and cold');
  assert.equal(dot({ ...idle, tool: 'codex' }).kind, 'cold');
  assert.equal(dot({ ...idle, warm: { state: 'unknown' } }).kind, 'unknown', 'a warm state that is not known is orange, not cold');
  assert.match(dot({ ...idle, warm: { state: 'unknown' } }).title, /^State unknown/);
  assert.equal(dot({ desired: 'active', observed: 'in_turn', tool: 'claude' }).kind, 'working', 'a working agent is online');
  assert.deepEqual([dot({ desired: 'stopped', observed: 'stopped' }).kind, dot({ desired: 'active', observed: 'lost' }).kind], ['offline', 'offline']);
  assert.match(dot({ desired: 'stopped', observed: 'stopped' }).title, /^Offline/);
  assert.deepEqual([dot({ desired: 'not_accepting', observed: 'idle' }).kind, dot({ desired: 'active', observed: 'idle', boundary_pending: true }).kind], ['unknown', 'unknown'], 'paused or waiting for a decision: orange');
  assert.deepEqual([dot({ desired: 'not_accepting', observed: 'idle' }).title, dot({ desired: 'active', observed: 'idle', boundary_pending: true }).title], ['Paused; no new work is accepted.', 'Waiting for project owner approval in Inbox.'], 'a paused or waiting agent says so, in its own words');
  assert.equal(m.warmOf({ observed: 'idle', warm: { state: 'unknown' } }, nowMs), null, 'and says neither warm nor sleeping');
}
console.log('agents model: keep-alive ok');
