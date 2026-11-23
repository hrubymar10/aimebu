// The agent log popup's rules, run in a vm: web/js/pages/agents/log-model.js (lines, ANSI, highlight, follow timing), the shared reader
// (web/js/agent-output-share.js) and the popup's store (web/js/pages/agents/log-store.js) over fake timers, a fake document and a fake source.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const read = file => fs.readFileSync(path.join(__dirname, '..', 'web', 'js', file), 'utf8');
const strip = source => source.replace(/^import .*$/gm, '').replace(/^export /gm, '');
const context = vm.createContext({ Date, Number, Math, JSON, Map, Set, Object, Array, String, RegExp, Promise, Error, queueMicrotask });
vm.runInContext(`${strip(read('pages/agents/log-model.js'))}
globalThis.model = { LINES_MIN, LINES_MAX, LINES_DEFAULT, MARK_CAP, HIGHLIGHT_MS, FOLLOW_GAP_MS, FOLLOW_FALLBACK_MS, RETRY_DEFAULT_MS, clampLines, stripAnsi, lastLines, escapeRegExp, highlightParts, matchLabel, stepMatch, nextReadAt, retryAfterMs, followEvent, atBottom, logStatus, logFailure };
${strip(read('agent-output-share.js'))}
globalThis.OutputShare = OutputShare; globalThis.REMOTE_WAIT_MS = REMOTE_WAIT_MS;
const signal = value => ({ value, peek() { return this.value; } });
${strip(read('pages/agents/log-store.js'))}
globalThis.LogStore = LogStore;`, context);
const m = context.model;
const plain = value => JSON.parse(JSON.stringify(value));

// Lines: whole numbers from 50 to 2000; anything else is the default.
assert.deepEqual([m.clampLines('200'), m.clampLines('10'), m.clampLines('99999'), m.clampLines('abc'), m.clampLines(''), m.clampLines('  350 '), m.clampLines('120.9')], [200, 50, 2000, 200, 200, 350, 120]);

// ANSI colour codes, cursor moves and OSC titles are taken out; the text around them stays.
assert.equal(m.stripAnsi('\u001b[31merror\u001b[0m: \u001b[1;33mwarn\u001b[m done\u001b[2K'), 'error: warn done');
assert.equal(m.stripAnsi('\u001b]0;title\u0007kept\u001b]8;;http://x\u001b\\link\u001b]8;;\u001b\\'), 'keptlink');
assert.equal(m.stripAnsi('plain [31m not an escape'), 'plain [31m not an escape');

// The last N lines of a longer answer, with or without a final newline.
assert.equal(m.lastLines('a\nb\nc\n', 2), 'b\nc\n');
assert.equal(m.lastLines('a\nb\nc', 2), 'b\nc');
assert.equal(m.lastLines('a\nb\n', 5), 'a\nb\n');

// The highlight is literal and case-insensitive: a regular expression's characters match themselves and never throw.
for (const query of ['.*', '(', '[a-z]+', 'a|b', '\\d', '$^', '/x/', '?']) {
  const found = m.highlightParts(`x ${query} y ${query.toUpperCase()}`, query);
  assert.ok(found.count >= 1, `"${query}" matches itself`);
  assert.equal(found.parts.filter(part => part.mark).every(part => part.text.toLowerCase() === query.toLowerCase()), true, `"${query}" marks only itself`);
}
assert.equal(m.highlightParts('aaaa', '.').count, 0, 'a dot is a dot, not any character');
const mixed = m.highlightParts('Error then ERROR and error', 'error');
assert.deepEqual(plain(mixed.parts), [{ text: 'Error', mark: true }, { text: ' then ', mark: false }, { text: 'ERROR', mark: true }, { text: ' and ', mark: false }, { text: 'error', mark: true }]);
assert.equal(mixed.parts.map(part => part.text).join(''), 'Error then ERROR and error', 'the parts put back together are the text');
// Markup in the log is text: the parts carry it unchanged (the dialog writes them as text nodes).
assert.deepEqual(plain(m.highlightParts('<img src=x onerror=alert(1)> img', 'img').parts.map(part => part.text)), ['<', 'img', ' src=x onerror=alert(1)> ', 'img']);
assert.deepEqual(plain(m.highlightParts('', 'x')), { parts: [], count: 0, capped: false });
assert.deepEqual(plain(m.highlightParts('abc', '')), { parts: [{ text: 'abc', mark: false }], count: 0, capped: false });
// At most MARK_CAP marks; the rest of the text stays, unmarked, and the counter says "N+".
const many = m.highlightParts('ab '.repeat(2500), 'ab');
assert.deepEqual([many.count, many.capped, many.parts.filter(part => part.mark).length, many.parts.map(part => part.text).join('').length], [m.MARK_CAP, true, 2000, 'ab '.repeat(2500).length]);
assert.equal(m.highlightParts('ab '.repeat(2000), 'ab').capped, false, 'exactly the cap is not more than the cap');
assert.deepEqual([m.matchLabel(many, 'ab'), m.matchLabel({ count: 0 }, 'x'), m.matchLabel({ count: 1 }, 'x'), m.matchLabel({ count: 4 }, 'x'), m.matchLabel({ count: 4 }, 'x', 2), m.matchLabel({ count: 4 }, '')],
  ['2000+ matches', 'No matches', '1 match', '4 matches', '3 of 4', '']);
// Enter goes to the next match and Shift+Enter to the previous one, wrapping.
assert.deepEqual([m.stepMatch(-1, 3, 1), m.stepMatch(-1, 3, -1), m.stepMatch(0, 3, 1), m.stepMatch(2, 3, 1), m.stepMatch(0, 3, -1), m.stepMatch(0, 0, 1)], [0, 2, 1, 0, 2, -1]);

// Follow timing: at most one read in FOLLOW_GAP_MS, never before a 429's wait is over.
assert.equal(m.FOLLOW_GAP_MS, 4000); assert.equal(m.FOLLOW_FALLBACK_MS, 15000); assert.equal(m.HIGHLIGHT_MS, 150);
assert.equal(m.nextReadAt({ now: 10000 }), 10000, 'the first read is at once');
assert.equal(m.nextReadAt({ now: 10000, lastReadAt: 9000 }), 13000);
assert.equal(m.nextReadAt({ now: 10000, lastReadAt: 1000 }), 10000);
assert.equal(m.nextReadAt({ now: 10000, lastReadAt: 9000, blockedUntil: 70000 }), 70000);
assert.deepEqual([m.retryAfterMs('60'), m.retryAfterMs('0'), m.retryAfterMs(''), m.retryAfterMs('soon'), m.retryAfterMs(new Date(30000).toUTCString(), 10000)], [60000, 0, 60000, 60000, 20000]);
// Only events of the agent shown, and only its activity, turns and answers.
assert.deepEqual(['supervisor_activity', 'turn_ack_started', 'turn_finished', 'answer_committed', 'agent_activity', 'message_created', 'supervisor_seen'].map(kind => m.followEvent({ kind, agent_id: 'a1' }, 'a1')), [true, true, true, true, false, false, false]);
assert.equal(m.followEvent({ kind: 'turn_finished', agent_id: 'a2' }, 'a1'), false);
assert.deepEqual([m.atBottom({ scrollTop: 492, scrollHeight: 800, clientHeight: 300 }), m.atBottom({ scrollTop: 400, scrollHeight: 800, clientHeight: 300 })], [true, false]);
// The words under the text.
assert.equal(m.logStatus({ text: '' }), 'No output has been captured for this agent yet.');
assert.equal(m.logStatus({ text: 'x', truncated: true }), 'Earlier output is not shown.');
assert.equal(m.logFailure({ status: 403, message: 'forbidden' }), 'Only the project owner or an admin can read an agent\'s logs.');
assert.equal(m.logFailure({ status: 404, code: 'no_output' }), 'No output has been captured for this agent yet.');
assert.equal(m.logFailure({ status: 429 }, 1000, 31000), 'Logs were read too often; trying again in 30s.');
assert.equal(m.logFailure({ status: 409, message: 'the machine this agent runs on is not connected' }), 'the machine this agent runs on is not connected');

// Fake time for the shared reader and the store.
function clock() {
  let now = 100000, seq = 0;
  const timers = new Map();
  const api = {
    now: () => now,
    setTimeout: (fn, ms) => { const id = ++seq; timers.set(id, { at: now + Math.max(0, ms), fn }); return id; },
    clearTimeout: id => { timers.delete(id); },
    setInterval: (fn, ms) => { const id = ++seq; timers.set(id, { at: now + ms, fn, every: ms }); return id; },
    clearInterval: id => { timers.delete(id); },
    async advance(ms) {
      const end = now + ms;
      for (;;) {
        const due = [...timers.entries()].filter(([, timer]) => timer.at <= end).sort((a, b) => a[1].at - b[1].at)[0];
        if (!due) break;
        const [id, timer] = due; now = timer.at;
        if (timer.every) timer.at += timer.every; else timers.delete(id);
        timer.fn(); await flush();
      }
      now = end; await flush();
    },
    pending: () => timers.size,
  };
  return api;
}
const flush = async () => { for (let i = 0; i < 10; i++) await Promise.resolve(); };
// Two tabs' channels: what one posts, the other hears.
function channels() {
  const pair = [{ onmessage: null, posted: [] }, { onmessage: null, posted: [] }];
  pair.forEach((channel, index) => { channel.postMessage = message => { channel.posted.push(message); const other = pair[1 - index]; other.onmessage?.({ data: JSON.parse(JSON.stringify(message)) }); }; channel.close = () => {}; });
  return pair;
}

(async () => {
  // In one tab: a read already out for as many lines or more answers a second ask; a read for more lines goes out on its own.
  {
    const time = clock(); const calls = []; let answer;
    const share = new context.OutputShare({ fetch: (room, agent, lines) => { calls.push(lines); return new Promise(resolve => { answer = resolve; }); }, now: time.now, timers: time, user: () => 'u1' });
    const first = share.read('r1', 'a1', 500), second = share.read('r1', 'a1', 200);
    await flush(); assert.deepEqual(calls, [500], 'a smaller ask joins the read in flight');
    answer({ text: 'one\n' }); assert.deepEqual([await first, await second].map(view => view.text), ['one\n', 'one\n']);
    // A fresh answer serves a follow read; an old one or an asked-for read goes to the server.
    await share.read('r1', 'a1', 200, { fresh: 4000 }); assert.deepEqual(calls, [500]);
    const asked = share.read('r1', 'a1', 200); await flush(); assert.deepEqual(calls, [500, 200]); answer({ text: 'two\n' }); await asked;
    await time.advance(5000); const old = share.read('r1', 'a1', 200, { fresh: 4000 }); await flush(); assert.deepEqual(calls, [500, 200, 200]); answer({ text: 'three\n' }); await old;
  }
  // Across two tabs of the same person: the second waits for the first's read and takes its answer; another person's tab is not answered.
  {
    const time = clock(); const [left, right] = channels(); const calls = { left: 0, right: 0 }; let answerLeft;
    const a = new context.OutputShare({ fetch: () => { calls.left++; return new Promise(resolve => { answerLeft = resolve; }); }, channel: left, now: time.now, timers: time, user: () => 'u1' });
    const b = new context.OutputShare({ fetch: () => { calls.right++; return Promise.resolve({ text: 'own\n' }); }, channel: right, now: time.now, timers: time, user: () => 'u1' });
    const first = a.read('r1', 'a1', 200); await flush();
    const second = b.read('r1', 'a1', 200, { fresh: 4000 }); await flush();
    answerLeft({ text: 'shared\n', truncated: false });
    assert.deepEqual([(await first).text, (await second).text, calls.left, calls.right], ['shared\n', 'shared\n', 1, 0], 'two tabs, one read');
    // A follow read in the other tab right after takes the fresh answer it heard.
    assert.equal((await b.read('r1', 'a1', 200, { fresh: 4000 })).text, 'shared\n'); assert.equal(calls.right, 0);
    // A failed read lets the waiting tab read for itself; so does a tab that never answers (after REMOTE_WAIT_MS).
    const failing = a.read('r1', 'a2', 200); await flush(); const waiting = b.read('r1', 'a2', 200); await flush();
    answerLeft(Promise.reject(Object.assign(new Error('offline'), { status: 409 })));
    await assert.rejects(failing); assert.equal((await waiting).text, 'own\n'); assert.equal(calls.right, 1);
    right.onmessage({ data: { type: 'reading', agentID: 'a3', lines: 200, user: 'u1' } });
    const silent = b.read('r1', 'a3', 200); await time.advance(context.REMOTE_WAIT_MS); assert.equal((await silent).text, 'own\n');
    const stranger = new context.OutputShare({ fetch: () => Promise.resolve({ text: 'x' }), channel: { postMessage() {}, close() {} }, now: time.now, timers: time, user: () => 'u1' });
    stranger.heard({ type: 'result', agentID: 'a9', lines: 200, view: { text: 'secret' }, user: 'u2' });
    assert.equal(stranger.recent.has('a9'), false, 'an answer of another signed-in person is ignored');
  }

  // The store: open reads at once; Follow reads on the agent's events at most once in 4 s, falls back to every 15 s in a turn, waits while the
  // tab is hidden, and after a 429 waits for Retry-After.
  {
    const time = clock(); const reads = []; const listeners = new Set(); const answers = [];
    const doc = { visibilityState: 'visible', listeners: new Set(), addEventListener(type, fn) { this.listeners.add(fn); }, removeEventListener(type, fn) { this.listeners.delete(fn); } };
    const agents = new Map([['a1', { id: 'a1', observed: 'in_turn' }]]);
    const source = {
      subscribe: fn => { listeners.add(fn); return () => listeners.delete(fn); },
      readOutput: (room, agent, lines, options) => { reads.push({ at: time.now(), lines, fresh: options.fresh }); const answer = answers.shift(); return answer instanceof Error ? Promise.reject(answer) : Promise.resolve(answer || { text: `t${reads.length}\n` }); },
    };
    const emit = event => { for (const fn of listeners) fn(event); };
    const store = new context.LogStore({ source, roomID: () => 'r1', agent: id => agents.get(id), now: time.now, timers: time, doc });
    answers.push({ text: '\u001b[31ma\u001b[0m\nb\nc\n', truncated: true, updated_at: 'u' });
    store.open('a1'); await flush();
    assert.deepEqual(reads.map(read => read.lines), [200]);
    assert.deepEqual([store.state.value.text, store.state.value.status, store.state.value.loading], ['a\nb\nc\n', 'Earlier output is not shown.', false], 'ANSI is stripped and the status says what was cut');
    emit({ kind: 'turn_finished', agent_id: 'a1' }); await flush(); assert.equal(reads.length, 1, 'without Follow an event reads nothing');
    store.setLines('400'); await flush(); assert.deepEqual(reads.map(read => read.lines), [200, 400], 'a new number of lines reads again');
    store.setLines('400'); await flush(); assert.equal(reads.length, 2, 'the same number does not');
    const start = time.now();
    store.setFollow(true); await flush();
    assert.equal(reads.length, 2, 'turning Follow on right after a read waits for the gap');
    await time.advance(4000); assert.equal(reads.length, 3); assert.equal(reads[2].at - start, 4000); assert.equal(reads[2].fresh, 4000, 'a follow read may take a shared fresh answer');
    // A burst of events makes one read, 4 s after the last one began.
    for (let i = 0; i < 5; i++) emit({ kind: 'supervisor_activity', agent_id: 'a1' });
    emit({ kind: 'supervisor_activity', agent_id: 'other' });
    await time.advance(3999); assert.equal(reads.length, 3); await time.advance(1); assert.equal(reads.length, 4);
    // Nothing in the stream: the agent in a turn is read every 15 s; an idle one is not.
    await time.advance(15000); assert.equal(reads.length, 5, 'the fallback reads an agent in a turn');
    agents.get('a1').observed = 'idle'; await time.advance(30000); assert.equal(reads.length, 5, 'an idle agent is not polled');
    // A hidden tab reads nothing, and once when it is shown again.
    doc.visibilityState = 'hidden'; emit({ kind: 'answer_committed', agent_id: 'a1' }); await time.advance(10000); assert.equal(reads.length, 5);
    doc.visibilityState = 'visible'; for (const fn of doc.listeners) fn(); await time.advance(0); assert.equal(reads.length, 6, 'the hidden tab reads once it is visible');
    // A 429 keeps the text, says when it tries again and waits for Retry-After.
    const before = store.state.value.text;
    answers.push(Object.assign(new Error('rate limited'), { status: 429, retryAfterHeader: '60' }));
    emit({ kind: 'turn_ack_started', agent_id: 'a1' }); await time.advance(4000);
    assert.equal(reads.length, 7); assert.equal(store.state.value.text, before, 'a 429 keeps the text on screen'); assert.match(store.state.value.status, /trying again in 6\ds/);
    const blockedAt = time.now();
    emit({ kind: 'turn_ack_started', agent_id: 'a1' }); await time.advance(59000); assert.equal(reads.length, 7, 'no read during the wait');
    await time.advance(2000); assert.equal(reads.length, 8); assert.ok(reads[7].at - blockedAt >= 60000, 'the read waits the whole Retry-After');
    // Follow off stops every timer; close drops an answer still out.
    store.setFollow(false); assert.equal(time.pending(), 0, 'Follow off leaves no timer');
    let late; source.readOutput = () => new Promise(resolve => { late = resolve; });
    store.setLines('600'); await flush(); store.close(); late({ text: 'late\n' }); await flush();
    assert.equal(store.state.value, null, 'an answer for a closed popup draws nothing');
    // An explicit read that fails shows only the refusal (a 403 for someone who may not read the logs).
    source.readOutput = () => Promise.reject(Object.assign(new Error('forbidden'), { status: 403 }));
    store.open('a1'); await flush();
    assert.deepEqual([store.state.value.text, store.state.value.status], ['', 'Only the project owner or an admin can read an agent\'s logs.']);
    store.destroy(); assert.equal(listeners.size, 0); assert.equal(doc.listeners.size, 0);
  }
  console.log('agent log model test passed');
})().catch(error => { console.error(error); process.exit(1); });
