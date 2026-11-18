import { request, currentUser, currentLogin } from './api.js';
import { PREFERENCE_KEYS, savePreference } from './preferences.js';
import { retention } from './retention.js';
import { usageTooltip } from './message-usage-format.js';
import { toggleMessageUsage, closeMessageUsage } from './message-usage.js';
import { renderMarkdown, renderRaw, copyText } from './markdown.js';
import { AttachmentStrip, Control, LatestButton, OlderStatus, RoomNote, TreeNewButton } from './components/chat-controls.js';
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
import { enterSends } from './enter-key.js';
import { closeEmojiPicker, openEmojiPicker } from './emoji-picker.js';
import { LiveAnnouncer } from './live-region.js';
import { ComposerFormatting } from './composer.js';
import { wakeMessageActions, releaseMessageActions } from './message-actions.js';
import { promptDialog } from './dialogs.js';
import { expandShortcodes } from './emoji-data.js';
import { ChatStore } from './pages/chat/store.js';
import { MessageList, ThreadList } from './pages/chat/rows.js';
import { PendingAttachments } from './pages/chat/message-box.js';
import { mountPreactPage } from './pages/mount-page.js';
import { html, render } from './ui.js';
import { MENTION_LOGINS_TTL_MS, answerArea, answerEligible, answersThroughInbox as answersThrough, articleClass, groupReactions, latestLabel, liveArrival as isLiveArrival, messageSnippet, newestShownSeq as shownSeq, treeLayout, newRepliesLabel, plainText, recallCandidates as ownMessages, recallStep, threadChip, threadCount, threadLabel as threadLabelOf, threadTitle } from './pages/chat/model.js';

export { plainText, messageSnippet };

// "Back to latest" and its thread twins are icon buttons (components/chat-controls.js); a count of missed messages is a pill on the icon, and the tooltip and
// accessible name carry the full wording ("Back to latest · 2 new").
function setLatest(control, label, count = 0) { control.set({ label, count }); }

export const FLASH_MS = 1500;
// Sizes a message box to its text, up to 180px: it never shows a scrollbar until it is at its height limit (a line of text and its borders can
// be a pixel more than the box's own minimum, which was enough to draw one for no reason). The room's box and the thread's are sized the same way.
export function fitDraft(draft) {
  draft.style.height = '39px';
  draft.style.height = `${Math.min(draft.scrollHeight, 180)}px`;
  draft.style.overflowY = draft.scrollHeight > 180 ? 'auto' : 'hidden';
}

export class Chat {
  constructor(onRoomMutation) {
    this.onRoomMutation = onRoomMutation;
    this.store = new ChatStore(this);
    this.list = document.querySelector('#messages'); this.title = document.querySelector('#room-title'); this.roomIcon = document.querySelector('#room-icon'); this.members = document.querySelector('#room-members'); this.form = document.querySelector('#composer'); this.draft = this.form.elements.body; this.formatting = new ComposerFormatting(this.draft, { onFiles: files => this.addFiles(files) });
    this.announcer = new LiveAnnouncer(document.querySelector('#live-announcer'));
    // Older pages load by themselves when the list is scrolled (or keyboard-scrolled: Home, PageUp) to the top; this only says so while it fetches.
    this.olderStatus = new Control(OlderStatus, { text: 'Loading older messages…', hidden: true });
    this.list.before(this.olderStatus.host);
    this.latestButton = new Control(LatestButton, { label: 'Back to latest', count: 0, hidden: true, onClick: () => this.open(this.room) });
    this.title.parentElement.append(this.latestButton.host);
    this.list.addEventListener('scroll', () => {
      if (this.list.scrollTop < 80) this.loadOlder();
      // A history window pages forward again when its bottom is reached (only then is scrollHeight read on scroll).
      if (this.contextMode && this.list.scrollHeight - this.list.scrollTop - this.list.clientHeight < 80) this.loadNewer();
    });
    this.scrollAnchor = new ScrollAnchor(this.list, '.chat-msg');
    this.list.addEventListener('keydown', event => this.moveActionFocus(event));
    this.chip = document.querySelector('#reply-chip'); this.chipText = this.chip.querySelector('#reply-chip-jump'); this.chipText.onclick = () => { if (this.reply) this.jumpToMessage(this.reply.id); }; this.error = document.querySelector('#chat-error'); this.nodes = new Map();
    this.pendingNode = document.querySelector('#pending-attachments'); this.pendingNode.replaceChildren(); this.pendingView = mountPreactPage(this.pendingNode, props => html`<${PendingAttachments} ...${props} />`, { onError: error => console.error('pending attachments failed', error) }); this.attachmentInput = document.querySelector('#attachment-input');
    this.attachmentInput.onchange = () => { this.addFiles([...this.attachmentInput.files]); this.attachmentInput.value = ''; };
    // The thread's composer offers the same attach and emoji controls, over its own list of waiting files.
    this.threadPendingNode = document.querySelector('#thread-pending-attachments'); this.threadPendingNode.replaceChildren();
    this.threadPendingView = mountPreactPage(this.threadPendingNode, props => html`<${PendingAttachments} ...${props} />`, { onError: error => console.error('thread pending attachments failed', error) });
    this.threadAttachmentInput = document.querySelector('#thread-attachment-input');
    this.threadAttachmentInput.onchange = () => { this.store.addThreadFiles([...this.threadAttachmentInput.files]); this.threadAttachmentInput.value = ''; };
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
    this.threadOlderStatus = new Control(OlderStatus, { text: 'Loading older replies…', className: 'thread-older', hidden: true });
    this.threadLatestButton = new Control(LatestButton, { label: 'Latest replies', count: 0, className: 'thread-latest', hidden: true, onClick: () => { if (this.threadRoot) this.openThread(this.threadRoot); } });
    this.threadNewButton = new Control(LatestButton, { label: 'New replies', count: 0, className: 'thread-new', hidden: true, onClick: () => { if (this.threadRoot) this.openThread(this.threadRoot); } });
    // Replies that arrived inside the tree (the thread panel's tree option) where the reader is not looking: ids, and the marker that leads to them.
    this.treeNewIDs = new Set();
    this.threadTreeButton = new Control(TreeNewButton, { label: 'New replies in the conversation', count: 0, dir: 'up', className: 'thread-tree-new', hidden: true, onClick: () => this.showTreeNew() });
    this.threadList.before(this.threadOlderStatus.host, this.threadLatestButton.host, this.threadNewButton.host, this.threadTreeButton.host);
    let treeFrame = 0;
    const settleTree = () => { cancelAnimationFrame(treeFrame); treeFrame = requestAnimationFrame(() => this.updateTreeNew()); };
    this.threadList.addEventListener('scroll', settleTree, { passive: true });
    this.threadList.addEventListener('scroll', () => { if (this.threadList.scrollTop < 80) this.loadOlderThread(); });
    // The root stays in view while the replies scroll: once its own entry is out of view, a one-line bar over the top of the list stands for it.
    this.threadPin = document.querySelector('#thread-root-pin');
    this.threadPin.onclick = () => { this.threadList.scrollTop = 0; };
    this.threadList.addEventListener('scroll', () => this.updateThreadPin());
    new ResizeObserver(() => this.updateThreadPin()).observe(this.threadPanel); // a resized panel or a wrapped header moves the list's top edge
    this.list.replaceChildren(); this.listView = mountPreactPage(this.list, props => html`<${MessageList} ...${props} />`, { onError: error => console.error('message list failed', error) });
    this.threadList.replaceChildren(); this.threadView = mountPreactPage(this.threadList, props => html`<${ThreadList} ...${props} />`, { onError: error => console.error('thread list failed', error) });
    this.delegateRows(this.list); this.delegateRows(this.threadList);
    this.threadForm.onsubmit = event => { event.preventDefault(); this.sendThread(); };
    document.querySelector('#close-thread').onclick = () => this.closeThread();
    this.form.onsubmit = event => { event.preventDefault(); this.send(); };
    // ↑/↓ history of the viewer's own messages, in each composer (the room's, and the thread panel's over that conversation). The keys go through the composer's input
    // (the editor while formatting is on, the textarea otherwise), because the editor, not the textarea, has the focus and the caret.
    this.recall = { index: null, draft: '' }; this.threadRecall = { index: null, draft: '' };
    this.formatting.input.addEventListener('input', () => { this.recall.index = null; });
    this.formatting.input.addEventListener('keydown', event => { this.recallKey(event); });
    this.threadFormatting.input.addEventListener('input', () => { this.threadRecall.index = null; });
    this.threadFormatting.input.addEventListener('keydown', event => { this.recallKey(event, true); });
    for (const field of [this.draft, this.threadDraft]) field.addEventListener('focus', () => { if (this.room) this.loadMentionLogins(this.room); });
    this.draft.addEventListener('keydown', event => {
      if (event.key === 'Enter' && enterSends() && !event.shiftKey && !event.isComposing && this.quickCommands?.popup.hidden !== false && this.macroAutocomplete?.popup.hidden !== false && this.mentionAutocomplete?.popup.hidden !== false && this.emojiAutocomplete?.popup.hidden !== false) { event.preventDefault(); this.form.requestSubmit(); }
    });
    this.threadDraft.addEventListener('input', () => this.resizeThreadDraft());
    this.threadDraft.addEventListener('keydown', event => {
      if (event.key === 'Enter' && enterSends() && !event.shiftKey && !event.isComposing && this.threadMentionAutocomplete?.popup.hidden !== false && this.threadEmojiAutocomplete?.popup.hidden !== false) { event.preventDefault(); this.threadForm.requestSubmit(); }
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
    this.threadEmojiButton = document.querySelector('#thread-emoji-button'); this.threadEmojiButton.onclick = () => openEmojiPicker({ anchor: this.threadEmojiButton, onPick: glyph => this.insertEmoji(glyph, this.threadDraft) });
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
    this.list.dataset.empty = String(!this.list.querySelector('article.chat-msg'));
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
    this.setReply(null); this.closeThread(); this.renderPending(); this.draft.value = ''; this.recall.index = null; this.resizeDraft();
    this.lightbox.close();
    this.quickCommands.reset(); this.macroAutocomplete.reset(); this.mentionAutocomplete.reset(); this.threadMentionAutocomplete.reset(); this.emojiAutocomplete.reset(); this.threadEmojiAutocomplete.reset(); closeEmojiPicker(false); this.mentionIndex = null; this.mentionLogins = []; this.mentionLoginsRoom = ''; this.mentionLoginsAt = 0; this.mentionLoginsSeq = (this.mentionLoginsSeq || 0) + 1;
    for (const button of document.querySelectorAll('.room-controls button')) button.disabled = true;
    this.setupPending = false; this.form.inert = false; this.draft.placeholder = 'Message'; this.formatting.setPlaceholder('Message');
    this.draft.disabled = true; this.emojiButton.disabled = true; this.attachmentInput.disabled = true; this.form.querySelector('button[type=submit]').disabled = true;
  }
  setSetupPending(pending) {
    this.setupPending = !!pending;
    this.form.inert = this.setupPending;
    const placeholder = this.setupPending ? 'Finish setup to chat' : 'Message';
    this.draft.placeholder = placeholder; this.formatting.setPlaceholder(placeholder);
    if (this.room) this.setRoomFacts(this.room);
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
  noteRow(message) { if (this.list.dataset.empty === 'true') this.list.dataset.empty = 'false'; }
  listAtBottom() { return this.list.scrollHeight - this.list.scrollTop - this.list.clientHeight < 48; }
  listBox() { const box = this.list.getBoundingClientRect(); return { top: box.top, bottom: box.bottom }; }
  rowBoxes() {
    return this.renderedRows().map(node => ({ id: node.dataset.messageId, seq: Number(node.dataset.seq), get top() { return node.getBoundingClientRect().top; }, get bottom() { return node.getBoundingClientRect().bottom; } }));
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
    const row = target.closest?.('article.chat-msg, article.thread-entry'); if (!row || !container.contains(row)) return;
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
    else if (is('thread-chip')) this.openConversation(message);
    else if (is('reaction-pill')) this.toggleReaction(message.id, control.dataset.emoji);
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
        this.composedAnswerID = message.id;
        this.draft.setSelectionRange?.(text.length, text.length);
      },
    };
  }
  updateAnswerControls(items) { return this.store.updateAnswerControls(items); }
  // Inserts at the caret through execCommand so the edit stays undoable; the range assignment is the fallback.
  insertEmoji(glyph, draft = this.draft) {
    if (draft.disabled) return;
    draft.focus();
    if (!document.execCommand?.('insertText', false, glyph)) { draft.setRangeText(glyph, draft.selectionStart, draft.selectionEnd, 'end'); draft.dispatchEvent(new Event('input', { bubbles: true })); }
  }
  resizeDraft() { fitDraft(this.draft); }
  resizeThreadDraft() { fitDraft(this.threadDraft); }
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
    const writable = open && !this.setupPending && (!viewer || (room.kind === 'dm' && !agentChat));
    this.draft.disabled = !writable; this.emojiButton.disabled = !writable;
    this.form.querySelector('button[type=submit]').disabled = !writable || this.pendingUploads > 0;
    this.attachmentInput.disabled = !writable;
    this.threadDraft.disabled = !writable; this.threadEmojiButton.disabled = !writable; this.threadAttachmentInput.disabled = !writable;
    this.threadForm.querySelector('button[type=submit], button:not([type])').disabled = !writable;
    const spawn = document.querySelector('#spawn-open');
    spawn.disabled = !open || room.kind === 'dm' || viewer;
    this.spawnTitle ||= spawn.title;
    spawn.title = viewer ? 'Viewers cannot spawn agents' : this.spawnTitle;
    const note = this.viewerNote ||= (() => { const control = new Control(RoomNote, { id: 'composer-viewer-note', text: '', hidden: true }); this.form.before(control.host); return control; })();
    note.set({ hidden: !(open && viewer && (room.kind !== 'dm' || agentChat)), text: agentChat ? 'You can read this chat. Writing here takes the owner or operator role in one of the agents\' projects.' : 'You can read this room as a viewer. Only owners and operators of the project can write here or run agents.' });
    // An archived project is read-only for everyone; the composer says why and where to undo it.
    const archivedNote = this.archivedNote ||= (() => { const control = new Control(RoomNote, { id: 'composer-archived-note', text: '', hidden: true }); this.form.before(control.host); return control; })();
    archivedNote.set({ hidden: !archivedProject, text: 'This project is archived and read-only. The owner or an admin can reopen it in the project\'s home-room settings.' });
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
    const host = document.createElement('div'); host.style.display = 'contents'; // takes no part in the layout: the strip is the box the page's styles see
    render(html`<${AttachmentStrip} attachments=${attachments} />`, host);
    element.append(host);
  }
  addFiles(files) { return this.store.addFiles(files); }
  renderThreadPending() {
    this.threadPendingView.update({ attachments: this.store.threadPending, uploads: this.store.threadUploads, onRemove: attachment => this.store.removeThreadAttachment(attachment) });
    this.threadForm.querySelector('button[type=submit], button:not([type])').disabled = this.setupPending || this.store.threadUploads > 0 || this.room?.lifecycle !== 'open';
  }
  renderPending() {
    this.pendingView.update({ attachments: this.pendingAttachments, uploads: this.pendingUploads, onRemove: attachment => this.store.removeAttachment(attachment) });
    this.form.querySelector('button[type=submit]').disabled = this.setupPending || this.pendingUploads > 0 || this.room?.lifecycle !== 'open';
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
    if (message.author_kind === 'user') return message.author_name || (message.author_id === currentUser && currentLogin) || (String(message.author_id).startsWith('usr_') ? 'Someone' : message.author_id); // your own messages carry your name like anyone's, not "You" // never the raw account id
    const agent = this.agents.get(message.author_id);
    if (agent?.name || agent?.slug) return agentLabel(agent);
    // An agent that was removed: the server sends the name it had (and its project), and the row says so.
    if (message.author_kind === 'agent' && message.author_name) return `${agentLabel({ slug: message.author_name, project_id: message.author_project_id })} (removed)`;
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
  // The viewer's own messages in this room, oldest first, consecutive duplicates collapsed; in the thread panel, those of the open conversation.
  recallCandidates(thread = false) { return ownMessages(thread ? this.store.threadEntries : this.messages.values(), currentUser); }
  // Terminal-style history: Up at an empty composer or caret start walks back through own messages (once recalling, on the first line),
  // Down walks forward (restoring the in-progress draft past the newest), Esc restores the draft.
  // Returns true when the key was consumed. Open pickers (mentions, macros, commands, emoji) keep their own arrow keys.
  recallKey(event, thread = false) {
    if (event.defaultPrevented || event.isComposing || event.shiftKey || event.ctrlKey || event.altKey || event.metaKey) return false;
    const popups = thread ? [this.threadMentionAutocomplete, this.threadEmojiAutocomplete] : [this.quickCommands, this.macroAutocomplete, this.mentionAutocomplete, this.emojiAutocomplete];
    if (!popups.every(popup => popup?.popup.hidden !== false)) return false;
    const draft = thread ? this.threadDraft : this.draft, formatting = thread ? this.threadFormatting : this.formatting, input = formatting.input, recall = thread ? this.threadRecall : this.recall;
    const walking = event.key === 'ArrowUp' || event.key === 'ArrowDown';
    const step = recallStep({ key: event.key, value: input.value, at: input.selectionStart, end: input.selectionEnd }, recall, walking ? this.recallCandidates(thread) : []);
    if (!step) return false;
    event.preventDefault();
    // The draft is kept as the editor's own document and put back as it was, so formatting in it survives a walk; a recalled message is loaded as Markdown, as formatting.
    if (step.index === null) {
      const saved = recall.saved; recall.saved = null;
      if (saved) formatting.restoreDraft(saved); else formatting.loadMarkdown(step.value);
    } else {
      if (recall.index === null) recall.saved = formatting.saveDraft();
      formatting.loadMarkdown(step.value);
    }
    recall.index = step.index; recall.draft = step.draft;
    if (!formatting.editor) input.setSelectionRange(step.value.length, step.value.length);
    if (thread) this.resizeThreadDraft(); else this.resizeDraft();
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
  // The line a reply quotes, as the row shows it: the author named by the same rule as a message header (authorName), looked up now so a name that became known later shows.
  quoteView(message) {
    if (!message.reply_to) return null;
    const parent = this.quotes.get(message.id);
    if (parent === undefined) return null;
    if (!parent) return { text: '\u21aa Earlier message', title: undefined };
    return { text: `\u21aa #${parent.no} ${this.authorName(parent.from)}: ${parent.snippet}`.trimEnd().replace(/:$/, ''), title: `Go to #${parent.no}` };
  }
  // What a row shows that is not in its record: the author as the page knows it, the answer state, and the keys that make the row draw again.
  rowView(message, main = false) {
    const agent = this.agents.get(message.author_id), kind = senderKind(message, agent), live = kind === 'live' && message.author_kind === 'agent';
    const author = { name: this.authorName(message), kind, link: kind !== 'live' || live, title: live ? 'Open profile' : `${kind === 'historical' ? 'Historical sender' : 'Departed agent'}: open profile`,
      identity: senderIdentity(message, agent), roleEmoji: agent?.role_emoji || '', roleTitle: agent?.role || '' };
    const answer = this.answerView(message), bodyKey = `${this.raw}|${this.mentionIndex?.signature ?? ''}`;
    const view = { author, answer, bodyKey, raw: this.raw, mentionKey: this.mentionIndex?.signature ?? '', usageTitle: message.usage ? usageTooltip(message.usage) : '', needsAttention: !!message.needs_attention };
    view.pills = groupReactions(this.reactions.get(message.id) || [], currentUser); view.quote = this.quoteView(message);
    view.articleClass = articleClass(message, currentUser); // the panel's entries wear the room's row classes, so one set of rules styles both
    let extra = `|${JSON.stringify(view.pills)}`;
    // The quote's text (author name included) is part of the draw key, so a name that becomes known later redraws the row.
    if (view.quote) extra += `|${view.quote.text}`;
    if (main) {
      const count = threadCount(this.threadCounts, this.serverThreadCounts, message.id);
      view.chip = threadChip(count); view.threadLabel = this.threadLabel(message.id);
      view.expired = this.expiry.expired.has(message.id); view.boundary = this.expiry.divider === message.id;
      extra += `|${count}|${view.expired}|${view.boundary}`;
    }
    view.key = `${this.agentSigs?.get(message.author_id) ?? ''}|${bodyKey}|${answer.key}${extra}`;
    return view;
  }
  answerView(message) {
    const id = message.id, item = this.answerItems?.get(id);
    if (answerEligible(item)) { return { kind: 'controls', item, parent: message, key: `c:${item.id}:${item.rev}` }; }
    const repliedByOther = !message.answer_state && this.store.repliedByOther(message);
    const answered = Boolean(message.answer_state === 'resolved' || this.answeredMessages.has(id) || repliedByOther);
    // The quiet "Answered" is for a message that asked for an answer (it needed attention, or its question was answered here); an ordinary message that
    // somebody replied to is not "answered".
    const doneOk = Boolean(this.answeredMessages.has(id) || (repliedByOther && message.needs_attention)), area = answerArea(item, message, doneOk);
    if (area === 'history') return { kind: 'history', answered, key: `h:${answered}:${message.answer_body || ''}` };
    if (message.answer_state === 'superseded') return { kind: 'done', answered: false, key: 'd:superseded', label: 'No longer pending' };
    if (area === 'done') return { kind: 'done', answered, key: 'd:answered', label: 'Answered' };
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
  openConversation(message) { return this.store.openConversation(message); }
  loadOlderThread() { return this.store.loadOlderThread(); }
  sendThread() { return this.store.sendThread(); }
  send() { return this.store.send(); }
  reopenLatest() { return this.store.reopenLatest(); }
  closeThread() { return this.store.closeThread(); }
  threadClosed() {
    this.threadNotice.clear(); this.threadRoot = null; this.threadTitle.textContent = 'Replies'; this.threadPin.hidden = true; this.threadUpdate(); this.threadOlderStatus.hidden = true; this.threadList.dataset.hasOlder = 'false';
    this.threadLatestButton.hidden = true; this.threadNewButton.hidden = true; this.treeNewIDs.clear(); this.updateTreeNew(); this.threadPanel.hidden = true; this.threadDraft.value = ''; this.threadRecall.index = null; this.resizeThreadDraft(); document.querySelector('#thread-error').textContent = '';
  }
  // The one-line stand-in for the root while its own entry is scrolled out of view (an overlay on the list's top edge, so it moves nothing).
  updateThreadPin() {
    const root = this.threadRoot, entry = this.threadList.firstElementChild;
    // Out of view: its entry is scrolled past, or the panel shows an older window that does not hold the root at all.
    const out = !!root && !!entry && (entry.dataset.messageId !== root.id || this.threadList.scrollTop > entry.offsetTop + entry.offsetHeight - 4);
    if (out) { this.threadPin.textContent = `${this.authorName(root)} · #${root.no}: ${messageSnippet(root)}`; this.threadPin.style.top = `${this.threadList.offsetTop}px`; }
    this.threadPin.hidden = !out;
  }
  setReply(message) { this.composedAnswerID = null; this.reply = message; this.chip.hidden = !message; this.chipText.textContent = message ? `Replying to ${this.authorName(message)}: ${messageSnippet(message)}` : ''; if (message) this.draft.focus(); }
  // Whether a reply to `parent` with this text is the answer to `item` (see send): a question or approval this person decides, answered with text
  // that mentions at most the requester. A structured ask accepts a draft explicitly started from its controls.
  answersThroughInbox(item, parent, body) { return answersThrough(item, this.answerOptions(parent).mention, body, this.composedAnswerID === parent.id); }
  beginThread(root, focus) {
    this.threadRoot = root; this.threadTitle.textContent = 'Replies'; this.threadPanel.hidden = false; this.threadPin.hidden = true; this.threadUpdate();
    this.threadOlderStatus.hidden = true; this.threadList.dataset.hasOlder = 'false'; this.threadLatestButton.hidden = !focus; this.threadNewButton.hidden = true; this.treeNewIDs.clear(); this.updateTreeNew();
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
    // Older replies can land anywhere among the rows of the tree (above or below what is in view), so the row held is the first one in view after the first message, which stays
    // at the top.
    const top = this.threadList.getBoundingClientRect().top;
    const rows = [...this.threadList.children].filter(row => row.matches?.('article.thread-entry')).slice(1);
    const anchor = rows.find(row => row.getBoundingClientRect().bottom > top) || rows[0], anchorTop = anchor?.getBoundingClientRect().top;
    this.threadList.dataset.hasOlder = String(this.threadHasMoreOlder);
    return () => { if (anchor?.isConnected) this.threadList.scrollTop += anchor.getBoundingClientRect().top - anchorTop; };
  }
  beginThreadSend() { document.querySelector('#thread-error').textContent = ''; this.threadNotice.clear(); }
  sentThreadReply() { this.threadDraft.value = ''; this.threadRecall.index = null; this.resizeThreadDraft(); }
  threadToEnd() { this.threadList.scrollTop = this.threadList.scrollHeight; }
  // A reply the viewer has just sent: it sits under the one it answers, which is not the end of the panel, so the panel goes to it and it is highlighted.
  threadShowSent(message) {
    const row = this.threadList.querySelector(`:scope > [data-message-id="${CSS.escape(message.id)}"]`);
    if (!row) { this.threadToEnd(); return; }
    row.scrollIntoView({ block: 'nearest' }); this.flash(row);
  }
  // A reply that came in live (not the viewer's own, and not one that opens a page): it sits under its parent, which is usually not the end of the panel, so a reader looking
  // elsewhere would not see it. It is marked with the pill, and highlighted when it came in where the reader can see it.
  threadInserted(message) {
    if (message.author_kind === 'user' && message.author_id === currentUser) return;
    const { order } = treeLayout(this.threadEntries);
    if (order.at(-1)?.id === message.id) return; // at the end, where a live reply always was
    this.treeNewIDs.add(message.id);
    // Looked at once the page has drawn and the panel's own scroll has settled (a row inserted above the reader moves what is in view).
    requestAnimationFrame(() => requestAnimationFrame(() => this.updateTreeNew({ arrived: message.id })));
  }
  // Which of the replies marked new are still out of view, and which way the first of them is: the pill shows the count and the direction, and goes when none is left.
  updateTreeNew({ arrived = '' } = {}) {
    const list = this.threadList, box = list.getBoundingClientRect(), out = [];
    for (const id of [...this.treeNewIDs]) {
      const row = list.querySelector(`:scope > [data-message-id="${CSS.escape(id)}"]`);
      if (!row) { this.treeNewIDs.delete(id); continue; }
      const at = row.getBoundingClientRect();
      if (at.top >= box.top && at.bottom <= box.bottom) { this.treeNewIDs.delete(id); if (id === arrived) this.flash(row); continue; }
      out.push({ id, above: at.top < box.top });
    }
    const first = [...list.querySelectorAll(':scope > article.thread-entry')].find(row => this.treeNewIDs.has(row.dataset.messageId));
    const dir = first && out.find(item => item.id === first.dataset.messageId)?.above === false ? 'down' : 'up', count = this.treeNewIDs.size;
    this.threadTreeButton.set({ label: count ? `${newRepliesLabel(count)} in the conversation ${dir === 'up' ? 'above' : 'below'}` : 'New replies in the conversation', count, dir, hidden: count === 0 });
  }
  showTreeNew() {
    const first = [...this.threadList.querySelectorAll(':scope > article.thread-entry')].find(row => this.treeNewIDs.has(row.dataset.messageId));
    if (!first) { this.treeNewIDs.clear(); this.updateTreeNew(); return; }
    this.treeNewIDs.delete(first.dataset.messageId);
    first.scrollIntoView({ block: 'center' }); this.flash(first);
    this.updateTreeNew();
  }
  threadSendFailed(message) { this.threadNotice.show(message); }
  restoreThreadDraft(draft) { this.threadDraft.value = draft; this.resizeThreadDraft(); this.draft.focus(); }
  // Whether the person has typed or attached something that is not sent: the message and the thread reply, whichever way the composer is
  // showing them (the formatted editor keeps the textarea in step), and files waiting to go with the message.
  hasDraft() { return !!(this.draft.value.trim() || this.threadDraft.value.trim() || this.pendingAttachments?.length || this.pendingUploads || this.store.threadPending?.length || this.store.threadUploads); }
  sendInput() {
    if (this.pendingUploads || (!this.draft.value.trim() && !this.pendingAttachments.length)) return null;
    return { body: this.draft.value, reply: this.reply, attachments: this.pendingAttachments.map(a => a.id) };
  }
  beginSend() { this.error.textContent = ''; this.notice.clear(); this.announcer.sendStarted(); }
  async sendAsAnswer(reply, attachments, body, current) {
    // A reply resolves a plain free-text ask, or a structured draft explicitly started from its controls.
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
