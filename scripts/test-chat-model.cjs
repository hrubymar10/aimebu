// The chat's message rules, DOM-free, run in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'chat', 'model.js'), 'utf8');
const context = {};
vm.createContext(context);
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { plainText, messageSnippet, threadCount, threadLabel, threadTitle, threadChip, newRepliesLabel, latestLabel, groupReactions, articleClass, liveArrival, liveRoute, newestShownSeq, trimCap, overCap, answerEligible, answerArea, answersThroughInbox, recallCandidates, recallStep };', context);
const api = context.api;
const plain = value => JSON.parse(JSON.stringify(value));

assert.equal(api.plainText('# Heading\n> quoted **bold** and `code` and [a link](https://x.y) and _soft_'), 'Heading\nquoted bold and code and a link and soft');
assert.equal(api.messageSnippet({ body: '  two\n\nlines   here ' }), 'two lines here');
assert.equal(api.messageSnippet({ body: '', attachments: [{ id: 'a' }] }), '[attachment]');
assert.equal(api.messageSnippet({ body: 'x'.repeat(100) }), `${'x'.repeat(95)}…`);
assert.equal(api.messageSnippet({ body: 'y'.repeat(96) }), 'y'.repeat(96));
assert.equal(api.messageSnippet(undefined), '');

const counts = new Map([['a', 2], ['b', 5]]), server = new Map([['a', 4]]);
assert.deepEqual(['a', 'b', 'c'].map(id => api.threadCount(counts, server, id)), [4, 5, 0]);
assert.equal(api.threadCount(counts, undefined, 'a'), 2);
assert.deepEqual([0, 1, 3].map(api.threadLabel), ['Reply in thread', 'Thread · 1', 'Thread · 3']);
assert.deepEqual([0, 2].map(api.threadTitle), ['Thread', 'Thread · 2']);
assert.deepEqual([0, 1, 4].map(count => plain(api.threadChip(count))), [
  { hidden: true, text: '0 replies', label: 'Open thread, 0 replies' }, { hidden: false, text: '1 reply', label: 'Open thread, 1 reply' }, { hidden: false, text: '4 replies', label: 'Open thread, 4 replies' }]);
assert.deepEqual([1, 3].map(api.newRepliesLabel), ['1 new reply', '3 new replies']);
assert.deepEqual([0, 2].map(api.latestLabel), ['Back to latest', 'Back to latest · 2 new']);

assert.deepEqual(plain(api.groupReactions([{ emoji: '👍', member_id: 'u1', member_name: 'ann' }, { emoji: '🎉', member_id: 'a1' }, { emoji: '👍', member_id: 'me', member_name: 'me' }], 'me')), [
  { emoji: '👍', count: 2, title: 'ann, me', pressed: true }, { emoji: '🎉', count: 1, title: 'a1', pressed: false }]);
assert.deepEqual(plain(api.groupReactions([], 'me')), []);

assert.deepEqual([{ author_kind: 'system' }, { author_kind: 'user', author_id: 'me' }, { author_kind: 'user', author_id: 'x' }, { author_kind: 'agent', author_id: 'me' }].map(message => api.articleClass(message, 'me')), ['message system-notice', 'message self', 'message', 'message']);
assert.deepEqual([{}, { thread_root: 'r' }, { author_kind: 'system' }].map(message => api.liveArrival(message, { initialLoading: false, contextMode: false })), [true, false, false]);
assert.deepEqual([[true, false], [false, true]].map(([initialLoading, contextMode]) => api.liveArrival({}, { initialLoading, contextMode })), [false, false]);

const base = { hasMoreOlder: false, oldestSeq: 0, rootRendered: false, openThreadID: undefined, threadLatestHidden: true, threadEntryKnown: false, contextMode: false, known: false, missed: false };
const route = (message, patch) => api.liveRoute(message, { ...base, ...patch });
assert.equal(route({ seq: 5 }, {}), 'add');
assert.equal(route({ seq: 5 }, { hasMoreOlder: true, oldestSeq: 10 }), 'skip', 'a row the live cap already dropped');
assert.equal(route({ seq: 15 }, { hasMoreOlder: true, oldestSeq: 10 }), 'add');
assert.equal(route({ seq: 10 }, { hasMoreOlder: true, oldestSeq: 10 }), 'add', 'the oldest row itself is not older than the oldest');
assert.equal(route({ seq: 5, thread_root: 'r' }, { hasMoreOlder: true, oldestSeq: 10, rootRendered: true }), 'add', 'the cap does not apply to a reply');
assert.equal(route({ seq: 5, thread_root: 'r' }, {}), 'skip', 'a reply with no row and no open thread');
assert.equal(route({ seq: 5, thread_root: 'r' }, { rootRendered: true }), 'add');
assert.equal(route({ seq: 5, thread_root: 'r' }, { openThreadID: 'r' }), 'add', 'an open thread takes its reply');
assert.equal(route({ id: 'm', thread_root: 'r' }, { openThreadID: 'r', threadLatestHidden: false }), 'new-reply', 'a reply while the panel is not at its end is counted');
assert.equal(route({ id: 'm', thread_root: 'r' }, { openThreadID: 'r', threadLatestHidden: false, threadEntryKnown: true }), 'add');
assert.equal(route({ seq: 5 }, { contextMode: true }), 'missed', 'a history window counts it on the button');
assert.equal(route({ seq: 5 }, { contextMode: true, known: true }), 'skip');
assert.equal(route({ seq: 5 }, { contextMode: true, missed: true }), 'skip');
assert.equal(route({ seq: 5, thread_root: 'r' }, { contextMode: true, rootRendered: true }), 'add', 'a window still shows replies of the rows it has');
assert.equal(route({ seq: 5, thread_root: 'r' }, { contextMode: true, openThreadID: 'r' }), 'add');

assert.deepEqual([{ initialLoading: true }, { newestSeq: 40 }, { newestSeq: 0 }, { contextMode: true, newestSeq: 40, lastRowSeq: () => 12 }, { catchingUp: true, newestSeq: 40, lastRowSeq: () => 0 }].map(state => api.newestShownSeq({ initialLoading: false, contextMode: false, catchingUp: false, newestSeq: 0, lastRowSeq: () => 0, ...state })), [0, 40, 0, 12, 0]);
const never = () => { throw new Error('the list was read'); };
assert.deepEqual([{ initialLoading: true }, { newestSeq: 9 }].map(state => api.newestShownSeq({ initialLoading: false, contextMode: false, catchingUp: false, newestSeq: 0, lastRowSeq: never, ...state })), [0, 9]);
assert.deepEqual([api.trimCap(true, 300, 800), api.trimCap(false, 300, 800)], [300, 800]);
assert.deepEqual([[10, 10], [11, 10], [3, 10]].map(([rows, cap]) => api.overCap(rows, cap)), [0, 1, 0]);

const ask = { kind: 'question', proposed_answers: ['alpha', 'beta'], free_text: false };
assert.deepEqual([{ kind: 'question' }, { kind: 'approval' }, { kind: 'note' }, { kind: 'question', boundary_pending: true }, { kind: 'question', can_decide: false }, null].map(api.answerEligible), [true, true, false, false, false, false]);
assert.deepEqual([['@zed beta', '@zed'], ['beta', '@zed'], ['@zed gamma', '@zed'], ['@other beta', '@zed'], ['@zed beta @other', '@zed'], ['BETA', ''], ['@zed', '']].map(([body, who]) => api.answersThroughInbox(ask, who, body)), [true, true, false, false, false, false, false]);
assert.deepEqual([['anything at all', '@zed'], ['@zed ok', '@zed'], ['@else ok', '@zed']].map(([body, who]) => api.answersThroughInbox({ ...ask, free_text: true }, who, body)), [true, true, false]);
assert.equal(api.answersThroughInbox({ ...ask, can_decide: false }, '@zed', '@zed beta'), false);
assert.equal(api.answersThroughInbox({ kind: 'question', options: ['yes', 'no'], free_text: false }, '', 'yes'), true, 'a plain option of an approval');

assert.deepEqual([[{ kind: 'question' }, null, false], [null, { proposed_answers: ['a'] }, true], [null, { open_questions: [{ question: 'q' }] }, false], [null, { proposed_answers: [] }, true], [null, null, false], [{ kind: 'note' }, {}, true]].map(([item, message, answered]) => api.answerArea(item, message, answered)), ['controls', 'history', 'history', 'done', 'none', 'done']);

const m = (seq, body, extra = {}) => ({ seq, body, author_kind: 'user', author_id: 'me', ...extra });
assert.deepEqual(plain(api.recallCandidates([m(5, 'c'), m(1, 'a'), m(2, 'a'), m(3, 'b'), m(4, 'x', { author_id: 'other' }), m(6, 'r', { thread_root: 'root' }), m(7, 'sys', { author_kind: 'agent' }), m(8, 'a')], 'me')), ['a', 'b', 'c', 'a']);
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
console.log('chat model: ok');
