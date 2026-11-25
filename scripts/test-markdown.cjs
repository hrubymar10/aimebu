// Unit checks for the message Markdown renderer (web/js/markdown.js), run in node over a small DOM stand-in: lists (nested, mixed,
// task items), rules, unterminated fences, and link forms, plus a few things that must not regress.
const assert = require('node:assert/strict');
const path = require('node:path');
const { pathToFileURL } = require('node:url');

class Base {
  constructor() { this.childNodes = []; }
  adopt(node) { if (node instanceof Fragment) { const moved = node.childNodes.splice(0); moved.forEach(child => this.childNodes.push(child)); } else if (typeof node === 'string') this.childNodes.push(new Text(node)); else this.childNodes.push(node); }
  append(...nodes) { nodes.forEach(node => this.adopt(node)); }
  prepend(...nodes) { const before = this.childNodes.splice(0); nodes.forEach(node => this.adopt(node)); this.childNodes.push(...before); }
  replaceChildren(...nodes) { this.childNodes = []; this.append(...nodes); }
  get lastElementChild() { return [...this.childNodes].reverse().find(node => node instanceof Element) || null; }
  get textContent() { return this.childNodes.map(node => node.textContent).join(''); }
}
class Text { constructor(value) { this.value = String(value); } get textContent() { return this.value; } }
class Fragment extends Base {}
class Element extends Base {
  constructor(tag) { super(); this.tagName = tag.toUpperCase(); this.className = ''; this.dataset = {}; this.attrs = {}; this.classList = { add: (...names) => { this.className = [...new Set([...this.className.split(' ').filter(Boolean), ...names])].join(' '); }, contains: name => this.className.split(' ').includes(name) }; }
  setAttribute(name, value) { this.attrs[name] = String(value); }
  getAttribute(name) { return this.attrs[name] ?? null; }
  set textContent(value) { this.childNodes = [new Text(value)]; }
  get textContent() { return this.childNodes.map(node => node.textContent).join(''); }
}
const escape = value => value.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
function serialize(node) {
  if (node instanceof Text) return escape(node.value);
  const tag = node.tagName.toLowerCase();
  const attributes = [];
  if (node.className) attributes.push(`class="${node.className}"`);
  for (const name of ['href', 'title', 'start']) if (node[name] !== undefined && node[name] !== '') attributes.push(`${name}="${node[name]}"`);
  if (node.type) attributes.push(`type=${node.type}`);
  if (node.disabled) attributes.push('disabled'); if (node.checked) attributes.push('checked');
  const open = `<${tag}${attributes.length ? ' ' + attributes.join(' ') : ''}>`;
  return ['hr', 'br', 'input'].includes(tag) ? open : `${open}${node.childNodes.map(serialize).join('')}</${tag}>`;
}
globalThis.document = { createElement: tag => new Element(tag), createTextNode: value => new Text(value), createDocumentFragment: () => new Fragment() };

(async () => {
  const { renderMarkdown } = await import(pathToFileURL(path.join(__dirname, '..', 'web', 'js', 'markdown.js')));
  const html = source => renderMarkdown(source).childNodes.map(serialize).join('');
  const task = checked => `<li class="md-task"><input type=checkbox disabled${checked ? ' checked' : ''}>`;

  // [group a]
  // Lists: flat, numbered with a start, a blank line between items, nested to depth, mixed kinds, and type changes.
  assert.equal(html('- a\n- b'), '<ul><li>a</li><li>b</li></ul>');
  assert.equal(html('5. five\n\n6. six'), '<ol start="5"><li>five</li><li>six</li></ol>');
  assert.equal(html('- one\n\n- two'), '<ul><li>one</li><li>two</li></ul>');
  assert.equal(html('- a\n  - b\n    - c\n- d'), '<ul><li>a<ul><li>b<ul><li>c</li></ul></li></ul></li><li>d</li></ul>');
  assert.equal(html('1. one\n   - sub\n2. two'), '<ol start="1"><li>one<ul><li>sub</li></ul></li><li>two</li></ol>');
  assert.equal(html('- a\n  1. b\n  2. c\n- d'), '<ul><li>a<ol start="1"><li>b</li><li>c</li></ol></li><li>d</li></ul>');
  assert.equal(html('- a\n1. b'), '<ul><li>a</li></ul><ol start="1"><li>b</li></ol>', 'a different marker kind at the same level starts a new list');
  assert.equal(html('  - a\n- b'), '<ul><li>a</li><li>b</li></ul>', 'a list that starts indented and comes back out is one list');
  assert.equal(html('- a\n    - deep\n  - mid\n- z'), '<ul><li>a<ul><li>deep</li></ul><ul><li>mid</li></ul></li><li>z</li></ul>', 'an uneven indent still nests');
  assert.equal(html('+ plus\n* star'), '<ul><li>plus</li><li>star</li></ul>');
  assert.equal(html('- item\ntext after'), '<ul><li>item</li></ul><p>text after</p>');
  // Task items.
  assert.equal(html('- [ ] todo\n- [x] done\n- [X] also'), `<ul>${task(false)}todo</li>${task(true)}done</li>${task(true)}also</li></ul>`);
  assert.equal(html('- [ ] a\n  - [x] b'), `<ul>${task(false)}a<ul>${task(true)}b</li></ul></li></ul>`);
  assert.equal(html('- [ ]'), '<ul><li>[ ]</li></ul>', 'an empty task marker is plain text');
  // Rules, and a rule does not swallow other syntax.
  assert.equal(html('---'), '<hr>'); assert.equal(html('* * *'), '<hr>'); assert.equal(html('___'), '<hr>'); assert.equal(html('text\n\n---\n\nmore'), '<p>text</p><hr><p>more</p>');
  assert.equal(html('text\n---'), '<p>text</p><hr>', 'a rule ends a paragraph');
  assert.equal(html('| a | b |\n| --- | --- |\n| 1 | 2 |').startsWith('<div class="md-table-wrap"><table class="md-table">'), true, 'a table delimiter row is not a rule');
  assert.equal(html('- - -'), '<hr>'); assert.equal(html('--'), '<p>--</p>');

  // [end group a]
  // [group b]
  // Fences close on a run of the same character at least as long as the opener.
  const block = body => `<div class="md-codeblock"><button class="row-btn md-copy-btn" title="Copy code block" type=button><span class="icon-mask icon-mask-copy"></span></button><pre><code>${body}</code></pre></div>`;
  assert.equal(html('```\ncode\n```'), block('code'));
  assert.equal(html('~~~\nin tilde\n~~~'), block('in tilde'));
  assert.equal(html('```\ncode\n````'), block('code'), 'a longer closer closes a triple fence');
  assert.equal(html('````\n```\ninner\n```\n````\nafter'), block('```\ninner\n```') + '<p>after</p>', 'shorter inner fences stay text');
  assert.equal(html('`````\n````\ninner\n`````'), block('````\ninner'), 'five backticks need five to close');
  assert.equal(html('~~~~\n~~~\ninner\n~~~~~'), block('~~~\ninner'), 'tilde fences obey the same length rule');
  assert.equal(html('```js\nlet a;\nmore **bold**'), '<p>```js</p><p>let a;<br>more <strong>bold</strong></p>', 'an unclosed fence leaves following Markdown live');
  assert.equal(html('before\n\n```\nnever closed\n\n- item'), '<p>before</p><p>```</p><p>never closed</p><ul><li>item</li></ul>');
  assert.equal(html('~~~\ncode\n```\nmore\n~~~'), block('code\n```\nmore'), 'a different fence character stays text');
  assert.equal(html('```\ncode\n``` extra\nmore\n```'), block('code\n``` extra\nmore'), 'a closer has only trailing spaces');
  assert.equal(html('````\ninner ```\n````'), block('inner ```'), 'a longer fence closes only on a fence as long');

  // Blocks inside list items stay in the item, so numbering and nesting survive (the agent-style "step, indented fence, next step").
  assert.equal(html('1. Install:\n   ```bash\n   npm i\n   ```\n2. Run it\n3. Done'), `<ol start="1"><li>Install:${block('npm i').replace('<code>', '<code class="language-bash">')}</li><li>Run it</li><li>Done</li></ol>`);
  assert.equal(html('- a\n  > quoted\n- b'), '<ul><li>a<blockquote>quoted</blockquote></li><li>b</li></ul>');
  assert.equal(html('- a\n\n  second para\n\n- b'), '<ul><li>a<p>second para</p></li><li>b</li></ul>');
  assert.equal(html('Steps:\n1. First\n   continues here\n2. Second'), '<p>Steps:</p><ol start="1"><li>First<br>continues here</li><li>Second</li></ol>', 'a continuation line joins its item');
  assert.equal(html('- a\n  - b\n    ```\n    x\n    ```\n  - c'), `<ul><li>a<ul><li>b${block('x')}</li><li>c</li></ul></li></ul>`, 'a fence belongs to the item it is indented under');
  assert.equal(html('1. a\n   ```\n   if x:\n       y\n   ```'), `<ol start="1"><li>a${block('if x:\n    y')}</li></ol>`, 'the fence indent is stripped and deeper indent inside the code is kept');
  assert.equal(html('- a\n  ```js\n  never closed'), '<ul><li>a<p>```js</p><p>never closed</p></li></ul>', 'an unclosed fence inside an item is text');
  assert.equal(html('- a\n  ```js\n  never closed\n- b'), '<ul><li>a<p>```js</p><p>never closed</p></li><li>b</li></ul>', 'an unclosed fence does not swallow the next item');
  assert.equal(html('- a\ntext at column 0'), '<ul><li>a</li></ul><p>text at column 0</p>', 'an unindented line ends the list');
  // Pathological input: nesting is capped and nothing throws.
  const nesting = text => (text.match(/<ul>/g) || []).length;
  const deep = Array.from({ length: 6000 }, (_, i) => ' '.repeat(i) + '- x').join('\n');
  assert.doesNotThrow(() => renderMarkdown(deep));
  assert.ok(nesting(html(deep)) <= 11, 'list nesting is capped at about ten levels');
  assert.ok(html(deep).includes('x'), 'the deepest items are still shown');
  // A render that fails falls back to the text, escaped.
  let calls = 0;
  const hostile = { toString() { if (calls++ === 0) throw new Error('boom'); return 'plain <b>text</b>'; } };
  assert.equal(renderMarkdown(hostile).childNodes.map(serialize).join(''), '<p class="md-unrendered">plain &lt;b&gt;text&lt;/b&gt;</p>');
  // [end group b]
  // [group d]
  // Links.
  assert.equal(html('[a](https://x.test/)'), '<p><a href="https://x.test/">a</a></p>');
  assert.equal(html('[a](https://x.test/a b)'), '<p><a href="https://x.test/a%20b">a</a></p>', 'spaces in the address are encoded');
  assert.equal(html('[a](https://x.test/ "The title")'), '<p><a href="https://x.test/" title="The title">a</a></p>');
  assert.equal(html("[a](https://x.test/ 'single')"), '<p><a href="https://x.test/" title="single">a</a></p>');
  assert.equal(html('<https://x.test/p?q=1>'), '<p><a href="https://x.test/p?q=1">https://x.test/p?q=1</a></p>');
  assert.equal(html('see <mailto:a@b.test> now'), '<p>see <a href="mailto:a@b.test">mailto:a@b.test</a> now</p>');
  assert.equal(html('<javascript:alert(1)>'), '<p>&lt;javascript:alert(1)&gt;</p>', 'only http, https and mailto autolink');
  assert.equal(html('<data:text/html,x>'), '<p>&lt;data:text/html,x&gt;</p>');
  assert.equal(html('<https://bad.test/"x>'), '<p>&lt;https://bad.test/"x&gt;</p>', 'an angle address with a quote in it is not linked');
  assert.equal(html('[x](javascript:alert(1))'), '<p>x)</p>', 'an unsafe address is plain text');
  assert.equal(html('[x](data:text/html,hi "t")'), '<p>x</p>');
  assert.equal(html('[x](  https://x.test/  )'), '<p><a href="https://x.test/">x</a></p>');
  assert.equal(html('<https://x.test/a> and [b](https://y.test/)'), '<p><a href="https://x.test/a">https://x.test/a</a> and <a href="https://y.test/">b</a></p>');
  assert.equal(html('go to https://x.test/a.'), '<p>go to <a href="https://x.test/a">https://x.test/a</a>.</p>', 'trailing punctuation stays outside a bare link');

  // Underscores inside a word never open or close emphasis (CommonMark); asterisks keep the intraword behaviour.
  assert.equal(html('attachment_get, close_room, create_room'), '<p>attachment_get, close_room, create_room</p>');
  assert.equal(html('a_b_c'), '<p>a_b_c</p>');
  assert.equal(html('_em_ and _em_.'), '<p><em>em</em> and <em>em</em>.</p>');
  assert.equal(html('foo_bar_ baz'), '<p>foo_bar_ baz</p>');
  assert.equal(html('__strong__ x'), '<p><strong>strong</strong> x</p>');
  assert.equal(html('snake__case__name'), '<p>snake__case__name</p>');
  assert.equal(html('_a_b_'), '<p><em>a_b</em></p>', 'an underscore inside emphasised text stays literal');
  assert.equal(html('- use snake_case_names here'), '<ul><li>use snake_case_names here</li></ul>');
  assert.equal(html('[snake_case_link](https://x.test/)'), '<p><a href="https://x.test/">snake_case_link</a></p>');
  assert.equal(html('_foo__bar_'), '<p><em>foo__bar</em></p>', 'a closer whose run goes on into another underscore does not close');
  assert.equal(html('__a___b__'), '<p><strong>a___b</strong></p>');
  assert.equal(html('a*b*c'), '<p>a<em>b</em>c</p>');

  // [end group d]
  // What was already better in v2 stays so.
  assert.equal(html('~~gone~~ <u>under</u>'), '<p><del>gone</del> <u>under</u></p>');
  assert.equal(html('#### four'), '<h4>four</h4>');
  assert.equal(html('(#42) and (https://x.test/p)'), '<p>(<a class="msg-ref" href="#42">#42</a>) and (<a href="https://x.test/p">https://x.test/p</a>)</p>');
  assert.equal(html('a\\@b'), '<p>a<span class="md-escaped">@</span>b</p>');
  assert.equal(html('> one\n> two'), '<blockquote>one\ntwo</blockquote>');
  console.log('markdown test passed');
})().catch(error => { console.error(error); process.exit(1); });
