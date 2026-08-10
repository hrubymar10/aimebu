// aimebu – room-based frontend
// Vanilla JS, no build tools, no frameworks.

(function () {
  'use strict';

  // ── State ────────────────────────────────────────────────────────

  let rooms = [];              // Room[] — all rooms from server
  let activeRoomID = null;     // currently viewed room ID
  let messages = {};           // { roomID: Message[] }
  let agents = [];             // Agent[]
  let agentSessions = {};      // { agentID: AgentSession }
  const storedAgentID = localStorage.getItem('aimebu_agent_id');
  // First-time visitors get 'user' as a placeholder until the welcome gate
  // captures a name. The placeholder must not be registered automatically.
  let agentID = storedAgentID || 'user';
  let agentIDFromStorage = storedAgentID !== null;
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
  let fleets = {};           // { name: { agents: [{ command }] } }
  let promptEntries = [];    // PromptEntry[] from GET /settings/prompts
  let roleEntries = [];      // RoleEntry[] from GET /roles
  let memorySnapshot = { records: [], usage: [], rendered: '' };
  let leaderboardSnapshot = null;
  let selectedLeaderboardKey = null;
  let roomSettingsDirty = false; // true when a focused role picker deferred a rebuild
  let systemEvents = [];     // Message[] — _system room events
  let systemUnread = 0;      // unread count for broadcast panel
  const scrollBottomThreshold = 50;
  let suppressScrollAnchor = false;
  let suppressScrollAnchorToken = 0;
  let messageListResizeObserver = null;
  let scrollAnchor = {
    pinnedToBottom: true,
    anchorEl: null,
    visibleOffset: 0,
    distanceFromBottom: 0,
    listWidth: 0,
    listHeight: 0,
    hasListSize: false,
  };
  let systemSSE = null;      // EventSource for _system room
  let macrosSaveTimer = null;
  let serverSettings = {};   // Settings from GET /settings
  let macrosFilter = '';     // search filter for macros panel
  let initComplete = false;  // true after first WS open — guards notification playback
  let maxSeenMsgID = 0;      // highest message id seen via WS or HTTP — replay guard
  let attentionCounts = {};  // { roomID: number } — needs-attention messages per room
  let answeredProposedAnswers = {}; // { messageID: true } — local v1 double-send guard
  let answeredOpenQuestions = {}; // { messageID: true } — local multi-question send guard
  let openQuestionDrafts = {}; // { messageID: { [questionIndex]: { selector, value } } }
  let openQuestionModalState = { open: false, messageID: null, currentIndex: 0 };
  let reactionControlsHideTimer = null;
  let pendingReply = null; // { roomID, messageID }
  let pendingAttachments = []; // {localID,status,error,attachment,file,url}
  let attentionTimers = {};        // { roomID: timeoutID } — pending 3s fade timers
  let attentionFocusListeners = {}; // { roomID: fn } — pending focus listener per room
  let notifSounds = [];      // sound list from GET /api/sounds
  let notifAudioCache = {};  // { soundID: HTMLAudioElement } — lazily created
  let notifAudioPrimed = false; // true after a trusted gesture warms audio playback
  let notifAudioPriming = null; // in-flight prime promise — dedupes gesture races
  let notifPromptAttempted = false; // flipped only after a real prompt attempt or CTA display
  let pendingNotifPrompt = null; // { senderName } queued while the tab is hidden
  let usageProviders = [];
  let usageSwitcherEnabled = false;
  let usagePercentDisplay = 'left';
  let usageCooldownTimer = null;
  const usageProviderFallbackOrder = ['codex', 'claude-code', 'github-copilot', 'mistral', 'ollama-cloud'];
  let copilotLoginState = { status: 'disconnected', enterpriseHost: '', flowId: '', interval: 5, timer: null, error: '' };
  let ollamaCookieEditorOpen = false;
  let mistralCookieEditorOpen = false;

  let usagesLoadSettled = false; // true once a usages load attempt has settled (success or failure) — first-open loading gate. Set in both branches of loadUsages; renderUsagesSidebar paints a placeholder and returns while it is false, so a success path that forgets it renders nothing forever.
  let usagesLoadFailed = false; // true if the last loadUsages failed — renderUsagesSidebar keeps the error across background re-renders.
  let roomPrefs = {}; // roomID → {hidden: bool, pinned: bool} — per-human view preferences from AgentRoomView
  let switcherInFlight = {}; // tool → true while a switch is in progress
  let messageDebugState = {
    open: false,
    messageID: null,
    selectedViewerID: '',
    povCacheByMessageID: {},
    loading: false,
    error: '',
  };
  let rightSidebarMode = 'members';

  let profileAgentID = '';
  let profileContext = 'room';
  let leftCollapsed = localStorage.getItem('aimebu_left_collapsed') === 'true';
  let rightCollapsed = localStorage.getItem('aimebu_right_collapsed') === 'true';
  let sessionStarted = false;

  let composerAutocomplete = null;
  let welcomeAutocomplete = null;
  let humanNameSuggestions = [];

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
  const roleEmojiChoices = ['👑', '🛠️', '🔎', '🛡️', '🧪', '🎨', '📚', '🧭', '🚦', '🧰', '✅', '⚠️', '💬', '📦'];

  // ── DOM refs ─────────────────────────────────────────────────────

  const $ = (sel) => document.querySelector(sel);
  const welcomeGate = $('#welcome-gate');
  const welcomeForm = $('#welcome-form');
  const welcomeNameInput = $('#welcome-name-input');
  const welcomeSubmitBtn = $('#welcome-submit-btn');
  const welcomeAcPopupEl = $('#welcome-ac-popup');
  const memoryOnboardingModal = $('#memory-onboarding-modal');
  const memoryOnboardingEnableBtn = $('#memory-onboarding-enable-btn');
  const memoryOnboardingDisableBtn = $('#memory-onboarding-disable-btn');
  const agentIDInput = $('#agent-id-input');
  const connectionStatus = $('#connection-status');
  const settingsBtn = $('#settings-btn');
  const settingsModal = $('#settings-modal');
  const settingsOverlay = $('#settings-overlay');
  const settingsCloseBtn = $('#settings-close-btn');
  const settingsSectionTitle = $('#settings-section-title');
  const themeSelect = $('#theme-select');
  const inlinePlanAppendixSelect = $('#inline-plan-appendix-select');
  const debugToggleBtn = $('#debug-toggle-btn');
  const macrosSearchInput = $('#macros-search-input');
  const macrosCopyBtn = $('#macros-copy-btn');
  const macrosImportBtn = $('#macros-import-btn');
  const macrosImportFallback = $('#macros-import-fallback');
  const macrosImportTextarea = $('#macros-import-textarea');
  const macrosImportApplyBtn = $('#macros-import-apply-btn');
  const macrosImportCancelBtn = $('#macros-import-cancel-btn');
  const fleetAddBtn = $('#fleet-add-btn');
  const fleetCopyAllBtn = $('#fleet-copy-all-btn');
  const fleetImportBtn = $('#fleet-import-btn');
  const fleetImportFallback = $('#fleet-import-fallback');
  const fleetImportTextarea = $('#fleet-import-textarea');
  const fleetImportApplyBtn = $('#fleet-import-apply-btn');
  const fleetImportCancelBtn = $('#fleet-import-cancel-btn');
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
  const roomMemberCountNum = $('#room-member-count-num');
  const leaveRoomBtn = $('#leave-room-btn');
  const roomSettingsBtn = $('#room-settings-btn');
  const exportBtn = $('#export-btn');
  const exportMenu = $('#export-menu');
  const exportWrap = exportBtn ? exportBtn.closest('.export-wrap') : null;
  const messageListEl = $('#message-list');
  const sendBar = $('#send-bar');
  const sendForm = $('#send-form');
  const systemRoomNotice = $('#system-room-notice');
  const replyPendingEl = $('#reply-pending');
  const attachmentPendingList = $('#attachment-pending-list');
  const attachmentPickerBtn = $('#attachment-picker-btn');
  const attachmentFileInput = $('#attachment-file-input');
  const msgBodyInput = $('#msg-body');
  const sendButton = sendForm ? sendForm.querySelector('.btn-send') : null;

  const appLayout = $('.app-layout');
  const sidebarLeft = $('.sidebar-left');
  const sidebarRight = $('.sidebar-right');
  const leftSidebarToggle = $('#left-sidebar-toggle');
  const rightSidebarToggle = $('#right-sidebar-toggle');
  const rightSidebarTitle = $('#right-sidebar-title');
  const rightProfilePanel = $('#right-profile-panel');
  const rightUsagesPanel = $('#right-usages-panel');
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
  const openQuestionsModal = $('#open-questions-modal');
  const openQuestionsModalOverlay = $('#open-questions-modal-overlay');
  const openQuestionsModalCloseBtn = $('#open-questions-modal-close-btn');
  const openQuestionsModalTitle = $('#open-questions-modal-title');
  const openQuestionsModalSubtitle = $('#open-questions-modal-subtitle');
  const openQuestionsModalBody = $('#open-questions-modal-body');
  const openQuestionsModalFooter = $('#open-questions-modal-footer');
  const attachmentLightboxModal = $('#attachment-lightbox-modal');
  const attachmentLightboxOverlay = $('#attachment-lightbox-overlay');
  const attachmentLightboxCloseBtn = $('#attachment-lightbox-close-btn');
  const attachmentLightboxTitle = $('#attachment-lightbox-title');
  const attachmentLightboxImg = $('#attachment-lightbox-img');

  const macrosListEl = $('#macros-list');
  const fleetsListEl = $('#fleets-list');
  const promptsListEl = $('#prompts-list');
  const promptsResetAllBtn = $('#prompts-reset-all-btn');
  const rolesListEl = $('#roles-list');
  const rolesResetAllBtn = $('#roles-reset-all-btn');
  const memoryViewerBtn = $('#memory-viewer-btn');
  const memoryViewerModal = $('#memory-viewer-modal');
  const memoryViewerOverlay = $('#memory-viewer-overlay');
  const memoryViewerCloseBtn = $('#memory-viewer-close-btn');
  const memoryGlobalToggleBtn = $('#memory-global-toggle-btn');
  const memoryGlobalStatusEl = $('#memory-global-status');
  const memoryDisabledBanner = $('#memory-disabled-banner');
  const memoryScopeSelect = $('#memory-scope-select');
  const memoryScopeKeyInput = $('#memory-scope-key-input');
  const memoryRefreshBtn = $('#memory-refresh-btn');
  const memoryUsageListEl = $('#memory-usage-list');
  const memoryListEl = $('#memory-list');
  const memoryAddForm = $('#memory-add-form');
  const memoryAddScopeSelect = $('#memory-add-scope-select');
  const memoryAddScopeKeyInput = $('#memory-add-scope-key-input');
  const memoryAddBodyInput = $('#memory-add-body-input');
  const memoryAddSubmitBtn = $('#memory-add-submit-btn');
  const memoryCleanFilterBtn = $('#memory-clean-filter-btn');
  const memoryCleanAllBtn = $('#memory-clean-all-btn');
  const leaderboardViewerBtn = $('#leaderboard-viewer-btn');
  const leaderboardViewerModal = $('#leaderboard-viewer-modal');
  const leaderboardViewerOverlay = $('#leaderboard-viewer-overlay');
  const leaderboardViewerCloseBtn = $('#leaderboard-viewer-close-btn');
  const leaderboardCategorySelect = $('#leaderboard-category-select');
  const leaderboardIncludeSelfToggle = $('#leaderboard-include-self-toggle');
  const leaderboardRefreshBtn = $('#leaderboard-refresh-btn');
  const leaderboardSummaryEl = $('#leaderboard-summary');
  const leaderboardMainEl = $('#leaderboard-main');
  const leaderboardScatterEl = $('#leaderboard-scatter');
  const leaderboardDetailEl = $('#leaderboard-detail');
  const leaderboardModelRollupEl = $('#leaderboard-model-rollup');
  const roleAddForm = $('#role-add-form');
  const roleKeyInput = $('#role-key-input');
  const roleEmojiInput = $('#role-emoji-input');
  const roleDescInput = $('#role-desc-input');
  const roleBodyInput = $('#role-body-input');
  const roomSettingsModal = $('#room-settings-modal');
  const roomSettingsOverlay = $('#room-settings-overlay');
  const roomSettingsCloseBtn = $('#room-settings-close-btn');
  const roomSettingsTitle = $('#room-settings-title');
  const roomSettingsMembers = $('#room-settings-members');
  const roomSettingsRemoveBtn = $('#room-settings-remove-btn');
  const roomSettingsKickAgentsBtn = $('#room-settings-kick-agents-btn');
  const roomSettingsHideBtn = $('#room-settings-hide-btn');
  const roomSettingsPinBtn = $('#room-settings-pin-btn');
  const roomMemorySelect = $('#room-memory-select');
  const roomMemoryStatusEl = $('#room-memory-status');
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
  const usagesBtn = $('#usages-btn');
  const usagesRefreshBtn = $('#usages-refresh-btn');
  const usagesRefreshInput = $('#usages-refresh-input');
  const usagesEnvBadge = $('#usages-env-badge');
  const usagesProviderRows = $('#usages-provider-rows');
  const retentionStaleAgentInput = $('#retention-stale-agent-input');
  const retentionLivenessSweepInput = $('#retention-liveness-sweep-input');
  const retentionAgentStaleInput = $('#retention-agent-stale-input');
  const retentionAgentOfflineInput = $('#retention-agent-offline-input');
  const retentionCleanupIntervalInput = $('#retention-cleanup-interval-input');
  const retentionExpiryInput = $('#retention-expiry-input');

  // ── Harness icons ────────────────────────────────────────────────

  var harnessIconMap = {
    'claude-code': '/icons/claude-code.svg',
    'codex':       '/icons/codex.svg',
    'cursor':      '/icons/cursor.svg',
    'cline':       '/icons/cline.svg',
    'vibe':        '/icons/vibe.svg',
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
  // esc, escAttr, unescHtml, COPY_CODE_ICON, CHECK_CODE_ICON, escRe
  // are defined globally in utils.js (loaded before this file).

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

  // renderMarkdown, renderPlainWithCodeMarkers, hasIndentedPrefix,
  // rewriteIndentedCodeBlocks are defined globally in render-markdown.js.

  // Visual plan renderers (visualPlanHTML, renderVisualPlanBlock, etc.)
  // and Mermaid helpers are defined globally in render-visual-plan.js.

  function updateMdToggleBtn() {
    if (markdownMode === 'rendered') {
      mdToggleBtn.title = 'Switch to raw';
      mdToggleBtn.setAttribute('aria-label', 'Switch to raw');
      mdToggleBtn.classList.remove('raw-mode');
      mdToggleBtn.classList.remove('md-toggle-raw');
      mdToggleBtn.classList.add('md-toggle-rendered');
    } else {
      mdToggleBtn.title = 'Switch to rendered';
      mdToggleBtn.setAttribute('aria-label', 'Switch to rendered');
      mdToggleBtn.classList.add('raw-mode');
      mdToggleBtn.classList.remove('md-toggle-rendered');
      mdToggleBtn.classList.add('md-toggle-raw');
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

  function createAutocomplete(options) {
    var input = options.input;
    var popup = options.popup;
    var items = [];
    var selected = -1;
    var hideTimer = null;

    function updateHighlight() {
      popup.querySelectorAll('.ac-item').forEach(function (el, i) {
        el.classList.toggle('active', i === selected);
      });
      var active = popup.querySelector('.ac-item.active');
      if (active) active.scrollIntoView({ block: 'nearest' });
    }

    function hide() {
      popup.classList.add('hidden');
      items = [];
      selected = -1;
    }

    function select(item) {
      if (!item) return;
      options.onSelect(item);
      hide();
    }

    function update() {
      var next = options.getCandidates() || [];
      if (next.length === 0) { hide(); return; }
      items = next;
      selected = 0;
      popup.innerHTML = items.map(function (item, i) {
        return (
          '<div class="ac-item' + (i === selected ? ' active' : '') + '" data-idx="' + i + '">' +
            '<span class="ac-item-key">' + esc(item.displayKey) + '</span>' +
            '<span class="ac-item-preview">' + esc(item.preview || '') + '</span>' +
          '</div>'
        );
      }).join('');
      popup.querySelectorAll('.ac-item').forEach(function (el) {
        el.addEventListener('mousedown', function (e) {
          e.preventDefault();
          var idx = parseInt(el.getAttribute('data-idx'), 10);
          select(items[idx]);
        });
      });
      popup.classList.remove('hidden');
    }

    function handleKeydown(e) {
      if (popup.classList.contains('hidden') || items.length === 0) return false;
      if (e.key === 'ArrowDown') {
        e.preventDefault();
        selected = (selected + 1) % items.length;
        updateHighlight();
        return true;
      }
      if (e.key === 'ArrowUp') {
        e.preventDefault();
        selected = (selected - 1 + items.length) % items.length;
        updateHighlight();
        return true;
      }
      if ((e.key === 'Enter' || e.key === 'Tab') && selected >= 0) {
        e.preventDefault();
        select(items[selected]);
        return true;
      }
      if (e.key === 'Escape') {
        e.preventDefault();
        hide();
        return true;
      }
      return false;
    }

    input.addEventListener('blur', function () {
      hideTimer = setTimeout(hide, 150);
    });
    input.addEventListener('focus', function () {
      clearTimeout(hideTimer);
      update();
    });

    return {
      hide: hide,
      update: update,
      handleKeydown: handleKeydown
    };
  }

  function getComposerAcItems() {
    var ctx = getAcContext();
    if (ctx === null) return [];
    if (ctx.kind === 'macro') {
      var merged = getMergedMacros();
      var macroLC = ctx.partial.toLowerCase();
      return Object.keys(merged).filter(function (k) {
        return k.indexOf(macroLC) === 0;
      }).sort().map(function (k) {
        return { kind: 'macro', macroKey: k, displayKey: '<' + k.toUpperCase() + '>', preview: truncate(merged[k], 40) };
      });
    }
    var room = rooms.find(function (r) { return r.id === activeRoomID; });
    var members = room ? (room.members || []) : [];
    var mentionLC = ctx.partial.toLowerCase();
    var specials = specialMentionItems.filter(function (item) {
      return !mentionLC || item.token.indexOf(mentionLC) === 0;
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
      return !mentionLC || item.insertText.slice(1).indexOf(mentionLC) === 0;
    }).sort(function (a, b) { return a.insertText.localeCompare(b.insertText); });
    var roleList = assignedRoleMentionItems(room).filter(function (item) {
      return !mentionLC || item.insertText.slice(1).indexOf(mentionLC) === 0;
    });
    return specials.concat(roleList).concat(membersList);
  }

  function updateAcPopup() {
    composerAutocomplete.update();
  }

  function hideAcPopup() {
    composerAutocomplete.hide();
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

  function refreshWelcomeSubmit() {
    if (!welcomeSubmitBtn) return;
    welcomeSubmitBtn.disabled = !welcomeNameInput.value.trim();
  }

  function getWelcomeNameItems() {
    var lc = welcomeNameInput.value.trim().toLowerCase();
    if (!lc) return [];
    return humanNameSuggestions.filter(function (name) {
      return name.toLowerCase().indexOf(lc) === 0;
    }).map(function (name) {
      return { kind: 'human', insertText: name, displayKey: name, preview: 'human' };
    });
  }

  function loadHumanNameSuggestions() {
    return api('GET', '/agents').then(function (data) {
      var seen = {};
      humanNameSuggestions = (data.agents || []).filter(function (agent) {
        return agent.kind === 'human' && agent.name;
      }).map(function (agent) {
        return agent.name;
      }).filter(function (name) {
        var key = name.toLowerCase();
        if (seen[key]) return false;
        seen[key] = true;
        return true;
      }).sort(function (a, b) {
        return a.localeCompare(b);
      });
      if (welcomeAutocomplete) welcomeAutocomplete.update();
    }).catch(function () {
      humanNameSuggestions = [];
    });
  }

  function showWelcomeGate() {
    welcomeGate.classList.remove('hidden');
    refreshWelcomeSubmit();
    setTimeout(function () { welcomeNameInput.focus(); }, 0);
    loadHumanNameSuggestions();
  }

  function hideWelcomeGate() {
    welcomeGate.classList.add('hidden');
    if (welcomeAutocomplete) welcomeAutocomplete.hide();
  }

  composerAutocomplete = createAutocomplete({
    input: msgBodyInput,
    popup: acPopupEl,
    getCandidates: getComposerAcItems,
    onSelect: insertAcItem
  });

  welcomeAutocomplete = createAutocomplete({
    input: welcomeNameInput,
    popup: welcomeAcPopupEl,
    getCandidates: getWelcomeNameItems,
    onSelect: function (item) {
      welcomeNameInput.value = item.insertText;
      refreshWelcomeSubmit();
      welcomeNameInput.focus();
    }
  });

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

  // ── Fleets ──────────────────────────────────────────────────────

  function sortedFleetNames() {
    return Object.keys(fleets || {}).sort();
  }

  function validFleetName(name) {
    return /^[a-z0-9][a-z0-9_.-]{0,63}$/.test(name || '');
  }

  function normalizeFleet(fleet) {
    var agents = Array.isArray(fleet && fleet.agents) ? fleet.agents : [];
    return {
      agents: agents.map(function (agent) {
        return {
          command: String((agent && agent.command) || ''),
          wrap_terminal: true,
          auto_set_cwd: fleetAgentChecked(agent, 'auto_set_cwd')
        };
      }).slice(0, 16)
    };
  }

  function fleetAgentChecked(agent, key) {
    return !agent || agent[key] !== false;
  }

  function fleetPreviewCommand(agent) {
    var command = String((agent && agent.command) || '');
    if (fleetAgentChecked(agent, 'auto_set_cwd')) {
      command = 'cd ${AIMEBU_FLEET_PATH} && ' + command;
    }
    if (fleetAgentChecked(agent, 'wrap_terminal')) {
      command = command.replace(/\\/g, '\\\\').replace(/"/g, '\\"');
      return "osascript -e 'tell application \"Terminal\" to do script \"" + command + "\"'";
    }
    return command;
  }

  function updateFleetPreviewForInput(input) {
    var fleetName = input.getAttribute('data-fleet');
    var idx = parseInt(input.getAttribute('data-index'), 10);
    var row = input.closest('.fleet-agent-row');
    var preview = row ? row.querySelector('.fleet-command-preview-code') : null;
    if (!preview || !fleets[fleetName] || !fleets[fleetName].agents[idx]) return;
    fleets[fleetName].agents[idx].command = input.value;
    preview.textContent = fleetPreviewCommand(fleets[fleetName].agents[idx]);
  }

  function fleetEnvelope(selectedName) {
    var payload = { version: 1, fleets: {} };
    if (selectedName) {
      if (fleets[selectedName]) payload.fleets[selectedName] = normalizeFleet(fleets[selectedName]);
      return payload;
    }
    sortedFleetNames().forEach(function (name) {
      payload.fleets[name] = normalizeFleet(fleets[name]);
    });
    return payload;
  }

  function saveFleets(sourceButton) {
    return api('PUT', '/fleets', fleetEnvelope()).then(function () {
      if (sourceButton) setTemporaryLabel(sourceButton, 'Saved', 1200);
    }).catch(function (err) {
      alert('Failed to save fleets: ' + (err && err.message ? err.message : err));
      return loadFleets();
    });
  }

  function hideFleetImportFallback() {
    if (!fleetImportFallback) return;
    fleetImportFallback.classList.add('hidden');
    if (fleetImportTextarea) fleetImportTextarea.value = '';
  }

  function showFleetImportFallback() {
    if (!fleetImportFallback) return;
    fleetImportFallback.classList.remove('hidden');
    if (fleetImportTextarea) fleetImportTextarea.focus();
  }

  function parseImportedFleets(rawText) {
    var parsed;
    try {
      parsed = JSON.parse(rawText);
    } catch (_) {
      throw new Error('Invalid JSON');
    }
    var payload = parsed && parsed.fleets ? parsed : { version: 1, fleets: parsed };
    if (!payload || !payload.fleets || typeof payload.fleets !== 'object' || Array.isArray(payload.fleets)) {
      throw new Error('Expected a fleets JSON envelope');
    }
    return payload;
  }

  function applyImportedFleets(rawText, sourceButton) {
    var payload = parseImportedFleets(rawText);
    var fleetCount = Object.keys(payload.fleets).length;
    return api('POST', '/fleets/import', payload)
      .then(loadFleets)
      .then(function () {
        hideFleetImportFallback();
        if (sourceButton) setTemporaryLabel(sourceButton, 'Imported ' + fleetCount, 2500);
        return true;
      });
  }

  function renderFleetsList() {
    if (!fleetsListEl) return;
    var names = sortedFleetNames();
    if (names.length === 0) {
      fleetsListEl.innerHTML = '<div class="empty-state">No fleets configured.</div>';
      return;
    }
    fleetsListEl.innerHTML = names.map(function (name) {
      var fleet = fleets[name] || { agents: [] };
      var agents = Array.isArray(fleet.agents) ? fleet.agents : [];
      var rows = agents.map(function (agent, idx) {
        var wrapChecked = fleetAgentChecked(agent, 'wrap_terminal') ? ' checked' : '';
        var cwdChecked = fleetAgentChecked(agent, 'auto_set_cwd') ? ' checked' : '';
        return '<div class="fleet-agent-row" data-fleet="' + esc(name) + '" data-index="' + idx + '">' +
          '<div class="fleet-agent-header">' +
            '<div class="fleet-agent-meta">Agent ' + idx + '</div>' +
            '<button class="btn btn-sm btn-danger fleet-agent-delete-btn" type="button" data-fleet="' + esc(name) + '" data-index="' + idx + '">Remove</button>' +
          '</div>' +
          '<div class="fleet-agent-options">' +
            '<label class="settings-checkbox" title="Wraps the agent command in \'osascript -e &quot;tell application \\&quot;Terminal\\&quot; to do script \\&quot;...\\&quot;&quot;\' so each agent opens its own Terminal.app window. Required in v1."><input type="checkbox" class="fleet-agent-wrap" disabled' + wrapChecked + '> Open in Terminal window <span class="settings-hint-inline">(v1: required)</span></label>' +
            '<label class="settings-checkbox" title="Prepends \'cd ${AIMEBU_FLEET_PATH} &amp;&amp; \' to the agent command so it runs in the target directory. Uncheck if your command already handles its own working directory."><input type="checkbox" class="fleet-agent-cwd"' + cwdChecked + '> Auto-set cwd</label>' +
          '</div>' +
          '<label class="fleet-command-label" title="The command to run inside the Terminal window. Use placeholders ${AIMEBU_FLEET_PATH}, ${AIMEBU_FLEET_NAME}, ${AIMEBU_FLEET_AGENT_INDEX} as needed. Example: aimebu agent --auto-room --assume-role leader -- claude-docker">Agent command:</label>' +
          '<textarea class="fleet-command-input" rows="3" data-fleet="' + esc(name) + '" data-index="' + idx + '" spellcheck="false" title="The command to run inside the Terminal window. Use placeholders ${AIMEBU_FLEET_PATH}, ${AIMEBU_FLEET_NAME}, ${AIMEBU_FLEET_AGENT_INDEX} as needed. Example: aimebu agent --auto-room --assume-role leader -- claude-docker">' + esc(agent.command || '') + '</textarea>' +
          '<div class="fleet-command-preview"><div class="fleet-command-preview-label" title="What aimebu will actually execute via \'sh -c\' after applying the two options above. Placeholders shown unresolved.">Final command:</div><code class="fleet-command-preview-code">' + esc(fleetPreviewCommand(agent)) + '</code></div>' +
        '</div>';
      }).join('');
      return '<div class="fleet-card" data-fleet="' + esc(name) + '">' +
        '<div class="fleet-card-header">' +
          '<input class="fleet-name-input" data-fleet="' + esc(name) + '" value="' + esc(name) + '" spellcheck="false">' +
          '<div class="fleet-card-actions">' +
            '<button class="btn btn-sm fleet-add-agent-btn" type="button" data-fleet="' + esc(name) + '">Add agent</button>' +
            '<button class="btn btn-sm fleet-copy-btn" type="button" data-fleet="' + esc(name) + '">Copy fleet</button>' +
            '<button class="btn btn-sm btn-danger fleet-delete-btn" type="button" data-fleet="' + esc(name) + '">Delete</button>' +
          '</div>' +
        '</div>' +
        '<div class="fleet-help">Use <code>${AIMEBU_FLEET_PATH}</code>, <code>${AIMEBU_FLEET_NAME}</code>, or <code>${AIMEBU_FLEET_AGENT_INDEX}</code>. aimebu replaces these strings before running the command.</div>' +
        '<div class="fleet-agents">' + rows + '</div>' +
      '</div>';
    }).join('');
  }

  function loadFleets() {
    return api('GET', '/fleets')
      .then(function (data) {
        fleets = (data && data.fleets) || {};
        renderFleetsList();
      })
      .catch(function () { fleets = {}; renderFleetsList(); });
  }

  function addFleet() {
    var base = 'default';
    var name = base;
    var i = 2;
    while (fleets[name]) {
      name = base + '-' + i++;
    }
    fleets[name] = { agents: [{ command: '', wrap_terminal: true, auto_set_cwd: true }] };
    renderFleetsList();
  }

  function renameFleet(oldName, newName, input) {
    newName = (newName || '').trim();
    if (newName === oldName) return;
    if (!validFleetName(newName) || fleets[newName]) {
      if (input) input.classList.add('error');
      setTimeout(function () { if (input) input.classList.remove('error'); }, 900);
      if (input) input.value = oldName;
      return;
    }
    fleets[newName] = fleets[oldName];
    delete fleets[oldName];
    renderFleetsList();
    saveFleets();
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

  // ── Memory ──────────────────────────────────────────────────────

  function memoryScopeLabel(scope) {
    if (scope === 'project_facts') return 'Project facts';
    if (scope === 'user_profile') return 'User profile';
    if (scope === 'agent_shared_notes') return 'Shared notes';
    return scope || 'Memory';
  }

  function memorySettingAnswered() {
    return Object.prototype.hasOwnProperty.call(serverSettings, 'memory_enabled');
  }

  function memoryGloballyEnabled() {
    return serverSettings.memory_enabled === true;
  }

  function applyMemorySettings() {
    var enabled = memoryGloballyEnabled();
    if (memoryGlobalToggleBtn) {
      memoryGlobalToggleBtn.textContent = enabled ? 'Enabled' : 'Disabled';
    }
    if (memoryGlobalStatusEl) {
      memoryGlobalStatusEl.textContent = memorySettingAnswered()
        ? (enabled ? 'Memory is enabled.' : 'Memory is disabled. Existing records are preserved.')
        : 'Memory has not been enabled yet.';
    }
    if (memoryDisabledBanner) {
      memoryDisabledBanner.classList.toggle('hidden', enabled);
    }
    if (memoryAddForm) {
      memoryAddForm.classList.toggle('memory-add-disabled', !enabled);
    }
    if (memoryAddSubmitBtn) {
      memoryAddSubmitBtn.disabled = !enabled;
      memoryAddSubmitBtn.textContent = enabled ? 'Add memory' : 'Memory disabled';
    }
    renderRoomSettings();
  }

  function applyLeaderboardSettings() {
    var enabled = serverSettings.leaderboard_enabled !== false;
    if (leaderboardViewerBtn) leaderboardViewerBtn.classList.toggle('hidden', !enabled);
    if (!enabled && leaderboardViewerModal && !leaderboardViewerModal.classList.contains('hidden')) {
      closeLeaderboardViewer();
    }
  }

  function applyAgentsBehaviourSettings() {
    if (inlinePlanAppendixSelect) {
      inlinePlanAppendixSelect.value = serverSettings.inline_plan_appendix || 'always';
    }
  }

  function setMemoryEnabled(enabled) {
    return saveSettings({ memory_enabled: !!enabled }).then(function () {
      applyMemorySettings();
      if (memoryViewerModal && !memoryViewerModal.classList.contains('hidden')) {
        loadMemory();
      }
    });
  }

  function maybeShowMemoryOnboarding() {
    if (!registered || !memoryOnboardingModal || memorySettingAnswered()) return;
    memoryOnboardingModal.classList.remove('hidden');
    updateBodyModalLock();
  }

  function closeMemoryOnboarding() {
    if (!memoryOnboardingModal) return;
    memoryOnboardingModal.classList.add('hidden');
    updateBodyModalLock();
  }

  function openMemoryViewer() {
    if (!memoryViewerModal) return;
    memoryViewerModal.classList.remove('hidden');
    applyMemorySettings();
    loadMemory();
    updateBodyModalLock();
    if (memoryViewerCloseBtn && memoryViewerCloseBtn.focus) memoryViewerCloseBtn.focus();
  }

  function closeMemoryViewer() {
    if (!memoryViewerModal) return;
    memoryViewerModal.classList.add('hidden');
    updateBodyModalLock();
  }

  function memoryQuery() {
    var qs = '?agent_id=' + encodeURIComponent(agentID);
    var scope = memoryScopeSelect ? memoryScopeSelect.value : '';
    var key = memoryScopeKeyInput ? memoryScopeKeyInput.value.trim() : '';
    if (scope === 'project_facts' && !key) return null;
    if (scope) qs += '&scope=' + encodeURIComponent(scope);
    if (key) qs += '&scope_key=' + encodeURIComponent(key);
    return qs;
  }

  function renderMemoryUsage() {
    if (!memoryUsageListEl) return;
    var usage = memorySnapshot.usage || [];
    if (!usage.length) {
      memoryUsageListEl.innerHTML = '';
      return;
    }
    memoryUsageListEl.innerHTML = usage.map(function (u) {
      var pct = u.cap ? Math.min(100, Math.round((u.used / u.cap) * 100)) : 0;
      return '<div class="memory-usage-row">' +
        '<div class="memory-usage-meta"><span>' + esc(memoryScopeLabel(u.scope)) + '</span><code>' + esc(u.key || '') + '</code><span>' + esc(String(u.used || 0)) + '/' + esc(String(u.cap || 0)) + '</span></div>' +
        '<div class="memory-usage-bar"><span style="width:' + pct + '%"></span></div>' +
      '</div>';
    }).join('');
  }

  function renderMemoryList() {
    if (!memoryListEl) return;
    renderMemoryUsage();
    var records = memorySnapshot.records || [];
    if (!records.length) {
      memoryListEl.innerHTML = '<div class="empty-state">No memory records.</div>';
      return;
    }
    memoryListEl.innerHTML = records.map(function (r) {
      return '<div class="memory-row" data-id="' + esc(r.id) + '">' +
        '<div class="memory-row-header">' +
          '<span class="memory-scope-badge">' + esc(memoryScopeLabel(r.scope)) + '</span>' +
          '<code>' + esc(r.scope_key || '') + '</code>' +
          '<span class="memory-meta">v' + esc(String(r.version || 0)) + ' · ' + esc(r.author || '') + '</span>' +
        '</div>' +
        '<textarea class="memory-textarea memory-edit-body" rows="3" data-id="' + esc(r.id) + '" data-version="' + esc(String(r.version || 0)) + '">' + esc(r.body || '') + '</textarea>' +
        '<div class="memory-row-actions">' +
          '<button class="btn btn-sm memory-save-btn" type="button" data-id="' + esc(r.id) + '">Save</button>' +
          '<button class="btn btn-sm btn-danger memory-delete-btn" type="button" data-id="' + esc(r.id) + '" data-version="' + esc(String(r.version || 0)) + '">Delete</button>' +
        '</div>' +
      '</div>';
    }).join('');
    memoryListEl.querySelectorAll('.memory-save-btn').forEach(function (btn) {
      btn.addEventListener('click', function () {
        var id = btn.getAttribute('data-id');
        var textarea = memoryListEl.querySelector('.memory-edit-body[data-id="' + id + '"]');
        if (!textarea) return;
        api('PUT', '/memory/' + encodeURIComponent(id), {
          agent_id: agentID,
          version: parseInt(textarea.getAttribute('data-version'), 10) || 0,
          body: textarea.value
        }).then(loadMemory).catch(function (err) {
          alert('Save failed: ' + (err && err.message ? err.message : err));
          return loadMemory();
        });
      });
    });
    memoryListEl.querySelectorAll('.memory-delete-btn').forEach(function (btn) {
      btn.addEventListener('click', function () {
        var id = btn.getAttribute('data-id');
        var version = btn.getAttribute('data-version');
        if (!confirm('Delete this memory record?')) return;
        api('DELETE', '/memory/' + encodeURIComponent(id) + '?agent_id=' + encodeURIComponent(agentID) + '&version=' + encodeURIComponent(version))
          .then(loadMemory)
          .catch(function (err) {
            alert('Delete failed: ' + (err && err.message ? err.message : err));
            return loadMemory();
          });
      });
    });
  }

  function loadMemory() {
    if (!registered) {
      memorySnapshot = { records: [], usage: [], rendered: '' };
      renderMemoryList();
      return Promise.resolve();
    }
    var query = memoryQuery();
    if (query === null) {
      memorySnapshot = { records: [], usage: [], rendered: '' };
      renderMemoryList();
      if (memoryListEl) memoryListEl.innerHTML = '<div class="empty-state">Enter a project scope key to load project facts.</div>';
      return Promise.resolve();
    }
    return api('GET', '/memory' + query)
      .then(function (data) {
        memorySnapshot = data || { records: [], usage: [], rendered: '' };
        renderMemoryList();
      })
      .catch(function (err) {
        memorySnapshot = { records: [], usage: [], rendered: '' };
        renderMemoryList();
        console.error('Failed to load memory:', err);
      });
  }

  function leaderboardScore(item, category) {
    if (!item) return 0;
    if (!category || category === 'overall') return item.overall || 0;
    return (item.categories && item.categories[category]) || 0;
  }

  function leaderboardCategoryLabel(key) {
    var labels = leaderboardSnapshot && leaderboardSnapshot.summary && leaderboardSnapshot.summary.category_labels;
    return (labels && labels[key]) || {
      overall: 'Overall',
      task_outcome: 'Task Outcome',
      role_execution: 'Role Execution',
      collaboration_process: 'Collaboration & Process',
      judgment_scope: 'Judgment & Scope',
      context_understanding: 'Context Understanding'
    }[key] || key;
  }

  function comboLabel(item) {
    if (!item) return '';
    return item.harness ? (item.model + ' / ' + item.harness) : item.model;
  }

  function renderLeaderboardSummary() {
    if (!leaderboardSummaryEl) return;
    var view = leaderboardSnapshot || {};
    var summary = view.summary || {};
    var best = summary.best_combo;
    var latest = summary.latest_rating_at;
    var topCategory = (leaderboardCategorySelect && leaderboardCategorySelect.value) || 'overall';
    var cards = [
      { label: 'Best', value: best ? comboLabel(best) : 'None', sub: best ? String(leaderboardScore(best, topCategory).toFixed ? leaderboardScore(best, topCategory).toFixed(2) : leaderboardScore(best, topCategory)) : '' },
      { label: 'Combos', value: String(summary.total_combos || 0), sub: 'ranked' },
      { label: 'Cards', value: String(summary.total_cards || 0), sub: 'submitted' },
      { label: 'Latest', value: latest || 'None', sub: String(summary.total_cards || 0) + ' cards' }
    ].map(function (card) {
      return '<div class="leaderboard-summary-card"><span>' + esc(card.label) + '</span><strong>' + esc(card.value) + '</strong><small>' + esc(card.sub || '') + '</small></div>';
    }).join('');
    leaderboardSummaryEl.innerHTML = cards;
  }

  function renderLeaderboardBars(items, category) {
    var maxScore = Math.max(5, items.reduce(function (m, item) { return Math.max(m, leaderboardScore(item, category)); }, 0));
    var rows = items.slice(0, 12).map(function (item, idx) {
      var score = leaderboardScore(item, category);
      var pct = maxScore ? Math.max(0, Math.min(100, (score / maxScore) * 100)) : 0;
      var selected = item.key === selectedLeaderboardKey ? ' selected' : '';
      return '<button class="leaderboard-rank-row' + selected + '" type="button" data-key="' + esc(item.key) + '">' +
        '<span class="leaderboard-rank-num">' + (idx + 1) + '</span>' +
        '<span class="leaderboard-rank-name">' + esc(comboLabel(item)) + '</span>' +
        '<span class="leaderboard-bar-track"><span style="width:' + pct.toFixed(1) + '%"></span></span>' +
        '<span class="leaderboard-score">' + score.toFixed(2) + '</span>' +
        '<span class="leaderboard-muted">' + esc(String(item.cards || 0)) + ' cards</span>' +
      '</button>';
    }).join('');
    return rows || '<div class="empty-state">No submitted leaderboard cards yet.</div>';
  }

  function renderLeaderboardScatter(items, category) {
    if (!leaderboardScatterEl) return;
    if (!items.length) {
      leaderboardScatterEl.innerHTML = '<div class="empty-state">No points yet.</div>';
      return;
    }
    var maxRatings = Math.max(1, items.reduce(function (m, item) { return Math.max(m, item.ratings || 0); }, 0));
    var width = 420;
    var height = 220;
    var points = items.slice(0, 24).map(function (item) {
      var score = leaderboardScore(item, category);
      var x = 32 + (score / 5) * (width - 56);
      var y = height - 30 - ((item.ratings || 0) / maxRatings) * (height - 60);
      var r = item.high_variance ? 6 : 4;
      return '<circle class="leaderboard-point" data-key="' + esc(item.key) + '" cx="' + x.toFixed(1) + '" cy="' + y.toFixed(1) + '" r="' + r + '"><title>' + esc(comboLabel(item) + ' · ' + score.toFixed(2) + ' · ' + (item.ratings || 0) + ' ratings') + '</title></circle>';
    }).join('');
    leaderboardScatterEl.innerHTML = '<svg class="leaderboard-scatter-svg" viewBox="0 0 ' + width + ' ' + height + '" role="img" aria-label="Score by sample size">' +
      '<line x1="32" y1="' + (height - 30) + '" x2="' + (width - 16) + '" y2="' + (height - 30) + '"></line>' +
      '<line x1="32" y1="16" x2="32" y2="' + (height - 30) + '"></line>' +
      '<text x="' + (width - 54) + '" y="' + (height - 8) + '">score</text>' +
      '<text x="8" y="24">n</text>' + points + '</svg>';
  }

  function renderLeaderboardDetail(item) {
    if (!leaderboardDetailEl) return;
    if (!item) {
      leaderboardDetailEl.innerHTML = '<div class="empty-state">Select a combo.</div>';
      return;
    }
    var bars = (leaderboardSnapshot.categories || []).map(function (cat) {
      var score = leaderboardScore(item, cat);
      var pct = Math.max(0, Math.min(100, (score / 5) * 100));
      return '<div class="leaderboard-category-row"><span>' + esc(leaderboardCategoryLabel(cat)) + '</span><div class="leaderboard-bar-track"><span style="width:' + pct.toFixed(1) + '%"></span></div><strong>' + score.toFixed(2) + '</strong></div>';
    }).join('');
    var trend = (item.recent_trend || []).map(function (v) { return v.toFixed(2); }).join(' → ');
    var delta = typeof item.self_delta === 'number' ? item.self_delta : 0;
    var peerOnly = typeof item.peer_only_overall === 'number' ? item.peer_only_overall : item.overall;
    var deltaLabel = (delta > 0 ? '+' : '') + delta.toFixed(2);
    leaderboardDetailEl.innerHTML = '<div class="leaderboard-detail-title">' + esc(comboLabel(item)) + '</div>' +
      bars +
      '<div class="leaderboard-detail-meta">' +
      '<span>' + esc(String(item.cards || 0)) + ' cards</span>' +
      '<span>' + esc(String(item.ratings || 0)) + ' ratings</span>' +
      '<span>' + esc(String(item.variance || 0)) + ' variance</span>' +
      '<span>Peer ' + esc(peerOnly.toFixed(2)) + '</span>' +
      '<span>Self Δ ' + esc(deltaLabel) + '</span>' +
      '</div>' +
      '<div class="leaderboard-trend">' + esc(trend || 'No trend yet') + '</div>';
  }

  function renderLeaderboardRollup(items) {
    if (!leaderboardModelRollupEl) return;
    if (!items.length) {
      leaderboardModelRollupEl.innerHTML = '<div class="empty-state">No model rollups yet.</div>';
      return;
    }
    leaderboardModelRollupEl.innerHTML = items.slice(0, 8).map(function (item, idx) {
      return '<div class="leaderboard-rollup-row"><span>' + (idx + 1) + '. ' + esc(item.model || 'unknown') + '</span><strong>' + (item.overall || 0).toFixed(2) + '</strong><small>' + esc(String(item.cards || 0)) + ' cards</small></div>';
    }).join('');
  }

  function renderLeaderboard() {
    var view = leaderboardSnapshot || { aggregates: [], model_rollups: [], categories: [] };
    var category = (leaderboardCategorySelect && leaderboardCategorySelect.value) || 'overall';
    var items = view.aggregates || [];
    if (!selectedLeaderboardKey && items.length) selectedLeaderboardKey = items[0].key;
    if (selectedLeaderboardKey && !items.some(function (item) { return item.key === selectedLeaderboardKey; })) {
      selectedLeaderboardKey = items.length ? items[0].key : null;
    }
    if (leaderboardMainEl) {
      leaderboardMainEl.innerHTML = renderLeaderboardBars(items, category);
      leaderboardMainEl.querySelectorAll('.leaderboard-rank-row').forEach(function (btn) {
        btn.addEventListener('click', function () {
          selectedLeaderboardKey = btn.getAttribute('data-key');
          renderLeaderboard();
        });
      });
    }
    renderLeaderboardSummary();
    renderLeaderboardScatter(items, category);
    renderLeaderboardDetail(items.find(function (item) { return item.key === selectedLeaderboardKey; }));
    renderLeaderboardRollup(view.model_rollups || []);
  }

  function loadLeaderboard() {
    var category = leaderboardCategorySelect ? leaderboardCategorySelect.value : 'overall';
    var includeSelf = leaderboardIncludeSelfToggle && leaderboardIncludeSelfToggle.checked;
    var qs = '?category=' + encodeURIComponent(category || 'overall') + '&exclude_self=' + encodeURIComponent(includeSelf ? 'false' : 'true');
    return api('GET', '/leaderboard' + qs)
      .then(function (data) {
        leaderboardSnapshot = data || { aggregates: [], model_rollups: [], categories: [] };
        renderLeaderboard();
      })
      .catch(function (err) {
        leaderboardSnapshot = { aggregates: [], model_rollups: [], categories: [] };
        renderLeaderboard();
        console.error('Failed to load leaderboard:', err);
      });
  }

  function openLeaderboardViewer() {
    if (!leaderboardViewerModal) return;
    leaderboardViewerModal.classList.remove('hidden');
    loadLeaderboard();
    updateBodyModalLock();
    if (leaderboardViewerCloseBtn && leaderboardViewerCloseBtn.focus) leaderboardViewerCloseBtn.focus();
  }

  function closeLeaderboardViewer() {
    if (!leaderboardViewerModal) return;
    leaderboardViewerModal.classList.add('hidden');
    updateBodyModalLock();
  }

  function addMemoryRecord() {
    if (!memoryGloballyEnabled()) {
      alert('Memory is disabled.');
      return Promise.resolve();
    }
    var scope = memoryAddScopeSelect ? memoryAddScopeSelect.value : 'project_facts';
    var key = memoryAddScopeKeyInput ? memoryAddScopeKeyInput.value.trim() : '';
    var body = memoryAddBodyInput ? memoryAddBodyInput.value.trim() : '';
    if (scope === 'project_facts' && !key) {
      alert('Project facts require a project scope key.');
      if (memoryAddScopeKeyInput) memoryAddScopeKeyInput.focus();
      return Promise.resolve();
    }
    if (!body) return Promise.resolve();
    return api('POST', '/memory', {
      agent_id: agentID,
      scope: scope,
      scope_key: key,
      body: body
    }).then(function (data) {
      if (memoryAddBodyInput) memoryAddBodyInput.value = '';
      var record = data && data.record;
      if (record) {
        if (memoryScopeSelect) memoryScopeSelect.value = record.scope || scope;
        if (memoryScopeKeyInput) memoryScopeKeyInput.value = record.scope_key || key;
      }
      return loadMemory();
    }).catch(function (err) {
      alert('Add failed: ' + (err && err.message ? err.message : err));
    });
  }

  function cleanMemory(filtered) {
    var qs = '?agent_id=' + encodeURIComponent(agentID);
    if (filtered) {
      var scope = memoryScopeSelect ? memoryScopeSelect.value : '';
      var key = memoryScopeKeyInput ? memoryScopeKeyInput.value.trim() : '';
      if (!scope) {
        alert('Choose a scope before clearing filtered memory.');
        return Promise.resolve();
      }
      if (scope === 'project_facts' && !key) {
        alert('Project facts require a project scope key.');
        if (memoryScopeKeyInput) memoryScopeKeyInput.focus();
        return Promise.resolve();
      }
      qs += '&scope=' + encodeURIComponent(scope);
      if (key) qs += '&scope_key=' + encodeURIComponent(key);
      if (!confirm('Clear filtered memory records? This cannot be undone.')) return Promise.resolve();
    } else if (!confirm('Clear all memory records? This cannot be undone.')) {
      return Promise.resolve();
    }
    return api('DELETE', '/memory' + qs).then(loadMemory).catch(function (err) {
      alert('Clear failed: ' + (err && err.message ? err.message : err));
    });
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

  function roleEntryByKey(key) {
    for (var i = 0; i < roleEntries.length; i++) {
      if (roleEntries[i].key === key) return roleEntries[i];
    }
    return null;
  }

  function rolePayloadObject(e, updates) {
    updates = updates || {};
    return {
      description: updates.description !== undefined ? updates.description : (e.description || ''),
      emoji: updates.emoji !== undefined ? updates.emoji : (e.emoji || e.icon || ''),
      body: updates.body !== undefined ? updates.body : (e.body || ''),
      cardinality: updates.cardinality !== undefined ? updates.cardinality : (e.cardinality || 'multi'),
      extends: updates.extends !== undefined ? updates.extends : (e.extends || '')
    };
  }

  // Build the complete PUT /roles payload that preserves all currently overridden
  // catalog roles and custom roles, replacing changedKey with updates.
  // Pass changedKey=null/updates=null to get just the current preservation map.
  function buildRolesPayload(changedKey, updates) {
    var payload = {};
    roleEntries.forEach(function (e) {
      if (e.key === changedKey || e.overridden || e.is_custom) {
        payload[e.key] = rolePayloadObject(e, e.key === changedKey ? updates : null);
      }
    });
    return payload;
  }

  function roleBadgeHTML(roleKey) {
    if (!roleKey) return '';
    var role = roleEntryByKey(roleKey);
    var emoji = role ? (role.emoji || role.icon || '') : '';
    if (!emoji) return '';
    return '<span class="role-emoji-badge" title="' + esc(roleKey) + '" aria-label="' + esc(roleKey) + '">' + esc(emoji) + '</span>';
  }

  function assignedRoleMentionItems(room) {
    if (!room || !room.roles) return [];
    var reserved = {};
    specialMentionItems.forEach(function (item) {
      reserved[item.token] = true;
    });
    (room.members || []).forEach(function (memberID) {
      var agent = agents.find(function (a) { return a.id === memberID; });
      var name = agent ? agent.name : memberID.split('@')[0];
      if (name) reserved[name.toLowerCase()] = true;
    });
    var byRole = {};
    Object.keys(room.roles).forEach(function (agentIDVal) {
      var roleKey = room.roles[agentIDVal];
      if (!roleKey) return;
      if (reserved[roleKey.toLowerCase()]) return;
      var agent = agents.find(function (a) { return a.id === agentIDVal; });
      if (!agent || agent.kind !== 'ai') return;
      if (!byRole[roleKey]) byRole[roleKey] = [];
      byRole[roleKey].push(agent.name || agentIDVal.split('@')[0]);
    });
    return Object.keys(byRole).sort().map(function (roleKey) {
      var role = roleEntryByKey(roleKey);
      var emoji = role && (role.emoji || role.icon) ? (role.emoji || role.icon) + ' ' : '';
      return {
        kind: 'mention',
        insertText: '@' + roleKey,
        displayKey: '@' + roleKey,
        preview: emoji + roleKey + ' -> ' + byRole[roleKey].join(', ')
      };
    });
  }

  function roleEmojiPickerHTML(key, current) {
    return '<div class="role-emoji-picker" data-key="' + esc(key) + '">' +
      roleEmojiChoices.map(function (emoji) {
        return '<button class="role-emoji-choice' + (emoji === current ? ' active' : '') + '" type="button" data-key="' + esc(key) + '" data-emoji="' + esc(emoji) + '">' + esc(emoji) + '</button>';
      }).join('') +
    '</div>';
  }

  function roleHolderInRoom(room, roleKey) {
    if (!room || !room.roles || !roleKey) return '';
    var holders = Object.keys(room.roles);
    for (var i = 0; i < holders.length; i++) {
      if (room.roles[holders[i]] === roleKey) return holders[i];
    }
    return '';
  }

  function agentDisplayName(agentIDVal) {
    var agent = agents.find(function (a) { return a.id === agentIDVal; });
    return agent ? (agent.name || agent.id) : agentIDVal;
  }

  function agentSlug(agentIDVal) {
    var agent = agentByID(agentIDVal);
    if (agent && agent.name) return agent.name;
    return String(agentIDVal || '').split('@')[0];
  }

  function agentByID(agentIDVal) {
    return agents.find(function (a) { return a.id === agentIDVal; }) || null;
  }

  function activeRoom() {
    return rooms.find(function (r) { return r.id === activeRoomID; }) || null;
  }

  function agentSlugFromID(agentIDVal) {
    return String(agentIDVal || '').split('@')[0];
  }

  function ambiguousSlugsForRoom(roomID) {
    var room = rooms.find(function (r) { return r.id === roomID; }) || null;
    var counts = {};
    (room ? (room.members || []) : []).forEach(function (memberID) {
      var slug = agentSlugFromID(memberID);
      if (!slug) return;
      counts[slug] = (counts[slug] || 0) + 1;
    });
    var ambiguous = {};
    Object.keys(counts).forEach(function (slug) {
      if (counts[slug] > 1) ambiguous[slug] = true;
    });
    return ambiguous;
  }

  function reactionAgentLabel(agentIDVal, roomID) {
    var slug = agentSlugFromID(agentIDVal);
    if (!slug) return String(agentIDVal || '');
    return ambiguousSlugsForRoom(roomID)[slug] ? String(agentIDVal) : slug;
  }

  function roomRoleKey(room, agentIDVal) {
    return room && room.roles ? (room.roles[agentIDVal] || '') : '';
  }

  function agentPresenceHTML(agentIDVal, room) {
    if (!room || !room.members || room.members.indexOf(agentIDVal) === -1) return '';
    var p = effectivePresence(room.id, agentIDVal);
    var head = roomHead(room.id);
    var cls, title;
    if (p.waiting) {
      cls = 'waiting';
      title = 'Listening live (bus_wait open)';
    } else if (head > 0 && p.cursor < head) {
      cls = 'behind';
      title = 'Behind head (not currently waiting), cursor at #' + p.cursor + ' of #' + head;
    } else {
      cls = 'idle';
      title = 'Caught up, not waiting';
    }
    return '<span class="agent-presence-dot ' + cls + '" title="' + esc(title) + '"></span>';
  }

  function agentPresenceText(agentIDVal, room) {
    if (!room || !room.members || room.members.indexOf(agentIDVal) === -1) return '';
    var p = effectivePresence(room.id, agentIDVal);
    var head = roomHead(room.id);
    if (p.waiting) return 'Listening live';
    if (head > 0 && p.cursor < head) return 'Behind head (#' + p.cursor + ' of #' + head + ')';
    return 'Caught up';
  }

  function resolveMentionAgentID(name) {
    if (!name) return '';
    var key = name.toLowerCase();
    if (specialMentionItems.some(function (item) { return item.token === key; })) return '';
    var room = activeRoom();
    if (assignedRoleMentionItems(room).some(function (item) { return item.insertText.slice(1).toLowerCase() === key; })) return '';
    var roomMembers = room ? (room.members || []) : [];
    for (var i = 0; i < roomMembers.length; i++) {
      var memberID = roomMembers[i];
      var member = agentByID(memberID);
      var memberName = member ? member.name : memberID.split('@')[0];
      if (memberName && memberName.toLowerCase() === key && member) return member.id;
    }
    for (var j = 0; j < agents.length; j++) {
      var a = agents[j];
      if ((a.name && a.name.toLowerCase() === key) || a.id.toLowerCase() === key) return a.id;
    }
    return '';
  }

  function mentionTargetsViewer(token, room) {
    if (!token || !agentID || agentID === 'user') return false;
    var key = token.toLowerCase();
    var me = agentByID(agentID);
    if (!me) return false;
    if (resolveMentionAgentID(key) === me.id) return true;
    var members = room ? (room.members || []) : [];
    if (members.indexOf(me.id) === -1) return false;
    if (key === 'everyone' || key === 'all' || key === 'channel' || key === 'here') return true;
    if (key === 'humans') return me.kind === 'human';
    if (key === 'ais') return me.kind === 'ai';
    return roomRoleKey(room, me.id).toLowerCase() === key;
  }

  function targetMatchesViewer(target) {
    if (!target || !agentID || agentID === 'user') return false;
    var key = String(target).toLowerCase();
    if (key === String(agentID).toLowerCase()) return true;
    return key.indexOf('@') === -1 && key === agentSlug(agentID).toLowerCase();
  }

  function messageTargetsViewer(m, room) {
    if (!m || !agentID || agentID === 'user' || m.from === agentID) return false;
    if (m.addressed_to_me) return true;
    if (Array.isArray(m.targets) && m.targets.some(targetMatchesViewer)) return true;
    if (Array.isArray(m.addressed_to) && m.addressed_to.some(targetMatchesViewer)) return true;
    return room && String(room.id || '').indexOf('dm:') === 0 && (room.members || []).indexOf(agentID) !== -1;
  }

  function mentionForAuthor(authorID, room) {
    var slug = agentSlug(authorID);
    var members = room ? (room.members || []) : [];
    var collisions = 0;
    members.forEach(function (memberID) {
      if (agentSlug(memberID).toLowerCase() === slug.toLowerCase()) collisions++;
    });
    return '@' + (collisions > 1 ? authorID : slug);
  }

  function renderRolesList() {
    if (!rolesListEl) return;
    if (!roleEntries.length) {
      rolesListEl.innerHTML = '<p class="roles-empty">No roles loaded.</p>';
      return;
    }
    var html = '';
    roleEntries.forEach(function (e) {
      html += '<div class="role-row' + (e.overridden ? ' role-overridden' : '') + (e.is_custom ? ' role-custom' : '') + '" data-key="' + esc(e.key) + '">';
      html += '<div class="role-row-header">';
      html += '<span class="role-key">' + esc(e.key) + '</span>';
      if (e.overridden) html += '<span class="role-modified-badge">Modified</span>';
      if (e.is_custom) html += '<span class="role-custom-badge">Custom</span>';
      html += '</div>';
      html += '<div class="role-meta-grid">';
      html += '<label class="role-field-label role-emoji-field">Emoji<input class="role-field-input role-emoji-edit" data-key="' + esc(e.key) + '" value="' + esc(e.emoji || e.icon || '') + '" maxlength="16"></label>';
      html += '<label class="role-field-label role-cardinality-field">Cardinality<select class="role-field-input role-cardinality-edit" data-key="' + esc(e.key) + '"><option value="multi"' + ((e.cardinality || 'multi') === 'multi' ? ' selected' : '') + '>multi</option><option value="singleton"' + (e.cardinality === 'singleton' ? ' selected' : '') + '>singleton</option></select></label>';
      html += '<label class="role-field-label role-extends-field">Extends<input class="role-field-input role-extends-edit" data-key="' + esc(e.key) + '" value="' + esc(e.extends || '') + '" placeholder="base role key"></label>';
      html += '</div>';
      html += roleEmojiPickerHTML(e.key, e.emoji || e.icon || '');
      html += '<label class="role-field-label">Description<input class="role-field-input role-desc-edit" data-key="' + esc(e.key) + '" value="' + esc(e.description || '') + '"></label>';
      html += '<label class="role-field-label">Instructions</label>';
      html += '<textarea class="role-textarea" data-key="' + esc(e.key) + '" rows="4">' + esc(e.body) + '</textarea>';
      if (e.resolved_body && e.resolved_body !== e.body) {
        html += '<details class="role-resolved-preview"><summary>Resolved prompt preview</summary><pre>' + esc(e.resolved_body) + '</pre></details>';
      }
      html += '<div class="role-row-actions">';
      html += '<button class="btn btn-sm btn-primary role-save-btn" type="button" data-key="' + esc(e.key) + '">Save</button>';
      if (e.overridden) {
        html += '<button class="btn btn-sm role-revert-btn" type="button" data-key="' + esc(e.key) + '">Revert to default</button>';
      }
      if (e.is_custom) {
        html += '<button class="btn btn-sm btn-danger role-delete-btn" type="button" data-key="' + esc(e.key) + '">Delete</button>';
      }
      html += '</div>';
      html += '</div>';
    });
    rolesListEl.innerHTML = html;

    rolesListEl.querySelectorAll('.role-save-btn').forEach(function (btn) {
      btn.addEventListener('click', function () {
        var key = btn.getAttribute('data-key');
        var ta = rolesListEl.querySelector('textarea[data-key="' + key + '"]');
        var descInput = rolesListEl.querySelector('.role-desc-edit[data-key="' + key + '"]');
        var emojiInput = rolesListEl.querySelector('.role-emoji-edit[data-key="' + key + '"]');
        var cardinalityInput = rolesListEl.querySelector('.role-cardinality-edit[data-key="' + key + '"]');
        var extendsInput = rolesListEl.querySelector('.role-extends-edit[data-key="' + key + '"]');
        if (!ta) return;
        api('PUT', '/roles', { roles: buildRolesPayload(key, {
          description: descInput ? descInput.value.trim() : '',
          emoji: emojiInput ? emojiInput.value.trim() : '',
          body: ta.value,
          cardinality: cardinalityInput ? cardinalityInput.value : 'multi',
          extends: extendsInput ? extendsInput.value.trim() : ''
        }) })
          .then(function () { return loadRoles(); })
          .catch(function (err) { console.error('save role', err); });
      });
    });

    rolesListEl.querySelectorAll('.role-emoji-choice').forEach(function (btn) {
      btn.addEventListener('click', function () {
        var key = btn.getAttribute('data-key');
        var emoji = btn.getAttribute('data-emoji') || '';
        var input = rolesListEl.querySelector('.role-emoji-edit[data-key="' + key + '"]');
        if (input) input.value = emoji;
        var picker = rolesListEl.querySelector('.role-emoji-picker[data-key="' + key + '"]');
        if (picker) {
          picker.querySelectorAll('.role-emoji-choice').forEach(function (choice) {
            choice.classList.toggle('active', choice.getAttribute('data-emoji') === emoji);
          });
        }
      });
    });

    rolesListEl.querySelectorAll('.role-revert-btn').forEach(function (btn) {
      btn.addEventListener('click', function () {
        var key = btn.getAttribute('data-key');
        if (!confirm('Revert "' + key + '" to its default?')) return;
        api('DELETE', '/roles/' + encodeURIComponent(key))
          .then(function () { return loadRoles(); })
          .catch(function (err) { console.error('revert role', err); });
      });
    });

    rolesListEl.querySelectorAll('.role-delete-btn').forEach(function (btn) {
      btn.addEventListener('click', function () {
        var key = btn.getAttribute('data-key');
        if (!confirm('Delete custom role "' + key + '"? This will unassign it from all rooms.')) return;
        api('DELETE', '/roles/' + encodeURIComponent(key) + '?force=true')
          .then(function () { return loadRoles(); })
          .catch(function (err) { console.error('delete role', err); });
      });
    });
  }

  function loadRoles() {
    return api('GET', '/roles')
      .then(function (data) {
        roleEntries = Array.isArray(data) ? data : [];
        renderRolesList();
        renderRoomAgents(); // refresh member list to update role emoji
        renderRoomSettings();
      })
      .catch(function () { roleEntries = []; renderRolesList(); });
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
      updateBodyModalLock();
      return;
    }

    var msg = currentDebugMessage();
    var debugMessages = availableDebugMessages();
    var viewerID = messageDebugState.selectedViewerID || agentID;
    var viewerFields = currentDebugViewerFields();

    messageDebugModal.classList.remove('hidden');
    updateBodyModalLock();

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

  function messageSnippet(msg) {
    if (!msg) return '';
    var text = String(msg.body || '').replace(/\s+/g, ' ').trim();
    if (!text && Array.isArray(msg.attachments) && msg.attachments.length) {
      text = '[attachment]';
    }
    if (text.length > 96) text = text.slice(0, 93) + '...';
    return text;
  }

  function replyReferenceHTML(m) {
    if (!m.reply_to) return '';
    var parentID = parseInt(m.reply_to, 10);
    if (!parentID || parentID <= 0) return '';
    var parent = findMessageInRoom(m.room_id, parentID);
    var label = parent
      ? ('#' + parentID + ' ' + parent.from + ': ' + messageSnippet(parent))
      : ('Reply to #' + parentID);
    return '<button class="reply-reference msg-ref" type="button" data-msg-id="' + esc(String(parentID)) + '" title="Jump to #' + esc(String(parentID)) + '">↪ ' + esc(label) + '</button>';
  }

  function renderPendingReply() {
    if (!replyPendingEl) return;
    if (!pendingReply || pendingReply.roomID !== activeRoomID) {
      replyPendingEl.classList.add('hidden');
      replyPendingEl.innerHTML = '';
      return;
    }
    var parent = findMessageInRoom(activeRoomID, pendingReply.messageID);
    var label = parent
      ? ('#' + pendingReply.messageID + ' ' + parent.from + ': ' + messageSnippet(parent))
      : ('Reply to #' + pendingReply.messageID);
    replyPendingEl.innerHTML =
      '<button class="reply-pending-link msg-ref" type="button" data-msg-id="' + esc(String(pendingReply.messageID)) + '">↪ ' + esc(label) + '</button>' +
      '<button class="reply-pending-clear" type="button" aria-label="Cancel reply" title="Cancel reply">×</button>';
    replyPendingEl.classList.remove('hidden');
  }

  function setPendingReply(messageID) {
    if (!activeRoomID || !messageID) return;
    pendingReply = { roomID: activeRoomID, messageID: messageID };
    renderPendingReply();
    msgBodyInput.focus();
  }

  function clearPendingReply() {
    pendingReply = null;
    renderPendingReply();
  }

  function updateBodyModalLock() {
    var settingsOpen = settingsModal && !settingsModal.classList.contains('hidden');
    var roomSettingsOpen = roomSettingsModal && !roomSettingsModal.classList.contains('hidden');
    var memoryViewerOpen = memoryViewerModal && !memoryViewerModal.classList.contains('hidden');
    var memoryOnboardingOpen = memoryOnboardingModal && !memoryOnboardingModal.classList.contains('hidden');
    var attachmentOpen = attachmentLightboxModal && !attachmentLightboxModal.classList.contains('hidden');
    var leaderboardOpen = leaderboardViewerModal && !leaderboardViewerModal.classList.contains('hidden');
    document.body.style.overflow = (settingsOpen || roomSettingsOpen || memoryViewerOpen || leaderboardOpen || memoryOnboardingOpen || messageDebugState.open || openQuestionModalState.open || attachmentOpen) ? 'hidden' : '';
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

  function lastSeenTitle(a) {
    if (!a || !a.last_seen) return 'Last seen: never';
    return 'Last seen ' + relativeTime(a.last_seen);
  }

  function agentStatus(agent) {
    if (!agent) return 'offline';
    if (typeof agent === 'string') {
      return agent ? 'active' : 'offline';
    }
    switch (agent.state) {
      case 'stale':
        return 'stale';
      case 'offline':
      case 'stopped':
      case 'error':
        return 'offline';
      default:
        return agent.last_seen ? 'active' : 'offline';
    }
  }

  function agentStateMeta(state) {
    switch (state) {
      case 'thinking':
        return { label: 'thinking', className: 'thinking', title: 'thinking' };
      case 'tool_call':
        return { label: 'tool', className: 'tool-call', title: 'running tool' };
      case 'idle':
        return { label: 'idle', className: 'idle', title: 'idle' };
      case 'bootstrapping':
        return { label: 'starting', className: 'bootstrapping', title: 'bootstrapping' };
      case 'respawning':
        return { label: 'respawn', className: 'respawning', title: 'respawning' };
      case 'error':
        return { label: 'error', className: 'error', title: 'error' };
      case 'stopped':
        return { label: 'stopped', className: 'stopped', title: 'stopped' };
      case 'stale':
        return { label: 'stale', className: 'stale', title: 'stale' };
      case 'offline':
        return { label: 'offline', className: 'offline', title: 'offline' };
      default:
        return null;
    }
  }

  function agentStateBadgeHTML(a) {
    var meta = agentStateMeta(a && a.state);
    if (!meta) return '';
    var elapsed = a.state_overlay ? '' : formatStateElapsed(a.state_at);
    var title = meta.title + (elapsed ? ' for ' + elapsed : '');
    var label = meta.label + (elapsed ? ' ' + elapsed : '');
    var data = a.state_at && !a.state_overlay
      ? ' data-state-label="' + esc(meta.label) + '" data-state-title="' + esc(meta.title) + '" data-state-at="' + esc(a.state_at) + '"'
      : '';
    return '<span class="agent-state-badge agent-state-' + esc(meta.className) + '" title="' + esc(title) + '"' + data + '>' + esc(label) + '</span>';
  }

  function formatStateElapsed(isoString, nowMS) {
    if (!isoString) return '';
    var stateMS = Date.parse(isoString);
    if (!Number.isFinite(stateMS)) return '';
    var elapsedSeconds = Math.max(0, Math.floor(((nowMS == null ? Date.now() : nowMS) - stateMS) / 1000));
    var hours = Math.floor(elapsedSeconds / 3600);
    var minutes = Math.floor((elapsedSeconds % 3600) / 60);
    var seconds = elapsedSeconds % 60;
    if (hours > 0) {
      return hours + ':' + String(minutes).padStart(2, '0') + ':' + String(seconds).padStart(2, '0');
    }
    return minutes + ':' + String(seconds).padStart(2, '0');
  }

  function refreshAgentStateBadges() {
    if (document.hidden) return;
    document.querySelectorAll('.agent-state-badge[data-state-at]').forEach(function (el) {
      var elapsed = formatStateElapsed(el.getAttribute('data-state-at'));
      if (!elapsed) return;
      var label = el.getAttribute('data-state-label') || '';
      var title = el.getAttribute('data-state-title') || label;
      el.textContent = label + ' ' + elapsed;
      el.title = title + ' for ' + elapsed;
    });
  }

  // mapLiveness maps an agent's live state onto the three dot visuals. The dot
  // answers one question — "will this agent respond?" error/stopped join offline
  // in red (a broken agent is still a live member in a bad state, not gone); the
  // tooltip names the precise state so "error" never reads as merely "offline".
  function mapLiveness(state) {
    if (state === 'offline' || state === 'error' || state === 'stopped') {
      return { cls: 'dot-offline', title: state };
    }
    if (state === 'stale') {
      return { cls: 'dot-stale', title: 'stale' };
    }
    return { cls: 'dot-active', title: state || 'active' };
  }

  // refreshMessageSenderPresence updates liveness dots in place on agent_update.
  // It never calls renderMessages — agent_update fires on every state transition
  // (unthrottled), so a full re-render would rebuild the chat several times per
  // second. Instead it walks dot elements keyed by data-agent-id, toggles their
  // class/title, and adjusts the icon tooltip's departed marker. Elements are
  // never added or removed here; departed dots turn red via dot-departed so a
  // re-register can flip them back. Identity: if the message carried
  // from_harness it is recomputed from the icon's persisted data attrs (live
  // never overrides it); otherwise it falls back to the live record — a best
  // guess for legacy messages, not a falsification of recorded truth. The icon
  // src/alt are touched only in that fallback branch — once from_harness is
  // present the icon is fixed to the message.
  function refreshMessageSenderPresence() {
    if (document.hidden) return;
    document.querySelectorAll('.msg-sender-dot').forEach(function (dot) {
      var id = dot.getAttribute('data-agent-id');
      var a = agents.find(function (x) { return x.id === id; });
      var wrap = dot.closest('.msg-sender-presence');
      var icon = wrap && wrap.querySelector('.msg-sender-icon');
      var msgHarness = icon ? icon.getAttribute('data-from-harness') : '';
      var msgModel = icon ? icon.getAttribute('data-from-model') : '';
      var model, harness;
      if (msgHarness) {
        model = msgModel || 'unknown';
        harness = msgHarness;
      } else if (a && a.kind === 'ai') {
        model = a.model || 'unknown';
        harness = a.harness || 'unknown';
      } else {
        model = 'unknown';
        harness = 'unknown';
      }
      if (a && a.kind === 'ai') {
        var meta = mapLiveness(a.state);
        dot.className = 'msg-sender-dot ' + meta.cls;
        dot.title = meta.title;
        if (icon) {
          if (!msgHarness) { icon.src = harnessIconSrc(harness); icon.alt = harness; }
          icon.title = model + ' · ' + harness;
        }
      } else {
        dot.className = 'msg-sender-dot dot-departed';
        dot.title = 'departed';
        if (icon) {
          if (!msgHarness) { icon.src = '/icons/unknown.svg'; icon.alt = 'unknown'; }
          icon.title = model + ' · ' + harness + ' (departed)';
        }
      }
    });
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
      connectionStatus.title = 'Connected';
      connectionStatus.setAttribute('aria-label', 'Connected');
    } else {
      connectionStatus.className = 'status-indicator disconnected';
      connectionStatus.title = 'Disconnected';
      connectionStatus.setAttribute('aria-label', 'Disconnected');
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
      if (!r.ok) {
        return r.text().then(function (t) {
          var parsed = null;
          if (t) {
            try { parsed = JSON.parse(t); } catch (_) {}
          }
          var msg = parsed && (parsed.error || parsed.message) ? (parsed.error || parsed.message) : t;
          var err = new Error('HTTP ' + r.status + (msg ? ': ' + msg : ''));
          err.status = r.status;
          err.body = parsed;
          err.responseText = t;
          throw err;
        });
      }
      if (r.status === 204) return null;
      return r.json();
    });
  }

  function renderUsages(resp) {
    if (!resp) return;
    if (resp.providers) {
      usageProviders = resp.providers;
    }
    if (resp.settings) {
      usagePercentDisplay = resp.settings.percent_display === 'used' ? 'used' : 'left';
    }
    usageSwitcherEnabled = !!resp.switcher_enabled;
    renderUsageProviderRows(usageProviders);
    renderUsagesSidebar();
  }

  function canonicalUsageProviders() {
    return (usageProviders || []).sort(function (a, b) {
      return (a.order_number || 0) - (b.order_number || 0);
    });
  }

  function renderUsagesSidebar() {
    if (!rightUsagesPanel) return;
    if (!usagesLoadSettled) {
      rightUsagesPanel.innerHTML = '<div class="usages-empty"><div>Waiting for background refresh\u2026</div></div>';
      return;
    }
    if (usagesLoadFailed) {
      rightUsagesPanel.innerHTML = '<div class="usages-empty">Failed to load usages.</div>';
      return;
    }
    var rows = canonicalUsageProviders();
    var sidebarRows = rows.filter(function (row) { return row.available !== false && !!row.enabled; });
    var empty = sidebarRows.length ? '' : '<div class="usages-empty">' +
      '<div>No usage providers enabled.</div>' +
      '<button class="btn btn-sm usages-settings-shortcut" type="button">Open Settings → Usages</button>' +
    '</div>';
    rightUsagesPanel.innerHTML = empty + '<div class="usages-sidebar-list">' + sidebarRows.map(function (row) {
      var profiles = row.profiles || [];
      if (!profiles || profiles.length === 0) {
        return renderUsageTile(row, { provider_name: row.provider_name, status: 'not_configured' });
      }
      if (profiles.length === 1) {
        return renderUsageTile(row, profiles[0]);
      }
      return renderMultiProfileTile(row, profiles);
    }).join('') + '</div>';
    refreshSwitcherPanel();
  }

  function usageProviderIcon(key) {
    if (key === 'codex' || key === 'claude-code' || key === 'github-copilot' || key === 'mistral' || key === 'ollama-cloud') {
      return '<span class="usages-provider-mask usages-provider-mask-' + esc(key) + '" aria-hidden="true"></span>';
    }
    return '<span aria-hidden="true">AI</span>';
  }

  function usageProviderIconClass(key) {
    return 'usages-provider-icon usages-provider-icon-' + String(key || 'unknown').replace(/[^a-z0-9_-]/gi, '-');
  }

  // Maps a usage provider key to its switcher tool key.
  function switcherToolForProvider(key) {
    if (key === 'claude-code') return 'claude';
    if (key === 'codex') return 'codex';
    return '';
  }

  // Expiry state icons (from __plans/ svgrepo exports). All three use
  // fill="currentColor"; the expired icon's red is set via a CSS class
  // (.switcher-expiry-icon--expired { color: var(--red) }) on the wrapping
  // span, not the SVG attribute — Safari doesn't support var() in attributes.
  var EXPIRY_ICON_OK = '<svg fill="currentColor" width="14" height="14" viewBox="0 0 20 20" aria-hidden="true"><path d="M2.93 17.07A10 10 0 1 1 17.07 2.93 10 10 0 0 1 2.93 17.07zm12.73-1.41A8 8 0 1 0 4.34 4.34a8 8 0 0 0 11.32 11.32zM6.7 9.29L9 11.6l4.3-4.3 1.4 1.42L9 14.4l-3.7-3.7 1.4-1.42z"/></svg>';
  var EXPIRY_ICON_SOON = '<svg fill="currentColor" width="14" height="14" viewBox="0 0 24 24" aria-hidden="true"><path d="M8.827 6.956c2.265.662 5.109-.295 8.172-1.867l.001.079c0 1.3-1.642 2.897-3.248 4.288a3.818 3.818 0 0 0-.752.898v6.726c1.321.372 2.815 2.089 3.827 3.655-.027.088-.043.179-.077.265h-8.5c-.034-.087-.057-.17-.082-.254 1.01-1.57 2.509-3.293 3.833-3.666v-6.729a3.819 3.819 0 0 0-.73-.88 17.898 17.898 0 0 1-2.443-2.515zM17.922 2H20v1h-1.516A5.594 5.594 0 0 1 19 5.319c0 2.15-1.479 4.294-3.545 6.092a1.544 1.544 0 0 0-.62 1.089 1.544 1.544 0 0 0 .62 1.089C17.521 15.387 19 17.53 19 19.68a5.595 5.595 0 0 1-.516 2.32H20v1H5v-1h1.5a5.666 5.666 0 0 1-.5-2.319c0-2.15 1.479-4.294 3.545-6.092a1.544 1.544 0 0 0 .62-1.089 1.544 1.544 0 0 0-.62-1.089C7.479 9.613 6 7.47 6 5.32A5.666 5.666 0 0 1 6.5 3H5V2zm-.545 1H7.624A4.68 4.68 0 0 0 7 5.32c0 1.645 1.137 3.54 3.2 5.336a2.435 2.435 0 0 1 .966 1.844 2.432 2.432 0 0 1-.965 1.843c-2.064 1.797-3.2 3.692-3.2 5.338A4.68 4.68 0 0 0 7.623 22h9.753A4.646 4.646 0 0 0 18 19.68c0-1.645-1.137-3.54-3.2-5.336a2.435 2.435 0 0 1-.966-1.844 2.432 2.432 0 0 1 .965-1.843c2.064-1.797 3.2-3.692 3.2-5.338A4.646 4.646 0 0 0 17.378 3z"/><path fill="none" d="M0 0h24v24H0z"/></svg>';
  var EXPIRY_ICON_EXPIRED = '<svg fill="currentColor" width="14" height="14" viewBox="0 0 24 24" aria-hidden="true"><path d="M7.085 2H5v1h1.5A5.666 5.666 0 0 0 6 5.319c0 2.15 1.479 4.294 3.545 6.092a1.544 1.544 0 0 1 .62 1.089 1.544 1.544 0 0 1-.62 1.089C7.479 15.387 6 17.53 6 19.68A5.666 5.666 0 0 0 6.5 22H5v1h15v-1h-1.516A5.595 5.595 0 0 0 19 19.681c0-2.15-1.479-4.294-3.545-6.092a1.544 1.544 0 0 1-.62-1.089 1.544 1.544 0 0 1 .62-1.089C17.521 9.613 19 7.47 19 5.32A5.594 5.594 0 0 0 18.484 3H20V2zm10.292 1A4.646 4.646 0 0 1 18 5.32c0 1.645-1.137 3.54-3.201 5.337a2.432 2.432 0 0 0-.965 1.843 2.435 2.435 0 0 0 .965 1.844c2.064 1.796 3.2 3.691 3.2 5.337A4.646 4.646 0 0 1 17.378 22H7.624A4.68 4.68 0 0 1 7 19.68c0-1.645 1.137-3.54 3.201-5.337a2.432 2.432 0 0 0-.965-1.843 2.435 2.435 0 0 0-.965-1.844C8.137 8.86 7 6.965 7 5.32A4.68 4.68 0 0 1 7.623 3zM8.102 18.925a3.246 3.246 0 0 1 1.35-1.9l2.593-1.722a.823.823 0 0 1 .91 0l2.594 1.722a3.248 3.248 0 0 1 1.35 1.901 3.051 3.051 0 0 1 .1.755A3.645 3.645 0 0 1 16.75 21h-8.5A3.713 3.713 0 0 1 8 19.68a3.05 3.05 0 0 1 .102-.755z"/><path fill="none" d="M0 0h24v24H0z"/></svg>';

  // Renders each switcher profile as a block inside the provider tile, with its
  // own usage windows — so every account's quota is visible at once, not just
  // the active one. Returns '' when the switcher is off or the tool has no
  // profiles, in which case the tile falls back to its single-account view.
  function renderSwitcherTileProfiles(providerKey) {
    var provider = (usageProviders || []).find(function (p) { return p.provider_name === providerKey; });
    if (!provider || !usageSwitcherEnabled) return '';
    var profiles = provider.profiles || [];
    profiles.sort(function (a, b) { return (b.active ? 1 : 0) - (a.active ? 1 : 0); });
    if (!profiles.length) return '';
    var blocked = !!switcherInFlight[providerKey] || (!provider.switch_eligible && provider.switch_ineligible_reason);
    return profiles.map(function (p) {
      var activeTag = p.active ? '<span class="switcher-tile-active">active</span>' : '';
      var switchBtn = (!p.active && !blocked)
        ? '<button class="switcher-switch-pill" type="button"' +
            ' data-tool="' + esc(switcherToolForProvider(providerKey)) + '" data-profile="' + esc(p.profile_name) + '"' +
            (!p.has_credentials ? ' data-empty="1"' : '') +
            ' title="Switch to ' + esc(p.profile_name) + '" aria-label="Switch to ' + esc(p.profile_name) + '">SWITCH</button>'
        : '';
      var emailTitle = p.email ? ' title="' + esc(p.email) + '"' : '';
      var expiryTitle = '';
      var expiryIcon = '';
      if (p.expires_at) {
        var expMs = new Date(p.expires_at).getTime();
        if (Number.isFinite(expMs)) {
          var diffMs = expMs - Date.now();
          var absSec = Math.abs(Math.round(diffMs / 1000));
          var eh = Math.floor(absSec / 3600);
          var em = Math.floor((absSec % 3600) / 60);
          var erel;
          if (eh === 0 && em === 0) {
            erel = diffMs > 0 ? 'in < 1m' : 'expired < 1m ago';
          } else {
            erel = diffMs > 0 ? ('in ' + (eh > 0 ? eh + 'h ' : '') + em + 'm') : ('expired ' + (eh > 0 ? eh + 'h ' : '') + em + 'm ago');
          }
          expiryTitle = 'Token expires: ' + new Date(expMs).toISOString() + ' (' + erel + ')';
          var iconSvg = diffMs <= 0 ? EXPIRY_ICON_EXPIRED
            : (diffMs <= 10800000 ? EXPIRY_ICON_SOON : EXPIRY_ICON_OK);
          var iconClass = 'switcher-expiry-icon' + (diffMs <= 0 ? ' switcher-expiry-icon--expired' : '');
          expiryIcon = '<span class="' + iconClass + '" title="' + esc(expiryTitle) + '">' + iconSvg + '</span>';
        }
      }
      var planBadge = p.plan
        ? '<span class="switcher-tile-plan"' + emailTitle + '>' + esc(p.plan) + '</span>'
        : '';
      var nameTitle = expiryTitle ? ' title="' + esc(expiryTitle) + '"' : '';
      // No per-row age: every profile of a provider refreshes on the same
      // interval, so the card header's single timestamp is true of all of them.
      // A per-row copy would repeat the same string on every line.
      var head = '<div class="switcher-tile-head">' +
        '<span class="switcher-tile-name"' + nameTitle + '>' + esc(p.profile_name) + '</span>' +
        expiryIcon + switchBtn + activeTag + planBadge +
      '</div>';
      var body;
      if (!p.has_credentials) {
        body = '<div class="usages-empty usages-empty-compact">Needs login</div>';
      } else if (!p.windows || p.windows.length === 0) {
        body = '<div class="usages-empty usages-empty-compact">Refreshing\u2026</div>';
      } else {
        var windows = p.windows.map(function (w) {
          return renderUsageWindowRow(w, p.last_refresh_at);
        }).join('');
        body = windows || '<div class="usages-empty usages-empty-compact">No usage data yet.</div>';
      }
      return '<div class="switcher-tile-profile' + (p.active ? ' switcher-tile-profile-active' : '') + '">' +
        head + body + renderCreditsRow(p.credits) + usageErrorLine(p) +
      '</div>';
    }).join('');
  }

  function renderMultiProfileTile(row, profiles) {
    var firstSnap = profiles[0] || {};
    var label = row.label || firstSnap.provider_name || firstSnap.provider;

    // One age for the card, taken from the oldest profile. Every profile of a
    // provider refreshes on the same interval, so they agree; the oldest is
    // chosen so the header can never claim to be fresher than a row beneath it.
    // Not conditional on the rows agreeing: two profiles fetched seconds apart
    // can format differently ("just now" vs "1m ago"), and a conditional would
    // move the label between the header and the rows on a rounding boundary.
    var oldest = null;
    (profiles || []).forEach(function (p) {
      if (!p.last_refresh_at) return;
      if (oldest === null || Date.parse(p.last_refresh_at) < Date.parse(oldest)) oldest = p.last_refresh_at;
    });
    var commonAge = oldest
      ? 'Updated ' + formatRelativeAge(oldest) + ' ago'
      : statusLabel((profiles[0] || {}).status || 'Not configured');

    var profileBlocks = renderSwitcherTileProfiles(row.provider_name, false);

    return '<div class="usages-provider-tile" data-provider="' + esc(row.provider_name) + '">' +
      '<div class="usages-provider-heading">' +
        '<div class="usages-provider-title"><span class="' + esc(usageProviderIconClass(row.provider_name)) + '">' + usageProviderIcon(row.provider_name) + '</span><div><div class="usages-provider-name">' + esc(label) + '</div>' + (commonAge ? '<div class="usages-provider-updated">' + esc(commonAge) + '</div>' : '') + '</div></div>' +
        // No card-level plan badge while profile rows are present: a plan
        // belongs to one account, and each row already renders its own. Hoisting
        // the first profile's plan into the header states it for the whole card,
        // which is wrong the moment two profiles are on different plans — the
        // same mistake as a card-level "Updated X ago" over per-profile numbers.
        (profileBlocks ? '' : '<span class="usages-plan-badge">' + esc(firstSnap.plan || '-') + '</span>') +
      '</div>' +
      profileBlocks +
      (profileBlocks ? '' : renderCreditsRow(firstSnap.credits) + usageErrorLine(firstSnap)) +
    '</div>';
  }

  function renderUsageTile(row, snap) {
    var available = row.available !== false;
    var enabled = !!row.enabled;
    if (!available || !enabled) {
      var state = available ? 'Not enabled' : 'Unavailable';
      var detail = available ? 'Configure in Settings → Usages' : 'Available in upcoming release';
      return '<button class="usages-provider-tile usages-provider-tile-inactive usages-settings-shortcut" type="button" data-provider="' + esc(row.provider_name) + '">' +
        '<span class="' + esc(usageProviderIconClass(row.provider_name)) + '">' + usageProviderIcon(row.provider_name) + '</span>' +
        '<span><strong>' + esc(row.label || row.provider_name) + '</strong><em>' + esc(state + ' — ' + detail) + '</em></span>' +
      '</button>';
    }
    var label = row.label || providerLabel(snap.provider_name || row.provider_name);
    var updated = snap.last_refresh_at ? 'Updated ' + formatRelativeAge(snap.last_refresh_at) + ' ago' : statusLabel(snap.status || 'Not configured');
    var plan = snap.plan || statusLabel(snap.status);
    
    return '<div class="usages-provider-tile" data-provider="' + esc(snap.provider_name || row.provider_name) + '">' +
      '<div class="usages-provider-heading">' +
        '<div class="usages-provider-title"><span class="' + esc(usageProviderIconClass(snap.provider_name || row.provider_name)) + '">' + usageProviderIcon(snap.provider_name || row.provider_name) + '</span><div><div class="usages-provider-name">' + esc(label) + '</div><div class="usages-provider-updated">' + esc(updated) + '</div></div></div>' +
        '<span class="usages-plan-badge">' + esc(plan || '-') + '</span>' +
      '</div>' +
      usageStaleLine(snap) +
      (snap.windows ? (snap.windows.map(function (w) { return renderUsageWindowRow(w, snap.last_refresh_at); }).join('') || '<div class="usages-empty usages-empty-compact">No window data yet.</div>') : '<div class="usages-empty usages-empty-compact">No window data yet.</div>') +
      renderCreditsRow(snap.credits) + usageErrorLine(snap) +
    '</div>';
  }

  function providerLabel(key) {
    var found = usageProviders.find(function (p) { return p.provider_name === key; });
    return found ? found.label : key;
  }

  function renderUsageWindowRow(w, lastRefreshAt) {
    if (!w) return '';
    var pct = Number(w.percent_used);
    var display = usagePercentValue(pct);
    var fill = Number.isFinite(display) ? Math.max(0, Math.min(100, display)) : 0;
    var label = usagePercentDisplay === 'used' ? 'used' : 'left';
    var reset = w.reset_at ? resetText(w.key, w.reset_at) : '';
    var resetAbs = w.reset_at ? formatAbsoluteDate(new Date(w.reset_at)) : '';
    var tickHtml = '';
    var paceHtml = '';
    if (w.pace) {
      var tickPct = Math.max(0, Math.min(100, usagePercentValue(Number(w.pace.expected_percent))));
      var tickClass = 'usages-pace-tick usages-pace-tick--' + (w.pace.state === 'reserve' ? 'reserve' : w.pace.state === 'deficit' ? 'deficit' : 'on-track');
      tickHtml = '<i class="' + tickClass + '" style="left:' + tickPct.toFixed(2) + '%"></i>';
      var absDelta = Math.abs(Number(w.pace.delta_percent));
      var paceLabel = '';
      if (w.pace.state === 'reserve') {
        paceLabel = formatPercent(absDelta) + ' in reserve';
      } else if (w.pace.state === 'deficit') {
        paceLabel = formatPercent(absDelta) + ' in deficit';
      }
      var etaText = '';
      var etaAbs = '';
      if (w.pace.lasts_to_reset) {
        etaText = 'Lasts until reset';
      } else if (w.pace.eta_seconds != null) {
        var fetchBase = lastRefreshAt ? Date.parse(lastRefreshAt) : Date.now();
        if (!Number.isFinite(fetchBase)) fetchBase = Date.now();
        var etaDate = new Date(fetchBase + w.pace.eta_seconds * 1000);
        etaText = 'Runs out in ' + formatResetCountdown(etaDate.toISOString());
        etaAbs = formatAbsoluteDate(etaDate);
      }
      if (paceLabel || etaText) {
        var paceStateClass = 'usages-pace-text usages-pace-text--' + (w.pace.state === 'reserve' ? 'reserve' : w.pace.state === 'deficit' ? 'deficit' : 'on-track');
        paceHtml = '<div class="' + paceStateClass + '">' +
          (paceLabel ? '<span>' + esc(paceLabel) + '</span>' : '') +
          (paceLabel && etaText ? '<span class="usages-pace-sep">·</span>' : '') +
          (etaText ? '<span' + (etaAbs ? ' title="' + esc(etaAbs) + '"' : '') + '>' + esc(etaText) + '</span>' : '') +
          '</div>';
      }
    }
    return '<div class="usages-window-row">' +
      '<div class="usages-window-top"><span>' + esc(windowLabel(w.key)) + '</span></div>' +
      '<div class="usages-progress" aria-label="' + esc(windowLabel(w.key)) + ' usage"><span style="width:' + fill.toFixed(2) + '%"></span>' + tickHtml + '</div>' +
      '<div class="usages-window-meta"><strong>' + esc(formatPercent(display) + ' ' + label) + '</strong><span' + (resetAbs ? ' title="' + esc(resetAbs) + '"' : '') + '>' + esc(reset) + '</span></div>' +
      paceHtml +
    '</div>';
  }

  function usagePercentValue(percentUsed) {
    if (!Number.isFinite(percentUsed)) return NaN;
    return usagePercentDisplay === 'used' ? percentUsed : 100 - percentUsed;
  }

  function formatPercent(value) {
    if (!Number.isFinite(value)) return '-';
    var rounded = Math.max(0, Math.min(100, value));
    return rounded.toFixed(rounded % 1 ? 1 : 0) + '%';
  }

  function formatResetCountdown(value) {
    var ms = Date.parse(value);
    if (!Number.isFinite(ms)) return '-';
    var sec = Math.max(0, Math.round((ms - Date.now()) / 1000));
    var days = Math.floor(sec / 86400);
    var hours = Math.floor((sec % 86400) / 3600);
    var mins = Math.floor((sec % 3600) / 60);
    if (days > 0) return days + 'd ' + hours + 'h';
    if (hours > 0) return hours + 'h ' + mins + 'm';
    return mins + 'm';
  }

  var ABSOLUTE_DATE_DAYS = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'];
  var ABSOLUTE_DATE_MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

  // formatAbsoluteDate renders a Date as "Friday, 14 Aug 2026, 21:17" in
  // English regardless of browser locale (hardcoded day/month names; local
  // time, matching the rest of the usages UI). Returns '' for an invalid date.
  function formatAbsoluteDate(d) {
    if (!(d instanceof Date) || isNaN(d.getTime())) return '';
    return ABSOLUTE_DATE_DAYS[d.getDay()] + ', ' + d.getDate() + ' ' +
      ABSOLUTE_DATE_MONTHS[d.getMonth()] + ' ' + d.getFullYear() + ', ' +
      String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0');
  }

  function resetText(key, value) {
    return 'Resets in ' + formatResetCountdown(value);
  }

  function windowLabel(key) {
    if (key === 'session') return 'Session';
    if (key === 'weekly') return 'Weekly';
    if (key === 'weekly_opus') return 'Weekly (Opus)';
    if (key === 'weekly_sonnet') return 'Weekly (Sonnet)';
    if (String(key || '').indexOf('weekly_scoped:') === 0) {
      return 'Weekly (' + key.slice('weekly_scoped:'.length).replace(/-/g, ' ') + ')';
    }
    if (key === 'codex_spark') return 'Codex Spark';
    if (key === 'codex_spark_weekly') return 'Codex Spark Weekly';
    if (key === 'premium') return 'Premium interactions';
    if (key === 'chat') return 'Chat';
    return key || 'Window';
  }

  function statusLabel(status) {
    return String(status || 'unknown').replace(/_/g, ' ');
  }

  function renderCreditsRow(credits) {
    if (!credits) return '';
    var value = Number(credits.balance);
    var limit = Number(credits.spend_limit);
    var text = Number.isFinite(value) ? value.toFixed(2) : '-';
    if (Number.isFinite(limit) && limit > 0) {
      text += ' / ' + limit.toFixed(2);
    }
    return '<div class="usages-credits-row"><span>' + esc(credits.label || 'Credits') + '</span><strong>' + esc(text) + '</strong></div>';
  }

  function formatRelativeAge(value) {
    var ms = Date.parse(value);
    if (!Number.isFinite(ms)) return 'just now';
    var sec = Math.max(0, Math.round((Date.now() - ms) / 1000));
    if (sec < 60) return sec + 's';
    var mins = Math.floor(sec / 60);
    if (mins < 60) return mins + 'm';
    var hours = Math.floor(mins / 60);
    if (hours < 24) return hours + 'h';
    return Math.floor(hours / 24) + 'd';
  }

  function usageStaleLine(snap) {
    if (!snap || !snap.stale) return '';
    var fetched = snap.last_refresh_at ? new Date(snap.last_refresh_at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : 'unknown';
    return '<div class="usages-stale-line">Stale, last fetched ' + esc(fetched) + '</div>';
  }

  function usageErrorLine(snap) {
    if (!snap || (!snap.error && !snap.stale)) return '';
    var text = snap.error;
    if (!text) return '';
    return '<div class="usages-error-line">' + esc(text) + '</div>';
  }

  var lastUsagesResponse = null;

  function loadUsages() {
    return api('GET', '/api/usages').then(function (resp) {
      lastUsagesResponse = resp;
      if (resp && resp.settings) renderUsageSettings(resp.settings);
      usagesLoadSettled = true; // a load attempt has now settled — must be set
      // before renderUsages, which paints the pre-first-load placeholder while
      // this is false and would otherwise discard the response it was given.
      usagesLoadFailed = false; // usages fetched OK — clear any prior error
      renderUsages(resp);
      return resp;
    }).catch(function (err) {
      // /api/usages failed. Settle the gate here so
      // a background renderRightSidebar re-entry can't strand the panel on a
      // permanent "loading" spinner, and keep the error via usagesLoadFailed.
      usagesLoadSettled = true;
      usagesLoadFailed = true;
      if (rightUsagesPanel) rightUsagesPanel.innerHTML = '<div class="usages-empty">Failed to load usages.</div>';
      console.error('usages', err);
    });
  }

  function renderUsageSettings(settings) {
    if (!settings) return;
    if (usagesRefreshInput) usagesRefreshInput.value = settings.refresh_interval_sec || 120;
    if (usagesEnvBadge) usagesEnvBadge.classList.toggle('hidden', !settings.env_override);
    usagePercentDisplay = settings.percent_display === 'used' ? 'used' : 'left';
    document.querySelectorAll('.usages-percent-option').forEach(function (btn) {
      btn.classList.toggle('active', btn.getAttribute('data-percent-display') === usagePercentDisplay);
    });
  }

  // ── Switcher ────────────────────────────────────────────────────────

  function loadSwitcher() {
    return loadUsages().then(function () {
      renderSwitcherSettingsRow();
      renderSwitcherSettingsProfiles();
      refreshSwitcherPanel();
    }).catch(function (err) {
      usagesLoadSettled = true;
      console.error('switcher', err);
    });
  }

  function renderSwitcherSettingsRow() {
    var el = document.getElementById('switcher-settings-row');
    if (!el) return;
    var enabled = usageSwitcherEnabled;
    var ineligibleNotes = (usageProviders || []).filter(function (p) { return !p.switch_eligible && p.switch_ineligible_reason; }).map(function (p) {
      return '<span class="switcher-ineligible-note">' + esc(p.provider_name) + ': ' + esc(p.switch_ineligible_reason) + '</span>';
    }).join('');
    el.innerHTML =
      '<div class="settings-row">' +
        '<div class="settings-row-info">' +
          '<label class="settings-label" for="switcher-enabled-toggle">Switcher profiles</label>' +
          '<span class="settings-desc">Enable per-profile credential switching for Claude Code and Codex.</span>' +
          ineligibleNotes +
        '</div>' +
        '<div class="settings-control">' +
          '<label class="usages-provider-toggle" aria-label="Switcher profiles">' +
            '<input type="checkbox" id="switcher-enabled-toggle"' + (enabled ? ' checked' : '') + '>' +
            '<span></span>' +
          '</label>' +
        '</div>' +
      '</div>';
  }

  // Settings → Switcher lists profiles for management (add / import / delete).
  // Switching itself deliberately stays in the usages panel, where the live
  // quota numbers are — settings is for configuration, not operation.
  function renderSwitcherSettingsProfiles() {
    var el = document.getElementById('switcher-settings-profiles');
    if (!el) return;
    if (!usageSwitcherEnabled) {
      el.innerHTML = '';
      return;
    }
    el.innerHTML = ['claude-code', 'codex'].map(function (name) {
      var provider = (usageProviders || []).find(function (p) { return p.provider_name === name; }) || { provider_name: name, profiles: [], switch_eligible: true, switch_ineligible_reason: '' };
      var elig = { tool: name, eligible: provider.switch_eligible, reason: provider.switch_ineligible_reason };
      return renderSwitcherSection(name, provider.profiles, elig, !!switcherInFlight[switcherToolForProvider(name)], 'manage');
    }).join('');
  }

  function refreshSwitcherPanel() {
    if (!rightUsagesPanel) return;
    var container = rightUsagesPanel.querySelector('.switcher-panel');
    if (!container) return;
    if (!usageSwitcherEnabled) {
      container.innerHTML = '';
      return;
    }
    var tools = ['claude-code', 'codex'];
    container.innerHTML = tools.map(function (name) {
      var provider = (usageProviders || []).find(function (p) { return p.provider_name === name; }) || { provider_name: name, profiles: [], switch_eligible: true, switch_ineligible_reason: '' };
      var toolElig = { tool: name, eligible: provider.switch_eligible, reason: provider.switch_ineligible_reason };
      return renderSwitcherSection(name, provider.profiles, toolElig, !!switcherInFlight[switcherToolForProvider(name)], 'switch');
    }).join('');
  }

  // mode: 'manage' (Settings -> Switcher) shows add/import/delete and no Switch
  // button; 'switch' (usages panel) shows only the Switch action. Profile
  // management lives in Settings; the panel is for operating, next to the
  // live quota numbers.
  function renderSwitcherSection(tool, profiles, eligibility, inFlight, mode) {
    var toolLabel = tool === 'claude-code' ? 'Claude Code' : tool === 'codex' ? 'Codex' : tool;
    var elig = eligibility || { eligible: true, reason: '' };
    var disabled = !!inFlight || (!elig.eligible && !elig.absent);
    var activeProfile = profiles.filter(function (p) { return p.active; })[0];
    var activeLabel = activeProfile ? activeProfile.profile_name : '—';
    var spinner = inFlight ? ' <span class="switcher-spinner" aria-hidden="true"></span>' : '';
    var eligNote = !elig.eligible && elig.reason
      ? '<div class="switcher-ineligible-inline">' + esc(elig.reason) + '</div>'
      : '';
    var rows = profiles.length
      ? profiles.map(function (p) { return renderSwitcherProfileRow(tool, p, elig, inFlight, mode); }).join('')
      : '<div class="switcher-empty">No profiles yet.</div>';
    var addImport = (disabled || mode !== 'manage') ? '' :
      '<div class="switcher-actions">' +
        '<button class="btn btn-sm switcher-add-btn" type="button" data-tool="' + esc(switcherToolForProvider(tool)) + '">Add profile</button>' +
        '<button class="btn btn-sm switcher-import-btn" type="button" data-tool="' + esc(switcherToolForProvider(tool)) + '">Import current login</button>' +
      '</div>';
    return '<div class="switcher-section" data-tool="' + esc(switcherToolForProvider(tool)) + '">' +
      '<div class="switcher-section-header">' +
        '<span class="switcher-section-tool">' + esc(toolLabel) + '</span>' +
        '<span class="switcher-section-active">' + esc(activeLabel) + '</span>' +
        spinner +
      '</div>' +
      eligNote +
      rows +
      addImport +
    '</div>';
  }

  // Compact per-profile usage for the switcher rows: the tightest window (the
  // one closest to running out), rendered in whichever percent convention the
  // user picked for the tiles. The full breakdown stays in the provider tile
  // above; this is a glance, not a second dashboard.
  function switcherProfileUsage(providerName, profileName) {
    var provider = (usageProviders || []).find(function (p) { return p.provider_name === providerName; });
    var snap = provider && provider.profiles && provider.profiles.find(function (s) { return s.profile_name === profileName; });
    if (!snap) return '';
    if (snap.status && snap.status !== 'ok') return '';
    var windows = Array.isArray(snap.windows) ? snap.windows : [];
    var worst = null;
    windows.forEach(function (w) {
      if (typeof w.percent_used !== 'number') return;
      if (!worst || w.percent_used > worst.percent_used) worst = w;
    });
    if (!worst) return '';
    var pct = usagePercentDisplay === 'used'
      ? Math.round(worst.percent_used) + '% used'
      : Math.max(0, 100 - Math.round(worst.percent_used)) + '% left';
    var plan = snap.plan ? esc(snap.plan) + ' · ' : '';
    return '<span class="switcher-profile-usage" title="' + esc(windowLabel(worst.key) || worst.key || '') + '">' +
      plan + esc(pct) + '</span>';
  }

  function renderSwitcherProfileRow(tool, profile, eligibility, inFlight, mode) {
    var isActive = !!profile.active;
    var isEmpty = !profile.has_credentials;
    var disabled = !!inFlight || (eligibility && !eligibility.eligible && !eligibility.absent);
    var emailTitle = profile.email ? ' title="' + esc(profile.email) + '"' : '';
    var activeMarker = isActive ? '<span class="switcher-active-marker" aria-label="Active">●</span> ' : '';
    var credLabel = isEmpty ? ' <span class="switcher-needs-login">needs login</span>' : '';
    var switchBtn = (!isActive && !disabled && mode !== 'manage')
      ? '<button class="btn btn-sm switcher-switch-btn" type="button"' +
          ' data-tool="' + esc(switcherToolForProvider(tool)) + '" data-profile="' + esc(profile.profile_name) + '"' +
          (isEmpty ? ' data-empty="1"' : '') +
          '>Switch</button>'
      : '';
    var deleteBtn = (!disabled && mode === 'manage')
      ? '<button class="btn btn-icon switcher-delete-btn" type="button"' +
          ' data-tool="' + esc(switcherToolForProvider(tool)) + '" data-profile="' + esc(profile.profile_name) + '"' +
          (isActive ? ' data-active="1"' : '') +
          ' title="Delete profile" aria-label="Delete ' + esc(profile.profile_name) + '">✕</button>'
      : '';
    var usage = switcherProfileUsage(tool, profile.profile_name);
    return '<div class="switcher-profile-row' + (isActive ? ' switcher-profile-row-active' : '') + '">' +
      '<span class="switcher-profile-name"' + emailTitle + '>' + activeMarker + esc(profile.profile_name) + credLabel + '</span>' +
      usage +
      '<span class="switcher-profile-controls">' + switchBtn + deleteBtn + '</span>' +
    '</div>';
  }

  function switcherToggleEnabled(v) {
    api('POST', '/api/usages/switcher/settings', { enabled: v })
      .then(function () { return loadUsages(); })
      .catch(function (err) {
        alert('Failed to ' + (v ? 'enable' : 'disable') + ' switcher: ' + (err && err.message ? err.message : err));
        renderSwitcherSettingsRow(); // restore toggle to actual state
      });
  }

  function switcherSwitch(tool, profile, isEmpty) {
    if (isEmpty && !confirm(
      'Profile "' + profile + '" is empty — switching will delete the live ' + tool + ' credentials and require a fresh login on next start.\n\nProceed?'
    )) return;
    switcherInFlight[tool] = true;
    refreshSwitcherPanel();
    // Separate catches: a failed switch and a failed refresh are different events.
    api('POST', '/api/usages/switcher/switch', { tool: tool, profile: profile })
      .then(
        function () {
          return loadUsages().catch(function (err) {
            alert('Switched to "' + profile + '", but refresh failed: ' + (err && err.message ? err.message : err));
          });
        },
        function (err) {
          alert('Failed to switch to "' + profile + '": ' + (err && err.message ? err.message : err));
        }
      )
      .then(function () {
        switcherInFlight[tool] = false;
        refreshSwitcherPanel();
        if (rightSidebarMode === 'usages') renderUsagesSidebar();
      });
  }

  function switcherAddProfile(tool) {
    var name = prompt('Profile name:');
    if (!name || !name.trim()) return;
    name = name.trim();
    api('POST', '/api/usages/switcher/profiles', { tool: tool, profile: name, mode: 'add' })
      .then(function () { return loadUsages(); })
      .catch(function (err) {
        alert('Failed to add profile: ' + (err && err.message ? err.message : err));
      });
  }

  function switcherImportProfile(tool) {
    var name = prompt('Name for this login:');
    if (!name || !name.trim()) return;
    name = name.trim();
    api('POST', '/api/usages/switcher/profiles', { tool: tool, profile: name, mode: 'import' })
      .then(function () { return loadUsages(); })
      .catch(function (err) {
        alert('Failed to import login: ' + (err && err.message ? err.message : err));
      });
  }

  function switcherDeleteProfile(tool, profile, isActive) {
    var msg = isActive
      ? 'Profile "' + profile + '" is active — deleting it will clear the live ' + tool + ' credentials and require a fresh login.\n\nProceed?'
      : 'Delete profile "' + profile + '"?';
    if (!confirm(msg)) return;
    var force = isActive;
    api('DELETE', '/api/usages/switcher/profiles', { tool: tool, profile: profile, force: force })
      .then(function () { return loadUsages(); })
      .catch(function (err) {
        if (err && err.status === 409) {
          // Server confirmed it's active — ask again with force flag
          if (confirm('Server reports this profile is active. Clear live credentials and delete anyway?')) {
            api('DELETE', '/api/usages/switcher/profiles', { tool: tool, profile: profile, force: true })
              .then(function () { return loadUsages(); })
              .catch(function (e) { alert('Failed to delete: ' + (e && e.message ? e.message : e)); });
          }
        } else {
          alert('Failed to delete profile: ' + (err && err.message ? err.message : err));
        }
      });
  }

  function renderUsageProviderRows(rows) {
    if (!usagesProviderRows) return;
    rows = rows || usageProviders || [];
    usagesProviderRows.innerHTML = rows.map(function (row, idx) {
      if (row.key === 'github-copilot') return renderCopilotProviderRow(row, idx, rows.length);
      if (row.key === 'mistral') return renderMistralProviderRow(row, idx, rows.length);
      if (row.key === 'ollama-cloud') return renderOllamaProviderRow(row, idx, rows.length);
      var available = !!row.available;
      var enabled = !!row.enabled;
      return '<div class="settings-row' + (available ? '' : ' usages-provider-row-disabled') + '">' +
        '<div class="settings-row-info">' +
          '<label class="settings-label">' + esc(row.label) + '</label>' +
          '<span class="settings-desc">' + esc(available ? 'Show this provider in the usage sidebar and CLI.' : 'Available in upcoming release.') + '</span>' +
        '</div>' +
        '<div class="settings-control usages-provider-control">' + usageProviderOrderControls(row, idx, rows.length) + '<label class="usages-provider-toggle" aria-label="' + esc(row.label) + ' provider">' +
          '<input type="checkbox" data-provider="' + esc(row.key) + '"' + (enabled ? ' checked' : '') + (available ? '' : ' disabled') + '>' +
          '<span></span>' +
        '</label></div>' +
      '</div>';
    }).join('');
  }

  function usageProviderOrderControls(row, idx, total) {
    var label = row && row.label ? row.label : row.key;
    return '<div class="usages-provider-order" aria-label="Provider order">' +
      '<button class="btn btn-icon usages-provider-order-btn" type="button" data-usage-provider-move="' + esc(row.key) + '" data-direction="up" title="Move ' + esc(label) + ' up" aria-label="Move ' + esc(label) + ' up"' + (idx <= 0 ? ' disabled' : '') + '>↑</button>' +
      '<button class="btn btn-icon usages-provider-order-btn" type="button" data-usage-provider-move="' + esc(row.key) + '" data-direction="down" title="Move ' + esc(label) + ' down" aria-label="Move ' + esc(label) + ' down"' + (idx >= total - 1 ? ' disabled' : '') + '>↓</button>' +
    '</div>';
  }

  function renderCopilotProviderRow(row, idx, total) {
    var available = !!row.available;
    var signedIn = !!row.enabled;
    if (!copilotLoginState.enterpriseHost && row.enterprise_host) copilotLoginState.enterpriseHost = row.enterprise_host;
    var status = signedIn && copilotLoginState.status === 'disconnected' ? 'signed_in' : copilotLoginState.status;
    var control = '';
    if (!available) {
      control = '<span class="settings-status">Available in upcoming release.</span>';
    } else if (status === 'signed_in') {
      control = '<span class="settings-status">Signed in</span><button class="btn btn-sm copilot-logout-btn" type="button">Sign out</button>';
    } else if (status === 'code_pending' || status === 'polling') {
      var code = copilotLoginState.userCode || '';
      var link = copilotLoginState.verificationURIComplete || copilotLoginState.verificationURI || '#';
      control = '<div class="copilot-login-flow">' +
        '<div class="copilot-code">' + esc(code) + '</div>' +
        '<div class="copilot-actions"><a class="btn btn-sm" href="' + esc(link) + '" target="_blank" rel="noopener noreferrer">Open verification page</a>' +
        '<button class="btn btn-sm copilot-copy-code" type="button">Copy code</button>' +
        '<button class="btn btn-sm copilot-cancel-btn" type="button">Cancel</button></div>' +
        '<span class="settings-status">' + esc(status === 'polling' ? 'Waiting for GitHub...' : 'Code ready') + '</span>' +
      '</div>';
    } else {
      var label = status === 'error' ? 'Try again' : 'Sign in with GitHub';
      control = '<button class="btn btn-sm copilot-login-btn" type="button">' + esc(label) + '</button>';
    }
    var error = status === 'error' && copilotLoginState.error ? '<span class="usages-error-line">' + esc(copilotLoginState.error) + '</span>' : '';
    return '<div class="settings-row copilot-provider-row' + (available ? '' : ' usages-provider-row-disabled') + '">' +
      '<div class="settings-row-info">' +
        '<label class="settings-label">GitHub Copilot</label>' +
        '<span class="settings-desc">Sign in with GitHub device flow and fetch Copilot quota.</span>' +
        error +
      '</div>' +
      '<div class="settings-control copilot-settings-control">' + usageProviderOrderControls(row, idx, total) +
        '<input type="text" class="settings-text-input copilot-enterprise-input" placeholder="https://github.example.com" value="' + esc(copilotLoginState.enterpriseHost || '') + '"' + (signedIn ? ' disabled' : '') + '>' +
        control +
      '</div>' +
    '</div>';
  }

  function renderOllamaProviderRow(row, idx, total) {
    var available = !!row.available;
    var configured = !!row.cookie_configured || !!row.api_key_configured || !!row.enabled;
    var authMode = row.auth_mode || 'auto';
    var provider = (usageProviders || []).find(function (p) { return p.provider_name === 'ollama-cloud'; });
    var snap = (provider && provider.profiles && provider.profiles[0]) || {};
    var status = snap.status || (configured ? 'saved' : 'not_configured');
    var hasError = status === 'auth_missing' || status === 'fetch_error';
    var showEditor = available && (!configured || hasError || ollamaCookieEditorOpen);
    var methods = [];
    if (row.cookie_configured) methods.push('cookie');
    if (row.api_key_configured) methods.push('API key');
    var statusText = 'Credentials not configured';
    if (!available) {
      statusText = 'Available in upcoming release.';
    } else if (hasError) {
      statusText = snap.error || 'Ollama Cloud fetch failed. Update credentials.';
    } else if (configured) {
      statusText = (methods.length ? methods.join(' + ') : 'Credentials') + ' configured' + (snap.last_refresh_at ? ' (last fetched ' + new Date(snap.last_refresh_at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) + ')' : '');
    }
    var editor = '';
    if (showEditor) {
      editor = '<select class="settings-text-input ollama-auth-mode-select">' +
          '<option value="auto"' + (authMode === 'auto' ? ' selected' : '') + '>Auto</option>' +
          '<option value="cookie"' + (authMode === 'cookie' ? ' selected' : '') + '>Cookie</option>' +
          '<option value="api_key"' + (authMode === 'api_key' ? ' selected' : '') + '>API key</option>' +
        '</select>' +
        '<input type="password" class="settings-text-input ollama-api-key-input" autocomplete="off" placeholder="' + (row.api_key_configured ? 'API key configured' : 'Paste an API key from ollama.com/settings/keys') + '">' +
        '<textarea class="settings-text-input ollama-cookie-input" rows="3" autocomplete="off" spellcheck="false" placeholder="' + (row.cookie_configured ? 'Cookie configured' : 'Paste the Cookie header from ollama.com/settings') + '"></textarea>' +
        '<div class="ollama-actions"><button class="btn btn-sm ollama-cookie-save-btn" type="button">Save</button>' +
        (configured ? '<button class="btn btn-sm ollama-cookie-cancel-btn" type="button">Cancel</button>' : '') + '</div>';
    } else if (configured) {
      editor = '<div class="ollama-actions"><button class="btn btn-sm ollama-cookie-update-btn" type="button">Update credentials</button><button class="btn btn-sm ollama-cookie-clear-btn" type="button">Clear</button></div>';
    }
    return '<div class="settings-row ollama-provider-row' + (available ? '' : ' usages-provider-row-disabled') + '">' +
      '<div class="settings-row-info">' +
        '<label class="settings-label">Ollama Cloud</label>' +
        '<span class="settings-desc">Use a Cookie header for quota windows, or an API key to verify Cloud access.</span>' +
        '<span class="settings-status">' + esc(statusText) + '</span>' +
      '</div>' +
      '<div class="settings-control ollama-settings-control">' + usageProviderOrderControls(row, idx, total) + editor + '</div>' +
    '</div>';
  }

  function renderMistralProviderRow(row, idx, total) {
    var available = !!row.available;
    var configured = !!row.cookie_configured || !!row.enabled;
    var provider = (usageProviders || []).find(function (p) { return p.provider_name === 'mistral'; });
    var snap = (provider && provider.profiles && provider.profiles[0]) || {};
    var status = snap.status || (configured ? 'saved' : 'not_configured');
    var hasError = status === 'auth_missing' || status === 'fetch_error';
    var showEditor = available && (!configured || hasError || mistralCookieEditorOpen);
    var statusText = 'Cookie not configured';
    if (!available) {
      statusText = 'Available in upcoming release.';
    } else if (hasError) {
      statusText = snap.error || 'Mistral fetch failed. Update credentials.';
    } else if (configured) {
      statusText = 'Cookie configured' + (snap.last_refresh_at ? ' (last fetched ' + new Date(snap.last_refresh_at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) + ')' : '');
    }
    var editor = '';
    if (showEditor) {
      editor = '<textarea class="settings-text-input mistral-cookie-input" rows="3" autocomplete="off" spellcheck="false" placeholder="' + (row.cookie_configured ? 'Cookie configured' : 'Paste the Cookie header from console.mistral.ai') + '"></textarea>' +
        '<div class="ollama-actions"><button class="btn btn-sm mistral-cookie-save-btn" type="button">Save</button>' +
        (configured ? '<button class="btn btn-sm mistral-cookie-cancel-btn" type="button">Cancel</button>' : '') + '</div>';
    } else if (configured) {
      editor = '<div class="ollama-actions"><button class="btn btn-sm mistral-cookie-update-btn" type="button">Update credentials</button><button class="btn btn-sm mistral-cookie-clear-btn" type="button">Clear</button></div>';
    }
    return '<div class="settings-row mistral-provider-row' + (available ? '' : ' usages-provider-row-disabled') + '">' +
      '<div class="settings-row-info">' +
        '<label class="settings-label">Mistral</label>' +
        '<span class="settings-desc">Use a Cookie header for Vibe monthly quota and optional API spend.</span>' +
        '<span class="settings-status">' + esc(statusText) + '</span>' +
      '</div>' +
      '<div class="settings-control ollama-settings-control">' + usageProviderOrderControls(row, idx, total) + editor + '</div>' +
    '</div>';
  }

  function saveUsageProviderToggle(provider, enabled) {
    api('POST', '/api/usages/providers', { provider: provider, enabled: enabled })
      .then(function () { return loadUsages(); })
      .catch(function (err) {
        alert('Failed to save usage provider: ' + (err && err.message ? err.message : err));
        loadUsages();
      });
  }

  function moveUsageProvider(provider, direction) {
    var rows = usageProviders || [];
    var order = rows.map(function (row) { return row.key; });
    var idx = order.indexOf(provider);
    if (idx === -1) return;
    var next = direction === 'up' ? idx - 1 : idx + 1;
    if (next < 0 || next >= order.length) return;
    var tmp = order[idx];
    order[idx] = order[next];
    order[next] = tmp;
    saveUsageProviderOrder(order);
  }

  function saveUsageProviderOrder(order) {
    api('POST', '/api/usages/settings', { provider_order: order })
      .then(function (resp) {
        if (resp && resp.settings) renderUsageSettings(resp.settings);
        return loadUsages();
      })
      .catch(function (err) {
        alert('Failed to save provider order: ' + (err && err.message ? err.message : err));
        loadUsages();
      });
  }

  function startCopilotLogin() {
    var input = usagesProviderRows && usagesProviderRows.querySelector('.copilot-enterprise-input');
    copilotLoginState.enterpriseHost = input ? input.value.trim() : '';
    copilotLoginState.status = 'polling';
    renderUsageProviderRows(usageProviders);
    api('POST', '/api/usages/copilot/login/start', { enterprise_host: copilotLoginState.enterpriseHost })
      .then(function (resp) {
        copilotLoginState.status = 'code_pending';
        copilotLoginState.flowId = resp.flow_id;
        copilotLoginState.userCode = resp.user_code;
        copilotLoginState.verificationURI = resp.verification_uri;
        copilotLoginState.verificationURIComplete = resp.verification_uri_complete;
        copilotLoginState.interval = resp.interval || 5;
        copilotLoginState.error = '';
        renderUsageProviderRows(usageProviders);
        scheduleCopilotPoll(500);
      })
      .catch(function (err) {
        copilotLoginState.status = 'error';
        copilotLoginState.error = err && err.message ? err.message : String(err);
        renderUsageProviderRows(usageProviders);
      });
  }

  function scheduleCopilotPoll(delayMs) {
    if (copilotLoginState.timer) clearTimeout(copilotLoginState.timer);
    copilotLoginState.timer = setTimeout(pollCopilotLogin, delayMs == null ? Math.max(1, copilotLoginState.interval || 5) * 1000 : delayMs);
  }

  function pollCopilotLogin() {
    if (!copilotLoginState.flowId) return;
    copilotLoginState.status = 'polling';
    renderUsageProviderRows(usageProviders);
    api('POST', '/api/usages/copilot/login/poll', { flow_id: copilotLoginState.flowId })
      .then(function (resp) {
        var status = resp.status || 'pending';
        if (status === 'success') {
          if (copilotLoginState.timer) clearTimeout(copilotLoginState.timer);
          copilotLoginState = { status: 'signed_in', enterpriseHost: copilotLoginState.enterpriseHost, flowId: '', interval: 5, timer: null, error: '' };
          return loadUsages();
        }
        if (status === 'pending' || status === 'slow_down') {
          copilotLoginState.status = 'code_pending';
          copilotLoginState.interval = resp.interval || copilotLoginState.interval || 5;
          renderUsageProviderRows(usageProviders);
          scheduleCopilotPoll(Math.max(1, resp.retry_after || copilotLoginState.interval) * 1000);
          return;
        }
        copilotLoginState.status = 'error';
        copilotLoginState.error = status === 'expired' ? 'Code expired. Start sign in again.' : status === 'denied' ? 'GitHub sign in was denied.' : (resp.error || 'GitHub sign in failed.');
        renderUsageProviderRows(usageProviders);
      })
      .catch(function (err) {
        copilotLoginState.status = 'error';
        copilotLoginState.error = err && err.message ? err.message : String(err);
        renderUsageProviderRows(usageProviders);
      });
  }

  function logoutCopilot() {
    api('POST', '/api/usages/copilot/login/logout', {})
      .then(function () {
        if (copilotLoginState.timer) clearTimeout(copilotLoginState.timer);
        copilotLoginState = { status: 'disconnected', enterpriseHost: copilotLoginState.enterpriseHost, flowId: '', interval: 5, timer: null, error: '' };
        return loadUsages();
      })
      .catch(function (err) {
        alert('Failed to sign out of GitHub Copilot: ' + (err && err.message ? err.message : err));
      });
  }

  function saveOllamaCookie() {
    var cookieInput = usagesProviderRows && usagesProviderRows.querySelector('.ollama-cookie-input');
    var apiKeyInput = usagesProviderRows && usagesProviderRows.querySelector('.ollama-api-key-input');
    var modeSelect = usagesProviderRows && usagesProviderRows.querySelector('.ollama-auth-mode-select');
    var payload = { auth_mode: modeSelect ? modeSelect.value : 'auto' };
    if (cookieInput && cookieInput.value.trim()) payload.cookie = cookieInput.value;
    if (apiKeyInput && apiKeyInput.value.trim()) payload.api_key = apiKeyInput.value;
    api('POST', '/api/usages/ollama/config', payload)
      .then(function () {
        if (cookieInput) cookieInput.value = '';
        if (apiKeyInput) apiKeyInput.value = '';
        ollamaCookieEditorOpen = false;
        return loadUsages();
      })
      .catch(function (err) {
        alert('Failed to save Ollama Cloud credentials: ' + (err && err.message ? err.message : err));
        if (apiKeyInput) apiKeyInput.value = '';
      });
  }

  function clearOllamaCookie() {
    api('POST', '/api/usages/ollama/config', { auth_mode: 'auto', api_key: '', cookie: '' })
      .then(function () {
        ollamaCookieEditorOpen = false;
        return loadUsages();
      })
      .catch(function (err) {
        alert('Failed to clear Ollama Cloud credentials: ' + (err && err.message ? err.message : err));
      });
  }

  function saveMistralCookie() {
    var cookieInput = usagesProviderRows && usagesProviderRows.querySelector('.mistral-cookie-input');
    var payload = {};
    if (cookieInput && cookieInput.value.trim()) payload.cookie = cookieInput.value;
    api('POST', '/api/usages/mistral/config', payload)
      .then(function () {
        if (cookieInput) cookieInput.value = '';
        mistralCookieEditorOpen = false;
        return loadUsages();
      })
      .catch(function (err) {
        alert('Failed to save Mistral credentials: ' + (err && err.message ? err.message : err));
      });
  }

  function clearMistralCookie() {
    api('POST', '/api/usages/mistral/config', { cookie: '' })
      .then(function () {
        mistralCookieEditorOpen = false;
        return loadUsages();
      })
      .catch(function (err) {
        alert('Failed to clear Mistral credentials: ' + (err && err.message ? err.message : err));
      });
  }

  function saveUsageRefreshInterval() {
    if (!usagesRefreshInput) return;
    var value = parseInt(usagesRefreshInput.value, 10);
    if (!Number.isFinite(value) || value < 15) value = 15;
    usagesRefreshInput.value = value;
    api('POST', '/api/usages/settings', { refresh_interval_sec: value, percent_display: usagePercentDisplay })
      .then(function (resp) {
        if (resp && resp.settings) renderUsageSettings(resp.settings);
      })
      .catch(function (err) {
        alert('Failed to save usage refresh interval: ' + (err && err.message ? err.message : err));
      });
  }

  function startUsageRefreshCooldown(seconds) {
    if (!usagesRefreshBtn) return;
    if (usageCooldownTimer) clearTimeout(usageCooldownTimer);
    var remaining = Math.max(1, seconds || 15);
    function tick() {
      usagesRefreshBtn.disabled = true;
      usagesRefreshBtn.textContent = String(remaining);
      remaining--;
      if (remaining < 0) {
        usagesRefreshBtn.disabled = false;
        usagesRefreshBtn.innerHTML = '<span class="usages-refresh-glyph" aria-hidden="true"></span>';
      } else {
        usageCooldownTimer = setTimeout(tick, 1000);
      }
    }
    tick();
  }

  function saveUsagePercentDisplay(value) {
    usagePercentDisplay = value === 'used' ? 'used' : 'left';
    var interval = usagesRefreshInput ? parseInt(usagesRefreshInput.value, 10) : 120;
    if (!Number.isFinite(interval) || interval < 15) interval = 15;
    api('POST', '/api/usages/settings', { refresh_interval_sec: interval, percent_display: usagePercentDisplay })
      .then(function (resp) {
        if (resp && resp.settings) renderUsageSettings(resp.settings);
        return loadUsages();
      })
      .catch(function (err) {
        alert('Failed to save percent display: ' + (err && err.message ? err.message : err));
      });
  }

  function forceRefreshUsages() {
    if (!usagesRefreshBtn || usagesRefreshBtn.disabled) return;
    // Start the cooldown on click, not on response. A force refresh fetches
    // every provider live and can take seconds; starting it in .then() leaves
    // the button enabled for that whole window, so a second click fires a
    // second POST that the server rejects — and the countdown only appears
    // then, after a click that appeared to do nothing.
    startUsageRefreshCooldown(15);
    api('POST', '/api/usages/refresh', {})
      .then(function (resp) {
        renderUsages(resp);
      })
      .catch(function (err) {
        var retryAfter = err && err.body && err.body.retry_after_sec;
        if (Number.isFinite(Number(retryAfter)) && Number(retryAfter) > 0) {
          startUsageRefreshCooldown(Number(retryAfter));
          return;
        }
        var msg = String(err && err.message || err);
        alert('Failed to refresh usages: ' + msg);
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

  // Lazy loading: page older messages in on scroll-up. Per-room flags
  // gate concurrent/exhausted loads. The block comes back newest-first (like the
  // initial fetch) and is reversed to oldest-first, then prepended; renderMessages
  // restores the viewport via the scroll anchor (no hand-computed scroll offsets).
  var loadingOlder = {};
  var noMoreOlder = {};
  function fetchOlderMessages(roomID) {
    if (!roomID || loadingOlder[roomID] || noMoreOlder[roomID]) return;
    var arr = messages[roomID] || [];
    if (!arr.length) return;
    var oldestID = arr[0].id; // messages are oldest-first
    loadingOlder[roomID] = true;
    var path = '/rooms/' + encodeURIComponent(roomID) + '/messages?before_id=' + oldestID + '&limit=50';
    if (agentID && agentID !== 'user') path += '&agent_id=' + encodeURIComponent(agentID);
    return api('GET', path).then(function (data) {
      // Staleness guard: fetchRoomMessages REPLACES messages[roomID] (doesn't
      // mutate), so if the room was re-fetched mid-flight (room switch/back),
      // `arr` is a stale reference. Re-read and bail on identity mismatch rather
      // than clobbering the fresh data. (appendMessage mutates in place via push,
      // so it doesn't trip this — `cur === arr`.)
      var cur = messages[roomID] || [];
      if (cur !== arr) return;
      var older = data.messages || [];
      older.reverse(); // API is newest-first; prepend oldest-first
      if (older.length < 50) noMoreOlder[roomID] = true; // exhausted history
      if (!older.length) {
        // History exhausted with zero new messages: the latch just flipped
        // false→true (the guard at the top bails when already true), so
        // re-render to surface the start-of-room divider now rather than
        // waiting for an unrelated re-render.
        renderMessages();
        // renderMessages()'s scroll-anchor compensation shifts scrollTop up by
        // the prepended divider's height to hold the oldest message in place —
        // which hides the divider just above the viewport. The user reached
        // this path by scrolling to the top and nothing but the divider was
        // added, so there's no reading position to protect: snap to 0 so the
        // "start of room" line is actually visible. (The partial-batch path
        // below stays untouched — real messages were prepended there and the
        // anchor contract is correct.)
        messageListEl.scrollTop = 0;
        return;
      }
      var have = {};
      cur.forEach(function (m) { have[m.id] = true; });
      var fresh = older.filter(function (m) { return !have[m.id]; });
      if (!fresh.length) return;
      messages[roomID] = fresh.concat(cur);
      fresh.forEach(function (m) { if (m.id > maxSeenMsgID) maxSeenMsgID = m.id; });
      renderMessages(); // the scroll anchor keeps the viewport on the same message
    }).catch(function (err) {
      console.error('older messages', err);
    }).finally(function () {
      loadingOlder[roomID] = false;
    });
  }

  // Cache purge: bound messages[roomID] in memory. Eviction runs only
  // when the user is scrolled to the bottom, so the anchor is always below the
  // evicted range — an invariant, not something derived from the floor. The
  // newest end is never evicted: before_id pages backwards only, so a downward
  // gap could never be refilled, and the memory it would save is reclaimed
  // anyway the moment the user returns to the bottom. The floor is well above a
  // screenful so a pruned list can't show its top without scrolling — otherwise
  // the scroll handler would immediately re-fetch what was just dropped.
  var MESSAGE_CACHE_CAP = 500;
  var MESSAGE_CACHE_FLOOR = 200;
  function pruneMessageCache(roomID) {
    var arr = messages[roomID];
    if (!arr || arr.length <= MESSAGE_CACHE_CAP) return false;
    // No eviction when pinned to the top — explicit: there's no safe end (the
    // top is the anchor; the bottom is where new messages arrive).
    if (messageListEl.scrollTop < 50) return false;
    // Evict the top (oldest) ONLY when at the bottom — the only position where
    // the evicted end is off-screen AND refillable (before_id pages back). Never
    // evict when scrolled up: the bottom (newest) can't be refilled (no
    // after_id), and the top is the anchor's neighborhood. So the anchor is
    // always below the evicted range — an invariant, not derived from a count.
    if (!isMessageListNearBottom()) return false;
    var k = arr.length - MESSAGE_CACHE_FLOOR;
    messages[roomID] = arr.slice(k);
    noMoreOlder[roomID] = false; // dropped the oldest → "seen the beginning" no longer holds
    return true;
  }

  function fetchRoomMessages(roomID) {
    var path = '/rooms/' + encodeURIComponent(roomID) + '/messages?limit=100';
    if (agentID && agentID !== 'user') path += '&agent_id=' + encodeURIComponent(agentID);
    return api('GET', path).then(function (data) {
      // Refresh clock offset from advisory server_time (if present).
      if (data.server_time) {
        var sMs = new Date(data.server_time).getTime();
        if (Number.isFinite(sMs)) clockOffsetMs = sMs - Date.now();
      }
      messages[roomID] = data.messages || [];
      // Replaced the array (newest 100) — the oldest loaded messages are gone,
      // so the "seen the beginning" latch no longer holds. Clear it so lazy
      // loading works on the next scroll-up. Same invariant as pruneMessageCache.
      noMoreOlder[roomID] = false;
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
      roomPrefs = {};
      list.forEach(function (r) {
        unreadCounts[r.id] = r.unread_count || 0;
        readCursors[r.id] = r.read_cursor || 0;
        attentionCounts[r.id] = r.attention_unread_count || 0;
        if (r.hidden || r.pinned) {
          roomPrefs[r.id] = { hidden: !!r.hidden, pinned: !!r.pinned };
        }
      });
      updateTitleAttention();
      renderRooms();
    }).catch(function (err) {
      console.error('Failed to fetch agent rooms:', err);
    });
  }

  function loadAgentSessions() {
    return api('GET', '/agent-sessions').then(function (data) {
      agentSessions = {};
      (data.agent_sessions || []).forEach(function (sess) {
        if (sess && sess.full_id) agentSessions[sess.full_id] = sess;
      });
      renderRightSidebar();
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

  function attachmentURL(id) {
    return '/api/attachments/' + encodeURIComponent(id);
  }

  function attachmentSummary(a) {
    var bits = [];
    if (a.mime) bits.push(a.mime.replace(/^image\//, '').toUpperCase());
    if (a.size) bits.push(formatBytes(a.size));
    return bits.join(' · ');
  }

  function formatBytes(size) {
    if (!size) return '0 B';
    if (size < 1024) return size + ' B';
    if (size < 1024 * 1024) return Math.round(size / 1024) + ' KB';
    return (size / 1024 / 1024).toFixed(1) + ' MB';
  }

  function hasAttachmentUploadsInFlight() {
    return pendingAttachments.some(function (item) { return item.status === 'uploading'; });
  }

  function readyPendingAttachments() {
    return pendingAttachments
      .filter(function (item) { return item.status === 'ready' && item.attachment; })
      .map(function (item) { return { id: item.attachment.id }; });
  }

  function updateSendAvailability() {
    if (sendButton) sendButton.disabled = hasAttachmentUploadsInFlight();
  }

  function renderPendingAttachments() {
    if (!attachmentPendingList) return;
    if (!pendingAttachments.length) {
      attachmentPendingList.classList.add('hidden');
      attachmentPendingList.innerHTML = '';
      updateSendAvailability();
      return;
    }
    attachmentPendingList.classList.remove('hidden');
    attachmentPendingList.innerHTML = pendingAttachments.map(function (item) {
      var name = item.attachment ? item.attachment.name : item.file.name;
      var status = item.status === 'uploading' ? 'Uploading...' : item.status === 'error' ? item.error : attachmentSummary(item.attachment);
      return (
        '<div class="attachment-pending-item ' + esc(item.status) + '" data-local-id="' + esc(item.localID) + '">' +
          '<img src="' + esc(item.url) + '" alt="" class="attachment-pending-thumb">' +
          '<div class="attachment-pending-meta"><div class="attachment-pending-name">' + esc(name) + '</div><div class="attachment-pending-status">' + esc(status || '') + '</div></div>' +
          '<button class="attachment-remove-btn" type="button" title="Remove attachment" aria-label="Remove attachment">×</button>' +
        '</div>'
      );
    }).join('');
    updateSendAvailability();
  }

  function uploadAttachmentItem(item) {
    var fd = new FormData();
    fd.append('file', item.file, item.file.name);
    return fetch('/api/attachments', { method: 'POST', body: fd })
      .then(function (r) {
        if (!r.ok) {
          return r.text().then(function (t) { throw new Error(t || ('HTTP ' + r.status)); });
        }
        return r.json();
      })
      .then(function (attachment) {
        item.status = 'ready';
        item.attachment = attachment;
        renderPendingAttachments();
      })
      .catch(function (err) {
        item.status = 'error';
        item.error = err && err.message ? err.message.replace(/^.*"error":"?/, '').replace(/"?}$/, '') : 'Upload failed';
        renderPendingAttachments();
      });
  }

  function addAttachmentFiles(fileList) {
    var files = Array.from(fileList || []).filter(function (file) {
      return file && /^image\/(png|jpeg|gif|webp)$/.test(file.type || '');
    });
    if (!files.length) return;
    files.forEach(function (file) {
      if (pendingAttachments.length >= 4) return;
      var item = {
        localID: String(Date.now()) + '-' + String(Math.random()).slice(2),
        status: 'uploading',
        file: file,
        url: URL.createObjectURL(file),
      };
      pendingAttachments.push(item);
      uploadAttachmentItem(item);
    });
    renderPendingAttachments();
  }

  function removePendingAttachment(localID) {
    var idx = pendingAttachments.findIndex(function (item) { return item.localID === localID; });
    if (idx < 0) return;
    var item = pendingAttachments[idx];
    pendingAttachments.splice(idx, 1);
    if (item.url) URL.revokeObjectURL(item.url);
    if (item.attachment && item.attachment.id) {
      fetch(attachmentURL(item.attachment.id), { method: 'DELETE' }).catch(function () {});
    }
    renderPendingAttachments();
  }

  function clearPendingAttachments() {
    pendingAttachments.forEach(function (item) {
      if (item.url) URL.revokeObjectURL(item.url);
    });
    pendingAttachments = [];
    renderPendingAttachments();
  }

  function attachmentsHTML(m) {
    var attachments = Array.isArray(m.attachments) ? m.attachments : [];
    if (!attachments.length) return '';
    var html = '<div class="message-attachments">';
    attachments.forEach(function (a) {
      var ratio = a.width && a.height ? ' style="aspect-ratio:' + esc(String(a.width)) + '/' + esc(String(a.height)) + '"' : '';
      html += '<button class="message-attachment" type="button" data-attachment-id="' + esc(a.id) + '" data-attachment-name="' + esc(a.name || 'attachment') + '" data-attachment-mime="' + esc(a.mime || '') + '" data-attachment-size="' + esc(String(a.size || 0)) + '">';
      html += '<img src="' + esc(attachmentURL(a.id)) + '" alt="' + esc(a.name || 'attachment') + '"' + ratio + '>';
      html += '<span>' + esc(a.name || 'attachment') + '</span>';
      html += '</button>';
    });
    html += '</div>';
    return html;
  }

  var reactionQuickPick = ['👍', '🆗', '✅', '👀', '🙏', '🎉'];

  function reactionSummariesWithLocalMe(reactions) {
    if (!Array.isArray(reactions)) return [];
    return reactions.map(function (r) {
      if (!r || !r.emoji || !r.count) return null;
      var agents = Array.isArray(r.agents) ? r.agents.filter(Boolean).map(String) : [];
      return Object.assign({}, r, {
        agents: agents,
        me: agents.length ? agents.indexOf(agentID) !== -1 : !!r.me,
      });
    }).filter(Boolean);
  }

  function reactionTitle(r, roomID) {
    var agents = Array.isArray(r.agents) ? r.agents : [];
    var who = agents.length
      ? agents.map(function (agentIDVal) { return reactionAgentLabel(agentIDVal, roomID); }).join(', ')
      : String(r.count || 0) + ' reaction' + (r.count === 1 ? '' : 's');
    return who + '\n' + (r.me ? 'Click to remove reaction' : 'Click to add reaction');
  }

  function reactionPickerHTML(m) {
    var html = '<details class="message-reaction-picker message-reaction-add">' +
      '<summary title="Add reaction" aria-label="Add reaction">+</summary>' +
      '<div class="message-reaction-menu">';
    reactionQuickPick.forEach(function (emoji) {
      html += '<button class="message-reaction-option" type="button" data-msg-id="' + esc(String(m.id)) + '" data-emoji="' + esc(emoji) + '">' + esc(emoji) + '</button>';
    });
    html += '</div></details>';
    return html;
  }

  function messageControlsHTML(m) {
    return '<div class="message-bubble-controls">' +
      reactionPickerHTML(m) +
      '<button class="message-reply-add chat-msg-reply" type="button" data-msg-id="' + esc(String(m.id)) + '" aria-label="Reply to #' + esc(String(m.id)) + '" title="Reply">↩</button>' +
      '</div>';
  }

  function reactionsHTML(m) {
    var reactions = reactionSummariesWithLocalMe(m.reactions);
    var validReactions = reactions.filter(function (r) { return r && r.emoji && r.count; });
    if (!validReactions.length) return '';
    var html = '<div class="message-reactions" data-msg-id="' + esc(String(m.id)) + '">';
    validReactions.forEach(function (r) {
      html += '<button class="message-reaction-pill' + (r.me ? ' mine' : '') + '" type="button" data-msg-id="' + esc(String(m.id)) + '" data-emoji="' + esc(r.emoji) + '" title="' + esc(reactionTitle(r, m.room_id)) + '">' +
        '<span class="message-reaction-emoji">' + esc(r.emoji) + '</span>' +
        '<span class="message-reaction-count">' + esc(String(r.count)) + '</span>' +
      '</button>';
    });
    html += '</div>';
    return html;
  }

  function setMessageReactions(roomID, messageID, reactions) {
    var msg = findMessageInRoom(roomID, messageID);
    if (!msg) return;
    msg.reactions = reactionSummariesWithLocalMe(reactions);
    patchReactionRow(msg);
  }

  function patchReactionRow(msg) {
    if (!activeRoomID || !messageListEl) return;
    var bubble = messageListEl.querySelector('.chat-msg[data-id="' + String(msg.id) + '"] .chat-msg-bubble');
    if (!bubble) return;
    var temp = document.createElement('div');
    temp.innerHTML = reactionsHTML(msg);
    preserveMessageListScrollForMutation(function () {
      var node = bubble.querySelector('.message-reactions');
      var next = temp.firstElementChild;
      if (node && next) {
        node.replaceWith(next);
      } else if (node && !next) {
        node.remove();
      } else if (!node && next) {
        var controls = bubble.querySelector('.message-bubble-controls');
        bubble.insertBefore(next, controls || null);
      }
    });
  }

  function applyReactionEvent(data) {
    if (!data || !data.room_id || !data.message_id || !data.emoji) return;
    if (data.agent_id === agentID) return;
    if (!Array.isArray(data.reactions)) return;
    setMessageReactions(data.room_id, data.message_id, data.reactions);
  }

  function sendReaction(messageID, emoji, remove) {
    return ensureRegistered().then(function () {
      return api(remove ? 'DELETE' : 'PUT', '/messages/' + encodeURIComponent(messageID) + '/reactions', {
        agent_id: agentID,
        emoji: emoji,
      });
    }).then(function (data) {
      if (data && activeRoomID) {
        setMessageReactions(activeRoomID, messageID, Array.isArray(data.reactions) ? data.reactions : []);
      }
    }).catch(function (err) {
      console.error('Failed to update reaction:', err);
    });
  }

  function openAttachmentLightbox(id, name) {
    if (!attachmentLightboxModal) return;
    attachmentLightboxModal.classList.remove('hidden');
    if (attachmentLightboxTitle) attachmentLightboxTitle.textContent = name || 'Attachment';
    if (attachmentLightboxImg) {
      attachmentLightboxImg.src = attachmentURL(id);
      attachmentLightboxImg.alt = name || 'Attachment';
    }
    document.body.style.overflow = 'hidden';
  }

  function closeAttachmentLightbox() {
    if (!attachmentLightboxModal) return;
    attachmentLightboxModal.classList.add('hidden');
    if (attachmentLightboxImg) attachmentLightboxImg.src = '';
    updateBodyModalLock();
  }

  function sendMessage(body, attachments, replyTo) {
    if (!activeRoomID) return;
    return ensureRegistered().then(function () {
      return api('POST', '/rooms/' + encodeURIComponent(activeRoomID) + '/send', {
        from: agentID,
        body: body,
        attachments: attachments || undefined,
        reply_to: replyTo || undefined,
      });
    }).catch(function (err) {
      console.error('Failed to send message:', err);
    });
  }

  // ── Settings ─────────────────────────────────────────────────────

  function applyTheme(theme) {
    if (theme && theme !== 'dark') {
      document.documentElement.setAttribute('data-theme', theme);
    } else {
      document.documentElement.removeAttribute('data-theme');
    }
    if (themeSelect) {
      themeSelect.value = theme;
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

  function applyRetentionSettings() {
    if (retentionStaleAgentInput) retentionStaleAgentInput.value = serverSettings.stale_agent_window_seconds || 1800;
    if (retentionLivenessSweepInput) retentionLivenessSweepInput.value = serverSettings.liveness_sweep_seconds || 15;
    if (retentionAgentStaleInput) retentionAgentStaleInput.value = serverSettings.agent_stale_window_seconds || 90;
    if (retentionAgentOfflineInput) retentionAgentOfflineInput.value = serverSettings.agent_offline_window_seconds || 300;
    if (retentionCleanupIntervalInput) retentionCleanupIntervalInput.value = serverSettings.cleanup_interval_seconds || 60;
    if (retentionExpiryInput) retentionExpiryInput.value = (serverSettings.messages_considered_expired_after_seconds != null) ? serverSettings.messages_considered_expired_after_seconds : 21600;
  }

  function saveRetentionSetting(field, input) {
    if (!input) return;
    input.setCustomValidity('');
    var value = parseInt(input.value, 10);
    if (!Number.isFinite(value)) return;
    var ok = input.checkValidity();
    if (field === 'agent_stale_window_seconds') {
      var offlineValue = parseInt(retentionAgentOfflineInput && retentionAgentOfflineInput.value, 10);
      if (Number.isFinite(offlineValue) && value >= offlineValue) {
        input.setCustomValidity('Use a value lower than the offline alert threshold.');
        ok = false;
      }
    }
    if (field === 'agent_offline_window_seconds') {
      var staleValue = parseInt(retentionAgentStaleInput && retentionAgentStaleInput.value, 10);
      if (Number.isFinite(staleValue) && value <= staleValue) {
        input.setCustomValidity('Use a value higher than the stale badge threshold.');
        ok = false;
      }
    }
    // Expiry: 0 means never expire; otherwise 60..2592000 (mirrors server validation in settings.go).
    if (field === 'messages_considered_expired_after_seconds' && value !== 0 && (value < 60 || value > 2592000)) {
      input.setCustomValidity('Use 0 for never, or a value from 60 to 2592000.');
      ok = false;
    }
    if (!ok) {
      input.reportValidity();
      applyRetentionSettings();
      return;
    }
    var patch = {};
    patch[field] = value;
    saveSettings(patch).then(function () {
      applyRetentionSettings();
    });
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
      // agent_id_default: pre-fill the welcome gate only when nothing was
      // stored locally. It never starts a session until the user submits.
      if (!agentIDFromStorage && serverSettings.agent_id_default) {
        welcomeNameInput.value = serverSettings.agent_id_default;
        refreshWelcomeSubmit();
      }
      if (agentIDDefaultInput) {
        agentIDDefaultInput.value = serverSettings.agent_id_default || '';
      }
      applyDebugButtonSetting(!!serverSettings.debug_button_enabled);
      applyNotificationSettings();
      applyRetentionSettings();
      applyMemorySettings();
      applyLeaderboardSettings();
      applyAgentsBehaviourSettings();
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
    loadFleets();
    loadPrompts();
    loadRoles();
    updateBodyModalLock();
    if (settingsCloseBtn && settingsCloseBtn.focus) settingsCloseBtn.focus();
  }

  function closeSettings() {
    settingsModal.classList.add('hidden');
    updateBodyModalLock();
  }

  function openRoomSettings() {
    if (!activeRoomID || !roomSettingsModal) return;
    roomSettingsModal.classList.remove('hidden');
    renderRoomSettings({ force: true });
    updateBodyModalLock();
  }

  function closeRoomSettings() {
    if (!roomSettingsModal) return;
    roomSettingsDirty = false;
    roomSettingsModal.classList.add('hidden');
    updateBodyModalLock();
  }

  function handleLocalRoomRemoval(roomID) {
    closeRoomSettings();
    rooms = rooms.filter(function (r) { return r.id !== roomID; });
    delete messages[roomID];
    delete presence[roomID];
    delete attentionCounts[roomID];
    if (attentionTimers[roomID]) {
      clearTimeout(attentionTimers[roomID]);
      delete attentionTimers[roomID];
    }
    if (attentionFocusListeners[roomID]) {
      window.removeEventListener('focus', attentionFocusListeners[roomID]);
      delete attentionFocusListeners[roomID];
    }
    delete unreadCounts[roomID];
    delete readCursors[roomID];
    delete lastMessagePreview[roomID];

    if (activeRoomID === roomID) {
      wsUnsubscribeRoom(roomID);
      showNoRoom();
      return;
    }
    renderRooms();
  }

  function activateSettingsSection(section) {
    settingsModal.querySelectorAll('.settings-nav-item').forEach(function (el) {
      el.classList.toggle('active', el.getAttribute('data-section') === section);
    });
    settingsModal.querySelectorAll('.settings-section').forEach(function (el) {
      el.classList.toggle('active', el.getAttribute('data-section') === section);
    });
    var titles = { general: 'General', retention: 'Retention', agents: 'Agents', notifications: 'Notifications', usages: 'Usages', macros: 'Macros', fleets: 'Fleets', memory: 'Memory', prompts: 'Prompts', roles: 'Roles', switcher: 'Switcher', danger: 'Danger Zone' };
    if (settingsSectionTitle) settingsSectionTitle.textContent = titles[section] || section;
    if (section === 'usages') loadUsages();
    if (section === 'switcher') loadSwitcher();
    if (section === 'fleets') loadFleets();
    if (section === 'memory') applyMemorySettings();
  }

  function exportBackup() {
    var payload = { settings: serverSettings, macros: macros, fleets: fleets };
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
        applyMemorySettings();
        applyLeaderboardSettings();
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
      if (data.fleets && typeof data.fleets === 'object') {
        promises.push(api('POST', '/fleets/import', { version: 1, fleets: data.fleets }).then(loadFleets));
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
    renderRoomSettings();
    closeMessageDebugModal();
  }

  function clearState() {
    return api('DELETE', '/all').then(resetLocalState);
  }

  function clearAll() {
    return api('DELETE', '/all?include_settings=true').then(function () {
      resetLocalState();
      macros = {};
      fleets = {};
      serverSettings = {};
      attentionCounts = {};
      updateTitleAttention();
      localStorage.removeItem('aimebu.ui.theme');
      applyTheme('dark');
      applyShowSystemEvents(true);
      applyDebugButtonSetting(false);
      applyMemorySettings();
      applyLeaderboardSettings();
      applyNotificationSettings();
      renderMacrosList();
      renderFleetsList();
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
          case 'reaction':
            applyReactionEvent(frame.data);
            break;
          case 'presence':
            handleWSPresence(frame.data);
            break;
          case 'macros_updated':
            loadMacros().catch(function () {});
            break;
          case 'roles_updated':
            loadRoles().catch(function () {});
            break;
          case 'fleets_updated':
            loadFleets().catch(function () {});
            break;
          case 'leaderboard_updated':
            if (leaderboardViewerModal && !leaderboardViewerModal.classList.contains('hidden')) loadLeaderboard().catch(function () {});
            break;
          case 'usages_updated':
            renderUsages(frame.data);
            if (frame.data && frame.data.settings) renderUsageSettings(frame.data.settings);
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
      if (msg.from_kind !== 'system') {
        renderMessages();
      } else {
        renderReadReceipts();
      }
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
    renderRoomSettings();
    renderMessageDebugModal();
    // Re-fetch per-human prefs (hidden/pinned) so join-unhides reflect immediately.
    // fetchMyRooms calls renderRooms itself, so skip the renderRooms above if it
    // resolves fast enough — but the WS-driven render is the immediate paint and
    // can't wait for the HTTP round-trip. The second render from fetchMyRooms is
    // the correctness pass (prefs may have changed server-side on join).
    fetchMyRooms().catch(function () {});
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
    loadAgentSessions().catch(function () {});
    renderAllAgents();
    renderRoomAgents();
    renderRoomSettings();
    renderMessageDebugModal();
    refreshMessageSenderPresence();
  }

  // ── Room selection ───────────────────────────────────────────────

  function selectRoom(roomID, scrollToMsgID) {
    if (activeRoomID === roomID) {
      if (scrollToMsgID) scrollToMessage(scrollToMsgID);
      return;
    }
    closeMessageDebugModal();
    activeRoomID = roomID;
    // Reset to a clean pinned state so stale anchor from the previous room
    // cannot be applied to this room's content during initial load.
    scrollAnchor.pinnedToBottom = true;
    scrollAnchor.anchorEl = null;
    scrollAnchor.visibleOffset = 0;
    scrollAnchor.distanceFromBottom = 0;
    scrollAnchor.hasListSize = false;
    historyIdx = null;
    historyDraft = null;
    clearPendingReply();

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
    renderPendingReply();
    fetchRoomMessages(roomID).then(function () {
      renderPendingReply();
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
    renderRoomSettings();

    // On mobile, switch to chat tab
    setMobileTab('chat');
    maybeShowPendingNotificationPrompt();
  }

  function showNoRoom() {
    closeMessageDebugModal();
    closeRoomSettings();
    activeRoomID = null;
    closeProfilePanel();
    noRoomView.classList.remove('hidden');
    roomView.classList.add('hidden');
    roomAgentsList.innerHTML = '<div class="empty-state">Select a room to see members.</div>';
    renderRoomSettings();
    renderRooms();
  }

  function updateRoomHeader() {
    if (!activeRoomID) return;
    var room = rooms.find(function (r) { return r.id === activeRoomID; });
    if (!room) return;

    var dm = isDM(room.id);
    roomIconEl.textContent = dm ? '@' : '#';
    roomIconEl.className = 'room-header-icon' + (dm ? ' dm' : '');
    if (roomSettingsBtn) roomSettingsBtn.disabled = room.id === '_system';
    if (dm) {
      var others = (room.members || []).filter(function (m) { return m !== agentID; });
      roomNameEl.textContent = others.length > 0 ? others[0] : room.id;
    } else {
      roomNameEl.textContent = room.id;
    }
    var members = room.members || [];
    var memberLabel = members.length + ' member' + (members.length !== 1 ? 's' : '');
    roomMemberCountNum.textContent = members.length;
    roomMemberCount.title = memberLabel;
    roomMemberCount.setAttribute('aria-label', memberLabel);

  }

  // ── Render rooms ─────────────────────────────────────────────────

  function renderRooms() {
    var visibleRooms = rooms.filter(function (r) {
      return r.id === '_system' || !(roomPrefs[r.id] && roomPrefs[r.id].hidden);
    });
    var channelRooms = visibleRooms.filter(function (r) { return !isDM(r.id) && r.id !== '_system'; });
    var dmRooms = visibleRooms.filter(function (r) { return isDM(r.id); });

    function sortList(list) {
      return list.slice().sort(function (a, b) {
        var pa = !!(roomPrefs[a.id] && roomPrefs[a.id].pinned);
        var pb = !!(roomPrefs[b.id] && roomPrefs[b.id].pinned);
        if (pa !== pb) return pb - pa;
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
      var isPinned = !!(roomPrefs[r.id] && roomPrefs[r.id].pinned);
      var displayName = dm
        ? (members.length > 0 ? members.join(' · ') : r.id)
        : r.id;
      var icon = dm ? '@' : '#';
      var pinGlyph = isPinned ? '<svg class="room-item-pin-icon" width="22" height="22" viewBox="-7.5 0 32 32" fill="currentColor" aria-hidden="true"><path d="M16.52 12.72l-4.84-4.84c-0.16-0.16-0.44-0.28-0.68-0.24s-0.48 0.16-0.6 0.36l-3.52 5.16-3.72 0.84c-0.28 0.080-0.52 0.28-0.64 0.6-0.080 0.28 0 0.6 0.2 0.84l2.52 2.52-5 5c-0.32 0.32-0.32 0.84 0 1.2 0.16 0.16 0.36 0.24 0.6 0.24s0.44-0.080 0.6-0.24l5.040-5.040 2.52 2.52c0.16 0.16 0.36 0.24 0.6 0.24 0.080 0 0.16 0 0.24-0.040 0.28-0.080 0.52-0.32 0.6-0.64l0.84-3.72 5.16-3.48c0.2-0.16 0.32-0.36 0.36-0.6-0.040-0.28-0.080-0.52-0.28-0.68zM10.080 16.32c-0.16 0.12-0.28 0.32-0.36 0.52l-0.6 2.56-4.12-4.12 2.56-0.56c0.2-0.040 0.4-0.16 0.52-0.36l3.12-4.56 3.4 3.4-4.52 3.12z"/></svg>' : '';
      return (
        '<div class="room-item' +
          (isActive ? ' active' : '') +
          (dm ? ' dm' : '') +
          (hasUnread ? ' has-unread' : '') +
          (attention > 0 ? ' has-attention' : '') +
          (isPinned ? ' pinned' : '') +
          '" data-room-id="' + esc(r.id) + '">' +
          '<span class="room-item-icon-group">' +
            '<span class="room-item-icon">' + icon + '</span>' +
            (pinGlyph ? '<span class="room-item-pin">' + pinGlyph + '</span>' : '') +
          '</span>' +
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
    var roleNames = assignedRoleMentionItems(room).map(function (item) { return item.insertText.slice(1); });
    memberNames.concat(specialMentionItems.map(function (item) { return item.token; })).concat(roleNames).forEach(function (name) {
      var key = name.toLowerCase();
      if (seen[key]) return;
      seen[key] = true;
      names.push(key);
    });
    if (names.length === 0) return null;
    var atNames = names.sort(function (a, b) { return b.length - a.length; }).map(escRe);
    return new RegExp('@(' + atNames.join('|') + ')(?![a-z0-9_-])', 'gi');
  }

  function highlightNames(rootEl) {
    var re = buildHighlightRegex();
    if (!re) return;
    var room = activeRoom();
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
        if (mentionTargetsViewer(m[1], room)) span.classList.add('mention-self');
        var mentionAgentID = resolveMentionAgentID(m[1]);
        if (mentionAgentID) {
          span.classList.add('agent-profile-link');
          span.setAttribute('data-profile-agent-id', mentionAgentID);
          span.setAttribute('data-profile-context', 'room');
          span.setAttribute('tabindex', '0');
          span.setAttribute('role', 'button');
          span.setAttribute('aria-label', 'Show profile for ' + mentionAgentID);
        }
        frag.appendChild(span);
        last = m.index + m[0].length;
      }
      if (last < text.length) frag.appendChild(document.createTextNode(text.slice(last)));
      textNode.parentNode.replaceChild(frag, textNode);
    });
  }

  function messageScrollItems() {
    return Array.from(messageListEl.querySelectorAll('.chat-msg, .chat-msg-system'));
  }

  function isMessageListNearBottom() {
    return (messageListEl.scrollHeight - messageListEl.scrollTop - messageListEl.clientHeight) < scrollBottomThreshold;
  }

  function suppressMessageListScroll(fn, refreshAnchorAfterWrite) {
    suppressScrollAnchor = true;
    suppressScrollAnchorToken++;
    var token = suppressScrollAnchorToken;
    fn();
    requestAnimationFrame(function () {
      if (token !== suppressScrollAnchorToken) return;
      suppressScrollAnchor = false;
      if (refreshAnchorAfterWrite) updateScrollAnchor(true);
    });
  }

  function updateScrollAnchor(allowListSizeChange) {
    if (!messageListEl) return;
    var listWidth = messageListEl.clientWidth;
    var listHeight = messageListEl.clientHeight;
    if (!allowListSizeChange && scrollAnchor.hasListSize &&
        (scrollAnchor.listWidth !== listWidth || scrollAnchor.listHeight !== listHeight)) {
      return;
    }
    scrollAnchor.listWidth = listWidth;
    scrollAnchor.listHeight = listHeight;
    scrollAnchor.hasListSize = true;
    scrollAnchor.distanceFromBottom = messageListEl.scrollHeight - messageListEl.scrollTop - messageListEl.clientHeight;
    scrollAnchor.pinnedToBottom = scrollAnchor.distanceFromBottom < scrollBottomThreshold;
    if (scrollAnchor.pinnedToBottom) {
      scrollAnchor.anchorEl = null;
      scrollAnchor.visibleOffset = 0;
      return;
    }
    var listRect = messageListEl.getBoundingClientRect();
    var items = messageScrollItems();
    scrollAnchor.anchorEl = null;
    scrollAnchor.visibleOffset = 0;
    for (var i = 0; i < items.length; i++) {
      var itemRect = items[i].getBoundingClientRect();
      if (itemRect.bottom >= listRect.top) {
        scrollAnchor.anchorEl = items[i];
        scrollAnchor.visibleOffset = itemRect.top - listRect.top;
        return;
      }
    }
  }

  function restoreScrollAnchorAfterResize() {
    if (!messageListEl || !activeRoomID) return;
    suppressMessageListScroll(function () {
      if (scrollAnchor.pinnedToBottom) {
        messageListEl.scrollTop = messageListEl.scrollHeight;
        return;
      }
      if (scrollAnchor.anchorEl && scrollAnchor.anchorEl.isConnected) {
        var listRect = messageListEl.getBoundingClientRect();
        var anchorRect = scrollAnchor.anchorEl.getBoundingClientRect();
        messageListEl.scrollTop += anchorRect.top - listRect.top - scrollAnchor.visibleOffset;
        return;
      }
      messageListEl.scrollTop = messageListEl.scrollHeight - messageListEl.clientHeight - scrollAnchor.distanceFromBottom;
    }, true);
  }

  function scheduleResizeAnchorRestore() {
    restoreScrollAnchorAfterResize();
  }

  function preserveMessageListScrollForMutation(fn, preserveScroll) {
    if (preserveScroll === false) {
      fn();
      return;
    }
    var wasPinned = isMessageListNearBottom();
    if (!wasPinned) updateScrollAnchor(true);
    fn();
    if (wasPinned) {
      scrollToBottom(true);
    } else {
      restoreScrollAnchorAfterResize();
    }
  }

  // Expiry cutoff + drift. Source of truth: expiredAfterWindow() in
  // internal/server/settings.go; 0 means never expire. The cutoff is `now -
  // window` and drifts while the tab is open. renderMessages recomputes it each
  // paint; the 30s tick re-renders only when the expired set changes, so an
  // idle room's divider actually moves as messages age out (not just on first
  // paint). Humans get unfiltered history + no AI `expired` marker, so the
  // client re-derives the line — a deliberate drift risk.
  // Clock offset: serverNow - clientNow, refreshed at fetch time from the
  // advisory server_time field. Starting at 0 degrades to today's behaviour
  // (local clock) before the first fetch. The window stays live from
  // serverSettings on every call, so a settings change re-scopes instantly.
  var clockOffsetMs = 0;
  function expiryCutoffMs() {
    var w = Number(serverSettings.messages_considered_expired_after_seconds) || 0;
    return w > 0 ? (Date.now() + clockOffsetMs) - w * 1000 : 0;
  }
  // Expiry render signature = (divider anchor, expired count). The anchor (ID
  // of the first live message after an expired one, or null) covers a live
  // message appended below expired ones (appendMessage can't insert the divider
  // itself). The count covers icon-only drift the anchor alone misses: all-live
  // →all-expired, or a middle message crossing with the anchor unchanged. The
  // 30s tick re-renders only when EITHER field changes.
  function expiryRenderSignature() {
    var cut = expiryCutoffMs();
    if (cut <= 0) return { anchor: null, count: 0 };
    var arr = messages[activeRoomID] || [];
    var count = 0, anchor = null, sawExpired = false;
    for (var i = 0; i < arr.length; i++) {
      var ms = new Date(arr[i].created_at).getTime();
      if (Number.isFinite(ms) && ms < cut) { count++; sawExpired = true; continue; }
      if (sawExpired && anchor === null) anchor = arr[i].id;
    }
    return { anchor: anchor, count: count };
  }
  var lastRenderAnchor = null;
  var lastRenderCount = 0;

  // Shared builder for both history dividers — the retention expiry line
  // (expired→live transition) and the start-of-room line (oldest message
  // reached). The two differ only in label/aria/title; CSS is one shared base.
  function historyDividerHTML(label, ariaLabel, title) {
    return '<div class="history-divider" role="separator" aria-label="' + esc(ariaLabel) + '">' +
      '<span class="history-divider-line"></span>' +
      '<span class="history-divider-label"' + (title ? ' title="' + esc(title) + '"' : '') + '>' + esc(label) + '</span>' +
      '<span class="history-divider-line"></span>' +
    '</div>';
  }

  function renderMessages() {
    if (!activeRoomID) return;
    var msgs = messages[activeRoomID] || [];

    if (msgs.length === 0) {
      messageListEl.innerHTML = '<div class="empty-state">No messages in this room yet.</div>';
      updateScrollAnchor(true);
      refreshOrRenderOpenQuestionsModal();
      return;
    }

    var atBottom = isMessageListNearBottom();
    updateScrollAnchor(true);
    var anchorId = (scrollAnchor.anchorEl && scrollAnchor.anchorEl.dataset) ? scrollAnchor.anchorEl.dataset.id : null;
    var visibleOffset = scrollAnchor.visibleOffset;
    var prevScrollTop = messageListEl.scrollTop;
    var prevScrollHeight = messageListEl.scrollHeight;

    var cutoffMs = expiryCutoffMs();
    var html = '';
    // Start-of-room divider: shown only once a scroll-up has exhausted older
    // history (noMoreOlder latched true) and the room holds ≥1 message. Empty
    // rooms returned above. It clears automatically when pruneMessageCache or a
    // room re-fetch drops the oldest messages (both set noMoreOlder false), so
    // no separate lifecycle — render reads the flag, nothing else.
    if (noMoreOlder[activeRoomID] && msgs.length > 0) {
      html += historyDividerHTML('start of room', 'Beginning of room history');
    }
    var dividerInserted = false; // latches: one divider at the first expired→live
    // transition even if clock skew interleaves expired/live created_at — that
    // is intentional (a single authoritative line), not a bug to remove.
    var expiredCount = 0;
    var dividerAnchor = null;
    for (var mi = 0; mi < msgs.length; mi++) {
      var m = msgs[mi];
      var createdMs = new Date(m.created_at).getTime();
      var expired = cutoffMs > 0 && Number.isFinite(createdMs) && createdMs < cutoffMs;
      if (expired) expiredCount++;
      // Insert the divider before the first LIVE message that follows an expired
      // one (messages are oldest-first, so expired sit at the top). When ALL
      // messages are expired there is no live transition, so no divider — the
      // ⏳ icons carry the meaning; a trailing divider would falsely imply live
      // messages below. Deliberate.
      if (cutoffMs > 0 && !dividerInserted && !expired && mi > 0) {
        var prevMs = new Date(msgs[mi - 1].created_at).getTime();
        if (Number.isFinite(prevMs) && prevMs < cutoffMs) {
          html += historyDividerHTML('expired from AI bulk reads', 'Older messages expired from AI bulk reads', 'Messages above this line are older than the expiry window and hidden from AI bulk reads — still visible to you. Cutoff: ' + new Date(cutoffMs).toISOString());
          dividerInserted = true;
          dividerAnchor = m.id; // the live message the divider sits before
        }
      }
      html += chatMessageHTML(m, expired);
    }
    messageListEl.innerHTML = html;
    lastRenderAnchor = dividerAnchor;
    lastRenderCount = expiredCount;

    messageListEl.querySelectorAll('.chat-msg-body').forEach(function (b) { highlightNames(b); });

    // Re-pin when async content (images, Mermaid, fonts) later grows scrollHeight.
    // Only fires if scrollAnchor.pinnedToBottom, so a user who scrolled up is never yanked.
    function repinIfPinned() {
      if (scrollAnchor.pinnedToBottom) scrollToBottom(true);
    }
    messageListEl.querySelectorAll('img').forEach(function (img) {
      if (!img.complete) {
        img.addEventListener('load', repinIfPinned);
        img.addEventListener('error', repinIfPinned);
      }
      // decode() resolves even for cached images where 'load' never fires
      if (img.decode) img.decode().then(repinIfPinned).catch(function () {});
    });
    renderMermaidBlocks(messageListEl, repinIfPinned);
    if (document.fonts && document.fonts.ready) {
      document.fonts.ready.then(repinIfPinned);
    }

    renderReadReceipts(false);
    if (atBottom) {
      scrollToBottom(true);
      setTimeout(repinIfPinned, 250);
    } else {
      suppressMessageListScroll(function () {
        if (anchorId) {
          var anchorEl = messageListEl.querySelector('[data-id="' + anchorId + '"]');
          if (anchorEl) {
            var listRect = messageListEl.getBoundingClientRect();
            var anchorRect = anchorEl.getBoundingClientRect();
            messageListEl.scrollTop += anchorRect.top - listRect.top - visibleOffset;
            return;
          }
        }
        messageListEl.scrollTop = prevScrollTop + (messageListEl.scrollHeight - prevScrollHeight);
      }, true);
    }
    renderMessageDebugModal();
    refreshOrRenderOpenQuestionsModal();
  }

  function chatMessageHTML(m, expired) {
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
    // Sender identity (icon/alt/tooltip): from_harness on the message wins
    // absolutely — the live record can never override recorded identity, so a
    // re-register on a different model/harness cannot rewrite old messages.
    // When from_harness is absent (legacy messages sent before this feature),
    // fall back to the live agent record: a best guess where we hold no truth,
    // not a falsification of one. The live record also feeds the liveness dot
    // (a "now" fact). Humans are identified by from_kind (or, for legacy
    // messages, the live record's stable kind) and get no dot.
    var senderKind = m.from_kind;
    var isHumanSender = senderKind === 'human' || (!senderKind && !!fromAgent && fromAgent.kind === 'human');
    var isAISender = senderKind === 'ai' || (!isHumanSender && !!fromAgent && fromAgent.kind === 'ai');
    var senderDeparted = !fromAgent;
    // AI display identity: message wins; else live fallback; else unknown.
    var aiModel = 'unknown', aiHarness = 'unknown';
    if (isAISender) {
      if (m.from_harness) {
        aiModel = m.from_model || 'unknown';
        aiHarness = m.from_harness;
      } else if (fromAgent) {
        aiModel = fromAgent.model || 'unknown';
        aiHarness = fromAgent.harness || 'unknown';
      }
    }
    var msgIconSrc = isHumanSender
      ? '/icons/human.svg'
      : isAISender
        ? harnessIconSrc(aiHarness)
        : '/icons/unknown.svg';
    var identityTitle = isHumanSender
      ? 'human'
      : isAISender
        ? (aiModel + ' · ' + aiHarness)
        : 'unknown';
    var msgIconTitle = identityTitle + (isAISender && senderDeparted ? ' (departed)' : '');
    var msgIconAlt = isHumanSender ? 'human' : (isAISender ? aiHarness : 'unknown');
    // Liveness dot: AI senders only. Departed → element kept, turned red via
    // dot-departed so a re-register can restore it without a re-render. Never
    // add/remove dot elements on the update path.
    var dotHTML = '';
    if (isAISender) {
      var dotMeta = fromAgent ? mapLiveness(fromAgent.state) : { cls: 'dot-departed', title: 'departed' };
      dotHTML = '<span class="msg-sender-dot ' + esc(dotMeta.cls) + '" data-agent-id="' + esc(m.from) + '" title="' + esc(dotMeta.title) + '"></span>';
    }
    var room = rooms.find(function (r) { return r.id === m.room_id; });
    var msgRoleKey = room && room.roles ? (room.roles[m.from] || '') : '';
    var msgRoleTag = roleBadgeHTML(msgRoleKey);
    var proposedAnswers = proposedAnswersHTML(m, room);
    var openQuestions = openQuestionsHTML(m, room);
    var visualPlan = visualPlanHTML(m);
    var attachments = attachmentsHTML(m);
    var replyReference = replyReferenceHTML(m);
    var reactions = reactionsHTML(m);
    var messageControls = messageControlsHTML(m);
    var senderAttrs = fromAgent
      ? ' class="chat-msg-from-name agent-profile-link" data-profile-agent-id="' + esc(fromAgent.id) + '" data-profile-context="room" tabindex="0" role="button" aria-label="Show profile for ' + esc(fromAgent.id) + '"'
      : ' class="chat-msg-from-name"';
    return (
      '<div class="chat-msg' + (isSelf ? ' self' : '') + (m.needs_human_attention ? ' needs-attention' : '') + (expired ? ' chat-msg-expired' : '') + '" data-id="' + esc(m.id) + '">' +
        '<div class="chat-msg-header">' +
          '<span class="chat-msg-from">' +
            '<span class="msg-sender-presence">' +
              '<img src="' + msgIconSrc + '" class="harness-icon chat-msg-icon msg-sender-icon" alt="' + esc(msgIconAlt) + '" title="' + esc(msgIconTitle) + '" width="14" height="14"' +
                (isAISender ? ' data-agent-id="' + esc(m.from) + '" data-from-harness="' + esc(m.from_harness || '') + '" data-from-model="' + esc(m.from_model || '') + '"' : '') +
              '>' +
              dotHTML +
            '</span>' +
            msgRoleTag +
            '<span' + senderAttrs + '>' + esc(m.from) + '</span>' +
          '</span>' +
          (m.needs_human_attention ? '<span class="chat-msg-attention-icon" title="Needs human attention">🔔</span>' : '') +
          '<span class="chat-msg-id" data-msg-id="' + esc(String(m.id)) + '" title="Click to copy">#' + esc(String(m.id)) + '</span>' +
          (showDebugButton ? '<button class="icon-button chat-msg-debug-toggle" type="button" data-msg-id="' + esc(String(m.id)) + '" aria-label="Open debug inspector" title="Open debug inspector"><span class="icon-mask icon-mask-bug" aria-hidden="true"></span></button>' : '') +
          '<span class="chat-msg-time" title="' + esc(m.created_at) + '">' + relativeTime(m.created_at) + '</span>' +
          (expired ? '<span class="chat-msg-expired-icon" title="Older than the expiry window — hidden from AI bulk reads, still visible to you" aria-label="Expired from AI bulk reads">⏳</span>' : '') +
        '</div>' +
        '<div class="chat-msg-bubble">' +
          replyReference +
          '<div class="chat-msg-body' + (markdownMode === 'rendered' ? ' md-rendered' : '') + '">' +
            (markdownMode === 'rendered' ? renderMarkdown(m.body) : renderPlainWithCodeMarkers(m.body)) +
          '</div>' +
          visualPlan +
          attachments +
          reactions +
          messageControls +
        '</div>' +
        proposedAnswers +
        openQuestions +
      '</div>'
    );
  }

  function proposedAnswersHTML(m, room) {
    var answers = Array.isArray(m.proposed_answers) ? m.proposed_answers : [];
    if (!answers.length || !messageTargetsViewer(m, room)) return '';
    var disabled = !!answeredProposedAnswers[String(m.id)];
    var html = '<div class="proposed-answers' + (disabled ? ' answered' : '') + '" data-msg-id="' + esc(String(m.id)) + '">';
    answers.forEach(function (answer, idx) {
      html += '<button class="proposed-answer-btn" type="button" data-msg-id="' + esc(String(m.id)) + '" data-answer-index="' + esc(String(idx)) + '"' + (disabled ? ' disabled' : '') + '>' + esc(answer) + '</button>';
    });
    html += '</div>';
    return html;
  }

  function openQuestionDraftHasAnswer(draft) {
    if (!draft || !draft.selector) return false;
    if (draft.selector === 'other') return !!String(draft.value || '').trim();
    return true;
  }

  function openQuestionsAnsweredCount(messageID) {
    var drafts = openQuestionDrafts[String(messageID)] || {};
    var count = 0;
    Object.keys(drafts).forEach(function (key) {
      if (openQuestionDraftHasAnswer(drafts[key])) count++;
    });
    return count;
  }

  function openQuestionOptionLetter(idx) {
    return String.fromCharCode(97 + idx);
  }

  function openQuestionsHTML(m, room) {
    if (!messageTargetsViewer(m, room)) return '';
    var questions = Array.isArray(m.open_questions) ? m.open_questions : [];
    if (!questions || !questions.length) return '';

    var msgID = String(m.id);
    var answered = !!answeredOpenQuestions[msgID];
    var answeredCount = openQuestionsAnsweredCount(msgID);
    var html = '<div class="open-questions-trigger' + (answered ? ' answered' : '') + '" data-msg-id="' + esc(msgID) + '">';
    html += '<button class="open-questions-open" type="button" data-msg-id="' + esc(msgID) + '"' + (answered ? ' disabled' : '') + '>';
    html += answered ? 'Answered' : 'Open Questions';
    html += '<span class="open-questions-count">' + esc(String(answeredCount)) + '/' + esc(String(questions.length)) + '</span>';
    html += '</button>';
    html += '</div>';
    return html;
  }

  /*
   * Open Questions Reply addressing fixtures:
   * - Room member [tina@aimebu] -> prefix is "@tina"
   * - Room members [tina@aimebu, tina@other] -> prefix falls back to "@tina@aimebu"
   */
  function senderHandleFor(msg) {
    var fullID = String(msg.from || '');
    if (!fullID) return '';
    var slug = fullID.split('@')[0];
    var room = (rooms || []).find(function (r) { return r.id === msg.room_id; });
    var members = room ? (room.members || []) : [];
    var collisions = 0;
    for (var i = 0; i < members.length; i++) {
      if (String(members[i]).split('@')[0] === slug) collisions++;
    }
    // Bare slug only when exactly one room member matches. Collision (>1) or
    // sender-left-room (0) both fall back to the full ID for safety.
    return collisions === 1 ? slug : fullID;
  }

  function composeOpenQuestionsReply(msgID) {
    var msg = (messages[activeRoomID] || []).find(function (m) { return String(m.id) === String(msgID); });
    var questions = msg && Array.isArray(msg.open_questions) ? msg.open_questions : null;
    if (!msg || !questions) return '';
    var drafts = openQuestionDrafts[String(msgID)] || {};
    var lines = [];
    questions.forEach(function (q, qIdx) {
      var draft = drafts[String(qIdx)];
      if (!openQuestionDraftHasAnswer(draft)) return;
      if (draft.selector === 'other') {
        lines.push('Q' + (qIdx + 1) + ') other) ' + String(draft.value || '').trim());
        return;
      }
      var optIdx = (q.options || []).findIndex(function (_, idx) { return openQuestionOptionLetter(idx) === draft.selector; });
      if (optIdx >= 0) lines.push('Q' + (qIdx + 1) + ') ' + draft.selector + ') ' + q.options[optIdx]);
    });
    if (msg.from && msg.from_kind !== 'system' && lines.length) {
      lines.unshift('@' + senderHandleFor(msg), '');
    }
    return lines.join('\n');
  }

  function currentOpenQuestionMessage() {
    if (!openQuestionModalState.messageID) return null;
    return findMessageInRoom(activeRoomID, parseInt(openQuestionModalState.messageID, 10));
  }

  function currentOpenQuestionList() {
    var msg = currentOpenQuestionMessage();
    return msg && Array.isArray(msg.open_questions) ? msg.open_questions : [];
  }

  function openQuestionAllAnswered(msgID, questions) {
    return openQuestionsAnsweredCount(msgID) === questions.length;
  }

  function setOpenQuestionDraft(msgID, qIdx, selector, value) {
    var key = String(msgID);
    if (!openQuestionDrafts[key]) openQuestionDrafts[key] = {};
    openQuestionDrafts[key][String(qIdx)] = {
      selector: selector,
      value: value || '',
    };
  }

  function refreshOpenQuestionsTrigger(msgID, questions) {
    document.querySelectorAll('.open-questions-trigger').forEach(function (trigger) {
      if (trigger.getAttribute('data-msg-id') !== String(msgID)) return;
      var count = trigger.querySelector('.open-questions-count');
      if (count) count.textContent = openQuestionsAnsweredCount(msgID) + '/' + questions.length;
    });
  }

  function refreshOpenQuestionsModalIndicators() {
    var msgID = openQuestionModalState.messageID;
    var msg = currentOpenQuestionMessage();
    var questions = currentOpenQuestionList();
    if (!msgID || !questions.length) return;
    var answeredCount = openQuestionsAnsweredCount(msgID);
    var allAnswered = answeredCount === questions.length;
    refreshOpenQuestionsTrigger(msgID, questions);
    if (openQuestionsModalFooter) {
      var progress = openQuestionsModalFooter.querySelector('.open-questions-progress');
      if (progress) progress.textContent = answeredCount + '/' + questions.length + ' answered';
    }
    if (openQuestionsModalBody) {
      openQuestionsModalBody.querySelectorAll('.open-questions-step[data-q-index]').forEach(function (step) {
        var idx = step.getAttribute('data-q-index');
        var stepDraft = (openQuestionDrafts[String(msgID)] || {})[String(idx)];
        step.classList.toggle('answered', openQuestionDraftHasAnswer(stepDraft));
      });
      var sendStep = openQuestionsModalBody.querySelector('.open-questions-send-step');
      if (sendStep) {
        sendStep.disabled = !allAnswered;
        sendStep.classList.toggle('answered', allAnswered);
      }
      var sendBtn = openQuestionsModalBody.querySelector('.open-questions-send');
      if (sendBtn) sendBtn.disabled = !allAnswered || !!answeredOpenQuestions[String(msgID)];
    }
    if (openQuestionsModalFooter) {
      var nextBtn = openQuestionsModalFooter.querySelector('.open-questions-next');
      if (nextBtn) {
        var qIdx = openQuestionModalState.currentIndex;
        nextBtn.disabled = qIdx >= questions.length || (qIdx === questions.length - 1 && !allAnswered);
      }
    }
  }

  function refreshOrRenderOpenQuestionsModal() {
    if (openQuestionModalState.open) {
      refreshOpenQuestionsModalIndicators();
      return;
    }
    renderOpenQuestionsModal();
  }

  function renderOpenQuestionsModal(focusOther) {
    if (!openQuestionsModal) return;

    if (!openQuestionModalState.open) {
      openQuestionsModal.classList.add('hidden');
      updateBodyModalLock();
      return;
    }

    var msg = currentOpenQuestionMessage();
    var questions = currentOpenQuestionList();
    if (!msg || !questions.length) {
      closeOpenQuestionsModal();
      return;
    }

    var msgID = String(msg.id);
    var allAnswered = openQuestionAllAnswered(msgID, questions);
    var qIdx = Math.max(0, Math.min(openQuestionModalState.currentIndex, questions.length));
    openQuestionModalState.currentIndex = qIdx;
    var onSendSheet = qIdx === questions.length;
    var q = onSendSheet ? null : (questions[qIdx] || {});
    var draft = onSendSheet ? {} : ((openQuestionDrafts[msgID] || {})[String(qIdx)] || {});
    var otherSelected = draft.selector === 'other';
    var currentAnswered = onSendSheet || openQuestionDraftHasAnswer(draft);
    var name = 'open-question-modal-' + msgID + '-' + qIdx;

    openQuestionsModal.classList.remove('hidden');
    updateBodyModalLock();
    if (openQuestionsModalTitle) openQuestionsModalTitle.textContent = 'Open Questions';
    if (openQuestionsModalSubtitle) {
      openQuestionsModalSubtitle.textContent = onSendSheet ? 'Ready to send from #' + msgID : 'Q' + (qIdx + 1) + ' of ' + questions.length + ' from #' + msgID;
    }

    var body = '';
    body += '<div class="open-questions-steps" role="tablist" aria-label="Questions">';
    questions.forEach(function (_, idx) {
      var stepDraft = (openQuestionDrafts[msgID] || {})[String(idx)];
      var stepAnswered = openQuestionDraftHasAnswer(stepDraft);
      body += '<button class="open-questions-step' + (idx === qIdx ? ' active' : '') + (stepAnswered ? ' answered' : '') + '" type="button" data-q-index="' + esc(String(idx)) + '" aria-label="Question ' + esc(String(idx + 1)) + '">' + esc(String(idx + 1)) + '</button>';
    });
    body += '<button class="open-questions-step open-questions-send-step' + (onSendSheet ? ' active' : '') + (allAnswered ? ' answered' : '') + '" type="button" data-send-step="true" aria-label="Send answers"' + (!allAnswered ? ' disabled' : '') + '><svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><line x1="22" y1="2" x2="11" y2="13"></line><polygon points="22 2 15 22 11 13 2 9 22 2"></polygon></svg></button>';
    body += '</div>';
    if (onSendSheet) {
      body += '<div class="open-question-send-sheet">';
      body += '<div class="open-question-send-title">' + esc(String(openQuestionsAnsweredCount(msgID))) + '/' + esc(String(questions.length)) + ' answered</div>';
      body += '<button class="btn btn-primary open-questions-send" type="button"' + (!allAnswered || answeredOpenQuestions[msgID] ? ' disabled' : '') + '>Send answers</button>';
      body += '</div>';
    } else {
      /*
       * Open Questions fixtures:
       * - description renders below the Q title and above option radios.
       * - submitted answers to tina@aimebu start with "@tina@aimebu\n\nQ1)".
       */
      body += '<fieldset class="open-question-sheet" data-msg-id="' + esc(msgID) + '" data-q-index="' + esc(String(qIdx)) + '">';
      body += '<legend>Q' + esc(String(qIdx + 1)) + ') ' + esc(q.question || '') + '</legend>';
      if (q.description) {
        body += '<div class="open-questions-description md-rendered">' + renderMarkdown(String(q.description)) + '</div>';
      }
      body += '<div class="open-question-options">';
      (q.options || []).forEach(function (opt, optIdx) {
        var letter = openQuestionOptionLetter(optIdx);
        var inputID = name + '-' + letter;
        var checked = draft.selector === letter;
        body += '<label class="open-question-option" for="' + esc(inputID) + '">';
        body += '<input id="' + esc(inputID) + '" type="radio" name="' + esc(name) + '" value="' + esc(letter) + '" data-selector="' + esc(letter) + '"' + (checked ? ' checked' : '') + '>';
        body += '<span><strong>' + esc(letter) + ')</strong> ' + esc(opt) + '</span>';
        body += '</label>';
      });
      var otherID = name + '-other';
      body += '<label class="open-question-option open-question-other" for="' + esc(otherID) + '">';
      body += '<input id="' + esc(otherID) + '" type="radio" name="' + esc(name) + '" value="other" data-selector="other"' + (otherSelected ? ' checked' : '') + '>';
      body += '<span>Other</span>';
      body += '<input class="open-question-other-input" type="text" data-q-index="' + esc(String(qIdx)) + '" value="' + esc(draft.value || '') + '" placeholder="Type answer"' + (!otherSelected ? ' disabled' : '') + '>';
      body += '</label>';
      body += '</div>';
      body += '</fieldset>';
    }
    if (openQuestionsModalBody) openQuestionsModalBody.innerHTML = body;

    if (openQuestionsModalFooter) {
      openQuestionsModalFooter.innerHTML =
        '<button class="btn btn-sm open-questions-back" type="button"' + (qIdx === 0 ? ' disabled' : '') + '>Back</button>' +
        '<div class="open-questions-progress">' + esc(String(openQuestionsAnsweredCount(msgID))) + '/' + esc(String(questions.length)) + ' answered</div>' +
        '<button class="btn btn-sm open-questions-next" type="button"' + (qIdx >= questions.length || (qIdx === questions.length - 1 && !allAnswered) ? ' disabled' : '') + '>Next</button>';
    }

    if (focusOther && otherSelected && openQuestionsModalBody) {
      var otherInput = openQuestionsModalBody.querySelector('.open-question-other-input');
      if (otherInput) {
        otherInput.focus();
        otherInput.setSelectionRange(otherInput.value.length, otherInput.value.length);
      }
    } else if (!currentAnswered && openQuestionsModalBody) {
      var firstRadio = openQuestionsModalBody.querySelector('input[type="radio"]');
      if (firstRadio) firstRadio.focus();
    }
  }

  function openOpenQuestionsModal(msgID) {
    if (answeredOpenQuestions[String(msgID)]) return;
    var msg = findMessageInRoom(activeRoomID, parseInt(msgID, 10));
    if (!msg || !Array.isArray(msg.open_questions) || !msg.open_questions.length) return;
    openQuestionModalState.open = true;
    openQuestionModalState.messageID = String(msgID);
    var questions = msg.open_questions;
    var current = openQuestionAllAnswered(String(msgID), questions)
      ? questions.length
      : Math.max(0, Math.min(openQuestionModalState.currentIndex || 0, questions.length - 1));
    for (var i = 0; i < questions.length; i++) {
      if (!openQuestionDraftHasAnswer((openQuestionDrafts[String(msgID)] || {})[String(i)])) {
        current = i;
        break;
      }
    }
    openQuestionModalState.currentIndex = current;
    renderOpenQuestionsModal();
  }

  function closeOpenQuestionsModal() {
    openQuestionModalState.open = false;
    openQuestionModalState.messageID = null;
    openQuestionModalState.currentIndex = 0;
    renderOpenQuestionsModal();
  }

  function setOpenQuestionModalIndex(idx) {
    var questions = currentOpenQuestionList();
    if (!questions.length) return;
    openQuestionModalState.currentIndex = Math.max(0, Math.min(idx, questions.length));
    renderOpenQuestionsModal(true);
  }

  function submitOpenQuestionsModal(e) {
    var msgID = openQuestionModalState.messageID;
    var questions = currentOpenQuestionList();
    if (!msgID || !openQuestionAllAnswered(msgID, questions)) return;
    var reply = composeOpenQuestionsReply(msgID);
    if (!reply) return;
    if (e && e.shiftKey) {
      msgBodyInput.value = reply;
      setPendingReply(parseInt(msgID, 10));
      answeredOpenQuestions[String(msgID)] = true;
      closeOpenQuestionsModal();
      renderMessages();
      msgBodyInput.focus();
      msgBodyInput.setSelectionRange(msgBodyInput.value.length, msgBodyInput.value.length);
      resizeMsgInput();
      updateAcPopup();
      return;
    }
    sendMessage(reply, undefined, parseInt(msgID, 10)).then(function (res) {
      if (!res) return;
      answeredOpenQuestions[String(msgID)] = true;
      closeOpenQuestionsModal();
      renderMessages();
    });
  }

  function appendMessage(msg) {
    if (!activeRoomID || msg.room_id !== activeRoomID) return;

    var atBottom = isMessageListNearBottom();

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
    var nearBottom = isMessageListNearBottom();
    if (!force && !nearBottom) return;
    // Double-rAF: outer frame lets the browser complete its layout pass (Safari
    // defers layout for cached images until after the first rAF fires), inner
    // frame performs the actual scroll write with fresh scrollHeight.
    requestAnimationFrame(function () {
      requestAnimationFrame(function () {
        suppressMessageListScroll(function () {
          messageListEl.scrollTop = messageListEl.scrollHeight;
        }, true);
      });
    });
  }

  // ── Render agents ────────────────────────────────────────────────

  function renderAllAgents() {
    if (agents.length === 0) {
      allAgentsList.innerHTML = '<div class="empty-state">No agents registered.</div>';
      return;
    }

    var sorted = agents.slice().sort(function (a, b) {
      var sa = agentStatus(a);
      var sb = agentStatus(b);
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
      renderRightSidebar();
      return;
    }

    var room = rooms.find(function (r) { return r.id === activeRoomID; });
    if (!room || !room.members || room.members.length === 0) {
      roomAgentsList.innerHTML = '<div class="empty-state">No members in this room.</div>';
      return;
    }

    roomAgentsList.innerHTML = room.members.map(function (memberID) {
      var agent = agentByID(memberID);
      if (agent) {
        return agentCardHTML(agent, 'room');
      }
      var roleKey = (room.roles && room.roles[memberID]) || '';
      return (
        '<div class="agent-card agent-card-compact">' +
          '<div class="agent-id">' +
            '<span class="agent-online-dot offline"></span>' +
            roleBadgeHTML(roleKey) +
            '<span class="agent-id-text">' + esc(memberID) + '</span>' +
          '</div>' +
        '</div>'
      );
    }).join('');
    renderRightSidebar();
  }

  function roomSettingsRoleSelectFocused() {
    var active = document.activeElement;
    return !!(
      active &&
      active.matches &&
      active.matches('.room-role-select') &&
      roomSettingsMembers &&
      roomSettingsMembers.contains(active)
    );
  }

  function roomMemoryOverrideValue(room) {
    if (!room || room.memory_enabled === undefined || room.memory_enabled === null) return 'inherit';
    return room.memory_enabled ? 'enabled' : 'disabled';
  }

  function roomMemoryEffectiveText(room) {
    if (!memoryGloballyEnabled()) return 'Effective: disabled by global setting';
    if (!room || room.memory_enabled === undefined || room.memory_enabled === null) return 'Effective: enabled by global setting';
    return room.memory_enabled ? 'Effective: enabled by room override' : 'Effective: disabled for this room';
  }

  function renderRoomMemorySettings(room) {
    if (roomMemorySelect) {
      roomMemorySelect.disabled = !room || room.id === '_system';
      roomMemorySelect.value = roomMemoryOverrideValue(room);
    }
    if (roomMemoryStatusEl) {
      roomMemoryStatusEl.textContent = room ? roomMemoryEffectiveText(room) : 'Select a room first.';
    }
  }

  function renderRoomSettings(opts) {
    opts = opts || {};
    if (!roomSettingsMembers) return;
    if (!opts.force && roomSettingsModal && roomSettingsModal.classList.contains('hidden')) return;
    if (!activeRoomID) {
      if (roomSettingsTitle) roomSettingsTitle.textContent = 'Room Settings';
      if (roomSettingsRemoveBtn) roomSettingsRemoveBtn.disabled = true;
      renderRoomMemorySettings(null);
      roomSettingsMembers.innerHTML = '<div class="empty-state">Select a room first.</div>';
      roomSettingsDirty = false;
      return;
    }
    var room = rooms.find(function (r) { return r.id === activeRoomID; });
    if (!room) {
      if (roomSettingsRemoveBtn) roomSettingsRemoveBtn.disabled = true;
      renderRoomMemorySettings(null);
      roomSettingsMembers.innerHTML = '<div class="empty-state">Room not found.</div>';
      roomSettingsDirty = false;
      return;
    }
    if (roomSettingsTitle) roomSettingsTitle.textContent = 'Room Settings: ' + room.id;
    if (roomSettingsRemoveBtn) roomSettingsRemoveBtn.disabled = room.id === '_system';
    // Kick all agents: disable for _system and when the room has no AI members.
    var hasAIAgent = (room.members || []).some(function (mid) {
      var a = agents.find(function (x) { return x.id === mid; });
      return a && a.kind === 'ai';
    });
    if (roomSettingsKickAgentsBtn) roomSettingsKickAgentsBtn.disabled = room.id === '_system' || !hasAIAgent;
    // Hide: disabled when not solo (roomHasOtherMember rule) or for _system.
    var others = (room.members || []).filter(function (m) { return m !== agentID; });
    var isSolo = others.length === 0;
    if (roomSettingsHideBtn) {
      roomSettingsHideBtn.disabled = room.id === '_system' || !isSolo;
      roomSettingsHideBtn.title = (room.id === '_system' || !isSolo)
        ? 'Only available when you are the last member of this room'
        : 'Hide this room from your sidebar';
    }
    // Pin: always available (except _system).
    if (roomSettingsPinBtn) {
      roomSettingsPinBtn.disabled = room.id === '_system';
      var isPinned = !!(roomPrefs[room.id] && roomPrefs[room.id].pinned);
      roomSettingsPinBtn.textContent = isPinned ? 'Unpin room' : 'Pin room';
    }
    // Hide button label reflects current state.
    if (roomSettingsHideBtn) {
      var isHidden = !!(roomPrefs[room.id] && roomPrefs[room.id].hidden);
      roomSettingsHideBtn.textContent = isHidden ? 'Unhide room' : 'Hide room';
    }
    renderRoomMemorySettings(room);
    if (!opts.force && roomSettingsRoleSelectFocused()) {
      roomSettingsDirty = true;
      return;
    }
    var members = (room.members || []).filter(function (memberID) {
      var agent = agents.find(function (a) { return a.id === memberID; });
      return agent && agent.kind === 'ai';
    });
    if (!members.length) {
      roomSettingsMembers.innerHTML = '<div class="empty-state">No AI agents in this room.</div>';
      roomSettingsDirty = false;
      return;
    }
    roomSettingsMembers.innerHTML = members.map(function (memberID) {
      var agent = agents.find(function (a) { return a.id === memberID; }) || {};
      var roleKey = (room.roles && room.roles[memberID]) || '';
      var status = agentStatus(agent);
      var runtime = (agent.model || 'unknown') + ' · ' + (agent.harness || 'unknown');
      var seen = 'seen ' + relativeTime(agent.last_seen);
      var options = '<option value="">No role</option>';
      roleEntries.forEach(function (re) {
        var label = ((re.emoji || re.icon) ? (re.emoji || re.icon) + ' ' : '') + re.key;
        var holder = (re.cardinality === 'singleton') ? roleHolderInRoom(room, re.key) : '';
        var disabled = holder && holder !== memberID && re.key !== roleKey;
        if (disabled) label += ' (held by ' + agentDisplayName(holder) + ')';
        options += '<option value="' + esc(re.key) + '"' + (re.key === roleKey ? ' selected' : '') + (disabled ? ' disabled' : '') + '>' + esc(label) + '</option>';
      });
      return (
        '<div class="room-settings-member">' +
          '<div class="room-settings-member-info">' +
            '<div class="room-settings-member-id">' +
              roleBadgeHTML(roleKey) +
              '<span class="room-settings-member-name">' + esc(memberID) + '</span>' +
            '</div>' +
            '<div class="room-settings-member-meta">' +
              '<span class="agent-online-dot ' + esc(status) + '" title="' + esc(lastSeenTitle(agent)) + '"></span>' +
              '<span>' + esc(runtime) + '</span>' +
              '<span class="room-settings-meta-separator">·</span>' +
              '<span>' + esc(seen) + '</span>' +
            '</div>' +
          '</div>' +
          '<select class="room-role-select" data-agent-id="' + esc(memberID) + '">' + options + '</select>' +
        '</div>'
      );
    }).join('');

    roomSettingsMembers.querySelectorAll('.room-role-select').forEach(function (sel) {
      sel.addEventListener('blur', function () {
        if (!roomSettingsDirty) return;
        roomSettingsDirty = false;
        renderRoomSettings({ force: true });
      });
      sel.addEventListener('change', function () {
        roomSettingsDirty = false;
        var agentIDVal = sel.getAttribute('data-agent-id');
        var roleKey = sel.value;
        api('POST', '/rooms/' + encodeURIComponent(activeRoomID) + '/roles', {
          agent_id: agentIDVal,
          role_key: roleKey
        }).then(function (updatedRoom) {
          rooms = rooms.map(function (r) { return r.id === updatedRoom.id ? updatedRoom : r; });
          renderRoomAgents();
          renderRoomSettings({ force: true });
        }).catch(function (err) {
          console.error('assign role', err);
          alert('Failed to assign role: ' + err.message);
        });
      });
    });
  }

  function agentCardHTML(a, context) {
    var status = agentStatus(a);
    var room = activeRoomID ? activeRoom() : null;
    var presenceTag = context === 'room' ? agentPresenceHTML(a.id, room) : '';
    var iconSrc = agentIconSrc(a);
    var iconTitle = a.kind === 'human' ? 'human' : esc((a.model || 'unknown') + ' · ' + (a.harness || 'unknown'));
    var iconAlt = a.kind === 'human' ? 'human' : esc(a.harness || 'unknown');
    var iconTag = '<img src="' + iconSrc + '" class="harness-icon" alt="' + iconAlt + '" title="' + iconTitle + '" width="14" height="14">';
    var roleKey = roomRoleKey(room, a.id);
    var roleTag = roleBadgeHTML(roleKey);
    var stateTag = agentStateBadgeHTML(a);
    var actionBtns = agentCardActionsHTML(a, context, room);
    return (
      '<div class="agent-card agent-card-compact agent-profile-link" data-agent-id="' + esc(a.id) + '" data-profile-agent-id="' + esc(a.id) + '" data-profile-context="' + esc(context) + '" tabindex="0" role="button" aria-label="Show profile for ' + esc(a.id) + '">' +
        '<div class="agent-id">' +
          '<span class="agent-online-dot ' + status + '" title="' + esc(lastSeenTitle(a)) + '"></span>' +
          presenceTag +
          iconTag +
          roleTag +
          stateTag +
          '<span class="agent-id-text">' + esc(a.id) + '</span>' +
        '</div>' +
        actionBtns +
      '</div>'
    );
  }

  function agentActionButtonsHTML(a, context, room) {
    if (!a || a.id === agentID) return '';
    var buttons = [
      '<button class="agent-action-btn agent-dm-btn" data-agent-id="' + esc(a.id) + '" title="Open DM with ' + esc(a.id) + '" aria-label="Open DM with ' + esc(a.id) + '">DM</button>'
    ];
    if (context === 'room' && room && room.members && room.members.indexOf(a.id) !== -1) {
      buttons.push('<button class="agent-action-btn agent-leave-btn" data-agent-id="' + esc(a.id) + '" title="Kick ' + esc(a.id) + ' from room" aria-label="Kick ' + esc(a.id) + ' from room">Kick</button>');
    } else if (context === 'global') {
      buttons.push('<button class="agent-action-btn agent-deregister-btn" data-agent-id="' + esc(a.id) + '" title="Deregister ' + esc(a.id) + '" aria-label="Deregister ' + esc(a.id) + '">Deregister</button>');
    }
    return buttons.join('');
  }

  function agentCardActionsHTML(a, context, room) {
    var actions = agentActionButtonsHTML(a, context, room);
    return actions ? '<div class="agent-actions agent-card-actions">' + actions + '</div>' : '';
  }

  function profileContextRoom(context) {
    if (context === 'global') return null;
    return activeRoom();
  }

  function openProfilePanel(agentIDVal, context) {
    if (!agentByID(agentIDVal)) return;
    if (rightCollapsed) {
      rightCollapsed = false;
      localStorage.setItem('aimebu_right_collapsed', 'false');
      applySidebarCollapseState();
    }
    rightSidebarMode = 'profile';
    profileAgentID = agentIDVal;
    profileContext = context || 'room';
    renderRightSidebar();
    if (window.innerWidth <= 900) setMobileTab('agents');
  }

  function closeProfilePanel() {
    rightSidebarMode = 'members';
    profileAgentID = '';
    profileContext = 'room';
    renderRightSidebar();
  }

  function openUsagesPanel() {
    if (rightCollapsed) {
      rightCollapsed = false;
      localStorage.setItem('aimebu_right_collapsed', 'false');
      applySidebarCollapseState();
    }
    rightSidebarMode = 'usages';
    profileAgentID = '';
    profileContext = 'room';
    renderRightSidebar();
    renderUsagesSidebar(); // show loading/previous tiles immediately; loadUsages refreshes
    loadUsages();
    if (window.innerWidth <= 900) setMobileTab('agents');
  }

  function closeUsagesPanel() {
    rightSidebarMode = 'members';
    renderRightSidebar();
  }

  function rightProfileHTML(a) {
    var room = profileContextRoom(profileContext);
    var roleKey = roomRoleKey(room || activeRoom(), a.id);
    var role = roleEntryByKey(roleKey);
    var status = agentStatus(a);
    var statusLabel = status === 'active' ? 'Online' : (status === 'stale' ? 'Recently active' : 'Offline');
    var presenceText = agentPresenceText(a.id, room || activeRoom());
    var runtime = a.kind === 'human' ? 'human' : ((a.model || 'unknown') + ' · ' + (a.harness || 'unknown'));
    var session = agentSessions[a.id] || null;
    var sessionRows = session ? [
      ['Session', session.harness_session_id || ''],
      ['Origin', session.origin || ''],
      ['Resume', session.resume_command || ''],
      ['CWD', session.cwd || ''],
      ['Seen', session.last_seen ? relativeTime(session.last_seen) : '']
    ].filter(function (row) {
      return row[1];
    }).map(function (row) {
      return '<div class="right-profile-meta-row"><dt>' + esc(row[0]) + '</dt><dd>' + esc(String(row[1])) + '</dd></div>';
    }).join('') : '';
    var meta = a.meta || {};
    var metaKeys = Object.keys(meta).sort();
    var metaRows = metaKeys.map(function (k) {
      return '<div class="right-profile-meta-row"><dt>' + esc(k) + '</dt><dd>' + esc(String(meta[k])) + '</dd></div>';
    }).join('');
    var actions = agentActionButtonsHTML(a, profileContext, room);
    return (
      '<div class="right-profile-card" aria-label="Agent profile: ' + esc(a.id) + '">' +
        '<div class="right-profile-header">' +
          '<img src="' + agentIconSrc(a) + '" class="right-profile-icon" alt="' + esc(a.kind === 'human' ? 'human' : (a.harness || 'unknown')) + '" width="40" height="40">' +
          '<div class="right-profile-title">' +
            '<div class="right-profile-id">' + esc(a.id) + '</div>' +
            '<div class="right-profile-subtitle">' + esc(a.name || a.id.split('@')[0] || a.id) + '</div>' +
          '</div>' +
        '</div>' +
        '<div class="right-profile-facts">' +
          '<div><span>Status</span><strong>' + esc(statusLabel) + '</strong></div>' +
          (presenceText ? '<div><span>Presence</span><strong>' + esc(presenceText) + '</strong></div>' : '') +
          '<div><span>Last seen</span><strong>' + esc(relativeTime(a.last_seen)) + '</strong></div>' +
          '<div><span>Runtime</span><strong>' + esc(runtime) + '</strong></div>' +
          (roleKey ? '<div><span>Role</span><strong>' + roleBadgeHTML(roleKey) + esc(role ? role.key : roleKey) + '</strong></div>' : '') +
        '</div>' +
        (sessionRows ? '<dl class="right-profile-meta">' + sessionRows + '</dl>' : '') +
        (metaRows ? '<dl class="right-profile-meta">' + metaRows + '</dl>' : '') +
        (actions ? '<div class="agent-actions right-profile-actions">' + actions + '</div>' : '') +
      '</div>'
    );
  }

  function renderRightSidebar() {
    if (!sidebarRight || !rightProfilePanel) return;
    var profileOpen = rightSidebarMode === 'profile' && !!profileAgentID;
    var usagesOpen = rightSidebarMode === 'usages';
    var agent = profileOpen ? agentByID(profileAgentID) : null;
    if (profileOpen && !agent) {
      rightSidebarMode = 'members';
      profileAgentID = '';
      profileOpen = false;
    }
    sidebarRight.setAttribute('data-mode', usagesOpen ? 'usages' : (profileOpen ? 'profile' : 'members'));
    if (rightSidebarTitle) rightSidebarTitle.textContent = usagesOpen ? 'Usages' : (profileOpen ? 'Agent Profile' : 'Room Members');
    if (profileOpen) {
      rightProfilePanel.setAttribute('aria-label', 'Agent profile: ' + profileAgentID);
      rightProfilePanel.innerHTML = rightProfileHTML(agent);
    } else {
      rightProfilePanel.innerHTML = '';
    }
    // Note: usages tiles are NOT rebuilt here — that was the 1-3s repaint bug
    // (every agent tick via renderRoomAgents rebuilt the whole usages panel).
    // Tiles are painted by renderUsages() on data change (loadUsages/WS events)
    // and by openUsagesPanel() on open. The data-mode attribute above
    // shows/hides the panel container; the tile HTML persists in the DOM.
    applyRightToggleState();
  }

  function applyRightToggleState() {
    if (!rightSidebarToggle) return;
    if (rightSidebarMode === 'profile' || rightSidebarMode === 'usages') {
      rightSidebarToggle.textContent = '×';
      var label = rightSidebarMode === 'profile' ? 'Close profile' : 'Close usages';
      rightSidebarToggle.setAttribute('aria-label', label);
      rightSidebarToggle.setAttribute('title', label);
      return;
    }
    renderChevronIcon(rightSidebarToggle, rightCollapsed ? 'left' : 'right');
    rightSidebarToggle.setAttribute('aria-label', rightCollapsed ? 'Expand agents sidebar' : 'Collapse agents sidebar');
    rightSidebarToggle.setAttribute('title', rightCollapsed ? 'Expand agents sidebar' : 'Collapse agents sidebar');
  }

  function renderChevronIcon(button, direction) {
    var points = direction === 'left' ? '15 18 9 12 15 6' : '9 18 15 12 9 6';
    button.innerHTML = [
      '<svg class="room-header-action-icon" width="13" height="13" viewBox="0 0 24 24"',
      ' fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"',
      ' stroke-linejoin="round" aria-hidden="true"><polyline points="' + points + '">',
      '</polyline></svg>',
    ].join('');
  }

  function applySidebarCollapseState() {
    if (!appLayout) return;
    appLayout.classList.toggle('left-collapsed', leftCollapsed);
    appLayout.classList.toggle('right-collapsed', rightCollapsed);
    if (leftSidebarToggle) {
      renderChevronIcon(leftSidebarToggle, leftCollapsed ? 'right' : 'left');
      leftSidebarToggle.setAttribute('aria-label', leftCollapsed ? 'Expand rooms sidebar' : 'Collapse rooms sidebar');
      leftSidebarToggle.setAttribute('title', leftCollapsed ? 'Expand rooms sidebar' : 'Collapse rooms sidebar');
    }
    applyRightToggleState();
  }

  function toggleLeftSidebar() {
    leftCollapsed = !leftCollapsed;
    localStorage.setItem('aimebu_left_collapsed', String(leftCollapsed));
    applySidebarCollapseState();
  }

  function toggleRightSidebar() {
    rightCollapsed = !rightCollapsed;
    localStorage.setItem('aimebu_right_collapsed', String(rightCollapsed));
    applySidebarCollapseState();
  }

  // renderReadReceipts appends a small avatar strip to each chat message
  // in the active room, one per agent whose effective cursor >= msg.id
  // (excluding the message author — sender is implicitly read).
  function renderReadReceipts(preserveScroll) {
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

    preserveMessageListScrollForMutation(function () {
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
    }, preserveScroll);
  }

  // ── Mobile tab handling ──────────────────────────────────────────

  function setMobileTab(tab) {
    document.body.classList.remove('tab-rooms', 'tab-chat', 'tab-agents');
    document.body.classList.add('tab-' + tab);
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

  welcomeNameInput.addEventListener('input', function () {
    refreshWelcomeSubmit();
    welcomeAutocomplete.update();
  });

  welcomeNameInput.addEventListener('keydown', function (e) {
    if (welcomeAutocomplete.handleKeydown(e)) return;
    if (e.key === 'Enter' && !e.isComposing) {
      e.preventDefault();
      welcomeForm.requestSubmit();
    }
  });

  welcomeForm.addEventListener('submit', function (e) {
    e.preventDefault();
    var name = welcomeNameInput.value.trim();
    if (!name) return;
    agentID = name;
    agentIDFromStorage = true;
    localStorage.setItem('aimebu_agent_id', name);
    agentIDInput.value = name;
    hideWelcomeGate();
    startSession();
  });

  // Settings modal open/close
  settingsBtn.addEventListener('click', function () { openSettings('general'); });
  settingsCloseBtn.addEventListener('click', closeSettings);
  settingsOverlay.addEventListener('click', closeSettings);
  if (roomSettingsBtn) roomSettingsBtn.addEventListener('click', openRoomSettings);
  if (roomSettingsCloseBtn) roomSettingsCloseBtn.addEventListener('click', closeRoomSettings);
  if (roomSettingsOverlay) roomSettingsOverlay.addEventListener('click', closeRoomSettings);
  if (roomSettingsRemoveBtn) roomSettingsRemoveBtn.addEventListener('click', function () {
    if (!activeRoomID || activeRoomID === '_system') return;
    var removedRoomID = activeRoomID;
    if (!confirm('Remove room "' + removedRoomID + '"? This deletes the room and its messages for everyone.')) return;

    roomSettingsRemoveBtn.disabled = true;
    api('DELETE', '/rooms/' + encodeURIComponent(removedRoomID)).then(function () {
      handleLocalRoomRemoval(removedRoomID);
    }).catch(function (err) {
      if (/404/.test(String(err && err.message))) {
        handleLocalRoomRemoval(removedRoomID);
        return;
      }
      console.error('remove room', err);
      alert('Failed to remove room: ' + (err && err.message ? err.message : err));
    }).finally(function () {
      roomSettingsRemoveBtn.disabled = !activeRoomID || activeRoomID === '_system';
    });
  });
  // Kick all AI agents from the active room. Per wren #11810 / viv #11806: the
  // response carries `remaining` (a point-in-time re-snapshot), but an agent can
  // join between the kick and a later hide click — so this only updates the UI;
  // the server's roomHasOtherMember check at hide time is the authoritative gate.
  if (roomSettingsKickAgentsBtn) roomSettingsKickAgentsBtn.addEventListener('click', function () {
    if (!activeRoomID || activeRoomID === '_system') return;
    var kickRoomID = activeRoomID;
    if (!confirm('Kick every AI agent from "' + kickRoomID + '"? They can re-join later.')) return;

    roomSettingsKickAgentsBtn.disabled = true;
    api('POST', '/rooms/' + encodeURIComponent(kickRoomID) + '/kick-agents').then(function (resp) {
      var kicked = (resp && resp.kicked) || [];
      var remaining = (resp && resp.remaining) || [];
      if (remaining.length) {
        // AI members survived the kick (e.g. a concurrent join) — room still not hideable.
        alert('Kicked ' + kicked.length + ' agent(s), but ' + remaining.length + ' remain(s): ' + remaining.join(', ') + '. The room is still not hideable.');
      } else {
        // `remaining` (AI-only) is empty, but under the solo-only rule the room is
        // hideable only when YOU are the sole member — another human still blocks it.
        // Drive the message off the member count, not `remaining`.
        var r = rooms.find(function (x) { return x.id === kickRoomID; });
        var others = (r && r.members || []).filter(function (mid) { return mid !== agentID; });
        if (others.length) {
          alert('Kicked ' + kicked.length + ' agent(s). ' + others.length + ' other member(s) are still in the room, so it can\'t be hidden yet.');
        } else if (kicked.length) {
          alert('Kicked ' + kicked.length + ' agent(s). You are now alone in the room and can hide it.');
        } else {
          alert('No AI agents were in the room.');
        }
      }
      // Refresh room state so the member list + button-disabled reflect reality.
      // The GET result is intentionally discarded: `rooms` is already updated by
      // WS room_update events by the time this lands — the fetch is a timing
      // barrier so renderRoomSettings runs after those events settle, not a data
      // source. Don't delete it (that reintroduces a race); if you touch it, call
      // renderRoomSettings directly and accept the WS-has-already-updated timing.
      return api('GET', '/rooms').then(function () { renderRoomSettings({ force: true }); });
    }).catch(function (err) {
      console.error('kick agents', err);
      alert('Failed to kick agents: ' + (err && err.message ? err.message : err));
    }).finally(function () {
      // Re-evaluate disabled state from the current room membership.
      var r = rooms.find(function (x) { return x.id === kickRoomID; });
      var stillHasAI = r && (r.members || []).some(function (mid) {
        var a = agents.find(function (x) { return x.id === mid; });
        return a && a.kind === 'ai';
      });
      roomSettingsKickAgentsBtn.disabled = !r || r.id === '_system' || !stillHasAI;
    });
  });
  // Hide/Unhide room — per-human, reversible. Solo-only rule enforced server-side;
  // the button is disabled when not solo so the user can't attempt a refused action.
  if (roomSettingsHideBtn) roomSettingsHideBtn.addEventListener('click', function () {
    if (!activeRoomID || activeRoomID === '_system') return;
    var hideRoomID = activeRoomID;
    var isHidden = !!(roomPrefs[hideRoomID] && roomPrefs[hideRoomID].hidden);
    var newHidden = !isHidden;
    api('POST', '/agents/' + encodeURIComponent(agentID) + '/rooms/' + encodeURIComponent(hideRoomID) + '/prefs', { hidden: newHidden })
      .then(function () { return fetchMyRooms(); })
      .then(function () {
        if (newHidden) {
          closeRoomSettings();
          alert('Room hidden — type its name in the join field to return.');
        }
        renderRoomSettings({ force: true });
      })
      .catch(function (err) {
        // 409 = not solo (someone joined between the render and the click).
        if (err && err.status === 409) {
          alert('Cannot hide this room while anyone else is in it.');
        } else {
          alert('Failed to update hide preference: ' + (err && err.message ? err.message : err));
        }
      });
  });
  // Pin/Unpin room — per-human, always available (except _system).
  if (roomSettingsPinBtn) roomSettingsPinBtn.addEventListener('click', function () {
    if (!activeRoomID || activeRoomID === '_system') return;
    var pinRoomID = activeRoomID;
    var isPinned = !!(roomPrefs[pinRoomID] && roomPrefs[pinRoomID].pinned);
    var newPinned = !isPinned;
    api('POST', '/agents/' + encodeURIComponent(agentID) + '/rooms/' + encodeURIComponent(pinRoomID) + '/prefs', { pinned: newPinned })
      .then(function () { return fetchMyRooms(); })
      .then(function () { renderRoomSettings({ force: true }); })
      .catch(function (err) {
        alert('Failed to update pin preference: ' + (err && err.message ? err.message : err));
      });
  });
  document.addEventListener('keydown', function (e) {
    var modifierOnly = (e.metaKey || e.ctrlKey) && !e.altKey && !e.shiftKey;
    if (modifierOnly && e.key === ',') {
      e.preventDefault();
      openSettings('general');
      return;
    }
    if (modifierOnly && e.key.toLowerCase() === 'w' && !settingsModal.classList.contains('hidden')) {
      e.preventDefault();
      closeSettings();
      return;
    }
    if (e.key === 'Escape' && attachmentLightboxModal && !attachmentLightboxModal.classList.contains('hidden')) {
      closeAttachmentLightbox();
      return;
    }
    if (e.key === 'Escape' && openQuestionsModal && !openQuestionsModal.classList.contains('hidden')) {
      closeOpenQuestionsModal();
      return;
    }
    if (e.key === 'Escape' && !messageDebugModal.classList.contains('hidden')) {
      closeMessageDebugModal();
      return;
    }
    if (e.key === 'Escape' && memoryViewerModal && !memoryViewerModal.classList.contains('hidden')) {
      closeMemoryViewer();
      return;
    }
    if (e.key === 'Escape' && leaderboardViewerModal && !leaderboardViewerModal.classList.contains('hidden')) {
      closeLeaderboardViewer();
      return;
    }
    if (e.key === 'Escape' && roomSettingsModal && !roomSettingsModal.classList.contains('hidden')) {
      closeRoomSettings();
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

  if (usagesBtn) {
    usagesBtn.addEventListener('click', function (e) {
      e.stopPropagation();
      if (rightSidebarMode === 'usages' && !rightCollapsed) closeUsagesPanel();
      else openUsagesPanel();
    });
  }
  if (usagesRefreshBtn) usagesRefreshBtn.addEventListener('click', forceRefreshUsages);
  if (rightUsagesPanel) rightUsagesPanel.addEventListener('click', function (e) {
    if (!e.target.closest('.usages-settings-shortcut')) return;
    openSettings('usages');
  });
  if (usagesRefreshInput) usagesRefreshInput.addEventListener('change', saveUsageRefreshInterval);
  document.querySelectorAll('.usages-percent-option').forEach(function (btn) {
    btn.addEventListener('click', function () {
      saveUsagePercentDisplay(btn.getAttribute('data-percent-display'));
    });
  });
  if (usagesProviderRows) {
    usagesProviderRows.addEventListener('change', function (e) {
      var input = e.target && e.target.closest ? e.target.closest('input[data-provider]') : null;
      if (!input || input.disabled) return;
      saveUsageProviderToggle(input.getAttribute('data-provider'), input.checked);
    });
    usagesProviderRows.addEventListener('input', function (e) {
      var input = e.target && e.target.closest ? e.target.closest('.copilot-enterprise-input') : null;
      if (input) copilotLoginState.enterpriseHost = input.value;
    });
    usagesProviderRows.addEventListener('click', function (e) {
      var move = e.target && e.target.closest ? e.target.closest('button[data-usage-provider-move]') : null;
      var login = e.target && e.target.closest ? e.target.closest('.copilot-login-btn') : null;
      var logout = e.target && e.target.closest ? e.target.closest('.copilot-logout-btn') : null;
      var cancel = e.target && e.target.closest ? e.target.closest('.copilot-cancel-btn') : null;
      var copy = e.target && e.target.closest ? e.target.closest('.copilot-copy-code') : null;
      var ollamaSave = e.target && e.target.closest ? e.target.closest('.ollama-cookie-save-btn') : null;
      var ollamaClear = e.target && e.target.closest ? e.target.closest('.ollama-cookie-clear-btn') : null;
      var ollamaUpdate = e.target && e.target.closest ? e.target.closest('.ollama-cookie-update-btn') : null;
      var ollamaCancel = e.target && e.target.closest ? e.target.closest('.ollama-cookie-cancel-btn') : null;
      var mistralSave = e.target && e.target.closest ? e.target.closest('.mistral-cookie-save-btn') : null;
      var mistralClear = e.target && e.target.closest ? e.target.closest('.mistral-cookie-clear-btn') : null;
      var mistralUpdate = e.target && e.target.closest ? e.target.closest('.mistral-cookie-update-btn') : null;
      var mistralCancel = e.target && e.target.closest ? e.target.closest('.mistral-cookie-cancel-btn') : null;
      if (move && !move.disabled) {
        moveUsageProvider(move.getAttribute('data-usage-provider-move'), move.getAttribute('data-direction'));
        return;
      }
      if (login) startCopilotLogin();
      if (logout) logoutCopilot();
      if (ollamaSave) saveOllamaCookie();
      if (ollamaClear) clearOllamaCookie();
      if (ollamaUpdate) {
        ollamaCookieEditorOpen = true;
        renderUsageProviderRows(usageProviders);
      }
      if (ollamaCancel) {
        ollamaCookieEditorOpen = false;
        renderUsageProviderRows(usageProviders);
      }
      if (mistralSave) saveMistralCookie();
      if (mistralClear) clearMistralCookie();
      if (mistralUpdate) {
        mistralCookieEditorOpen = true;
        renderUsageProviderRows(usageProviders);
      }
      if (mistralCancel) {
        mistralCookieEditorOpen = false;
        renderUsageProviderRows(usageProviders);
      }
      if (cancel) {
        if (copilotLoginState.timer) clearTimeout(copilotLoginState.timer);
        copilotLoginState.status = 'disconnected';
        copilotLoginState.flowId = '';
        renderUsageProviderRows(usageProviders);
      }
      if (copy) {
        copyText(copilotLoginState.userCode || '').then(function () {
          setTemporaryLabel(copy, 'Copied', 1200);
        }).catch(function () {
          setTemporaryLabel(copy, 'Copy failed', 1200);
        });
      }
    });
  }
  renderUsageProviderRows();

  // Switcher toggle (settings panel — delegated because the element is dynamic).
  document.addEventListener('change', function (e) {
    if (e.target && e.target.id === 'switcher-enabled-toggle') {
      switcherToggleEnabled(e.target.checked);
    }
  });

  // Switcher action buttons. Bound on document rather than on rightUsagesPanel
  // because the same rows render in two places: the usages panel (with Switch)
  // and Settings -> Switcher (manage only, no Switch button).
  document.addEventListener('click', function (e) {
    if (!e.target || !e.target.closest) return;
    var switchBtn = e.target.closest('.switcher-switch-btn, .switcher-switch-pill');
    var addBtn = e.target.closest('.switcher-add-btn');
    var importBtn = e.target.closest('.switcher-import-btn');
    var deleteBtn = e.target.closest('.switcher-delete-btn');
    if (switchBtn) {
      switcherSwitch(switchBtn.getAttribute('data-tool'), switchBtn.getAttribute('data-profile'), !!switchBtn.getAttribute('data-empty'));
    } else if (addBtn) {
      switcherAddProfile(addBtn.getAttribute('data-tool'));
    } else if (importBtn) {
      switcherImportProfile(importBtn.getAttribute('data-tool'));
    } else if (deleteBtn) {
      switcherDeleteProfile(deleteBtn.getAttribute('data-tool'), deleteBtn.getAttribute('data-profile'), !!deleteBtn.getAttribute('data-active'));
    }
  });

  // Theme select — writes to both localStorage (client-local) and server settings.
  themeSelect.addEventListener('change', function () {
    var next = themeSelect.value;
    localStorage.setItem('aimebu.ui.theme', next);
    saveSettings({ theme: next });
    applyTheme(next);
  });

  if (inlinePlanAppendixSelect) {
    inlinePlanAppendixSelect.addEventListener('change', function () {
      saveSettings({ inline_plan_appendix: inlinePlanAppendixSelect.value });
    });
  }

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

  if (memoryGlobalToggleBtn) {
    memoryGlobalToggleBtn.addEventListener('click', function () {
      setMemoryEnabled(!memoryGloballyEnabled());
    });
  }

  if (memoryViewerBtn) memoryViewerBtn.addEventListener('click', openMemoryViewer);
  if (memoryViewerCloseBtn) memoryViewerCloseBtn.addEventListener('click', closeMemoryViewer);
  if (memoryViewerOverlay) memoryViewerOverlay.addEventListener('click', closeMemoryViewer);
  if (leaderboardViewerBtn) leaderboardViewerBtn.addEventListener('click', openLeaderboardViewer);
  if (leaderboardViewerCloseBtn) leaderboardViewerCloseBtn.addEventListener('click', closeLeaderboardViewer);
  if (leaderboardViewerOverlay) leaderboardViewerOverlay.addEventListener('click', closeLeaderboardViewer);
  if (leaderboardRefreshBtn) leaderboardRefreshBtn.addEventListener('click', loadLeaderboard);
  if (leaderboardCategorySelect) leaderboardCategorySelect.addEventListener('change', loadLeaderboard);
  if (leaderboardIncludeSelfToggle) leaderboardIncludeSelfToggle.addEventListener('change', loadLeaderboard);
  if (memoryOnboardingEnableBtn) {
    memoryOnboardingEnableBtn.addEventListener('click', function () {
      setMemoryEnabled(true).then(closeMemoryOnboarding);
    });
  }
  if (memoryOnboardingDisableBtn) {
    memoryOnboardingDisableBtn.addEventListener('click', function () {
      setMemoryEnabled(false).then(closeMemoryOnboarding);
    });
  }
  if (roomMemorySelect) {
    roomMemorySelect.addEventListener('change', function () {
      if (!activeRoomID) return;
      var value = roomMemorySelect.value;
      var next = null;
      if (value === 'enabled') next = true;
      if (value === 'disabled') next = false;
      api('PUT', '/rooms/' + encodeURIComponent(activeRoomID) + '/memory', {
        memory_enabled: next
      }).then(function (updatedRoom) {
        rooms = rooms.map(function (r) { return r.id === updatedRoom.id ? updatedRoom : r; });
        renderRoomSettings({ force: true });
      }).catch(function (err) {
        alert('Failed to update room memory setting: ' + err.message);
        renderRoomSettings({ force: true });
      });
    });
  }

  [
    [retentionStaleAgentInput, 'stale_agent_window_seconds'],
    [retentionLivenessSweepInput, 'liveness_sweep_seconds'],
    [retentionAgentStaleInput, 'agent_stale_window_seconds'],
    [retentionAgentOfflineInput, 'agent_offline_window_seconds'],
    [retentionCleanupIntervalInput, 'cleanup_interval_seconds'],
    [retentionExpiryInput, 'messages_considered_expired_after_seconds']
  ].forEach(function (entry) {
    var input = entry[0];
    var field = entry[1];
    if (!input) return;
    input.addEventListener('change', function () {
      saveRetentionSetting(field, input);
    });
  });

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
  if (openQuestionsModalOverlay) {
    openQuestionsModalOverlay.addEventListener('click', closeOpenQuestionsModal);
  }
  if (openQuestionsModalCloseBtn) {
    openQuestionsModalCloseBtn.addEventListener('click', closeOpenQuestionsModal);
  }
  if (openQuestionsModal) {
    openQuestionsModal.addEventListener('click', function (e) {
      var step = e.target.closest('.open-questions-step');
      if (step) {
        e.preventDefault();
        if (step.hasAttribute('data-send-step')) {
          submitOpenQuestionsModal(e);
          return;
        }
        setOpenQuestionModalIndex(parseInt(step.getAttribute('data-q-index'), 10));
        return;
      }
      var back = e.target.closest('.open-questions-back');
      if (back) {
        e.preventDefault();
        setOpenQuestionModalIndex(openQuestionModalState.currentIndex - 1);
        return;
      }
      var next = e.target.closest('.open-questions-next');
      if (next) {
        e.preventDefault();
        setOpenQuestionModalIndex(openQuestionModalState.currentIndex + 1);
        return;
      }
      var send = e.target.closest('.open-questions-send');
      if (send) {
        e.preventDefault();
        submitOpenQuestionsModal(e);
      }
    });
    openQuestionsModal.addEventListener('change', function (e) {
      if (!e.target.matches('.open-question-sheet input[type="radio"]')) return;
      var msgID = openQuestionModalState.messageID;
      var qIdx = openQuestionModalState.currentIndex;
      var selector = e.target.getAttribute('data-selector') || e.target.value;
      var otherInput = openQuestionsModal.querySelector('.open-question-other-input');
      var value = selector === 'other' && otherInput ? otherInput.value : '';
      setOpenQuestionDraft(msgID, qIdx, selector, value);
      refreshOpenQuestionsModalIndicators();
      if (selector === 'other') {
        renderOpenQuestionsModal(true);
        return;
      }
      var questions = currentOpenQuestionList();
      if (qIdx < questions.length - 1) {
        setOpenQuestionModalIndex(qIdx + 1);
      } else if (openQuestionAllAnswered(msgID, questions)) {
        setOpenQuestionModalIndex(questions.length);
      } else {
        renderOpenQuestionsModal();
      }
    });
    openQuestionsModal.addEventListener('input', function (e) {
      if (!e.target.matches('.open-question-other-input')) return;
      var radio = openQuestionsModal.querySelector('.open-question-sheet input[data-selector="other"]');
      if (radio && !radio.checked) radio.checked = true;
      setOpenQuestionDraft(openQuestionModalState.messageID, openQuestionModalState.currentIndex, 'other', e.target.value);
      refreshOpenQuestionsModalIndicators();
    });
  }
  if (attachmentLightboxOverlay) attachmentLightboxOverlay.addEventListener('click', closeAttachmentLightbox);
  if (attachmentLightboxCloseBtn) attachmentLightboxCloseBtn.addEventListener('click', closeAttachmentLightbox);

  // Prime notification audio on first gesture so later attention pings play immediately.
  function primeNotificationAudioOnGesture() {
    if (notifAudioPrimed) return;
    primeNotificationAudio(currentNotificationSoundID(), false);
  }
  document.addEventListener('pointerdown', primeNotificationAudioOnGesture, { passive: true });
  document.addEventListener('keydown', primeNotificationAudioOnGesture, { passive: true });
  document.addEventListener('visibilitychange', maybeShowPendingNotificationPrompt);
  document.addEventListener('visibilitychange', function () { if (!document.hidden) refreshMessageSenderPresence(); });
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

  if (fleetAddBtn) fleetAddBtn.addEventListener('click', addFleet);
  if (fleetCopyAllBtn) fleetCopyAllBtn.addEventListener('click', function () {
    copyText(JSON.stringify(fleetEnvelope(), null, 2)).then(function () {
      setTemporaryLabel(fleetCopyAllBtn, 'Copied', 2000);
    }).catch(function (err) {
      console.error('Failed to copy fleets JSON:', err);
      setTemporaryLabel(fleetCopyAllBtn, 'Copy failed', 2500);
    });
  });
  if (fleetImportBtn) fleetImportBtn.addEventListener('click', function () {
    if (!navigator.clipboard || !navigator.clipboard.readText) {
      showFleetImportFallback();
      setTemporaryLabel(fleetImportBtn, 'Paste below', 2500);
      return;
    }
    navigator.clipboard.readText().then(function (text) {
      if (!text.trim()) throw new Error('Clipboard is empty');
      return applyImportedFleets(text, fleetImportBtn);
    }).then(function (imported) {
      if (imported) hideFleetImportFallback();
    }).catch(function (err) {
      console.error('Failed to read fleets JSON from clipboard:', err);
      showFleetImportFallback();
      setTemporaryLabel(fleetImportBtn, err.message === 'Clipboard is empty' ? 'Clipboard empty' : 'Paste below', 2500);
    });
  });
  if (fleetImportApplyBtn) {
    fleetImportApplyBtn.addEventListener('click', function () {
      var raw = fleetImportTextarea.value.trim();
      if (!raw) {
        setTemporaryLabel(fleetImportApplyBtn, 'Paste JSON first', 2500);
        return;
      }
      applyImportedFleets(raw, fleetImportApplyBtn).catch(function (err) {
        console.error('Failed to import pasted fleets JSON:', err);
        setTemporaryLabel(fleetImportApplyBtn, err.message, 2500);
      });
    });
  }
  if (fleetImportCancelBtn) {
    fleetImportCancelBtn.addEventListener('click', function () {
      hideFleetImportFallback();
    });
  }
  if (memoryRefreshBtn) memoryRefreshBtn.addEventListener('click', loadMemory);
  if (memoryScopeSelect) memoryScopeSelect.addEventListener('change', loadMemory);
  if (memoryScopeKeyInput) {
    memoryScopeKeyInput.addEventListener('keydown', function (e) {
      if (e.key === 'Enter') loadMemory();
    });
  }
  if (memoryAddForm) {
    memoryAddForm.addEventListener('submit', function (e) {
      e.preventDefault();
      addMemoryRecord();
    });
  }
  if (memoryCleanFilterBtn) {
    memoryCleanFilterBtn.addEventListener('click', function () {
      cleanMemory(true);
    });
  }
  if (memoryCleanAllBtn) {
    memoryCleanAllBtn.addEventListener('click', function () {
      cleanMemory(false);
    });
  }
  if (fleetImportTextarea) {
    fleetImportTextarea.addEventListener('keydown', function (e) {
      if (e.key === 'Escape') hideFleetImportFallback();
    });
  }
  if (fleetImportApplyBtn && !fleetImportTextarea) {
    fleetImportApplyBtn.addEventListener('click', function () {
      setTemporaryLabel(fleetImportApplyBtn, 'Paste JSON first', 2500);
    });
  }
  if (fleetsListEl) {
    fleetsListEl.addEventListener('input', function (e) {
      var commandInput = e.target.closest('.fleet-command-input');
      if (commandInput) updateFleetPreviewForInput(commandInput);
    });
    fleetsListEl.addEventListener('change', function (e) {
      var nameInput = e.target.closest('.fleet-name-input');
      if (nameInput) {
        renameFleet(nameInput.getAttribute('data-fleet'), nameInput.value, nameInput);
        return;
      }
      var cwdInput = e.target.closest('.fleet-agent-cwd');
      if (cwdInput) {
        var row = cwdInput.closest('.fleet-agent-row');
        if (!row) return;
        var cwdName = row.getAttribute('data-fleet');
        var cwdIdx = parseInt(row.getAttribute('data-index'), 10);
        if (fleets[cwdName] && fleets[cwdName].agents[cwdIdx]) {
          fleets[cwdName].agents[cwdIdx].auto_set_cwd = cwdInput.checked;
          var previewInput = row.querySelector('.fleet-command-input');
          if (previewInput) updateFleetPreviewForInput(previewInput);
          saveFleets();
        }
        return;
      }
      var commandInput = e.target.closest('.fleet-command-input');
      if (commandInput) {
        var fleetName = commandInput.getAttribute('data-fleet');
        var idx = parseInt(commandInput.getAttribute('data-index'), 10);
        if (fleets[fleetName] && fleets[fleetName].agents[idx]) {
          fleets[fleetName].agents[idx].command = commandInput.value;
          saveFleets();
        }
      }
    });
    fleetsListEl.addEventListener('click', function (e) {
      var addBtn = e.target.closest('.fleet-add-agent-btn');
      if (addBtn) {
        var addName = addBtn.getAttribute('data-fleet');
        if (!fleets[addName]) return;
        fleets[addName].agents = Array.isArray(fleets[addName].agents) ? fleets[addName].agents : [];
        if (fleets[addName].agents.length >= 16) {
          setTemporaryLabel(addBtn, 'Max 16', 1600);
          return;
        }
        fleets[addName].agents.push({ command: '', wrap_terminal: true, auto_set_cwd: true });
        renderFleetsList();
        return;
      }
      var removeAgentBtn = e.target.closest('.fleet-agent-delete-btn');
      if (removeAgentBtn) {
        var rmName = removeAgentBtn.getAttribute('data-fleet');
        var rmIdx = parseInt(removeAgentBtn.getAttribute('data-index'), 10);
        if (fleets[rmName] && Array.isArray(fleets[rmName].agents)) {
          fleets[rmName].agents.splice(rmIdx, 1);
          renderFleetsList();
          saveFleets();
        }
        return;
      }
      var copyBtnFleet = e.target.closest('.fleet-copy-btn');
      if (copyBtnFleet) {
        var copyName = copyBtnFleet.getAttribute('data-fleet');
        copyText(JSON.stringify(fleetEnvelope(copyName), null, 2)).then(function () {
          setTemporaryLabel(copyBtnFleet, 'Copied', 2000);
        }).catch(function (err) {
          console.error('Failed to copy fleet JSON:', err);
          setTemporaryLabel(copyBtnFleet, 'Copy failed', 2500);
        });
        return;
      }
      var deleteBtn = e.target.closest('.fleet-delete-btn');
      if (deleteBtn) {
        var delName = deleteBtn.getAttribute('data-fleet');
        if (!confirm('Delete fleet "' + delName + '"?')) return;
        delete fleets[delName];
        renderFleetsList();
        saveFleets();
      }
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

  // Roles reset-all
  if (rolesResetAllBtn) {
    rolesResetAllBtn.addEventListener('click', function () {
      if (!confirm('Reset all role overrides to defaults and delete custom roles? This will unassign roles from all rooms. Cannot be undone.')) return;
      api('DELETE', '/roles?force=true')
        .then(function () { return loadRoles(); })
        .catch(function (err) { alert('Error: ' + err.message); });
    });
  }

  // Role add form
  if (roleAddForm) {
    roleAddForm.addEventListener('submit', function (e) {
      e.preventDefault();
      var key = (roleKeyInput ? roleKeyInput.value.trim() : '');
      var emoji = (roleEmojiInput ? roleEmojiInput.value.trim() : '');
      var desc = (roleDescInput ? roleDescInput.value.trim() : '');
      var body = (roleBodyInput ? roleBodyInput.value : '');
      if (!key) return;
      var payload = buildRolesPayload(null, null);
      payload[key] = { description: desc, emoji: emoji, body: body, cardinality: 'multi' };
      api('PUT', '/roles', { roles: payload })
        .then(function () {
          if (roleKeyInput) roleKeyInput.value = '';
          if (roleEmojiInput) roleEmojiInput.value = '';
          if (roleDescInput) roleDescInput.value = '';
          if (roleBodyInput) roleBodyInput.value = '';
          return loadRoles();
        })
        .catch(function (err) { alert('Error: ' + err.message); });
    });
  }

  // Danger zone
  clearStateBtn.addEventListener('click', function () {
    if (!confirm('Clear all rooms, messages, and agents? Macros, fleets, and settings are preserved. This cannot be undone.')) return;
    clearState().catch(function (err) { alert('Error: ' + err.message); });
  });

  clearAllBtn.addEventListener('click', function () {
    if (!confirm('Clear everything including macros, fleets, and settings? This cannot be undone.')) return;
    clearAll().catch(function (err) { alert('Error: ' + err.message); });
  });

  if (attachmentPickerBtn && attachmentFileInput) {
    attachmentPickerBtn.addEventListener('click', function () {
      attachmentFileInput.click();
    });
    attachmentFileInput.addEventListener('change', function () {
      addAttachmentFiles(attachmentFileInput.files);
      attachmentFileInput.value = '';
    });
  }
  if (attachmentPendingList) {
    attachmentPendingList.addEventListener('click', function (e) {
      var btn = e.target.closest('.attachment-remove-btn');
      if (!btn) return;
      var item = btn.closest('.attachment-pending-item');
      if (item) removePendingAttachment(item.getAttribute('data-local-id'));
    });
  }
  if (sendBar) {
    sendBar.addEventListener('paste', function (e) {
      if (!e.clipboardData || !e.clipboardData.items) return;
      var files = [];
      Array.from(e.clipboardData.items).forEach(function (item) {
        if (item.kind === 'file') {
          var file = item.getAsFile();
          if (file) files.push(file);
        }
      });
      if (files.length) addAttachmentFiles(files);
    });
    sendBar.addEventListener('dragover', function (e) {
      if (!e.dataTransfer || !e.dataTransfer.files || !e.dataTransfer.files.length) return;
      e.preventDefault();
      sendBar.classList.add('drag-attachment');
    });
    sendBar.addEventListener('dragleave', function () {
      sendBar.classList.remove('drag-attachment');
    });
    sendBar.addEventListener('drop', function (e) {
      if (!e.dataTransfer || !e.dataTransfer.files || !e.dataTransfer.files.length) return;
      e.preventDefault();
      sendBar.classList.remove('drag-attachment');
      addAttachmentFiles(e.dataTransfer.files);
    });
  }

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

  if (leftSidebarToggle) leftSidebarToggle.addEventListener('click', toggleLeftSidebar);
  if (rightSidebarToggle) {
    rightSidebarToggle.addEventListener('click', function () {
      if (rightSidebarMode === 'profile') closeProfilePanel();
      else if (rightSidebarMode === 'usages') closeUsagesPanel();
      else toggleRightSidebar();
    });
  }
  messageListEl.addEventListener('scroll', function () {
    if (suppressScrollAnchor) return;
    updateScrollAnchor(!messageListResizeObserver);
    // Lazy-load older messages when scrolled to the top.
    if (activeRoomID && messageListEl.scrollTop < 50 &&
        messageListEl.scrollHeight > messageListEl.clientHeight + 50) {
      fetchOlderMessages(activeRoomID);
    }
  });
  if (window.ResizeObserver) {
    messageListResizeObserver = new ResizeObserver(scheduleResizeAnchorRestore);
    messageListResizeObserver.observe(messageListEl);
  }
  window.addEventListener('resize', scheduleResizeAnchorRestore);

  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape' && rightSidebarMode === 'profile') {
      closeProfilePanel();
      return;
    }
    if (e.key === 'Escape' && rightSidebarMode === 'usages') {
      closeUsagesPanel();
      return;
    }
    if (e.key !== 'Enter' && e.key !== ' ') return;
    if (e.target.closest('button, a, input, select, textarea')) return;
    var trigger = e.target.closest('[data-profile-agent-id]');
    if (!trigger) return;
    e.preventDefault();
    openProfilePanel(trigger.getAttribute('data-profile-agent-id'), trigger.getAttribute('data-profile-context') || 'room');
  });

  // Agent actions are delegated globally because buttons can live in the
  // right sidebar cards, settings cards, or the right-sidebar profile view.
  document.addEventListener('click', function (e) {
    var dmBtn = e.target.closest('.agent-dm-btn');
    var leaveBtn = e.target.closest('.agent-leave-btn');
    var deregBtn = e.target.closest('.agent-deregister-btn');

    if (dmBtn) {
      e.preventDefault();
      e.stopPropagation();
      var dmTargetID = dmBtn.getAttribute('data-agent-id');
      dmBtn.disabled = true;
      api('POST', '/dm', { from: agentID, to: dmTargetID, body: '' })
        .then(function (data) {
          closeProfilePanel();
          selectRoom(data.room);
          if (!settingsModal.classList.contains('hidden')) closeSettings();
        })
        .catch(function (err) { alert('Failed to open DM: ' + err.message); })
        .finally(function () { dmBtn.disabled = false; });
      return;
    }

    if (leaveBtn) {
      e.preventDefault();
      e.stopPropagation();
      var leaveTargetID = leaveBtn.getAttribute('data-agent-id');
      if (!activeRoomID) return;
      if (!confirm('Kick ' + leaveTargetID + ' from room "' + activeRoomID + '"?')) return;
      api('POST', '/rooms/' + encodeURIComponent(activeRoomID) + '/leave', { agent_id: leaveTargetID, kicked: true })
        .then(closeProfilePanel)
        .catch(function (err) { alert('Failed to kick agent from room: ' + err.message); });
      return;
    }

    if (deregBtn) {
      e.preventDefault();
      e.stopPropagation();
      var deregTargetID = deregBtn.getAttribute('data-agent-id');
      if (!confirm('Permanently deregister ' + deregTargetID + ' from all rooms?')) return;
      api('DELETE', '/agents/' + encodeURIComponent(deregTargetID))
        .then(closeProfilePanel)
        .catch(function (err) { alert('Failed to deregister agent: ' + err.message); });
      return;
    }

    var profileTrigger = e.target.closest('[data-profile-agent-id]');
    if (!profileTrigger) return;
    openProfilePanel(profileTrigger.getAttribute('data-profile-agent-id'), profileTrigger.getAttribute('data-profile-context') || 'room');
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

  messageListEl.addEventListener('pointerdown', function (e) {
    if (!window.matchMedia || !window.matchMedia('(hover: none)').matches) return;
    if (e.target.closest('button, a, summary, details, input, textarea, select, .message-attachment, .chat-msg-debug-toggle, .proposed-answer-btn, .open-questions-open, .chat-msg-id')) {
      return;
    }
    var msg = e.target.closest('.chat-msg');
    if (!msg || !msg.querySelector('.message-reaction-add')) return;
    messageListEl.querySelectorAll('.chat-msg.show-reaction-controls').forEach(function (node) {
      if (node !== msg) node.classList.remove('show-reaction-controls');
    });
    msg.classList.add('show-reaction-controls');
    if (reactionControlsHideTimer) clearTimeout(reactionControlsHideTimer);
    reactionControlsHideTimer = setTimeout(function () {
      msg.classList.remove('show-reaction-controls');
      reactionControlsHideTimer = null;
    }, 2200);
  });

  // Message ID badge (copy) and #NN autolinks (jump) — event delegation
  messageListEl.addEventListener('click', function (e) {
    var codeCopyBtn = e.target.closest('.md-copy-btn');
    if (codeCopyBtn) {
      e.preventDefault();
      var encodedCode = codeCopyBtn.getAttribute('data-code') || '';
      var codeText = '';
      try {
        codeText = decodeURIComponent(encodedCode);
      } catch (err) {
        console.error('Failed to decode code block payload:', err);
        setTemporaryLabel(codeCopyBtn, 'Copy failed', 1600);
        return;
      }
      codeCopyBtn.disabled = true;
      copyText(codeText).then(function () {
        codeCopyBtn.classList.add('copied');
        codeCopyBtn.innerHTML = CHECK_CODE_ICON;
        flashTitleHint(codeCopyBtn, 'Copied', 1200);
        setTimeout(function () {
          codeCopyBtn.innerHTML = COPY_CODE_ICON;
          codeCopyBtn.classList.remove('copied');
        }, 1000);
      }).catch(function (err) {
        console.error('Failed to copy code block:', err);
        flashTitleHint(codeCopyBtn, 'Copy failed', 1800);
      }).finally(function () {
        codeCopyBtn.disabled = false;
      });
      return;
    }
    var attachmentBtn = e.target.closest('.message-attachment');
    if (attachmentBtn) {
      e.preventDefault();
      openAttachmentLightbox(attachmentBtn.getAttribute('data-attachment-id'), attachmentBtn.getAttribute('data-attachment-name'));
      return;
    }
    var debugToggle = e.target.closest('.chat-msg-debug-toggle');
    if (debugToggle) {
      e.preventDefault();
      openMessageDebugModal(parseInt(debugToggle.getAttribute('data-msg-id'), 10));
      return;
    }
    var replyBtn = e.target.closest('.chat-msg-reply');
    if (replyBtn) {
      e.preventDefault();
      setPendingReply(parseInt(replyBtn.getAttribute('data-msg-id'), 10));
      return;
    }
    var reactionBtn = e.target.closest('.message-reaction-pill, .message-reaction-option');
    if (reactionBtn) {
      e.preventDefault();
      var reactionMsgID = parseInt(reactionBtn.getAttribute('data-msg-id'), 10);
      var emoji = reactionBtn.getAttribute('data-emoji');
      var msgForReaction = findMessageInRoom(activeRoomID, reactionMsgID);
      var existing = msgForReaction && Array.isArray(msgForReaction.reactions)
        ? msgForReaction.reactions.find(function (r) { return r && r.emoji === emoji; })
        : null;
      var removeReaction = !!(existing && existing.me);
      reactionBtn.disabled = true;
      sendReaction(reactionMsgID, emoji, removeReaction).finally(function () {
        reactionBtn.disabled = false;
        var picker = reactionBtn.closest('.message-reaction-picker');
        if (picker) picker.open = false;
      });
      return;
    }
    var answerBtn = e.target.closest('.proposed-answer-btn');
    if (answerBtn) {
      e.preventDefault();
      var answerMsgID = parseInt(answerBtn.getAttribute('data-msg-id'), 10);
      var answerIdx = parseInt(answerBtn.getAttribute('data-answer-index'), 10);
      var msg = (messages[activeRoomID] || []).find(function (m) { return m.id === answerMsgID; });
      var answers = msg && Array.isArray(msg.proposed_answers) ? msg.proposed_answers : [];
      if (!msg || answerIdx < 0 || answerIdx >= answers.length) return;
      var room = activeRoom();
      var reply = mentionForAuthor(msg.from, room) + ' ' + answers[answerIdx];
      if (e.shiftKey) {
        msgBodyInput.value = reply;
        setPendingReply(answerMsgID);
        answeredProposedAnswers[String(answerMsgID)] = true;
        renderMessages();
        msgBodyInput.focus();
        msgBodyInput.setSelectionRange(msgBodyInput.value.length, msgBodyInput.value.length);
        resizeMsgInput();
        updateAcPopup();
        return;
      }
      sendMessage(reply, undefined, answerMsgID).then(function (res) {
        if (!res) return;
        answeredProposedAnswers[String(answerMsgID)] = true;
        renderMessages();
      });
      return;
    }
    var openQuestionOpen = e.target.closest('.open-questions-open');
    if (openQuestionOpen) {
      e.preventDefault();
      openOpenQuestionsModal(openQuestionOpen.getAttribute('data-msg-id'));
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

  if (replyPendingEl) {
    replyPendingEl.addEventListener('click', function (e) {
      var clearBtn = e.target.closest('.reply-pending-clear');
      if (clearBtn) {
        e.preventDefault();
        clearPendingReply();
        msgBodyInput.focus();
        return;
      }
      var ref = e.target.closest('.msg-ref');
      if (ref) {
        e.preventDefault();
        jumpToMessage(parseInt(ref.getAttribute('data-msg-id'), 10), ref);
      }
    });
  }

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
    if (composerAutocomplete.handleKeydown(e)) return;
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

  // Send message
  sendForm.addEventListener('submit', function (e) {
    e.preventDefault();
    var body = msgBodyInput.value.trim();
    var attachments = readyPendingAttachments();
    if (!body && !attachments.length) return;
    if (hasAttachmentUploadsInFlight()) return;
    historyIdx = null;
    historyDraft = null;
    hideAcPopup();
    var replyTo = pendingReply && pendingReply.roomID === activeRoomID ? pendingReply.messageID : 0;
    sendMessage(body, attachments, replyTo).then(function (res) {
      if (res) {
        clearPendingAttachments();
        clearPendingReply();
      }
    });
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
  setInterval(refreshAgentStateBadges, 1000);

  setInterval(function () {
    messageListEl.querySelectorAll('.chat-msg-time').forEach(function (el) {
      el.textContent = relativeTime(el.title);
    });
    // Bound the room's in-memory cache (only when scrolled to the
    // bottom — see pruneMessageCache), then check the expiry render signature.
    // One re-render if either changed; a quiet room costs nothing.
    // The count covers icon-only drift the anchor alone misses (all-live→all-
    // expired, a middle message crossing). A system message becoming the first
    // live message in an all-expired room (anchor null→id) is caught here too —
    // on the next tick (up to 30s) rather than instantly. That's deliberate: the
    // SSE caller at app.js:4661 already re-renders for non-system messages, so a
    // per-appendMessage check would double-render every real message.
    if (activeRoomID) {
      var pruned = pruneMessageCache(activeRoomID);
      var sig = expiryRenderSignature();
      if (pruned || sig.anchor !== lastRenderAnchor || sig.count !== lastRenderCount) renderMessages();
    }
    renderRooms();
    renderAllAgents();
    renderRoomAgents();
    renderRoomSettings();
  }, 30000);

  // ── Init ─────────────────────────────────────────────────────────

  function initPublicShell() {
    setMobileTab('rooms');
    applySidebarCollapseState();
    renderRightSidebar();
    updateMdToggleBtn();

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
  }

  function startSession() {
    if (sessionStarted) return;
    sessionStarted = true;
    registerHuman().catch(function () {}).then(function () {
      return Promise.all([
        loadSettings().catch(function () {}),
        fetchMyRooms().catch(function () {}),
        loadAgentSessions().catch(function () {}),
        loadMacros().catch(function () {}),
        loadFleets().catch(function () {}),
        loadRoles().catch(function () {})
      ]);
    }).finally(function () {
      maybeShowMemoryOnboarding();
      connectWS();
    });
  }

  agentIDInput.value = agentID;
  initPublicShell();
  if (agentIDFromStorage) {
    startSession();
  } else {
    showWelcomeGate();
  }
})();
