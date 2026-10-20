// The composer's rich-text field, built on ProseMirror (vendored in web/vendor/prosemirror/, loaded the first time the composer
// is shown) whose document is written back, as Markdown, into the composer's textarea. The textarea stays the source of truth
// for the rest of the app: sending reads its value, and anything that sets its value (a sent message clears it, a room change
// resets it) reloads the editor. The shared pickers (@ mentions, < macros, / commands, : emoji) work here through EditorBridge,
// which gives them a textarea-like view of the editor.
import { docToMarkdown } from './composer-markdown.js';

let loading = null;
// The version in the URL is the start of the vendored file's SHA-256 (see web/vendor/prosemirror/README.md).
export const loadProseMirror = () => (loading ||= import('/vendor/prosemirror/prosemirror.js?v=a0b4ee15'));

// Only these addresses may be a link: a pasted javascript: or data: address must not become a clickable link in the editor.
export function safeHref(href) {
  if (typeof href !== 'string' || !/^(https?:\/\/|mailto:)/i.test(href.trim())) return false;
  try { return ['http:', 'https:', 'mailto:'].includes(new URL(href.trim()).protocol); } catch { return false; }
}

export function buildSchema(PM) {
  const block = { group: 'block' };
  return new PM.Schema({
    nodes: {
      doc: { content: 'block+' },
      paragraph: { ...block, content: 'inline*', parseDOM: [{ tag: 'p' }], toDOM: () => ['p', 0] },
      blockquote: { ...block, content: 'block+', defining: true, parseDOM: [{ tag: 'blockquote' }], toDOM: () => ['blockquote', 0] },
      bullet_list: { ...block, content: 'list_item+', parseDOM: [{ tag: 'ul' }], toDOM: () => ['ul', 0] },
      ordered_list: { ...block, content: 'list_item+', attrs: { order: { default: 1 } }, parseDOM: [{ tag: 'ol', getAttrs: dom => ({ order: dom.hasAttribute('start') ? +dom.getAttribute('start') : 1 }) }], toDOM: node => (node.attrs.order === 1 ? ['ol', 0] : ['ol', { start: node.attrs.order }, 0]) },
      list_item: { content: 'paragraph block*', defining: true, parseDOM: [{ tag: 'li' }], toDOM: () => ['li', 0] },
      code_block: { ...block, content: 'text*', marks: '', code: true, defining: true, parseDOM: [{ tag: 'pre', preserveWhitespace: 'full' }], toDOM: () => ['pre', ['code', 0]] },
      hard_break: { inline: true, group: 'inline', selectable: false, parseDOM: [{ tag: 'br' }], toDOM: () => ['br'] },
      text: { group: 'inline' },
    },
    marks: {
      strong: { parseDOM: [{ tag: 'strong' }, { tag: 'b' }], toDOM: () => ['strong', 0] },
      em: { parseDOM: [{ tag: 'em' }, { tag: 'i' }], toDOM: () => ['em', 0] },
      underline: { parseDOM: [{ tag: 'u' }], toDOM: () => ['u', 0] },
      strike: { parseDOM: [{ tag: 's' }, { tag: 'del' }], toDOM: () => ['s', 0] },
      code: { excludes: '_', parseDOM: [{ tag: 'code' }], toDOM: () => ['code', 0] },
      link: { attrs: { href: {} }, inclusive: false, parseDOM: [{ tag: 'a[href]', getAttrs: dom => (safeHref(dom.getAttribute('href')) ? { href: dom.getAttribute('href').trim() } : false) }], toDOM: node => ['a', { href: node.attrs.href, rel: 'noopener noreferrer' }, 0] },
    },
  });
}

// `**x**`-style typing converts to the mark; the marker characters are removed.
function markRule(PM, pattern, markType) {
  return new PM.InputRule(pattern, (state, match, start, end) => {
    const lead = match[1] ?? '', inner = match[2];
    const from = start + lead.length;
    const tr = state.tr.delete(from, end).insertText(inner, from);
    tr.addMark(from, from + inner.length, markType.create());
    tr.removeStoredMark(markType);
    return tr;
  });
}

export class ComposerEditor {
  constructor(PM, host, { onChange, onState, onSend, onLink, placeholder = '' }) {
    this.PM = PM;
    this.onState = onState;
    this.schema = buildSchema(PM);
    this.onChange = onChange;
    const { schema } = this;
    const { keymap, toggleMark, baseKeymap, chainCommands, exitCode } = PM;
    const rules = [
      PM.wrappingInputRule(/^\s*>\s$/, schema.nodes.blockquote),
      PM.wrappingInputRule(/^(\d+)\.\s$/, schema.nodes.ordered_list, match => ({ order: +match[1] }), (match, node) => node.childCount + node.attrs.order === +match[1]),
      PM.wrappingInputRule(/^\s*([-+*])\s$/, schema.nodes.bullet_list),
      PM.textblockTypeInputRule(/^```$/, schema.nodes.code_block),
      markRule(PM, /(^|[\s(])\*\*([^*\s][^*]*)\*\*$/, schema.marks.strong),
      markRule(PM, /(^|[\s(])_([^_\s][^_]*)_$/, schema.marks.em),
      markRule(PM, /(^|[\s(])~~([^~\s][^~]*)~~$/, schema.marks.strike),
      markRule(PM, /(^|[\s(])`([^`\s][^`]*)`$/, schema.marks.code),
    ];
    const inCode = state => state.selection.$from.parent.type === schema.nodes.code_block;
    const inList = state => { const { $from } = state.selection; for (let depth = $from.depth; depth > 0; depth--) if ($from.node(depth).type === schema.nodes.list_item) return true; return false; };
    // The block the caret is in that Backspace or an empty Enter should leave: a list item or a quote (the innermost), or a code block.
    const enclosing = state => {
      const { $from } = state.selection;
      for (let depth = $from.depth; depth > 0; depth--) {
        const type = $from.node(depth).type;
        if (type === schema.nodes.list_item || type === schema.nodes.blockquote) return type;
      }
      return null;
    };
    const leave = (state, dispatch) => {
      const type = enclosing(state);
      if (type === schema.nodes.list_item) return PM.liftListItem(schema.nodes.list_item)(state, dispatch);
      if (type === schema.nodes.blockquote) return PM.lift(state, dispatch);
      return false;
    };
    // Backspace at the start of a block's text lifts it out of its quote or list item, or turns a code block into a paragraph;
    // only when there is nothing to lift does it join with the block before (the default).
    const backspace = (state, dispatch) => {
      const { selection } = state;
      if (!selection.empty) return false;
      // On the empty last line of a code block, Backspace leaves the block just as Enter does there.
      if (enterEndOfCode(state, dispatch)) return true;
      if (selection.$from.parentOffset !== 0) return false;
      if (inCode(state)) return PM.setBlockType(schema.nodes.paragraph)(state, dispatch);
      return leave(state, dispatch);
    };
    // Enter on an empty line inside a quote or list leaves it, as a new paragraph after; it does not send the message.
    const enterEmpty = (state, dispatch) => {
      const { selection } = state;
      if (!selection.empty || inCode(state) || selection.$from.parent.content.size !== 0) return false;
      return leave(state, dispatch);
    };
    // In a code block, Enter or Backspace on the empty last line leaves it: the newline goes, and a paragraph follows the block.
    const enterEndOfCode = (state, dispatch) => {
      const { selection } = state, { $from } = selection;
      if (!selection.empty || !inCode(state) || $from.parentOffset !== $from.parent.content.size || !$from.parent.textContent.endsWith('\n')) return false;
      if (dispatch) {
        const tr = state.tr.delete($from.pos - 1, $from.pos);
        const after = tr.doc.resolve($from.pos - 1).after();
        tr.insert(after, schema.nodes.paragraph.create());
        dispatch(tr.setSelection(PM.TextSelection.create(tr.doc, after + 1)).scrollIntoView());
      }
      return true;
    };
    const keys = {
      'Mod-b': toggleMark(schema.marks.strong), 'Mod-i': toggleMark(schema.marks.em), 'Mod-u': toggleMark(schema.marks.underline),
      'Mod-Shift-x': toggleMark(schema.marks.strike), 'Mod-Shift-c': toggleMark(schema.marks.code),
      'Mod-k': (state) => { onLink?.(); return true; },
      'Mod-z': PM.undo, 'Mod-y': PM.redo, 'Mod-Shift-z': PM.redo,
      'Mod-Shift-7': this.command('ol'), 'Mod-Shift-8': this.command('ul'), 'Mod-Shift-9': this.command('quote'), 'Mod-Alt-Shift-c': this.command('codeblock'),
      // A plain Enter sends (a code block keeps its newlines); Shift+Enter continues a list or quote, or breaks the line.
      Enter: (state, dispatch) => {
        if (enterEmpty(state, dispatch) || enterEndOfCode(state, dispatch)) return true;
        if (inCode(state)) return false;
        onSend?.();
        return true;
      },
      Backspace: backspace,
      'Shift-Enter': chainCommands(
        (state, dispatch) => (inList(state) ? PM.splitListItem(schema.nodes.list_item)(state, dispatch) : false),
        PM.newlineInCode, PM.createParagraphNear, PM.liftEmptyBlock, PM.splitBlock,
      ),
      'Mod-Enter': exitCode,
    };
    this.state = PM.EditorState.create({ schema, plugins: [PM.history(), PM.inputRules({ rules }), keymap(keys), keymap(baseKeymap), this.placeholderPlugin(PM, placeholder)] });
    this.view = new PM.EditorView(host, {
      state: this.state,
      attributes: { role: 'textbox', 'aria-multiline': 'true', 'aria-label': 'Message', spellcheck: 'true' },
      dispatchTransaction: transaction => {
        const next = this.view.state.apply(transaction);
        this.view.updateState(next);
        if (transaction.docChanged) {
          this.onChange?.(this.markdown());
          // The editor handles many edits itself (Backspace, Delete, undo, list keys) and cancels the browser's own input event, so
          // the pickers, which listen for it, would never see the trigger text vanish. Tell them after every change; a later
          // browser input event for the same edit only makes them re-read the same text.
          queueMicrotask(() => this.view?.dom.dispatchEvent(new Event('input', { bubbles: true })));
        }
        this.onState?.();
      },
    });
  }
  placeholderPlugin(PM, text) {
    return new PM.Plugin({
      view: view => {
        const sync = () => { const empty = view.state.doc.childCount === 1 && view.state.doc.firstChild.content.size === 0 && view.state.doc.firstChild.type.name === 'paragraph'; view.dom.classList.toggle('composer-empty', empty); view.dom.dataset.placeholder = text; };
        sync();
        return { update: sync };
      },
    });
  }
  markdown() { return docToMarkdown(this.view.state.doc.toJSON()); }
  // Replace the whole document with plain text (one paragraph per line). Used when something outside sets the draft.
  setText(value) {
    const { schema, PM } = this;
    const lines = String(value).split('\n');
    const paragraphs = lines.map(line => schema.nodes.paragraph.create(null, line ? schema.text(line) : null));
    const doc = schema.nodes.doc.create(null, paragraphs.length ? paragraphs : [schema.nodes.paragraph.create()]);
    // The caret ends up at the end of the text, as a textarea's does after its value is set.
    const state = PM.EditorState.create({ schema, doc, plugins: this.view.state.plugins, selection: PM.TextSelection.atEnd(doc) });
    this.view.updateState(state);
    this.onState?.();
  }
  focus() { this.view.focus(); }
  destroy() { this.view.destroy(); }
  isActive(name) {
    const { state } = this.view, { from, $from, to, empty } = state.selection;
    const mark = this.schema.marks[{ bold: 'strong', italic: 'em' }[name] || name];
    if (mark) return empty ? !!mark.isInSet(state.storedMarks || $from.marks()) : state.doc.rangeHasMark(from, to, mark);
    const nodeName = { ol: 'ordered_list', ul: 'bullet_list', quote: 'blockquote', codeblock: 'code_block' }[name];
    if (!nodeName) return false;
    for (let depth = $from.depth; depth >= 0; depth--) if ($from.node(depth).type.name === nodeName) return true;
    return false;
  }
  // The command for a toolbar button or shortcut, as a ProseMirror command (state, dispatch).
  command(id) {
    const { PM } = this, nodes = this.schema.nodes, marks = this.schema.marks;
    const marked = { bold: marks.strong, italic: marks.em, underline: marks.underline, strike: marks.strike, code: marks.code };
    if (marked[id]) return PM.toggleMark(marked[id]);
    const wrapper = { ol: nodes.ordered_list, ul: nodes.bullet_list };
    if (wrapper[id]) return (state, dispatch) => (this.isActive(id) ? PM.liftListItem(nodes.list_item)(state, dispatch) : PM.wrapInList(wrapper[id])(state, dispatch));
    if (id === 'quote') return (state, dispatch) => (this.isActive('quote') ? PM.lift(state, dispatch) : PM.wrapIn(nodes.blockquote)(state, dispatch));
    if (id === 'codeblock') return (state, dispatch) => (this.isActive('codeblock') ? PM.setBlockType(nodes.paragraph)(state, dispatch) : PM.setBlockType(nodes.code_block)(state, dispatch));
    return () => false;
  }
  run(id) {
    if (!this.view) return false;
    const done = this.command(id)(this.view.state, this.view.dispatch);
    this.view.focus();
    return done;
  }
  // Take the link off the selection, or off the whole link the caret is in.
  removeLink() {
    const { state, dispatch } = this.view, type = this.schema.marks.link;
    let { from, to } = state.selection;
    if (state.selection.empty) {
      const { $from } = state.selection, parent = $from.parent, base = $from.start();
      let offset = 0;
      parent.forEach(child => {
        const end = offset + child.nodeSize;
        if (type.isInSet(child.marks) && $from.parentOffset >= offset && $from.parentOffset <= end) { from = base + offset; to = base + end; }
        offset = end;
      });
    }
    dispatch(state.tr.removeMark(from, to, type));
    this.view.focus();
  }
  // Wrap the selection (or insert the address as its own text) in a link.
  link(href) {
    if (!safeHref(href)) return;
    const { state, dispatch } = this.view, { from, to, empty } = state.selection, mark = this.schema.marks.link.create({ href });
    if (empty) dispatch(state.tr.insertText(href, from).addMark(from, from + href.length, mark));
    else dispatch(state.tr.addMark(from, to, mark));
    this.view.focus();
  }
}

// The editor seen as a textarea: its text is the whole document as plain text (one line per paragraph), offsets are offsets
// in that text, and edits that stay inside one line replace only that range, so formatting elsewhere survives a pick.
export class EditorBridge {
  constructor(editor) { this.editor = editor; }
  get view() { return this.editor.view; }
  get dom() { return this.view.dom; }
  // The text blocks with where their text starts in the plain text and where their content starts in the document.
  blocks() {
    const blocks = []; let offset = 0;
    this.view.state.doc.descendants((node, pos) => {
      if (!node.isTextblock) return true;
      if (blocks.length) offset += 1;
      blocks.push({ text: offset, pos: pos + 1, size: node.content.size });
      offset += node.content.size;
      return false;
    });
    return blocks;
  }
  flush() { this.view.domObserver?.flush?.(); }
  get value() { this.flush(); const { doc } = this.view.state; return doc.textBetween(0, doc.content.size, '\n', '\n'); }
  textToPos(offset) {
    const blocks = this.blocks();
    const block = blocks.find(item => offset >= item.text && offset <= item.text + item.size) || blocks[blocks.length - 1];
    return block.pos + Math.max(0, Math.min(block.size, offset - block.text));
  }
  posToText(pos) {
    const blocks = this.blocks();
    const block = blocks.find(item => pos >= item.pos && pos <= item.pos + item.size) || blocks[blocks.length - 1];
    return block.text + Math.max(0, Math.min(block.size, pos - block.pos));
  }
  get selectionStart() { this.flush(); return this.posToText(this.view.state.selection.from); }
  get selectionEnd() { this.flush(); return this.posToText(this.view.state.selection.to); }
  setSelectionRange(start, end = start) {
    const { PM } = this.editor, { state } = this.view;
    this.view.dispatch(state.tr.setSelection(PM.TextSelection.create(state.doc, this.textToPos(start), this.textToPos(end))));
  }
  replaceRange(start, end, text) {
    const current = this.value;
    const blocks = this.blocks();
    const within = blocks.find(item => start >= item.text && end <= item.text + item.size);
    if (within && !String(text).includes('\n')) {
      const { state } = this.view, from = this.textToPos(start), to = this.textToPos(end);
      const tr = text ? state.tr.replaceWith(from, to, this.editor.schema.text(text)) : state.tr.delete(from, to);
      this.view.dispatch(tr);
      return;
    }
    this.editor.setText(current.slice(0, start) + text + current.slice(end));
    this.editor.onChange?.(this.editor.markdown());
  }
  set value(next) {
    const current = this.value;
    if (next === current) return;
    let from = 0;
    while (from < current.length && from < next.length && current[from] === next[from]) from++;
    let oldEnd = current.length, newEnd = next.length;
    while (oldEnd > from && newEnd > from && current[oldEnd - 1] === next[newEnd - 1]) { oldEnd--; newEnd--; }
    this.replaceRange(from, oldEnd, next.slice(from, newEnd));
  }
}
