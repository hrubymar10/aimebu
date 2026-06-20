// aimebu – visual plan block renderers. Depends on utils.js and render-markdown.js.
function visualPlanBlockData(block) {
  if (!block || block.data === undefined || block.data === null) return {};
  if (typeof block.data === 'string') {
    try {
      return JSON.parse(block.data);
    } catch (_) {
      return { text: block.data };
    }
  }
  if (typeof block.data === 'object') return block.data;
  return { text: String(block.data) };
}

function visualPlanBlockHeader(block) {
  return '<div class="visual-plan-block-header">' +
    '<span class="visual-plan-block-type">' + esc(block.type || 'block') + '</span>' +
    (block.title ? '<span class="visual-plan-block-title">' + esc(block.title) + '</span>' : '') +
  '</div>';
}

function renderRawBlockData(data) {
  var text = '';
  if (data === undefined) text = '';
  else if (data === null) text = 'null';
  else if (typeof data === 'string') text = data;
  else {
    try {
      text = JSON.stringify(data, null, 2);
    } catch (_) {
      text = String(data);
    }
  }
  return '<pre class="visual-plan-raw-fallback"><code>' + esc(text) + '</code></pre>';
}

function visualPlanHasMeaningfulValue(value) {
  if (value === undefined || value === null) return false;
  if (typeof value === 'string') return value.trim() !== '';
  if (Array.isArray(value)) return value.length > 0;
  if (typeof value === 'object') return Object.keys(value).length > 0;
  return true;
}

function visualPlanFileTreeEntry(entry) {
  if (typeof entry === 'string') return { name: entry, type: 'file' };
  if (!entry || typeof entry !== 'object') return null;
  var children = Array.isArray(entry.children) ? entry.children.map(visualPlanFileTreeEntry).filter(Boolean) : [];
  var name = entry.name || entry.path || '';
  if (!name && !children.length) return null;
  var rawType = String(entry.type || '').toLowerCase();
  var type = rawType === 'dir' || rawType === 'directory' || rawType === 'folder' || children.length ? 'dir' : 'file';
  return {
    name: name,
    type: type,
    note: entry.note || entry.status || entry.description || '',
    children: children,
  };
}

function visualPlanFileTreeRoots(data) {
  var source = null;
  if (Array.isArray(data)) source = data;
  else if (data && typeof data === 'object' && data.root) source = [data.root];
  else if (data && typeof data === 'object' && Array.isArray(data.files)) source = data.files;
  else if (data && typeof data === 'object' && Array.isArray(data.children) && !(data.name || data.path)) source = data.children;
  else if (data && typeof data === 'object' && (data.name || data.path || Array.isArray(data.children))) source = [data];
  if (!source) return [];
  return source.map(visualPlanFileTreeEntry).filter(Boolean);
}

function renderVisualPlanFileTreeNode(node) {
  if (!node) return '';
  var children = Array.isArray(node.children) ? node.children : [];
  if (!node.name && children.length) return children.map(renderVisualPlanFileTreeNode).join('');
  if (!node.name) return '';
  return '<li><span class="visual-plan-file-node ' + esc(node.type || 'file') + '">' +
    '<span class="visual-plan-file-name">' + esc(node.name) + '</span>' +
    (node.note ? '<span class="visual-plan-file-note">' + esc(node.note) + '</span>' : '') +
    '</span>' +
    (children.length ? '<ul>' + children.map(renderVisualPlanFileTreeNode).join('') + '</ul>' : '') +
  '</li>';
}

function renderVisualPlanFileTree(data) {
  var roots = visualPlanFileTreeRoots(data);
  if (!roots.length) return renderRawBlockData(data);
  return '<ul class="visual-plan-file-tree">' + roots.map(renderVisualPlanFileTreeNode).join('') + '</ul>';
}

function renderVisualPlanDataModel(data) {
  var entities = Array.isArray(data.entities) ? data.entities : (Array.isArray(data.tables) ? data.tables : []);
  if (!entities.length) return renderRawBlockData(data);
  return entities.map(function (entity) {
    var fields = Array.isArray(entity.fields) ? entity.fields : [];
    return '<div class="visual-plan-model-entity">' +
      '<div class="visual-plan-model-name">' + esc(entity.name || entity.table || 'Entity') + '</div>' +
      '<table class="visual-plan-block-table"><tbody>' +
        fields.map(function (field) {
          if (typeof field === 'string') return '<tr><td colspan="3">' + esc(field) + '</td></tr>';
          return '<tr><td>' + esc(field.name || '') + '</td><td>' + esc(field.type || '') + '</td><td>' + esc(field.notes || field.description || '') + '</td></tr>';
        }).join('') +
      '</tbody></table>' +
    '</div>';
  }).join('');
}

function renderVisualPlanAPIEndpoint(data) {
  var rows = [
    ['Method', data.method || ''],
    ['Path', data.path || data.url || ''],
    ['Request', data.request || data.request_body || ''],
    ['Response', data.response || data.response_body || ''],
    ['Notes', data.notes || data.description || ''],
  ].filter(function (row) { return row[1] !== ''; });
  if (!rows.length) return renderRawBlockData(data);
  return '<table class="visual-plan-block-table"><tbody>' + rows.map(function (row) {
    var val = typeof row[1] === 'object' ? JSON.stringify(row[1], null, 2) : String(row[1]);
    return '<tr><th>' + esc(row[0]) + '</th><td><pre>' + esc(val) + '</pre></td></tr>';
  }).join('') + '</tbody></table>';
}

function renderVisualPlanAnnotatedCode(data) {
  var code = data.code || data.source || '';
  var annotations = Array.isArray(data.annotations) ? data.annotations : [];
  if (!code && !annotations.length) return renderRawBlockData(data);
  return '<pre class="visual-plan-code-block"><code>' + esc(code) + '</code></pre>' +
    (annotations.length ? '<ol class="visual-plan-annotations">' + annotations.map(function (a) {
      if (typeof a === 'string') return '<li>' + esc(a) + '</li>';
      return '<li><span>' + esc(a.line ? 'L' + a.line : '') + '</span>' + esc(a.text || a.note || '') + '</li>';
    }).join('') + '</ol>' : '');
}

function renderVisualPlanChecklist(data) {
  var items = Array.isArray(data.items) ? data.items : [];
  if (!items.length) return renderRawBlockData(data);
  return '<div class="visual-plan-checklist">' + items.map(function (item) {
    var text = typeof item === 'string' ? item : (item.text || item.label || '');
    var checked = typeof item === 'object' && !!item.checked;
    return '<div class="visual-plan-check-row">' +
      '<input type="checkbox" disabled' + (checked ? ' checked' : '') + '>' +
      '<span class="' + (checked ? 'checked' : '') + '">' + esc(text) + '</span>' +
    '</div>';
  }).join('') + '</div>';
}

function renderVisualPlanQuestionForm(data) {
  var questions = Array.isArray(data.questions) ? data.questions : [];
  if (!questions.length) return renderRawBlockData(data);
  return '<div class="visual-plan-question-list">' + questions.map(function (q, idx) {
    var options = Array.isArray(q.options) ? q.options : [];
    return '<div class="visual-plan-question">' +
      '<div class="visual-plan-question-title">Q' + (idx + 1) + '. ' + esc(q.question || '') + '</div>' +
      (q.description ? '<div class="visual-plan-question-desc">' + renderMarkdown(q.description) + '</div>' : '') +
      '<div class="visual-plan-question-options">' + options.map(function (opt) { return '<span>' + esc(opt) + '</span>'; }).join('') + '</div>' +
    '</div>';
  }).join('') + '</div>';
}

function clampPercent(value, fallback) {
  var n = Number(value);
  if (!Number.isFinite(n)) return fallback;
  return Math.max(0, Math.min(100, n));
}

function visualPlanBoundsStyle(node, x, y, w, h) {
  return 'left:' + clampPercent(node.x, x).toFixed(2) + '%;' +
    'top:' + clampPercent(node.y, y).toFixed(2) + '%;' +
    'width:' + clampPercent(node.w, w).toFixed(2) + '%;' +
    'height:' + clampPercent(node.h, h).toFixed(2) + '%;';
}

function renderVisualPlanCanvas(data) {
  var nodes = Array.isArray(data.nodes) ? data.nodes : [];
  if (!nodes.length) return renderRawBlockData(data);
  return '<div class="visual-plan-canvas">' + nodes.map(function (node) {
    var style = visualPlanBoundsStyle(node || {}, 0, 0, 20, 10);
    return '<div class="visual-plan-canvas-node" style="' + escAttr(style) + '">' + esc(node.label || node.text || node.id || '') + '</div>';
  }).join('') + '</div>';
}

function visualPlanPrototypeSrcdoc(data) {
  var screens = Array.isArray(data.screens) ? data.screens : [];
  if (!screens.length) {
    return '<!doctype html><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="default-src \'none\'; style-src \'unsafe-inline\'; img-src data:"><style>body{font:14px system-ui;margin:16px;color:#222;background:#fff}pre{white-space:pre-wrap}</style>' + renderRawBlockData(data);
  }
  var firstID = screens[0].id || 'screen-0';
  var css = 'body{font:14px system-ui;margin:0;background:#f8fafc;color:#111827}.screen{min-height:280px;position:relative;padding:16px;display:none}.screen:first-of-type{display:block}.screen:target{display:block}.screen:target~.screen{display:none}body:has(.screen:target) .screen:first-of-type{display:none}.title{font-weight:700;margin-bottom:8px}.el{position:absolute;border:1px solid #94a3b8;background:#fff;border-radius:6px;padding:8px;box-sizing:border-box}.button{background:#111827;color:#fff;text-decoration:none;text-align:center}.input{background:#f1f5f9;color:#64748b}';
  var body = screens.map(function (screen, idx) {
    var elements = Array.isArray(screen.elements) ? screen.elements : [];
    var screenID = screen.id || ('screen-' + idx);
    return '<section class="screen" id="' + escAttr(screenID) + '">' +
      '<div class="title">' + esc(screen.title || screen.id || ('Screen ' + (idx + 1))) + '</div>' +
      elements.map(function (el) {
        el = el || {};
        var cls = el.type === 'button' || el.target ? 'el button' : (el.type === 'input' ? 'el input' : 'el');
        var style = visualPlanBoundsStyle(el, 4, 16, 24, 10);
        var text = esc(el.text || el.label || el.id || '');
        if (el.target) return '<a class="' + cls + '" style="' + escAttr(style) + '" href="#' + escAttr(el.target) + '">' + text + '</a>';
        return '<div class="' + cls + '" style="' + escAttr(style) + '">' + text + '</div>';
      }).join('') +
    '</section>';
  }).join('');
  return '<!doctype html><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="default-src \'none\'; style-src \'unsafe-inline\'; img-src data:"><style>' + css + '</style><a href="#' + escAttr(firstID) + '" style="position:absolute;left:-9999px">start</a>' + body;
}

function renderVisualPlanBlock(block, idx) {
  var data = visualPlanBlockData(block);
  var body = '';
  try {
    if (block.type === 'markdown') body = visualPlanHasMeaningfulValue(data.markdown || data.text) ? renderMarkdown(data.markdown || data.text || '') : renderRawBlockData(data);
    else if (block.type === 'file-tree') body = renderVisualPlanFileTree(data);
    else if (block.type === 'data-model') body = renderVisualPlanDataModel(data);
    else if (block.type === 'api-endpoint') body = renderVisualPlanAPIEndpoint(data);
    else if (block.type === 'annotated-code') body = renderVisualPlanAnnotatedCode(data);
    else if (block.type === 'diff') body = visualPlanHasMeaningfulValue(data.diff || data.text) ? '<pre class="visual-plan-code-block visual-plan-diff"><code>' + esc(data.diff || data.text || '') + '</code></pre>' : renderRawBlockData(data);
    else if (block.type === 'checklist') body = renderVisualPlanChecklist(data);
    else if (block.type === 'question-form') body = renderVisualPlanQuestionForm(data);
    else if (block.type === 'diagram') body = visualPlanHasMeaningfulValue(data.mermaid || data.source || data.text) ? '<pre class="mermaid visual-plan-mermaid" data-visual-plan-block="' + escAttr(block.id || String(idx)) + '">' + esc(data.mermaid || data.source || data.text || '') + '</pre>' : renderRawBlockData(data);
    else if (block.type === 'canvas') body = renderVisualPlanCanvas(data);
    else if (block.type === 'prototype') body = '<iframe class="visual-plan-prototype-frame" sandbox srcdoc="' + escAttr(visualPlanPrototypeSrcdoc(data)) + '"></iframe>';
    else body = renderRawBlockData(data);
  } catch (err) {
    body = renderRawBlockData(data);
  }
  return '<section class="visual-plan-block">' + visualPlanBlockHeader(block) + '<div class="visual-plan-block-body">' + body + '</div></section>';
}

function renderAppendixPage(page, idx) {
  page = page || {};
  var title = page.title || ('Page ' + (idx + 1));
  return '<section class="visual-plan-appendix-page">' +
    '<h4>' + esc(title) + '</h4>' +
    '<div class="visual-plan-appendix-body md-rendered">' + renderMarkdown(page.body || '') + '</div>' +
  '</section>';
}

function renderVisualPlanAppendix(pages) {
  if (!Array.isArray(pages) || !pages.length) return '';
  return '<details class="visual-plan-block visual-plan-appendix">' +
    '<summary>Full plan (' + esc(String(pages.length)) + ' page' + (pages.length === 1 ? '' : 's') + ')</summary>' +
    '<div class="visual-plan-block-body">' + pages.map(renderAppendixPage).join('') + '</div>' +
  '</details>';
}

function visualPlanHTML(message) {
  var blocks = Array.isArray(message.visual_plan) ? message.visual_plan.slice() : [];
  var appendixPages = Array.isArray(message.appendix_pages) ? message.appendix_pages.slice() : [];
  if (!blocks.length && !appendixPages.length) return '';
  blocks.sort(function (a, b) { return (a.order || 0) - (b.order || 0); });
  return '<div class="visual-plan" data-msg-id="' + esc(String(message.id)) + '">' +
    blocks.map(renderVisualPlanBlock).join('') +
    renderVisualPlanAppendix(appendixPages) +
  '</div>';
}

function mermaidNodeIsError(node) {
  if (!node.querySelector('svg')) return true;
  return !!(node.querySelector('.error-icon, .error-text'));
}

function runMermaidNode(node, source, onDone, isRetry) {
  window.mermaid.run({ nodes: [node] }).then(function () {
    if (!isRetry && mermaidNodeIsError(node)) {
      // mermaid.run skips nodes with data-processed set; clear it before retry.
      node.removeAttribute('data-processed');
      node.textContent = source;
      runMermaidNode(node, source, onDone, true);
      return;
    }
    if (mermaidNodeIsError(node)) {
      restoreMermaidSource(node, source, new Error('diagram render produced error graphic'));
    }
    if (onDone) onDone();
  }).catch(function (err) {
    restoreMermaidSource(node, source, err);
    if (onDone) onDone();
  });
}

function renderMermaidBlocks(root, onDone) {
  if (!window.mermaid || !root) return;
  var nodes = Array.from(root.querySelectorAll('.visual-plan-mermaid'));
  if (!nodes.length) return;
  try {
    window.mermaid.initialize({ startOnLoad: false, securityLevel: 'strict', theme: document.documentElement.getAttribute('data-theme') === 'dark' ? 'dark' : 'default' });
    nodes.forEach(function (node) {
      var source = node.textContent || '';
      runMermaidNode(node, source, onDone, false);
    });
  } catch (err) {
    nodes.forEach(function (node) {
      restoreMermaidSource(node, node.textContent || '', err);
    });
    if (onDone) onDone();
  }
}

function restoreMermaidSource(node, source, err) {
  if (!node || node.dataset.visualPlanFallback === '1') return;
  node.dataset.visualPlanFallback = '1';
  node.classList.remove('mermaid');
  node.classList.add('visual-plan-mermaid-fallback');
  node.textContent = '';
  var label = document.createElement('span');
  label.className = 'visual-plan-render-error';
  label.textContent = 'Diagram failed to render' + (err && err.message ? ': ' + err.message : '');
  var code = document.createElement('code');
  code.textContent = source;
  node.appendChild(label);
  node.appendChild(code);
}
