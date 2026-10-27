// The Notifications page's pure rules (web/js/pages/notifications/model.js), run in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'pages', 'notifications', 'model.js'), 'utf8');
const context = vm.createContext({});
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { DEFAULT_SOUND, volumeText, volumeFill, soundLabel, soundSize, uploadedSounds, chosenSound, soundPath, permissionView };', context);
const m = context.api;
const plain = value => JSON.parse(JSON.stringify(value));

const chime = { id: 'builtin:chime', kind: 'builtin', name: 'Chime', size: 0 }, ping = { id: 'user:abc 1', kind: 'user', name: 'ping.wav', size: 1536 }, tiny = { id: 'user:t', kind: 'user', name: 'tiny.wav', size: 10 };
assert.deepEqual([m.volumeText(70), m.volumeFill(70), m.volumeFill(0), m.volumeFill(100)], ['70%', '0.7', '0', '1']);
assert.deepEqual([m.soundLabel(chime), m.soundLabel(ping)], ['Chime', 'ping.wav (uploaded)']);
assert.deepEqual([m.soundSize(ping), m.soundSize(tiny)], ['ping.wav · 2 KB ', 'tiny.wav · 1 KB '], 'a size is rounded to whole KB and never below 1');
assert.deepEqual(plain(m.uploadedSounds([chime, ping])), [ping]);
// A choice that is no longer on offer falls back to the chime.
assert.equal(m.chosenSound([chime, ping], 'user:abc 1'), 'user:abc 1');
assert.equal(m.chosenSound([chime], 'user:abc 1'), m.DEFAULT_SOUND);
assert.equal(m.chosenSound([], ''), m.DEFAULT_SOUND);
assert.equal(m.soundPath(ping), '/api/sounds/abc%201', 'the path names the part after "user:", encoded');
// The permission button: pressable only while the browser has not been asked.
assert.deepEqual(plain(m.permissionView(true, 'default')), { disabled: false, label: 'Enable browser notifications' });
assert.deepEqual(plain(m.permissionView(true, 'granted')), { disabled: true, label: 'Enabled' });
assert.deepEqual(plain(m.permissionView(true, 'denied')), { disabled: true, label: 'Enable browser notifications' });
assert.deepEqual(plain(m.permissionView(false, 'granted')), { disabled: true, label: 'Enable browser notifications' }, 'without the API nothing is enabled');
console.log('notifications model tests passed');
