// The in-app dialog's field rules (web/js/pages/app-dialog/model.js), run in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'app-dialog', 'model.js'), 'utf8');
assert.ok(!/^import /m.test(source), 'the model imports nothing');
const context = vm.createContext({});
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { readValues, startValues, fieldStates, firstProblem, accepted, declined, focusTarget };', context);
const m = context.api;
const plain = value => JSON.parse(JSON.stringify(value));

// readValues: a checkbox is a boolean, text is trimmed, keyed by name
const fields = [{ name: 'name', label: 'Name' }, { name: 'remote', label: 'Remote', type: 'checkbox' }, { name: 'url', label: 'URL', showIf: values => values.remote }];
const inputs = [{ value: '  box  ' }, { checked: 1 }, { value: 'ssh://x ' }];
assert.deepEqual(plain(m.readValues(fields, index => inputs[index])), { name: 'box', remote: true, url: 'ssh://x' });
assert.deepEqual(plain(m.readValues([], () => null)), {});

// startValues: what readValues gives for untouched inputs
assert.deepEqual(plain(m.startValues([{ name: 'a', value: ' x ' }, { name: 'b' }, { name: 'c', type: 'checkbox', value: 'yes' }, { name: 'd', type: 'checkbox' }])), { a: 'x', b: '', c: true, d: false });

// fieldStates: showIf hides, disabledIf greys out, a reset button follows its field
assert.deepEqual(plain(m.fieldStates(fields, { name: 'box', remote: false, url: '' })), [
  { hidden: false, disabled: false, reset: null }, { hidden: false, disabled: false, reset: null }, { hidden: true, disabled: false, reset: null }]);
assert.equal(m.fieldStates(fields, { remote: true })[2].hidden, false, 'shown once what it depends on is ticked');
const reset = [{ name: 'base', label: 'Base', reset: { label: 'Use the default', value: 'main' }, disabledIf: values => values.lock }, { name: 'lock', type: 'checkbox' }];
assert.deepEqual(plain(m.fieldStates(reset, { base: 'dev', lock: false })[0]), { hidden: false, disabled: false, reset: { hidden: false, disabled: false } });
assert.deepEqual(plain(m.fieldStates(reset, { base: 'main', lock: false })[0].reset), { hidden: false, disabled: true }, 'off while the field holds the reset value');
assert.deepEqual(plain(m.fieldStates(reset, { base: 'dev', lock: true })[0]), { hidden: false, disabled: true, reset: { hidden: false, disabled: true } }, 'off with its field');
assert.equal(m.fieldStates([{ name: 'base', reset: { label: 'x', value: '' } }], { base: 'a' })[0].reset.hidden, true, 'hidden with nothing to put back');
assert.equal(m.fieldStates([{ name: 'a', disabledIf: () => 'yes' }], {})[0].disabled, true, 'a truthy disabledIf counts');

// firstProblem: required first (in field order), then validators; hidden fields are skipped
const form = [
  { name: 'a', label: 'Alpha', validate: value => (value === 'bad' ? 'Alpha is bad.' : '') },
  { name: 'b', label: 'Beta', required: true },
  { name: 'c', label: 'Gamma', required: true, requiredMessage: 'Say something.', showIf: values => values.a === 'show' },
];
assert.deepEqual(plain(m.firstProblem(form, { a: 'bad', b: '', c: '' })), { index: 1, message: 'Beta is required.' }, 'a missing field before a validator');
assert.deepEqual(plain(m.firstProblem(form, { a: 'bad', b: 'x', c: '' })), { index: 0, message: 'Alpha is bad.' });
assert.equal(m.firstProblem(form, { a: 'ok', b: 'x', c: '' }), null, 'a hidden required field is not checked');
assert.deepEqual(plain(m.firstProblem(form, { a: 'show', b: 'x', c: '' })), { index: 2, message: 'Say something.' }, 'the field\'s own message');
const seen = [];
m.firstProblem([{ name: 'n', validate: (value, values) => { seen.push([value, values.other]); return ''; } }, { name: 'other' }], { n: 'v', other: 'o' });
assert.deepEqual(seen, [['v', 'o']], 'a validator gets its value and all the values');
assert.equal(m.firstProblem([{ name: 'h', validate: () => 'never', showIf: () => false }], { h: '' }), null, 'a hidden field is not validated');
assert.equal(m.firstProblem([{ name: 'r', label: 'R', required: true, type: 'checkbox' }], { r: false })?.message, 'R is required.', 'an unticked required box');

// answers and focus
assert.equal(m.accepted([], {}), true);
assert.deepEqual(plain(m.accepted([{ name: 'a' }], { a: 'x' })), { a: 'x' });
assert.equal(m.declined([]), false);
assert.equal(m.declined(undefined), false);
assert.equal(m.declined([{ name: 'a' }]), null);
assert.equal(m.focusTarget({ danger: true, fields: [{}] }), 'cancel', 'a destructive question starts on Cancel');
assert.equal(m.focusTarget({ fields: [{}] }), 'field', 'a form on its first field');
assert.equal(m.focusTarget({}), 'ok', 'anything else on the confirm button');

console.log('test-app-dialog-model: ok');
