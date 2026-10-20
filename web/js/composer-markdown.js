// Serialises the editor's document (ProseMirror's node.toJSON() shape) to the Markdown the chat renders. Pure and free of the
// editor library, so it is unit-tested on its own. There is no escaping of Markdown characters in plain text: the renderer
// has no escapes either, so what is typed is what is sent, as in the plain textarea.

const MARK_ORDER = ['strong', 'em', 'underline', 'strike', 'code', 'link'];
const markKey = mark => mark.type + (mark.attrs?.href ? `:${mark.attrs.href}` : '');
const open = mark => ({ strong: '**', em: '_', underline: '<u>', strike: '~~', code: '`', link: '[' })[mark.type] || '';
const close = mark => ({ strong: '**', em: '_', underline: '</u>', strike: '~~', code: mark.run || '`', link: `](${mark.attrs?.href || ''})` })[mark.type] || '';

function inline(nodes = []) {
  let out = '';
  let active = [];
  const closeFrom = index => {
    if (index >= active.length) return;
    const trailing = out.match(/\s+$/)?.[0] || '';
    out = out.slice(0, out.length - trailing.length);
    for (const mark of active.slice(index).reverse()) out += close(mark);
    out += trailing;
    active = active.slice(0, index);
  };
  for (const node of nodes) {
    if (node.type === 'hard_break') { closeFrom(0); out += '\n'; continue; }
    if (node.type !== 'text') continue;
    const marks = [...(node.marks || [])].sort((a, b) => MARK_ORDER.indexOf(a.type) - MARK_ORDER.indexOf(b.type));
    const keys = new Set(marks.map(markKey));
    let keep = 0;
    while (keep < active.length && keys.has(markKey(active[keep]))) keep++;
    closeFrom(keep);
    let text = node.text;
    const fresh = marks.filter(mark => !active.some(existing => markKey(existing) === markKey(mark)));
    if (fresh.length) { const lead = text.match(/^\s+/)?.[0] || ''; out += lead; text = text.slice(lead.length); }
    for (const mark of fresh) {
      if (mark.type === 'code') { const run = '`'.repeat(1 + Math.max(0, ...(text.match(/`+/g) || []).map(ticks => ticks.length))); out += run; active.push({ ...mark, run }); }
      else { out += open(mark); active.push(mark); }
    }
    out += text;
  }
  closeFrom(0);
  return out;
}

function block(node) {
  switch (node.type) {
    case 'paragraph': return inline(node.content);
    case 'blockquote': return blocks(node.content).split('\n').map(line => (line ? `> ${line}` : '>')).join('\n');
    case 'code_block': {
      // The fence is longer than any run of backticks in the code, so the code can never end its own block.
      const code = (node.content || []).map(child => child.text || '').join('');
      const fence = '`'.repeat(Math.max(3, 1 + Math.max(0, ...(code.match(/`+/g) || []).map(run => run.length))));
      return `${fence}\n${code}\n${fence}`;
    }
    case 'bullet_list': return (node.content || []).map(item => listItem(item, '- ')).join('\n');
    case 'ordered_list': return (node.content || []).map((item, index) => listItem(item, `${(node.attrs?.order || 1) + index}. `)).join('\n');
    default: return blocks(node.content);
  }
}

function listItem(item, marker) {
  const body = blocks(item.content, '\n');
  const pad = ' '.repeat(marker.length);
  return body.split('\n').map((line, index) => (index === 0 ? marker + line : (line ? pad + line : line))).join('\n');
}

// Blocks one after another: paragraphs are lines (Enter makes a new paragraph, so a message typed in the editor reads like the
// same lines typed in a textarea), anything else is set off by a blank line. `lines` forces a single newline.
function blocks(nodes = [], lines = '') {
  let out = '';
  nodes.forEach((child, index) => {
    if (index) out += lines || (nodes[index - 1].type === 'paragraph' && child.type === 'paragraph' ? '\n' : '\n\n');
    out += block(child);
  });
  return out;
}

// Empty paragraphs at either end are not part of the message; everything else, code blocks above all, is kept byte for byte.
export function docToMarkdown(doc) {
  const content = [...(doc.content || [])];
  const empty = node => node.type === 'paragraph' && !(node.content || []).length;
  while (content.length && empty(content[0])) content.shift();
  while (content.length && empty(content[content.length - 1])) content.pop();
  return blocks(content);
}
