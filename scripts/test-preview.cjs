// The room list's one-line preview: Markdown taken out, code as [code], a diff as [diff], an image as [image].
'use strict';
const assert = require('node:assert/strict');
const path = require('node:path');
const { pathToFileURL } = require('node:url');
(async () => {
  const { previewText } = await import(pathToFileURL(path.join(__dirname, '..', 'web', 'js', 'preview.js')).href);
  const cases = [
    ['plain text stays', 'hello   there\nworld', 'hello there world'],
    ['empty', '', ''],
    ['a fenced block', 'Look:\n```go\nfunc main() {}\n```\ndone', 'Look: [code] done'],
    ['a tilde fence', '~~~\nx := 1\n~~~', '[code]'],
    ['a labelled diff', '```diff\n--- a/web/theme-v1.css\n+++ b/web/theme-v1.css\n@@ -1 +1 @@\n-a\n+b\n```', '[diff]'],
    ['a patch fence', '```patch\n@@ -1 +1 @@\n-a\n+b\n```', '[diff]'],
    ['an unlabelled unified diff', '```\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n-a\n+b\n```', '[diff]'],
    ['an unclosed fence', 'text\n```go\nfunc x() {}', 'text [code]'],
    ['an indented block', 'Run:\n\n    make test\n    make vet\n\nthen stop', 'Run: [code] then stop'],
    ['an image', 'see ![a chart](https://x.test/c.png) here', 'see [image] here'],
    ['a link', 'open [the docs](https://x.test/a b) now', 'open the docs now'],
    ['a link with a title', '[x](https://x.test "tip")', 'x'],
    ['headings and quotes', '## Plan\n> a quote\n- one\n* two\n1. three\n- [x] done', 'Plan a quote one two three done'],
    ['inline marks', '**bold** and _it_ and ~~gone~~ and `code` and <u>under</u>', 'bold and it and gone and code and under'],
    ['a snake_case name is not italic', 'call snake_case_name now', 'call snake_case_name now'],
    ['a mention stays', 'ping @wren please', 'ping @wren please'],
  ];
  for (const [name, input, expected] of cases) assert.equal(previewText(input), expected, name);
  assert.ok(!/```|^#|\*\*/.test(previewText('# T\n```js\nx\n```\n**b**')), 'no raw Markdown is left');
  console.log('preview test passed');
})().catch(error => { console.error(error); process.exit(1); });
