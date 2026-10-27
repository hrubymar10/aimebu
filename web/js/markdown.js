// A small, DOM-only renderer for the message syntax used by the first web UI.
// Source text is never parsed as HTML or assigned to innerHTML. (The one parse is code-highlight.js turning highlight.js's output
// into DOM; it keeps only text and hljs-* spans.)
import { setGlyph } from './icon-button.js';
import { cells, tableSpec } from './markdown-table.js';
import { fenceLanguage, highlightCode, isDiffLanguage, looksLikeDiff, renderDiff } from './code-highlight.js';
function text(value) { return document.createTextNode(value); }

const URL_TAIL = /[.,;:!?'"*_~]$/;
const urlStart = (source, i) => i === 0 || /[\s(]/.test(source[i - 1]);
const refStart = (source, i) => i === 0 || !/[\w&=/#]/.test(source[i - 1]);
function bareURL(source, i) {
  const match = source.slice(i).match(/^https?:\/\/[^\s<>"]+/i);
  if (!match) return null;
  let value = match[0];
  for (;;) {
    const last = value.slice(-1);
    if (URL_TAIL.test(value)) value = value.slice(0, -1);
    else if (last === ')' && (value.match(/\)/g) || []).length > (value.match(/\(/g) || []).length) value = value.slice(0, -1);
    else if (last === ']' && (value.match(/\]/g) || []).length > (value.match(/\[/g) || []).length) value = value.slice(0, -1);
    else break;
  }
  try { const url = new URL(value); if (['http:', 'https:'].includes(url.protocol) && url.host) return { value, href: url.href }; } catch {}
  return null;
}
function appendInline(parent, source, depth = 0, inLink = false) {
  if (depth > 12) { parent.append(text(source)); return; }
  let i = 0;
  while (i < source.length) {
    if (source[i] === '\\' && i + 1 < source.length) { if (source[i + 1] === '@') { const escaped = document.createElement('span'); escaped.className = 'md-escaped'; escaped.textContent = '@'; parent.append(escaped); } else parent.append(text(source[i + 1])); i += 2; continue; }
    if (source[i] === '`') {
      const run = source.slice(i).match(/^`+/)[0];
      const end = source.indexOf(run, i + run.length);
      if (end >= 0) { const code = document.createElement('code'); code.textContent = source.slice(i + run.length, end); parent.append(code); i = end + run.length; continue; }
    }
    if (source[i] === '[') {
      // [text](address) or [text](address "title"); the address may hold spaces (the URL parser encodes them).
      const match = source.slice(i).match(/^\[([^\]\n]+)\]\(([^)\n]*)\)/);
      if (match) {
        const inside = match[2].trim(), titled = inside.match(/^(\S.*?)\s+(["'])(.*)\2$/);
        const address = titled ? titled[1] : inside, title = titled ? titled[3] : '';
        if (!inLink && /^(https?:\/\/|mailto:)/i.test(address)) {
          try {
            const url = new URL(address);
            if (['http:', 'https:', 'mailto:'].includes(url.protocol)) {
              const link = document.createElement('a'); link.href = url.href; link.target = '_blank'; link.rel = 'noopener noreferrer'; if (title) link.title = title;
              appendInline(link, match[1], depth + 1, true); parent.append(link); i += match[0].length; continue;
            }
          } catch {}
        }
        parent.append(text(match[1])); i += match[0].length; continue;
      }
    }
    if (!inLink && source[i] === '<') {
      // <https://example.test/x> and <mailto:a@b.test> are links to what is inside the brackets.
      const auto = source.slice(i).match(/^<((?:https?:\/\/|mailto:)[^\s<>"]+)>/i);
      if (auto) {
        try {
          const url = new URL(auto[1]);
          if (['http:', 'https:', 'mailto:'].includes(url.protocol) && (url.protocol === 'mailto:' || url.host)) { const link = document.createElement('a'); link.href = url.href; link.target = '_blank'; link.rel = 'noopener noreferrer'; link.textContent = auto[1]; parent.append(link); i += auto[0].length; continue; }
        } catch {}
      }
    }
    if (!inLink && urlStart(source, i) && /^https?:\/\//i.test(source.slice(i, i + 8))) {
      const found = bareURL(source, i);
      if (found) { const link = document.createElement('a'); link.href = found.href; link.target = '_blank'; link.rel = 'noopener noreferrer'; link.textContent = found.value; parent.append(link); i += found.value.length; continue; }
    }
    if (!inLink && source[i] === '#' && refStart(source, i)) {
      const ref = source.slice(i).match(/^#([1-9]\d*)\b/);
      if (ref) { const link = document.createElement('a'); link.className = 'msg-ref'; link.href = `#${ref[1]}`; link.dataset.no = ref[1]; link.textContent = ref[0]; parent.append(link); i += ref[0].length; continue; }
    }
    if (source.startsWith('<u>', i)) {
      const end = source.indexOf('</u>', i + 3);
      if (end > i + 3) { const underline = document.createElement('u'); appendInline(underline, source.slice(i + 3, end), depth + 1, inLink); parent.append(underline); i = end + 4; continue; }
    }
    const marker = ['***', '___', '**', '__', '~~', '*', '_'].find(m => source.startsWith(m, i));
    if (marker) {
      const end = source.indexOf(marker, i + marker.length);
      if (end > i + marker.length) {
        const content = source.slice(i + marker.length, end);
        if (marker.length === 3) { const strong = document.createElement('strong'); const em = document.createElement('em'); appendInline(em, content, depth + 1, inLink); strong.append(em); parent.append(strong); }
        else { const element = document.createElement(marker === '~~' ? 'del' : marker.length === 2 ? 'strong' : 'em'); appendInline(element, content, depth + 1, inLink); parent.append(element); }
        i = end + marker.length; continue;
      }
    }
    let next = i + 1;
    while (next < source.length && !'\\`[*_~#<'.includes(source[next]) && !((source[next] === 'h' || source[next] === 'H') && urlStart(source, next))) next++;
    parent.append(text(source.slice(i, next))); i = next;
  }
}

function fallbackCopyText(value) {
  const area = document.createElement('textarea');
  area.value = value; area.setAttribute('readonly', ''); area.style.cssText = 'position:fixed;top:-9999px;left:-9999px';
  document.body.append(area); area.focus(); area.select();
  try { return document.execCommand('copy'); } finally { area.remove(); }
}
// Clipboard API needs a secure context; fall back to execCommand on plain-HTTP origins.
export async function copyText(value) {
  if (navigator.clipboard?.writeText) {
    try { await navigator.clipboard.writeText(value); return; } catch {}
  }
  if (!fallbackCopyText(value)) throw new Error('clipboard write unavailable');
}
function copyButton(content) {
  const button = document.createElement('button'); button.type = 'button'; button.className = 'row-btn md-copy'; setGlyph(button, 'copy', 'Copy code block');
  button.onclick = async () => { try { await copyText(content); setGlyph(button, 'check', 'Copied'); } catch { setGlyph(button, 'close', 'Copy failed'); } setTimeout(() => setGlyph(button, 'copy', 'Copy code block'), 1500); };
  return button;
}
const indented = line => /^(\t| {4})/.test(line) && line.trim() !== '';
function codeBlock(content, info = '') {
  const wrapper = document.createElement('div'); wrapper.className = 'md-codeblock';
  const button = copyButton(content);
  const pre = document.createElement('pre'), code = document.createElement('code'); code.textContent = content; pre.append(code); wrapper.append(button, pre);
  // A fence's language word picks highlighting; diffs (labelled, or an unlabelled unified diff) are rendered here, the rest load highlight.js on demand.
  const language = fenceLanguage(info);
  if (language) code.className = `language-${language}`;
  if (isDiffLanguage(language) || (!language && looksLikeDiff(content))) renderDiff(code, content);
  else if (language) highlightCode(code, content, language);
  return wrapper;
}
// A horizontal rule: three or more of -, * or _ (spaces allowed between), alone on the line.
const RULE = /^ {0,3}([-*_])(?:\s*\1){2,}\s*$/;
const LIST_ITEM = /^(\s*)(?:([-*+])|(\d+)\.)\s+(.*)$/;
const listItem = line => LIST_ITEM.test(line) && !RULE.test(line);
const indentOf = whitespace => whitespace.replace(/\t/g, '    ').length;
// Lists nest at most this deep; a deeper item is shown at the deepest level instead of building DOM without bound.
const MAX_LIST_DEPTH = 10;
// Blocks nested inside blocks (a quote or a list item holding blocks) are read to this depth; below it the text is shown as it is.
const MAX_BLOCK_DEPTH = 8;
const leadingWidth = line => indentOf(line.match(/^\s*/)[0]);
// Reads a list starting at lines[start]: every following item line (a single blank line between items is allowed), with deeper
// indentation nesting a list in the item above, and a change of marker type at the same level starting a new list. Items that
// begin with [ ] or [x] are task items with a disabled checkbox. Lines indented under an item that are not themselves items
// (a continuation, a fenced code block, a quote, a second paragraph) belong to that item and are rendered inside it.
function readList(lines, start, depth) {
  const items = []; let i = start;
  while (i < lines.length) {
    if (listItem(lines[i])) {
      const m = lines[i].match(LIST_ITEM);
      const item = { indent: indentOf(m[1]), ordered: !!m[3], number: Number(m[3]), body: m[4], extra: [] };
      items.push(item); i++;
      // Lines under the item: indented deeper than its marker and not another item. Blank lines count only when more of the item follows.
      let fence = null;
      while (i < lines.length) {
        const line = lines[i];
        if (!line.trim()) {
          let next = i + 1; while (next < lines.length && !lines[next].trim()) next++;
          if (fence || (next < lines.length && !listItem(lines[next]) && leadingWidth(lines[next]) > item.indent)) { item.extra.push(''); i++; continue; }
          break;
        }
        if (!fence && (listItem(line) || leadingWidth(line) <= item.indent)) break;
        if (fence) { if (line.trim().startsWith(fence)) fence = null; }
        else { const open = line.match(/^\s*(`{3,}|~{3,})([^`]*)$/); if (open) fence = open[1]; }
        item.extra.push(line); i++;
      }
      // A fence that never closed inside the item is just text there; trailing blank lines are not part of the item.
      while (item.extra.length && !item.extra[item.extra.length - 1].trim()) item.extra.pop();
      continue;
    }
    if (!lines[i].trim() && i + 1 < lines.length && listItem(lines[i + 1])) { i++; continue; }
    break;
  }
  // A list that begins indented and then comes back out is one list: the first item takes the smallest indent of the run.
  items[0].indent = Math.min(...items.map(item => item.indent));
  let k = 0;
  const build = level => {
    const base = items[k].indent, ordered = items[k].ordered;
    const element = document.createElement(ordered ? 'ol' : 'ul');
    if (ordered) element.start = items[k].number;
    while (k < items.length && items[k].indent >= base) {
      if (items[k].indent === base && items[k].ordered !== ordered) break;
      if (items[k].indent > base && level < MAX_LIST_DEPTH) { // deeper than this list's own items: it nests in the item above
        const parent = element.lastElementChild;
        if (parent) parent.append(build(level + 1)); else { element.append(document.createElement('li')); element.lastElementChild.append(build(level + 1)); }
        continue;
      }
      const item = items[k];
      const li = document.createElement('li'); const task = item.body.match(/^\[([ xX])\]\s+(.*)$/);
      if (task) {
        li.className = 'md-task'; const box = document.createElement('input'); box.type = 'checkbox'; box.disabled = true; box.checked = task[1] !== ' ';
        box.setAttribute('aria-label', task[1] === ' ' ? 'Not done' : 'Done'); li.append(box); appendInline(li, task[2]);
      } else appendInline(li, item.body);
      if (item.extra.length) {
        // Strip the indentation the lines share, then read them as blocks of their own.
        const width = Math.min(...item.extra.filter(line => line.trim()).map(leadingWidth));
        const dedented = item.extra.map(line => line.trim() ? line.replace(/\t/g, '    ').slice(width) : '');
        const content = renderBlocks(dedented.join('\n'), depth + 1);
        // A short continuation line reads as part of the item's own line, not as a new paragraph under it.
        if (content.childNodes.length === 1 && content.childNodes[0].tagName === 'P' && dedented.every(line => line)) dedented.forEach(line => { li.append(document.createElement('br')); appendInline(li, line); });
        else li.append(content);
      }
      element.append(li); k++;
    }
    return element;
  };
  const elements = [];
  while (k < items.length) elements.push(build(0));
  return { elements, next: i };
}
// A message is rendered whole or not at all: if reading it fails (a pathological nesting, a bug), the text is shown as it was
// written, escaped, so one bad message never takes the list down with it.
export function renderMarkdown(source) {
  try { return renderBlocks(source, 0); } catch {
    const fragment = document.createDocumentFragment(), p = document.createElement('p'); p.className = 'md-unrendered'; try { p.textContent = String(source || ''); } catch { p.textContent = ''; } fragment.append(p); return fragment;
  }
}
function renderBlocks(source, depth) {
  const fragment = document.createDocumentFragment();
  if (depth > MAX_BLOCK_DEPTH) { const p = document.createElement('p'); p.textContent = String(source || ''); fragment.append(p); return fragment; }
  const lines = String(source || '').replace(/\r\n?/g, '\n').split('\n');
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (!line.trim()) { i++; continue; }
    const fence = line.match(/^\s*(`{3,}|~{3,})([^`]*)$/);
    if (fence) {
      let close = i + 1;
      while (close < lines.length && !lines[close].trim().startsWith(fence[1])) close++;
      if (close < lines.length) { fragment.append(codeBlock(lines.slice(i + 1, close).join('\n'), fence[2])); i = close + 1; continue; }
      // A fence that never closes is not a code block: the opening line is shown as it was typed and the rest is read as Markdown.
      const literal = document.createElement('p'); literal.append(text(line)); fragment.append(literal); i++; continue;
    }
    if (indented(line) && (i === 0 || !lines[i - 1].trim())) {
      const content = [];
      while (i < lines.length && indented(lines[i])) content.push(lines[i++].replace(/^(\t| {4})/, ''));
      fragment.append(codeBlock(content.join('\n'))); continue;
    }
    const heading = line.match(/^(#{1,6})\s+(.+)$/);
    if (heading) { const element = document.createElement(`h${heading[1].length}`); appendInline(element, heading[2]); fragment.append(element); i++; continue; }
    if (/^>\s?/.test(line)) {
      const quote = document.createElement('blockquote'); const body = [];
      while (i < lines.length && /^>\s?/.test(lines[i])) body.push(lines[i++].replace(/^>\s?/, ''));
      appendInline(quote, body.join('\n')); fragment.append(quote); continue;
    }
    const spec = tableSpec(line, lines[i + 1]);
    if (spec) {
      const wrap = document.createElement('div'), table = document.createElement('table'), thead = document.createElement('thead'), tbody = document.createElement('tbody'); wrap.className = 'md-table-wrap';
      const row = (values, tag) => { const tr = document.createElement('tr'); spec.aligns.forEach((align, index) => { const cell = document.createElement(tag); cell.className = `md-${tag} md-align-${align}`; appendInline(cell, values[index] || ''); tr.append(cell); }); return tr; };
      thead.append(row(spec.headings, 'th')); i += 2;
      while (i < lines.length && lines[i].trim() && lines[i].includes('|')) {
        const values = cells(lines[i]);
        if (values.length > spec.headings.length) values.splice(spec.headings.length - 1, values.length, values.slice(spec.headings.length - 1).join(' | '));
        tbody.append(row(values, 'td')); i++;
      }
      table.append(thead, tbody); wrap.append(table); fragment.append(wrap); continue;
    }
    if (RULE.test(line)) { fragment.append(document.createElement('hr')); i++; continue; }
    if (listItem(line)) { const read = readList(lines, i, depth); fragment.append(...read.elements); i = read.next; continue; }
    const paragraph = [];
    while (i < lines.length && lines[i].trim() && (paragraph.length === 0 || (!/^(#{1,6}\s|>\s?|\s*(?:[-*+]|\d+\.)\s+|\s*`{3,}|\s*~{3,})/.test(lines[i]) && !RULE.test(lines[i]) && !tableSpec(lines[i], lines[i + 1])))) paragraph.push(lines[i++]);
    const p = document.createElement('p'); paragraph.forEach((part, n) => { if (n) p.append(document.createElement('br')); appendInline(p, part); }); fragment.append(p);
  }
  return fragment;
}

// Raw view: the source text verbatim, with a copy button on fenced and indented code blocks.
export function renderRaw(source) {
  const fragment = document.createDocumentFragment();
  const lines = String(source || '').replace(/\r\n?/g, '\n').split('\n');
  let plain = [], i = 0;
  const flush = () => { if (!plain.length) return; const span = document.createElement('span'); span.className = 'raw-text'; span.textContent = plain.join('\n'); fragment.append(span); plain = []; };
  while (i < lines.length) {
    const fence = lines[i].match(/^\s*(`{3,}|~{3,})([^`]*)$/);
    if (fence) {
      const block = [lines[i++]], body = [];
      while (i < lines.length && !lines[i].trim().startsWith(fence[1])) { body.push(lines[i]); block.push(lines[i++]); }
      if (i < lines.length) block.push(lines[i++]);
      flush(); fragment.append(rawBlock(block.join('\n'), body.join('\n'))); continue;
    }
    if (indented(lines[i]) && (i === 0 || !lines[i - 1].trim())) {
      const block = [];
      while (i < lines.length && indented(lines[i])) block.push(lines[i++]);
      flush(); fragment.append(rawBlock(block.join('\n'), block.map(line => line.replace(/^(\t| {4})/, '')).join('\n'))); continue;
    }
    plain.push(lines[i++]);
  }
  flush();
  return fragment;
}
function rawBlock(shown, copied) {
  const wrapper = document.createElement('div'); wrapper.className = 'md-codeblock raw-codeblock';
  const pre = document.createElement('pre'), code = document.createElement('code'); code.textContent = shown; pre.append(code);
  wrapper.append(copyButton(copied), pre); return wrapper;
}
