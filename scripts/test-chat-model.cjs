// The chat's message rules, DOM-free, run in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'chat', 'model.js'), 'utf8');
const context = {};
vm.createContext(context);
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { treeLayout, plainText, messageSnippet, threadCount, threadLabel, threadTitle, threadChip, newRepliesLabel, latestLabel, groupReactions, articleClass, liveArrival, liveRoute, newestShownSeq, trimCap, overCap, answerEligible, answerArea, answersThroughInbox, recallCandidates, recallStep, handoffOutcome };', context);
const api = context.api;
const plain = value => JSON.parse(JSON.stringify(value));

assert.equal(api.plainText('# Heading\n> quoted **bold** and `code` and [a link](https://x.y) and _soft_'), 'Heading\nquoted bold and code and a link and soft');
assert.equal(api.messageSnippet({ body: '  two\n\nlines   here ' }), 'two lines here');
assert.equal(api.messageSnippet({ body: '', attachments: [{ id: 'a' }] }), '[attachment]');
assert.equal(api.messageSnippet({ body: 'x'.repeat(100) }), `${'x'.repeat(95)}…`);
assert.equal(api.messageSnippet({ body: 'y'.repeat(96) }), 'y'.repeat(96));
assert.equal(api.messageSnippet(undefined), '');

// A handoff's outcome badge: a word and a glyph for each known outcome, failed with its own class; none for any other message or an older handoff.
assert.deepEqual(plain(api.handoffOutcome({ outcome: 'failed' })), { outcome: 'failed', className: 'handoff-outcome handoff-outcome-failed', glyph: '✕', label: 'Failed', title: 'Handoff: the task could not be done; the message says why' });
assert.deepEqual(['done', 'partial'].map(outcome => api.handoffOutcome({ outcome }).label), ['Done', 'Partial']);
assert.notEqual(api.handoffOutcome({ outcome: 'failed' }).className, api.handoffOutcome({ outcome: 'done' }).className);
for (const message of [{}, { outcome: '' }, { outcome: 'unknown' }, { outcome: 'toString' }, { outcome: '__proto__' }, null, undefined]) assert.equal(api.handoffOutcome(message), null);

const counts = new Map([['a', 2], ['b', 5]]), server = new Map([['a', 4]]);
assert.deepEqual(['a', 'b', 'c'].map(id => api.threadCount(counts, server, id)), [4, 5, 0]);
assert.equal(api.threadCount(counts, undefined, 'a'), 2);
assert.deepEqual([0, 1, 3].map(api.threadLabel), ['Reply in thread', 'Replies · 1', 'Replies · 3']);
assert.deepEqual([0, 2].map(api.threadTitle), ['Replies', 'Replies · 2']);
assert.deepEqual([0, 1, 4].map(count => plain(api.threadChip(count))), [
  { hidden: true, text: '0 replies', label: 'Open thread, 0 replies' }, { hidden: false, text: '1 reply', label: 'Open thread, 1 reply' }, { hidden: false, text: '4 replies', label: 'Open thread, 4 replies' }]);
assert.deepEqual([1, 3].map(api.newRepliesLabel), ['1 new reply', '3 new replies']);
assert.deepEqual([0, 2].map(api.latestLabel), ['Back to latest', 'Back to latest · 2 new']);

assert.deepEqual(plain(api.groupReactions([{ emoji: '👍', member_id: 'u1', member_name: 'ann' }, { emoji: '🎉', member_id: 'a1' }, { emoji: '👍', member_id: 'me', member_name: 'me' }], 'me')), [
  { emoji: '👍', count: 2, title: 'ann, me', pressed: true }, { emoji: '🎉', count: 1, title: 'a1', pressed: false }]);
assert.deepEqual(plain(api.groupReactions([], 'me')), []);

assert.deepEqual([{ author_kind: 'system' }, { author_kind: 'user', author_id: 'me' }, { author_kind: 'user', author_id: 'x' }, { author_kind: 'agent', author_id: 'me' }].map(message => api.articleClass(message, 'me')), ['chat-msg chat-msg-system', 'chat-msg self', 'chat-msg', 'chat-msg']);
assert.deepEqual([{}, { thread_root: 'r' }, { author_kind: 'system' }].map(message => api.liveArrival(message, { initialLoading: false, contextMode: false })), [true, true, false]);
assert.deepEqual([[true, false], [false, true]].map(([initialLoading, contextMode]) => api.liveArrival({}, { initialLoading, contextMode })), [false, false]);

const base = { hasMoreOlder: false, oldestSeq: 0, rootRendered: false, openThreadID: undefined, threadLatestHidden: true, threadEntryKnown: false, contextMode: false, known: false, missed: false };
const route = (message, patch) => api.liveRoute(message, { ...base, ...patch });
assert.equal(route({ seq: 5 }, {}), 'add');
assert.equal(route({ seq: 5 }, { hasMoreOlder: true, oldestSeq: 10 }), 'skip', 'a row the live cap already dropped');
assert.equal(route({ seq: 15 }, { hasMoreOlder: true, oldestSeq: 10 }), 'add');
assert.equal(route({ seq: 10 }, { hasMoreOlder: true, oldestSeq: 10 }), 'add', 'the oldest row itself is not older than the oldest');
assert.equal(route({ seq: 5, thread_root: 'r' }, { hasMoreOlder: true, oldestSeq: 10, rootRendered: true }), 'skip', 'a reply is a row like any other, so the cap applies to it');
assert.equal(route({ seq: 5, thread_root: 'r' }, {}), 'add', 'a reply is a row of the room whether or not its root or a panel is there');
assert.equal(route({ seq: 5, thread_root: 'r' }, { rootRendered: true }), 'add');
assert.equal(route({ seq: 5, thread_root: 'r' }, { openThreadID: 'r' }), 'add', 'an open thread takes its reply');
assert.equal(route({ id: 'm', thread_root: 'r' }, { openThreadID: 'r', threadLatestHidden: false }), 'new-reply', 'a reply while the panel is not at its end is counted');
assert.equal(route({ id: 'm', thread_root: 'r' }, { openThreadID: 'r', threadLatestHidden: false, threadEntryKnown: true }), 'add');
assert.equal(route({ seq: 5 }, { contextMode: true }), 'missed', 'a history window counts it on the button');
assert.equal(route({ seq: 5 }, { contextMode: true, known: true }), 'skip');
assert.equal(route({ seq: 5 }, { contextMode: true, missed: true }), 'skip');
assert.equal(route({ seq: 5, thread_root: 'r' }, { contextMode: true, rootRendered: true }), 'panel', 'a window counts the reply on its root and takes no row');
assert.equal(route({ seq: 5, thread_root: 'r' }, { contextMode: true, openThreadID: 'r' }), 'panel');
assert.equal(route({ seq: 5, thread_root: 'r' }, { contextMode: true }), 'missed', 'a reply of nothing shown is counted on the button');

assert.deepEqual([{ initialLoading: true }, { newestSeq: 40 }, { newestSeq: 0 }, { contextMode: true, newestSeq: 40, lastRowSeq: () => 12 }, { catchingUp: true, newestSeq: 40, lastRowSeq: () => 0 }].map(state => api.newestShownSeq({ initialLoading: false, contextMode: false, catchingUp: false, newestSeq: 0, lastRowSeq: () => 0, ...state })), [0, 40, 0, 12, 0]);
const never = () => { throw new Error('the list was read'); };
assert.deepEqual([{ initialLoading: true }, { newestSeq: 9 }].map(state => api.newestShownSeq({ initialLoading: false, contextMode: false, catchingUp: false, newestSeq: 0, lastRowSeq: never, ...state })), [0, 9]);
assert.deepEqual([api.trimCap(true, 300, 800), api.trimCap(false, 300, 800)], [300, 800]);
assert.deepEqual([[10, 10], [11, 10], [3, 10]].map(([rows, cap]) => api.overCap(rows, cap)), [0, 1, 0]);

const ask = { kind: 'question', proposed_answers: ['alpha', 'beta'], free_text: false };
assert.deepEqual([{ kind: 'question' }, { kind: 'approval' }, { kind: 'note' }, { kind: 'question', boundary_pending: true }, { kind: 'question', can_decide: false }, null].map(api.answerEligible), [true, true, false, false, false, false]);
assert.deepEqual([['@zed beta', '@zed'], ['beta', '@zed'], ['@zed gamma', '@zed'], ['@other beta', '@zed'], ['@zed beta @other', '@zed']].map(([body, who]) => api.answersThroughInbox(ask, who, body, true)), [true, true, true, false, false]);
assert.equal(api.answersThroughInbox(ask, '@zed', '@zed beta'), false, 'a typed option without using its control is an ordinary reply');
assert.deepEqual([['anything at all', '@zed'], ['@zed ok', '@zed'], ['@else ok', '@zed']].map(([body, who]) => api.answersThroughInbox({ kind: 'question', free_text: true }, who, body)), [true, true, false]);
assert.equal(api.answersThroughInbox({ ...ask, can_decide: false }, '@zed', '@zed beta'), false);
assert.equal(api.answersThroughInbox({ kind: 'question', options: ['yes', 'no'], free_text: false }, '', 'yes'), true, 'a plain option of an approval');
assert.equal(api.answersThroughInbox({ kind: 'question', open_questions: [{ question: 'Route?' }], free_text: true }, '@zed', '2) check the host architecture'), false, 'an ordinary reply cannot close Open Questions');
assert.equal(api.answersThroughInbox({ kind: 'question', open_questions: [{ question: 'Route?' }], free_text: true }, '@zed', '@zed Q1) custom route', true), true, 'an edited Open Questions control draft can answer');
assert.equal(api.answersThroughInbox({ kind: 'question', proposed_answers: ['alpha'], free_text: true }, '@zed', '@zed unrelated reply'), false, 'a proposed answer needs an exact choice');
assert.equal(api.answersThroughInbox({ kind: 'question', proposed_answers: ['alpha'], free_text: true }, '@zed', '@zed edited', true), true, 'an edited control draft still answers');

assert.deepEqual([[{ kind: 'question' }, null, false], [null, { proposed_answers: ['a'] }, true], [null, { open_questions: [{ question: 'q' }] }, false], [null, { proposed_answers: [] }, true], [null, null, false], [{ kind: 'note' }, {}, true]].map(([item, message, answered]) => api.answerArea(item, message, answered)), ['controls', 'history', 'history', 'done', 'none', 'done']);

const m = (seq, body, extra = {}) => ({ seq, body, author_kind: 'user', author_id: 'me', ...extra });
assert.deepEqual(plain(api.recallCandidates([m(5, 'c'), m(1, 'a'), m(2, 'a'), m(3, 'b'), m(4, 'x', { author_id: 'other' }), m(6, 'r', { thread_root: 'root' }), m(7, 'sys', { author_kind: 'agent' }), m(8, 'a')], 'me')), ['a', 'b', 'c', 'r', 'a']);
assert.equal(api.recallCandidates(Array.from({ length: 250 }, (_, i) => m(i, `m${i}`)), 'me').length, 200);

const step = (key, value, recall, at = value.length, end = at, candidates = ['a', 'b', 'c']) => plain(api.recallStep({ key, value, at, end }, recall, candidates));
assert.deepEqual(step('ArrowUp', '', { index: null, draft: '' }), { index: 2, draft: '', value: 'c' });
assert.deepEqual(step('ArrowUp', 'typing', { index: null, draft: '' }, 0, 0), { index: 2, draft: 'typing', value: 'c' }, 'at the start of a draft it keeps the draft');
assert.equal(step('ArrowUp', 'typing', { index: null, draft: '' }, 3, 3), null, 'in the middle of a draft Up is the caret');
assert.deepEqual(step('ArrowUp', 'c', { index: 2, draft: 'typing' }), { index: 1, draft: 'typing', value: 'b' });
assert.deepEqual(step('ArrowUp', 'a', { index: 0, draft: 'typing' }), { index: 0, draft: 'typing', value: 'a' }, 'the oldest stays');
assert.equal(step('ArrowUp', 'a\nb', { index: 0, draft: '' }, 3), null, 'while recalling only the first line walks back');
assert.equal(step('ArrowUp', '', { index: null, draft: '' }, 0, 0, []), null, 'nothing to recall');
assert.deepEqual(step('ArrowDown', 'a', { index: 0, draft: 'typing' }), { index: 1, draft: 'typing', value: 'b' });
assert.deepEqual(step('ArrowDown', 'c', { index: 2, draft: 'typing' }), { index: null, draft: 'typing', value: 'typing' }, 'past the newest the draft is back');
assert.equal(step('ArrowDown', 'draft', { index: null, draft: '' }), null);
assert.equal(step('ArrowDown', 'a\nb', { index: 0, draft: '' }, 0), null, 'Down on a line that is not the last moves the caret');
assert.deepEqual(step('Escape', 'b', { index: 1, draft: 'typing' }), { index: null, draft: 'typing', value: 'typing' });
assert.equal(step('Escape', 'b', { index: null, draft: '' }), null);
assert.equal(step('a', 'b', { index: 1, draft: '' }), null);
// A conversation as `tree` prints it: depth first, the replies in the order they were written, with the guide lines of each row.
{
  const message = (id, reply_to) => ({ id, no: Number(id.slice(1)), reply_to });
  const entries = [message('m1'), message('m2', 'm1'), message('m3', 'm2'), message('m4', 'm3'), message('m5', 'm4'), message('m6', 'm2'), message('m7', 'm1')];
  const { order, info } = api.treeLayout(entries), at = id => { const { depth, guides, last, branch, deep, orphan } = info.get(id); return plain({ depth, guides, last, branch, deep, orphan }); };
  assert.deepEqual(plain(order.map(item => item.id)), ['m1', 'm2', 'm3', 'm4', 'm5', 'm6', 'm7'], 'a message, its first reply\'s whole subtree, then its next reply');
  // m1
  // ├── m2        (its sibling m7 follows: a tee)
  // │   ├── m3    (the pipe of m2 goes on; m6 follows)
  // │   │   └── m4
  // │   │       └── m5
  // │   └── m6
  // └── m7
  assert.deepEqual(at('m1'), { depth: 0, guides: [], last: true, branch: false, deep: false, orphan: false });
  assert.deepEqual(at('m2'), { depth: 1, guides: [], last: false, branch: true, deep: false, orphan: false });
  assert.deepEqual(at('m3'), { depth: 2, guides: [true], last: false, branch: true, deep: false, orphan: false });
  assert.deepEqual(at('m4'), { depth: 3, guides: [true, true], last: true, branch: true, deep: false, orphan: false }, 'the pipes of m2 and m3 go on past m4 (m3 and m2 have later replies)');
  assert.deepEqual(at('m5'), { depth: 4, guides: [true, true, false], last: true, branch: true, deep: false, orphan: false }, 'under m4, which was the last: blank');
  assert.deepEqual(at('m6'), { depth: 2, guides: [true], last: true, branch: true, deep: false, orphan: false });
  assert.deepEqual(at('m7'), { depth: 1, guides: [], last: true, branch: true, deep: false, orphan: false });
  assert.equal(info.get('m6').parent.id, 'm2');
  // Past the cap no step is added: the row keeps the guides of the cap's depth and says it is deep.
  const chain = [message('c1'), ...Array.from({ length: 9 }, (_, i) => message(`c${i + 2}`, `c${i + 1}`))];
  const deep = api.treeLayout(chain, 6).info;
  assert.deepEqual(plain([6, 7, 9].map(level => { const item = deep.get(`c${level + 1}`); return [item.depth, item.shown, item.guides.length, item.deep]; })), [[6, 6, 5, false], [7, 6, 5, true], [9, 6, 5, true]], 'six steps at most, then "deep"');
  // An older page is not loaded: a reply whose parent is not among the entries is a root of its own, with its own subtree under it, in the order written.
  const partial = api.treeLayout([message('m1'), message('m2', 'm1'), message('m9', 'm8'), message('m10', 'm9'), message('m11', 'm1')]);
  assert.deepEqual(plain(partial.order.map(item => item.id)), ['m1', 'm2', 'm11', 'm9', 'm10'], 'the roots in order: the first message and its replies, then the orphan and its replies');
  assert.deepEqual(plain([partial.info.get('m9').orphan, partial.info.get('m9').branch, partial.info.get('m10').depth, partial.info.get('m10').guides]), [true, false, 1, []]);
  // Just the first message, and nothing at all.
  assert.deepEqual(plain(api.treeLayout([message('m1')]).order.map(item => item.id)), ['m1']);
  assert.deepEqual(plain(api.treeLayout([]).order), []);
}
console.log('chat model: ok');
