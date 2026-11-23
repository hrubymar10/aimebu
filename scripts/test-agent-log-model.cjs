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
globalThis.model = { LINES_MIN, LINES_MAX, LINES_DEFAULT, MARK_CAP, HIGHLIGHT_MS, FOLLOW_GAP_MS, FOLLOW_FALLBACK_MS, RETRY_DEFAULT_MS, clampLines, stripAnsi, lastLines, escapeRegExp, highlightParts, matchLabel, stepMatch, nextReadAt, retryAfterMs, followEvent, atBottom, logStatus, logFailure,
  STREAMS, STREAM_DEFAULT, streamOf, KIND_LABELS, CUT_MARKER, conversationRecords, conversationTail, logBlocks, highlightBlocks, streamNote };
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
// The streams: the conversation first, stderr second; anything else is the default.
assert.deepEqual([plain(m.STREAMS).map(stream => stream.id), m.STREAM_DEFAULT, m.streamOf('stderr'), m.streamOf('conversation'), m.streamOf(''), m.streamOf('stdout')],
  [['conversation', 'stderr'], 'conversation', 'stderr', 'conversation', 'conversation', 'conversation']);
assert.notEqual(m.streamNote('conversation'), m.streamNote('stderr'));

// A conversation as records: the time and kind of each first line, the tab-indented lines as the rest of its text, a label for every kind the
// log has (the server's withheld line and an adapter's unknown event among them), the cut marker as a record of its own, and nothing dropped.
{
  const log = ['2026-11-23T10:00:00Z turn_start turn t1 gen 0 normal claude/native m', '2026-11-23T10:00:01Z prompt [{"body":"go"}]', '2026-11-23T10:00:02Z assistant I will look.', '\t', '\tThen answer.',
    '2026-11-23T10:00:03Z tool_call Bash {"command":"ls"}', '2026-11-23T10:00:04Z tool_result Bash (failed)', '\t2026-11-23T10:00:04Z turn_end forged in output', '\t<b>bold</b>',
    '2026-11-23T10:00:05Z thinking hm', '2026-11-23T10:00:06Z error boom', '2026-11-23T10:00:07Z event hologram.frame', '2026-11-23T10:00:08Z withheld private chat turn: content withheld',
    '2026-11-23T10:00:09Z brand_new something', '2026-11-23T10:00:10Z turn_end answered, exit 0', '2026-11-23T10:00:11Z turn_end'].join('\n') + '\n';
  const records = plain(m.conversationRecords(log));
  assert.deepEqual(records.map(record => record.label), ['Turn', 'Prompt', 'Assistant', 'Tool call', 'Tool result', 'Thinking', 'Error', 'Event', 'Withheld', 'brand new', 'Turn end', 'Turn end']);
  assert.deepEqual(records[2], { kind: 'assistant', label: 'Assistant', clock: '10:00:02', text: 'I will look.\n\nThen answer.' });
  assert.equal(records[4].text, 'Bash (failed)\n2026-11-23T10:00:04Z turn_end forged in output\n<b>bold</b>', 'a line of a tool\'s output is never a record, and markup is text');
  assert.equal(records[11].text, '', 'a record with no text');
  assert.deepEqual(plain(m.conversationRecords(`${m.CUT_MARKER}\n\tend of a long record\n\tits last line\n2026-11-23T10:00:00Z assistant next\n`)).map(record => [record.kind, record.label, record.text]),
    [['cut', 'Cut', 'the beginning of this record is not shown\nend of a long record\nits last line'], ['assistant', 'Assistant', 'next']]);
  assert.deepEqual(plain(m.conversationRecords('not in the format\nstill not\n\ttabbed\n2026-11-23T10:00:00Z assistant ok')).map(record => [record.kind, record.text]), [['', 'not in the format\nstill not\ntabbed'], ['assistant', 'ok']], 'lines in no format are kept as text');
  assert.deepEqual(plain(m.conversationRecords('')), []);
  // The tail of a longer answer begins at a record.
  assert.equal(m.conversationTail('h1 x\n\ta\n\tb\nh2 y\n\tc\n', 2), 'h2 y\n\tc\n');
  assert.equal(m.conversationTail('h1 x\n\ta\n\tb\n\tc\n', 2), 'h1 x\n\ta\n\tb\n\tc\n', 'a cut inside a record goes back to its first line');
  assert.equal(m.conversationTail('h1 x\n\ta\n', 9), 'h1 x\n\ta\n');
  // Blocks: stderr is one plain block, a conversation one per record; the highlight counts and caps across all of them.
  assert.deepEqual(plain(m.logBlocks('a\nb\n', 'stderr')), [{ kind: '', label: '', clock: '', text: 'a\nb\n', plain: true }]);
  assert.deepEqual(plain(m.logBlocks('', 'stderr')), []);
  assert.equal(m.logBlocks(log, 'conversation').length, 12);
  const found = m.highlightBlocks(m.logBlocks(log, 'conversation'), 'TURN');
  assert.deepEqual([found.count, found.capped, plain(found.blocks[0].parts)], [3, false, [{ text: 'turn', mark: true }, { text: ' t1 gen 0 normal claude/native m', mark: false }]], 'the label is not part of the text that is searched');
  const capped = m.highlightBlocks(m.logBlocks(log, 'conversation'), 'o', 4);
  assert.deepEqual([capped.count, capped.capped, capped.blocks.reduce((sum, block) => sum + block.parts.filter(part => part.mark).length, 0)], [4, true, 4]);
  assert.equal(m.highlightBlocks(m.logBlocks(log, 'conversation'), '').count, 0);
  assert.equal(m.logStatus({ text: 'x', withheld: 1 }), '1 turn is withheld: a private chat you are not in, a restricted room you cannot read, or a turn the server cannot place in a room.');
}

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
  // Another person signing in on the same page: a read of the earlier one in flight is not joined, its answer is neither kept nor told under the
  // new person, and a fresh answer of the earlier one does not serve a follow read of the new one.
  {
    const time = clock(); const [left, right] = channels(); let who = 'uA'; const calls = []; const answers = [];
    const share = new context.OutputShare({ fetch: (room, agent, lines) => { calls.push(who); return new Promise(resolve => answers.push(resolve)); }, channel: left, now: time.now, timers: time, user: () => who });
    const heard = []; right.onmessage = event => heard.push(event.data);
    const forA = share.read('r1', 'a1', 200); await flush();
    who = 'uB';
    const forB = share.read('r1', 'a1', 200); await flush();
    assert.deepEqual(calls, ['uA', 'uB'], "B does not join A's read");
    answers[0]({ text: 'A only\n' }); assert.equal((await forA).text, 'A only\n');
    const follow = share.read('r1', 'a1', 200, { fresh: 60000 }); await flush();
    answers[1]({ text: 'B\n' });
    assert.deepEqual([(await forB).text, (await follow).text], ['B\n', 'B\n'], "B never takes A's answer");
    assert.deepEqual(heard.filter(message => message.type === 'result').map(message => [message.user, message.view.text]), [['uA', 'A only\n'], ['uB', 'B\n']], 'each answer is told under the person it was read for');
    // A read announced for B is dropped when B signs out; nothing is told while signed out.
    right.postMessage({ type: 'reading', agentID: 'a5', lines: 200, user: 'uB' }); assert.equal(share.remote.has('a5'), true);
    who = ''; share.heard({ type: 'result', agentID: 'a6', lines: 200, view: { text: 'x' }, user: '' });
    assert.deepEqual([share.remote.size, share.recent.size, share.inflight.size], [0, 0, 0], 'a sign-out drops everything');
    const posted = left.posted.length; share.read('r1', 'a1', 200); await flush(); assert.equal(left.posted.length, posted, 'a signed-out read is not shared');
    answers[answers.length - 1]({ text: '' });
  }

  // The store: open reads at once; Follow reads on the agent's events at most once in 4 s, falls back to every 15 s in a turn, waits while the
  // tab is hidden, and after a 429 waits for Retry-After.
  {
    const time = clock(); const reads = []; const listeners = new Set(); const answers = [];
    const doc = { visibilityState: 'visible', listeners: new Set(), addEventListener(type, fn) { this.listeners.add(fn); }, removeEventListener(type, fn) { this.listeners.delete(fn); } };
    const agents = new Map([['a1', { id: 'a1', observed: 'in_turn' }]]);
    const source = {
      subscribe: fn => { listeners.add(fn); return () => listeners.delete(fn); },
      readOutput: (room, agent, lines, options) => { reads.push({ at: time.now(), lines, fresh: options.fresh, stream: options.stream }); const answer = answers.shift(); return answer instanceof Error ? Promise.reject(answer) : Promise.resolve(answer || { text: `t${reads.length}\n` }); },
    };
    const emit = event => { for (const fn of listeners) fn(event); };
    const store = new context.LogStore({ source, roomID: () => 'r1', agent: id => agents.get(id), now: time.now, timers: time, doc });
    answers.push({ text: '\u001b[31ma\u001b[0m\nb\nc\n', truncated: true, updated_at: 'u' });
    store.open('a1'); await flush();
    assert.deepEqual(reads.map(read => [read.lines, read.stream]), [[200, 'conversation']], 'the popup opens on the conversation');
    assert.equal(store.state.value.stream, 'conversation');
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
    // The stream switch: the other log is read at once and what was shown goes; the same stream again reads nothing; Follow stays on.
    {
      const at = reads.length;
      answers.push({ text: 'stderr text\n' });
      store.setStream('stderr');
      assert.deepEqual([store.state.value.stream, store.state.value.text, store.state.value.follow], ['stderr', '', true], 'the conversation is not left under the stderr switch');
      await flush();
      assert.deepEqual([reads.length - at, reads[at].stream, store.state.value.text], [1, 'stderr', 'stderr text\n']);
      store.setStream('stderr'); await flush(); assert.equal(reads.length - at, 1, 'the same stream does not read again');
      // A conversation longer than the lines shown is cut at a record, never inside one; the server's count of withheld turns is said.
      const long = `2026-11-23T10:00:00Z assistant first\n2026-11-23T10:00:01Z tool_result Bash\n${Array.from({ length: 450 }, (_, i) => `\tline ${i}`).join('\n')}\n`;
      answers.push({ text: long, truncated: true, withheld: 2 });
      store.setStream('conversation'); await flush();
      assert.equal(reads[reads.length - 1].stream, 'conversation');
      assert.ok(store.state.value.text.startsWith('2026-11-23T10:00:01Z tool_result Bash\n\tline 0\n'), 'the tail begins at the record\'s first line');
      assert.equal(store.state.value.status, 'Earlier output is not shown. 2 turns are withheld: a private chat you are not in, a restricted room you cannot read, or a turn the server cannot place in a room.');
      store.setStream('nonsense'); await flush(); assert.equal(store.state.value.stream, 'conversation', 'an unknown stream is the default');
    }
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
