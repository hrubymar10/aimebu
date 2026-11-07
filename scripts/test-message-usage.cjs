// The wording of a message's usage popover and tooltip is a browser module with no DOM or network use, so this test runs it in a vm:
// numbers, cost, unknown figures, the turn's place among its messages, and a partial turn.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'message-usage-format.js'), 'utf8');
const context = { Intl };
vm.createContext(context);
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { DASH, formatTokens, formatCost, usageRows, usageNotes, usageTooltip, totalsRows, totalsNotes, groupLine };', context);
const { DASH, formatTokens, formatCost, usageTooltip } = context.api;
// What the vm returns is of its own realm; a round trip through JSON makes it comparable.
const plain = value => JSON.parse(JSON.stringify(value));
const usageRows = value => plain(context.api.usageRows(value)), usageNotes = value => plain(context.api.usageNotes(value));

// Tokens: thousands separators; anything that is not a count is unknown, and zero is a count.
assert.equal(formatTokens(1234567), '1,234,567');
assert.equal(formatTokens(0), '0');
for (const bad of [undefined, null, NaN, -1, 'x']) assert.equal(formatTokens(bad), DASH, String(bad));

// Cost: two decimals, four under a cent, unknown is a dash and zero is $0.00.
assert.equal(formatCost(1.5), '$1.50');
assert.equal(formatCost(0.0042), '$0.0042');
assert.equal(formatCost(0), '$0.00');
assert.equal(formatCost(0.01), '$0.01');
for (const bad of [undefined, null, NaN, -0.5]) assert.equal(formatCost(bad), DASH, String(bad));

// The rows name every figure, and a figure nobody reported is a dash, never 0.
const full = { model: 'claude-haiku-4-5', tokens_in: 12, tokens_out: 3, tokens_cache_read: 90, tokens_cache_write: 4, cost: 0.004, index: 1, count: 1 };
assert.deepEqual(usageRows(full), [['Model', 'claude-haiku-4-5'], ['Input', '12'], ['Output', '3'], ['Cache read', '90'], ['Cache written', '4'], ['Cost (estimate)', '$0.0040']]);
assert.deepEqual(usageRows({ tokens_in: 5 }), [['Model', DASH], ['Input', '5'], ['Output', DASH], ['Cache read', DASH], ['Cache written', DASH], ['Cost (estimate)', DASH]]);
assert.deepEqual(usageRows(undefined).map(row => row[1]), [DASH, DASH, DASH, DASH, DASH, DASH]);
assert.deepEqual(usageRows({ cost: 0.003, cost_source: 'estimated' }).at(-1), ['Cost (price estimate)', '$0.0030']);

// Notes: a one-message turn says so; a several-message turn says "n of N" and that the figures are the turn's, not added per message.
assert.deepEqual(usageNotes(full), ['These are the figures of the turn that wrote this message.']);
const several = usageNotes({ index: 2, count: 3 });
assert.equal(several.length, 1);
assert.match(several[0], /wrote 3 messages; this is 2 of 3/);
assert.match(several[0], /not added up per message/);
const partial = usageNotes({ index: 1, count: 1, partial: true });
assert.equal(partial.length, 2);
assert.match(partial[1], /floor/);

// The tooltip: one line, only what is known.
// "in" is all the input the model read, with the split the popover shows: the real case was a turn of 2 new tokens that read tens of thousands from the cache.
assert.equal(usageTooltip(full), 'Usage: 106 in (12 new, 90 cache read, 4 cache written) · 3 out · $0.0040');
assert.equal(usageTooltip({ tokens_in: 2, tokens_out: 233, tokens_cache_read: 41250, tokens_cache_write: 0, cost: 0.16 }), 'Usage: 41,252 in (2 new, 41,250 cache read) · 233 out · $0.16');
assert.equal(usageTooltip({ tokens_in: 12, tokens_cache_read: 0, tokens_cache_write: 0, tokens_out: 3 }), 'Usage: 12 in · 3 out', 'nothing cached: just the input');
assert.equal(usageTooltip({ tokens_cache_read: 500, tokens_out: 3 }), 'Usage: 500 in (500 cache read) · 3 out', 'only the cached figures were reported');
assert.equal(usageTooltip({ tokens_in: 12, tokens_cache_read: 90 }), 'Usage: 102 in (12 new, 90 cache read)');
assert.equal(usageTooltip({ tokens_in: 12, tokens_out: 3, cost: 0.004 }).includes('new'), false, 'a harness that reports no cache figures gets the plain number');
assert.match(usageTooltip({ cost: 0.003, cost_source: 'estimated' }), /estimated/);
assert.match(usageNotes({ cost_source: 'estimated' }).at(-1), /dated model price list/);
assert.equal(usageTooltip({ count: 3, tokens_in: 12 }), 'Usage: 12 in (turn of 3 messages)');
assert.equal(usageTooltip({ partial: true, tokens_in: 7 }), 'Usage: 7 in (partial)');
assert.equal(usageTooltip({}), 'Usage');

// The totals of a room or project.
const totals = { turns: 3, turns_with_cost: 2, cost_coverage: 'partial', tokens_in: 2200, tokens_out: 40, tokens_cache_read: 9000, cost: 1.5, partial: true };
assert.deepEqual(plain(context.api.totalsRows(totals)), [['Turns', '3'], ['Input', '2,200'], ['Output', '40'], ['Cache read', '9,000'], ['Cache written', DASH], ['Cost (estimate)', '$1.50']]);
const notes = plain(context.api.totalsNotes(totals));
assert.equal(notes.length, 2);
assert.match(notes[0], /known for 2 of 3 turns.*floor/);
assert.match(notes[1], /ended before they reported/);
assert.deepEqual(plain(context.api.totalsNotes({ turns: 0 })), ['Nothing was spent in this range.']);
assert.deepEqual(plain(context.api.totalsNotes({ turns: 2, turns_with_cost: 0, cost_coverage: 'unknown' })), ['No turn in this range reported a cost.']);
assert.deepEqual(plain(context.api.totalsNotes({ turns: 2, turns_with_cost: 2, cost_coverage: 'known' })), []);
assert.deepEqual(plain(context.api.totalsNotes({ turns: 2, turns_with_cost: 2, turns_with_estimated_cost: 1, cost_coverage: 'estimated' })), ['1 turn uses the dated model price list; that cost is estimated.']);
assert.deepEqual(plain(context.api.totalsRows({})).map(row => row[1]), ['0', DASH, DASH, DASH, DASH, DASH]);
assert.equal(context.api.groupLine({ turns: 1, tokens_in: 10, tokens_out: 2, cost: 0.5 }), '1 turn \u00b7 10 in / 2 out \u00b7 $0.50');
assert.equal(context.api.groupLine({ turns: 2 }), `2 turns \u00b7 ${DASH}`);
assert.match(context.api.groupLine({ turns: 1, cost: 0.01, cost_estimated: true }), /estimated$/);
console.log('message usage test passed');
