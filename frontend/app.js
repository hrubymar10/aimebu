// aimebu – room-based frontend
// Vanilla JS, no build tools, no frameworks.

(function () {
  'use strict';

  // ── State ────────────────────────────────────────────────────────

  let rooms = [];              // Room[] — all rooms from server
  let activeRoomID = null;     // currently viewed room ID
  let messages = {};           // { roomID: Message[] }
  let agents = [];             // Agent[]
  // First-time visitors get 'user' as a placeholder; init() will try to
  // replace it with $AIMEBU_NAME from the server before registration.
  let agentID = localStorage.getItem('aimebu_agent_id') || 'user';
  let agentIDFromStorage = localStorage.getItem('aimebu_agent_id') !== null;
  let registered = false; // true after a successful POST /agents for current agentID
  let ws = null;               // WebSocket connection
  let wsReconnectTimer = null;
  let wsReconnectAttempts = 0;
  let prevSubscribedRoom = null; // room we're currently subscribed to via WS
  let lastMessagePreview = {}; // { roomID: string } — for sidebar preview
  let unreadCounts = {};       // { roomID: number } — unread per room for current agent
  let readCursors = {};        // { roomID: int64 as number } — cursor per room
  // presence[roomID][agentID] = { cursor, waiting } — per-room per-agent
  // read cursor + live-listening flag. Populated by GET /rooms/{id}
  // (members_presence) and kept in sync by WS `presence` events.
  // The special bucket presence['*'][agentID] carries agent-wide waits
  // (bus_wait without a room filter) — those apply to every room the agent
  // is in.
  let presence = {};
  let markdownMode = localStorage.getItem('aimebu.ui.markdownMode') || 'rendered';
  let macros = {};           // { lowercasedKey: body } — global shared macros
  let promptEntries = [];    // PromptEntry[] from GET /settings/prompts
  let systemEvents = [];     // Message[] — _system room events
  let systemUnread = 0;      // unread count for broadcast panel
  let systemSSE = null;      // EventSource for _system room
  let macrosSaveTimer = null;
  let serverSettings = {};   // Settings from GET /settings
  let macrosFilter = '';     // search filter for macros panel
  let initComplete = false;  // true after first WS open — guards notification playback
  let maxSeenMsgID = 0;      // highest message id seen via WS or HTTP — replay guard
  let attentionCounts = {};  // { roomID: number } — needs-attention messages per room
  let attentionTimers = {};        // { roomID: timeoutID } — pending 3s fade timers
  let attentionFocusListeners = {}; // { roomID: fn } — pending focus listener per room
  let notifSounds = [];      // sound list from GET /api/sounds
  let notifAudioCache = {};  // { soundID: HTMLAudioElement } — lazily created
  let notifAudioPrimed = false; // true after a trusted gesture warms audio playback
  let notifAudioPriming = null; // in-flight prime promise — dedupes gesture races
  let notifPromptAttempted = false; // flipped only after a real prompt attempt or CTA display
  let pendingNotifPrompt = null; // { senderName } queued while the tab is hidden
  let messageDebugState = {
    open: false,
    messageID: null,
    selectedViewerID: '',
    povCacheByMessageID: {},
    loading: false,
    error: '',
  };

  // Autocomplete state
  let acItems = [];          // Array<{kind,insertText,displayKey,preview}> — ac candidates
  let acSelected = -1;       // currently highlighted item index
  let acHideTimer = null;    // debounce timer for blur→hide

  // Composer history state (terminal-style ↑/↓)
  let historyIdx = null;     // null = scratch; integer = index into getRecallCandidates()
  let historyDraft = null;   // saved in-progress text during navigation
  const specialMentionItems = [
    { token: 'everyone', preview: 'all members of this room' },
    { token: 'all', preview: 'alias for @everyone' },
    { token: 'channel', preview: 'all members of this room' },
    { token: 'here', preview: 'active room members' },
    { token: 'humans', preview: 'human members of this room' },
    { token: 'ais', preview: 'AI members of this room' }
  ];

  // ── DOM refs ─────────────────────────────────────────────────────

  const $ = (sel) => document.querySelector(sel);
  const agentIDInput = $('#agent-id-input');
  const connectionStatus = $('#connection-status');
  const statusText = connectionStatus.querySelector('.status-text');
  const settingsBtn = $('#settings-btn');
  const settingsModal = $('#settings-modal');
  const settingsOverlay = $('#settings-overlay');
  const settingsCloseBtn = $('#settings-close-btn');
  const settingsSectionTitle = $('#settings-section-title');
  const themeToggleBtn = $('#theme-toggle-btn');
  const debugToggleBtn = $('#debug-toggle-btn');
  const macrosSearchInput = $('#macros-search-input');
  const macrosCopyBtn = $('#macros-copy-btn');
  const macrosImportBtn = $('#macros-import-btn');
  const macrosImportFallback = $('#macros-import-fallback');
  const macrosImportTextarea = $('#macros-import-textarea');
  const macrosImportApplyBtn = $('#macros-import-apply-btn');
  const macrosImportCancelBtn = $('#macros-import-cancel-btn');
  const backupExportBtn = $('#backup-export-btn');
  const backupImportBtn = $('#backup-import-btn');
  const backupImportFile = $('#backup-import-file');
  const clearStateBtn = $('#clear-state-btn');
  const clearAllBtn = $('#clear-all-btn');

  const joinRoomInput = $('#join-room-input');
  const joinRoomBtn = $('#join-room-btn');
  const roomListEl = $('#room-list');
  const dmListEl = $('#dm-list');

  const noRoomView = $('#no-room-view');
  const roomView = $('#room-view');
  const roomIconEl = $('#room-icon');
  const roomNameEl = $('#room-name');
  const roomMemberCount = $('#room-member-count');
  const roomMemberAvatars = $('#room-member-avatars');
  const leaveRoomBtn = $('#leave-room-btn');
  const exportBtn = $('#export-btn');
  const exportMenu = $('#export-menu');
  const exportWrap = exportBtn ? exportBtn.closest('.export-wrap') : null;
  const messageListEl = $('#message-list');
  const sendForm = $('#send-form');
  const systemRoomNotice = $('#system-room-notice');
  const msgBodyInput = $('#msg-body');

  const roomAgentsList = $('#room-agents-list');
  const allAgentsList = $('#all-agents-list');

  const mobileTabs = $('#mobile-tabs');
  const mdToggleBtn = $('#md-toggle-btn');
  const msgSearchBtn = $('#msg-search-btn');
  const msgSearchBar = $('#msg-search-bar');
  const msgSearchInput = $('#msg-search-input');
  const notifPromptBanner = $('#notif-prompt-banner');
  const notifPromptBannerText = $('#notif-prompt-banner-text');
  const notifPromptEnableBtn = $('#notif-prompt-enable-btn');
  const notifPromptDismissBtn = $('#notif-prompt-dismiss-btn');

  const broadcastBtn = $('#broadcast-btn');
  const broadcastBadge = $('#broadcast-badge');
  const systemEventsPanel = $('#system-events-panel');
  const systemEventsListEl = $('#system-events-list');
  const messageDebugModal = $('#message-debug-modal');
  const messageDebugOverlay = $('#message-debug-overlay');
  const messageDebugCloseBtn = $('#message-debug-close-btn');
  const messageDebugMessageSelect = $('#message-debug-message-select');
  const messageDebugPrevBtn = $('#message-debug-prev-btn');
  const messageDebugNextBtn = $('#message-debug-next-btn');
  const messageDebugViewerSelect = $('#message-debug-viewer-select');
  const messageDebugStored = $('#message-debug-stored');
  const messageDebugViewer = $('#message-debug-viewer');
  const messageDebugStatus = $('#message-debug-status');

  const macrosListEl = $('#macros-list');
  const promptsListEl = $('#prompts-list');
  const promptsResetAllBtn = $('#prompts-reset-all-btn');
  const macroAddForm = $('#macro-add-form');
  const macroKeyInput = $('#macro-key-input');
  const macroBodyInput = $('#macro-body-input');
  const acPopupEl = $('#ac-popup');
  const agentIDDefaultInput = $('#agent-id-default-input');
  const systemEventsToggleBtn = $('#system-events-toggle-btn');
  const notifEnabledBtn = $('#notif-enabled-btn');
  const notifSoundSelect = $('#notif-sound-select');
  const notifTestBtn = $('#notif-test-btn');
  const notifVolumeSlider = $('#notif-volume-slider');
  const notifVolumeLabel = $('#notif-volume-label');
  const notifUploadBtn = $('#notif-upload-btn');
  const notifUploadFile = $('#notif-upload-file');
  const notifSoundsListEl = $('#notif-sounds-list');
  const notifAudioStatusEl = $('#notif-audio-status');
  const notifSysBtn = $('#notif-sys-btn');
  const notifSysForceBtn = $('#notif-sys-force-btn');
  const notifSysStatusEl = $('#notif-sys-status');
  const notifSysHelpEl = $('#notif-sys-help');
  const notifSysHelpCloseBtn = $('#notif-sys-help-close-btn');

  // ── Harness icons ────────────────────────────────────────────────

  var harnessIconMap = {
    'claude-code': '/icons/claude-code.svg',
    'codex':       '/icons/codex.svg',
    'cursor':      '/icons/cursor.svg',
    'cline':       '/icons/cline.svg',
    'pi':          '/icons/pi.svg',
  };

  function harnessIconSrc(harness) {
    return harnessIconMap[harness] || '/icons/unknown.svg';
  }

  function agentIconSrc(agent) {
    if (!agent) return '/icons/unknown.svg';
    if (agent.kind === 'human') return '/icons/human.svg';
    return harnessIconSrc(agent.harness);
  }

  // ── Utility ──────────────────────────────────────────────────────

  function esc(str) {
    const div = document.createElement('div');
    div.textContent = str || '';
    return div.innerHTML;
  }

  function escRe(s) {
    return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  }

  function setTemporaryLabel(button, text, ms) {
    if (!button) return;
    if (!button.dataset.defaultLabel) button.dataset.defaultLabel = button.textContent;
    button.textContent = text;
    clearTimeout(button._resetTimer);
    button._resetTimer = setTimeout(function () {
      button.textContent = button.dataset.defaultLabel;
    }, ms || 2500);
  }

  function flashTitleHint(el, text, ms) {
    if (!el) return;
    var original = el.getAttribute('title') || '';
    el.setAttribute('title', text);
    clearTimeout(el._titleResetTimer);
    el._titleResetTimer = setTimeout(function () {
      if (original) el.setAttribute('title', original);
      else el.removeAttribute('title');
    }, ms || 2500);
  }

  function fallbackCopyText(text) {
    var ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.top = '-9999px';
    ta.style.left = '-9999px';
    document.body.appendChild(ta);
    ta.focus();
    ta.select();
    try {
      return document.execCommand('copy');
    } finally {
      document.body.removeChild(ta);
    }
  }

  function copyText(text) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      return navigator.clipboard.writeText(text).catch(function () {
        if (fallbackCopyText(text)) return;
        throw new Error('clipboard write failed');
      });
    }
    return fallbackCopyText(text) ? Promise.resolve() : Promise.reject(new Error('clipboard write unavailable'));
  }

  function normalizeMacroKey(key) {
    return String(key || '').trim().toLowerCase();
  }

  function macroBodySize(value) {
    return new TextEncoder().encode(value).length;
  }

  function validateMacroMap(rawMap) {
    var source = rawMap && typeof rawMap === 'object' && !Array.isArray(rawMap) ? rawMap : {};
    var keys = Object.keys(source);
    var next = {};
    var invalid = 0;
    keys.forEach(function (originalKey) {
      var key = normalizeMacroKey(originalKey);
      var value = source[originalKey];
      if (!/^[a-z][a-z0-9_-]*$/.test(key) || typeof value !== 'string' || macroBodySize(value) > 16 * 1024 || next[key] !== undefined) {
        invalid++;
        return;
      }
      next[key] = value;
    });
    return {
      macros: next,
      invalid: invalid,
    };
  }

  function parseImportedMacros(rawText) {
    var parsed;
    try {
      parsed = JSON.parse(rawText);
    } catch (_) {
      throw new Error('Invalid JSON');
    }
    var importedFromBackup = false;
    var candidate = parsed;
    if (parsed && typeof parsed === 'object' && !Array.isArray(parsed) && parsed.macros && typeof parsed.macros === 'object' && !Array.isArray(parsed.macros)) {
      candidate = parsed.macros;
      importedFromBackup = true;
    }
    if (!candidate || typeof candidate !== 'object' || Array.isArray(candidate)) {
      throw new Error('Expected a macros JSON object');
    }
    var validated = validateMacroMap(candidate);
    var totalEntries = Object.keys(candidate).length;
    if (totalEntries > 256) {
      throw new Error('Too many macros (max 256)');
    }
    return {
      importedFromBackup: importedFromBackup,
      macros: validated.macros,
      invalid: validated.invalid,
      totalEntries: totalEntries,
    };
  }

  function hideMacrosImportFallback() {
    if (!macrosImportFallback) return;
    macrosImportFallback.classList.add('hidden');
    if (macrosImportTextarea) macrosImportTextarea.value = '';
  }

  function showMacrosImportFallback() {
    if (!macrosImportFallback) return;
    macrosImportFallback.classList.remove('hidden');
    if (macrosImportTextarea) macrosImportTextarea.focus();
  }

  function persistMacros(nextMacros) {
    macros = nextMacros;
    renderMacrosList();
    return api('PUT', '/macros', { macros: macros });
  }

  function describeMacroMerge(incomingMacros) {
    var addCount = 0;
    var updateCount = 0;
    Object.keys(incomingMacros).forEach(function (key) {
      if (macros[key] === undefined) addCount++;
      else if (macros[key] !== incomingMacros[key]) updateCount++;
    });
    return { addCount: addCount, updateCount: updateCount };
  }

  function applyImportedMacros(rawText, sourceButton) {
    var parsed = parseImportedMacros(rawText);
    var incomingKeys = Object.keys(parsed.macros);
    var counts = describeMacroMerge(parsed.macros);
    var nextTotal = Object.keys(macros).length + counts.addCount;
    if (nextTotal > 256) {
      throw new Error('Import would exceed the 256 macro limit');
    }
    var details = [];
    if (parsed.importedFromBackup) {
      details.push('Detected full backup JSON — importing only the macros subset (' + incomingKeys.length + ' entries).');
    }
    details.push('Add ' + counts.addCount + ' new, update ' + counts.updateCount + ' existing (key match -> overwrite), skip ' + parsed.invalid + ' invalid. Continue?');
    if (!confirm(details.join('\n'))) return Promise.resolve(false);
    var merged = {};
    Object.keys(macros).forEach(function (key) { merged[key] = macros[key]; });
    incomingKeys.forEach(function (key) { merged[key] = parsed.macros[key]; });
    return persistMacros(merged).then(function () {
      hideMacrosImportFallback();
      if (sourceButton) {
        var label = 'Imported ' + incomingKeys.length;
        if (parsed.invalid) label += ' / skipped ' + parsed.invalid;
        setTemporaryLabel(sourceButton, label, 2500);
      }
      return true;
    });
  }

  function renderMarkdown(rawText) {
    if (!rawText) return '';
    var html = esc(rawText);
    var holders = [];

    function stash(s) {
      holders.push(s);
      return '\x00' + (holders.length - 1) + '\x00';
    }

    // Extract fenced code blocks before any other transforms
    html = html.replace(/```([a-zA-Z0-9]*)\n?([\s\S]*?)```/g, function (_, lang, code) {
      var cls = lang.trim() ? ' class="lang-' + lang.trim() + '"' : '';
      return stash('<pre class="md-pre"><code' + cls + '>' + code.replace(/\n$/, '') + '</code></pre>');
    });
    html = html.replace(/~~~([a-zA-Z0-9]*)\n?([\s\S]*?)~~~/g, function (_, lang, code) {
      var cls = lang.trim() ? ' class="lang-' + lang.trim() + '"' : '';
      return stash('<pre class="md-pre"><code' + cls + '>' + code.replace(/\n$/, '') + '</code></pre>');
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
        out.push({ type: 'block', html: '<pre class="md-pre"><code>' + indented.join('\n') + '</code></pre>' });
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
      if (/^[-*]\s/.test(line)) {
        var ulItems = [];
        while (i < lines.length && /^[-*]\s/.test(lines[i])) {
          ulItems.push('<li>' + applyInline(lines[i].replace(/^[-*]\s+/, '')) + '</li>');
          i++;
        }
        out.push({ type: 'block', html: '<ul class="md-list">' + ulItems.join('') + '</ul>' });
        continue;
      }

      // Ordered list
      if (/^\d+\.\s/.test(line)) {
        var olItems = [];
        while (i < lines.length && /^\d+\.\s/.test(lines[i])) {
          olItems.push('<li>' + applyInline(lines[i].replace(/^\d+\.\s+/, '')) + '</li>');
          i++;
        }
        out.push({ type: 'block', html: '<ol class="md-list">' + olItems.join('') + '</ol>' });
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

    html = html.replace(/```([a-zA-Z0-9]*)\n?([\s\S]*?)```/g, function (_, lang, code) {
      var text = '```' + (lang || '') + '\n' + code + '```';
      return stash('<span class="raw-code raw-code-block">' + text + '</span>');
    });
    html = html.replace(/~~~([a-zA-Z0-9]*)\n?([\s\S]*?)~~~/g, function (_, lang, code) {
      var text = '~~~' + (lang || '') + '\n' + code + '~~~';
      return stash('<span class="raw-code raw-code-block">' + text + '</span>');
    });
    html = rewriteIndentedCodeBlocks(html, function (block) {
      return stash('<span class="raw-code raw-code-block">' + block + '</span>');
    });
    html = html.replace(/`([^`\n]+)`/g, function (_, code) {
      return stash('<code class="raw-code">' + code + '</code>');
    });

    return html.replace(/\x00(\d+)\x00/g, function (_, i) {
      return holders[+i];
    });
  }

  function updateMdToggleBtn() {
    if (markdownMode === 'rendered') {
      mdToggleBtn.textContent = 'Rendered';
      mdToggleBtn.classList.remove('raw-mode');
    } else {
      mdToggleBtn.textContent = 'Raw';
      mdToggleBtn.classList.add('raw-mode');
    }
  }

  function expandMacros(body) {
    // \<KEY> passes through as literal <KEY> (backslash stripped).
    var ESCAPE = '\x02';
    var s = body.replace(/\\</g, ESCAPE);
    // Recursive expansion, depth-capped to 8 to handle cycles.
    var MAX_DEPTH = 8;
    for (var i = 0; i < MAX_DEPTH; i++) {
      var next = s.replace(/<([a-z][a-z0-9_-]*)>/gi, function (_, key) {
        var k = key.toLowerCase();
        return macros[k] !== undefined ? macros[k] : '<' + key + '>';
      });
      if (next === s) break;
      s = next;
    }
    return s.replace(/\x02/g, '<');
  }

  function getMergedMacros() {
    var merged = {};
    Object.keys(macros).forEach(function (k) { merged[k] = macros[k]; });
    return merged;
  }

  function getAcContext() {
    var val = msgBodyInput.value;
    var pos = msgBodyInput.selectionStart;
    var before = val.substring(0, pos);
    for (var i = before.length - 1; i >= 0; i--) {
      var ch = before[i];
      if (ch === '<' && (i === 0 || before[i - 1] !== '\\')) {
        var macroPartial = before.substring(i + 1);
        return /^[a-zA-Z0-9_-]*$/.test(macroPartial) ? { kind: 'macro', partial: macroPartial, triggerPos: i } : null;
      }
      if (ch === '@' && (i === 0 || /\s/.test(before[i - 1]))) {
        var mentionPartial = before.substring(i + 1);
        return /^[a-z0-9_-]*$/.test(mentionPartial) ? { kind: 'mention', partial: mentionPartial, triggerPos: i } : null;
      }
      if (/[\s>]/.test(ch)) return null;
    }
    return null;
  }

  function updateAcHighlight() {
    acPopupEl.querySelectorAll('.ac-item').forEach(function (el, i) {
      el.classList.toggle('active', i === acSelected);
    });
    // Scroll selected item into view
    var active = acPopupEl.querySelector('.ac-item.active');
    if (active) active.scrollIntoView({ block: 'nearest' });
  }

  function updateAcPopup() {
    var ctx = getAcContext();
    if (ctx === null) { hideAcPopup(); return; }
    var items = [];
    if (ctx.kind === 'macro') {
      var merged = getMergedMacros();
      var lc = ctx.partial.toLowerCase();
      items = Object.keys(merged).filter(function (k) {
        return k.indexOf(lc) === 0;
      }).sort().map(function (k) {
        return { kind: 'macro', macroKey: k, displayKey: '<' + k.toUpperCase() + '>', preview: truncate(merged[k], 40) };
      });
    } else {
      var room = rooms.find(function (r) { return r.id === activeRoomID; });
      var members = room ? (room.members || []) : [];
      var lc = ctx.partial.toLowerCase();
      var specials = specialMentionItems.filter(function (item) {
        return !lc || item.token.indexOf(lc) === 0;
      }).map(function (item) {
        return {
          kind: 'mention',
          insertText: '@' + item.token,
          displayKey: '@' + item.token,
          preview: item.preview
        };
      });
      var membersList = members.map(function (memberID) {
        var a = agents.find(function (a) { return a.id === memberID; });
        var name = a ? a.name : memberID.split('@')[0];
        var preview = a ? (a.kind === 'human' ? 'human' : ((a.model || 'unknown') + ' · ' + (a.harness || 'unknown'))) : 'unknown';
        return { kind: 'mention', insertText: '@' + name, displayKey: '@' + name, preview: preview };
      }).filter(function (item) {
        return !lc || item.insertText.slice(1).indexOf(lc) === 0;
      }).sort(function (a, b) { return a.insertText.localeCompare(b.insertText); });
      items = specials.concat(membersList);
    }
    if (items.length === 0) { hideAcPopup(); return; }
    acItems = items;
    acSelected = items.length === 1 ? 0 : -1;
    acPopupEl.innerHTML = items.map(function (item, i) {
      return (
        '<div class="ac-item' + (i === acSelected ? ' active' : '') + '" data-idx="' + i + '">' +
          '<span class="ac-item-key">' + esc(item.displayKey) + '</span>' +
          '<span class="ac-item-preview">' + esc(item.preview) + '</span>' +
        '</div>'
      );
    }).join('');
    acPopupEl.querySelectorAll('.ac-item').forEach(function (el) {
      el.addEventListener('mousedown', function (e) {
        e.preventDefault();
        var idx = parseInt(el.getAttribute('data-idx'), 10);
        insertAcItem(acItems[idx]);
      });
    });
    acPopupEl.classList.remove('hidden');
  }

  function hideAcPopup() {
    acPopupEl.classList.add('hidden');
    acItems = [];
    acSelected = -1;
  }

  function resizeMsgInput() {
    var maxHeight = 160;
    msgBodyInput.style.height = 'auto';
    var scrollHeight = msgBodyInput.scrollHeight;
    var h = Math.min(scrollHeight, maxHeight);
    msgBodyInput.style.height = h + 'px';
    msgBodyInput.style.overflowY = scrollHeight > maxHeight ? 'auto' : 'hidden';
  }

  function insertAcItem(item) {
    var val = msgBodyInput.value;
    var pos = msgBodyInput.selectionStart;
    var before = val.substring(0, pos);
    var triggerChar = item.kind === 'macro' ? '<' : '@';
    var lastTrigger = -1;
    for (var i = before.length - 1; i >= 0; i--) {
      if (before[i] === triggerChar) {
        if (triggerChar === '<' && i > 0 && before[i - 1] === '\\') continue;
        if (triggerChar === '@' && i > 0 && !/\s/.test(before[i - 1])) break;
        lastTrigger = i;
        break;
      }
      if (/[\s>]/.test(before[i])) break;
    }
    if (lastTrigger === -1) { hideAcPopup(); return; }
    var after = val.substring(pos);
    var nextChar = after.charAt(0);
    var needsSpace = !nextChar || (!/\s/.test(nextChar) && !/[,.!?;:)\]\}>]/.test(nextChar));
    var insertText = item.kind === 'macro'
      ? expandMacros('<' + item.macroKey + '>')
      : item.insertText;
    insertText += needsSpace ? ' ' : '';
    var newVal = before.substring(0, lastTrigger) + insertText + after;
    msgBodyInput.value = newVal;
    var newPos = lastTrigger + insertText.length;
    msgBodyInput.setSelectionRange(newPos, newPos);
    resizeMsgInput();
    hideAcPopup();
    msgBodyInput.focus();
  }

  function renderMacrosList() {
    if (!macrosListEl) return;
    var lc = macrosFilter.toLowerCase();
    var keys = Object.keys(macros).filter(function (k) {
      return !lc || k.indexOf(lc) !== -1 || macros[k].toLowerCase().indexOf(lc) !== -1;
    }).sort();
    if (keys.length === 0) {
      macrosListEl.innerHTML = '<div class="empty-state">' +
        (lc ? 'No macros match "' + esc(lc) + '".' : 'No macros defined.') + '</div>';
      return;
    }
    macrosListEl.innerHTML = keys.map(function (k) {
      return (
        '<div class="macro-row">' +
          '<span class="macro-key">&lt;' + esc(k.toUpperCase()) + '&gt;</span>' +
          '<span class="macro-body">' + esc(macros[k]) + '</span>' +
          '<button class="btn btn-sm btn-danger macro-delete-btn" data-key="' + esc(k) + '" type="button">&times;</button>' +
        '</div>'
      );
    }).join('');
    macrosListEl.querySelectorAll('.macro-delete-btn').forEach(function (btn) {
      btn.addEventListener('click', function () {
        delete macros[btn.getAttribute('data-key')];
        renderMacrosList();
        scheduleMacrosSave();
      });
    });
  }

  function scheduleMacrosSave() {
    if (macrosSaveTimer) clearTimeout(macrosSaveTimer);
    macrosSaveTimer = setTimeout(function () {
      api('PUT', '/macros', { macros: macros })
        .catch(function (err) { console.error('Failed to save macros:', err); });
    }, 500);
  }

  // ── Notification sounds ──────────────────────────────────────────

  function setAudioStatus(msg) {
    if (notifAudioStatusEl) notifAudioStatusEl.textContent = msg;
  }

  function currentNotificationSoundID() {
    return serverSettings.notification_sound || 'builtin:chime';
  }

  function notificationSoundVolume() {
    return serverSettings.notification_volume !== undefined ? serverSettings.notification_volume : 70;
  }

  function resolveNotificationSoundURL(soundID) {
    if (!soundID || soundID === 'builtin:chime') return '/sounds-builtin/chime.wav';
    if (soundID.startsWith('builtin:')) {
      var name = soundID.slice('builtin:'.length);
      if (name === 'chime' || name === 'ding' || name === 'beep' || name === 'knock') {
        return '/sounds-builtin/' + encodeURIComponent(name) + '.wav';
      }
      return null;
    }
    if (soundID.startsWith('user:')) {
      return '/api/sounds/' + encodeURIComponent(soundID.slice('user:'.length));
    }
    return null;
  }

  function formatAudioError(err) {
    return err && err.message ? err.message : String(err);
  }

  function updateNotificationAudioStatus() {
    setAudioStatus(notifAudioPrimed ? 'primed — ready' : 'not primed yet');
  }

  function dropNotificationAudio(soundID) {
    var audio = notifAudioCache[soundID];
    if (!audio) return;
    audio.pause();
    audio.removeAttribute('src');
    audio.load();
    delete notifAudioCache[soundID];
  }

  function getNotificationAudio(soundID, forceRefresh) {
    if (forceRefresh) dropNotificationAudio(soundID);
    var audio = notifAudioCache[soundID];
    if (audio) return audio;
    var url = resolveNotificationSoundURL(soundID);
    if (!url) return null;
    audio = new Audio(url);
    audio.preload = 'auto';
    notifAudioCache[soundID] = audio;
    return audio;
  }

  function warmNotificationAudio(soundID, allowMutedPlay, forceRefresh) {
    var audio = getNotificationAudio(soundID, forceRefresh);
    if (!audio) return Promise.reject(new Error('unknown notification sound: ' + soundID));
    audio.load();
    if (!allowMutedPlay) {
      updateNotificationAudioStatus();
      return Promise.resolve(audio);
    }
    audio.pause();
    audio.currentTime = 0;
    audio.muted = true;
    var p = audio.play();
    if (!p || !p.then) {
      audio.pause();
      audio.currentTime = 0;
      audio.muted = false;
      notifAudioPrimed = true;
      setAudioStatus('primed — ready');
      return Promise.resolve(audio);
    }
    return p.then(function () {
      audio.pause();
      audio.currentTime = 0;
      audio.muted = false;
      notifAudioPrimed = true;
      setAudioStatus('primed — ready');
      return audio;
    }).catch(function (err) {
      audio.pause();
      audio.currentTime = 0;
      audio.muted = false;
      throw err;
    });
  }

  function primeNotificationAudio(soundID, forceRefresh) {
    if (notifAudioPriming) return notifAudioPriming;
    notifAudioPriming = warmNotificationAudio(soundID || currentNotificationSoundID(), true, forceRefresh).catch(function (err) {
      var blocked = err && err.name === 'NotAllowedError';
      setAudioStatus(blocked
        ? 'last attempt: blocked — click anywhere to unlock'
        : 'last attempt: error — ' + formatAudioError(err));
      return null;
    }).finally(function () {
      notifAudioPriming = null;
    });
    return notifAudioPriming;
  }

  function playNotificationAudioByID(soundID, vol, allowFallback) {
    var audio = getNotificationAudio(soundID);
    if (!audio) {
      if (allowFallback && soundID !== 'builtin:chime') {
        console.warn('[aimebu] unknown notification sound, falling back to chime:', soundID);
        return playNotificationAudioByID('builtin:chime', vol, false);
      }
      var missing = new Error('unknown notification sound: ' + soundID);
      setAudioStatus('last attempt: error — ' + formatAudioError(missing));
      return Promise.reject(missing);
    }
    audio.pause();
    audio.muted = false;
    audio.currentTime = 0;
    audio.volume = vol / 100;
    var p = audio.play();
    if (!p || !p.then) {
      notifAudioPrimed = true;
      setAudioStatus('last attempt: ok');
      return Promise.resolve();
    }
    return p.then(function () {
      notifAudioPrimed = true;
      setAudioStatus('last attempt: ok');
    }).catch(function (err) {
      var blocked = err && err.name === 'NotAllowedError';
      if (!blocked && allowFallback && soundID !== 'builtin:chime') {
        console.warn('[aimebu] notification sound failed, falling back to chime:', soundID, err);
        dropNotificationAudio(soundID);
        return playNotificationAudioByID('builtin:chime', vol, false);
      }
      setAudioStatus(blocked
        ? 'last attempt: blocked — click anywhere to unlock'
        : 'last attempt: error — ' + formatAudioError(err));
      return Promise.reject(err);
    });
  }

  function playNotificationSound() {
    if (serverSettings.notification_enabled === false) return;
    playNotificationAudioByID(currentNotificationSoundID(), notificationSoundVolume(), true)
      .catch(function () {});
  }

  // ── System notifications (Notification API) ───────────────────────

  function notificationsEnabledInApp() {
    return serverSettings.notification_enabled !== false;
  }

  function senderDisplayName(agentID) {
    var agent = agents.find(function (a) { return a.id === agentID; });
    return agent ? agent.name : String(agentID || 'someone').split('@')[0];
  }

  function hideNotificationHelp() {
    if (notifSysHelpEl) notifSysHelpEl.classList.add('hidden');
  }

  function showNotificationHelp() {
    if (notifSysHelpEl) notifSysHelpEl.classList.remove('hidden');
  }

  function clearNotificationPromptBanner() {
    pendingNotifPrompt = null;
    if (notifPromptBanner) notifPromptBanner.classList.add('hidden');
    if (notifPromptBannerText) notifPromptBannerText.textContent = '';
  }

  function maybeShowPendingNotificationPrompt() {
    if (!pendingNotifPrompt || document.hidden || !activeRoomID || !notifPromptBanner || !notifPromptBannerText) return;
    notifPromptAttempted = true;
    notifPromptBannerText.textContent = pendingNotifPrompt.senderName + ' sent an attention-flagged message. Enable OS notifications so future ones can alert you when aimebu isn\'t focused.';
    notifPromptBanner.classList.remove('hidden');
  }

  function updateSysNotifStatus() {
    if (!notifSysStatusEl || !notifSysBtn) return;
    if (!('Notification' in window)) {
      notifSysStatusEl.textContent = 'not supported';
      notifSysBtn.disabled = true;
      notifSysBtn.textContent = 'Not supported';
      hideNotificationHelp();
      return;
    }
    var perm = Notification.permission;
    notifSysStatusEl.textContent = perm;
    notifSysBtn.textContent = perm === 'granted' ? 'Granted' : (perm === 'denied' ? 'How to enable' : 'Enable notifications');
    notifSysBtn.disabled = perm === 'granted';
    if (perm !== 'denied') hideNotificationHelp();
  }

  function requestSysNotifPermission() {
    if (!('Notification' in window)) {
      updateSysNotifStatus();
      return Promise.resolve('unsupported');
    }
    if (Notification.permission === 'denied') {
      updateSysNotifStatus();
      return Promise.resolve('denied');
    }
    notifPromptAttempted = true;
    return Notification.requestPermission().then(function (perm) {
      updateSysNotifStatus();
      clearNotificationPromptBanner();
      return perm;
    });
  }

  function maybePromptForAttentionNotification(msg) {
    if (!msg || msg.from === agentID) return;
    if (!('Notification' in window)) return;
    if (!notificationsEnabledInApp()) return;
    if (Notification.permission !== 'default') return;
    if (notifPromptAttempted) return;
    if (document.hidden) {
      pendingNotifPrompt = { senderName: senderDisplayName(msg.from) };
      return;
    }
    requestSysNotifPermission().then(function (perm) {
      if (perm === 'default') {
        pendingNotifPrompt = { senderName: senderDisplayName(msg.from) };
        maybeShowPendingNotificationPrompt();
      }
    }).catch(function (err) {
      console.warn('[aimebu] Notification.requestPermission failed:', err);
    });
  }

  function fireSystemNotification(msg, roomID) {
    if (!('Notification' in window) || Notification.permission !== 'granted') return;
    if (document.hasFocus()) return; // only when the aimebu window is not focused
    var roomName = roomID || 'unknown room';
    var bodyText = msg.body ? msg.body.slice(0, 80) : '';
    var note = new Notification('Attention requested in ' + roomName, {
      body: bodyText,
      tag: roomID,
      icon: '/icons/aimebu-192.png',
      silent: false,
    });
    note.onclick = function () {
      window.focus();
      if (roomID) openRoom(roomID);
      note.close();
    };
  }

  function updateSoundSelect() {
    if (!notifSoundSelect) return;
    var current = serverSettings.notification_sound || 'builtin:chime';
    notifSoundSelect.innerHTML = notifSounds.map(function (s) {
      return '<option value="' + esc(s.id) + '"' + (s.id === current ? ' selected' : '') + '>' + esc(s.name) + '</option>';
    }).join('');
  }

  function renderNotifSoundsList() {
    if (!notifSoundsListEl) return;
    var userSounds = notifSounds.filter(function (s) { return s.kind === 'user'; });
    if (userSounds.length === 0) {
      notifSoundsListEl.innerHTML = '<div class="sound-list-empty">No custom sounds uploaded.</div>';
      return;
    }
    notifSoundsListEl.innerHTML = userSounds.map(function (s) {
      var kb = s.size ? Math.round(s.size / 1024) + ' KB' : '';
      return (
        '<div class="sound-row">' +
          '<span class="sound-row-name" title="' + esc(s.name) + '">' + esc(s.name) + '</span>' +
          (kb ? '<span class="sound-row-size">' + esc(kb) + '</span>' : '') +
          '<button class="btn btn-sm btn-danger" data-del-uuid="' + esc(s.id.slice('user:'.length)) + '" type="button">&times;</button>' +
        '</div>'
      );
    }).join('');
    notifSoundsListEl.querySelectorAll('[data-del-uuid]').forEach(function (btn) {
      btn.addEventListener('click', function () { deleteSound(btn.getAttribute('data-del-uuid')); });
    });
  }

  function loadSounds() {
    return api('GET', '/api/sounds').then(function (data) {
      notifSounds = data.sounds || [];
      updateSoundSelect();
      renderNotifSoundsList();
      updateNotificationAudioStatus();
    }).catch(function () {});
  }

  function deleteSound(uuid) {
    fetch('/api/sounds/' + encodeURIComponent(uuid), { method: 'DELETE' })
      .then(function (r) {
        if (!r.ok && r.status !== 404) return;
        var deletedID = 'user:' + uuid;
        var wasSelected = currentNotificationSoundID() === deletedID;
        notifSounds = notifSounds.filter(function (s) { return s.id !== deletedID; });
        dropNotificationAudio(deletedID);
        if (wasSelected) {
          saveSettings({ notification_sound: 'builtin:chime' });
          dropNotificationAudio('builtin:chime');
          if (notifAudioPrimed) {
            primeNotificationAudio('builtin:chime', true);
          } else {
            updateNotificationAudioStatus();
          }
        }
        updateSoundSelect();
        renderNotifSoundsList();
      })
      .catch(function (err) { console.error('delete sound failed:', err); });
  }

  function uploadSound(file) {
    var fd = new FormData();
    fd.append('file', file);
    fetch('/api/sounds', { method: 'POST', body: fd })
      .then(function (r) {
        if (!r.ok) return r.text().then(function (t) { throw new Error(t); });
        return r.json();
      })
      .then(function (entry) {
        notifSounds.push({ id: entry.id, name: entry.name, kind: 'user', size: entry.size });
        updateSoundSelect();
        renderNotifSoundsList();
        if (notifUploadBtn) {
          var orig = notifUploadBtn.textContent;
          notifUploadBtn.textContent = 'Uploaded!';
          setTimeout(function () { notifUploadBtn.textContent = orig; }, 1500);
        }
      })
      .catch(function (err) {
        alert('Upload failed: ' + err.message);
      });
  }

  function applyNotificationSettings() {
    var enabled = serverSettings.notification_enabled !== false;
    if (notifEnabledBtn) notifEnabledBtn.textContent = enabled ? 'Enabled' : 'Disabled';
    var vol = serverSettings.notification_volume !== undefined ? serverSettings.notification_volume : 70;
    if (notifVolumeSlider) notifVolumeSlider.value = vol;
    if (notifVolumeLabel) notifVolumeLabel.textContent = vol + '%';
    updateSoundSelect();
    updateNotificationAudioStatus();
    if (!enabled) clearNotificationPromptBanner();
  }

  function updateTitleAttention() {
    var hasAny = Object.keys(attentionCounts).some(function (r) { return attentionCounts[r] > 0; });
    document.title = hasAny ? '(!) aimebu' : 'aimebu';
  }

  // Schedule a 3-second fade for roomID's attention bell. If the window is
  // currently unfocused, the timer is deferred until the next focus event so
  // the bell stays visible while the user is away.
  function scheduleAttentionFade(roomID) {
    if (attentionTimers[roomID]) {
      clearTimeout(attentionTimers[roomID]);
      delete attentionTimers[roomID];
    }
    function startFade() {
      attentionTimers[roomID] = setTimeout(function () {
        delete attentionTimers[roomID];
        attentionCounts[roomID] = 0;
        updateTitleAttention();
        renderRooms();
      }, 3000);
    }
    if (document.hasFocus()) {
      startFade();
    } else {
      // Remove any pending focus listener for this room before adding a new one
      // to prevent unbounded listener accumulation during idle periods.
      if (attentionFocusListeners[roomID]) {
        window.removeEventListener('focus', attentionFocusListeners[roomID]);
        delete attentionFocusListeners[roomID];
      }
      function onFocus() {
        delete attentionFocusListeners[roomID];
        startFade();
      }
      attentionFocusListeners[roomID] = onFocus;
      window.addEventListener('focus', onFocus, { once: true });
    }
  }

  // Rebuild attentionCounts for all rooms where we have cached messages.
  // Called after message load or read-cursor updates so badges survive page reload
  // and room switches. For rooms with no cached messages the count remains 0.
  // Historical attention badges persist until the user visits that room.
  function recomputeAttentionCounts() {
    Object.keys(messages).forEach(function (roomID) {
      var cursor = readCursors[roomID] || 0;
      var count = 0;
      (messages[roomID] || []).forEach(function (m) {
        if (m.needs_human_attention && m.from !== agentID && m.id > cursor) count++;
      });
      attentionCounts[roomID] = count;
    });
    updateTitleAttention();
  }

  function loadMacros() {
    return api('GET', '/macros')
      .then(function (data) {
        macros = data.macros || {};
        renderMacrosList();
      })
      .catch(function () { macros = {}; renderMacrosList(); });
  }

  // ── Prompts ──────────────────────────────────────────────────────

  var GROUP_LABELS = {
    etiquette: 'Etiquette',
    tool_descriptions: 'Tool Descriptions',
    spawn_prompts: 'Spawn Prompts',
    errors: 'Errors'
  };

  function renderPromptsList() {
    if (!promptsListEl) return;
    if (!promptEntries.length) {
      promptsListEl.innerHTML = '<p class="prompts-empty">No prompts loaded.</p>';
      return;
    }

    var groups = {};
    var groupOrder = [];
    promptEntries.forEach(function (e) {
      if (!groups[e.group]) {
        groups[e.group] = [];
        groupOrder.push(e.group);
      }
      groups[e.group].push(e);
    });

    var html = '';
    groupOrder.forEach(function (g) {
      html += '<div class="prompt-group">';
      html += '<div class="prompt-group-label">' + esc(GROUP_LABELS[g] || g) + '</div>';
      groups[g].forEach(function (e) {
        html += '<div class="prompt-row' + (e.overridden ? ' prompt-overridden' : '') + '" data-key="' + esc(e.key) + '">';
        html += '<div class="prompt-row-header">';
        html += '<span class="prompt-key">' + esc(e.label) + '</span>';
        if (e.overridden) html += '<span class="prompt-modified-badge">Modified</span>';
        if (e.tokens && e.tokens.length) {
          html += '<span class="prompt-tokens">Tokens: ' + e.tokens.map(function (t) { return '<code>' + esc(t) + '</code>'; }).join(', ') + '</span>';
        }
        html += '</div>';
        html += '<div class="prompt-desc">' + esc(e.description) + '</div>';
        html += '<textarea class="prompt-textarea" data-key="' + esc(e.key) + '" rows="5">' + esc(e.body) + '</textarea>';
        html += '<div class="prompt-row-actions">';
        html += '<button class="btn btn-sm btn-primary prompt-save-btn" type="button" data-key="' + esc(e.key) + '">Save</button>';
        if (e.overridden) {
          html += '<button class="btn btn-sm prompt-revert-btn" type="button" data-key="' + esc(e.key) + '">Revert to default</button>';
        }
        html += '</div>';
        html += '</div>';
      });
      html += '</div>';
    });
    promptsListEl.innerHTML = html;

    promptsListEl.querySelectorAll('.prompt-save-btn').forEach(function (btn) {
      btn.addEventListener('click', function () {
        var key = btn.getAttribute('data-key');
        var ta = promptsListEl.querySelector('textarea[data-key="' + key + '"]');
        if (!ta) return;
        api('PUT', '/settings/prompts/' + encodeURIComponent(key), { value: ta.value })
          .then(function () { return loadPrompts(); })
          .catch(function (err) { console.error('save prompt', err); });
      });
    });

    promptsListEl.querySelectorAll('.prompt-revert-btn').forEach(function (btn) {
      btn.addEventListener('click', function () {
        var key = btn.getAttribute('data-key');
        if (!confirm('Revert "' + key + '" to its compiled default?')) return;
        api('DELETE', '/settings/prompts/' + encodeURIComponent(key))
          .then(function () { return loadPrompts(); })
          .catch(function (err) { console.error('revert prompt', err); });
      });
    });
  }

  function loadPrompts() {
    return api('GET', '/settings/prompts')
      .then(function (data) {
        promptEntries = Array.isArray(data) ? data : [];
        renderPromptsList();
      })
      .catch(function () { promptEntries = []; renderPromptsList(); });
  }

  function scrollToMessage(id, triggerEl) {
    var el = messageListEl.querySelector('[data-id="' + id + '"]');
    if (!el) {
      // Message exists (server confirmed) but is older than loaded window
      if (triggerEl) {
        triggerEl.classList.add('msg-ref-error');
        triggerEl.title = 'Message out of view (load older)';
        setTimeout(function () {
          triggerEl.classList.remove('msg-ref-error');
          triggerEl.title = '';
        }, 2000);
      }
      return;
    }
    el.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
    el.classList.add('msg-highlight');
    setTimeout(function () { el.classList.remove('msg-highlight'); }, 1500);
  }

  function jumpToMessage(id, triggerEl) {
    api('GET', '/messages/' + id + '?agent_id=' + encodeURIComponent(agentID))
      .then(function (msg) {
        if (msg.room_id !== activeRoomID) {
          selectRoom(msg.room_id, msg.id);
        } else {
          scrollToMessage(msg.id, triggerEl);
        }
      })
      .catch(function () {
        if (triggerEl) {
          triggerEl.classList.add('msg-ref-error');
          triggerEl.title = 'Message not accessible';
          setTimeout(function () {
            triggerEl.classList.remove('msg-ref-error');
            triggerEl.title = '';
          }, 2000);
        }
      });
  }

  function extractViewerFields(msg) {
    return {
      addressed_to: Array.isArray(msg && msg.addressed_to) ? msg.addressed_to.slice() : [],
      addressed_to_me: !!(msg && msg.addressed_to_me),
      should_respond: !!(msg && msg.should_respond),
    };
  }

  function extractStoredFields(msg) {
    return {
      id: msg.id,
      room_id: msg.room_id,
      from: msg.from,
      from_kind: msg.from_kind || '',
      body: msg.body || '',
      created_at: msg.created_at || '',
      needs_human_attention: !!msg.needs_human_attention,
      targets: Array.isArray(msg.targets) ? msg.targets.slice() : [],
    };
  }

  function shouldShowDebugButton() {
    return !!serverSettings.debug_button_enabled;
  }

  function availableViewerOptions(selectedViewerID) {
    var options = agents.slice().sort(function (a, b) {
      return a.id.localeCompare(b.id);
    });
    if (selectedViewerID && !options.some(function (agent) { return agent.id === selectedViewerID; })) {
      options.unshift({ id: selectedViewerID });
    }
    return options.map(function (agent) {
      return {
        id: agent.id,
        label: agent.id,
      };
    });
  }

  function availableDebugMessages() {
    return (messages[activeRoomID] || []).slice();
  }

  function currentDebugMessage() {
    return findMessageInRoom(activeRoomID, messageDebugState.messageID);
  }

  function currentDebugMessageIndex() {
    var debugMessages = availableDebugMessages();
    for (var i = 0; i < debugMessages.length; i++) {
      if (debugMessages[i].id === messageDebugState.messageID) return i;
    }
    return -1;
  }

  function ensureDebugCache(msg, viewerID) {
    if (!msg) return null;
    var msgKey = String(msg.id);
    if (!messageDebugState.povCacheByMessageID[msgKey]) {
      messageDebugState.povCacheByMessageID[msgKey] = {};
    }
    if (viewerID) {
      messageDebugState.povCacheByMessageID[msgKey][viewerID] = extractViewerFields(msg);
    }
    return messageDebugState.povCacheByMessageID[msgKey];
  }

  function currentDebugViewerFields() {
    var msg = currentDebugMessage();
    if (!msg) return null;
    var cache = ensureDebugCache(msg);
    return cache[messageDebugState.selectedViewerID] || null;
  }

  function debugValueHTML(field, value) {
    if (field === 'body') {
      return '<pre class="chat-msg-debug-pre">' + esc(String(value || '')) + '</pre>';
    }
    if (Array.isArray(value)) {
      return value.length === 0
        ? '<span class="chat-msg-debug-empty">[]</span>'
        : '<code class="chat-msg-debug-code">' + esc(JSON.stringify(value)) + '</code>';
    }
    if (typeof value === 'boolean') {
      return '<span class="chat-msg-debug-bool ' + (value ? 'true' : 'false') + '">' + (value ? 'true' : 'false') + '</span>';
    }
    if (value === null || value === undefined) {
      return '<span class="chat-msg-debug-empty">null</span>';
    }
    if (value === '') {
      return '<code class="chat-msg-debug-code">""</code>';
    }
    return '<code class="chat-msg-debug-code">' + esc(String(value)) + '</code>';
  }

  function debugRowsHTML(fields, order) {
    return order.map(function (field) {
      return (
        '<div class="chat-msg-debug-row">' +
          '<div class="chat-msg-debug-key">' + esc(field) + '</div>' +
          '<div class="chat-msg-debug-value">' + debugValueHTML(field, fields[field]) + '</div>' +
        '</div>'
      );
    }).join('');
  }

  function renderMessageDebugModal() {
    if (!messageDebugModal) return;

    if (!messageDebugState.open) {
      messageDebugModal.classList.add('hidden');
      document.body.style.overflow = settingsModal.classList.contains('hidden') ? '' : 'hidden';
      return;
    }

    var msg = currentDebugMessage();
    var debugMessages = availableDebugMessages();
    var viewerID = messageDebugState.selectedViewerID || agentID;
    var viewerFields = currentDebugViewerFields();

    messageDebugModal.classList.remove('hidden');
    document.body.style.overflow = 'hidden';

    if (messageDebugMessageSelect) {
      messageDebugMessageSelect.innerHTML = debugMessages.map(function (item) {
        return '<option value="' + esc(String(item.id)) + '"' + (item.id === messageDebugState.messageID ? ' selected' : '') + '>#' + esc(String(item.id)) + '</option>';
      }).join('');
    }
    var debugIndex = currentDebugMessageIndex();
    if (messageDebugPrevBtn) messageDebugPrevBtn.disabled = debugIndex <= 0;
    if (messageDebugNextBtn) messageDebugNextBtn.disabled = debugIndex < 0 || debugIndex >= debugMessages.length - 1;

    if (messageDebugViewerSelect) {
      messageDebugViewerSelect.innerHTML = availableViewerOptions(viewerID).map(function (option) {
        return '<option value="' + esc(option.id) + '"' + (option.id === viewerID ? ' selected' : '') + '>' + esc(option.label) + '</option>';
      }).join('');
    }

    if (!msg) {
      if (messageDebugStored) messageDebugStored.innerHTML = '';
      if (messageDebugViewer) messageDebugViewer.innerHTML = '';
      if (messageDebugStatus) {
        messageDebugStatus.textContent = 'The selected message is not available in the active room window.';
        messageDebugStatus.classList.remove('hidden');
      }
      return;
    }

    if (messageDebugStored) {
      messageDebugStored.innerHTML = debugRowsHTML(extractStoredFields(msg), ['id', 'room_id', 'from', 'from_kind', 'body', 'created_at', 'needs_human_attention', 'targets']);
    }

    if (messageDebugViewer) {
      if (viewerFields) {
        messageDebugViewer.innerHTML = debugRowsHTML(viewerFields, ['addressed_to', 'addressed_to_me', 'should_respond']);
      } else if (messageDebugState.loading) {
        messageDebugViewer.innerHTML = '<div class="chat-msg-debug-status">Loading viewer-specific fields...</div>';
      } else {
        messageDebugViewer.innerHTML = '<div class="chat-msg-debug-status">No viewer-specific fields loaded.</div>';
      }
    }

    if (messageDebugStatus) {
      if (messageDebugState.error) {
        messageDebugStatus.textContent = messageDebugState.error;
        messageDebugStatus.classList.remove('hidden');
        messageDebugStatus.classList.add('error');
      } else {
        messageDebugStatus.textContent = '';
        messageDebugStatus.classList.add('hidden');
        messageDebugStatus.classList.remove('error');
      }
    }
  }

  function findMessageInRoom(roomID, messageID) {
    var roomMessages = messages[roomID] || [];
    for (var i = 0; i < roomMessages.length; i++) {
      if (roomMessages[i].id === messageID) return roomMessages[i];
    }
    return null;
  }

  function closeMessageDebugModal() {
    messageDebugState.open = false;
    messageDebugState.messageID = null;
    messageDebugState.selectedViewerID = '';
    messageDebugState.povCacheByMessageID = {};
    messageDebugState.loading = false;
    messageDebugState.error = '';
    renderMessageDebugModal();
  }

  function openMessageDebugModal(messageID) {
    var msg = findMessageInRoom(activeRoomID, messageID);
    if (!msg) return;
    messageDebugState.open = true;
    messageDebugState.messageID = messageID;
    messageDebugState.selectedViewerID = messageDebugState.selectedViewerID || agentID;
    messageDebugState.loading = false;
    messageDebugState.error = '';
    ensureDebugCache(msg, agentID);
    if (!messageDebugState.povCacheByMessageID[String(messageID)][messageDebugState.selectedViewerID]) {
      loadMessageDebugViewer(messageID, messageDebugState.selectedViewerID);
      return;
    }
    renderMessageDebugModal();
  }

  function selectDebugMessage(messageID) {
    var msg = findMessageInRoom(activeRoomID, messageID);
    if (!msg) return;
    messageDebugState.messageID = messageID;
    messageDebugState.loading = false;
    messageDebugState.error = '';
    ensureDebugCache(msg, agentID);
    if (!messageDebugState.povCacheByMessageID[String(messageID)][messageDebugState.selectedViewerID]) {
      loadMessageDebugViewer(messageID, messageDebugState.selectedViewerID);
      return;
    }
    renderMessageDebugModal();
  }

  function stepDebugMessage(delta) {
    var debugMessages = availableDebugMessages();
    var index = currentDebugMessageIndex();
    if (index < 0) return;
    var nextIndex = index + delta;
    if (nextIndex < 0 || nextIndex >= debugMessages.length) return;
    selectDebugMessage(debugMessages[nextIndex].id);
  }

  function loadMessageDebugViewer(messageID, viewerID) {
    if (!viewerID) return;
    messageDebugState.selectedViewerID = viewerID;
    messageDebugState.error = '';
    var msg = findMessageInRoom(activeRoomID, messageID);
    if (!msg) {
      renderMessageDebugModal();
      return;
    }
    var cache = ensureDebugCache(msg, agentID);
    if (cache[viewerID]) {
      messageDebugState.loading = false;
      renderMessageDebugModal();
      return;
    }
    messageDebugState.loading = true;
    renderMessageDebugModal();
    api('GET', '/messages/' + messageID + '?agent_id=' + encodeURIComponent(viewerID))
      .then(function (msg) {
        ensureDebugCache(msg, viewerID);
        messageDebugState.loading = false;
        messageDebugState.error = '';
        if (messageDebugState.selectedViewerID === viewerID && messageDebugState.messageID === messageID) {
          renderMessageDebugModal();
        }
      })
      .catch(function (err) {
        messageDebugState.loading = false;
        messageDebugState.error = err && err.message ? err.message : 'Failed to load message debug info';
        if (messageDebugState.selectedViewerID === viewerID && messageDebugState.messageID === messageID) {
          renderMessageDebugModal();
        }
      });
  }

  function relativeTime(isoString) {
    if (!isoString) return 'never';
    const now = Date.now();
    const then = new Date(isoString).getTime();
    const diff = now - then;
    if (diff < 0) return 'just now';
    const seconds = Math.floor(diff / 1000);
    if (seconds < 5) return 'just now';
    if (seconds < 60) return seconds + 's ago';
    const minutes = Math.floor(seconds / 60);
    if (minutes < 60) return minutes + 'm ago';
    const hours = Math.floor(minutes / 60);
    if (hours < 24) return hours + 'h ago';
    const days = Math.floor(hours / 24);
    if (days < 30) return days + 'd ago';
    return new Date(isoString).toLocaleDateString();
  }

  function agentStatus(lastSeen) {
    if (!lastSeen) return 'offline';
    const diff = Date.now() - new Date(lastSeen).getTime();
    if (diff < 2 * 60 * 1000) return 'active';
    if (diff < 10 * 60 * 1000) return 'stale';
    return 'offline';
  }

  function isDM(roomID) {
    return roomID && roomID.startsWith('dm:');
  }

  function truncate(str, len) {
    if (!str) return '';
    return str.length > len ? str.substring(0, len) + '...' : str;
  }

  function initials(name) {
    if (!name) return '?';
    const parts = name.split(/[-_.\s]+/);
    if (parts.length >= 2) return (parts[0][0] + parts[1][0]).toUpperCase();
    return name.substring(0, 2).toUpperCase();
  }

  // ── Connection status ────────────────────────────────────────────

  function setConnected(connected) {
    if (connected) {
      connectionStatus.className = 'status-indicator connected';
      statusText.textContent = 'Connected';
    } else {
      connectionStatus.className = 'status-indicator disconnected';
      statusText.textContent = 'Disconnected';
    }
  }

  // ── API calls (HTTP — used for mutations only) ──────────────────

  function api(method, path, body) {
    var opts = {
      method: method,
      headers: { 'Content-Type': 'application/json' },
    };
    if (body) opts.body = JSON.stringify(body);
    return fetch(path, opts).then(function (r) {
      if (!r.ok) return r.text().then(function (t) { throw new Error('HTTP ' + r.status + ': ' + t); });
      if (r.status === 204) return null;
      return r.json();
    });
  }

  // registerHuman tells the server this UI user is a human with the current
  // agentID as their name. Idempotent on the server — safe to call on every
  // page load and name change. Required before any room operation (the
  // server rejects join/send from unregistered agents).
  function registerHuman() {
    return api('POST', '/agents', {
      kind: 'human',
      name: agentID,
      project: 'web-ui',
      meta: { via: 'web-ui', protocol: 'fe' },
    }).then(function () {
      registered = true;
    }).catch(function (err) {
      registered = false;
      console.error('register failed:', err);
      // 409: name is in use by an AI — let the user know so they can change it.
      if (String(err).indexOf('409') >= 0 || String(err).indexOf('in use') >= 0) {
        alert('The name "' + agentID + '" is already in use by another agent on the bus. Change your name in the top bar.');
      }
      throw err;
    });
  }

  // ensureRegistered registers the human if not already done for the current
  // agentID. Returns a promise that resolves when the user is safe to use
  // room/message endpoints.
  function ensureRegistered() {
    if (registered) return Promise.resolve();
    return registerHuman();
  }

  function fetchRoomMessages(roomID) {
    return api('GET', '/rooms/' + encodeURIComponent(roomID) + '/messages?limit=100').then(function (data) {
      messages[roomID] = data.messages || [];
      // Reverse so oldest first (API returns newest first)
      messages[roomID].reverse();
      // Store last message preview
      if (messages[roomID].length > 0) {
        var last = messages[roomID][messages[roomID].length - 1];
        lastMessagePreview[roomID] = last.from + ': ' + last.body;
      }
      // Advance watermark so WS reconnect replays of these messages don't ring.
      messages[roomID].forEach(function (m) {
        if (m.id > maxSeenMsgID) maxSeenMsgID = m.id;
      });
      recomputeAttentionCounts();
      renderMessages();
    }).catch(function (err) {
      console.error('Failed to fetch messages for room ' + roomID + ':', err);
    });
  }

  // fetchRoomPresence pulls the current per-member presence snapshot for a
  // room. Called when opening a room so the member list reflects live state
  // immediately — subsequent updates arrive via WS.
  function fetchRoomPresence(roomID) {
    return api('GET', '/rooms/' + encodeURIComponent(roomID)).then(function (data) {
      mergeRoomPresence(roomID, data.members_presence);
      renderRoomAgents();
      renderReadReceipts();
    }).catch(function (err) {
      console.error('Failed to fetch room presence:', err);
    });
  }

  // fetchMyRooms loads this agent's room-view list (with unread counts and
  // cursors) so the sidebar renders accurate badges immediately after login.
  function fetchMyRooms() {
    return api('GET', '/agents/' + encodeURIComponent(agentID) + '/rooms').then(function (data) {
      var list = data.rooms || [];
      unreadCounts = {};
      readCursors = {};
      attentionCounts = {};
      list.forEach(function (r) {
        unreadCounts[r.id] = r.unread_count || 0;
        readCursors[r.id] = r.read_cursor || 0;
        attentionCounts[r.id] = r.attention_unread_count || 0;
      });
      updateTitleAttention();
      renderRooms();
    }).catch(function (err) {
      console.error('Failed to fetch agent rooms:', err);
    });
  }

  // markRead tells the server this agent has read up to the latest message
  // in the given room. Called when the user opens a room. Fire-and-forget —
  // the server will broadcast a read_update WS event to sync other clients.
  function markRead(roomID) {
    if (!roomID || !registered) return;
    return api('POST', '/agents/' + encodeURIComponent(agentID) + '/read', {
      room: roomID,
      message_id: 0, // 0 = current HEAD
    }).then(function (data) {
      unreadCounts[roomID] = 0;
      readCursors[roomID] = data.read_cursor || readCursors[roomID] || 0;
      updateTitleAttention();
      renderRooms();
    }).catch(function (err) {
      console.error('Failed to mark-read:', err);
    });
  }

  function joinRoom(roomID) {
    return ensureRegistered().then(function () {
      return api('POST', '/rooms/' + encodeURIComponent(roomID) + '/join', { agent_id: agentID });
    }).then(function () {
      // Server broadcasts room_update via WS — no need to fetch rooms
      selectRoom(roomID);
    });
  }

  function leaveRoom(roomID) {
    return api('POST', '/rooms/' + encodeURIComponent(roomID) + '/leave', { agent_id: agentID })
      .then(function () {
        if (activeRoomID === roomID) {
          wsUnsubscribeRoom(roomID);
          activeRoomID = null;
          prevSubscribedRoom = null;
          delete messages[roomID];
          showNoRoom();
        }
        // Server broadcasts room_update via WS
      })
      .catch(function (err) {
        console.error('Failed to leave room:', err);
      });
  }

  function getRecallCandidates() {
    var arr = messages[activeRoomID] || [];
    var out = [];
    for (var i = 0; i < arr.length; i++) {
      if (arr[i].from !== agentID) continue;
      var b = arr[i].body;
      if (out.length && out[out.length - 1] === b) continue;
      out.push(b);
    }
    return out.slice(-200);
  }

  function sendMessage(body) {
    if (!activeRoomID) return;
    return ensureRegistered().then(function () {
      return api('POST', '/rooms/' + encodeURIComponent(activeRoomID) + '/send', {
        from: agentID,
        body: body,
      });
    }).catch(function (err) {
      console.error('Failed to send message:', err);
    });
  }

  // ── Settings ─────────────────────────────────────────────────────

  function applyTheme(theme) {
    if (theme === 'light') {
      document.documentElement.setAttribute('data-theme', 'light');
    } else {
      document.documentElement.removeAttribute('data-theme');
    }
    if (themeToggleBtn) {
      themeToggleBtn.textContent = (theme === 'light') ? 'Dark' : 'Light';
    }
  }

  function applyShowSystemEvents(show) {
    broadcastBtn.style.display = show ? '' : 'none';
    if (systemEventsToggleBtn) {
      systemEventsToggleBtn.textContent = show ? 'Hide' : 'Show';
    }
  }

  function applyDebugButtonSetting(enabled) {
    if (debugToggleBtn) {
      debugToggleBtn.textContent = enabled ? 'Enabled' : 'Disabled';
    }
    if (!enabled) closeMessageDebugModal();
    renderMessages();
  }

  function loadSettings() {
    return api('GET', '/settings').then(function (data) {
      serverSettings = data || {};
      // localStorage shadows the server value so the browser keeps its last
      // explicit choice even when the server default changes.
      var localTheme = localStorage.getItem('aimebu.ui.theme');
      applyTheme(localTheme || serverSettings.theme || 'dark');
      // show_system_events: server default is true; false = hidden
      applyShowSystemEvents(serverSettings.show_system_events !== false);
      // agent_id_default: pre-fill topbar only when nothing was stored locally
      if (!agentIDFromStorage && serverSettings.agent_id_default) {
        agentID = serverSettings.agent_id_default;
        agentIDInput.value = agentID;
      }
      if (agentIDDefaultInput) {
        agentIDDefaultInput.value = serverSettings.agent_id_default || '';
      }
      applyDebugButtonSetting(!!serverSettings.debug_button_enabled);
      applyNotificationSettings();
    }).catch(function () {});
  }

  function saveSettings(patch) {
    Object.assign(serverSettings, patch);
    return api('PUT', '/settings', serverSettings).catch(function (err) {
      console.error('Failed to save settings:', err);
    });
  }

  function openSettings(section) {
    settingsModal.classList.remove('hidden');
    if (section) {
      activateSettingsSection(section);
    }
    renderMacrosList();
    loadPrompts();
    document.body.style.overflow = 'hidden';
  }

  function closeSettings() {
    settingsModal.classList.add('hidden');
    document.body.style.overflow = messageDebugState.open ? 'hidden' : '';
  }

  function activateSettingsSection(section) {
    settingsModal.querySelectorAll('.settings-nav-item').forEach(function (el) {
      el.classList.toggle('active', el.getAttribute('data-section') === section);
    });
    settingsModal.querySelectorAll('.settings-section').forEach(function (el) {
      el.classList.toggle('active', el.getAttribute('data-section') === section);
    });
    var titles = { general: 'General', appearance: 'Appearance', debug: 'Debug', notifications: 'Notifications', macros: 'Macros', prompts: 'Prompts', danger: 'Danger Zone' };
    if (settingsSectionTitle) settingsSectionTitle.textContent = titles[section] || section;
  }

  function exportBackup() {
    var payload = { settings: serverSettings, macros: macros };
    var blob = new Blob([JSON.stringify(payload, null, 2)], { type: 'application/json' });
    var url = URL.createObjectURL(blob);
    var a = document.createElement('a');
    a.href = url;
    a.download = 'aimebu-backup.json';
    a.click();
    URL.revokeObjectURL(url);
  }

  function importBackup(file) {
    var reader = new FileReader();
    reader.onload = function (e) {
      var data;
      try { data = JSON.parse(e.target.result); } catch (_) { alert('Invalid JSON file'); return; }
      var promises = [];
      if (data.settings && typeof data.settings === 'object') {
        serverSettings = data.settings;
        promises.push(api('PUT', '/settings', serverSettings));
        var localTheme = localStorage.getItem('aimebu.ui.theme');
        applyTheme(localTheme || serverSettings.theme || 'dark');
        applyShowSystemEvents(serverSettings.show_system_events !== false);
        applyDebugButtonSetting(!!serverSettings.debug_button_enabled);
        if (agentIDDefaultInput) agentIDDefaultInput.value = serverSettings.agent_id_default || '';
      }
      if (data.macros && typeof data.macros === 'object') {
        var validated = validateMacroMap(data.macros);
        var imported = Object.keys(validated.macros).length;
        var skipped = validated.invalid;
        Object.keys(validated.macros).forEach(function (key) {
          macros[key] = validated.macros[key];
        });
        promises.push(api('PUT', '/macros', { macros: macros }));
        renderMacrosList();
        if (backupImportBtn) {
          backupImportBtn.textContent = 'Imported ' + imported + (skipped ? ' / skipped ' + skipped : '');
          setTimeout(function () { backupImportBtn.textContent = 'Import JSON'; }, 2500);
        }
      }
      Promise.all(promises).catch(function (err) { console.error('Import failed:', err); });
    };
    reader.readAsText(file);
  }

  function resetLocalState() {
    rooms = [];
    messages = {};
    agents = [];
    activeRoomID = null;
    prevSubscribedRoom = null;
    lastMessagePreview = {};
    renderRooms();
    showNoRoom();
    renderAllAgents();
    renderRoomAgents();
    closeMessageDebugModal();
  }

  function clearState() {
    return api('DELETE', '/all').then(resetLocalState);
  }

  function clearAll() {
    return api('DELETE', '/all?include_settings=true').then(function () {
      resetLocalState();
      macros = {};
      serverSettings = {};
      attentionCounts = {};
      updateTitleAttention();
      localStorage.removeItem('aimebu.ui.theme');
      applyTheme('dark');
      applyShowSystemEvents(true);
      applyDebugButtonSetting(false);
      applyNotificationSettings();
      renderMacrosList();
      if (agentIDDefaultInput) agentIDDefaultInput.value = '';
    });
  }

  // ── WebSocket ───────────────────────────────────────────────────

  function connectWS() {
    if (ws) {
      ws.close();
      ws = null;
    }

    var proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    ws = new WebSocket(proto + '//' + location.host + '/ws');

    ws.onopen = function () {
      wsReconnectAttempts = 0;
      setConnected(true);
      initComplete = true;

      // Refresh macros in case a macros_updated event was missed during disconnect
      loadMacros().catch(function () {});

      // Identify ourselves so the server can keep our agent alive
      wsSend({ type: 'hello', agent_id: agentID });

      // Re-subscribe to active room if we had one
      if (activeRoomID) {
        wsSend({ type: 'subscribe', rooms: [activeRoomID] });
        prevSubscribedRoom = activeRoomID;
      }
    };

    ws.onmessage = function (event) {
      try {
        var frame = JSON.parse(event.data);
        switch (frame.type) {
          case 'message':
            handleWSMessage(frame.data);
            break;
          case 'room_update':
            handleWSRoomUpdate(frame.data);
            break;
          case 'attention_event':
            handleWSAttentionEvent(frame.data);
            break;
          case 'agent_update':
            handleWSAgentUpdate(frame.data);
            break;
          case 'read_update':
            handleWSReadUpdate(frame.data);
            break;
          case 'presence':
            handleWSPresence(frame.data);
            break;
          case 'macros_updated':
            loadMacros().catch(function () {});
            break;
        }
      } catch (e) {
        console.error('WS parse error:', e);
      }
    };

    ws.onclose = function () {
      setConnected(false);
      ws = null;
      scheduleWSReconnect();
    };

    ws.onerror = function () {
      // onclose will fire after this
    };
  }

  function scheduleWSReconnect() {
    if (wsReconnectTimer) return;
    wsReconnectAttempts++;
    var delay = Math.min(1000 * Math.pow(2, wsReconnectAttempts - 1), 30000);
    wsReconnectTimer = setTimeout(function () {
      wsReconnectTimer = null;
      connectWS();
    }, delay);
  }

  function wsSend(obj) {
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify(obj));
    }
  }

  function wsSubscribeRoom(roomID) {
    if (prevSubscribedRoom && prevSubscribedRoom !== roomID) {
      wsSend({ type: 'unsubscribe', rooms: [prevSubscribedRoom] });
    }
    wsSend({ type: 'subscribe', rooms: [roomID] });
    prevSubscribedRoom = roomID;
  }

  function wsUnsubscribeRoom(roomID) {
    wsSend({ type: 'unsubscribe', rooms: [roomID] });
    if (prevSubscribedRoom === roomID) {
      prevSubscribedRoom = null;
    }
  }

  // ── WS event handlers ──────────────────────────────────────────

  function handleWSMessage(msg) {
    var roomID = msg.room_id;
    if (!roomID) return;

    if (!messages[roomID]) messages[roomID] = [];
    // Deduplicate
    if (messages[roomID].some(function (m) { return m.id === msg.id; })) return;

    // Track whether this id is higher than anything we've seen before (replay guard).
    var isFirstSeen = msg.id > maxSeenMsgID;
    if (msg.id > maxSeenMsgID) maxSeenMsgID = msg.id;

    messages[roomID].push(msg);

    // Update preview
    lastMessagePreview[roomID] = msg.from + ': ' + msg.body;

    // If this is the active room, append it AND keep the cursor in sync so
    // the badge doesn't linger after the user is literally watching it.
    if (roomID === activeRoomID) {
      appendMessage(msg);
      renderReadReceipts();
      markRead(roomID);
    } else {
      // Someone else's message in a room we're not looking at → unread++.
      // Don't badge our own outgoing in an inactive room (another tab/device may have sent it).
      if (msg.from !== agentID) {
        unreadCounts[roomID] = (unreadCounts[roomID] || 0) + 1;
      }
    }

    // Attention notification — active room gets the transient fade; inactive
    // rooms keep their bell until explicitly opened.
    if (msg.needs_human_attention && msg.from !== agentID && initComplete && isFirstSeen) {
      attentionCounts[roomID] = (attentionCounts[roomID] || 0) + 1;
      updateTitleAttention();
      if (roomID === activeRoomID) scheduleAttentionFade(roomID);
      playNotificationSound();
      fireSystemNotification(msg, roomID);
      maybePromptForAttentionNotification(msg);
    }

    // Update room list preview + unread badge
    renderRooms();
  }

  function handleWSReadUpdate(data) {
    if (!data || !data.room) return;
    // The server emits a `presence` event alongside every read_update now,
    // so `presence[roomID][agentID]` will already reflect the new cursor.
    // Still patch readCursors/unread for the current user — unread badges
    // are separate from read-receipt avatars.
    if (data.agent_id === agentID) {
      readCursors[data.room] = data.read_cursor || 0;
      var msgs = messages[data.room] || [];
      var count = 0;
      for (var i = 0; i < msgs.length; i++) {
        if (msgs[i].id > (readCursors[data.room] || 0) && msgs[i].from !== agentID) {
          count++;
        }
      }
      unreadCounts[data.room] = count;
      renderRooms();
    }
    if (data.room === activeRoomID) {
      renderReadReceipts();
    }
  }

  // handleWSPresence records a single agent's {cursor, waiting} for a room
  // (or for the synthetic '*' bucket = agent-wide wait across any room).
  // Re-renders the member list and, if the affected room is visible, its
  // read-receipt markers.
  function handleWSPresence(data) {
    if (!data || !data.agent) return;
    var roomKey = data.room || '*';
    if (!presence[roomKey]) presence[roomKey] = {};
    presence[roomKey][data.agent] = {
      cursor: data.cursor || 0,
      waiting: !!data.waiting,
    };
    renderRoomAgents();
    if (roomKey === activeRoomID || roomKey === '*') {
      renderReadReceipts();
    }
  }

  // mergeRoomPresence ingests the `members_presence` array returned by
  // GET /rooms/{id}. Overwrites local room-scoped presence so stale entries
  // for agents who left don't linger.
  function mergeRoomPresence(roomID, list) {
    if (!roomID) return;
    var bucket = {};
    (list || []).forEach(function (p) {
      bucket[p.agent] = { cursor: p.cursor || 0, waiting: !!p.waiting };
    });
    presence[roomID] = bucket;
  }

  // effectivePresence returns {cursor, waiting} for (roomID, agentID),
  // merging the room-scoped entry with the agent-wide '*' bucket. An
  // agent-wide wait counts as "waiting" for every room the agent is in.
  function effectivePresence(roomID, agentID) {
    var room = (presence[roomID] || {})[agentID] || {};
    var global = (presence['*'] || {})[agentID] || {};
    return {
      cursor: room.cursor || 0,
      waiting: !!(room.waiting || global.waiting),
    };
  }

  // roomHead returns the highest message ID we've seen locally for a room.
  // 0 if we have no messages. Used to decide green vs yellow vs grey.
  function roomHead(roomID) {
    var msgs = messages[roomID] || [];
    return msgs.length > 0 ? msgs[msgs.length - 1].id : 0;
  }

  function handleWSRoomUpdate(data) {
    rooms = data.rooms || [];
    renderRooms();
    updateRoomHeader();
    renderRoomAgents();
    renderMessageDebugModal();
  }

  // Handles server-pushed attention_event: fires sound, bell, and OS notification
  // for needs_attention=true messages in rooms this WS is not subscribed to.
  // The server suppresses attention_event for rooms the WS is already subscribed to,
  // so handleWSMessage handles those and there is no double-counting.
  function handleWSAttentionEvent(data) {
    var roomID = data.room_id;
    var msg = data.message;
    if (!roomID || !msg) return;
    // Mirror the init + replay guards from handleWSMessage to prevent spurious
    // alerts on reconnect and edge-case double-fire on rapid unsubscribe.
    var isFirstSeen = msg.id > maxSeenMsgID;
    if (msg.id > maxSeenMsgID) maxSeenMsgID = msg.id;
    if (msg.from === agentID || !initComplete || !isFirstSeen) return;
    attentionCounts[roomID] = (attentionCounts[roomID] || 0) + 1;
    updateTitleAttention();
    renderRooms();
    playNotificationSound();
    fireSystemNotification(msg, roomID);
    maybePromptForAttentionNotification(msg);
  }

  function handleWSAgentUpdate(data) {
    agents = data.agents || [];
    renderAllAgents();
    renderRoomAgents();
    renderMessageDebugModal();
  }

  // ── Room selection ───────────────────────────────────────────────

  function selectRoom(roomID, scrollToMsgID) {
    if (activeRoomID === roomID) {
      if (scrollToMsgID) scrollToMessage(scrollToMsgID);
      return;
    }
    closeMessageDebugModal();
    activeRoomID = roomID;
    historyIdx = null;
    historyDraft = null;

    // Show room view
    noRoomView.classList.add('hidden');
    roomView.classList.remove('hidden');

    // _system is read-only: hide composer, show notice, hide export (not applicable)
    var isSystem = roomID === '_system';
    sendForm.style.display = isSystem ? 'none' : '';
    systemRoomNotice.style.display = isSystem ? '' : 'none';
    if (exportWrap) exportWrap.style.display = isSystem ? 'none' : '';
    exportMenu.classList.add('hidden');

    // Update header
    updateRoomHeader();

    // Clear and fetch messages via HTTP (one-time load)
    renderMessages();
    fetchRoomMessages(roomID).then(function () {
      if (scrollToMsgID) scrollToMessage(scrollToMsgID);
      else scrollToBottom(true);
      // Clear attention after fetchRoomMessages has rebuilt counts from history
      // so an in-flight markRead update cannot restore the bell for this room.
      attentionCounts[roomID] = 0;
      if (attentionTimers[roomID]) {
        clearTimeout(attentionTimers[roomID]);
        delete attentionTimers[roomID];
      }
      if (attentionFocusListeners[roomID]) {
        window.removeEventListener('focus', attentionFocusListeners[roomID]);
        delete attentionFocusListeners[roomID];
      }
      updateTitleAttention();
      renderRooms();
      // Pull presence snapshot after messages so read-receipt rendering
      // has the head message id available.
      return fetchRoomPresence(roomID);
    });

    // Mark the room read as soon as the user opens it.
    markRead(roomID);

    // Subscribe to room messages via WebSocket
    wsSubscribeRoom(roomID);

    // Update sidebar highlights
    renderRooms();
    renderRoomAgents();

    // On mobile, switch to chat tab
    setMobileTab('chat');
    maybeShowPendingNotificationPrompt();
  }

  function showNoRoom() {
    closeMessageDebugModal();
    activeRoomID = null;
    noRoomView.classList.remove('hidden');
    roomView.classList.add('hidden');
    roomAgentsList.innerHTML = '<div class="empty-state">Select a room to see members.</div>';
    renderRooms();
  }

  function updateRoomHeader() {
    if (!activeRoomID) return;
    var room = rooms.find(function (r) { return r.id === activeRoomID; });
    if (!room) return;

    var dm = isDM(room.id);
    roomIconEl.textContent = dm ? '@' : '#';
    roomIconEl.className = 'room-header-icon' + (dm ? ' dm' : '');
    if (dm) {
      var others = (room.members || []).filter(function (m) { return m !== agentID; });
      roomNameEl.textContent = others.length > 0 ? others[0] : room.id;
    } else {
      roomNameEl.textContent = room.id;
    }
    var members = room.members || [];
    roomMemberCount.textContent = members.length + ' member' + (members.length !== 1 ? 's' : '');

    // Render avatars (max 6)
    var shown = members.slice(0, 6);
    roomMemberAvatars.innerHTML = shown.map(function (m) {
      return '<span class="member-avatar" title="' + esc(m) + '">' + esc(initials(m)) + '</span>';
    }).join('');
    if (members.length > 6) {
      roomMemberAvatars.innerHTML += '<span class="member-avatar" title="' + (members.length - 6) + ' more">+' + (members.length - 6) + '</span>';
    }
  }

  // ── Render rooms ─────────────────────────────────────────────────

  function renderRooms() {
    var channelRooms = rooms.filter(function (r) { return !isDM(r.id) && r.id !== '_system'; });
    var dmRooms = rooms.filter(function (r) { return isDM(r.id); });

    function sortList(list) {
      return list.slice().sort(function (a, b) {
        var ma = (a.members || []).length;
        var mb = (b.members || []).length;
        if (mb !== ma) return mb - ma;
        return a.id.localeCompare(b.id);
      });
    }

    function roomItemHTML(r) {
      var dm = isDM(r.id);
      var isActive = r.id === activeRoomID;
      var members = r.members || [];
      var preview = lastMessagePreview[r.id] || '';
      var unread = unreadCounts[r.id] || 0;
      var hasUnread = unread > 0 && !isActive;
      var attention = attentionCounts[r.id] || 0;
      var displayName = dm
        ? (members.length > 0 ? members.join(' · ') : r.id)
        : r.id;
      var icon = dm ? '@' : '#';
      return (
        '<div class="room-item' +
          (isActive ? ' active' : '') +
          (dm ? ' dm' : '') +
          (hasUnread ? ' has-unread' : '') +
          '" data-room-id="' + esc(r.id) + '">' +
          '<span class="room-item-icon">' + icon + '</span>' +
          '<div class="room-item-info">' +
            '<div class="room-item-top">' +
              '<span class="room-item-name">' + esc(displayName) + '</span>' +
              '<span class="room-item-count">' + members.length + '</span>' +
              (hasUnread ? '<span class="room-item-unread">' + unread + '</span>' : '') +
              (attention > 0 ? '<span class="room-item-attention">🔔 ' + attention + '</span>' : '') +
            '</div>' +
            (preview ? '<div class="room-item-preview">' + esc(truncate(preview, 40)) + '</div>' : '') +
          '</div>' +
        '</div>'
      );
    }

    function attachClickHandlers(container) {
      container.querySelectorAll('.room-item').forEach(function (el) {
        el.addEventListener('click', function () {
          var rid = el.getAttribute('data-room-id');
          var room = rooms.find(function (r) { return r.id === rid; });
          var isMember = room && room.members && room.members.indexOf(agentID) !== -1;
          if (isMember) {
            selectRoom(rid);
          } else {
            joinRoom(rid);
          }
        });
      });
    }

    if (channelRooms.length === 0) {
      roomListEl.innerHTML = '<div class="empty-state">No rooms yet. Join or create one above.</div>';
    } else {
      roomListEl.innerHTML = sortList(channelRooms).map(roomItemHTML).join('');
      attachClickHandlers(roomListEl);
    }

    if (dmRooms.length === 0) {
      dmListEl.innerHTML = '<div class="empty-state">No direct messages yet.</div>';
    } else {
      dmListEl.innerHTML = sortList(dmRooms).map(roomItemHTML).join('');
      attachClickHandlers(dmListEl);
    }
  }

  // ── System events panel ──────────────────────────────────────────

  function renderSystemBadge() {
    broadcastBadge.style.display = systemUnread > 0 ? '' : 'none';
  }

  function renderSystemPanel() {
    if (systemEvents.length === 0) {
      systemEventsListEl.innerHTML = '<div class="system-events-empty">No system events yet.</div>';
      return;
    }
    var toShow = systemEvents.slice(-100).reverse();
    systemEventsListEl.innerHTML = toShow.map(function (m) {
      return '<div class="system-event-row">' +
        '<span class="system-event-body">' + esc(m.body) + '</span>' +
        '<span class="system-event-time" title="' + esc(m.created_at) + '">' + relativeTime(m.created_at) + '</span>' +
        '</div>';
    }).join('');
  }

  function connectSystemSSE() {
    if (systemSSE) { systemSSE.close(); }
    systemSSE = new EventSource('/rooms/_system/firehose');
    systemSSE.onmessage = function (e) {
      try {
        var msg = JSON.parse(e.data);
        if (!msg || !msg.id) return;
        if (systemEvents.some(function (m) { return m.id === msg.id; })) return;
        systemEvents.push(msg);
        if (systemEventsPanel.classList.contains('hidden')) {
          systemUnread++;
          renderSystemBadge();
        } else {
          renderSystemPanel();
        }
      } catch (err) {}
    };
  }

  // ── Render messages ──────────────────────────────────────────────

  function buildHighlightRegex() {
    var room = rooms.find(function (r) { return r.id === activeRoomID; });
    var members = room ? (room.members || []) : [];
    var memberNames = members.map(function (memberID) {
      var a = agents.find(function (a) { return a.id === memberID; });
      return a ? a.name : memberID.split('@')[0];
    }).filter(Boolean);
    var seen = {};
    var names = [];
    memberNames.concat(specialMentionItems.map(function (item) { return item.token; })).forEach(function (name) {
      var key = name.toLowerCase();
      if (seen[key]) return;
      seen[key] = true;
      names.push(key);
    });
    if (names.length === 0) return null;
    var atNames = names.sort(function (a, b) { return b.length - a.length; }).map(escRe);
    return new RegExp('@(' + atNames.join('|') + ')(?![a-z0-9])', 'gi');
  }

  function highlightNames(rootEl) {
    var re = buildHighlightRegex();
    if (!re) return;
    var skipSel = 'code, pre, a, .mention, .raw-code';
    var toReplace = [];
    var walker = document.createTreeWalker(rootEl, NodeFilter.SHOW_TEXT, {
      acceptNode: function (node) {
        return node.parentElement && node.parentElement.closest(skipSel)
          ? NodeFilter.FILTER_REJECT
          : NodeFilter.FILTER_ACCEPT;
      }
    });
    var node;
    while ((node = walker.nextNode())) {
      re.lastIndex = 0;
      if (re.test(node.nodeValue)) toReplace.push(node);
    }
    toReplace.forEach(function (textNode) {
      var frag = document.createDocumentFragment();
      var text = textNode.nodeValue;
      var last = 0;
      re.lastIndex = 0;
      var m;
      while ((m = re.exec(text)) !== null) {
        if (m.index > 0 && /[a-z0-9]/i.test(text[m.index - 1])) continue;
        // \@mention → strip the backslash and render as plain text (no highlight)
        if (m.index > 0 && text[m.index - 1] === '\\') {
          frag.appendChild(document.createTextNode(text.slice(last, m.index - 1)));
          frag.appendChild(document.createTextNode(m[0]));
          last = m.index + m[0].length;
          continue;
        }
        if (m.index > last) frag.appendChild(document.createTextNode(text.slice(last, m.index)));
        var span = document.createElement('span');
        span.className = 'mention';
        span.textContent = m[0];
        frag.appendChild(span);
        last = m.index + m[0].length;
      }
      if (last < text.length) frag.appendChild(document.createTextNode(text.slice(last)));
      textNode.parentNode.replaceChild(frag, textNode);
    });
  }

  function renderMessages() {
    if (!activeRoomID) return;
    var msgs = messages[activeRoomID] || [];

    if (msgs.length === 0) {
      messageListEl.innerHTML = '<div class="empty-state">No messages in this room yet.</div>';
      return;
    }

    var atBottom = (messageListEl.scrollHeight - messageListEl.scrollTop - messageListEl.clientHeight) < 50;
    var prevScrollTop = messageListEl.scrollTop;
    var prevScrollHeight = messageListEl.scrollHeight;

    messageListEl.innerHTML = msgs.map(function (m) {
      return chatMessageHTML(m);
    }).join('');

    messageListEl.querySelectorAll('.chat-msg-body').forEach(function (b) { highlightNames(b); });
    renderReadReceipts();
    if (atBottom) {
      scrollToBottom(true);
    } else {
      messageListEl.scrollTop = prevScrollTop + (messageListEl.scrollHeight - prevScrollHeight);
    }
    renderMessageDebugModal();
  }

  function chatMessageHTML(m) {
    var showDebugButton = shouldShowDebugButton();
    if (m.from_kind === 'system') {
      return '<div class="chat-msg-system" data-id="' + esc(m.id) + '">' +
        '<div class="chat-msg-system-row">' +
          '<span class="chat-msg-system-body">' + esc(m.body) + '</span>' +
          '<span class="chat-msg-time" title="' + esc(m.created_at) + '">' + relativeTime(m.created_at) + '</span>' +
          (showDebugButton ? '<button class="icon-button chat-msg-debug-toggle" type="button" data-msg-id="' + esc(String(m.id)) + '" aria-label="Open debug inspector" title="Open debug inspector"><span class="icon-mask icon-mask-bug" aria-hidden="true"></span></button>' : '') +
        '</div>' +
        '</div>';
    }
    var isSelf = m.from === agentID;
    var fromAgent = agents.find(function (a) { return a.id === m.from; });
    var msgIconSrc = agentIconSrc(fromAgent);
    var msgIconTitle = fromAgent
      ? (fromAgent.kind === 'human' ? 'human' : esc((fromAgent.model || 'unknown') + ' · ' + (fromAgent.harness || 'unknown')))
      : 'unknown';
    var msgIconAlt = fromAgent
      ? (fromAgent.kind === 'human' ? 'human' : esc(fromAgent.harness || 'unknown'))
      : 'unknown';
    return (
      '<div class="chat-msg' + (isSelf ? ' self' : '') + (m.needs_human_attention ? ' needs-attention' : '') + '" data-id="' + esc(m.id) + '">' +
        '<div class="chat-msg-header">' +
          '<span class="chat-msg-from">' +
            '<img src="' + msgIconSrc + '" class="harness-icon chat-msg-icon" alt="' + msgIconAlt + '" title="' + msgIconTitle + '" width="14" height="14">' +
            '<span class="chat-msg-from-name">' + esc(m.from) + '</span>' +
          '</span>' +
          (m.needs_human_attention ? '<span class="chat-msg-attention-icon" title="Needs human attention">🔔</span>' : '') +
          '<span class="chat-msg-id" data-msg-id="' + esc(String(m.id)) + '" title="Click to copy">#' + esc(String(m.id)) + '</span>' +
          (showDebugButton ? '<button class="icon-button chat-msg-debug-toggle" type="button" data-msg-id="' + esc(String(m.id)) + '" aria-label="Open debug inspector" title="Open debug inspector"><span class="icon-mask icon-mask-bug" aria-hidden="true"></span></button>' : '') +
          '<span class="chat-msg-time" title="' + esc(m.created_at) + '">' + relativeTime(m.created_at) + '</span>' +
        '</div>' +
        '<div class="chat-msg-bubble">' +
          '<div class="chat-msg-body' + (markdownMode === 'rendered' ? ' md-rendered' : '') + '">' +
            (markdownMode === 'rendered' ? renderMarkdown(m.body) : renderPlainWithCodeMarkers(m.body)) +
          '</div>' +
        '</div>' +
      '</div>'
    );
  }

  function appendMessage(msg) {
    if (!activeRoomID || msg.room_id !== activeRoomID) return;

    var atBottom = (messageListEl.scrollHeight - messageListEl.scrollTop - messageListEl.clientHeight) < 50;

    // Remove empty state if present
    var empty = messageListEl.querySelector('.empty-state');
    if (empty) empty.remove();

    var temp = document.createElement('div');
    temp.innerHTML = chatMessageHTML(msg);
    var el = temp.firstChild;
    el.classList.add('new-message');
    messageListEl.appendChild(el);
    var msgBody = el.querySelector('.chat-msg-body');
    if (msgBody) highlightNames(msgBody);

    if (atBottom) scrollToBottom(true);
    renderMessageDebugModal();
  }

  function scrollToBottom(force) {
    var nearBottom = (messageListEl.scrollHeight - messageListEl.scrollTop - messageListEl.clientHeight) < 50;
    if (!force && !nearBottom) return;
    requestAnimationFrame(function () {
      messageListEl.scrollTop = messageListEl.scrollHeight;
    });
  }

  // ── Render agents ────────────────────────────────────────────────

  function renderAllAgents() {
    if (agents.length === 0) {
      allAgentsList.innerHTML = '<div class="empty-state">No agents registered.</div>';
      return;
    }

    var sorted = agents.slice().sort(function (a, b) {
      var sa = agentStatus(a.last_seen);
      var sb = agentStatus(b.last_seen);
      var order = { active: 0, stale: 1, offline: 2 };
      if (order[sa] !== order[sb]) return order[sa] - order[sb];
      return new Date(b.last_seen || 0) - new Date(a.last_seen || 0);
    });

    allAgentsList.innerHTML = sorted.map(function (a) {
      return agentCardHTML(a, 'global');
    }).join('');
  }

  function renderRoomAgents() {
    if (!activeRoomID) {
      roomAgentsList.innerHTML = '<div class="empty-state">Select a room to see members.</div>';
      return;
    }

    var room = rooms.find(function (r) { return r.id === activeRoomID; });
    if (!room || !room.members || room.members.length === 0) {
      roomAgentsList.innerHTML = '<div class="empty-state">No members in this room.</div>';
      return;
    }

    roomAgentsList.innerHTML = room.members.map(function (memberID) {
      var agent = agents.find(function (a) { return a.id === memberID; });
      if (agent) {
        return agentCardHTML(agent, 'room');
      }
      var leaveBtn = memberID !== agentID
        ? '<div class="agent-actions"><button class="agent-action-btn agent-leave-btn" data-agent-id="' + esc(memberID) + '" title="Kick ' + esc(memberID) + ' from room" aria-label="Kick ' + esc(memberID) + ' from room">Kick</button></div>'
        : '';
      return (
        '<div class="agent-card">' +
          '<div class="agent-id">' +
            '<span class="agent-online-dot offline"></span>' +
            esc(memberID) +
          '</div>' +
          leaveBtn +
        '</div>'
      );
    }).join('');
  }

  function agentCardHTML(a, context) {
    var status = agentStatus(a.last_seen);
    var meta = a.meta || {};
    var metaKeys = Object.keys(meta);
    var metaTags = metaKeys.map(function (k) {
      return '<span class="meta-tag" title="' + esc(k) + '"><span class="meta-key">' + esc(k) + '</span> ' + esc(meta[k]) + '</span>';
    }).join('');
    // Presence dot: only meaningful inside a specific room, so only render
    // when we're viewing a room AND this agent is a member.
    var presenceTag = '';
    if (activeRoomID) {
      var room = rooms.find(function (r) { return r.id === activeRoomID; });
      var isMember = room && room.members && room.members.indexOf(a.id) !== -1;
      if (isMember) {
        var p = effectivePresence(activeRoomID, a.id);
        var head = roomHead(activeRoomID);
        var cls, title;
        if (p.waiting) {
          cls = 'waiting';
          title = 'Listening live (bus_wait open)';
        } else if (head > 0 && p.cursor < head) {
          cls = 'behind';
          title = 'Behind head (not currently waiting) — cursor at #' + p.cursor + ' of #' + head;
        } else {
          cls = 'idle';
          title = 'Caught up, not waiting';
        }
        presenceTag = '<span class="agent-presence-dot ' + cls + '" title="' + esc(title) + '"></span>';
      }
    }
    var iconSrc = agentIconSrc(a);
    var iconTitle = a.kind === 'human' ? 'human' : esc((a.model || 'unknown') + ' · ' + (a.harness || 'unknown'));
    var iconAlt = a.kind === 'human' ? 'human' : esc(a.harness || 'unknown');
    var iconTag = '<img src="' + iconSrc + '" class="harness-icon" alt="' + iconAlt + '" title="' + iconTitle + '" width="14" height="14">';
    var actionBtns = '';
    if (a.id !== agentID) {
      var dmBtn = '<button class="agent-action-btn agent-dm-btn" data-agent-id="' + esc(a.id) + '" title="Open DM with ' + esc(a.id) + '" aria-label="Open DM with ' + esc(a.id) + '">DM</button>';
      var kickLabel = context === 'room' ? 'Kick' : 'Deregister';
      var kickClass = context === 'room' ? 'agent-leave-btn' : 'agent-deregister-btn';
      var kickAction = context === 'room' ? 'Kick ' + a.id + ' from room' : 'Deregister ' + a.id;
      var kickBtn = '<button class="agent-action-btn ' + kickClass + '" data-agent-id="' + esc(a.id) + '" title="' + esc(kickAction) + '" aria-label="' + esc(kickAction) + '">' + esc(kickLabel) + '</button>';
      actionBtns = '<div class="agent-actions">' + dmBtn + kickBtn + '</div>';
    }
    return (
      '<div class="agent-card">' +
        '<div class="agent-id">' +
          '<span class="agent-online-dot ' + status + '"></span>' +
          presenceTag +
          iconTag +
          esc(a.id) +
        '</div>' +
        (metaTags ? '<div class="agent-meta">' + metaTags + '</div>' : '') +
        '<div class="agent-lastseen">Last seen: ' + relativeTime(a.last_seen) + '</div>' +
        actionBtns +
      '</div>'
    );
  }

  // renderReadReceipts appends a small avatar strip to each chat message
  // in the active room, one per agent whose effective cursor >= msg.id
  // (excluding the message author — sender is implicitly read).
  function renderReadReceipts() {
    if (!activeRoomID) return;
    var msgs = messages[activeRoomID] || [];
    var room = rooms.find(function (r) { return r.id === activeRoomID; });
    var members = (room && room.members) || [];

    // For each member, find the single highest-id message they've read
    // (last msg where msg.id <= cursor and msg.from !== memberID).
    var memberTarget = {};
    members.forEach(function (memberID) {
      var p = effectivePresence(activeRoomID, memberID);
      for (var i = msgs.length - 1; i >= 0; i--) {
        if (msgs[i].id <= p.cursor) {
          memberTarget[memberID] = msgs[i].id;
          break;
        }
      }
    });

    // Invert: msgID → [memberIDs] whose cursor lands on that message.
    var markersByMsg = {};
    Object.keys(memberTarget).forEach(function (memberID) {
      var t = memberTarget[memberID];
      if (!markersByMsg[t]) markersByMsg[t] = [];
      markersByMsg[t].push(memberID);
    });

    // Render: one strip per message, placed only where a cursor lands.
    msgs.forEach(function (m) {
      var el = messageListEl.querySelector('[data-id="' + esc(m.id) + '"]');
      if (!el) return;
      var strip = el.querySelector('.chat-msg-receipts');
      var seenBy = markersByMsg[m.id] || [];
      if (seenBy.length === 0) {
        if (strip) strip.remove();
        return;
      }
      var html = seenBy.map(function (memberID) {
        return '<span class="receipt-avatar" title="Seen by ' + esc(memberID) + '">' + esc(initials(memberID)) + '</span>';
      }).join('');
      if (!strip) {
        strip = document.createElement('div');
        strip.className = 'chat-msg-receipts';
        el.appendChild(strip);
      }
      strip.innerHTML = html;
    });
  }

  // ── Mobile tab handling ──────────────────────────────────────────

  function setMobileTab(tab) {
    document.body.className = 'tab-' + tab;
    mobileTabs.querySelectorAll('.mobile-tab').forEach(function (el) {
      el.classList.toggle('active', el.getAttribute('data-tab') === tab);
    });
  }

  // ── Event handlers ───────────────────────────────────────────────

  // Agent ID — persist to localStorage. On change, re-register under the new
  // name so the user can use the bus without manual re-auth.
  agentIDInput.value = agentID;
  agentIDInput.addEventListener('change', function () {
    var newID = agentIDInput.value.trim();
    if (newID && newID !== agentID) {
      agentID = newID;
      localStorage.setItem('aimebu_agent_id', newID);
      registered = false;
      unreadCounts = {};
      readCursors = {};
      registerHuman().then(function () {
        fetchMyRooms().catch(function () {});
      }).catch(function () {});
    }
  });

  agentIDInput.addEventListener('keydown', function (e) {
    if (e.key === 'Enter') {
      e.preventDefault();
      agentIDInput.blur();
    }
  });

  // Settings modal open/close
  settingsBtn.addEventListener('click', function () { openSettings('general'); });
  settingsCloseBtn.addEventListener('click', closeSettings);
  settingsOverlay.addEventListener('click', closeSettings);
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape' && !messageDebugModal.classList.contains('hidden')) {
      closeMessageDebugModal();
      return;
    }
    if (e.key === 'Escape' && !settingsModal.classList.contains('hidden')) closeSettings();
  });

  // Settings nav
  settingsModal.querySelectorAll('.settings-nav-item').forEach(function (btn) {
    btn.addEventListener('click', function () {
      activateSettingsSection(btn.getAttribute('data-section'));
    });
  });

  // Theme toggle — writes to both localStorage (client-local) and server settings.
  themeToggleBtn.addEventListener('click', function () {
    var next = (serverSettings.theme === 'light') ? 'dark' : 'light';
    localStorage.setItem('aimebu.ui.theme', next);
    saveSettings({ theme: next });
    applyTheme(next);
  });

  // System events visibility toggle
  if (systemEventsToggleBtn) {
    systemEventsToggleBtn.addEventListener('click', function () {
      var current = serverSettings.show_system_events !== false;
      var next = !current;
      saveSettings({ show_system_events: next });
      applyShowSystemEvents(next);
    });
  }

  if (debugToggleBtn) {
    debugToggleBtn.addEventListener('click', function () {
      var next = !serverSettings.debug_button_enabled;
      saveSettings({ debug_button_enabled: next });
      applyDebugButtonSetting(next);
    });
  }

  // Notification settings
  if (notifEnabledBtn) {
    notifEnabledBtn.addEventListener('click', function () {
      var next = serverSettings.notification_enabled === false ? true : false;
      saveSettings({ notification_enabled: next });
      notifEnabledBtn.textContent = next ? 'Enabled' : 'Disabled';
    });
  }

  if (notifSoundSelect) {
    notifSoundSelect.addEventListener('change', function () {
      var next = notifSoundSelect.value;
      var prev = currentNotificationSoundID();
      saveSettings({ notification_sound: next });
      dropNotificationAudio(prev);
      dropNotificationAudio(next);
      if (notifAudioPrimed) {
        primeNotificationAudio(next, true);
      } else {
        updateNotificationAudioStatus();
      }
    });
  }

  if (notifTestBtn) {
    notifTestBtn.addEventListener('click', function () {
      primeNotificationAudio(currentNotificationSoundID(), true).finally(function () {
        playNotificationSound();
      });
      if ('Notification' in window && Notification.permission === 'granted') {
        var n = new Notification('aimebu test notification', { body: 'Test alert — sound + notifications are working', icon: '/icons/aimebu-192.png', tag: 'aimebu-test' });
        setTimeout(function () { n.close(); }, 4000);
      } else {
        requestSysNotifPermission().then(function (perm) {
          if (perm === 'granted') {
            var n = new Notification('aimebu test notification', { body: 'Test alert — sound + notifications are working', icon: '/icons/aimebu-192.png', tag: 'aimebu-test' });
            setTimeout(function () { n.close(); }, 4000);
          }
        });
      }
    });
  }

  if (notifVolumeSlider) {
    notifVolumeSlider.addEventListener('input', function () {
      var v = parseInt(notifVolumeSlider.value, 10);
      if (notifVolumeLabel) notifVolumeLabel.textContent = v + '%';
      saveSettings({ notification_volume: v });
    });
  }

  if (notifUploadBtn) {
    notifUploadBtn.addEventListener('click', function () {
      if (notifUploadFile) notifUploadFile.click();
    });
  }

  if (notifUploadFile) {
    notifUploadFile.addEventListener('change', function () {
      if (notifUploadFile.files.length > 0) {
        uploadSound(notifUploadFile.files[0]);
        notifUploadFile.value = '';
      }
    });
  }

  if (notifSysBtn) {
    notifSysBtn.addEventListener('click', function () {
      if ('Notification' in window && Notification.permission === 'denied') {
        showNotificationHelp();
        return;
      }
      requestSysNotifPermission();
    });
  }

  if (notifSysForceBtn) {
    notifSysForceBtn.addEventListener('click', function () {
      if (!('Notification' in window)) {
        alert('Prompt failed with: Notification API not supported in this browser');
        return;
      }
      Notification.requestPermission().then(function (perm) {
        updateSysNotifStatus();
        clearNotificationPromptBanner();
        alert('Prompt sent successfully — result: ' + perm);
      }).catch(function (err) {
        alert('Prompt failed with: ' + (err && err.message ? err.message : String(err)));
      });
    });
  }

  if (notifSysHelpCloseBtn) {
    notifSysHelpCloseBtn.addEventListener('click', hideNotificationHelp);
  }

  if (notifPromptEnableBtn) {
    notifPromptEnableBtn.addEventListener('click', function () {
      requestSysNotifPermission();
    });
  }

  if (notifPromptDismissBtn) {
    notifPromptDismissBtn.addEventListener('click', clearNotificationPromptBanner);
  }

  if (messageDebugOverlay) {
    messageDebugOverlay.addEventListener('click', closeMessageDebugModal);
  }
  if (messageDebugCloseBtn) {
    messageDebugCloseBtn.addEventListener('click', closeMessageDebugModal);
  }
  if (messageDebugMessageSelect) {
    messageDebugMessageSelect.addEventListener('change', function () {
      selectDebugMessage(parseInt(messageDebugMessageSelect.value, 10));
    });
  }
  if (messageDebugPrevBtn) {
    messageDebugPrevBtn.addEventListener('click', function () {
      stepDebugMessage(-1);
    });
  }
  if (messageDebugNextBtn) {
    messageDebugNextBtn.addEventListener('click', function () {
      stepDebugMessage(1);
    });
  }
  if (messageDebugViewerSelect) {
    messageDebugViewerSelect.addEventListener('change', function () {
      loadMessageDebugViewer(messageDebugState.messageID, messageDebugViewerSelect.value);
    });
  }

  // Prime notification audio on first gesture so later attention pings play immediately.
  function primeNotificationAudioOnGesture() {
    if (notifAudioPrimed) return;
    primeNotificationAudio(currentNotificationSoundID(), false);
  }
  document.addEventListener('pointerdown', primeNotificationAudioOnGesture, { passive: true });
  document.addEventListener('keydown', primeNotificationAudioOnGesture, { passive: true });
  document.addEventListener('visibilitychange', maybeShowPendingNotificationPrompt);
  window.addEventListener('focus', maybeShowPendingNotificationPrompt);

  // Agent ID default — debounced PUT on change
  var agentIDDefaultSaveTimer = null;
  if (agentIDDefaultInput) {
    agentIDDefaultInput.addEventListener('input', function () {
      clearTimeout(agentIDDefaultSaveTimer);
      agentIDDefaultSaveTimer = setTimeout(function () {
        saveSettings({ agent_id_default: agentIDDefaultInput.value.trim() });
      }, 600);
    });
  }

  // Macros search filter
  macrosSearchInput.addEventListener('input', function () {
    macrosFilter = macrosSearchInput.value;
    renderMacrosList();
  });

  // Backup & Sync
  backupExportBtn.addEventListener('click', exportBackup);
  backupImportBtn.addEventListener('click', function () { backupImportFile.click(); });
  backupImportFile.addEventListener('change', function () {
    if (backupImportFile.files.length > 0) {
      importBackup(backupImportFile.files[0]);
      backupImportFile.value = '';
    }
  });

  if (macrosCopyBtn) {
    macrosCopyBtn.addEventListener('click', function () {
      var payload = JSON.stringify(macros, null, 2);
      copyText(payload).then(function () {
        setTemporaryLabel(macrosCopyBtn, 'Copied', 2000);
      }).catch(function (err) {
        console.error('Failed to copy macros JSON:', err);
        setTemporaryLabel(macrosCopyBtn, 'Copy failed', 2500);
      });
    });
  }

  if (macrosImportBtn) {
    macrosImportBtn.addEventListener('click', function () {
      if (!navigator.clipboard || !navigator.clipboard.readText) {
        showMacrosImportFallback();
        setTemporaryLabel(macrosImportBtn, 'Paste below', 2500);
        return;
      }
      navigator.clipboard.readText().then(function (text) {
        if (!text.trim()) throw new Error('Clipboard is empty');
        return applyImportedMacros(text, macrosImportBtn);
      }).then(function (imported) {
        if (imported) hideMacrosImportFallback();
      }).catch(function (err) {
        console.error('Failed to read macros JSON from clipboard:', err);
        showMacrosImportFallback();
        setTemporaryLabel(macrosImportBtn, err.message === 'Clipboard is empty' ? 'Clipboard empty' : 'Paste below', 2500);
      });
    });
  }

  if (macrosImportApplyBtn) {
    macrosImportApplyBtn.addEventListener('click', function () {
      var raw = macrosImportTextarea.value.trim();
      if (!raw) {
        setTemporaryLabel(macrosImportApplyBtn, 'Paste JSON first', 2500);
        return;
      }
      applyImportedMacros(raw, macrosImportApplyBtn).catch(function (err) {
        console.error('Failed to import pasted macros JSON:', err);
        setTemporaryLabel(macrosImportApplyBtn, err.message, 2500);
      });
    });
  }

  if (macrosImportCancelBtn) {
    macrosImportCancelBtn.addEventListener('click', function () {
      hideMacrosImportFallback();
    });
  }

  // Prompts reset-all
  if (promptsResetAllBtn) {
    promptsResetAllBtn.addEventListener('click', function () {
      if (!confirm('Reset all prompt overrides to compiled defaults? This cannot be undone.')) return;
      api('DELETE', '/settings/prompts')
        .then(function () { return loadPrompts(); })
        .catch(function (err) { alert('Error: ' + err.message); });
    });
  }

  // Danger zone
  clearStateBtn.addEventListener('click', function () {
    if (!confirm('Clear all rooms, messages, and agents? Macros and settings are preserved. This cannot be undone.')) return;
    clearState().catch(function (err) { alert('Error: ' + err.message); });
  });

  clearAllBtn.addEventListener('click', function () {
    if (!confirm('Clear everything including macros and settings? This cannot be undone.')) return;
    clearAll().catch(function (err) { alert('Error: ' + err.message); });
  });

  // Join room
  function handleJoinRoom() {
    var roomID = joinRoomInput.value.trim();
    if (!roomID) return;
    joinRoomBtn.disabled = true;
    joinRoom(roomID)
      .then(function () {
        joinRoomInput.value = '';
      })
      .catch(function (err) {
        console.error('Failed to join room:', err);
        alert('Failed to join room: ' + err.message);
      })
      .finally(function () {
        joinRoomBtn.disabled = false;
      });
  }

  joinRoomBtn.addEventListener('click', handleJoinRoom);
  joinRoomInput.addEventListener('keydown', function (e) {
    if (e.key === 'Enter') {
      e.preventDefault();
      handleJoinRoom();
    }
  });

  // Leave current room
  leaveRoomBtn.addEventListener('click', function () {
    if (!activeRoomID) return;
    leaveRoom(activeRoomID);
  });

  // Agent card actions — event delegation on both agent list containers.
  // Handles DM, per-room leave, and global deregister buttons rendered by agentCardHTML.
  [roomAgentsList, allAgentsList].forEach(function (container) {
    container.addEventListener('click', function (e) {
      var dmBtn = e.target.closest('.agent-dm-btn');
      var leaveBtn = e.target.closest('.agent-leave-btn');
      var deregBtn = e.target.closest('.agent-deregister-btn');

      if (dmBtn) {
        var targetID = dmBtn.getAttribute('data-agent-id');
        dmBtn.disabled = true;
        api('POST', '/dm', { from: agentID, to: targetID, body: '' })
          .then(function (data) { selectRoom(data.room); })
          .catch(function (err) { alert('Failed to open DM: ' + err.message); })
          .finally(function () { dmBtn.disabled = false; });

      } else if (leaveBtn) {
        var targetID = leaveBtn.getAttribute('data-agent-id');
        if (!activeRoomID) return;
        if (!confirm('Kick ' + targetID + ' from room "' + activeRoomID + '"?')) return;
        api('POST', '/rooms/' + encodeURIComponent(activeRoomID) + '/leave', { agent_id: targetID, kicked: true })
          .catch(function (err) { alert('Failed to kick agent from room: ' + err.message); });

      } else if (deregBtn) {
        var targetID = deregBtn.getAttribute('data-agent-id');
        if (!confirm('Permanently deregister ' + targetID + ' from all rooms?')) return;
        api('DELETE', '/agents/' + encodeURIComponent(targetID))
          .catch(function (err) { alert('Failed to deregister agent: ' + err.message); });
      }
    });
  });

  // Export room
  exportBtn.addEventListener('click', function (e) {
    e.stopPropagation();
    exportMenu.classList.toggle('hidden');
  });
  exportMenu.addEventListener('click', function (e) {
    var btn = e.target.closest('button[data-format]');
    if (!btn || !activeRoomID) return;
    var format = btn.getAttribute('data-format');
    window.location.href = '/rooms/' + encodeURIComponent(activeRoomID) + '/export?format=' + format + '&agent_id=' + encodeURIComponent(agentID);
    exportMenu.classList.add('hidden');
  });
  document.addEventListener('click', function () {
    exportMenu.classList.add('hidden');
  });

  // Markdown / raw toggle
  mdToggleBtn.addEventListener('click', function () {
    markdownMode = markdownMode === 'rendered' ? 'raw' : 'rendered';
    localStorage.setItem('aimebu.ui.markdownMode', markdownMode);
    updateMdToggleBtn();
    renderMessages();
  });

  // Message ID badge (copy) and #NN autolinks (jump) — event delegation
  messageListEl.addEventListener('click', function (e) {
    var debugToggle = e.target.closest('.chat-msg-debug-toggle');
    if (debugToggle) {
      e.preventDefault();
      openMessageDebugModal(parseInt(debugToggle.getAttribute('data-msg-id'), 10));
      return;
    }
    var badge = e.target.closest('.chat-msg-id');
    if (badge) {
      var msgId = badge.getAttribute('data-msg-id');
      copyText('#' + msgId).then(function () {
        badge.classList.add('copied');
        setTimeout(function () { badge.classList.remove('copied'); }, 800);
      }).catch(function (err) {
        console.error('Failed to copy message reference:', err);
        flashTitleHint(badge, 'Copy failed', 2500);
      });
      return;
    }
    var ref = e.target.closest('.msg-ref');
    if (ref) {
      e.preventDefault();
      jumpToMessage(parseInt(ref.getAttribute('data-msg-id'), 10), ref);
    }
  });

  // Message search bar toggle + submit
  msgSearchBtn.addEventListener('click', function () {
    var hidden = msgSearchBar.classList.toggle('hidden');
    if (!hidden) {
      msgSearchInput.focus();
    }
  });

  msgSearchInput.addEventListener('keydown', function (e) {
    if (e.key === 'Escape') {
      msgSearchBar.classList.add('hidden');
      return;
    }
    if (e.key !== 'Enter') return;
    e.preventDefault();
    var raw = msgSearchInput.value.trim().replace(/^#/, '');
    var id = parseInt(raw, 10);
    if (!id || id <= 0) {
      msgSearchInput.classList.add('error');
      setTimeout(function () { msgSearchInput.classList.remove('error'); }, 800);
      return;
    }
    // Keep bar open until success/failure so the user sees error feedback
    api('GET', '/messages/' + id + '?agent_id=' + encodeURIComponent(agentID))
      .then(function (msg) {
        msgSearchInput.value = '';
        msgSearchBar.classList.add('hidden');
        if (msg.room_id !== activeRoomID) {
          selectRoom(msg.room_id, msg.id);
        } else {
          scrollToMessage(msg.id, null);
        }
      })
      .catch(function () {
        msgSearchInput.classList.add('error');
        setTimeout(function () { msgSearchInput.classList.remove('error'); }, 800);
      });
  });

  // Multiline composer: Enter submits, Shift+Enter inserts newline.
  // IME guard prevents submission mid-composition (CJK / dead-key input).
  // When the autocomplete popup is open, arrow keys / Enter navigate it instead.
  msgBodyInput.addEventListener('keydown', function (e) {
    if (!acPopupEl.classList.contains('hidden') && acItems.length > 0) {
      if (e.key === 'ArrowDown') {
        e.preventDefault();
        acSelected = (acSelected + 1) % acItems.length;
        updateAcHighlight();
        return;
      }
      if (e.key === 'ArrowUp') {
        e.preventDefault();
        acSelected = (acSelected - 1 + acItems.length) % acItems.length;
        updateAcHighlight();
        return;
      }
      if ((e.key === 'Enter' || e.key === 'Tab') && acSelected >= 0) {
        e.preventDefault();
        insertAcItem(acItems[acSelected]);
        return;
      }
      if (e.key === 'Escape') {
        hideAcPopup();
        return;
      }
    }
    // Terminal-style ↑/↓ message history
    if ((e.key === 'ArrowUp' || e.key === 'ArrowDown') &&
        !e.shiftKey && !e.ctrlKey && !e.altKey && !e.metaKey &&
        !e.isComposing) {
      var ss = msgBodyInput.selectionStart, se = msgBodyInput.selectionEnd;
      var len = msgBodyInput.value.length;
      var atStart = ss === 0 && se === 0;
      var atEnd = ss === len && se === len;
      var isEmpty = len === 0;
      if (e.key === 'ArrowUp' && (isEmpty || atStart)) {
        var cands = getRecallCandidates();
        if (cands.length > 0) {
          e.preventDefault();
          if (historyIdx === null) {
            historyDraft = msgBodyInput.value;
            historyIdx = cands.length - 1;
          } else {
            historyIdx = Math.max(historyIdx - 1, 0);
          }
          msgBodyInput.value = cands[historyIdx];
          msgBodyInput.setSelectionRange(msgBodyInput.value.length, msgBodyInput.value.length);
          resizeMsgInput();
          updateAcPopup();
        }
      } else if (e.key === 'ArrowDown' && (isEmpty || atEnd) && historyIdx !== null) {
        var cands = getRecallCandidates();
        e.preventDefault();
        historyIdx++;
        if (historyIdx >= cands.length) {
          msgBodyInput.value = historyDraft !== null ? historyDraft : '';
          historyDraft = null;
          historyIdx = null;
        } else {
          msgBodyInput.value = cands[historyIdx];
        }
        msgBodyInput.setSelectionRange(msgBodyInput.value.length, msgBodyInput.value.length);
        resizeMsgInput();
        updateAcPopup();
      }
    }

    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      sendForm.requestSubmit();
    }
  });

  // Auto-grow textarea (JS fallback for browsers without field-sizing: content).
  msgBodyInput.addEventListener('input', function () {
    resizeMsgInput();
    updateAcPopup();
  });

  msgBodyInput.addEventListener('blur', function () {
    acHideTimer = setTimeout(hideAcPopup, 150);
  });

  msgBodyInput.addEventListener('focus', function () {
    clearTimeout(acHideTimer);
  });

  // Send message
  sendForm.addEventListener('submit', function (e) {
    e.preventDefault();
    var body = msgBodyInput.value.trim();
    if (!body) return;
    historyIdx = null;
    historyDraft = null;
    hideAcPopup();
    sendMessage(body);
    msgBodyInput.value = '';
    msgBodyInput.style.height = '';
    msgBodyInput.style.overflowY = '';
    msgBodyInput.focus();
  });

  // Broadcast / system events panel
  broadcastBtn.addEventListener('click', function (e) {
    e.stopPropagation();
    var isOpen = !systemEventsPanel.classList.contains('hidden');
    systemEventsPanel.classList.toggle('hidden', isOpen);
    if (!isOpen) {
      systemUnread = 0;
      renderSystemBadge();
      renderSystemPanel();
    }
  });

  document.addEventListener('click', function (e) {
    if (!systemEventsPanel.classList.contains('hidden') &&
        !broadcastBtn.contains(e.target) && !systemEventsPanel.contains(e.target)) {
      systemEventsPanel.classList.add('hidden');
    }
  });

  macroAddForm.addEventListener('submit', function (e) {
    e.preventDefault();
    var key = macroKeyInput.value.trim().toLowerCase().replace(/\s+/g, '');
    var body = macroBodyInput.value;
    if (!/^[a-z][a-z0-9_-]*$/.test(key)) {
      macroKeyInput.classList.add('error');
      setTimeout(function () { macroKeyInput.classList.remove('error'); }, 800);
      return;
    }
    if (macros[key] !== undefined) {
      macroKeyInput.classList.add('error');
      setTimeout(function () { macroKeyInput.classList.remove('error'); }, 800);
      return;
    }
    macros[key] = body;
    renderMacrosList();
    scheduleMacrosSave();
    macroKeyInput.value = '';
    macroBodyInput.value = '';
    macroKeyInput.focus();
  });

  // Flush pending macros save on tab close (keepalive lets the request complete
  // after the page unloads — use fetch+keepalive, not sendBeacon, because we
  // need PUT not POST).
  window.addEventListener('beforeunload', function () {
    if (macrosSaveTimer) {
      clearTimeout(macrosSaveTimer);
      macrosSaveTimer = null;
      fetch('/macros', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ macros: macros }),
        keepalive: true,
      });
    }
  });

  // Mobile tabs
  mobileTabs.querySelectorAll('.mobile-tab').forEach(function (tab) {
    tab.addEventListener('click', function () {
      setMobileTab(tab.getAttribute('data-tab'));
    });
  });

  // ── Periodic refresh (timestamps only) ──────────────────────────

  // Update relative timestamps every 30 seconds (purely cosmetic)
  setInterval(function () {
    messageListEl.querySelectorAll('.chat-msg-time').forEach(function (el) {
      el.textContent = relativeTime(el.title);
    });
    renderRooms();
    renderAllAgents();
    renderRoomAgents();
  }, 30000);

  // ── Init ─────────────────────────────────────────────────────────

  setMobileTab('rooms');
  updateMdToggleBtn();

  // If no persisted name yet, try to prefill from the server's $AIMEBU_NAME.
  var prefillPromise;
  if (!agentIDFromStorage) {
    prefillPromise = fetch('/default-name').then(function (r) {
      return r.ok ? r.json() : null;
    }).then(function (data) {
      if (data && data.name) {
        agentID = data.name;
        agentIDInput.value = agentID;
      }
    }).catch(function () {});
  } else {
    prefillPromise = Promise.resolve();
  }

  // Register as a human on the bus before opening the websocket. If
  // registration fails (e.g. name clash with an AI) we still connect —
  // subsequent operations will retry via ensureRegistered and surface the
  // error to the user.
  // Load initial system events (history) and start SSE listener
  api('GET', '/rooms/_system/messages?limit=100').then(function (data) {
    systemEvents = (data.messages || []).slice().reverse();
  }).catch(function () {});
  connectSystemSSE();

  loadSettings();

  api('GET', '/buildinfo').then(function (info) {
    var el = document.getElementById('build-version');
    if (!el || !info) return;
    el.textContent = info.version || '';
    var parts = [info.version, info.go_version].filter(Boolean);
    if (parts.length) el.title = parts.join(' · ');
  }).catch(function () {});

  updateSysNotifStatus();

  loadSounds().catch(function () {});

  prefillPromise.then(function () {
    return registerHuman().catch(function () {});
  }).then(function () {
    fetchMyRooms().catch(function () {});
    loadMacros().catch(function () {});
  }).finally(function () {
    connectWS();
  });
})();
