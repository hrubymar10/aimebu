// The Inbox never shows raw JSON. humanize() is a pure function in a browser module, so this runs it in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'inbox', 'model.js'), 'utf8');
const head = source.replace(/^export /gm, '');
const context = {};
vm.createContext(context);
vm.runInContext(head + '\nglobalThis.api = { humanize, readablePreview, deliveryEvidence };', context);
const { humanize, readablePreview, deliveryEvidence } = context.api;

// Plain text passes through untouched.
assert.equal(humanize('An agent reply could not be delivered.'), 'An agent reply could not be delivered.');
assert.equal(humanize(undefined), '');
assert.equal(humanize(null), '');
// A draft is shown as its text.
assert.equal(humanize('{"messages":[{"room":"r","body":"The summary is ready."}]}'), 'The summary is ready.');
assert.equal(humanize('[{"body":"one"},{"text":"two"}]'), 'one\n\ntwo');
// An error object becomes a few words, never braces.
assert.equal(humanize('{"error":"validation_failed","code":3}'), 'error: validation_failed · code: 3');
assert.equal(humanize('{"exit":17,"signal":null,"reason":"checkout missing"}'), 'exit: 17 · reason: checkout missing');
// The server cuts long drafts off with an ellipsis, which is invalid JSON; the text is still recovered.
const cut = '{"messages":[{"room":"room_01","body":"@una final verdict on HELLO2.md: PASS. Checked again just now: the file is at the project path, its content is exactly `hello again` an…';
assert.equal(humanize(cut), '@una final verdict on HELLO2.md: PASS. Checked again just now: the file is at the project path, its content is exactly `hello again` an…');
assert.equal(readablePreview(cut), humanize(cut));
assert.equal(humanize('{"messages":[{"body":"line\\nbreak and \\"quotes\\""}]}'), 'line\nbreak and "quotes"');
assert.equal(humanize('{"body":"half \\"quoted\\" and cut'), 'half "quoted" and cut');
// Unreadable JSON says so instead of showing it.
assert.match(humanize('{"oops'), /incomplete/);
assert.match(humanize('{}'), /No readable details/);
for (const input of [cut, '{"error":"x"}', '{"oops', '[1,2,3]']) assert.ok(!/[{}"]/.test(humanize(input)), `raw JSON characters left in ${input}`);
// Why a reply was held: a sentence for the reasons we know (real data had "room closed room is closing or closed").
assert.equal(deliveryEvidence({ held_reason: 'room_closed', delivery_error: 'room is closing or closed' }), 'The room was archived before this reply could be posted.');
assert.match(deliveryEvidence({ held_reason: 'correction_unavailable', delivery_error: 'invalid_answer: json: unknown field "type"' }), /did not pass validation/);
assert.equal(deliveryEvidence({ held_reason: 'something_new', delivery_error: 'it broke' }), 'something new it broke');
assert.equal(deliveryEvidence({}), 'The reply was held; view the room for context.');
assert.ok(!/[{}]/.test(deliveryEvidence({ held_reason: 'x', delivery_error: '{"a":1}' })));
console.log('humanize test passed');
