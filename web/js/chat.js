import { RoomNotifySettings } from './room-notify-settings.js';
import { request, upload, download, currentUser, currentLogin, rights } from './api.js';
import { PREFERENCE_KEYS, savePreference } from './preferences.js';
import { retention } from './retention.js';
import { expiryCutoffMs, planExpiry } from './expiry.js';
import { EXPIRED_HOURGLASS } from './usage-expiry.js';
import { usageTooltip } from './message-usage-format.js';
import { toggleMessageUsage, closeMessageUsage } from './message-usage.js';
import { UsageSummary } from './usage-summary.js';
import { renderMarkdown, renderRaw, copyText } from './markdown.js';
import { setGlyph, iconGlyph } from './icon-button.js';
import { stampTime, refreshRelativeTimes } from './time.js';
import { ScrollAnchor } from './scroll-anchor.js';
import { senderIdentity, senderKind, createSenderPresence, applySenderIdentity, agentLabel } from './sender-identity.js';
import { openSenderProfile } from './sender-profile.js';
import { renderVisualPlan } from './visual-plan.js';
import { ScopeControl } from './scope-control.js';
import { Schedules } from './schedules.js';
import { RoomMembers } from './room-members.js';
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
import { confirmDialog, promptDialog } from './dialogs.js';
import { expandShortcodes } from './emoji-data.js';

const MENTION_LOGINS_TTL_MS = 30000;

// One line of a message for a quote or a reply chip: whitespace collapsed, cut to 96 characters, "[attachment]" for a message that is only files.
// The words of a message without its Markdown syntax (bold, code, links, quotes and headings), for a one-line quote of it.
export const plainText = text => String(text || '')
  .replace(/^\s{0,3}(?:#{1,6}|>)\s?/gm, '')
  .replace(/!?\[([^\]]*)\]\([^)]*\)/g, '$1')
  .replace(/(\*\*|__|~~)(?=\S)(.+?)(?<=\S)\1/g, '$2')
  .replace(/(^|[\s(])([*_])(?=\S)([^*_\n]+?)(?<=\S)\2(?=$|[\s).,;:!?])/g, '$1$3')
  .replace(/`+([^`]*)`+/g, '$1');

export function messageSnippet(message) {
  let text = plainText(message?.body).replace(/\s+/g, ' ').trim();
  if (!text && message?.attachments?.length) text = '[attachment]';
  return text.length > 96 ? `${text.slice(0, 95).trimEnd()}…` : text;
}

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
    this.list = document.querySelector('#messages'); this.title = document.querySelector('#room-title'); this.roomIcon = document.querySelector('#room-icon'); this.members = document.querySelector('#room-members'); this.form = document.querySelector('#composer'); this.draft = this.form.elements.body; this.formatting = new ComposerFormatting(this.draft, { onFiles: files => this.addFiles(files) });
    this.pageSize = 100;
    this.olderPageSize = 50;
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
    this.chip = document.querySelector('#reply-chip'); this.chipText = this.chip.querySelector('#reply-chip-jump'); this.chipText.onclick = () => { if (this.reply) this.jumpToMessage(this.reply.id); }; this.error = document.querySelector('#chat-error'); this.nodes = new Map(); this.messages = new Map(); this.reactions = new Map(); this.threadCounts = new Map(); this.serverThreadCounts = new Map();
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
    this.openGeneration = 0;
    this.sessionGeneration = 0;
    this.uploadRequests = new Set();
    this.raw = false;
    this.infoDialog = document.querySelector('#room-info-dialog');
    this.worktreeSection = document.createElement('section'); this.worktreeSection.className = 'room-worktree';
    this.worktreeSection.innerHTML = '<h3>Git worktree</h3><p role="status"></p><button type="button" class="btn btn-primary">Create worktree</button><button type="button" class="btn btn-danger" hidden>Remove worktree</button>';
    document.querySelector('#room-info-error').before(this.worktreeSection);
    // Task rooms keep task notes; the home room has none of its own and points at the project's memory instead.
    this.notesSection = document.createElement('section'); this.notesSection.className = 'room-notes'; this.notesSection.setAttribute('aria-labelledby', 'room-notes-title');
    this.notesSection.hidden = true;
    this.notesSection.innerHTML = '<h3 id="room-notes-title">Task notes</h3><p class="settings-intro"></p><button type="button" class="btn">Open task notes</button>';
    this.notesSection.querySelector('button').onclick = () => this.openRoomNotes();
    document.querySelector('#room-info-error').before(this.notesSection);
    this.projectSection = document.querySelector('#room-project');
    // The title of the room, edited here and nowhere else: the home room's is the project's name.
    this.renameForm = document.querySelector('#room-rename-form');
    setGlyph(this.renameForm.querySelector('#room-rename-save'), 'check', 'Save title');
    this.renameForm.onsubmit = event => { event.preventDefault(); this.saveRoomTitle(); };
    this.renameForm.elements.title.oninput = () => { this.renameForm.querySelector('#room-rename-status').textContent = ''; };
    this.notifySettings = new RoomNotifySettings(document.querySelector('#room-member-list'), (field, value) => this.setRoomPreference?.(this.roomView?.(this.room?.id) || this.room, field, value), () => this.roomView?.(this.room?.id) || this.room);
    this.memorySection = document.createElement('section');
    this.memorySection.className = 'room-memory-setting';
    this.memorySection.innerHTML = '<h3>Room memory</h3><label>Message memory <select><option value="inherit">Use project setting</option><option value="off">Off in this room</option></select></label><p role="status"></p><p>Turning this off prevents this room’s messages from feeding memory or recall. It does not erase saved memory.</p>';
    document.querySelector('#room-info-error').before(this.memorySection);
    this.memorySection.querySelector('select').onchange = () => this.changeRoomMemory();
    this.worktreeSection.querySelector('button:first-of-type').onclick = () => this.createWorktree();
    this.worktreeSection.querySelector('button:last-of-type').onclick = () => this.removeWorktree();
    this.scopeControl = new ScopeControl(document.querySelector('#room-scope-control'), 'room');
    this.schedules = new Schedules(document.querySelector('#room-schedules'));
    this.memberList = new RoomMembers(document.querySelector('#room-member-list'));
    this.exportDialog = document.querySelector('#room-export-dialog');
    this.roomUsage = new UsageSummary(document.querySelector('#room-usage'), id => `/api/rooms/${id}/usage`);
    document.querySelector('#room-info').onclick = () => this.showRoomInfo();
    document.querySelector('#room-info-close').onclick = () => this.infoDialog.close();
    this.infoDialog.addEventListener('close', () => this.projectSettings?.release(this.projectSection));
    document.querySelector('#room-lifecycle').onclick = () => this.changeRoomLifecycle();
    document.querySelector('#room-remove').onclick = () => this.removeRoom();
    document.querySelector('#room-jump').onclick = () => this.promptJump();
    document.querySelector('#room-raw').onclick = () => this.toggleRaw();
    document.querySelector('#room-export').onclick = () => this.exportDialog.showModal();
    document.querySelector('#room-export-close').onclick = () => this.exportDialog.close();
    for (const button of this.exportDialog.querySelectorAll('[data-format]')) button.onclick = () => this.exportRoom(button.dataset.format);
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
  async open(room) {
    const generation = ++this.openGeneration;
    const session = this.sessionGeneration;
    this.announcer.reset();
    if (this.room?.id !== room.id) { this.pendingAttachments = []; this.renderPending(); if (this.recall) this.recall.index = null; }
    closeMessageUsage(); this.room = room; this.syncEmpty(); this.title.textContent = room.title; this.setRoomFacts(room); this.error.textContent = ''; this.notice.clear(); this.threadNotice.clear(); this.answeredMessages = new Set(); this.repliedTo = new Set(); this.nodes.clear(); this.messages.clear(); this.historicalAuthors = new Set(); this.reactions.clear(); this.threadCounts.clear(); this.serverThreadCounts.clear(); this.list.replaceChildren(); this.olderStatus.hidden = true; this.list.dataset.hasOlder = 'false'; this.latestButton.hidden = true; this.newestSeq = 0; this.oldestSeq = 0; this.hasMoreOlder = false; this.loadingOlder = false; this.initialLoading = true; this.contextMode = false; this.catchingUp = false; this.windowSeq = 0; this.loadingNewer = false; this.missedIDs = new Set(); this.updateLatestLabel(); this.setReply(null); this.closeThread();
    for (const button of document.querySelectorAll('.room-controls button')) button.disabled = false;
    this.setRoomFacts(room);
    this.scrollAnchor.reset();
    delete this.list.dataset.empty; // no hint while the room is still loading
    const messages = await request(`/api/rooms/${room.id}/messages?limit=${this.pageSize}`);
    if (session !== this.sessionGeneration || generation !== this.openGeneration || this.room?.id !== room.id) return;
    for (const message of messages) this.add(message);
    this.list.dataset.empty = String(!this.list.querySelector('article.message:not(.system-notice)')); // notices alone leave it empty
    this.newestSeq = messages.at(-1)?.seq || 0;
    this.oldestSeq = messages[0]?.seq || 0;
    this.hasMoreOlder = messages.length === this.pageSize;
    this.list.dataset.hasOlder = String(this.hasMoreOlder);
    this.list.scrollTop = this.list.scrollHeight; this.scrollAnchor?.update();
    this.initialLoading = false; this.loadOlderIfShort();
    // The room is marked read once its latest page is on screen, up to that page (see newestShownSeq).
    this.onLatest?.(room);
    await this.refreshMessages();
  }
  // How far the read mark may go (0 while a room is still loading). At the latest view the list is contiguous to the live
  // cursor: every message up to newestSeq is applied (top-level rows rendered, thread replies counted on their roots).
  // Otherwise (a history window, or its catch-up in flight) only the newest top-level row actually in the list counts,
  // never a thread reply, which an open thread panel can hold far past the window's end.
  newestShownSeq() {
    if (this.initialLoading) return 0;
    if (!this.contextMode && !this.catchingUp) return this.newestSeq || 0;
    for (let node = this.list.lastElementChild; node; node = node.previousElementSibling) {
      if (node.matches('article.message[data-message-id]')) return Number(node.dataset.seq) || 0;
    }
    return 0;
  }
  // Live top-level messages that arrived while a history window was shown: they are not in the list, so the
  // Back to latest button counts them.
  updateLatestLabel() {
    const count = this.missedIDs?.size || 0;
    setLatest(this.latestButton, count ? `Back to latest · ${count} new` : 'Back to latest', count);
  }
  reset() {
    this.openGeneration++; this.sessionGeneration++; if (this.recall) this.recall.index = null; this.announcer.reset();
    for (const controller of this.uploadRequests) controller.abort();
    this.uploadRequests.clear();
    this.room = null; this.syncEmpty(); this.title.textContent = 'Choose a room'; this.roomIcon.hidden = true; this.members.hidden = true;
    this.error.textContent = ''; this.notice.clear(); this.threadNotice.clear(); this.answeredMessages = new Set(); this.repliedTo = new Set(); this.nodes.clear(); this.messages.clear(); this.historicalAuthors = new Set(); this.reactions.clear(); this.threadCounts.clear(); this.serverThreadCounts.clear(); this.agents.clear(); this.list.replaceChildren(); this.olderStatus.hidden = true; this.list.dataset.hasOlder = 'false'; this.latestButton.hidden = true; this.newestSeq = 0; this.oldestSeq = 0; this.hasMoreOlder = false; this.loadingOlder = false; this.initialLoading = false; this.contextMode = false; this.catchingUp = false; this.windowSeq = 0; this.loadingNewer = false; this.missedIDs = new Set(); this.updateLatestLabel(); this.answerItems = new Map(); this.closeBlocked?.clear(); this.worktree = null;
    this.setReply(null); this.closeThread(); this.pendingAttachments = []; this.pendingUploads = 0; this.pendingNode.replaceChildren(); this.draft.value = ''; this.resizeDraft();
    this.infoDialog.close(); this.exportDialog.close(); this.lightbox.close();
    this.scopeControl.reset(); this.schedules.reset(); this.memberList.reset(); this.quickCommands.reset(); this.macroAutocomplete.reset(); this.mentionAutocomplete.reset(); this.threadMentionAutocomplete.reset(); this.emojiAutocomplete.reset(); this.threadEmojiAutocomplete.reset(); closeEmojiPicker(false); this.mentionIndex = null; this.mentionLogins = []; this.mentionLoginsRoom = ''; this.mentionLoginsAt = 0; this.mentionLoginsSeq = (this.mentionLoginsSeq || 0) + 1;
    this.worktreeSection.querySelector('p').textContent = ''; this.memorySection.querySelector('p').textContent = '';
    for (const button of document.querySelectorAll('.room-controls button')) button.disabled = true;
    this.draft.disabled = true; this.emojiButton.disabled = true; this.attachmentInput.disabled = true; this.form.querySelector('button[type=submit]').disabled = true;
  }
  // With no room open the chat shows an empty-state card instead of the composer and room controls; the app decides what the card says.
  syncEmpty() { this.onEmptyChange?.(!this.room); }
  current(session, roomID) { return session === this.sessionGeneration && this.room?.id === roomID; }
  // A list too short to scroll cannot ask for more by scrolling, so a short page with more behind it asks right away.
  loadOlderIfShort() {
    requestAnimationFrame(() => { if (this.hasMoreOlder && this.list.scrollHeight <= this.list.clientHeight + 80) this.loadOlder(); });
  }
  async loadOlder() {
    if (!this.room || !this.hasMoreOlder || !this.oldestSeq || this.loadingOlder) return;
    const roomID = this.room.id, session = this.sessionGeneration, generation = this.openGeneration;
    this.loadingOlder = true; this.olderStatus.hidden = false;
    try {
      const messages = await request(`/api/rooms/${roomID}/messages?before=${this.oldestSeq}&limit=${this.olderPageSize}`);
      if (!this.current(session, roomID) || generation !== this.openGeneration) return;
      // Measure the anchor now, not before the request: the reader may have scrolled while the page was loading, and only the
      // height inserted above the anchor may be compensated, never the reader's own movement.
      const anchor = this.list.firstElementChild, anchorTop = anchor?.getBoundingClientRect().top;
      for (let i = messages.length - 1; i >= 0; i--) this.add(messages[i], true);
      if (messages.length) this.oldestSeq = messages[0].seq;
      this.hasMoreOlder = messages.length === this.olderPageSize;
      this.list.dataset.hasOlder = String(this.hasMoreOlder);
      if (anchor?.isConnected) this.list.scrollTop += anchor.getBoundingClientRect().top - anchorTop;
      this.trimNewest();
    } catch (error) { if (this.current(session, roomID)) this.error.textContent = error.message; }
    finally { if (generation === this.openGeneration) { this.loadingOlder = false; this.olderStatus.hidden = true; this.loadOlderIfShort(); } }
  }
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
    if (this.trimOldest(atBottom ? this.liveCap() : this.renderCap())) this.scrollAnchor?.update();
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
    if (rows.length <= cap || this.loadingNewer) return;
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
    if (rows.length <= cap) return false;
    if (rows[rows.length - cap - 1].getBoundingClientRect().bottom > this.list.getBoundingClientRect().top) return false;
    const keep = rows[rows.length - cap], keepTop = keep.getBoundingClientRect().top;
    this.dropRows(rows.slice(0, rows.length - cap));
    this.list.scrollTop += keep.getBoundingClientRect().top - keepTop;
    this.oldestSeq = Number(keep.dataset.seq); this.hasMoreOlder = true; this.list.dataset.hasOlder = 'true';
    return true;
  }
  async loadNewer() {
    if (!this.room || !this.contextMode || !this.windowSeq || this.loadingNewer || this.loadingOlder) return;
    const roomID = this.room.id, session = this.sessionGeneration, generation = this.openGeneration;
    this.loadingNewer = true;
    try {
      const messages = await request(`/api/rooms/${roomID}/messages?after=${this.windowSeq}&limit=${this.olderPageSize}`);
      if (!this.current(session, roomID) || generation !== this.openGeneration || !this.contextMode) return;
      for (const message of messages) this.add(message, false, true);
      if (messages.length) this.windowSeq = messages.at(-1).seq;
      // A short page is the live end of the room: the window is the latest view again.
      if (messages.length < this.olderPageSize) { await this.leaveWindow(); return; }
      this.trimOldest();
      this.scrollAnchor?.update();
    } catch (error) { if (this.current(session, roomID)) this.error.textContent = error.message; }
    finally { if (generation === this.openGeneration) this.loadingNewer = false; }
  }
  // The live cursor restarts at the window's end, not at the newest seq seen: a live refresh that ran while the last page
  // was in flight counted arrivals newer than that page without rendering them, and this refresh is what renders them.
  // Those were announced when counted and are not announced again. The room is marked read only once they are shown, and
  // only up to what is shown. If that refresh fails the view stays a window ending at what it did render, with the
  // still-unseen arrivals counted, so scrolling down again or Back to latest completes it.
  async leaveWindow() {
    const room = this.room, session = this.sessionGeneration, generation = this.openGeneration, missed = this.missedIDs;
    this.newestSeq = this.windowSeq;
    this.announcedIDs = missed;
    // Not the latest view for the read mark until the catch-up has rendered (see holdRead in app.js).
    this.catchingUp = true;
    this.contextMode = false; this.windowSeq = 0; this.latestButton.hidden = true;
    this.missedIDs = new Set(); this.updateLatestLabel();
    this.scrollAnchor?.update();
    let caughtUp = true;
    try { await this.refreshMessages(); } catch (error) { caughtUp = false; if (this.current(session, room.id)) this.error.textContent = error.message; }
    finally { this.announcedIDs = null; }
    if (!this.current(session, room.id) || generation !== this.openGeneration || this.contextMode) return;
    if (!caughtUp) {
      this.contextMode = true; this.catchingUp = false; this.windowSeq = this.newestSeq; this.latestButton.hidden = false;
      for (const id of missed) if (!this.messages.has(id)) this.missedIDs.add(id);
      this.updateLatestLabel(); return;
    }
    this.catchingUp = false;
    this.onLatest?.(room);
  }
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
    const count = Math.max(this.threadCounts.get(id) || 0, this.serverThreadCounts?.get(id) || 0);
    return count ? `Thread · ${count}` : 'Reply in thread';
  }
  setInbox(inbox) { this.inbox = inbox; }
  // `messageID` is the message the container belongs to (an item is not there once it has been answered). A question this viewer
  // had open that is now gone from the inbox leaves a quiet, disabled "Answered" in its place for the rest of the session, as v1
  // did, instead of just losing its buttons.
  syncAnswerControl(container, item, messageID) {
    if (!container) return;
    const current = container.querySelector(':scope > .answer-controls');
    const id = messageID ?? item?.message_id;
    const eligible = item && ['question', 'approval'].includes(item.kind) && !item.boundary_pending && item.can_decide !== false;
    const marker = () => container.querySelector(':scope > .answer-done');
    const history = () => container.querySelector(':scope > .answer-history');
    if (!eligible) {
      if (current && !item && id) (this.answeredMessages ||= new Set()).add(id);
      current?.remove();
      // A question or proposed answers on a message with nothing open for it (history, an imported ask, an answered one) stay
      // visible under it, disabled: "Answered" when it was answered, otherwise marked as no longer pending.
      const message = id ? this.messages?.get(id) || (this.threadRoot?.id === id ? this.threadRoot : null) : null;
      const answered = Boolean(id && (this.answeredMessages?.has(id) || this.repliedTo?.has(id)));
      if (message && (message.proposed_answers?.length || message.open_questions?.length)) {
        marker()?.remove();
        const shown = history();
        if (shown?.dataset.answered === String(answered)) return;
        shown?.remove();
        container.append(answerHistory(message, answered));
        return;
      }
      history()?.remove();
      if (answered && !marker()) {
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
    this.title.textContent = room.title; // a rename by someone else reaches here through the room list
    this.refreshMentions();
    this.loadMentionLogins(room);
    const count = room.member_count || 0;
    const archivedProject = !!this.projectArchived?.(room.project_id);
    const status = archivedProject ? 'Project archived' : room.lifecycle === 'closing' ? 'Closing…' : room.lifecycle === 'closed' ? 'Archived' : '';
    this.roomIcon.textContent = room.kind === 'home' ? '⌂' : room.agent_chat ? '⇄' : room.kind === 'dm' ? '@' : '#';
    this.roomIcon.classList.toggle('dm', room.kind === 'dm'); this.roomIcon.hidden = false;
    this.members.querySelector('.room-members-count').textContent = String(count);
    this.members.querySelector('.room-header-status')?.remove();
    if (status) {
      const statusNode = document.createElement('span'); statusNode.className = 'room-header-status'; statusNode.textContent = status;
      this.members.append(statusNode);
    }
    const membersLabel = `${count} ${count === 1 ? 'member' : 'members'}${status ? ` · ${status}` : ''}`;
    this.members.title = membersLabel; this.members.setAttribute('aria-label', membersLabel); this.members.hidden = false;
    const blocked = this.closeBlocked?.get(room.id);
    if (blocked && room.lifecycle === 'closing') this.error.textContent = `Room closing blocked: ${blocked}`;
    else if (this.error.textContent.startsWith('Room closing blocked:')) this.error.textContent = '';
    if (room.lifecycle !== 'closing') this.closeBlocked?.delete(room.id);
    if (this.infoDialog.open) this.renderRoomActions();
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
    archivedNote.textContent = 'This project is archived and read-only. The owner or an admin can reopen it in Settings → Project.';
  }
  showRoomInfo() {
    if (!this.room) return;
    this.roomUsage.load(this.room.id);
    const controls = document.querySelector('#room-scope-control');
    controls.hidden = this.room.kind === 'dm';
    const facts = document.querySelector('#room-info-facts'); facts.replaceChildren();
    // Kind and Room ID are for debugging: they show only while the message debug inspector is on (Settings → General).
    // Whoever may rename the room edits its title in the form above the facts instead of reading it among them.
    const canRename = this.canRenameRoom(this.room);
    this.renameForm.hidden = !canRename;
    if (canRename) { this.renameForm.elements.title.value = this.room.title; this.renameForm.querySelector('#room-rename-status').textContent = ''; }
    for (const [label, value, debug] of [...(canRename ? [] : [['Name', this.room.title]]), ['Kind', this.room.kind, true], ['Status', this.room.lifecycle || 'open'], ['Members', String(this.room.member_count || 0)], ['Room ID', this.room.id, true]]) {
      const term = document.createElement('dt'); term.textContent = label;
      const detail = document.createElement('dd'); detail.textContent = value;
      if (debug) { term.className = detail.className = 'room-fact-debug'; }
      facts.append(term, detail);
    }
    this.renderRoomActions();
    document.querySelector('#room-info-error').textContent = '';
    this.renderViewActions();
    this.notifySettings.render();
    const home = this.room.kind === 'home';
    this.infoDialog.classList.toggle('room-info-home', home);
    this.renderRoomNotes();
    this.projectSection.hidden = !home || !this.projectSettings;
    this.infoDialog.showModal();
    // The home room's settings carry the project's own (Settings → Project shows the same form).
    if (!this.projectSection.hidden) this.projectSettings.mount(this.projectSection, this.room.project_id);
    this.loadWorktree();
    // What is said in a chat between agents never feeds memory or recall, so there is no setting to show.
    this.memorySection.hidden = !!this.room.agent_chat;
    if (!this.room.agent_chat) this.loadRoomMemory();
    if (!controls.hidden) this.scopeControl.load(this.room.id);
    this.schedules.load(this.room);
    this.memberList.load(this.room.id);
  }
  // The server decides; this only keeps a field out of sight that it would refuse. A task room's title is for owners and operators, the home
  // room's is the project's name (its owner or an admin), and a private chat has none of its own.
  canRenameRoom(room) {
    const role = this.projectRole?.(room.project_id);
    if (room.kind === 'dm') return false;
    return room.kind === 'home' ? role === 'owner' || rights.admin : !!role && role !== 'viewer';
  }
  async saveRoomTitle() {
    const room = this.room, form = this.renameForm, input = form.elements.title, save = form.querySelector('#room-rename-save'), status = form.querySelector('#room-rename-status');
    if (!room) return;
    const title = input.value.trim();
    status.className = 'room-rename-status'; status.textContent = '';
    if (!title) { status.className = 'room-rename-status error'; status.textContent = 'A room needs a title.'; input.focus(); return; }
    // The home room's title is the project's name, which this field may not show (a title that drifted from it), so only the server can
    // tell a home room's rename from no change.
    if (room.kind !== 'home' && title === room.title) { input.value = title; status.textContent = 'Nothing changed.'; return; }
    save.disabled = true;
    try {
      const updated = await request(`/api/rooms/${encodeURIComponent(room.id)}`, { method: 'PATCH', body: { title } });
      if (this.room?.id !== room.id) return;
      room.title = updated.title; this.title.textContent = updated.title; input.value = updated.title;
      status.textContent = 'Saved.';
    } catch (error) {
      if (this.room?.id === room.id) { status.className = 'room-rename-status error'; status.textContent = error.message; input.focus(); }
    } finally { save.disabled = false; }
  }
  // Archive/Reopen and Remove follow the room's lifecycle, also while the dialog is open (a room goes closing, then closed).
  renderRoomActions() {
    const room = this.room, action = document.querySelector('#room-lifecycle'), remove = document.querySelector('#room-remove');
    if (!room) return;
    action.hidden = room.kind === 'home';
    action.disabled = room.lifecycle === 'closing';
    setGlyph(action, room.lifecycle === 'closed' ? 'reopen' : 'archive', room.lifecycle === 'closed' ? 'Reopen room' : room.lifecycle === 'closing' ? 'Closing…' : 'Archive room');
    // Removing is for archived task rooms only: never a home room or a private chat, and not while the room is open.
    remove.hidden = room.kind === 'home' || room.kind === 'dm' || this.projectRole?.(room.project_id) !== 'owner';
    remove.disabled = room.lifecycle !== 'closed';
    // Someone who joined a chat between agents can leave it again; there is nothing else to do to it from here.
    const leave = document.querySelector('#room-leave');
    leave.hidden = !room.agent_chat;
    setGlyph(leave, 'left', 'Leave chat');
    leave.onclick = () => this.leaveChat();
    setGlyph(remove, 'trash', 'Remove room');
    remove.title = remove.disabled ? 'Archive the room before removing it' : 'Remove this room and its messages for good';
  }
  // Hide (per user, on the room lists) is set here rather than on the rows. Pinning is for projects, from the room list.
  renderViewActions() {
    const hide = document.querySelector('#room-hide');
    hide.onclick = async () => {
      const room = this.room; hide.disabled = true;
      const done = await this.setRoomPreference?.(this.roomView?.(room.id) || room, 'hidden', true);
      hide.disabled = false; if (done) this.infoDialog.close();
    };
  }
  async loadRoomMemory() {
    const room = this.room;
    if (!room) return;
    const session = this.sessionGeneration;
    const select = this.memorySection.querySelector('select');
    const status = this.memorySection.querySelector('p');
    select.value = room.memory_enabled === false ? 'off' : 'inherit';
    select.disabled = true;
    try {
      const project = await request(`/api/projects/${encodeURIComponent(room.project_id)}/memory`);
      if (!this.current(session, room.id)) return;
      status.textContent = project.enabled ? 'Project memory is on.' : 'Project memory is off.';
      select.disabled = !project.can_edit;
    } catch (error) { if (this.current(session, room.id)) status.textContent = error.message || 'Could not load memory setting'; }
  }
  async changeRoomMemory() {
    const room = this.room;
    if (!room) return;
    const session = this.sessionGeneration;
    const select = this.memorySection.querySelector('select');
    const status = this.memorySection.querySelector('p');
    select.disabled = true;
    try {
      const result = await request(`/api/rooms/${encodeURIComponent(room.id)}/memory-enabled`, { method: 'PUT', body: { enabled: select.value === 'off' ? false : null } });
      if (!this.current(session, room.id)) return;
      room.memory_enabled = result.memory_enabled;
      status.textContent = select.value === 'off' ? 'Room message memory is off.' : 'Room follows the project setting.';
      select.disabled = false;
    } catch (error) { if (this.current(session, room.id)) { select.value = room.memory_enabled === false ? 'off' : 'inherit'; status.textContent = error.message || 'Could not change room memory'; select.disabled = false; } }
  }
  renderRoomNotes() {
    const kind = this.room?.kind, home = kind === 'home';
    this.notesSection.hidden = kind !== 'task' && !home;
    this.notesSection.querySelector('h3').textContent = home ? 'Project memory' : 'Task notes';
    this.notesSection.querySelector('p').textContent = home ? 'The home room has no task notes of its own: its agents use the project\u2019s memory (project facts).' : 'Notes the agents keep for this task. They stay with this room.';
    this.notesSection.querySelector('button').textContent = home ? 'Open project memory' : 'Open task notes';
  }
  openRoomNotes() {
    const room = this.room;
    if (!room) return;
    this.infoDialog.close();
    if (room.kind === 'home') this.openProjectMemory?.(room.project_id); else this.openTaskNotes?.();
  }
  async loadWorktree() {
    const buttons = this.worktreeSection.querySelectorAll('button');
    if (this.room?.kind === 'home') {
      // The home room always works in the project folder; only task rooms get a worktree of their own.
      this.worktreeSection.hidden = false;
      for (const button of buttons) button.hidden = true;
      this.worktreeSection.querySelector('p').textContent = 'The home room works in the project folder. Task rooms can have their own Git worktree.';
      return;
    }
    if (!this.room || this.room.kind !== 'task') { this.worktreeSection.hidden = true; return; }
    const room = this.room, session = this.sessionGeneration;
    this.worktreeSection.hidden = false;
    this.worktreeSection.querySelector('p').textContent = '';
    try {
      const item = await request(`/api/rooms/${room.id}/worktree`);
      if (!this.current(session, room.id)) return;
      this.worktree = item;
      const status = this.worktreeSection.querySelector('p');
      status.textContent = item.enabled === false ? 'Using the project folder.' : `${item.state}: ${item.path || item.relative_path}${item.reason ? ` · ${item.reason}` : ''}${item.dirty_base_count ? ` · The base has ${item.dirty_base_count} uncommitted changes, which are not in the worktree.` : ''}`;
      const create = this.worktreeSection.querySelector('button:first-of-type');
      const remove = this.worktreeSection.querySelector('button:last-of-type');
      create.hidden = this.room.lifecycle !== 'open' || (item.enabled !== false && item.state !== 'failed');
      create.textContent = item.state === 'failed' ? 'Retry worktree' : 'Create worktree';
      remove.hidden = item.enabled === false || (this.room.lifecycle !== 'closed' && !!item.path);
      remove.textContent = this.room.lifecycle === 'closed' ? 'Remove worktree' : 'Cancel worktree';
    } catch (error) { if (this.current(session, room.id)) this.worktreeSection.querySelector('p').textContent = error.message; }
  }
  async createWorktree() {
    if (!this.room) return;
    const room = this.room, session = this.sessionGeneration;
    const baseRef = await promptDialog('Base branch or ref (leave blank for current HEAD)', { title: 'Create worktree', label: 'Base branch or ref', placeholder: 'Current HEAD', confirm: 'Create' });
    if (baseRef === null) return;
    try { await request(`/api/rooms/${room.id}/worktree`, { method: 'POST', body: { enabled: true, base_ref: baseRef.trim() } }); if (!this.current(session, room.id)) return; await this.loadWorktree(); }
    catch (error) { if (this.current(session, room.id)) this.worktreeSection.querySelector('p').textContent = error.message; }
  }
  async removeWorktree() {
    const room = this.room, session = this.sessionGeneration;
    if (room?.lifecycle === 'open') {
      try { await request(`/api/rooms/${room.id}/worktree`, { method: 'DELETE', body: {} }); if (!this.current(session, room.id)) return; await this.loadWorktree(); }
      catch (error) { if (this.current(session, room.id)) this.worktreeSection.querySelector('p').textContent = error.message; }
      return;
    }
    if (!room || !await confirmDialog('Remove this archived room worktree? Clean merged worktrees are removed; a dirty or unmerged worktree will be preserved.', { title: 'Remove worktree', confirm: 'Remove', danger: true })) return;
    try {
      let result = await request(`/api/rooms/${room.id}/worktree`, { method: 'DELETE', body: { force: false } });
      if (!this.current(session, room.id)) return;
      if (result.state === 'preserved' && await confirmDialog(`Worktree is preserved: ${result.reason}. Remove its checkout and discard uncommitted local files? Committed changes remain on the room branch.`, { title: 'Discard local files', confirm: 'Discard and remove', danger: true })) {
        result = await request(`/api/rooms/${room.id}/worktree`, { method: 'DELETE', body: { force: true, confirm: `REMOVE ${room.id}` } });
        if (!this.current(session, room.id)) return;
      }
      await this.loadWorktree();
    } catch (error) { if (this.current(session, room.id)) this.worktreeSection.querySelector('p').textContent = error.message; }
  }
  canArchiveRoom() {
    const room = this.room, role = room && this.projectRole?.(room.project_id);
    return !!room && room.kind === 'task' && (!room.lifecycle || room.lifecycle === 'open') && (role === 'owner' || role === 'operator');
  }
  canSpawnAgent() { return !!this.room && !document.querySelector('#spawn-open').disabled; }
  archiveRoom() {
    if (!this.canArchiveRoom()) return;
    return this.changeRoomLifecycle('archive');
  }
  async changeRoomLifecycle(intent = 'toggle') {
    const room = this.room;
    if (intent === 'archive' && !this.canArchiveRoom()) return;
    if (!room || room.kind === 'home' || room.lifecycle === 'closing') return;
    const session = this.sessionGeneration;
    const action = document.querySelector('#room-lifecycle');
    const errorNode = document.querySelector('#room-info-error');
    const closing = intent === 'archive' || room.lifecycle !== 'closed';
    if (closing && !await confirmDialog(`Archive ${room.title}? Agents homed here stop, and open questions are withdrawn. Its history remains available.`, { title: 'Archive room', confirm: 'Archive' })) return;
    action.disabled = true; errorNode.textContent = '';
    const path = `/api/rooms/${room.id}/${closing ? 'close' : 'reopen'}`;
    try {
      let updated;
      try { updated = await request(path, { method: 'POST', body: { lifecycle_rev: room.lifecycle_rev || 0 } }); }
      catch (error) {
        if (!this.current(session, room.id)) return;
        if (!closing || error.code !== 'open_children' || !await confirmDialog(`Archive ${room.title} and all its open child rooms? Agents homed in these rooms stop, and open questions are withdrawn.`, { title: 'Archive room and its child rooms', confirm: 'Archive all' })) throw error;
        updated = await request(path, { method: 'POST', body: { lifecycle_rev: room.lifecycle_rev || 0, cascade: true } });
      }
      if (!this.current(session, room.id)) return;
      this.setRoomFacts(updated);
      await this.onRoomMutation?.(updated);
      if (!this.current(session, room.id)) return;
      this.infoDialog.close();
    } catch (error) { if (this.current(session, room.id)) { this.error.textContent = error.message; if (this.infoDialog.open) errorNode.textContent = error.message; action.disabled = false; } }
  }
  async leaveChat() {
    const room = this.room;
    if (!room?.agent_chat) return;
    const session = this.sessionGeneration;
    const button = document.querySelector('#room-leave'), errorNode = document.querySelector('#room-info-error');
    button.disabled = true; errorNode.textContent = '';
    try {
      await request(`/api/rooms/${room.id}/leave`, { method: 'POST' });
      if (!this.current(session, room.id)) return;
      this.infoDialog.close();
      await this.onChatLeft?.(room);
    } catch (error) { if (this.current(session, room.id)) errorNode.textContent = error.message || 'Could not leave the chat'; }
    button.disabled = false;
  }
  async removeRoom() {
    const room = this.room;
    if (!room || room.kind === 'home' || room.kind === 'dm' || room.lifecycle !== 'closed') return;
    const session = this.sessionGeneration;
    const button = document.querySelector('#room-remove'), errorNode = document.querySelector('#room-info-error');
    if (!await confirmDialog(`Remove ${room.title} for good? Its messages, reactions, notes, attachments and its usage and cost records are deleted and cannot be restored.`, { title: 'Remove room', confirm: 'Remove for good', danger: true })) return;
    button.disabled = true; errorNode.textContent = '';
    try {
      await request(`/api/rooms/${room.id}`, { method: 'DELETE' });
      if (!this.current(session, room.id)) return;
      this.infoDialog.close();
      await this.onRoomRemoved?.(room);
    } catch (error) { if (this.current(session, room.id)) { errorNode.textContent = error.message || 'Could not remove the room'; button.disabled = false; } }
  }
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
  async jumpToMessage(key) {
    if (!this.room) return;
    if (typeof key === 'number') {
      const loaded = [...this.messages.values()].find(item => item.no === key);
      if (loaded) key = loaded.id;
      else {
        let found;
        try { found = await request(`/api/message-numbers/${encodeURIComponent(key)}`); }
        catch (error) { this.error.textContent = error.status === 404 ? `Message #${key} does not exist or is not visible to you.` : error.message; return; }
        if (!this.room) return;
        if (found.room_id !== this.room.id) { this.error.textContent = ''; this.onOpenMessage?.(found); return; }
        key = found.id;
      }
    }
    const roomID = this.room.id, session = this.sessionGeneration;
    let generation = this.openGeneration;
    const jump = this.jumpGeneration = (this.jumpGeneration || 0) + 1;
    let message = this.messages.get(key);
    try {
      if (!message) {
        const messages = await request(`/api/rooms/${roomID}/messages?around=${encodeURIComponent(key)}&limit=${this.pageSize}`);
        if (!this.current(session, roomID) || generation !== this.openGeneration || jump !== this.jumpGeneration) return;
        message = messages.find(item => item.id === key);
        if (!message) throw new Error('Message is not in this room.');
        generation = ++this.openGeneration;
        // A page in flight belongs to the old view (its generation no longer matches, so it will not clear its own flag).
        this.hasMoreOlder = false; this.loadingOlder = false; this.loadingNewer = false; this.catchingUp = false; this.olderStatus.hidden = true; this.list.dataset.hasOlder = 'false'; this.oldestSeq = 0;
        this.closeThread(); this.nodes.clear(); this.messages.clear(); this.historicalAuthors = new Set(); this.reactions.clear(); this.threadCounts.clear(); this.serverThreadCounts.clear(); this.list.replaceChildren();
        for (const item of messages) this.add(item);
        this.oldestSeq = messages[0]?.seq || 0; this.hasMoreOlder = messages.length === this.pageSize; this.list.dataset.hasOlder = String(this.hasMoreOlder);
        this.contextMode = true; this.latestButton.hidden = false; this.windowSeq = messages.at(-1)?.seq || 0;
      }
      if (message.thread_root) {
        let root = this.messages.get(message.thread_root);
        if (!root) {
          const roots = await request(`/api/rooms/${roomID}/messages?around=${encodeURIComponent(message.thread_root)}&limit=1`);
          if (!this.current(session, roomID) || generation !== this.openGeneration || jump !== this.jumpGeneration) return;
          root = roots.find(item => item.id === message.thread_root);
        }
        if (root) await this.openThread(root, message);
        if (!this.current(session, roomID) || generation !== this.openGeneration || jump !== this.jumpGeneration) return;
        { const entry = this.threadList.querySelector(`[data-message-id="${CSS.escape(message.id)}"]`); entry?.scrollIntoView({ block: 'center' }); this.flash(entry); }
      } else { const row = this.nodes.get(message.id); row?.scrollIntoView({ block: 'center' }); this.scrollAnchor?.update(); this.flash(row?.querySelector('.message-bubble') || row); }
      this.error.textContent = '';
    } catch (error) { if (this.current(session, roomID) && jump === this.jumpGeneration) this.error.textContent = error.message; }
  }
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
  async exportRoom(format) {
    if (!this.room) return;
    const room = this.room, session = this.sessionGeneration;
    try {
      // The server writes the file (every message of the room, with replies, attachments and reactions); the page only saves it.
      const { blob, filename } = await download(`/api/rooms/${room.id}/export?format=${format === 'json' ? 'json' : 'markdown'}`);
      if (!this.current(session, room.id)) return;
      const url = URL.createObjectURL(blob);
      const anchor = document.createElement('a'); anchor.href = url;
      anchor.download = filename || `${room.title.replace(/[^a-z0-9_-]+/gi, '-').replace(/^-|-$/g, '') || 'room'}.${format === 'json' ? 'json' : 'md'}`;
      anchor.click(); setTimeout(() => URL.revokeObjectURL(url), 1000);
      this.exportDialog.close();
    } catch (error) { if (this.current(session, room.id)) { this.error.textContent = error.message; this.exportDialog.close(); } }
  }
  async refreshMessages() {
    if (!this.room || this.initialLoading) return;
    const id = this.room.id, session = this.sessionGeneration, generation = this.openGeneration;
    for (;;) {
      const messages = await request(`/api/rooms/${id}/messages?after=${this.newestSeq || 0}&limit=${this.pageSize}`);
      if (!this.current(session, id) || generation !== this.openGeneration) return;
      for (const message of messages) {
        // A refresh that overlapped another can return a row the live cap already dropped: appending it would put it out of order.
        if (!message.thread_root && this.hasMoreOlder && message.seq < this.oldestSeq) continue;
        // A live reply whose root is neither rendered nor open has no row to count it on (the cap trimmed the root, or it was
        // never loaded): kept, it would pile up in the maps with no owner. The root's page brings the count back.
        if (message.thread_root && !this.nodes.has(message.thread_root) && this.threadRoot?.id !== message.thread_root) continue;
        if (message.thread_root === this.threadRoot?.id && !this.threadLatestButton.hidden && !this.threadEntryIDs.has(message.id)) { this.markNewThreadReply(message); continue; }
        if (!this.contextMode || (message.thread_root && (this.threadRoot?.id === message.thread_root || this.nodes.has(message.thread_root)))) { const known = this.messages.has(message.id); this.add(message); if (!known && this.liveArrival(message) && !this.announcedIDs?.has(message.id)) this.announce(message); }
        else if (!message.thread_root && !this.messages.has(message.id) && !this.missedIDs.has(message.id)) {
          // A history window does not show the live end of the room, but the arrival is still news: count it on the button and announce it.
          this.missedIDs.add(message.id); this.updateLatestLabel();
          if (!this.initialLoading && message.author_kind !== 'system') this.announce(message);
        }
      }
      if (messages.length) this.newestSeq = messages.at(-1).seq;
      if (messages.length) this.trimLive();
      // What was just rendered at the latest view is seen now. The project refresh for the same event ran before this
      // render and could only mark up to the previous newest message.
      if (messages.length && !this.contextMode && !this.catchingUp) this.onLatest?.(this.room);
      if (messages.length < this.pageSize) return;
    }
  }
  announce(message) { this.announcer.arrival({ id: message.id, mine: message.author_kind === 'user' && message.author_id === currentUser, from: this.authorName(message), body: message.body, room: this.room.title }); }
  // Only messages that land in the main list while the room is open are announced: not history, older
  // pages, thread replies or system notices. What this tab sent is silenced by the announcer, by id.
  liveArrival(message) {
    return !this.initialLoading && !this.contextMode && !message.thread_root && message.author_kind !== 'system';
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
  recallCandidates() {
    const own = [...this.messages.values()].filter(message => message.author_kind === 'user' && message.author_id === currentUser && !message.thread_root).sort((a, b) => a.seq - b.seq);
    const out = [];
    for (const message of own) if (out.at(-1) !== message.body) out.push(message.body);
    return out.slice(-200);
  }
  // Terminal-style history: Up at an empty composer or caret start walks back through own messages (once recalling, on the first line),
  // Down walks forward (restoring the in-progress draft past the newest), Esc restores the draft.
  // Returns true when the key was consumed. Open autocomplete popups keep their own arrow keys.
  recallKey(event) {
    if (event.isComposing || event.shiftKey || event.ctrlKey || event.altKey || event.metaKey) return false;
    if (![this.quickCommands, this.macroAutocomplete, this.mentionAutocomplete].every(popup => popup?.popup.hidden !== false)) return false;
    const draft = this.draft, recall = this.recall, at = draft.selectionStart, end = draft.selectionEnd, empty = !draft.value;
    const show = value => { draft.value = value; draft.setSelectionRange(value.length, value.length); this.resizeDraft(); };
    if (event.key === 'Escape' && recall.index !== null) { event.preventDefault(); show(recall.draft); recall.index = null; return true; }
    if (event.key === 'ArrowUp' && (empty || (at === 0 && end === 0) || (recall.index !== null && !draft.value.slice(0, at).includes('\n')))) {
      const candidates = this.recallCandidates(); if (!candidates.length) return false;
      event.preventDefault();
      if (recall.index === null) { recall.draft = draft.value; recall.index = candidates.length - 1; } else recall.index = Math.max(recall.index - 1, 0);
      show(candidates[recall.index]); return true;
    }
    if (event.key === 'ArrowDown' && recall.index !== null && (empty || !draft.value.slice(end).includes('\n'))) {
      const candidates = this.recallCandidates(); event.preventDefault();
      recall.index++;
      if (recall.index >= candidates.length) { show(recall.draft); recall.index = null; } else show(candidates[recall.index]);
      return true;
    }
    return false;
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
    const article = document.createElement('article'); article.className = message.author_kind === 'system' ? 'message system-notice' : message.author_kind === 'user' && message.author_id === currentUser ? 'message self' : 'message'; article.dataset.messageId = message.id; article.dataset.seq = String(message.seq); article.dataset.no = String(message.no);
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
    else this.refreshReactions(message.id);
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
    const count = Math.max(this.threadCounts.get(id) || 0, this.serverThreadCounts?.get(id) || 0), label = this.threadLabel(id);
    const article = this.nodes.get(id);
    for (const button of article?.querySelectorAll('.thread-action') || []) { button.title = label; button.setAttribute('aria-label', label); }
    const chip = article?.querySelector(':scope > .thread-chip'); if (chip) { chip.hidden = !count; chip.textContent = count === 1 ? '1 reply' : `${count} replies`; chip.title = 'Open thread'; chip.setAttribute('aria-label', `Open thread, ${count} ${count === 1 ? 'reply' : 'replies'}`); }
    if (this.threadRoot?.id === id && this.threadTitle) this.threadTitle.textContent = count ? `Thread · ${count}` : 'Thread';
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
    setLatest(this.threadNewButton, `${count} new ${count === 1 ? 'reply' : 'replies'}`, count);
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
  async send() {
    if (!this.room || this.pendingUploads || (!this.draft.value.trim() && !this.pendingAttachments.length)) return;
    const room = this.room.id, body = this.draft.value, reply = this.reply, attachments = this.pendingAttachments.map(a => a.id), session = this.sessionGeneration;
    this.error.textContent = ''; this.notice.clear();
    this.announcer.sendStarted();
    let sentID = null;
    try {
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
  async refreshReactions(id) {
    const article = this.nodes.get(id); if (!article) return;
    const session = this.sessionGeneration;
    const entries = await request(`/api/messages/${id}/reactions`);
    if (session !== this.sessionGeneration || this.nodes.get(id) !== article) return;
    this.showReactions(id, entries);
  }
  showReactions(id, entries) {
    this.reactions.set(id, entries);
    const article = this.nodes.get(id); if (!article) return;
    const pills = article.querySelector('.reaction-pills'); pills.replaceChildren();
    const grouped = new Map();
    for (const entry of entries) { if (!grouped.has(entry.emoji)) grouped.set(entry.emoji, []); grouped.get(entry.emoji).push(entry); }
    for (const [emoji, members] of grouped) {
      const pill = document.createElement('button'); pill.type = 'button'; pill.className = 'row-btn reaction-pill'; pill.textContent = `${emoji} ${members.length}`;
      pill.title = members.map(member => member.member_name || member.member_id).join(', '); pill.setAttribute('aria-pressed', members.some(member => member.member_id === currentUser)); pill.dataset.emoji = emoji; pills.append(pill);
    }
  }
  async toggleReaction(id, emoji) {
    const session = this.sessionGeneration;
    const mine = this.reactions.get(id)?.some(entry => entry.emoji === emoji && entry.member_id === currentUser);
    try { await request(`/api/messages/${id}/reactions/${encodeURIComponent(emoji)}`, { method: mine ? 'DELETE' : 'PUT' }); if (session !== this.sessionGeneration) return; await this.refreshReactions(id); }
    catch (error) { if (session === this.sessionGeneration) this.error.textContent = error.message; }
  }
}
