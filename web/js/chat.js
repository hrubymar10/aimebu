import { request, currentUser, currentLogin } from './api.js';
import { PREFERENCE_KEYS, savePreference } from './preferences.js';
import { retention } from './retention.js';
import { usageTooltip } from './message-usage-format.js';
import { toggleMessageUsage, closeMessageUsage } from './message-usage.js';
import { renderMarkdown, renderRaw, copyText } from './markdown.js';
import { iconGlyph } from './icon-button.js';
import { refreshRelativeTimes } from './time.js';
import { ScrollAnchor } from './scroll-anchor.js';
import { senderIdentity, senderKind, agentLabel } from './sender-identity.js';
import { openSenderProfile } from './pages/sender-profile/page.js';
import { RoomPanel } from './pages/room-panel/page.js';
import { QuickCommands } from './quick-commands.js';
import { MacroAutocomplete } from './macros.js';
import { MentionAutocomplete, MentionIndex, highlightMentions } from './mentions.js';
import { EmojiAutocomplete } from './emoji-suggest.js';
import { Notice } from './notice.js';
import { closeEmojiPicker, openEmojiPicker } from './emoji-picker.js';
import { LiveAnnouncer } from './live-region.js';
import { ComposerFormatting } from './composer.js';
import { messageAction, wakeMessageActions, releaseMessageActions } from './message-actions.js';
import { promptDialog } from './dialogs.js';
import { expandShortcodes } from './emoji-data.js';
import { ChatStore } from './pages/chat/store.js';
import { MessageList, ThreadList } from './pages/chat/rows.js';
import { PendingAttachments } from './pages/chat/message-box.js';
import { mountPreactPage } from './pages/mount-page.js';
import { html } from './ui.js';
import { MENTION_LOGINS_TTL_MS, answerArea, answerEligible, answersThroughInbox as answersThrough, articleClass, groupReactions, latestLabel, liveArrival as isLiveArrival, messageSnippet, newestShownSeq as shownSeq, newRepliesLabel, plainText, recallCandidates as ownMessages, recallStep, threadChip, threadCount, threadLabel as threadLabelOf, threadTitle } from './pages/chat/model.js';

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
    this.latestButton = document.createElement('button'); this.latestButton.type = 'button'; this.latestButton.className = 'btn-icon back-to-latest'; setLatest(this.latestButton, 'Back to latest'); this.latestButton.hidden = true;
    this.latestButton.onclick = () => this.open(this.room);
    this.title.parentElement.append(this.latestButton);
    this.list.addEventListener('scroll', () => {
      if (this.list.scrollTop < 80) this.loadOlder();
      // A history window pages forward again when its bottom is reached (only then is scrollHeight read on scroll).
      if (this.contextMode && this.list.scrollHeight - this.list.scrollTop - this.list.clientHeight < 80) this.loadNewer();
    });
    this.scrollAnchor = new ScrollAnchor(this.list, '.chat-msg, .answer-row');
    this.list.addEventListener('keydown', event => this.moveActionFocus(event));
    this.chip = document.querySelector('#reply-chip'); this.chipText = this.chip.querySelector('#reply-chip-jump'); this.chipText.onclick = () => { if (this.reply) this.jumpToMessage(this.reply.id); }; this.error = document.querySelector('#chat-error'); this.nodes = new Map();
    this.pendingNode = document.querySelector('#pending-attachments'); this.pendingNode.replaceChildren(); this.pendingView = mountPreactPage(this.pendingNode, props => html`<${PendingAttachments} ...${props} />`, { onError: error => console.error('pending attachments failed', error) }); this.attachmentInput = document.querySelector('#attachment-input');
    this.attachmentInput.onchange = () => { this.addFiles([...this.attachmentInput.files]); this.attachmentInput.value = ''; };
    this.draft.addEventListener('input', () => this.resizeDraft());
    document.addEventListener('click', event => { const ref = event.target.closest?.('a.msg-ref'); if (!ref || event.defaultPrevented || event.button) return; event.preventDefault(); this.jumpToMessage(Number(ref.dataset.no)); });
    this.draft.addEventListener('paste', event => { const files = [...event.clipboardData?.files || []]; if (files.length) { event.preventDefault(); this.addFiles(files); } });
    this.sendBar = this.form.closest('.send-bar');
    this.sendBar.addEventListener('dragover', event => { if (event.dataTransfer?.types.includes('Files')) { event.preventDefault(); this.sendBar.classList.add('drag-attachment'); } });
    this.sendBar.addEventListener('dragleave', event => { if (!this.sendBar.contains(event.relatedTarget)) this.sendBar.classList.remove('drag-attachment'); });
    this.sendBar.addEventListener('drop', event => { this.sendBar.classList.remove('drag-attachment'); if (event.dataTransfer?.files.length) { event.preventDefault(); this.addFiles([...event.dataTransfer.files]); } });
    setInterval(() => refreshRelativeTimes(this.list), 30000);
    this.lightbox = document.querySelector('#image-lightbox'); document.querySelector('#image-lightbox-close').onclick = () => this.lightbox.close();
    this.agents = new Map(); this.threadPanel = document.querySelector('#thread-panel'); this.threadTitle = this.threadPanel.querySelector('h2'); this.threadList = document.querySelector('#thread-messages'); this.threadForm = document.querySelector('#thread-composer'); this.threadDraft = this.threadForm.elements.body; this.threadFormatting = new ComposerFormatting(this.threadDraft);
    this.threadOlderStatus = document.createElement('div'); this.threadOlderStatus.className = 'older-status thread-older'; this.threadOlderStatus.setAttribute('role', 'status'); this.threadOlderStatus.textContent = 'Loading older replies…'; this.threadOlderStatus.hidden = true;
    this.threadLatestButton = document.createElement('button'); this.threadLatestButton.type = 'button'; this.threadLatestButton.className = 'btn-icon back-to-latest thread-latest'; setLatest(this.threadLatestButton, 'Latest replies'); this.threadLatestButton.hidden = true;
    this.threadLatestButton.onclick = () => { if (this.threadRoot) this.openThread(this.threadRoot); };
    this.threadNewButton = document.createElement('button'); this.threadNewButton.type = 'button'; this.threadNewButton.className = 'btn-icon back-to-latest thread-new'; setLatest(this.threadNewButton, 'New replies'); this.threadNewButton.hidden = true;
    this.threadNewButton.onclick = () => { if (this.threadRoot) this.openThread(this.threadRoot); };
    this.threadList.before(this.threadOlderStatus, this.threadLatestButton, this.threadNewButton);
    this.threadList.addEventListener('scroll', () => { if (this.threadList.scrollTop < 80) this.loadOlderThread(); });
    this.list.replaceChildren(); this.listView = mountPreactPage(this.list, props => html`<${MessageList} ...${props} />`, { onError: error => console.error('message list failed', error) });
    this.threadList.replaceChildren(); this.threadView = mountPreactPage(this.threadList, props => html`<${ThreadList} ...${props} />`, { onError: error => console.error('thread list failed', error) });
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
    retention.onChange(() => this.store.scheduleExpiry());
    setInterval(() => this.store.syncExpiry(), 30000);
    if (window.__exposeChat) window.__chat = this; // test hook: the live-cap smoke reads the per-message maps
  }
  open(room) { return this.store.open(room); }
  prefetchLatest(roomID) { this.store.prefetchLatest(roomID); }
  // The view before a room's first page: the app's and the composer's state for the room, an empty list.
  beginOpen(room) {
    this.announcer.reset();
    if (this.room?.id !== room.id) { this.renderPending(); if (this.recall) this.recall.index = null; }
    closeMessageUsage(); this.room = room; this.syncEmpty(); this.setRoomFacts(room); this.error.textContent = ''; this.notice.clear(); this.threadNotice.clear();
    this.olderStatus.hidden = true; this.list.dataset.hasOlder = 'false'; this.latestButton.hidden = true; this.updateLatestLabel(); this.setReply(null);
    for (const button of document.querySelectorAll('.room-controls button')) button.disabled = false;
    this.setRoomFacts(room);
    this.scrollAnchor.reset();
    delete this.list.dataset.empty; // no hint while the room is still loading
  }
  pageDrawn() {
    this.list.scrollTop = this.list.scrollHeight;
    this.list.dataset.empty = String(!this.list.querySelector('article.chat-msg:not(.chat-msg-system)')); // notices alone leave it empty
  }
  showNewest() { this.list.dataset.hasOlder = String(this.hasMoreOlder); this.list.scrollTop = this.list.scrollHeight; this.scrollAnchor?.update(); }
  // How far the read mark may go (0 while a room is still loading). At the latest view the list is contiguous to the live
  // cursor: every message up to newestSeq is applied (top-level rows rendered, thread replies counted on their roots).
  // Otherwise (a history window, or its catch-up in flight) only the newest top-level row actually in the list counts,
  // never a thread reply, which an open thread panel can hold far past the window's end.
  newestShownSeq() {
    const lastRowSeq = () => {
      for (let node = this.list.lastElementChild; node; node = node.previousElementSibling) if (node.matches('article.chat-msg[data-message-id]')) return Number(node.dataset.seq) || 0;
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
    this.room = null; this.syncEmpty(); this.panel.reset();
    this.error.textContent = ''; this.notice.clear(); this.threadNotice.clear(); this.agents.clear(); this.olderStatus.hidden = true; this.list.dataset.hasOlder = 'false'; this.latestButton.hidden = true; this.updateLatestLabel(); this.closeBlocked?.clear(); this.worktree = null;
    this.setReply(null); this.closeThread(); this.renderPending(); this.draft.value = ''; this.resizeDraft();
    this.lightbox.close();
    this.quickCommands.reset(); this.macroAutocomplete.reset(); this.mentionAutocomplete.reset(); this.threadMentionAutocomplete.reset(); this.emojiAutocomplete.reset(); this.threadEmojiAutocomplete.reset(); closeEmojiPicker(false); this.mentionIndex = null; this.mentionLogins = []; this.mentionLoginsRoom = ''; this.mentionLoginsAt = 0; this.mentionLoginsSeq = (this.mentionLoginsSeq || 0) + 1;
    for (const button of document.querySelectorAll('.room-controls button')) button.disabled = true;
    this.draft.disabled = true; this.emojiButton.disabled = true; this.attachmentInput.disabled = true; this.form.querySelector('button[type=submit]').disabled = true;
  }
  destroy() { this.reset(); this.store.destroy(); this.panel.destroy(); this.threadView.destroy(); this.listView.destroy(); this.pendingView.destroy(); }
  // With no room open the chat shows an empty-state card instead of the composer and room controls; the app decides what the card says.
  syncEmpty() { this.onEmptyChange?.(!this.room); }
  // A list too short to scroll cannot ask for more by scrolling, so a short page with more behind it asks right away.
  loadOlderIfShort() {
    requestAnimationFrame(() => { if (this.hasMoreOlder && this.list.scrollHeight <= this.list.clientHeight + 80) this.loadOlder(); });
  }
  loadOlder() { return this.store.loadOlder(); }
  showOlderStatus(shown) { this.olderStatus.hidden = !shown; }
  // Older rows go above the first row without moving what the reader looks at: only the height inserted above the anchor is compensated, never the
  // reader's own movement, so the anchor is measured now, not before the request.
  holdPlace() {
    const anchor = this.list.firstElementChild, anchorTop = anchor?.getBoundingClientRect().top;
    return () => { if (anchor?.isConnected) this.list.scrollTop += anchor.getBoundingClientRect().top - anchorTop; };
  }
  hasOlder(flag) { this.list.dataset.hasOlder = String(flag); }
  windowDrawn() { this.list.scrollTop = this.list.scrollHeight; this.scrollAnchor?.update(); }
  // A prepended (older) or paged (newer, in a history window) message leaves the scroll position alone; only a live append follows the bottom.
  listPlace() { return { atBottom: this.list.scrollHeight - this.list.scrollTop - this.list.clientHeight < 48, top: this.list.scrollTop }; }
  followList(place) { this.list.scrollTop = place.atBottom ? this.list.scrollHeight : place.top; this.scrollAnchor?.update(); }
  noteRow(message) { if (this.list.dataset.empty === 'true' && message.author_kind !== 'system') this.list.dataset.empty = 'false'; }
  listAtBottom() { return this.list.scrollHeight - this.list.scrollTop - this.list.clientHeight < 48; }
  listBox() { const box = this.list.getBoundingClientRect(); return { top: box.top, bottom: box.bottom }; }
  rowBoxes() {
    // A row's answers (the controls, the history or the marker) are a row of the list under its card: the row ends where they end.
    const tailOf = node => { const next = node.nextElementSibling; return next?.classList.contains('answer-row') ? next : node; };
    return this.renderedRows().map(node => ({ id: node.dataset.messageId, seq: Number(node.dataset.seq), get top() { return node.getBoundingClientRect().top; }, get bottom() { return tailOf(node).getBoundingClientRect().bottom; } }));
  }
  keepPlace(id) {
    const node = this.nodes.get(id), keepTop = node.getBoundingClientRect().top;
    return () => { this.list.scrollTop += node.getBoundingClientRect().top - keepTop; };
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
  renderedRows() { return [...this.list.children].filter(node => node.matches?.('article.chat-msg[data-message-id]')); }
  loadNewer() { return this.store.loadNewer(); }
  leaveWindow() { return this.store.leaveWindow(); }
  setSpawnOpener(openSpawn) { this.spawnCallback = openSpawn; }
  // The two things a historical sender's profile can do: start it as a v2 agent (opens the spawn dialog prefilled), and show the agent once started.
  setLegacyActions(start, open) { this.legacyStarter = start; this.legacyOpener = open; }
  setDebugInspector(open) { this.debugCallback = open; }
  // One click and one key listener per list serve every row's controls (see onMessageClick), so a row costs no closures.
  // Rows also install them (once per list), for a chat assembled without its constructor.
  delegateRows(container) {
    if (!container || container.__rowsDelegated) return;
    container.__rowsDelegated = true;
    container.addEventListener('click', event => this.onMessageClick(event));
    container.addEventListener('keydown', event => this.onAuthorKey(event));
    // A row's toolbar is made when the row is reached by the pointer, the keyboard or a touch (see message-actions.js), and the oldest ones are
    // released as others are reached.
    for (const type of ['pointerover', 'focusin', 'pointerdown']) container.addEventListener(type, event => this.wakeActions(event, container));
  }
  wakeActions(event, container) {
    const target = event.target;
    const row = target.closest?.('article.chat-msg'); if (!row || !container.contains(row)) return;
    const bar = row.querySelector('.message-actions'); if (!bar || bar.dataset.built) return;
    const message = this.entryMessage(row.dataset.messageId); if (!message) return;
    wakeMessageActions(this, bar, message);
    this.enterActions(event, bar);
    const live = (this.liveActions ||= []).filter(item => item.isConnected && item !== bar && item.dataset.built);
    live.push(bar);
    // Only the three most recent stay made; an older one that still holds the focus, the pointer or an open pop-up stays too, and is looked at again next time.
    const older = live.slice(0, Math.max(0, live.length - 3)), kept = older.filter(item => !releaseMessageActions(item));
    this.liveActions = [...kept, ...live.slice(older.length)];
  }
  // The focus was on the hidden "Message actions" button that has just become the toolbar: it goes to the toolbar's first button, or to
  // its last when the row was entered from below (Shift+Tab).
  enterActions(event, bar) {
    if (event.type === 'pointerover' || event.type === 'pointerdown') return;
    if (!event.target.classList?.contains('message-actions-wake')) return;
    const buttons = bar.querySelectorAll(':scope > button'), row = bar.closest('.chat-msg');
    const below = event.relatedTarget && row && !!(row.compareDocumentPosition(event.relatedTarget) & Node.DOCUMENT_POSITION_FOLLOWING) && !row.contains(event.relatedTarget);
    const to = below ? buttons[buttons.length - 1] : buttons[0];
    // The focus is still moving to the button this toolbar replaced, so it is moved on afterwards.
    queueMicrotask(() => to?.focus());
  }
  // The row controls of the message list and the thread panel, delegated: the control's row names the message.
  onMessageClick(event) {
    if (event.target.classList?.contains('message-actions-wake')) { this.wakeActions(event, event.currentTarget); return; }
    const control = event.target.closest?.('button, strong.chat-msg-from-name, span.mention[role=button]');
    if (!control || !event.currentTarget.contains(control)) return;
    const holder = control.closest('[data-message-id]'); if (!holder || !event.currentTarget.contains(holder)) return;
    const message = this.entryMessage(holder.dataset.messageId); if (!message) return;
    const is = name => control.classList.contains(name);
    if (is('chat-msg-from-name')) { if (control.getAttribute('role') === 'button') this.openSender(message); return; }
    if (is('mention')) { this.openMention(control.textContent); return; }
    if (is('chat-msg-id')) this.copyNumber(control, message);
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
    const author = event.target.closest?.('strong.chat-msg-from-name[role=button], span.mention[role=button]');
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
  updateAnswerControls(items) { return this.store.updateAnswerControls(items); }
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
    const note = this.viewerNote ||= (() => { const node = document.createElement('p'); node.className = 'system-room-notice'; node.id = 'composer-viewer-note'; node.setAttribute('role', 'note'); this.form.before(node); return node; })();
    note.hidden = !(open && viewer && (room.kind !== 'dm' || agentChat));
    note.textContent = agentChat ? 'You can read this chat. Writing here takes the owner or operator role in one of the agents\' projects.' : 'You can read this room as a viewer. Only owners and operators of the project can write here or run agents.';
    // An archived project is read-only for everyone; the composer says why and where to undo it.
    const archivedNote = this.archivedNote ||= (() => { const node = document.createElement('p'); node.className = 'system-room-notice'; node.id = 'composer-archived-note'; node.setAttribute('role', 'note'); this.form.before(node); return node; })();
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
  // A history window around a message: the list starts again from the page that holds it.
  beginWindow() { this.olderStatus.hidden = true; this.list.dataset.hasOlder = 'false'; }
  showWindow() { this.list.dataset.hasOlder = String(this.hasMoreOlder); this.latestButton.hidden = false; }
  showReply(message) { const entry = this.threadList.querySelector(`[data-message-id="${CSS.escape(message.id)}"]`); entry?.scrollIntoView({ block: 'center' }); this.flash(entry); }
  showRow(message) { const row = this.nodes.get(message.id); row?.scrollIntoView({ block: 'center' }); this.scrollAnchor?.update(); this.flash(row?.querySelector('.chat-msg-bubble') || row); }
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
  addFiles(files) { return this.store.addFiles(files); }
  renderPending() {
    this.pendingView.update({ attachments: this.pendingAttachments, uploads: this.pendingUploads, onRemove: attachment => this.store.removeAttachment(attachment) });
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
    this.threadUpdate();
    const button = document.querySelector('#room-raw');
    button.setAttribute('aria-pressed', String(this.raw));
    button.title = this.raw ? 'Show rendered Markdown' : 'Show raw Markdown';
    button.setAttribute('aria-label', button.title);
    this.listUpdate();
  }
  exportRoom(format) { return this.panel.exportRoom(format); }
  refreshMessages() { return this.store.refresh(); }
  syncInPlace() { return this.store.syncInPlace(); }
  threadLatestHidden() { return this.threadLatestButton.hidden; }
  hideThreadLatest() { this.threadLatestButton.hidden = true; }
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
  scheduleMentions() { if (!this.mentionTimer) this.mentionTimer = setTimeout(() => { this.mentionTimer = null; this.refreshMentions(); }, 60); }
  refreshMentions() {
    const index = this.room ? new MentionIndex([...this.agents.values()].filter(agent => agent.room_id === this.room.id), this.room.project_id, this.mentionLogins, [...(this.historicalAuthors || [])], currentLogin) : null;
    if (index?.signature === this.mentionIndex?.signature) return;
    this.mentionIndex = index;
    this.mentionAutocomplete?.setIndex(index); this.threadMentionAutocomplete?.setIndex(index);
    this.listUpdate(); this.threadUpdate();
  }
  setAgents(agents) {
    this.agents = new Map(agents.map(agent => [agent.id, agent]));
    // One signature per agent record, so a row whose sender did not change between polls is not drawn again.
    this.agentSigs = new Map(agents.map(agent => [agent.id, JSON.stringify(agent)]));
    this.refreshMentions();
    this.listUpdate(); this.threadUpdate();
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
  showThreadCount(id, count) { if (this.threadRoot?.id === id && this.threadTitle) this.threadTitle.textContent = threadTitle(count); }
  // The panel's entries are a Preact list (rows.js): the root first, the replies by seq. An update renders at once, so callers read the rows back.
  threadUpdate(restore) {
    if (!this.threadView) return;
    this.delegateRows(this.threadList);
    this.threadView.update({ entries: this.threadEntries, host: this, viewOf: message => this.rowView(message), restore });
  }
  listUpdate(restore) {
    if (!this.listView) return;
    this.delegateRows(this.list);
    this.listView.update({ entries: this.rows, host: this, viewOf: message => this.rowView(message, true), restore });
  }
  registerNode(id, element) { if (element) this.nodes.set(id, element); else this.nodes.delete(id); }
  quoteView(message) { return this.quotes.get(message.id) ?? null; }
  // What a row shows that is not in its record: the author as the page knows it, the answer state, and the keys that make the row draw again.
  rowView(message, main = false) {
    const agent = this.agents.get(message.author_id), kind = senderKind(message, agent), live = kind === 'live' && message.author_kind === 'agent';
    const author = { name: this.authorName(message), kind, link: kind !== 'live' || live, title: live ? 'Open profile' : `${kind === 'historical' ? 'Historical sender' : 'Departed agent'}: open profile`,
      identity: senderIdentity(message, agent), roleEmoji: agent?.role_emoji || '', roleTitle: agent?.role || '' };
    const answer = this.answerView(message), bodyKey = `${this.raw}|${this.mentionIndex?.signature ?? ''}`;
    const view = { author, answer, bodyKey, raw: this.raw, mentionKey: this.mentionIndex?.signature ?? '', usageTitle: message.usage ? usageTooltip(message.usage) : '', needsAttention: !!message.needs_attention };
    let extra = '';
    if (main) {
      const count = threadCount(this.threadCounts, this.serverThreadCounts, message.id);
      view.articleClass = articleClass(message, currentUser); view.chip = threadChip(count); view.threadLabel = this.threadLabel(message.id);
      view.pills = groupReactions(this.reactions.get(message.id) || [], currentUser); view.quote = this.quoteView(message);
      view.expired = this.expiry.expired.has(message.id); view.boundary = this.expiry.divider === message.id;
      extra = `|${count}|${JSON.stringify(view.pills)}|${view.expired}|${view.boundary}`;
    }
    view.key = `${this.agentSigs?.get(message.author_id) ?? ''}|${bodyKey}|${answer.key}${extra}`;
    return view;
  }
  answerView(message) {
    const id = message.id, item = this.answerItems?.get(id);
    if (answerEligible(item)) { return { kind: 'controls', item, parent: message, key: `c:${item.id}:${item.rev}` }; }
    const answered = Boolean(this.answeredMessages.has(id) || this.repliedTo.has(id)), area = answerArea(item, message, answered);
    if (area === 'history') return { kind: 'history', answered, key: `h:${answered}` };
    if (area === 'done') return { kind: 'done', answered, key: 'd' };
    return { kind: 'none', answered, key: '' };
  }
  answer(item, value) { return this.inbox.answer(item, value); }
  highlight(element) { if (!this.raw) highlightMentions(element, this.mentionIndex); }
  updateThreadNewMarker() {
    const count = this.threadNewReplyIDs.size;
    setLatest(this.threadNewButton, newRepliesLabel(count), count);
    this.threadNewButton.hidden = count === 0;
  }
  openThread(root, focus = null) { return this.store.openThread(root, focus); }
  loadOlderThread() { return this.store.loadOlderThread(); }
  sendThread() { return this.store.sendThread(); }
  send() { return this.store.send(); }
  reopenLatest() { return this.store.reopenLatest(); }
  closeThread() { return this.store.closeThread(); }
  threadClosed() {
    this.threadNotice.clear(); this.threadRoot = null; this.threadTitle.textContent = 'Thread'; this.threadUpdate(); this.threadOlderStatus.hidden = true; this.threadList.dataset.hasOlder = 'false';
    this.threadLatestButton.hidden = true; this.threadNewButton.hidden = true; this.threadPanel.hidden = true; this.threadDraft.value = ''; document.querySelector('#thread-error').textContent = '';
  }
  setReply(message) { this.reply = message; this.chip.hidden = !message; this.chipText.textContent = message ? `Replying to ${this.authorName(message)}: ${messageSnippet(message)}` : ''; if (message) this.draft.focus(); }
  // Whether a reply to `parent` with this text is the answer to `item` (see send): a question or approval this person decides, answered with text
  // that mentions at most the requester. A free-text ask takes any text; an options-only ask only the text of one of its options (so Shift-clicking
  // an answer and sending it unedited acts like the button, while edited text is an ordinary reply).
  answersThroughInbox(item, parent, body) { return answersThrough(item, this.answerOptions(parent).mention, body); }
  beginThread(root, focus) {
    this.threadRoot = root; this.threadTitle.textContent = 'Thread'; this.threadPanel.hidden = false; this.threadUpdate();
    this.threadOlderStatus.hidden = true; this.threadList.dataset.hasOlder = 'false'; this.threadLatestButton.hidden = !focus; this.threadNewButton.hidden = true;
    document.querySelector('#thread-error').textContent = '';
    this.threadDraft.focus();
  }
  showThreadPage(focus) {
    this.threadList.dataset.hasOlder = String(this.threadHasMoreOlder);
    if (focus) this.threadList.querySelector(`[data-message-id="${CSS.escape(focus.id)}"]`)?.scrollIntoView({ block: 'center' });
    else this.threadList.scrollTop = this.threadList.scrollHeight;
  }
  threadError(message) { document.querySelector('#thread-error').textContent = message; }
  showThreadOlderStatus(shown) { this.threadOlderStatus.hidden = !shown; }
  holdThreadPlace() {
    // Measured after the page arrives, as in loadOlder: only the inserted height is compensated, not the reader's own scrolling.
    const anchor = this.threadList.children[1], anchorTop = anchor?.getBoundingClientRect().top;
    this.threadList.dataset.hasOlder = String(this.threadHasMoreOlder);
    return () => { if (anchor?.isConnected) this.threadList.scrollTop += anchor.getBoundingClientRect().top - anchorTop; };
  }
  beginThreadSend() { document.querySelector('#thread-error').textContent = ''; this.threadNotice.clear(); }
  sentThreadReply() { this.threadDraft.value = ''; }
  threadToEnd() { this.threadList.scrollTop = this.threadList.scrollHeight; }
  threadSendFailed(message) { this.threadNotice.show(message); }
  restoreThreadDraft(draft) { this.threadDraft.value = draft; this.draft.focus(); }
  // Whether the person has typed or attached something that is not sent: the message and the thread reply, whichever way the composer is
  // showing them (the formatted editor keeps the textarea in step), and files waiting to go with the message.
  hasDraft() { return !!(this.draft.value.trim() || this.threadDraft.value.trim() || this.pendingAttachments?.length || this.pendingUploads); }
  sendInput() {
    if (this.pendingUploads || (!this.draft.value.trim() && !this.pendingAttachments.length)) return null;
    return { body: this.draft.value, reply: this.reply, attachments: this.pendingAttachments.map(a => a.id) };
  }
  beginSend() { this.error.textContent = ''; this.notice.clear(); this.announcer.sendStarted(); }
  async sendAsAnswer(reply, attachments, body, current) {
    // A reply to a message with a free-text question or approval open for this person is the answer to it: it goes through the inbox, which
    // resolves the item, so the item closes and the Inbox count drops, whether the text came from Shift-clicking an answer or was typed. It
    // stays an ordinary reply when it carries attachments (an answer is text), when the item is not one this person decides, when the ask is
    // options only and the text is not one of the options (arbitrary text does not answer it), or when the text mentions anyone but the requester (so
    // an @reviewer wake still works).
    const open = reply && !attachments.length ? this.answerItems?.get(reply.id) : null;
    if (!(open && this.answersThroughInbox(open, reply, body))) return false;
    if (this.answering) return true;
    this.answering = true;
    try {
      // Only a confirmed answer clears the draft; a card that is already pending answers undefined, and a refusal answers false.
      if (await this.inbox.answer(open, body.trim()) !== true) throw new Error('Could not send the answer');
    } finally { this.answering = false; }
    if (current()) { this.draft.value = ''; if (this.recall) this.recall.index = null; this.resizeDraft(); this.setReply(null); }
    return true;
  }
  sentMessage() { this.draft.value = ''; if (this.recall) this.recall.index = null; this.resizeDraft(); this.renderPending(); this.setReply(null); }
  sendFailed(message) { this.notice.show(message); }
  sendFinished(id) { this.announcer.sendFinished(id); }
  refreshReactions(id) { return this.store.refreshReactions(id); }
  toggleReaction(id, emoji) { return this.store.toggleReaction(id, emoji); }
}

for (const name of ['messages', 'reactions', 'threadCounts', 'serverThreadCounts', 'repliedTo', 'answeredMessages', 'historicalAuthors', 'missedIDs', 'announcedIDs', 'newestSeq', 'oldestSeq', 'hasMoreOlder', 'loadingOlder', 'loadingNewer', 'contextMode', 'catchingUp', 'windowSeq', 'initialLoading', 'openGeneration', 'jumpGeneration', 'pageSize', 'olderPageSize', 'answerItems', 'rows', 'quotes', 'expiry', 'threadEntries', 'threadEntryIDs', 'threadNewReplyIDs', 'pendingAttachments', 'pendingUploads', 'uploadRequests', 'threadGeneration', 'threadOldestSeq', 'threadHasMoreOlder', 'loadingOlderThread', 'threadPageSize', 'threadOlderPageSize']) {
  Object.defineProperty(Chat.prototype, name, { configurable: true, get() { return this.store[name]; }, set(value) { this.store[name] = value; } });
}
