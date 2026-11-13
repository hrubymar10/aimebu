import { renderMarkdown } from './markdown.js';
import { renderDiff } from './code-highlight.js';

const kinds = new Set(['markdown', 'file-tree', 'data-model', 'api-endpoint', 'annotated-code', 'diff', 'checklist', 'question-form', 'diagram', 'canvas', 'prototype']);
const element = (tag, className, value) => {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (value !== undefined) node.textContent = String(value);
  return node;
};
const object = value => value && typeof value === 'object' && !Array.isArray(value);
const display = value => typeof value === 'string' ? value : JSON.stringify(value, null, 2) ?? String(value);
const code = (value, className = 'visual-plan-code-block') => {
  const pre = element('pre', className);
  pre.append(element('code', '', display(value)));
  return pre;
};
const fallback = value => code(value, 'visual-plan-raw-fallback');
const content = (parent, value) => parent.append(renderMarkdown(String(value)));
const dataOf = block => {
  if (!object(block)) return null;
  if (object(block.data)) return block.data;
  // Data given as a bare string is text for the block (markdown, diff, diagram); a string that is a JSON object is that object.
  if (typeof block.data === 'string') {
    try { const parsed = JSON.parse(block.data); if (object(parsed)) return parsed; } catch {}
    return { text: block.data };
  }
  return null;
};
const meaningful = value => value !== undefined && value !== null && (typeof value !== 'string' || value.trim() !== '') && (!Array.isArray(value) || value.length > 0);
const usable = (data, ...keys) => keys.some(key => meaningful(data[key]));
const textField = (data, key) => data[key] === undefined || typeof data[key] === 'string';
const position = data => ['x', 'y', 'w', 'h'].every(key => data[key] === undefined || Number.isFinite(Number(data[key])));
function treeEntry(entry, depth = 0) {
  if (typeof entry === 'string') return true;
  if (!object(entry) || depth > 30 || !['name', 'path', 'type', 'note', 'status', 'description'].every(key => textField(entry, key))) return false;
  return entry.children === undefined || Array.isArray(entry.children) && entry.children.every(child => treeEntry(child, depth + 1));
}
function wellFormedData(type, data) {
  switch (type) {
    case 'markdown': return typeof (data.markdown ?? data.text) === 'string';
    case 'diff': return typeof (data.diff ?? data.text) === 'string';
    case 'diagram': return typeof (data.mermaid ?? data.source ?? data.text) === 'string';
    case 'file-tree': return data.root !== undefined ? treeEntry(data.root) :
      ['files', 'children'].some(key => Array.isArray(data[key]) && data[key].every(entry => treeEntry(entry)));
    case 'data-model': {
      const entities = data.entities ?? data.tables;
      return Array.isArray(entities) && entities.every(entity => object(entity) && ['name', 'table'].every(key => textField(entity, key)) &&
        (entity.fields === undefined || Array.isArray(entity.fields) && entity.fields.every(field => typeof field === 'string' ||
          object(field) && ['name', 'type', 'notes', 'description'].every(key => textField(field, key)))));
    }
    case 'api-endpoint': return ['method', 'path', 'url', 'notes', 'description'].every(key => textField(data, key));
    case 'annotated-code': return ['code', 'source'].every(key => textField(data, key)) &&
      (data.annotations === undefined || Array.isArray(data.annotations) && data.annotations.every(note => typeof note === 'string' ||
        object(note) && ['text', 'note'].every(key => textField(note, key))));
    case 'checklist': return Array.isArray(data.items) && data.items.every(item => typeof item === 'string' ||
      object(item) && ['text', 'label'].every(key => textField(item, key)) && (item.checked === undefined || typeof item.checked === 'boolean'));
    case 'question-form': return Array.isArray(data.questions) && data.questions.every(question => object(question) &&
      typeof question.question === 'string' && textField(question, 'description') &&
      (question.options === undefined || Array.isArray(question.options) && question.options.every(option => typeof option === 'string')));
    case 'canvas': return Array.isArray(data.nodes) && data.nodes.every(node => object(node) && position(node) &&
      ['label', 'text', 'id'].every(key => textField(node, key)));
    case 'prototype': return Array.isArray(data.screens) && data.screens.every(screen => object(screen) &&
      ['id', 'title'].every(key => textField(screen, key)) &&
      (screen.elements === undefined || Array.isArray(screen.elements) && screen.elements.every(item => object(item) && position(item) &&
        ['id', 'text', 'label', 'target', 'type'].every(key => textField(item, key)))));
  }
  return false;
}
const table = rows => {
  const table = element('table', 'visual-plan-block-table');
  const body = element('tbody');
  rows.forEach(row => { const tr = element('tr'); row.forEach((cell, index) => tr.append(element(index === 0 ? 'th' : 'td', '', display(cell)))); body.append(tr); });
  table.append(body); return table;
};
const percent = (value, fallback) => Number.isFinite(Number(value)) ? `${Math.max(0, Math.min(100, Number(value)))}%` : `${fallback}%`;
const bounds = (node, spec) => {
  node.style.left = percent(spec.x, 0); node.style.top = percent(spec.y, 0);
  node.style.width = percent(spec.w, 20); node.style.height = percent(spec.h, 10);
};

let mermaidLoader;
let mermaidQueue = Promise.resolve();
function loadMermaid() {
  if (window.mermaid) return Promise.resolve(window.mermaid);
  if (!mermaidLoader) mermaidLoader = new Promise((resolve, reject) => {
    const script = document.createElement('script');
    script.src = '/vendor/mermaid.min.js';
    script.async = true;
    script.onload = () => window.mermaid ? resolve(window.mermaid) : reject(new Error('Mermaid did not initialize'));
    script.onerror = () => reject(new Error('Mermaid could not load'));
    document.head.append(script);
  }).catch(error => { mermaidLoader = null; throw error; });
  return mermaidLoader;
}
function restoreMermaidSource(node, source, error) {
  node.classList.remove('mermaid');
  node.classList.add('visual-plan-mermaid-source');
  node.replaceChildren();
  node.append(element('span', 'visual-plan-render-error', `Diagram failed to render: ${error.message || error}`));
  node.append(element('code', '', source));
}
// A diagram is a picture: a `click NODE href "…"` line makes mermaid draw the node inside a live <a>, a same-tab link to any address the author
// wrote (a phishing link under a harmless label), which the strict level allows. Every link is taken out and its drawing kept.
function stripDiagramLinks(node) {
  for (const link of [...node.querySelectorAll('a')]) link.replaceWith(...link.childNodes);
}
export function queueMermaid(node, source) {
  requestAnimationFrame(() => {
    if (!node.isConnected) return;
    mermaidQueue = mermaidQueue.catch(() => {}).then(async () => {
      const mermaid = await loadMermaid();
      if (!node.isConnected) return;
      const theme = /(?:^|-)dark$/.test(document.documentElement.dataset.theme || '') ? 'dark' : 'default';
      // Labels are drawn as SVG text, never as HTML (a label's markup is not trusted, however a diagram asks), and a diagram's own %%{init}%% block may not
      // turn that back on: `secure` lists the keys it cannot change, and htmlLabels is one of them.
      mermaid.initialize({ startOnLoad: false, securityLevel: 'strict', theme, htmlLabels: false, flowchart: { htmlLabels: false },
        secure: ['secure', 'securityLevel', 'startOnLoad', 'maxTextSize', 'suppressErrorRendering', 'maxEdges', 'htmlLabels', 'flowchart', 'dompurifyConfig'] });
      // A first render sometimes ends in mermaid's own error graphic; one retry from the original source clears that.
      for (let attempt = 0; ; attempt++) {
        node.classList.add('mermaid');
        node.classList.remove('visual-plan-mermaid-source');
        node.removeAttribute('data-processed');
        node.textContent = source;
        await mermaid.run({ nodes: [node] }).catch(error => { if (attempt) throw error; });
        stripDiagramLinks(node);
        if (node.querySelector('svg') && !node.querySelector('.error-icon, .error-text')) break;
        if (attempt) throw new Error('invalid diagram');
      }
    }).catch(error => { if (node.isConnected) restoreMermaidSource(node, source, error); });
  });
}
function diagram(data) {
  const source = data.mermaid || data.source || data.text;
  if (typeof source !== 'string' || !source.trim()) return null;
  const node = code(source, 'visual-plan-code-block visual-plan-mermaid-source');
  queueMermaid(node, source);
  return node;
}

function fileTree(data) {
  const roots = Array.isArray(data) ? data : data.root ? [data.root] : Array.isArray(data.files) ? data.files : Array.isArray(data.children) ? data.children : [];
  if (!roots.length) return null;
  const list = element('ul', 'visual-plan-file-tree');
  const append = (parent, entry, depth) => {
    if (depth > 30) return;
    const node = typeof entry === 'string' ? { name: entry } : entry;
    if (!object(node)) return;
    const li = element('li'); const label = element('span', `visual-plan-file-node ${node.type === 'dir' || node.type === 'directory' || (Array.isArray(node.children) && node.children.length) ? 'dir' : 'file'}`);
    label.append(element('span', 'visual-plan-file-name', node.name || node.path || ''));
    if (node.note || node.status || node.description) label.append(element('span', 'visual-plan-file-note', node.note || node.status || node.description));
    li.append(label);
    if (Array.isArray(node.children) && node.children.length) { const children = element('ul'); node.children.forEach(child => append(children, child, depth + 1)); li.append(children); }
    parent.append(li);
  };
  roots.forEach(root => append(list, root, 0));
  return list.childNodes.length ? list : null;
}

function model(data) {
  const entities = data.entities || data.tables;
  if (!Array.isArray(entities) || !entities.length) return null;
  const wrap = element('div');
  for (const entity of entities) {
    if (!object(entity)) return null;
    const section = element('div', 'visual-plan-model-entity');
    section.append(element('div', 'visual-plan-model-name', entity.name || entity.table || 'Entity'));
    const rows = Array.isArray(entity.fields) ? entity.fields.map(field => typeof field === 'string' ? [field, '', ''] : [field?.name || '', field?.type || '', field?.notes || field?.description || '']) : [];
    section.append(table(rows)); wrap.append(section);
  }
  return wrap;
}

function endpoint(data) {
  const rows = [['Method', data.method], ['Path', data.path || data.url], ['Request', data.request || data.request_body], ['Response', data.response || data.response_body], ['Notes', data.notes || data.description]].filter(row => meaningful(row[1]));
  return rows.length ? table(rows) : null;
}

function annotatedCode(data) {
  if (!usable(data, 'code', 'source', 'annotations')) return null;
  const wrap = element('div'); wrap.append(code(data.code || data.source || ''));
  if (Array.isArray(data.annotations) && data.annotations.length) {
    const list = element('ol', 'visual-plan-annotations');
    data.annotations.forEach(note => { const item = element('li'); if (object(note)) { if (note.line) item.append(element('span', '', `L${note.line} `)); item.append(document.createTextNode(String(note.text || note.note || ''))); } else item.textContent = String(note); list.append(item); });
    wrap.append(list);
  }
  return wrap;
}

function checklist(data) {
  if (!Array.isArray(data.items) || !data.items.length) return null;
  const wrap = element('div', 'visual-plan-checklist');
  for (const entry of data.items) {
    const item = object(entry) ? entry : { text: entry };
    const row = element('div', 'visual-plan-check-row'), input = element('input'), label = element('span', item.checked ? 'checked' : '', item.text || item.label || '');
    input.type = 'checkbox'; input.disabled = true; input.setAttribute('aria-label', item.text || item.label || 'Checklist item'); input.checked = Boolean(item.checked); row.append(input, label); wrap.append(row);
  }
  return wrap;
}

function questionForm(data) {
  if (!Array.isArray(data.questions) || !data.questions.length) return null;
  const wrap = element('div', 'visual-plan-question-list');
  data.questions.forEach((question, index) => {
    if (!object(question)) return;
    const section = element('div', 'visual-plan-question'); section.append(element('div', 'visual-plan-question-title', `Q${index + 1}. ${question.question || ''}`));
    if (question.description) { const desc = element('div', 'visual-plan-question-desc'); content(desc, question.description); section.append(desc); }
    const options = element('div', 'visual-plan-question-options');
    if (Array.isArray(question.options)) question.options.forEach(option => options.append(element('span', '', option)));
    section.append(options); wrap.append(section);
  });
  return wrap.childNodes.length ? wrap : null;
}

function canvas(data) {
  if (!Array.isArray(data.nodes) || !data.nodes.length) return null;
  const wrap = element('div', 'visual-plan-canvas');
  data.nodes.forEach(spec => { if (!object(spec)) return; const node = element('div', 'visual-plan-canvas-node', spec.label || spec.text || spec.id || ''); bounds(node, spec); wrap.append(node); });
  return wrap.childNodes.length ? wrap : null;
}

function prototype(data) {
  if (!Array.isArray(data.screens) || !data.screens.length) return null;
  const wrap = element('div', 'visual-plan-prototype');
  const tabs = element('div', 'visual-plan-prototype-tabs');
  const panels = [];
  const show = index => panels.forEach((panel, n) => { panel.hidden = n !== index; tabs.children[n].setAttribute('aria-pressed', String(n === index)); });
  data.screens.forEach((screen, index) => {
    if (!object(screen)) return;
    const tab = element('button', 'row-btn visual-plan-prototype-tab', screen.title || screen.id || `Screen ${index + 1}`);
    tab.type = 'button'; tab.onclick = () => show(panels.indexOf(section)); tabs.append(tab);
    const section = element('section', 'visual-plan-prototype-screen'); section.dataset.screenId = String(screen.id || `screen-${index}`); section.append(element('strong', '', screen.title || screen.id || `Screen ${index + 1}`));
    const stage = element('div', 'visual-plan-canvas');
    if (Array.isArray(screen.elements)) screen.elements.forEach(spec => {
      if (!object(spec)) return;
      const target = data.screens.some(candidate => object(candidate) && candidate.id === spec.target);
      const node = element(target ? 'button' : 'div', 'visual-plan-canvas-node', spec.text || spec.label || spec.id || '');
      if (target || spec.type === 'button') node.classList.add('visual-plan-prototype-button');
      else if (spec.type === 'input') node.classList.add('visual-plan-prototype-input');
      if (target) { node.type = 'button'; node.onclick = () => { const next = panels.findIndex(panel => panel.dataset.screenId === String(spec.target)); if (next >= 0) show(next); }; }
      bounds(node, spec); stage.append(node);
    });
    section.append(stage); panels.push(section); wrap.append(section);
  });
  if (!panels.length) return null;
  wrap.prepend(tabs); show(0);
  return wrap;
}

function blockBody(block) {
  const data = dataOf(block);
  if (!data || !kinds.has(block.type) || !wellFormedData(block.type, data)) return fallback(block);
  let rendered = null;
  switch (block.type) {
    case 'markdown': if (usable(data, 'markdown', 'text')) { rendered = element('div'); content(rendered, data.markdown || data.text); } break;
    case 'file-tree': rendered = fileTree(data); break;
    case 'data-model': rendered = model(data); break;
    case 'api-endpoint': rendered = endpoint(data); break;
    case 'annotated-code': rendered = annotatedCode(data); break;
    case 'diff': if (usable(data, 'diff', 'text')) { const text = String(data.diff || data.text); rendered = code(text, 'visual-plan-code-block visual-plan-diff'); renderDiff(rendered.querySelector('code'), text); } break;
    case 'checklist': rendered = checklist(data); break;
    case 'question-form': rendered = questionForm(data); break;
    case 'diagram': rendered = diagram(data); break;
    case 'canvas': rendered = canvas(data); break;
    case 'prototype': rendered = prototype(data); break;
  }
  return rendered || fallback(block);
}

function renderBlock(block) {
  const section = element('section', 'visual-plan-block');
  const header = element('div', 'visual-plan-block-header');
  header.append(element('span', 'visual-plan-block-type', object(block) ? block.type || 'block' : 'block'));
  if (object(block) && block.title) header.append(element('span', 'visual-plan-block-title', block.title));
  const body = element('div', 'visual-plan-block-body');
  try { body.append(blockBody(block)); } catch { body.append(fallback(block)); }
  section.append(header, body); return section;
}

export const VISUAL_PLAN_ROOT = { tag: 'div', class: 'visual-plan' };

export function hasVisualPlan(message) {
  const blocks = message.visual_plan, pages = message.appendix_pages;
  return !((blocks == null || (Array.isArray(blocks) && !blocks.length)) && (pages == null || (Array.isArray(pages) && !pages.length)));
}

export function renderVisualPlan(message) {
  if (!hasVisualPlan(message)) return null;
  const wrap = element(VISUAL_PLAN_ROOT.tag, VISUAL_PLAN_ROOT.class);
  fillVisualPlan(wrap, message);
  return wrap;
}

export function fillVisualPlan(wrap, message) {
  const blocks = message.visual_plan;
  const pages = message.appendix_pages;
  wrap.replaceChildren();
  if (Array.isArray(blocks)) {
    [...blocks].sort((a, b) => (Number(a?.order) || 0) - (Number(b?.order) || 0)).forEach(block => wrap.append(renderBlock(block)));
  } else if (blocks != null) wrap.append(fallback(blocks));
  if (Array.isArray(pages) && pages.length) {
    const details = element('details', 'visual-plan-block visual-plan-appendix');
    details.append(element('summary', '', `Full plan (${pages.length} page${pages.length === 1 ? '' : 's'})`));
    const body = element('div', 'visual-plan-block-body');
    pages.forEach((page, index) => {
      const section = element('section', 'visual-plan-appendix-page');
      section.append(element('h4', '', object(page) ? page.title || `Page ${index + 1}` : `Page ${index + 1}`));
      const text = element('div', 'visual-plan-appendix-body');
      if (object(page) && typeof page.body === 'string') content(text, page.body); else text.append(fallback(page));
      section.append(text); body.append(section);
    });
    details.append(body); wrap.append(details);
  } else if (pages != null) wrap.append(fallback(pages));
}
