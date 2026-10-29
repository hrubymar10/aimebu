import { request, upload, currentUser, currentLogin } from './api.js';
import { PREFERENCE_KEYS, savePreference } from './preferences.js';
import { retention } from './retention.js';
import { expiryCutoffMs, planExpiry } from './expiry.js';
import { EXPIRED_HOURGLASS } from './usage-expiry.js';
import { usageTooltip } from './message-usage-format.js';
import { toggleMessageUsage, closeMessageUsage } from './message-usage.js';
import { renderMarkdown, renderRaw, copyText } from './markdown.js';
import { setGlyph, iconGlyph } from './icon-button.js';
import { stampTime, refreshRelativeTimes } from './time.js';
import { ScrollAnchor } from './scroll-anchor.js';
import { senderIdentity, senderKind, createSenderPresence, applySenderIdentity, agentLabel } from './sender-identity.js';
import { openSenderProfile } from './pages/sender-profile/page.js';
import { renderVisualPlan } from './visual-plan.js';
import { RoomPanel } from './pages/room-panel/page.js';
import { QuickCommands } from './quick-commands.js';
import { MacroAutocomplete } from './macros.js';
import { MentionAutocomplete, MentionIndex, highlightMentions } from './mentions.js';
import { EmojiAutocomplete } from './emoji-suggest.js';
import { Notice, megabytes } from './notice.js';
import { closeEmojiPicker, openEmojiPicker } from './emoji-picker.js';
import { LiveAnnouncer } from './live-region.js';
import { ComposerFormatting } from './composer.js';
import { buildMessageActions, messageAction } from './message-actions.js';
import { answerControls, answerHistory } from './answer-controls.js';
import { promptDialog } from './dialogs.js';
import { expandShortcodes } from './emoji-data.js';
import { ChatStore } from './pages/chat/store.js';
import { MENTION_LOGINS_TTL_MS, answerArea, answerEligible, answersThroughInbox as answersThrough, articleClass, groupReactions, latestLabel, liveArrival as isLiveArrival, liveRoute, messageSnippet, newestShownSeq as shownSeq, newRepliesLabel, overCap, plainText, recallCandidates as ownMessages, recallStep, threadChip, threadCount, threadLabel as threadLabelOf, threadTitle, trimCap } from './pages/chat/model.js';

export { plainText, messageSnippet };

// "Back to latest" and its thread twins are icon buttons; a count of missed messages is a pill on the icon, and the tooltip and
// accessible name carry the full wording ("Back to latest · 2 new").
function setLatest(button, label, count = 0) {
  button.replaceChildren(iconGlyph('arrow-down'));
  if (count) { const pill = document.createElement('span'); pill.className = 'badge'; pill.textContent = String(count); button.append(pill); }
  button.title = label; button.setAttribute('aria-label', label);
}

export const FLASH_MS = 1500;
export class Chat {
  constructor(onRoomMutation) {
    this.onRoomMutation = onRoomMutation;
    this.store = new ChatStore(this);
    this.list = document.querySelector('#messages'); this.title = document.querySelector('#room-title'); this.roomIcon = document.querySelector('#room-icon'); this.members = document.querySelector('#room-members'); this.form = document.querySelector('#composer'); this.draft = this.form.elements.body; this.formatting = new ComposerFormatting(this.draft, { onFiles: files => this.addFiles(files) });
    this.announcer = new LiveAnnouncer(document.querySelector('#live-announcer'));
    // Older pages load by themselves when the list is scrolled (or keyboard-scrolled: Home, PageUp) to the top; this only says so while it fetches.
    this.olderStatus = document.createElement('div'); this.olderStatus.className = 'older-status'; this.olderStatus.setAttribute('role', 'status'); this.olderStatus.textContent = 'Loading older messages…'; this.olderStatus.hidden = true;
    this.list.before(this.olderStatus);
    this.latestButton = document.createElement('button'); this.latestButton.type = 'button'; this.latestButton.className = 'icon-btn back-to-latest'; setLatest(this.latestButton, 'Back to latest'); this.latestButton.hidden = true;
    this.latestButton.onclick = () => this.open(this.room);
    this.title.parentElement.append(this.latestButton);
    this.list.addEventListener('scroll', () => {
      if (this.list.scrollTop < 80) this.loadOlder();
      // A history window pages forward again when its bottom is reached (only then is scrollHeight read on scroll).
      if (this.contextMode && this.list.scrollHeight - this.list.scrollTop - this.list.clientHeight < 80) this.loadNewer();
    });
    this.scrollAnchor = new ScrollAnchor(this.list);
    this.list.addEventListener('keydown', event => this.moveActionFocus(event));
    this.chip = document.querySelector('#reply-chip'); this.chipText = this.chip.querySelector('#reply-chip-jump'); this.chipText.onclick = () => { if (this.reply) this.jumpToMessage(this.reply.id); }; this.error = document.querySelector('#chat-error'); this.nodes = new Map();
    this.pendingAttachments = []; this.pendingUploads = 0; this.pendingNode = document.querySelector('#pending-attachments'); this.attachmentInput = document.querySelector('#attachment-input');
    this.attachmentInput.onchange = () => { this.addFiles([...this.attachmentInput.files]); this.attachmentInput.value = ''; };
    this.draft.addEventListener('input', () => this.resizeDraft());
    document.addEventListener('click', event => { const ref = event.target.closest?.('a.msg-ref'); if (!ref || event.defaultPrevented || event.button) return; event.preventDefault(); this.jumpToMessage(Number(ref.dataset.no)); });
    this.draft.addEventListener('paste', event => { const files = [...event.clipboardData?.files || []]; if (files.length) { event.preventDefault(); this.addFiles(files); } });
    this.form.addEventListener('dragover', event => { if (event.dataTransfer?.types.includes('Files')) { event.preventDefault(); this.form.classList.add('drag-attachment'); } });
    this.form.addEventListener('dragleave', event => { if (!this.form.contains(event.relatedTarget)) this.form.classList.remove('drag-attachment'); });
    this.form.addEventListener('drop', event => { this.form.classList.remove('drag-attachment'); if (event.dataTransfer?.files.length) { event.preventDefault(); this.addFiles([...event.dataTransfer.files]); } });
    setInterval(() => refreshRelativeTimes(this.list), 30000);
    this.lightbox = document.querySelector('#image-lightbox'); document.querySelector('#image-lightbox-close').onclick = () => this.lightbox.close();
    this.lightbox.onclick = event => { if (event.target === this.lightbox) this.lightbox.close(); };
    this.agents = new Map(); this.threadPanel = document.querySelector('#thread-panel'); this.threadTitle = this.threadPanel.querySelector('h2'); this.threadList = document.querySelector('#thread-messages'); this.threadForm = document.querySelector('#thread-composer'); this.threadDraft = this.threadForm.elements.body; this.threadFormatting = new ComposerFormatting(this.threadDraft);
    this.threadPageSize = 100; this.threadOlderPageSize = 50;
    this.threadOlderStatus = document.createElement('div'); this.threadOlderStatus.className = 'older-status thread-older'; this.threadOlderStatus.setAttribute('role', 'status'); this.threadOlderStatus.textContent = 'Loading older replies…'; this.threadOlderStatus.hidden = true;
    this.threadLatestButton = document.createElement('button'); this.threadLatestButton.type = 'button'; this.threadLatestButton.className = 'icon-btn back-to-latest thread-latest'; setLatest(this.threadLatestButton, 'Latest replies'); this.threadLatestButton.hidden = true;
    this.threadLatestButton.onclick = () => { if (this.threadRoot) this.openThread(this.threadRoot); };
    this.threadNewButton = document.createElement('button'); this.threadNewButton.type = 'button'; this.threadNewButton.className = 'icon-btn back-to-latest thread-new'; setLatest(this.threadNewButton, 'New replies'); this.threadNewButton.hidden = true;
    this.threadNewButton.onclick = () => { if (this.threadRoot) this.openThread(this.threadRoot); };
    this.threadList.before(this.threadOlderStatus, this.threadLatestButton, this.threadNewButton);
    this.threadList.addEventListener('scroll', () => { if (this.threadList.scrollTop < 80) this.loadOlderThread(); });
    this.delegateRows(this.list); this.delegateRows(this.threadList);
    this.threadForm.onsubmit = event => { event.preventDefault(); this.sendThread(); };
    document.querySelector('#close-thread').onclick = () => this.closeThread();
    this.form.onsubmit = event => { event.preventDefault(); this.send(); };
    this.recall = { index: null, draft: '' };
    this.draft.addEventListener('input', () => { this.recall.index = null; });
    for (const field of [this.draft, this.threadDraft]) field.addEventListener('focus', () => { if (this.room) this.loadMentionLogins(this.room); });
    this.draft.addEventListener('keydown', event => {
      if (this.recallKey(event)) return;
      if (event.key === 'Enter' && !event.shiftKey && !event.isComposing && this.quickCommands?.popup.hidden !== false && this.macroAutocomplete?.popup.hidden !== false && this.mentionAutocomplete?.popup.hidden !== false && this.emojiAutocomplete?.popup.hidden !== false) { event.preventDefault(); this.form.requestSubmit(); }
    });
    this.threadDraft.addEventListener('keydown', event => {
      if (event.key === 'Enter' && !event.shiftKey && !event.isComposing && this.threadMentionAutocomplete?.popup.hidden !== false && this.threadEmojiAutocomplete?.popup.hidden !== false) { event.preventDefault(); this.threadForm.requestSubmit(); }
    });
    document.querySelector('#cancel-reply').onclick = () => this.setReply(null);
    this.uploadRequests = new Set();
    this.raw = false;
    this.panel = new RoomPanel(this);
    document.querySelector('#room-jump').onclick = () => this.promptJump();
    document.querySelector('#room-raw').onclick = () => this.toggleRaw();
    this.quickCommands = new QuickCommands(this.formatting.input, () => this.archiveRoom(), () => this.spawnCallback?.(),
      () => this.canArchiveRoom(), () => this.canSpawnAgent());
    this.macroAutocomplete = new MacroAutocomplete(this.formatting.input);
    this.notice = new Notice(this.form, 'composer-notice'); this.threadNotice = new Notice(this.threadForm, 'thread-notice'); this.mentionAutocomplete = new MentionAutocomplete(this.formatting.input); this.threadMentionAutocomplete = new MentionAutocomplete(this.threadFormatting.input); this.mentionIndex = null; this.mentionLogins = []; this.mentionLoginsRoom = ''; this.mentionLoginsAt = 0; this.mentionLoginsSeq = (this.mentionLoginsSeq || 0) + 1;
    this.emojiAutocomplete = new EmojiAutocomplete(this.formatting.input); this.threadEmojiAutocomplete = new EmojiAutocomplete(this.threadFormatting.input);
    this.emojiButton = document.querySelector('#emoji-button'); this.emojiButton.onclick = () => openEmojiPicker({ anchor: this.emojiButton, onPick: glyph => this.insertEmoji(glyph) });
    for (const button of document.querySelectorAll('.room-controls button')) button.disabled = true;
    retention.onChange(() => this.scheduleExpiry());
    setInterval(() => this.syncExpiry(), 30000);
    if (window.__exposeChat) window.__chat = this; // test hook: the live-cap smoke reads the per-message maps
  }
  open(room) { return this.store.open(room); }
  // The view before a room's first page: the app's and the composer's state for the room, an empty list.
  beginOpen(room) {
    this.announcer.reset();
    if (this.room?.id !== room.id) { this.pendingAttachments = []; this.renderPending(); if (this.recall) this.recall.index = null; }
    closeMessageUsage(); this.room = room; this.syncEmpty(); this.setRoomFacts(room); this.error.textContent = ''; this.notice.clear(); this.threadNotice.clear();
    this.store.clearRoom(); this.store.initialLoading = true; this.nodes.clear(); this.list.replaceChildren(); this.olderStatus.hidden = true; this.list.dataset.hasOlder = 'false'; this.latestButton.hidden = true; this.updateLatestLabel(); this.setReply(null); this.closeThread();
    for (const button of document.querySelectorAll('.room-controls button')) button.disabled = false;
    this.setRoomFacts(room);
    this.scrollAnchor.reset();
    delete this.list.dataset.empty; // no hint while the room is still loading
  }
  drawPage(messages) {
    for (const message of messages) this.add(message);
    this.list.dataset.empty = String(!this.list.querySelector('article.message:not(.system-notice)')); // notices alone leave it empty
  }
  showNewest() { this.list.dataset.hasOlder = String(this.hasMoreOlder); this.list.scrollTop = this.list.scrollHeight; this.scrollAnchor?.update(); }
  latestShown(room) { this.onLatest?.(room); }
  // How far the read mark may go (0 while a room is still loading). At the latest view the list is contiguous to the live
  // cursor: every message up to newestSeq is applied (top-level rows rendered, thread replies counted on their roots).
  // Otherwise (a history window, or its catch-up in flight) only the newest top-level row actually in the list counts,
  // never a thread reply, which an open thread panel can hold far past the window's end.
  newestShownSeq() {
    const lastRowSeq = () => {
      for (let node = this.list.lastElementChild; node; node = node.previousElementSibling) if (node.matches('article.message[data-message-id]')) return Number(node.dataset.seq) || 0;
      return 0;
    };
    return shownSeq({ initialLoading: this.initialLoading, contextMode: this.contextMode, catchingUp: this.catchingUp, newestSeq: this.newestSeq, lastRowSeq });
  }
  // Live top-level messages that arrived while a history window was shown: they are not in the list, so the
  // Back to latest button counts them.
  updateLatestLabel() {
    const count = this.missedIDs?.size || 0;
    setLatest(this.latestButton, latestLabel(count), count);
  }
  reset() {
    this.store.reset(); if (this.recall) this.recall.index = null; this.announcer.reset();
    for (const controller of this.uploadRequests) controller.abort();
    this.uploadRequests.clear();
    this.room = null; this.syncEmpty(); this.panel.reset();
    this.error.textContent = ''; this.notice.clear(); this.threadNotice.clear(); this.nodes.clear(); this.agents.clear(); this.list.replaceChildren(); this.olderStatus.hidden = true; this.list.dataset.hasOlder = 'false'; this.latestButton.hidden = true; this.updateLatestLabel(); this.answerItems = new Map(); this.closeBlocked?.clear(); this.worktree = null;
    this.setReply(null); this.closeThread(); this.pendingAttachments = []; this.pendingUploads = 0; this.pendingNode.replaceChildren(); this.draft.value = ''; this.resizeDraft();
    this.lightbox.close();
    this.quickCommands.reset(); this.macroAutocomplete.reset(); this.mentionAutocomplete.reset(); this.threadMentionAutocomplete.reset(); this.emojiAutocomplete.reset(); this.threadEmojiAutocomplete.reset(); closeEmojiPicker(false); this.mentionIndex = null; this.mentionLogins = []; this.mentionLoginsRoom = ''; this.mentionLoginsAt = 0; this.mentionLoginsSeq = (this.mentionLoginsSeq || 0) + 1;
    for (const button of document.querySelectorAll('.room-controls button')) button.disabled = true;
    this.draft.disabled = true; this.emojiButton.disabled = true; this.attachmentInput.disabled = true; this.form.querySelector('button[type=submit]').disabled = true;
  }
  destroy() { this.reset(); this.store.destroy(); this.panel.destroy(); }
  // With no room open the chat shows an empty-state card instead of the composer and room controls; the app decides what the card says.
  syncEmpty() { this.onEmptyChange?.(!this.room); }
  current(session, roomID) { return session === this.sessionGeneration && this.room?.id === roomID; }
  // A list too short to scroll cannot ask for more by scrolling, so a short page with more behind it asks right away.
  loadOlderIfShort() {
    requestAnimationFrame(() => { if (this.hasMoreOlder && this.list.scrollHeight <= this.list.clientHeight + 80) this.loadOlder(); });
  }
  loadOlder() { return this.store.loadOlder(); }
  showOlderStatus(shown) { this.olderStatus.hidden = !shown; }
  // Older rows go above the first row without moving what the reader looks at: only the height inserted above the anchor is compensated, never the
  // reader's own movement, so the anchor is measured now, not before the request.
  prependPage(messages) {
    const anchor = this.list.firstElementChild, anchorTop = anchor?.getBoundingClientRect().top;
    for (let i = messages.length - 1; i >= 0; i--) this.add(messages[i], true);
    this.list.dataset.hasOlder = String(this.hasMoreOlder);
    if (anchor?.isConnected) this.list.scrollTop += anchor.getBoundingClientRect().top - anchorTop;
    this.trimNewest();
  }
  say(text) { this.error.textContent = text; }
  anchorUpdate() { this.scrollAnchor?.update(); }
  showLatestButton(shown) { this.latestButton.hidden = !shown; }
  // Scrolling back through a long room would otherwise keep every page in the DOM (about 49 elements per message). Past the cap the
  // newest rows are dropped and the view becomes a history window: live messages stop appending, scrolling to its bottom pages
  // forward again, and "Back to latest" reloads.
  renderCap() { return window.__renderCap || 800; } // __renderCap is a test hook (the perf smoke lowers it)
  // The live end has its own, smaller cap: a room left open keeps receiving messages, and without it every arrival stayed in
  // the list (rows, listeners, records) for as long as the tab lived. __liveCap is a test hook; otherwise a lowered
  // __renderCap lowers it too, so the live cap never exceeds the history cap.
  liveCap() { return window.__liveCap || Math.min(300, this.renderCap()); }
  // After live appends: a reader following the bottom keeps the live cap, one scrolled up keeps the history cap (their rows
  // are not taken from under them, see trimOldest). The dropped rows come back by scrolling up (hasMoreOlder is set).
  // Never while an older page is in flight: it was requested before the current oldest row and would land behind a gap.
  trimLive() {
    if (this.contextMode || this.loadingOlder || this.initialLoading) return;
    const atBottom = this.list.scrollHeight - this.list.scrollTop - this.list.clientHeight < 48;
    if (this.trimOldest(trimCap(atBottom, this.liveCap(), this.renderCap()))) this.scrollAnchor?.update();
  }
  renderedRows() { return [...this.list.children].filter(node => node.matches?.('article.message[data-message-id]')); }
  // A dropped row takes its thread replies and counts along; they come back with the row when its page is loaded again
  // (kept, they would linger in the inspector's previous/next and stay in memory). The open thread keeps its replies.
  dropRows(rows) {
    const gone = new Set(), open = this.threadRoot?.id;
    for (const node of rows) {
      const id = node.dataset.messageId;
      node.remove(); this.nodes.delete(id); this.messages.delete(id); this.reactions.delete(id); gone.add(id);
      if (id !== open) { this.threadCounts.delete(id); this.serverThreadCounts.delete(id); }
    }
    for (const [id, message] of this.messages) if (message.thread_root && message.thread_root !== open && gone.has(message.thread_root)) this.messages.delete(id);
  }
  // A trim never removes what the reader is looking at: when the rows to drop reach into the visible part of the list it
  // is deferred to the next page, by which time the reader has moved on (only possible with a cap near a screenful).
  trimNewest() {
    const rows = this.renderedRows(), cap = this.renderCap();
    if (!overCap(rows.length, cap) || this.loadingNewer) return;
    if (rows[cap].getBoundingClientRect().top < this.list.getBoundingClientRect().bottom) return;
    this.dropRows(rows.slice(cap));
    this.windowSeq = Number(rows[cap - 1].dataset.seq);
    this.contextMode = true; this.latestButton.hidden = false;
  }
  // The mirror of trimNewest while a history window pages forward: past the cap the oldest rows go, and the list is
  // scrolled by the height they took so the reader does not move.
  // Also used by the live end (trimLive) with its own cap. Returns whether rows were dropped.
  trimOldest(cap = this.renderCap()) {
    const rows = this.renderedRows();
    if (!overCap(rows.length, cap)) return false;
    if (rows[rows.length - cap - 1].getBoundingClientRect().bottom > this.list.getBoundingClientRect().top) return false;
    const keep = rows[rows.length - cap], keepTop = keep.getBoundingClientRect().top;
    this.dropRows(rows.slice(0, rows.length - cap));
    this.list.scrollTop += keep.getBoundingClientRect().top - keepTop;
    this.oldestSeq = Number(keep.dataset.seq); this.hasMoreOlder = true; this.list.dataset.hasOlder = 'true';
    return true;
  }
  loadNewer() { return this.store.loadNewer(); }
  leaveWindow() { return this.store.leaveWindow(); }
  setSpawnOpener(openSpawn) { this.spawnCallback = openSpawn; }
  // The two things a historical sender's profile can do: start it as a v2 agent (opens the spawn dialog prefilled), and show the agent once started.
  setLegacyActions(start, open) { this.legacyStarter = start; this.legacyOpener = open; }
  setDebugInspector(open) { this.debugCallback = open; }
  // v1's bug icon: opens the message's debug inspector. It is hidden unless Settings > General turns the inspector on.
  // The turn's spend, from the chart icon beside the number, on a message an agent wrote (the server sends `usage` with it).
  usageButton(message) {
    if (!message.usage) return null;
    const button = document.createElement('button'); button.type = 'button'; button.className = 'row-btn quiet-glyph chat-msg-usage-toggle';
    button.title = usageTooltip(message.usage); button.setAttribute('aria-label', button.title); button.setAttribute('aria-haspopup', 'dialog'); button.setAttribute('aria-expanded', 'false');
    const glyph = document.createElement('span'); glyph.className = 'icon-mask icon-mask-chart-column'; glyph.setAttribute('aria-hidden', 'true'); button.append(glyph);
    return button;
  }
  debugButton(message) {
    const button = document.createElement('button'); button.type = 'button'; button.className = 'row-btn quiet-glyph chat-msg-debug-toggle'; button.title = 'Open debug inspector'; button.setAttribute('aria-label', 'Open debug inspector');
    const glyph = document.createElement('span'); glyph.className = 'icon-mask icon-mask-bug'; glyph.setAttribute('aria-hidden', 'true'); button.append(glyph);
    return button;
  }
  // One click and one key listener per list serve every row's controls (see onMessageClick), so a row costs no closures.
  // Rows also install them (once per list), for a chat assembled without its constructor.
  delegateRows(container) {
    if (!container || container.__rowsDelegated) return;
    container.__rowsDelegated = true;
    container.addEventListener('click', event => this.onMessageClick(event));
    container.addEventListener('keydown', event => this.onAuthorKey(event));
  }
  // The row controls of the message list and the thread panel, delegated: the control's row names the message.
  onMessageClick(event) {
    const control = event.target.closest?.('button, strong.message-author, span.mention[role=button]');
    if (!control || !event.currentTarget.contains(control)) return;
    const holder = control.closest('[data-message-id]'); if (!holder || !event.currentTarget.contains(holder)) return;
    const message = this.entryMessage(holder.dataset.messageId); if (!message) return;
    const is = name => control.classList.contains(name);
    if (is('message-author')) { if (control.getAttribute('role') === 'button') this.openSender(message); return; }
    if (is('mention')) { this.openMention(control.textContent); return; }
    if (is('message-number')) this.copyNumber(control, message);
    else if (is('chat-msg-debug-toggle')) this.debugCallback?.(message);
    else if (is('chat-msg-usage-toggle')) toggleMessageUsage(control, message.usage);
    else if (is('quote')) this.jumpToMessage(message.reply_to);
    else if (is('image-thumb')) { const full = this.lightbox.querySelector('img'); full.src = control.dataset.url; this.lightbox.showModal(); }
    else if (is('thread-chip')) this.openThread(message);
    else if (is('reaction-pill')) this.toggleReaction(message.id, control.dataset.emoji);
    else messageAction(this, control, message);
  }
  // A departed or historical sender's name is a role=button: Enter and Space open its profile like a click.
  onAuthorKey(event) {
    if (event.key !== 'Enter' && event.key !== ' ') return;
    const author = event.target.closest?.('strong.message-author[role=button], span.mention[role=button]');
    if (!author) return;
    const holder = author.closest('[data-message-id]'), message = holder && this.entryMessage(holder.dataset.messageId);
    if (!message) return;
    event.preventDefault();
    if (author.classList.contains('mention')) this.openMention(author.textContent); else this.openSender(message);
  }
  async copyNumber(number, message) {
    try { await copyText(`#${message.no}`); number.classList.add('copied'); } catch { number.title = 'Copy failed'; }
    setTimeout(() => { number.classList.remove('copied'); number.title = 'Click to copy'; }, 1500);
  }
  // The record behind a rendered row or thread entry. The open thread's root keeps its panel entry after its row was
  // trimmed from a history window, when it is no longer in this.messages.
  entryMessage(id) { return this.messages.get(id) || (this.threadRoot?.id === id ? this.threadRoot : undefined); }
  // The room's messages, oldest first, for the inspector's previous and next.
  orderedMessages() { return [...this.messages.values()].sort((a, b) => a.seq - b.seq); }
  threadLabel(id) {
    return threadLabelOf(threadCount(this.threadCounts, this.serverThreadCounts, id));
  }
  setInbox(inbox) { this.inbox = inbox; }
  // `messageID` is the message the container belongs to (an item is not there once it has been answered). A question this viewer
  // had open that is now gone from the inbox leaves a quiet, disabled "Answered" in its place for the rest of the session, as v1
  // did, instead of just losing its buttons.
  syncAnswerControl(container, item, messageID) {
    if (!container) return;
    const current = container.querySelector(':scope > .answer-controls');
    const id = messageID ?? item?.message_id;
    const eligible = answerEligible(item);
    const marker = () => container.querySelector(':scope > .answer-done');
    const history = () => container.querySelector(':scope > .answer-history');
    if (!eligible) {
      if (current && !item && id) (this.answeredMessages ||= new Set()).add(id);
      current?.remove();
      // A question or proposed answers on a message with nothing open for it (history, an imported ask, an answered one) stay
      // visible under it, disabled: "Answered" when it was answered, otherwise marked as no longer pending.
      const message = id ? this.messages?.get(id) || (this.threadRoot?.id === id ? this.threadRoot : null) : null;
      const answered = Boolean(id && (this.answeredMessages?.has(id) || this.repliedTo?.has(id))), area = answerArea(item, message, answered);
      if (area === 'history') {
        marker()?.remove();
        const shown = history();
        if (shown?.dataset.answered === String(answered)) return;
        shown?.remove();
        container.append(answerHistory(message, answered));
        return;
      }
      history()?.remove();
      if (area === 'done' && !marker()) {
        const done = document.createElement('div'); done.className = 'answer-done';
        const button = document.createElement('button'); button.type = 'button'; button.className = 'btn'; button.disabled = true; button.textContent = 'Answered';
        done.append(button); container.append(done);
      }
      return;
    }
    history()?.remove();
    marker()?.remove(); this.answeredMessages?.delete(id);
    if (current?.dataset.inboxId === item.id && current.dataset.rev === String(item.rev)) return;
    current?.remove();
    const parent = id ? this.messages?.get(id) || (this.threadRoot?.id === id ? this.threadRoot : null) : null;
    const controls = answerControls(item, value => this.inbox.answer(item, value), this.answerOptions(parent));
    controls.dataset.inboxId = item.id; controls.dataset.rev = String(item.rev);
    container.append(controls);
  }
  // How a proposed answer addresses the message it answers: "@author answer" is what a click sends, and Shift-click puts the same text in the composer,
  // as a reply to that message, to change before sending.
  answerOptions(message) {
    if (!message || message.author_kind === 'system') return {};
    const agent = message.author_kind === 'user' ? null : this.agents?.get(message.author_id);
    const name = agent?.slug || (message.author_kind === 'user' ? message.author_name : '');
    return {
      mention: name ? `@${name}` : '',
      compose: text => {
        this.draft.value = text; this.resizeDraft(); this.setReply(message);
        this.draft.setSelectionRange?.(text.length, text.length);
      },
    };
  }
  updateAnswerControls(items) {
    this.answerItems = new Map(items.map(item => [item.message_id, item]));
    for (const [id, node] of this.nodes) {
      this.syncAnswerControl(node.querySelector('.message-bubble'), this.answerItems.get(id), id);
    }
    for (const entry of this.threadList.querySelectorAll('.thread-entry[data-message-id]')) this.syncAnswerControl(entry, this.answerItems.get(entry.dataset.messageId), entry.dataset.messageId);
  }
  // Inserts at the caret through execCommand so the edit stays undoable; the range assignment is the fallback.
  insertEmoji(glyph) {
    const draft = this.draft; if (draft.disabled) return;
    draft.focus();
    if (!document.execCommand?.('insertText', false, glyph)) { draft.setRangeText(glyph, draft.selectionStart, draft.selectionEnd, 'end'); draft.dispatchEvent(new Event('input', { bubbles: true })); }
  }
  resizeDraft() { this.draft.style.height = '39px'; this.draft.style.height = `${Math.min(this.draft.scrollHeight, 180)}px`; }
  setRoomFacts(room) {
    if (this.room?.id !== room.id) { this.mentionLogins = []; this.mentionLoginsRoom = ''; this.mentionLoginsAt = 0; this.mentionLoginsSeq = (this.mentionLoginsSeq || 0) + 1; }
    this.room = room;
    this.panel.setRoom(room);
    this.refreshMentions();
    this.loadMentionLogins(room);
    const archivedProject = !!this.projectArchived?.(room.project_id);
    const blocked = this.closeBlocked?.get(room.id);
    if (blocked && room.lifecycle === 'closing') this.error.textContent = `Room closing blocked: ${blocked}`;
    else if (this.error.textContent.startsWith('Room closing blocked:')) this.error.textContent = '';
    if (room.lifecycle !== 'closing') this.closeBlocked?.delete(room.id);
    const open = (!room.lifecycle || room.lifecycle === 'open') && !archivedProject;
    // A viewer can read an ordinary room but not write in it or run agents there (a private chat between two members is the exception),
    // so those controls are off with the reason, rather than live and answering 403.
    // A chat between agents has no project of its own: the server says whether this person may write in it (a role in one of the agents' projects).
    const agentChat = !!room.agent_chat;
    const viewer = agentChat ? room.can_write === false : this.projectRole?.(room.project_id) === 'viewer';
    const writable = open && (!viewer || (room.kind === 'dm' && !agentChat));
    this.draft.disabled = !writable; this.emojiButton.disabled = !writable;
    this.form.querySelector('button[type=submit]').disabled = !writable || this.pendingUploads > 0;
    this.attachmentInput.disabled = !writable;
    this.threadDraft.disabled = !writable;
    this.threadForm.querySelector('button[type=submit], button:not([type])').disabled = !writable;
    const spawn = document.querySelector('#spawn-open');
    spawn.disabled = !open || room.kind === 'dm' || viewer;
    this.spawnTitle ||= spawn.title;
    spawn.title = viewer ? 'Viewers cannot spawn agents' : this.spawnTitle;
    const note = this.viewerNote ||= (() => { const node = document.createElement('p'); node.className = 'composer-note'; node.id = 'composer-viewer-note'; node.setAttribute('role', 'note'); this.form.before(node); return node; })();
    note.hidden = !(open && viewer && (room.kind !== 'dm' || agentChat));
    note.textContent = agentChat ? 'You can read this chat. Writing here takes the owner or operator role in one of the agents\' projects.' : 'You can read this room as a viewer. Only owners and operators of the project can write here or run agents.';
    // An archived project is read-only for everyone; the composer says why and where to undo it.
    const archivedNote = this.archivedNote ||= (() => { const node = document.createElement('p'); node.className = 'composer-note'; node.id = 'composer-archived-note'; node.setAttribute('role', 'note'); this.form.before(node); return node; })();
    archivedNote.hidden = !archivedProject;
    archivedNote.textContent = 'This project is archived and read-only. The owner or an admin can reopen it in the project\'s home-room settings.';
  }
  get renameForm() { return this.panel.renameForm; }
  get memorySection() { return this.panel.memorySection; }
  get notesSection() { return this.panel.notesSection; }
  get worktreeSection() { return this.panel.worktreeSection; }
  get infoDialog() { return this.panel.infoDialog; }
  get exportDialog() { return this.panel.exportDialog; }
  showRoomInfo() { this.panel.show(); }
  canRenameRoom(room) { return this.panel.canRename(room); }
  saveRoomTitle() { return this.panel.saveTitle(); }
  loadRoomMemory() { return this.panel.store.loadMemory(); }
  changeRoomMemory() { return this.panel.changeMemory(); }
  openRoomNotes() { this.panel.openNotes(); }
  loadWorktree() { return this.panel.store.loadWorktree(); }
  createWorktree() { return this.panel.store.createWorktree(); }
  removeWorktree() { return this.panel.store.removeWorktree(); }
  canArchiveRoom() {
    const room = this.room, role = room && this.projectRole?.(room.project_id);
    return !!room && room.kind === 'task' && (!room.lifecycle || room.lifecycle === 'open') && (role === 'owner' || role === 'operator');
  }
  canSpawnAgent() { return !!this.room && !document.querySelector('#spawn-open').disabled; }
  archiveRoom() {
    if (!this.canArchiveRoom()) return;
    return this.changeRoomLifecycle('archive');
  }
  changeRoomLifecycle(intent = 'toggle') { return this.panel.store.changeLifecycle(intent); }
  leaveChat() { return this.panel.store.leave(); }
  removeRoom() { return this.panel.store.remove(); }
  showCloseBlocked(roomID, reason) {
    this.closeBlocked ||= new Map();
    const label = reason === 'agent_stop_invalid_transition' ? 'an agent could not be stopped' : (reason || 'unknown reason').replaceAll('_', ' ');
    this.closeBlocked.set(roomID, label);
    if (this.room?.id === roomID) this.setRoomFacts(this.room);
  }
  async promptJump() {
    if (!this.room) return;
    const value = await promptDialog('Go to message #', { title: 'Go to message', label: 'Message number', inputMode: 'numeric', confirm: 'Go', required: true, validate: text => (/^[1-9]\d*$/.test(text) ? '' : 'Enter a message number.') });
    if (value === null) return;
    this.jumpToMessage(Number(value.trim()));
  }
  // A short highlight on a message the reader was just taken to (a #N link, a reply quote, jump-to, an Inbox item), so the eye finds it. It
  // lasts FLASH_MS; with reduced motion the same class is a plain outline and does not animate (see the stylesheet).
  flash(node) {
    if (!node) return;
    clearTimeout(node.flashTimer); node.classList.remove('message-flash'); void node.offsetWidth; node.classList.add('message-flash');
    node.flashTimer = setTimeout(() => node.classList.remove('message-flash'), FLASH_MS);
  }
  // `key` is a message number (the "#N" people write, one sequence across every room) or a message id. A number is looked up on the
  // server when this room has not loaded it, and a message of another room opens that room (onOpenMessage) on that message.
  jumpToMessage(key) { return this.store.jump(key); }
  openMessage(found) { this.onOpenMessage?.(found); }
  // A history window around a message: the list starts again from the page that holds it.
  beginWindow() { this.olderStatus.hidden = true; this.list.dataset.hasOlder = 'false'; this.closeThread(); this.nodes.clear(); this.list.replaceChildren(); }
  drawWindow(messages) { for (const item of messages) this.add(item); }
  showWindow() { this.list.dataset.hasOlder = String(this.hasMoreOlder); this.latestButton.hidden = false; }
  showReply(message) { const entry = this.threadList.querySelector(`[data-message-id="${CSS.escape(message.id)}"]`); entry?.scrollIntoView({ block: 'center' }); this.flash(entry); }
  showRow(message) { const row = this.nodes.get(message.id); row?.scrollIntoView({ block: 'center' }); this.scrollAnchor?.update(); this.flash(row?.querySelector('.message-bubble') || row); }
  renderBody(element, message) { element.replaceChildren(); if (this.raw) element.append(renderRaw(message.body)); else { element.append(renderMarkdown(message.body)); highlightMentions(element, this.mentionIndex); expandShortcodes(element); } if (message.attachments?.length) this.renderAttachments(element, message.attachments); }
  renderAttachments(element, attachments) {
    const strip = document.createElement('div'); strip.className = 'message-images';
    for (const attachment of attachments) {
      // Retention removed the file; the message keeps this note where the attachment was.
      if (attachment.removed_at) {
        const note = document.createElement('span'); note.className = 'attachment-removed'; note.dataset.attachmentId = attachment.id;
        const days = attachment.removed_after_days;
        note.textContent = `${attachment.name || (attachment.mime.startsWith('image/') ? 'Image' : 'Attachment')}: attachment removed${days > 0 ? ` after ${days} days` : ''}`;
        strip.append(note); continue;
      }
      if (!attachment.mime.startsWith('image/')) {
        const link = document.createElement('a'); link.href = attachment.url; link.className = 'attachment-file';
        link.textContent = attachment.name || 'Attachment'; link.download = attachment.name || 'attachment';
        strip.append(link); continue;
      }
      const button = document.createElement('button'); button.type = 'button'; button.className = 'row-btn image-thumb'; button.setAttribute('aria-label', 'Open attached image');
      const img = document.createElement('img'); img.src = attachment.url; img.alt = 'Attached image'; img.loading = 'lazy';
      // The size, when known, keeps the thumbnail's place so the list does not jump as images load.
      if (attachment.width > 0 && attachment.height > 0) { img.width = attachment.width; img.height = attachment.height; }
      button.append(img);
      button.dataset.url = attachment.url; // opened by onMessageClick
      // v1 names the file under its thumbnail.
      const figure = document.createElement('figure'); figure.className = 'image-item'; const caption = document.createElement('figcaption'); caption.textContent = attachment.name || 'Image'; caption.title = caption.textContent;
      figure.append(button, caption); strip.append(figure);
    }
    element.append(strip);
  }
  async addFiles(files) {
    if (!this.room || this.room.lifecycle !== 'open') return;
    this.notice.clear(); // a new pick, paste or drop starts over: what was said about the last one no longer holds
    const roomID = this.room.id;
    const session = this.sessionGeneration;
    for (const file of files) {
      if (!this.current(session, roomID)) return;
      const name = file.name || 'The file';
      if (this.pendingAttachments.length + this.pendingUploads >= 4) { this.notice.show('Up to four files per message.', { append: true }); break; }
      if (!file.size) { this.notice.show(`${name} is empty.`, { append: true }); continue; }
      if (file.size > 10 * 1024 * 1024) { this.notice.show(`${name} is too big (${megabytes(file.size)}). Use files up to 10 MB.`, { append: true }); continue; }
      const controller = new AbortController(); this.uploadRequests.add(controller);
      this.pendingUploads++; this.renderPending();
      try {
        const attachment = await upload(`/api/rooms/${roomID}/attachments`, file, { signal: controller.signal });
        if (this.current(session, roomID)) this.pendingAttachments.push(attachment);
        else await request(`/api/rooms/${roomID}/attachments/${attachment.id}`, { method: 'DELETE' }).catch(() => {});
      } catch (error) { if (this.current(session, roomID)) this.notice.show(`${name}: ${error.message}`, { append: true }); }
      finally { this.uploadRequests.delete(controller); if (session === this.sessionGeneration) { this.pendingUploads--; this.renderPending(); } }
    }
  }
  renderPending() {
    this.pendingNode.replaceChildren();
    for (const attachment of this.pendingAttachments) {
      const item = document.createElement('span'); item.className = 'pending-image';
      const preview = attachment.mime.startsWith('image/') ? document.createElement('img') : document.createElement('span');
      if (preview.tagName === 'IMG') { preview.src = attachment.url; preview.alt = 'Pending image'; }
      else preview.textContent = attachment.name || 'Attachment';
      const remove = document.createElement('button'); remove.type = 'button'; remove.className = 'icon-btn'; setGlyph(remove, 'close', 'Remove file');
      remove.onclick = async () => { if (!this.room) return; const roomID = this.room.id, session = this.sessionGeneration; try { await request(`/api/rooms/${roomID}/attachments/${attachment.id}`, { method: 'DELETE' }); if (!this.current(session, roomID)) return; this.pendingAttachments = this.pendingAttachments.filter(a => a.id !== attachment.id); this.renderPending(); } catch (error) { if (this.current(session, roomID)) this.error.textContent = error.message; } };
      item.append(preview, remove); this.pendingNode.append(item);
    }
    if (this.pendingUploads) { const status = document.createElement('span'); status.textContent = `Uploading ${this.pendingUploads}…`; this.pendingNode.append(status); }
    this.form.querySelector('button[type=submit]').disabled = this.pendingUploads > 0 || this.room?.lifecycle !== 'open';
  }
  // Whether message bodies show as raw Markdown is a personal preference kept on the server (see preferences.js); the browser copy lets the
  // page paint it at once. It is read again after sign-in, when the server's value is known.
  applyMarkdownMode() {
    let stored = null;
    try { stored = localStorage.getItem(PREFERENCE_KEYS.markdown_mode(currentUser)); } catch { /* private browsing: rendered */ }
    this.setRaw(stored === 'raw');
  }
  toggleRaw() {
    this.setRaw(!this.raw);
    const mode = this.raw ? 'raw' : 'rendered';
    try { localStorage.setItem(PREFERENCE_KEYS.markdown_mode(currentUser), mode); } catch { /* the server copy still applies */ }
    savePreference('markdown_mode', mode);
  }
  setRaw(raw) {
    this.raw = raw;
    const button = document.querySelector('#room-raw');
    button.setAttribute('aria-pressed', String(this.raw));
    button.title = this.raw ? 'Show rendered Markdown' : 'Show raw Markdown';
    button.setAttribute('aria-label', button.title);
    for (const element of document.querySelectorAll('.chat .message[data-message-id] .message-body, .chat .thread-entry[data-message-id] .message-body')) {
      const message = this.entryMessage(element.closest('[data-message-id]').dataset.messageId);
      if (message) this.renderBody(element, message);
    }
  }
  exportRoom(format) { return this.panel.exportRoom(format); }
  refreshMessages() { return this.store.refresh(); }
  // What the open thread panel knows of a live message: whether its root is drawn, whether the panel is open on it and at its end.
  threadState(message) { return { rootRendered: this.nodes.has(message.thread_root), openThreadID: this.threadRoot?.id, threadLatestHidden: this.threadLatestButton.hidden, threadEntryKnown: this.threadEntryIDs.has(message.id) }; }
  announce(message) { this.announcer.arrival({ id: message.id, mine: message.author_kind === 'user' && message.author_id === currentUser, from: this.authorName(message), body: message.body, room: this.room.title }); }
  // Only messages that land in the main list while the room is open are announced: not history, older
  // pages, thread replies or system notices. What this tab sent is silenced by the announcer, by id.
  liveArrival(message) {
    return isLiveArrival(message, this);
  }
  async fetchMessage() {
    await this.refreshMessages();
  }
  // The login list changes with project membership, which room.member_count
  // does not track (it counts disabled users too). Refetch on room change, on
  // project member events (force), and on composer focus once it is older than
  // the TTL, so a deactivated or newly joined human shows up without a reload.
  async loadMentionLogins(room, force = false) {
    const fresh = this.mentionLoginsRoom === room.id && Date.now() - this.mentionLoginsAt < MENTION_LOGINS_TTL_MS;
    if (!force && fresh) return;
    const seq = ++this.mentionLoginsSeq;
    this.mentionLoginsRoom = room.id; this.mentionLoginsAt = Date.now();
    try {
      const { logins } = await request(`/api/rooms/${room.id}/mention-users`);
      if (this.room?.id !== room.id || this.mentionLoginsSeq !== seq) return;
      this.mentionLogins = logins || []; this.refreshMentions();
    } catch { if (this.mentionLoginsSeq === seq) this.mentionLoginsAt = 0; }
  }
  // An agent that wrote in this room and is no longer a member (or came from the v1 import) still reads as @name in message bodies:
  // its slug and its full id are remembered per room, and the highlight is redone once after a burst of messages.
  noteAuthor(message) {
    if (message.author_kind !== 'agent') return;
    const name = this.authorName(message), set = this.historicalAuthors ||= new Set();
    if (name.startsWith('Agent ')) return;
    const keys = [name, name.split('@')[0]].map(key => key.toLowerCase()).filter(key => /^[a-z][a-z0-9_-]*(@[a-z0-9_.-]+)?$/.test(key));
    let added = false; for (const key of keys) if (!set.has(key)) { set.add(key); added = true; }
    if (added && !this.mentionTimer) this.mentionTimer = setTimeout(() => { this.mentionTimer = null; this.refreshMentions(); }, 60);
  }
  refreshMentions() {
    const index = this.room ? new MentionIndex([...this.agents.values()].filter(agent => agent.room_id === this.room.id), this.room.project_id, this.mentionLogins, [...(this.historicalAuthors || [])], currentLogin) : null;
    if (index?.signature === this.mentionIndex?.signature) return;
    this.mentionIndex = index;
    this.mentionAutocomplete?.setIndex(index); this.threadMentionAutocomplete?.setIndex(index);
    if (!this.raw) for (const body of document.querySelectorAll('.chat .message-body')) highlightMentions(body, index);
  }
  setAgents(agents) {
    this.agents = new Map(agents.map(agent => [agent.id, agent]));
    // One signature per agent record, so updateAuthor can skip nodes whose author did not change between polls.
    this.agentSigs = new Map(agents.map(agent => [agent.id, JSON.stringify(agent)]));
    this.refreshMentions();
    for (const [id, node] of this.nodes) this.updateAuthor(node, this.messages.get(id));
    for (const node of this.threadList.querySelectorAll('.thread-entry')) this.updateAuthor(node, this.entryMessage(node.dataset.messageId));
    if (this.reply) this.chipText.textContent = `Replying to ${this.authorName(this.reply)}: ${messageSnippet(this.reply)}`;
  }
  authorName(message) {
    if (message.author_kind === 'system') return 'System';
    if (message.author_kind === 'user') return message.author_id === currentUser ? 'You' : message.author_name || (String(message.author_id).startsWith('usr_') ? 'Someone' : message.author_id); // never the raw account id
    const agent = this.agents.get(message.author_id);
    if (agent?.name || agent?.slug) return agentLabel(agent);
    // A native agent whose record is gone has only an opaque id; an imported v1 author's id is already the readable name.
    return message.author_id.startsWith('agt_') ? `Agent ${message.author_id.slice(-6)}` : message.author_id;
  }
  updateAuthor(node, message) {
    if (!message) return;
    const author = node.querySelector('.message-author');
    if (!author) return;
    const sig = `${message.author_kind}|${message.author_id}|${this.agentSigs?.get(message.author_id) ?? ''}`;
    if (node.__authorSig === sig) return;
    node.__authorSig = sig;
    author.textContent = this.authorName(message);
    const presence = node.querySelector('.msg-sender-presence');
    if (presence) applySenderIdentity(presence, senderIdentity(message, this.agents.get(message.author_id)));
    const role = node.querySelector('.message-role');
    if (role) { const agent = this.agents.get(message.author_id); role.textContent = agent?.role_emoji || ''; role.title = agent?.role || ''; role.hidden = !role.textContent; }
    // A sender with no live record (imported history, or an agent that left) has its name open a read-only profile.
    const kind = senderKind(message, this.agents.get(message.author_id));
    author.dataset.senderKind = kind;
    // A live agent's name opens its profile in the members panel; a sender with no live record opens the read-only one.
    const live = kind === 'live' && message.author_kind === 'agent';
    const link = kind !== 'live' || live;
    author.classList.toggle('message-author-link', link);
    if (link) { author.tabIndex = 0; author.setAttribute('role', 'button'); author.title = live ? 'Open profile' : `${kind === 'historical' ? 'Historical sender' : 'Departed agent'}: open profile`; }
    else { author.removeAttribute('tabindex'); author.removeAttribute('role'); author.removeAttribute('title'); }
  }
  // A mention of an agent opens its profile: the live one in this room, or else an author of the history that has left.
  openMention(text) {
    const token = String(text).replace(/^@/, '').toLowerCase(), slug = token.split('@')[0];
    const live = [...this.agents.values()].filter(agent => agent.room_id === this.room?.id && String(agent.slug).toLowerCase() === slug);
    if (live.length === 1) { this.legacyOpener?.(live[0].id); return; }
    const written = [...this.messages.values()].find(message => message.author_kind === 'agent' && !this.agents.has(message.author_id)
      && [this.authorName(message), this.authorName(message).split('@')[0]].some(name => name.toLowerCase() === token));
    if (written) this.openSender(written);
  }
  openSender(message) {
    const kind = senderKind(message, this.agents.get(message.author_id));
    if (kind === 'live') { if (message.author_kind === 'agent') this.legacyOpener?.(message.author_id); return; }
    return openSenderProfile({ name: this.authorName(message), authorID: message.author_id, kind, roomID: this.room?.id, messages: this.messages.values(), identity: senderIdentity(message, undefined), onStartAgent: this.legacyStarter, onOpenAgent: this.legacyOpener });
  }
  authorMeta(message) {
    const author = document.createElement('strong'); author.className = 'message-author';
    const role = document.createElement('span'); role.className = 'message-role';
    const wrapper = document.createElement('span'); wrapper.className = 'message-author-wrap';
    if (message.author_kind === 'system') wrapper.append(author, role); else wrapper.append(createSenderPresence(), author, role);
    this.updateAuthor(wrapper, message); return wrapper;
  }
  // The viewer's own messages in this room, oldest first, consecutive duplicates collapsed.
  recallCandidates() { return ownMessages(this.messages.values(), currentUser); }
  // Terminal-style history: Up at an empty composer or caret start walks back through own messages (once recalling, on the first line),
  // Down walks forward (restoring the in-progress draft past the newest), Esc restores the draft.
  // Returns true when the key was consumed. Open autocomplete popups keep their own arrow keys.
  recallKey(event) {
    if (event.isComposing || event.shiftKey || event.ctrlKey || event.altKey || event.metaKey) return false;
    if (![this.quickCommands, this.macroAutocomplete, this.mentionAutocomplete].every(popup => popup?.popup.hidden !== false)) return false;
    const draft = this.draft, recall = this.recall;
    const walking = event.key === 'ArrowUp' || event.key === 'ArrowDown';
    const step = recallStep({ key: event.key, value: draft.value, at: draft.selectionStart, end: draft.selectionEnd }, recall, walking ? this.recallCandidates() : []);
    if (!step) return false;
    event.preventDefault();
    recall.index = step.index; recall.draft = step.draft;
    draft.value = step.value; draft.setSelectionRange(step.value.length, step.value.length); this.resizeDraft();
    return true;
  }
  // Arrow keys move between the buttons of a focused action bar.
  moveActionFocus(event) {
    const bar = event.target.closest?.('.message-actions'); if (!bar || !['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return;
    const buttons = [...bar.querySelectorAll('button')].filter(button => !button.closest('[hidden]')), index = buttons.indexOf(document.activeElement); if (index < 0) return;
    event.preventDefault();
    const next = event.key === 'Home' ? 0 : event.key === 'End' ? buttons.length - 1 : (index + (event.key === 'ArrowRight' ? 1 : -1) + buttons.length) % buttons.length;
    buttons[next].focus();
  }
  // A prepended (older) or paged (newer, in a history window) message leaves the scroll position alone; only a live
  // append follows the bottom.
  add(message, prepend = false, paged = false) {
    if (Number.isInteger(message.thread_count)) {
      const rootID = message.thread_root || message.id;
      this.serverThreadCounts.set(rootID, Math.max(this.serverThreadCounts.get(rootID) || 0, message.thread_count));
      this.updateThreadCount(rootID);
    }
    if (this.messages.has(message.id)) return;
    this.noteAuthor(message);
    this.messages.set(message.id, message);
    if (message.reply_to && message.author_kind === 'user') {
      // A person replying to a question is its answer, for the history block under that question.
      (this.repliedTo ||= new Set()).add(message.reply_to);
      const asked = this.nodes.get(message.reply_to); if (asked) this.syncAnswerControl(asked.querySelector('.message-bubble'), this.answerItems?.get(message.reply_to), message.reply_to);
    }
    if (this.missedIDs?.delete(message.id)) this.updateLatestLabel();
    if (message.thread_root) {
      const count = (this.threadCounts.get(message.thread_root) || 0) + 1;
      this.threadCounts.set(message.thread_root, count);
      this.updateThreadCount(message.thread_root);
      // The thread panel pages and refreshes itself; a forward page of the main list could land outside the replies it shows.
      if (this.threadRoot?.id === message.thread_root && !paged) this.insertThreadEntry(message);
      return;
    }
    this.delegateRows(this.list);
    const quiet = prepend || paged;
    const atBottom = !quiet && this.list.scrollHeight - this.list.scrollTop - this.list.clientHeight < 48;
    // Reading scrollTop or scrollHeight forces a layout of the whole list. A page is added message by message, so
    // these are read only for a live appended message, which is the one case that uses them.
    const oldTop = quiet ? 0 : this.list.scrollTop;
    if (this.list.dataset.empty === 'true' && message.author_kind !== 'system') this.list.dataset.empty = 'false';
    const article = document.createElement('article'); article.className = articleClass(message, currentUser); article.dataset.messageId = message.id; article.dataset.seq = String(message.seq); article.dataset.no = String(message.no);
    if (message.author_kind === 'system') {
      const row = document.createElement('div'); row.className = 'system-row';
      const body = document.createElement('span'); body.className = 'message-body'; body.textContent = message.body;
      const time = stampTime(document.createElement('time'), message.created_at);
      row.append(body, time, this.debugButton(message)); article.append(row);
      const visualPlan = renderVisualPlan(message); if (visualPlan) article.append(visualPlan);
      this.nodes.set(message.id, article); if (prepend) this.list.prepend(article); else this.list.append(article);
      if (!quiet) { if (atBottom) this.list.scrollTop = this.list.scrollHeight; else this.list.scrollTop = oldTop; this.scrollAnchor?.update(); }
      this.scheduleExpiry();
      return;
    }
    article.append(this.messageMeta(message, article));
    const bubble = document.createElement('div'); bubble.className = 'message-bubble'; article.append(bubble);
    if (message.reply_to) {
      const quote = document.createElement('button'); quote.className = 'row-btn quote'; quote.type = 'button';
      // A parent in the loaded window is read from it; one outside it comes with the reply itself (reply_preview, from the server).
      const loaded = this.messages.get(message.reply_to), preview = message.reply_preview;
      const parent = loaded ? { no: loaded.no, name: this.authorName(loaded), snippet: messageSnippet(loaded) }
        : preview ? { no: preview.no, name: this.authorName({ author_kind: preview.author_kind, author_id: preview.author_id, author_name: preview.author_name }), snippet: preview.snippet } : null;
      quote.textContent = parent ? `↪ #${parent.no} ${parent.name}: ${parent.snippet}`.trimEnd().replace(/:$/, '') : '↪ Earlier message';
      if (parent) quote.title = `Go to #${parent.no}`;
      bubble.append(quote);
    }
    const body = document.createElement('div'); body.className = 'message-body'; this.renderBody(body, message); bubble.append(body);
    const visualPlan = renderVisualPlan(message); if (visualPlan) bubble.append(visualPlan);
    const actions = buildMessageActions(this, message);
    const pills = document.createElement('span'); pills.className = 'reaction-pills'; const threadChip = document.createElement('button'); threadChip.type = 'button'; threadChip.className = 'row-btn thread-chip';
    bubble.append(actions, pills); article.append(threadChip);
    this.nodes.set(message.id, article); if (prepend) this.list.prepend(article); else this.list.append(article);
    this.updateThreadCount(message.id);
    this.syncAnswerControl(bubble, this.answerItems?.get(message.id), message.id);
    if (!quiet) { if (atBottom) this.list.scrollTop = this.list.scrollHeight; else this.list.scrollTop = oldTop; this.scrollAnchor?.update(); }
    if (Array.isArray(message.reactions)) this.showReactions(message.id, message.reactions);
    else Promise.resolve(this.refreshReactions(message.id)).catch(() => {});
    this.scheduleExpiry();
  }
  // Expiry marks (an hourglass and one divider) are display-only: the chat shows every message. They are
  // recomputed once per task after a burst of add() calls, when the window changes, and every 30 s.
  scheduleExpiry() {
    if (this.expiryQueued) return;
    this.expiryQueued = true;
    queueMicrotask(() => { this.expiryQueued = false; this.syncExpiry(); });
  }
  syncExpiry() {
    const cutoff = expiryCutoffMs(Date.now(), retention.seconds);
    const articles = [...this.list.children].filter(item => item.matches?.('article.message[data-message-id]'));
    const plan = planExpiry(articles.map(item => { const message = this.messages.get(item.dataset.messageId); return { id: item.dataset.messageId, createdMs: Date.parse(message?.created_at), expirable: message?.expirable === true }; }), cutoff);
    for (const article of articles) {
      const expired = plan.expired.has(article.dataset.messageId);
      if (article.classList.contains('message-expired') === expired) continue;
      article.classList.toggle('message-expired', expired);
      article.querySelector('.message-expired-mark')?.remove();
      if (expired) {
        const mark = document.createElement('span'); mark.className = 'message-expired-mark'; mark.innerHTML = EXPIRED_HOURGLASS; mark.title = 'Older than the expiry window: AI room chatter this old is not delivered to agents. Still shown here.'; mark.setAttribute('role', 'img'); mark.setAttribute('aria-label', 'Expired from AI context');
        const holder = article.querySelector('.message-meta, .system-row'); const time = holder?.querySelector('time'); if (time) time.before(mark); else holder?.append(mark);
      }
    }
    // The boundary label is a real element drawn out of flow in the gap above the first newer message, so adding or
    // removing it never moves what the reader is looking at.
    const target = plan.divider ? this.nodes.get(plan.divider) : null;
    for (const marked of this.list.querySelectorAll(':scope > .expiry-boundary')) if (marked !== target) { marked.classList.remove('expiry-boundary'); marked.querySelector(':scope > .expiry-label')?.remove(); }
    if (target && this.list.contains(target) && !target.classList.contains('expiry-boundary')) {
      target.classList.add('expiry-boundary');
      const label = document.createElement('span'); label.className = 'expiry-label'; label.textContent = "Older AI chatter won't reach agents automatically";
      target.prepend(label);
    }
  }
  updateThreadCount(id) {
    const count = threadCount(this.threadCounts, this.serverThreadCounts, id), label = this.threadLabel(id);
    const article = this.nodes.get(id);
    for (const button of article?.querySelectorAll('.thread-action') || []) { button.title = label; button.setAttribute('aria-label', label); }
    const chip = article?.querySelector(':scope > .thread-chip'); if (chip) { const view = threadChip(count); chip.hidden = view.hidden; chip.textContent = view.text; chip.title = 'Open thread'; chip.setAttribute('aria-label', view.label); }
    if (this.threadRoot?.id === id && this.threadTitle) this.threadTitle.textContent = threadTitle(count);
  }
  // The header line of a message, in the main list and in the thread panel alike: sender, (bell), #number, bug, time.
  messageMeta(message, article) {
    const meta = document.createElement('div'); meta.className = 'message-meta';
    meta.append(this.authorMeta(message));
    const date = stampTime(document.createElement('time'), message.created_at);
    const number = document.createElement('button'); number.type = 'button'; number.className = 'row-btn quiet-glyph message-number'; number.textContent = `#${message.no}`; number.title = 'Click to copy';
    const attention = message.needs_attention ? document.createElement('span') : null;
    if (attention) { attention.className = 'message-attention'; attention.textContent = '🔔'; attention.title = 'Needs human attention'; attention.setAttribute('aria-label', 'Needs human attention'); article.classList.add('needs-attention'); }
    meta.append(...(attention ? [attention] : []), number, ...[this.usageButton(message)].filter(Boolean), this.debugButton(message), date);
    return meta;
  }
  threadEntry(message) {
    this.delegateRows(this.threadList);
    const entry = document.createElement('article'); entry.className = 'thread-entry'; entry.dataset.messageId = message.id; entry.dataset.seq = String(message.seq); entry.dataset.no = String(message.no);
    const body = document.createElement('div'); body.className = 'message-body'; this.renderBody(body, message);
    entry.append(this.messageMeta(message, entry), body);
    const visualPlan = renderVisualPlan(message); if (visualPlan) entry.append(visualPlan);
    this.syncAnswerControl(entry, this.answerItems?.get(message.id), message.id);
    return entry;
  }
  insertThreadEntry(message) {
    if (this.threadEntryIDs.has(message.id)) return;
    this.threadEntryIDs.add(message.id);
    const entries = this.threadList.children;
    let low = 1, high = entries.length;
    while (low < high) {
      const middle = (low + high) >> 1;
      if (Number(entries[middle].dataset.seq) < message.seq) low = middle + 1;
      else high = middle;
    }
    this.threadList.insertBefore(this.threadEntry(message), entries[low] || null);
  }
  markNewThreadReply(message) {
    if (!this.threadNewReplyIDs.has(message.id)) {
      this.threadNewReplyIDs.add(message.id);
    }
    this.messages.set(message.id, message);
    if (Number.isInteger(message.thread_count)) this.serverThreadCounts.set(message.thread_root, Math.max(this.serverThreadCounts.get(message.thread_root) || 0, message.thread_count));
    this.updateThreadCount(message.thread_root);
    this.updateThreadNewMarker();
  }
  updateThreadNewMarker() {
    const count = this.threadNewReplyIDs.size;
    setLatest(this.threadNewButton, newRepliesLabel(count), count);
    this.threadNewButton.hidden = count === 0;
  }
  async openThread(root, focus = null) {
    const roomID = this.room?.id, session = this.sessionGeneration, generation = this.openGeneration;
    const thread = this.threadGeneration = (this.threadGeneration || 0) + 1;
    if (this.threadRoot?.id !== root.id) this.releaseThread();
    this.threadRoot = root; this.threadTitle.textContent = 'Thread'; this.threadPanel.hidden = false; this.threadList.replaceChildren(this.threadEntry(root)); this.threadEntryIDs = new Set([root.id]);
    this.threadOldestSeq = 0; this.threadHasMoreOlder = false; this.loadingOlderThread = false; this.threadOlderStatus.hidden = true; this.threadList.dataset.hasOlder = 'false'; this.threadLatestButton.hidden = !focus; this.threadNewReplyIDs = new Set(); this.threadNewButton.hidden = true;
    document.querySelector('#thread-error').textContent = '';
    if (Number.isInteger(root.thread_count)) this.serverThreadCounts.set(root.id, Math.max(this.serverThreadCounts.get(root.id) || 0, root.thread_count));
    this.updateThreadCount(root.id);
    this.threadDraft.focus();
    try {
      const context = focus ? `&around=${encodeURIComponent(focus.id)}` : '';
      const page = await request(`/api/rooms/${roomID}/messages?thread_root=${encodeURIComponent(root.id)}${context}&limit=${this.threadPageSize}`);
      if (!this.current(session, roomID) || generation !== this.openGeneration || thread !== this.threadGeneration || this.threadRoot?.id !== root.id) return;
      for (const message of page) { this.threadNewReplyIDs.delete(message.id); this.add(message); this.insertThreadEntry(message); }
      this.updateThreadNewMarker();
      this.threadOldestSeq = page[0]?.seq || 0;
      this.threadHasMoreOlder = page.length === this.threadPageSize;
      this.threadList.dataset.hasOlder = String(this.threadHasMoreOlder);
      if (focus) this.threadList.querySelector(`[data-message-id="${CSS.escape(focus.id)}"]`)?.scrollIntoView({ block: 'center' });
      else this.threadList.scrollTop = this.threadList.scrollHeight;
      const lastSeq = focus ? page.at(-1)?.seq : null;
      if (lastSeq) {
        const newer = await request(`/api/rooms/${roomID}/messages?thread_root=${encodeURIComponent(root.id)}&after=${lastSeq}&limit=1`);
        if (!this.current(session, roomID) || generation !== this.openGeneration || thread !== this.threadGeneration || this.threadRoot?.id !== root.id) return;
        if (!newer.length) {
          this.threadLatestButton.hidden = true;
          for (const id of this.threadNewReplyIDs) {
            const message = this.messages.get(id);
            if (message) this.insertThreadEntry(message);
          }
          this.threadNewReplyIDs.clear();
          this.updateThreadNewMarker();
        }
      }
    } catch (error) {
      if (this.current(session, roomID) && thread === this.threadGeneration) document.querySelector('#thread-error').textContent = error.message;
    }
  }
  async loadOlderThread() {
    if (!this.room || !this.threadRoot || !this.threadHasMoreOlder || !this.threadOldestSeq || this.loadingOlderThread) return;
    const roomID = this.room.id, rootID = this.threadRoot.id, session = this.sessionGeneration, generation = this.openGeneration, thread = this.threadGeneration;
    this.loadingOlderThread = true; this.threadOlderStatus.hidden = false;
    try {
      const page = await request(`/api/rooms/${roomID}/messages?thread_root=${encodeURIComponent(rootID)}&before=${this.threadOldestSeq}&limit=${this.threadOlderPageSize}`);
      if (!this.current(session, roomID) || generation !== this.openGeneration || thread !== this.threadGeneration || this.threadRoot?.id !== rootID) return;
      // Measured after the page arrives, as in loadOlder: only the inserted height is compensated, not the reader's own scrolling.
      const anchor = this.threadList.children[1], anchorTop = anchor?.getBoundingClientRect().top;
      for (const message of page) { this.add(message); this.insertThreadEntry(message); }
      if (page.length) this.threadOldestSeq = page[0].seq;
      this.threadHasMoreOlder = page.length === this.threadOlderPageSize;
      this.threadList.dataset.hasOlder = String(this.threadHasMoreOlder);
      if (anchor?.isConnected) this.threadList.scrollTop += anchor.getBoundingClientRect().top - anchorTop;
    } catch (error) {
      if (this.current(session, roomID) && thread === this.threadGeneration) document.querySelector('#thread-error').textContent = error.message;
    } finally {
      if (thread === this.threadGeneration) { this.loadingOlderThread = false; this.threadOlderStatus.hidden = true; }
    }
  }
  // Replies kept for an open thread whose root row is not rendered (trimmed from a window, or never loaded) have no other
  // owner: when the panel lets go of that thread they are dropped with its counts, and come back with the row's page.
  releaseThread() {
    const id = this.threadRoot?.id;
    if (!id || this.nodes.has(id)) return;
    for (const [key, message] of this.messages) if (message.thread_root === id) this.messages.delete(key);
    this.threadCounts.delete(id); this.serverThreadCounts.delete(id);
  }
  closeThread() { this.threadNotice.clear(); this.releaseThread(); this.threadGeneration = (this.threadGeneration || 0) + 1; this.threadRoot = null; this.threadTitle.textContent = 'Thread'; this.threadEntryIDs = new Set(); this.threadNewReplyIDs = new Set(); this.threadOldestSeq = 0; this.threadHasMoreOlder = false; this.loadingOlderThread = false; this.threadOlderStatus.hidden = true; this.threadList.dataset.hasOlder = 'false'; this.threadLatestButton.hidden = true; this.threadNewButton.hidden = true; this.threadPanel.hidden = true; this.threadDraft.value = ''; this.threadList.replaceChildren(); document.querySelector('#thread-error').textContent = ''; }
  async sendThread() {
    if (!this.room || !this.threadRoot || !this.threadDraft.value.trim()) return;
    const room = this.room.id, root = this.threadRoot.id, body = this.threadDraft.value, session = this.sessionGeneration;
    const errorNode = document.querySelector('#thread-error'); errorNode.textContent = ''; this.threadNotice.clear();
    try {
      const message = await request(`/api/rooms/${room}/messages`, { method: 'POST', body: { body, thread_root: root, reply_to: root } });
      if (this.current(session, room) && this.threadRoot?.id === root) {
        this.threadDraft.value = '';
        if (!this.threadLatestButton.hidden) this.markNewThreadReply(message);
        else { this.add(message); this.threadList.scrollTop = this.threadList.scrollHeight; }
      }
    } catch (error) { if (this.current(session, room)) this.threadNotice.show(error.message); }
  }
  setReply(message) { this.reply = message; this.chip.hidden = !message; this.chipText.textContent = message ? `Replying to ${this.authorName(message)}: ${messageSnippet(message)}` : ''; if (message) this.draft.focus(); }
  // Whether a reply to `parent` with this text is the answer to `item` (see send): a question or approval this person decides, answered with text
  // that mentions at most the requester. A free-text ask takes any text; an options-only ask only the text of one of its options (so Shift-clicking
  // an answer and sending it unedited acts like the button, while edited text is an ordinary reply).
  answersThroughInbox(item, parent, body) { return answersThrough(item, this.answerOptions(parent).mention, body); }
  async send() {
    if (!this.room || this.pendingUploads || (!this.draft.value.trim() && !this.pendingAttachments.length)) return;
    const room = this.room.id, body = this.draft.value, reply = this.reply, attachments = this.pendingAttachments.map(a => a.id), session = this.sessionGeneration;
    this.error.textContent = ''; this.notice.clear();
    this.announcer.sendStarted();
    let sentID = null;
    try {
      // A reply to a message with a free-text question or approval open for this person is the answer to it: it goes through the inbox, which
      // resolves the item, so the item closes and the Inbox count drops, whether the text came from Shift-clicking an answer or was typed. It
      // stays an ordinary reply when it carries attachments (an answer is text), when the item is not one this person decides, when the ask is
      // options only and the text is not one of the options (arbitrary text does not answer it), or when the text mentions anyone but the requester (so
      // an @reviewer wake still works).
      const open = reply && !attachments.length ? this.answerItems?.get(reply.id) : null;
      if (open && this.answersThroughInbox(open, reply, body)) {
        if (this.answering) return;
        this.answering = true;
        try {
          // Only a confirmed answer clears the draft; a card that is already pending answers undefined, and a refusal answers false.
          if (await this.inbox.answer(open, body.trim()) !== true) throw new Error('Could not send the answer');
        } finally { this.answering = false; }
        if (this.current(session, room)) { this.draft.value = ''; if (this.recall) this.recall.index = null; this.resizeDraft(); this.setReply(null); }
        return;
      }
      const message = await request(`/api/rooms/${room}/messages`, { method: 'POST', body: { body, attachments, ...(reply ? { reply_to: reply.id } : {}) } });
      sentID = message.id;
      if (this.current(session, room)) {
        this.draft.value = ''; if (this.recall) this.recall.index = null; this.resizeDraft(); this.pendingAttachments = []; this.renderPending(); this.setReply(null);
        // A history window does not show the live end, so appending there would put the message among old ones: show the latest instead.
        if (this.contextMode) await this.reopenLatest(); else { this.add(message); this.trimLive(); }
      }
    } catch (error) { if (this.current(session, room)) this.notice.show(error.message); }
    finally { this.announcer.sendFinished(sentID); }
  }
  // Reopening the room at the latest closes the thread panel; an open thread and its unsent reply survive it.
  async reopenLatest() {
    const root = this.threadRoot, draft = this.threadDraft.value, room = this.room, session = this.sessionGeneration;
    await this.open(room);
    if (!root || !this.current(session, room.id) || this.threadRoot) return;
    const thread = this.openThread(root);
    this.threadDraft.value = draft; this.draft.focus();
    await thread;
  }
  refreshReactions(id) { return this.store.refreshReactions(id); }
  node(id) { return this.nodes.get(id); }
  showReactions(id, entries) {
    this.reactions.set(id, entries);
    const article = this.nodes.get(id); if (!article) return;
    const pills = article.querySelector('.reaction-pills'); pills.replaceChildren();
    for (const group of groupReactions(entries, currentUser)) {
      const pill = document.createElement('button'); pill.type = 'button'; pill.className = 'row-btn reaction-pill'; pill.textContent = `${group.emoji} ${group.count}`;
      pill.title = group.title; pill.setAttribute('aria-pressed', group.pressed); pill.dataset.emoji = group.emoji; pills.append(pill);
    }
  }
  toggleReaction(id, emoji) { return this.store.toggleReaction(id, emoji); }
}

for (const name of ['messages', 'reactions', 'threadCounts', 'serverThreadCounts', 'repliedTo', 'answeredMessages', 'historicalAuthors', 'missedIDs', 'announcedIDs', 'newestSeq', 'oldestSeq', 'hasMoreOlder', 'loadingOlder', 'loadingNewer', 'contextMode', 'catchingUp', 'windowSeq', 'initialLoading', 'openGeneration', 'sessionGeneration', 'jumpGeneration', 'pageSize', 'olderPageSize']) {
  Object.defineProperty(Chat.prototype, name, { configurable: true, get() { return this.store[name]; }, set(value) { this.store[name] = value; } });
}
