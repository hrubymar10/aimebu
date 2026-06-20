// aimebu – Markdown renderer. Depends on utils.js.
function renderMarkdown(rawText) {
  if (!rawText) return '';
  /*
    List regression fixtures:
    - "1. one\n\n2. two" -> one <ol> with two <li> children.
    - "5. five\n\n6. six" -> one <ol start="5"> with two <li> children.
    - "- one\n\n- two" -> one <ul> with two <li> children.
  */
  var html = esc(rawText);
  var holders = [];

  function stash(s) {
    holders.push(s);
    return '\x00' + (holders.length - 1) + '\x00';
  }

  function codeBlock(code, lang) {
    var normalized = code.replace(/\n$/, '');
    var cls = lang.trim() ? ' class="lang-' + lang.trim() + '"' : '';
    var copyPayload = encodeURIComponent(unescHtml(normalized));
    return '<div class="md-codeblock">' +
      '<button class="md-copy-btn" type="button" data-code="' + copyPayload + '" aria-label="Copy code block" title="Copy code block">' + COPY_CODE_ICON + '</button>' +
      '<pre class="md-pre"><code' + cls + '>' + normalized + '</code></pre>' +
      '</div>';
  }

  // Extract fenced code blocks before any other transforms
  html = html.replace(/```([a-zA-Z0-9]*)\n?([\s\S]*?)```/g, function (_, lang, code) {
    return stash(codeBlock(code, lang));
  });
  html = html.replace(/~~~([a-zA-Z0-9]*)\n?([\s\S]*?)~~~/g, function (_, lang, code) {
    return stash(codeBlock(code, lang));
  });

  // Extract inline code spans
  html = html.replace(/`([^`\n]+)`/g, function (_, code) {
    return stash('<code class="md-code">' + code + '</code>');
  });

  function applyInline(s) {
    s = s.replace(/\*\*\*(.+?)\*\*\*/g, '<strong><em>$1</em></strong>');
    s = s.replace(/\*\*(.+?)\*\*/g, '<strong>$1</strong>');
    s = s.replace(/__(.+?)__/g, '<strong>$1</strong>');
    s = s.replace(/\*([^*\n]+)\*/g, '<em>$1</em>');
    s = s.replace(/_([^_\n]+)_/g, '<em>$1</em>');
    s = s.replace(/\[([^\]]+)\]\(([^)]+)\)/g, function (_, text, href) {
      if (!/^(https?:|mailto:)/i.test(href)) return text;
      // esc() does not encode " or ' — percent-encode them so they can't break out of href=""
      var safeHref = href.replace(/"/g, '%22').replace(/'/g, '%27');
      return '<a href="' + safeHref + '" target="_blank" rel="noopener noreferrer">' + text + '</a>';
    });
    // Bare URLs — skip those already inside href="..." or link text (preceded by > from a tag)
    s = s.replace(/(?<![="'(>])(https?:\/\/[^\s<>"]+)/g, function (_, url) {
      return '<a href="' + url + '" target="_blank" rel="noopener noreferrer">' + url + '</a>';
    });
    // Stash all existing <a>…</a> tags so the #NN pass can't corrupt URLs
    // that contain fragment identifiers (e.g. https://x.com/path#42).
    var linkHolders = [];
    s = s.replace(/<a\b[^>]*>[\s\S]*?<\/a>/g, function (match) {
      linkHolders.push(match);
      return '\x01' + (linkHolders.length - 1) + '\x01';
    });
    // #NN message references — only match positive IDs (not #0)
    s = s.replace(/(?<![="'(>])#([1-9]\d*)\b/g, function (_, id) {
      return '<a class="msg-ref" data-msg-id="' + id + '">#' + id + '</a>';
    });
    // Restore stashed links
    s = s.replace(/\x01(\d+)\x01/g, function (_, i) { return linkHolders[+i]; });
    return s;
  }

  var lines = html.split('\n');
  var out = [];
  var i = 0;
  var unorderedListRE = /^[-*]\s/;
  var orderedListRE = /^(\d+)\.\s/;

  function nextNonBlankIndex(start) {
    var idx = start;
    while (idx < lines.length && lines[idx].trim() === '') idx++;
    return idx;
  }

  while (i < lines.length) {
    var line = lines[i];

    // Stashed placeholder on its own line (fenced code block)
    if (/^\x00\d+\x00$/.test(line.trim())) {
      out.push({ type: 'block', html: line.trim() });
      i++;
      continue;
    }

    // Narrow CommonMark-style indented code block: line starts after BOF or
    // a blank line, then the run continues while indentation holds.
    if (hasIndentedPrefix(line) && (i === 0 || lines[i - 1].trim() === '')) {
      var indented = [];
      while (i < lines.length) {
        if (lines[i].trim() === '') break;
        if (!hasIndentedPrefix(lines[i])) break;
        indented.push(lines[i]);
        i++;
      }
      out.push({ type: 'block', html: codeBlock(indented.join('\n'), '') });
      continue;
    }

    // Heading h1–h3
    var hm = line.match(/^(#{1,3})\s(.+)$/);
    if (hm) {
      var lvl = hm[1].length;
      out.push({ type: 'block', html: '<h' + lvl + ' class="md-h">' + applyInline(hm[2]) + '</h' + lvl + '>' });
      i++;
      continue;
    }

    // Blockquote
    if (/^(&gt;|>)/.test(line)) {
      var bqLines = [];
      while (i < lines.length && /^(&gt;|>)/.test(lines[i])) {
        bqLines.push(lines[i].replace(/^(&gt;|>)\s?/, ''));
        i++;
      }
      out.push({ type: 'block', html: '<blockquote class="md-blockquote">' + applyInline(bqLines.join('<br>')) + '</blockquote>' });
      continue;
    }

    // Unordered list
    if (unorderedListRE.test(line)) {
      var ulItems = [];
      while (i < lines.length) {
        if (unorderedListRE.test(lines[i])) {
          ulItems.push('<li>' + applyInline(lines[i].replace(/^[-*]\s+/, '')) + '</li>');
          i++;
          continue;
        }
        if (lines[i].trim() === '') {
          var nextUL = nextNonBlankIndex(i);
          if (nextUL < lines.length && unorderedListRE.test(lines[nextUL])) {
            i = nextUL;
            continue;
          }
        }
        break;
      }
      out.push({ type: 'block', html: '<ul class="md-list">' + ulItems.join('') + '</ul>' });
      continue;
    }

    // Ordered list
    var orderedStart = line.match(orderedListRE);
    if (orderedStart) {
      var startAttr = orderedStart[1] === '1' ? '' : ' start="' + orderedStart[1] + '"';
      var olItems = [];
      while (i < lines.length) {
        if (orderedListRE.test(lines[i])) {
          olItems.push('<li>' + applyInline(lines[i].replace(/^\d+\.\s+/, '')) + '</li>');
          i++;
          continue;
        }
        if (lines[i].trim() === '') {
          var nextOL = nextNonBlankIndex(i);
          if (nextOL < lines.length && orderedListRE.test(lines[nextOL])) {
            i = nextOL;
            continue;
          }
        }
        break;
      }
      out.push({ type: 'block', html: '<ol class="md-list"' + startAttr + '>' + olItems.join('') + '</ol>' });
      continue;
    }

    // Empty line — paragraph separator
    if (line.trim() === '') {
      out.push({ type: 'empty' });
      i++;
      continue;
    }

    // Normal inline text
    out.push({ type: 'text', html: applyInline(line) });
    i++;
  }

  var result = '';
  for (var j = 0; j < out.length; j++) {
    var item = out[j];
    if (item.type === 'block') {
      result += item.html;
    } else if (item.type === 'empty') {
      result += '<br>';
    } else {
      var nextIsText = j + 1 < out.length && out[j + 1].type === 'text';
      result += item.html + (nextIsText ? '<br>' : '');
    }
  }

  // Restore placeholders
  result = result.replace(/\x00(\d+)\x00/g, function (_, k) {
    return holders[+k];
  });

  return result;
}

function hasIndentedPrefix(line) {
  return /^\t/.test(line) || /^ {4}/.test(line);
}

function rewriteIndentedCodeBlocks(html, renderBlock) {
  var lines = html.split('\n');
  var out = [];
  var i = 0;

  while (i < lines.length) {
    var line = lines[i];
    var prev = i === 0 ? '' : lines[i - 1];
    if (!hasIndentedPrefix(line) || (i > 0 && prev.trim() !== '')) {
      out.push(line);
      i++;
      continue;
    }
    var block = [];
    while (i < lines.length) {
      var cur = lines[i];
      if (cur.trim() === '') {
        block.push(cur);
        i++;
        break;
      }
      if (!hasIndentedPrefix(cur)) break;
      block.push(cur);
      i++;
    }
    out.push(renderBlock(block.join('\n')));
  }
  return out.join('\n');
}

function renderPlainWithCodeMarkers(rawText) {
  if (!rawText) return '';
  var html = esc(rawText);
  var holders = [];

  function stash(s) {
    holders.push(s);
    return '\x00' + (holders.length - 1) + '\x00';
  }

  function rawCodeBlock(text, code) {
    var normalized = code.replace(/\n$/, '');
    var copyPayload = encodeURIComponent(unescHtml(normalized));
    return '<div class="md-codeblock raw-codeblock">' +
      '<button class="md-copy-btn" type="button" data-code="' + copyPayload + '" aria-label="Copy code block" title="Copy code block">' + COPY_CODE_ICON + '</button>' +
      '<span class="raw-code raw-code-block">' + text + '</span>' +
      '</div>';
  }

  html = html.replace(/```([a-zA-Z0-9]*)\n?([\s\S]*?)```/g, function (_, lang, code) {
    var text = '```' + (lang || '') + '\n' + code + '```';
    return stash(rawCodeBlock(text, code));
  });
  html = html.replace(/~~~([a-zA-Z0-9]*)\n?([\s\S]*?)~~~/g, function (_, lang, code) {
    var text = '~~~' + (lang || '') + '\n' + code + '~~~';
    return stash(rawCodeBlock(text, code));
  });
  html = rewriteIndentedCodeBlocks(html, function (block) {
    return stash(rawCodeBlock(block, block));
  });
  html = html.replace(/`([^`\n]+)`/g, function (_, code) {
    return stash('<code class="raw-code">' + code + '</code>');
  });

  return html.replace(/\x00(\d+)\x00/g, function (_, i) {
    return holders[+i];
  });
}
