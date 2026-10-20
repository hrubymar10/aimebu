// The emoji data module is pure (no DOM, no imports), so this loads the real file: skin tones, search ranking, the code-span guard.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { pathToFileURL } = require('node:url');

(async () => {
  const web = path.join(__dirname, '..', 'web');
  const data = await import(pathToFileURL(path.join(web, 'js', 'emoji-data.js')).href);
  const list = JSON.parse(fs.readFileSync(path.join(web, 'vendor', 'gemoji', 'emoji.json'), 'utf8'));
  const db = data.buildDatabase(list);
  const find = glyph => { const entry = db.entries.find(item => item.emoji === glyph); assert.ok(entry, `${glyph} is in the database`); return entry; };
  const [, light, , medium] = data.SKIN_TONES.map(tone => tone.key);

  // Skin tones: one modifier per person, none on anything else, no variation selector left behind a toned base.
  assert.equal(data.withTone(find('👍'), medium), '👍🏽');
  assert.equal(data.withTone(find('👍'), ''), '👍');
  assert.equal(data.withTone(find('😀'), medium), '😀', 'an emoji without tones is left alone');
  assert.equal(data.withTone(find('💏'), medium), '💏🏽');
  assert.equal(data.withTone(find('🧑‍🤝‍🧑'), medium), '🧑🏽‍🤝‍🧑🏽', 'both people of a joined pair are toned');
  assert.equal(data.withTone(find('👩‍❤️‍💋‍👨'), light), '👩🏻‍❤️‍💋‍👨🏻', 'a four-part ZWJ sequence tones both people, not the heart');
  assert.equal(data.withTone(find('👩‍❤️‍👨'), medium), '👩🏽‍❤️‍👨🏽');
  assert.equal(data.withTone(find('⛹️‍♂️'), medium), '⛹🏽‍♂️', 'the variation selector after a toned base is dropped');
  assert.equal(data.withTone(find('🏃‍♀️'), light), '🏃🏻‍♀️');
  const modifier = /[\u{1F3FB}-\u{1F3FF}]/gu, people = /\p{Emoji_Modifier_Base}(?!\u200D)|(?<!\u200D)\p{Emoji_Modifier_Base}/gu;
  let toned = 0;
  for (const entry of db.entries) {
    if (!entry.skin_tones) { assert.equal(data.withTone(entry, medium), entry.emoji); continue; }
    toned++;
    const result = data.withTone(entry, medium), bases = entry.emoji.match(people)?.length || 0;
    assert.ok(bases > 0, `${entry.emoji} offers tones but has nobody to tone`);
    assert.equal(result.match(modifier)?.length, bases, `${entry.emoji} -> ${result}: one modifier per person`);
    assert.equal(result.replace(modifier, '').replace(/️/g, ''), entry.emoji.replace(/️/g, ''), `${entry.emoji}: toning only adds modifiers`);
  }
  assert.ok(toned > 250, `${toned} emoji take a skin tone`);

  // Search: an alias that starts with the query first, then names, then tags; exact alias wins.
  assert.equal(data.searchEmoji(db, 'shrug')[0].emoji, '🤷');
  assert.equal(data.searchEmoji(db, '+1')[0].emoji, '👍');
  assert.equal(data.searchEmoji(db, 'tada', 3)[0].emoji, '🎉');
  assert.ok(data.searchEmoji(db, 'happy').length >= 3, 'tags match');
  assert.ok(data.searchEmoji(db, 'grinning face').some(entry => entry.emoji === '😀'), 'names match');
  assert.deepEqual(data.searchEmoji(db, '  '), []);
  assert.equal(data.searchEmoji(db, 'zzzzzz').length, 0);
  assert.equal(db.byAlias.get('shrug').emoji, '🤷');
  assert.equal(data.matchedAlias(find('👍'), 'thumbsu'), 'thumbsup');

  // Every database entry lands in a picker category.
  const groups = new Set(data.CATEGORIES.map(category => category.key));
  assert.ok(db.entries.every(entry => groups.has(entry.group)));

  // ":" is left alone inside code spans and fenced blocks.
  assert.equal(data.insideCode('a `b :sh', 8), true);
  assert.equal(data.insideCode('a `b` :sh', 9), false);
  assert.equal(data.insideCode('```\n:sh', 7), true);
  assert.equal(data.insideCode('```\ncode\n```\n:sh', 16), false);
  assert.equal(data.insideCode('x\n`a`\n:sh', 9), false);
  console.log('emoji data test passed');
})().catch(error => { console.error(error); process.exit(1); });
