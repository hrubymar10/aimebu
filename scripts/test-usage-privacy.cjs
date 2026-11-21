// The usages-ports privacy masks (web/js/usage-privacy.js), run in a vm, with
// usagesPorts pinned to B (a stub signal: the same shape the real module has).
// Part of `make test-web`.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const root = path.join(__dirname, '..', 'web', 'js');
const source = fs.readFileSync(path.join(root, 'usage-privacy.js'), 'utf8');
const context = vm.createContext({ document: { addEventListener() {}, dispatchEvent() {} }, localStorage: { getItem: () => 'on', setItem() {}, removeItem() {} } });
// The mask's on/off state comes from maskingOn(), usagesPorts.value === 'b'
// && usagePrivacy.value; the stub pins the experiment to B and the test flips
// the toggle through the same signal the real module keeps.
vm.runInContext(`var usagesPorts = { value: 'b' };
var signal = value => ({ value, peek: () => value });
${source.replace(/^import .*$/gm, '').replace(/^export /gm, '')}
globalThis.api = { maskName, maskEmail, maskNames, setPrivacy: value => { usagePrivacy.value = value; } };`, context);
const { maskName, maskEmail, maskNames, setPrivacy } = context.api;

assert.equal(maskName('main'), 'm•••', 'masking is on in B by default (no saved state reads as off, so the stub forces the check)');
setPrivacy(false); // toggling off leaves the plain text
assert.equal(maskName('main'), 'main', 'masking off leaves the name');
assert.equal(maskName(''), '', 'an empty name has nothing to hide');
assert.equal(maskEmail('main@example.test'), 'main@example.test', 'masking off leaves the email');
setPrivacy(true);
assert.equal(maskEmail('main@example.test'), 'm•••@e•••');

// Overlapping names must be masked in one pass, longest first: "m" inside
// "main" must not leave a readable suffix behind.
const mask = maskNames(['m', 'main', 'backup']);
assert.equal(mask('claude profile main and m and backup: expiring.'),
  `claude profile ${maskName('main')} and ${maskName('m')} and ${maskName('backup')}: expiring.`,
  'every name is masked in one pass, no suffix leaks');
// A name is masked as a whole token only: the email's local part is a token
// ending at the @, while "m" inside "example" is part of a longer word.
assert.equal(mask('main@example.test'), `m•••@example.test`, 'a whole name is masked, a fragment of a longer word is not');

// A one-letter name must not mangle the words around it.
const single = maskNames(['a']);
assert.equal(single('a banana, a-b, a.b and (a): a'), `${maskName('a')} banana, a-b, a.b and (${maskName('a')}): ${maskName('a')}`,
  'only whole-name tokens are masked; letters inside words and dash or dot joined tokens stay');

// A name list with duplicates or empties is deduplicated and filtered.
const sparse = maskNames(['', 'main', 'main']);
assert.equal(sparse('main is main'), `${maskName('main')} is ${maskName('main')}`);
assert.equal(maskNames([]), undefined, 'no names, no mask');
assert.equal(maskNames(null), undefined, 'no list, no mask');

console.log('usage privacy mask tests passed');
