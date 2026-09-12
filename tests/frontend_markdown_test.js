#!/usr/bin/env node
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const rendererPath = path.join(__dirname, '..', 'frontend', 'render-markdown.js');
const source = fs.readFileSync(rendererPath, 'utf8');

function htmlEscape(value) {
  return String(value || '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

function htmlUnescape(value) {
  return String(value || '')
    .replace(/&quot;/g, '"')
    .replace(/&gt;/g, '>')
    .replace(/&lt;/g, '<')
    .replace(/&amp;/g, '&');
}

const context = { esc: htmlEscape, unescHtml: htmlUnescape, COPY_CODE_ICON: '' };
vm.createContext(context);
vm.runInContext(source, context);

function render(markdown) {
  return context.renderMarkdown(markdown);
}

assert.strictEqual(render('one\ntwo'), 'one<br>two', 'a single newline renders as one hard break');
assert.strictEqual(render('one\n\ntwo'), 'one<br><br>two', 'a blank line renders as a paragraph gap');
assert.strictEqual(render('one\n\n\ntwo'), 'one<br><br>two', 'longer blank-line runs normalize to one paragraph gap');

const fencedCode = render('```js\none\ntwo\n```');
assert(fencedCode.includes('<code class="lang-js">one\ntwo</code>'), 'fenced code preserves literal newlines');
assert(!fencedCode.includes('one<br>two'), 'fenced code does not gain hard-break markup');

const indentedCode = render('    one\n    two');
assert(indentedCode.includes('<code>    one\n    two</code>'), 'indented code preserves literal newlines');
assert(!indentedCode.includes('one<br>'), 'indented code does not gain hard-break markup');

const aligned = render([
  '| Left | Center | Right |',
  '| :--- | :---: | ---: |',
  '| `a\\|b` | **bold** | [link](https://example.com) #42 |',
].join('\n'));
assert(aligned.includes('md-align-left'), 'left delimiter emits its alignment class');
assert(aligned.includes('md-align-center'), 'center delimiter emits its alignment class');
assert(aligned.includes('md-align-right'), 'right delimiter emits its alignment class');
assert(aligned.includes('<code class="md-code">a|b</code>'), 'table-cell inline code resolves escaped pipes');
assert(!aligned.includes('a\\|b'), 'table-cell inline code does not retain a backslash');
assert(aligned.includes('<strong>bold</strong>'), 'bold formatting survives in a cell');
assert(aligned.includes('href="https://example.com"'), 'links survive in a cell');
assert(aligned.includes('data-msg-id="42"'), 'message references survive in a cell');
assert(!aligned.includes('<br>'), 'table source newlines do not become hard breaks');

const proseCode = render('run `grep \'foo\\|bar\' file.txt` to match either');
assert(proseCode.includes('<code class="md-code">grep \'foo\\|bar\' file.txt</code>'), 'prose inline code retains escaped pipes');

const ragged = render([
  '| A | B |',
  '| --- | --- |',
  '| short |',
  '| long | middle | tail |',
].join('\n'));
assert(ragged.includes('<td class="md-td md-align-left">short</td><td class="md-td md-align-left"></td>'), 'short body rows are padded');
assert(ragged.includes('<td class="md-td md-align-left">middle | tail</td>'), 'extra body cells merge into the final column');

const prose = render('a pipe | in prose\nwithout a table delimiter');
assert(!prose.includes('md-table'), 'a pipe-bearing prose line is not a table');

const list = render('- one\n- two');
assert(/<ul\b/.test(list), 'list-like delimiter candidates still render as a list');

const listShapedTable = render('x | y\n- | -');
assert(listShapedTable.includes('md-table'), 'list-shaped delimiters still form a table after a header row');

for (const delimiter of ['|:::|', '|- -|']) {
  const rejected = render(`| Header |\n${delimiter}`);
  assert(!rejected.includes('md-table'), `${delimiter} is not a valid table delimiter`);
}

console.log('frontend markdown tests passed');
