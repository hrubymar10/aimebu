// Which pasted text the rich composer converts from Markdown, and which pasted HTML it leaves to the editor: pure functions, run in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

// The two modules run as one script: the table reader the renderer uses, then the paste decisions that use it.
const read = name => fs.readFileSync(path.join(__dirname, '..', 'web', 'js', name), 'utf8').replace(/^import .*$/gm, '').replace(/^export /gm, '');
const context = {};
vm.createContext(context);
vm.runInContext(read('markdown-table.js') + '\n' + read('composer-paste.js') + '\nglobalThis.api = { looksLikeMarkdown, carriesFormatting };', context);
const { looksLikeMarkdown, carriesFormatting } = context.api;

// Markdown the editor has a place for is converted.
for (const text of [
  'this is **bold** text',
  '**bold** at the start',
  'some __bold__ too',
  'a ~~struck~~ word',
  'run `make test` now',
  'an _italic_ word, and more',
  'an *italic* word.',
  'see [the docs](https://example.test/docs)',
  '- one\n- two',
  '* one\n* two',
  '1. first\n2. second',
  '> quoted',
  '```js\nconst x = 1;\n```',
  'intro\n\n- a\n- b\n\nand `code`',
]) assert.equal(looksLikeMarkdown(text), true, `markdown: ${JSON.stringify(text)}`);

// Plain prose stays plain, whatever punctuation it has.
for (const text of [
  '', '   ', 'just words', 'snake_case_name and another_one', '2 * 3 * 4 = 24', 'a * b', 'wait... what? yes!', 'path/to/file.txt',
  'price is 5$ or 6$', 'a_b_c', 'mail me: me@example.test', 'one - two', 'a - b - c', 'a > b', 'Step 1.5 of the plan', 'unclosed ** bold', 'only one `backtick', null, undefined, 42,
]) assert.equal(looksLikeMarkdown(text), false, `plain: ${JSON.stringify(text)}`);

// A paste with structure the document cannot hold stays exactly as written, even when it also has marks the editor knows.
for (const text of [
  '# A heading\n\nwith **bold**',
  '| a | b |\n|---|---|\n| **1** | 2 |',
  '**x**\n\n---\n\ny',
  'an image ![alt](https://example.test/x.png) and **bold**',
  '- [ ] todo\n- [x] done',
  // A table without outer pipes, with alignment colons, with an escaped pipe or code in a cell: the renderer reads all of them as tables.
  'name | status\n--- | ---\n**alice** | ready',
  'name | status\n:--- | ---:\n**alice** | ready',
  '| a | b |\n| --- | --- |\n| `x\\|y` | **z** |',
  'intro with **bold**\n\na | b\n- | -\n1 | 2',
]) assert.equal(looksLikeMarkdown(text), false, `unsupported: ${JSON.stringify(text)}`);
// A pipe alone is not a table: a line with a pipe needs a rule line of as many cells under it.
for (const text of ['use `a | b` and **bold**', 'a | b\n**bold** here', 'a | b | c\n--- | ---\n**x**', '- one | two\n- three | four'])
  assert.equal(looksLikeMarkdown(text), true, `not a table: ${JSON.stringify(text)}`);

// A very large paste is not converted: it is not read line by line to decide.
assert.equal(looksLikeMarkdown('**a** '.repeat(60000)), false);

// HTML from a rendered message or a web page has structure the editor reads; a code editor's copy has none.
for (const html of ['<p>a <strong>b</strong></p>', '<b>x</b>', '<ul><li>a</li></ul>', '<pre><code>x</code></pre>', '<a href="https://e.test">x</a>', '<blockquote>q</blockquote>', '<code>x</code>', '<em>x</em>'])
  assert.equal(carriesFormatting(html), true, html);
for (const html of ['<meta charset="utf-8"><div style="font-family: Menlo"><span style="color:#fff">**bold**</span></div>', '<span>- one</span><br><span>- two</span>', '<div>plain</div>', '', null, undefined, '<abbr>x</abbr>', '<body>use <bold-ish>x</bold-ish></body>'])
  assert.equal(carriesFormatting(html), false, String(html));

console.log('composer paste ok');
