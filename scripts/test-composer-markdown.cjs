// The editor's Markdown serialiser is a pure function over the document JSON, so this runs it in a vm.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'js', 'composer-markdown.js'), 'utf8');
const context = {};
vm.createContext(context);
vm.runInContext(source.replace(/^export /gm, '') + '\nglobalThis.api = { docToMarkdown };', context);
const { docToMarkdown } = context.api;

const text = (value, ...marks) => ({ type: 'text', text: value, marks: marks.map(type => (typeof type === 'string' ? { type } : type)) });
const para = (...content) => ({ type: 'paragraph', content });
const doc = (...content) => ({ type: 'doc', content });
const md = (...blocks) => docToMarkdown(doc(...blocks));

// Marks: each one, nested ones, and the order they open in.
assert.equal(md(para(text('hi', 'strong'))), '**hi**');
assert.equal(md(para(text('hi', 'em'))), '_hi_');
assert.equal(md(para(text('hi', 'underline'))), '<u>hi</u>');
assert.equal(md(para(text('hi', 'strike'))), '~~hi~~');
assert.equal(md(para(text('hi', 'code'))), '`hi`');
assert.equal(md(para(text('docs', { type: 'link', attrs: { href: 'https://example.test/x' } }))), '[docs](https://example.test/x)');
assert.equal(md(para(text('both', 'em', 'strong'))), '**_both_**');
assert.equal(md(para(text('a ', 'strong'), text('b', 'strong', 'em'), text(' c', 'strong'))), '**a _b_ c**');
assert.equal(md(para(text('x'), text('y', 'strong'), text('z'))), 'x**y**z');
// Whitespace stays outside the markers, so the Markdown is still bold.
assert.equal(md(para(text('x'), text(' hi ', 'strong'), text('y'))), 'x **hi** y');
assert.equal(md(para(text('hi ', 'strong'), text('there'))), '**hi** there');
// Code that contains backticks gets a longer fence.
assert.equal(md(para(text('a`b', 'code'))), '``a`b``');
// A hard break is a newline and ends open marks.
assert.equal(md(para(text('one', 'strong'), { type: 'hard_break' }, text('two'))), '**one**\ntwo');

// Blocks.
assert.equal(md(para(text('one')), para(text('two'))), 'one\ntwo', 'paragraphs are lines');
assert.equal(md(para(text('one')), para(), para(text('two'))), 'one\n\ntwo', 'an empty paragraph is a blank line');
assert.equal(md({ type: 'bullet_list', content: [{ type: 'list_item', content: [para(text('a'))] }, { type: 'list_item', content: [para(text('b'))] }] }), '- a\n- b');
assert.equal(md({ type: 'ordered_list', content: [{ type: 'list_item', content: [para(text('a'))] }, { type: 'list_item', content: [para(text('b'))] }] }), '1. a\n2. b');
assert.equal(md({ type: 'ordered_list', attrs: { order: 4 }, content: [{ type: 'list_item', content: [para(text('a'))] }] }), '4. a');
assert.equal(md({ type: 'blockquote', content: [para(text('said'))] }), '> said');
assert.equal(md({ type: 'blockquote', content: [para(text('one')), para(text('two'))] }), '> one\n> two');
assert.equal(md({ type: 'code_block', content: [{ type: 'text', text: 'let a = 1;\nlet b = 2;' }] }), '```\nlet a = 1;\nlet b = 2;\n```');
assert.equal(md(para(text('before')), { type: 'code_block', content: [{ type: 'text', text: 'x' }] }, para(text('after'))), 'before\n\n```\nx\n```\n\nafter');
// A nested list is indented under its item; a list in a quote keeps the quote marker.
assert.equal(md({ type: 'bullet_list', content: [{ type: 'list_item', content: [para(text('a')), { type: 'bullet_list', content: [{ type: 'list_item', content: [para(text('b'))] }] }] }] }), '- a\n  - b');
assert.equal(md({ type: 'blockquote', content: [{ type: 'bullet_list', content: [{ type: 'list_item', content: [para(text('a'))] }] }] }), '> - a');
// An empty document and empty paragraphs are an empty message.
assert.equal(md(para()), '');
assert.equal(docToMarkdown(doc()), '');
assert.equal(md(para(), para(text('x')), para()), 'x');
assert.equal(md(para(text('intro')), { type: 'bullet_list', content: [{ type: 'list_item', content: [para(text('a'))] }] }, para(text('outro'))), 'intro\n\n- a\n\noutro');

// Code is kept byte for byte: blank lines stay, and a fence longer than any backtick run in the code is used.
const code = value => ({ type: 'code_block', content: [{ type: 'text', text: value }] });
assert.equal(md(code('a\n\n\nb')), '```\na\n\n\nb\n```', 'blank lines in code are not collapsed');
assert.equal(md(code('use ``` to fence')), '````\nuse ``` to fence\n````');
assert.equal(md(code('x\n```\ny')), '````\nx\n```\ny\n````', 'a line of backticks cannot end the block');
assert.equal(md(code('``````')), '```````\n``````\n```````');
assert.equal(md(code('  indented\n\ttab ')), '```\n  indented\n\ttab \n```', 'whitespace in code is untouched');
// Outside code only the empty paragraphs at either end are dropped; leading spaces and blank lines in between stay.
assert.equal(md(para(text('  indented')), para(), para(), para(text('after'))), '  indented\n\n\nafter');
assert.equal(md(para(), para(text('x')), para(), para()), 'x');
console.log('composer markdown test passed');
