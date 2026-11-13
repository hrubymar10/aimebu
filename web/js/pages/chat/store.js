import { request, upload, currentUser } from '../../api.js';
import { megabytes } from '../../notice.js';
import { expiryCutoffMs, planExpiry } from '../../expiry.js';
import { retention } from '../../retention.js';
import { answerEligible, liveArrival, liveRoute, messageSnippet, overCap, threadCount, trimCap } from './model.js';

// The room's messages and the calls that load them; `host` draws. A call says nothing and starts nothing once its generation or the store is gone.
export class ChatStore {
  constructor(host) {
    this.host = host; this.alive = true;
    this.openGeneration = 0; this.sessionGeneration = 0; this.jumpGeneration = 0; this.pageSize = 100; this.openPageSize = globalThis.__openPageSize || 25; this.olderPageSize = 50;
    this.threadGeneration = 0; this.threadOldestSeq = 0; this.threadHasMoreOlder = false; this.loadingOlderThread = false; this.threadPageSize = 100; this.threadOlderPageSize = 50;
    this.initialLoading = false; this.announcedIDs = null; this.reactionTickets = new Map(); this.prefetched = null;
    this.pendingAttachments = []; this.pendingUploads = 0; this.uploadRequests = new Set(); this.threadPending = []; this.threadUploads = 0;
    this.rows = []; this.rowIDs = new Set(); this.quotes = new Map(); this.rowBatching = false; this.rowPending = []; this.expiry = { expired: new Set(), divider: null }; this.expiryQueued = false;
    this.threadEntries = []; this.threadEntryIDs = new Set(); this.threadBatching = false; this.threadNewReplyIDs = new Set(); this.answerItems = new Map();
    this.clearRoom();
  }

  clearRoom() {
    // repliedTo: parent id -> the ids of the people who replied to it.
    this.answeredMessages = new Set(); this.repliedTo = new Map(); this.messages = new Map(); this.historicalAuthors = new Set(); this.reactions = new Map();
    this.threadCounts = new Map(); this.counted = new Set(); this.serverThreadCounts = new Map(); this.missedIDs = new Set(); this.announcedIDs = null;
    this.newestSeq = 0; this.oldestSeq = 0; this.hasMoreOlder = false; this.loadingOlder = false; this.loadingNewer = false; this.contextMode = false; this.catchingUp = false; this.windowSeq = 0;
  }

  current(session, roomID) { return session === this.sessionGeneration && this.host.room?.id === roomID; }

  reset() {
    for (const controller of this.uploadRequests) controller.abort();
    this.uploadRequests.clear(); this.pendingAttachments = []; this.pendingUploads = 0; this.answerItems = new Map();
    this.openGeneration++; this.sessionGeneration++; this.threadGeneration++; this.prefetched = null; this.clearRoom(); this.clearRows(); this.initialLoading = false; }

  destroy() { this.reset(); this.alive = false; }

  // Starts reading the newest page of a room that is likely to be opened next (sign-in does this beside its other first reads, so the room's page
  // does not wait for the lists that tell the app which room to open). Only open() of that very room takes it, and only once; a failure is
  // dropped and open() asks again.
  prefetchLatest(roomID) {
    this.prefetched = null;
    if (!roomID || !this.alive) return;
    const page = request(`/api/rooms/${encodeURIComponent(roomID)}/messages?limit=${this.openPageSize}`);
    page.catch(() => {});
    this.prefetched = { roomID, page, at: Date.now() };
  }
  takePrefetched(roomID) {
    const held = this.prefetched; this.prefetched = null;
    return held && held.roomID === roomID && Date.now() - held.at < 30000 ? held.page.catch(() => null) : null;
  }

  async open(room) {
    if (!this.alive) return;
    const generation = ++this.openGeneration;
    if (this.host.room?.id !== room.id) this.pendingAttachments = [];
    this.clearRoom(); this.initialLoading = true; this.clearRows(); this.closeThread();
    this.host.beginOpen(room);
    const messages = (await this.takePrefetched(room.id)) || await request(`/api/rooms/${room.id}/messages?limit=${this.openPageSize}`);
    if (generation !== this.openGeneration) return;
    this.batchRows(() => { for (const message of messages) this.add(message); });
    this.host.pageDrawn();
    this.newestSeq = messages.at(-1)?.seq || 0;
    this.oldestSeq = messages[0]?.seq || 0;
    this.hasMoreOlder = messages.length === this.openPageSize;
    this.host.showNewest();
    this.initialLoading = false; this.host.loadOlderIfShort();
    this.host.onLatest?.(room);
    await this.refresh();
  }

  // `quiet`: the messages are a catch-up after a gap, taken in without announcing each one to a screen reader.
  async refresh({ quiet = false } = {}) {
    if (!this.alive || !this.host.room || this.initialLoading) return;
    const id = this.host.room.id, generation = this.openGeneration;
    for (;;) {
      const messages = await request(`/api/rooms/${id}/messages?after=${this.newestSeq || 0}&limit=${this.pageSize}`);
      if (generation !== this.openGeneration) return;
      for (const message of messages) {
        const route = liveRoute(message, { hasMoreOlder: this.hasMoreOlder, oldestSeq: this.oldestSeq, contextMode: this.contextMode, known: this.messages.has(message.id), missed: this.missedIDs.has(message.id), ...this.threadState(message) });
        if (route === 'new-reply') { this.markNewThreadReply(message); if (!this.contextMode) this.add(message); }
        else if (route === 'panel') this.add(message, false, false, true);
        else if (route === 'add') { const known = this.messages.has(message.id); this.add(message); if (!known && !quiet && liveArrival(message, this) && !this.announcedIDs?.has(message.id)) this.host.announce(message); }
        else if (route === 'missed') {
          this.missedIDs.add(message.id); this.host.updateLatestLabel();
          if (!this.initialLoading && !quiet && message.author_kind !== 'system') this.host.announce(message);
        }
      }
      if (messages.length) this.newestSeq = messages.at(-1).seq;
      if (messages.length) this.trimLive();
      // The project refresh for the same event ran before this render and could only mark up to the previous newest message.
      if (messages.length && !this.contextMode && !this.catchingUp) this.host.onLatest?.(this.host.room);
      if (messages.length < this.pageSize) return;
    }
  }

  // Brings the open room up to date after a gap (a reconnect) and keeps where the reader is: the history that is loaded, the scroll position and
  // the open thread stay as they are, and nothing is opened again. New messages come in as they would live. What is on screen is read again for
  // its reactions and thread counts, which have no message of their own to catch up on: the rendered span, from its oldest message, page by page.
  async syncInPlace() {
    if (!this.alive || !this.host.room || this.initialLoading) return;
    const generation = this.openGeneration, id = this.host.room.id;
    await this.refresh({ quiet: true });
    if (generation !== this.openGeneration) return;
    let after = Infinity;
    for (const message of this.messages.values()) after = Math.min(after, message.seq);
    after = Number.isFinite(after) ? after - 1 : 0;
    for (let page = 0; page < 12; page++) {
      const messages = await request(`/api/rooms/${id}/messages?after=${after}&limit=${this.pageSize}`);
      if (generation !== this.openGeneration) return;
      for (const message of messages) {
        if (!this.messages.has(message.id)) continue; // new ones came with the refresh; a gap in what is on screen stays a gap
        this.add(message); // a known message takes only its thread count here
        if (Array.isArray(message.reactions) && JSON.stringify(message.reactions) !== JSON.stringify(this.reactions.get(message.id))) this.showReactions(message.id, message.reactions);
      }
      if (messages.length < this.pageSize) break;
      after = messages.at(-1).seq;
    }
  }

  async loadOlder() {
    if (!this.alive || !this.hasMoreOlder || !this.oldestSeq || this.loadingOlder) return;
    const roomID = this.host.room.id, generation = this.openGeneration;
    this.loadingOlder = true; this.host.showOlderStatus(true);
    try {
      const messages = await request(`/api/rooms/${roomID}/messages?before=${this.oldestSeq}&limit=${this.olderPageSize}`);
      if (generation !== this.openGeneration) return;
      if (messages.length) this.oldestSeq = messages[0].seq;
      this.hasMoreOlder = messages.length === this.olderPageSize;
      const place = this.host.holdPlace();
      this.host.hasOlder(this.hasMoreOlder);
      this.batchRows(() => { for (let i = messages.length - 1; i >= 0; i--) this.add(messages[i], true); }, place);
      this.trimNewest();
    } catch (error) { if (generation === this.openGeneration) this.host.say(error.message); }
    finally { if (generation === this.openGeneration) { this.loadingOlder = false; this.host.showOlderStatus(false); this.host.loadOlderIfShort(); } }
  }

  async loadNewer() {
    if (!this.alive || !this.windowSeq || this.loadingNewer || this.loadingOlder) return;
    const roomID = this.host.room.id, generation = this.openGeneration;
    this.loadingNewer = true;
    try {
      const messages = await request(`/api/rooms/${roomID}/messages?after=${this.windowSeq}&limit=${this.olderPageSize}`);
      if (generation !== this.openGeneration || !this.contextMode) return;
      for (const message of messages) this.add(message, false, true);
      if (messages.length) this.windowSeq = messages.at(-1).seq;
      // A short page is the live end of the room: the window is the latest view again.
      if (messages.length < this.olderPageSize) { await this.leaveWindow(); return; }
      this.trimOldest();
      this.host.anchorUpdate();
    } catch (error) { if (generation === this.openGeneration) this.host.say(error.message); }
    finally { if (generation === this.openGeneration) this.loadingNewer = false; }
  }

  // The cursor restarts at the window's end: arrivals counted while the last page was in flight were not rendered, and this refresh renders them.
  async leaveWindow() {
    if (!this.alive) return;
    const room = this.host.room, generation = this.openGeneration, missed = this.missedIDs;
    this.newestSeq = this.windowSeq;
    this.announcedIDs = missed;
    // Not the latest view for the read mark until the catch-up has rendered (see holdRead in app.js).
    this.catchingUp = true;
    this.contextMode = false; this.windowSeq = 0; this.host.showLatestButton(false);
    this.missedIDs = new Set(); this.host.updateLatestLabel();
    this.host.anchorUpdate();
    let caughtUp = true;
    try { await this.refresh(); } catch (error) { caughtUp = false; if (generation === this.openGeneration) this.host.say(error.message); }
    finally { if (generation === this.openGeneration) this.announcedIDs = null; }
    if (generation !== this.openGeneration || this.contextMode) return;
    if (!caughtUp) {
      this.contextMode = true; this.catchingUp = false; this.windowSeq = this.newestSeq; this.host.showLatestButton(true);
      for (const id of missed) if (!this.messages.has(id)) this.missedIDs.add(id);
      this.host.updateLatestLabel(); return;
    }
    this.catchingUp = false;
    this.host.onLatest?.(room);
  }

  // `key` is a message number (one sequence across every room) or a message id; a number of another room opens that room on that message.
  async jump(key) {
    if (!this.alive || !this.host.room) return;
    if (typeof key === 'number') {
      const lookup = this.openGeneration;
      const loaded = [...this.messages.values()].find(item => item.no === key);
      if (loaded) key = loaded.id;
      else {
        let found;
        try { found = await request(`/api/message-numbers/${encodeURIComponent(key)}`); }
        catch (error) { if (lookup === this.openGeneration) this.host.say(error.status === 404 ? `Message #${key} does not exist or is not visible to you.` : error.message); return; }
        if (lookup !== this.openGeneration) return;
        if (found.room_id !== this.host.room.id) { this.host.say(''); this.host.onOpenMessage?.(found); return; }
        key = found.id;
      }
    }
    const roomID = this.host.room.id;
    let generation = this.openGeneration;
    const jump = ++this.jumpGeneration;
    let message = this.messages.get(key);
    try {
      // A message the panel loaded on its own has no row (it may be outside the window): it is looked up like one not loaded.
      if (!message || !this.rowIDs.has(message.id)) {
        const messages = await request(`/api/rooms/${roomID}/messages?around=${encodeURIComponent(key)}&limit=${this.pageSize}`);
        if (generation !== this.openGeneration || jump !== this.jumpGeneration) return;
        message = messages.find(item => item.id === key);
        if (!message) throw new Error('Message is not in this room.');
        generation = ++this.openGeneration; this.announcedIDs = null;
        // A page in flight belongs to the old view and will not clear its own flag.
        this.hasMoreOlder = false; this.loadingOlder = false; this.loadingNewer = false; this.catchingUp = false; this.oldestSeq = 0;
        this.closeThread(); this.clearRows();
        this.host.beginWindow();
        this.messages.clear(); this.historicalAuthors = new Set(); this.reactions.clear(); this.threadCounts.clear(); this.counted.clear(); this.serverThreadCounts.clear();
        this.batchRows(() => { for (const item of messages) this.add(item); });
        this.host.windowDrawn();
        this.oldestSeq = messages[0]?.seq || 0; this.hasMoreOlder = messages.length === this.pageSize;
        this.host.showWindow();
        this.contextMode = true; this.host.showLatestButton(true); this.windowSeq = messages.at(-1)?.seq || 0;
      }
      this.host.showRow(message);
      this.host.say('');
    } catch (error) { if (generation === this.openGeneration && jump === this.jumpGeneration) this.host.say(error.message); }
  }

  async refreshReactions(id) {
    if (!this.alive || !this.host.nodes.has(id)) return;
    const generation = this.openGeneration, ticket = (this.reactionTickets.get(id) || 0) + 1;
    this.reactionTickets.set(id, ticket);
    const entries = await request(`/api/messages/${id}/reactions`);
    if (generation !== this.openGeneration || this.reactionTickets.get(id) !== ticket) return;
    this.showReactions(id, entries);
  }

  showReactions(id, entries) {
    this.reactions.set(id, entries);
    if (this.host.nodes.has(id)) this.host.listUpdate();
  }

  async toggleReaction(id, emoji) {
    if (!this.alive || !this.host.room) return;
    const generation = this.openGeneration;
    const mine = this.reactions.get(id)?.some(entry => entry.emoji === emoji && entry.member_id === currentUser);
    try { await request(`/api/messages/${id}/reactions/${encodeURIComponent(emoji)}`, { method: mine ? 'DELETE' : 'PUT' }); if (generation !== this.openGeneration) return; await this.refreshReactions(id); }
    catch (error) { if (generation === this.openGeneration) this.host.say(error.message); }
  }

  // Expiry marks (an hourglass and one divider) are display-only: the chat shows every message. They are
  // recomputed once per task after a burst of add() calls, when the window changes, and every 30 s.
  scheduleExpiry() {
    if (this.expiryQueued) return;
    this.expiryQueued = true;
    queueMicrotask(() => { this.expiryQueued = false; this.syncExpiry(); });
  }

  syncExpiry() {
    if (!this.alive) return;
    const cutoff = expiryCutoffMs(Date.now(), retention.seconds);
    const plan = planExpiry(this.rows.map(message => ({ id: message.id, createdMs: Date.parse(message.created_at), expirable: message.expirable === true })), cutoff);
    const same = plan.expired.size === this.expiry.expired.size && [...plan.expired].every(id => this.expiry.expired.has(id));
    if (same && (plan.divider || null) === this.expiry.divider) return;
    this.expiry = { expired: plan.expired, divider: plan.divider || null };
    this.host.listUpdate();
  }

  clearRows() { this.rows = []; this.rowIDs.clear(); this.quotes.clear(); this.expiry = { expired: new Set(), divider: null }; this.host.listUpdate(); }

  batchRows(fill, restore) {
    this.rowBatching = true; this.rowPending = [];
    try { fill(); } finally { this.rowBatching = false; }
    this.host.listUpdate(restore);
    const pending = this.rowPending; this.rowPending = [];
    for (const id of pending) Promise.resolve(this.refreshReactions(id)).catch(() => {});
    this.scheduleExpiry();
  }

  // A prepended (older) or paged (newer, in a history window) message leaves the scroll position alone; only a live append follows the bottom.
  // Whether anyone but the message's own author has replied to it.
  repliedByOther(message) {
    for (const author of this.repliedTo.get(message.id) || []) if (author !== message.author_id) return true;
    return false;
  }
  // Every message is a row of the room, replies included (a reply shows its quote). A message that belongs to a conversation also counts on its root and goes to the open
  // panel; `panelOnly` is for what the panel loads itself, which may lie outside the window the room shows and so gets no row.
  add(message, prepend = false, paged = false, panelOnly = false) {
    if (Number.isInteger(message.thread_count)) {
      const rootID = message.thread_root || message.id;
      this.serverThreadCounts.set(rootID, Math.max(this.serverThreadCounts.get(rootID) || 0, message.thread_count));
      this.updateThreadCount(rootID);
    }
    if (!this.messages.has(message.id)) {
      this.noteAuthor(message);
      this.messages.set(message.id, message);
      // Keep reply authors for older messages that have no durable Inbox state. A newer reply does not resolve a structured Inbox request.
      if (message.reply_to && message.author_kind === 'user') {
        if (!this.repliedTo.has(message.reply_to)) this.repliedTo.set(message.reply_to, new Set());
        this.repliedTo.get(message.reply_to).add(message.author_id);
      }
      if (this.missedIDs.delete(message.id)) this.host.updateLatestLabel();
      if (message.thread_root && !this.counted.has(message.id)) {
        this.counted.add(message.id);
        const count = (this.threadCounts.get(message.thread_root) || 0) + 1;
        this.threadCounts.set(message.thread_root, count);
        this.updateThreadCount(message.thread_root);
        // The thread panel pages and refreshes itself; a forward page of the main list could land outside the replies it shows.
        if (this.host.threadRoot?.id === message.thread_root && !paged) this.insertThreadEntry(message);
      }
      if (message.reply_to) this.quotes.set(message.id, this.quoteFor(message));
    }
    if ((message.thread_root && panelOnly) || this.rowIDs.has(message.id)) return;
    this.rowIDs.add(message.id);
    const quiet = prepend || paged || this.rowBatching;
    const place = quiet ? null : this.host.listPlace();
    this.host.noteRow(message);
    if (Array.isArray(message.reactions)) this.reactions.set(message.id, message.reactions);
    if (answerEligible(this.answerItems.get(message.id))) this.answeredMessages.delete(message.id);
    this.rows = prepend ? [message, ...this.rows] : [...this.rows, message];
    if (this.rowBatching) { if (!Array.isArray(message.reactions)) this.rowPending.push(message.id); }
    else {
      this.host.listUpdate();
      if (place) this.host.followList(place);
      if (!Array.isArray(message.reactions)) Promise.resolve(this.refreshReactions(message.id)).catch(() => {});
    }
    this.scheduleExpiry();
  }

  // What a reply quotes, read once when its row is made: the parent's number and snippet, and who wrote it (the loaded parent, or the preview that came with the reply
  // for one outside the loaded window). The author's NAME is not kept: the page resolves it each time the row is drawn (chat.js quoteView), as a message header does, so
  // an agent the page learns of after this row was made is named, not shown as "Agent 3F9A2B" for good.
  quoteFor(message) {
    const loaded = this.messages.get(message.reply_to), preview = message.reply_preview;
    if (loaded) return { no: loaded.no, from: loaded, snippet: messageSnippet(loaded) };
    if (preview) return { no: preview.no, from: { author_kind: preview.author_kind, author_id: preview.author_id, author_name: preview.author_name, author_project_id: preview.author_project_id }, snippet: preview.snippet };
    return null;
  }

  // A question this viewer had open that is now gone from the inbox leaves a quiet "Answered" in its place; one that is open again loses it.
  updateAnswerControls(items) {
    const next = new Map(items.map(item => [item.message_id, item]));
    const closed = [];
    for (const [id, item] of this.answerItems) if (!next.has(id) && answerEligible(item) && (this.host.nodes.has(id) || this.threadEntryIDs.has(id))) {
      this.answeredMessages.add(id);
      closed.push(id);
    }
    for (const [id, item] of next) if (answerEligible(item)) this.answeredMessages.delete(id);
    this.answerItems = next;
    this.host.listUpdate(); this.host.threadUpdate();
    for (const id of closed) void this.refreshAnswerState(id);
  }

  async refreshAnswerState(id) {
    const roomID = this.host.room?.id, generation = this.openGeneration;
    if (!roomID) return;
    try {
      const page = await request(`/api/rooms/${encodeURIComponent(roomID)}/messages?around=${encodeURIComponent(id)}&limit=1`);
      if (generation !== this.openGeneration || this.host.room?.id !== roomID) return;
      const message = page.find(entry => entry.id === id);
      if (!message) return;
      const prior = this.messages.get(id);
      if (prior?.answer_state === message.answer_state && prior?.answer_body === message.answer_body) return;
      this.messages.set(id, message);
      this.rows = this.rows.map(entry => entry.id === id ? message : entry);
      this.threadEntries = this.threadEntries.map(entry => entry.id === id ? message : entry);
      this.answeredMessages.delete(id);
      this.host.listUpdate(); this.host.threadUpdate();
    } catch { /* A later room load will read the durable answer state. */ }
  }

  // An agent that wrote in this room and is no longer a member (or came from the v1 import) still reads as @name in message bodies:
  // its slug and its full id are remembered per room, and the highlight is redone once after a burst of messages.
  noteAuthor(message) {
    if (message.author_kind !== 'agent') return;
    const name = this.host.authorName(message), set = this.historicalAuthors;
    if (name.startsWith('Agent ')) return;
    const keys = [name, name.split('@')[0]].map(key => key.toLowerCase()).filter(key => /^[a-z][a-z0-9_-]*(@[a-z0-9_.-]+)?$/.test(key));
    let added = false; for (const key of keys) if (!set.has(key)) { set.add(key); added = true; }
    if (added) this.host.scheduleMentions();
  }

  updateThreadCount(id) {
    if (this.host.nodes.has(id)) this.host.listUpdate();
    this.host.showThreadCount(id, threadCount(this.threadCounts, this.serverThreadCounts, id));
  }

  // The panel's entries: the root first, the replies by seq. An update renders at once, so callers read the rows back.
  insertThreadEntry(message) {
    if (this.threadEntryIDs.has(message.id)) return;
    this.threadEntryIDs.add(message.id);
    let low = 1, high = this.threadEntries.length;
    while (low < high) {
      const middle = (low + high) >> 1;
      if (this.threadEntries[middle].seq < message.seq) low = middle + 1;
      else high = middle;
    }
    this.threadEntries = [...this.threadEntries.slice(0, low), message, ...this.threadEntries.slice(low)];
    if (!this.threadBatching) { this.host.threadUpdate(); this.host.threadInserted(message); }
  }

  batchThreadEntries(fill, restore) {
    this.threadBatching = true;
    try { fill(); } finally { this.threadBatching = false; }
    this.host.threadUpdate(restore);
  }

  markNewThreadReply(message) {
    this.threadNewReplyIDs.add(message.id);
    this.messages.set(message.id, message);
    if (Number.isInteger(message.thread_count)) this.serverThreadCounts.set(message.thread_root, Math.max(this.serverThreadCounts.get(message.thread_root) || 0, message.thread_count));
    this.updateThreadCount(message.thread_root);
    this.host.updateThreadNewMarker();
  }

  settleThreadLatest() {
    this.host.hideThreadLatest();
    this.batchThreadEntries(() => {
      for (const id of this.threadNewReplyIDs) {
        const message = this.messages.get(id);
        if (message) this.insertThreadEntry(message);
      }
    });
    this.threadNewReplyIDs.clear();
    this.host.updateThreadNewMarker();
  }

  // What the open thread panel knows of a live message: whether its root is drawn, whether the panel is open on it and at its end.
  threadState(message) {
    return { rootRendered: this.host.nodes.has(message.thread_root), openThreadID: this.host.threadRoot?.id, threadLatestHidden: this.host.threadLatestHidden(), threadEntryKnown: this.threadEntryIDs.has(message.id) };
  }

  // Replies kept for an open thread whose root row is not rendered (trimmed from a window, or never loaded) have no other
  // owner: when the panel lets go of that thread they are dropped with its counts, and come back with the row's page.
  releaseThread() {
    const id = this.host.threadRoot?.id;
    if (!id || this.host.nodes.has(id)) return;
    for (const [key, message] of this.messages) if (message.thread_root === id && !this.rowIDs.has(key)) { this.messages.delete(key); this.counted.delete(key); }
    this.threadCounts.delete(id); this.serverThreadCounts.delete(id);
  }

  // The files waiting to go with a reply in the thread: the same four-file rule and room endpoint as the composer's, kept apart from it. A thread
  // closed, or another one opened, drops them (the room's attachment is deleted, as a removed one is).
  discardThreadAttachments() {
    const roomID = this.host.room?.id, dropped = this.threadPending;
    this.threadPending = [];
    if (roomID) for (const attachment of dropped) request(`/api/rooms/${roomID}/attachments/${attachment.id}`, { method: 'DELETE' }).catch(() => {});
    this.host.renderThreadPending?.();
  }

  async addThreadFiles(files) {
    const room = this.host.room, root = this.host.threadRoot?.id;
    if (!this.alive || !room || !root || room.lifecycle !== 'open') return;
    this.host.threadNotice.clear();
    const roomID = room.id, session = this.sessionGeneration, thread = this.threadGeneration;
    const live = () => this.current(session, roomID) && thread === this.threadGeneration;
    for (const file of files) {
      if (!live()) return;
      const name = file.name || 'The file';
      if (this.threadPending.length + this.threadUploads >= 4) { this.host.threadNotice.show('Up to four files per message.', { append: true }); break; }
      if (!file.size) { this.host.threadNotice.show(`${name} is empty.`, { append: true }); continue; }
      if (file.size > 10 * 1024 * 1024) { this.host.threadNotice.show(`${name} is too big (${megabytes(file.size)}). Use files up to 10 MB.`, { append: true }); continue; }
      this.threadUploads++; this.host.renderThreadPending();
      try {
        const attachment = await upload(`/api/rooms/${roomID}/attachments`, file);
        if (live()) this.threadPending = [...this.threadPending, attachment];
        else await request(`/api/rooms/${roomID}/attachments/${attachment.id}`, { method: 'DELETE' }).catch(() => {});
      } catch (error) { if (live()) this.host.threadNotice.show(`${name}: ${error.message}`, { append: true }); }
      finally { if (this.alive) { this.threadUploads--; this.host.renderThreadPending(); } }
    }
  }

  async removeThreadAttachment(attachment) {
    if (!this.alive || !this.host.room) return;
    const roomID = this.host.room.id, thread = this.threadGeneration;
    try {
      await request(`/api/rooms/${roomID}/attachments/${attachment.id}`, { method: 'DELETE' });
      if (thread !== this.threadGeneration) return;
      this.threadPending = this.threadPending.filter(item => item.id !== attachment.id); this.host.renderThreadPending();
    } catch (error) { if (thread === this.threadGeneration) this.host.threadNotice.show(error.message); }
  }

  closeThread() {
    this.discardThreadAttachments();
    this.releaseThread();
    this.threadGeneration++;
    this.threadEntryIDs = new Set(); this.threadEntries = []; this.threadNewReplyIDs = new Set();
    this.threadOldestSeq = 0; this.threadHasMoreOlder = false; this.loadingOlderThread = false;
    this.host.threadClosed();
  }

  // A dropped row takes its thread replies and counts along; they come back with the row when its page is loaded again
  // (kept, they would linger in the inspector's previous/next and stay in memory). The open thread keeps its replies.
  dropRows(ids) {
    const gone = new Set(), open = this.host.threadRoot?.id;
    for (const id of ids) {
      this.messages.delete(id); this.reactions.delete(id); this.rowIDs.delete(id); gone.add(id);
      if (id !== open) { this.threadCounts.delete(id); this.serverThreadCounts.delete(id); }
    }
    for (const [id, message] of this.messages) if (message.thread_root && message.thread_root !== open && gone.has(message.thread_root) && !this.rowIDs.has(id)) { this.messages.delete(id); this.counted.delete(id); }
    this.rows = this.rows.filter(message => !gone.has(message.id));
    for (const id of gone) this.quotes.delete(id);
    this.host.listUpdate();
  }

  // After live appends: a reader following the bottom keeps the live cap, one scrolled up keeps the history cap (their rows
  // are not taken from under them, see trimOldest). The dropped rows come back by scrolling up (hasMoreOlder is set).
  // Never while an older page is in flight: it was requested before the current oldest row and would land behind a gap.
  trimLive() {
    if (this.contextMode || this.loadingOlder || this.initialLoading) return;
    if (this.trimOldest(trimCap(this.host.listAtBottom(), this.host.liveCap(), this.host.renderCap()))) this.host.anchorUpdate();
  }

  // A trim never removes what the reader is looking at: when the rows to drop reach into the visible part of the list it
  // is deferred to the next page, by which time the reader has moved on (only possible with a cap near a screenful).
  trimNewest() {
    const rows = this.host.rowBoxes(), cap = this.host.renderCap();
    if (!overCap(rows.length, cap) || this.loadingNewer) return;
    if (rows[cap].top < this.host.listBox().bottom) return;
    this.dropRows(rows.slice(cap).map(row => row.id));
    this.windowSeq = rows[cap - 1].seq;
    this.contextMode = true; this.host.showLatestButton(true);
  }

  // The mirror of trimNewest while a history window pages forward: past the cap the oldest rows go, and the list is
  // scrolled by the height they took so the reader does not move.
  // Also used by the live end (trimLive) with its own cap. Returns whether rows were dropped.
  trimOldest(cap = this.host.renderCap()) {
    const rows = this.host.rowBoxes();
    if (!overCap(rows.length, cap)) return false;
    if (rows[rows.length - cap - 1].bottom > this.host.listBox().top) return false;
    const keep = rows[rows.length - cap], settle = this.host.keepPlace(keep.id);
    this.dropRows(rows.slice(0, rows.length - cap).map(row => row.id));
    settle();
    this.oldestSeq = keep.seq; this.hasMoreOlder = true; this.host.hasOlder(true);
    return true;
  }

  // The conversation a message belongs to, in the panel: a reply opens its root's conversation, with itself in view.
  async openConversation(message) {
    if (!message.thread_root) return this.openThread(message);
    let root = this.messages.get(message.thread_root);
    if (!root) {
      const roomID = this.host.room?.id, thread = this.threadGeneration;
      try {
        const roots = await request(`/api/rooms/${roomID}/messages?around=${encodeURIComponent(message.thread_root)}&limit=1`);
        if (thread !== this.threadGeneration) return;
        root = roots.find(item => item.id === message.thread_root);
      } catch (error) { this.host.say(error.message); return; }
    }
    if (root) await this.openThread(root, message);
  }

  async openThread(root, focus = null) {
    if (!this.alive) return;
    const roomID = this.host.room?.id, thread = ++this.threadGeneration;
    if (this.host.threadRoot?.id !== root.id) { this.releaseThread(); this.discardThreadAttachments(); }
    this.threadEntries = [root]; this.threadEntryIDs = new Set([root.id]); this.threadNewReplyIDs = new Set();
    this.threadOldestSeq = 0; this.threadHasMoreOlder = false; this.loadingOlderThread = false;
    if (Number.isInteger(root.thread_count)) this.serverThreadCounts.set(root.id, Math.max(this.serverThreadCounts.get(root.id) || 0, root.thread_count));
    this.host.beginThread(root, focus);
    this.updateThreadCount(root.id);
    try {
      const context = focus ? `&around=${encodeURIComponent(focus.id)}` : '';
      const page = await request(`/api/rooms/${roomID}/messages?thread_root=${encodeURIComponent(root.id)}${context}&limit=${this.threadPageSize}`);
      if (thread !== this.threadGeneration) return;
      this.batchThreadEntries(() => { for (const message of page) { this.threadNewReplyIDs.delete(message.id); this.add(message, false, false, true); this.insertThreadEntry(message); } });
      this.host.updateThreadNewMarker();
      this.threadOldestSeq = page[0]?.seq || 0;
      this.threadHasMoreOlder = page.length === this.threadPageSize;
      this.host.showThreadPage(focus);
      const lastSeq = focus ? page.at(-1)?.seq : null;
      if (lastSeq) {
        const newer = await request(`/api/rooms/${roomID}/messages?thread_root=${encodeURIComponent(root.id)}&after=${lastSeq}&limit=1`);
        if (thread !== this.threadGeneration) return;
        if (!newer.length) this.settleThreadLatest();
      }
    } catch (error) { if (thread === this.threadGeneration) this.host.threadError(error.message); }
  }

  async loadOlderThread() {
    if (!this.alive || !this.host.threadRoot || !this.threadHasMoreOlder || !this.threadOldestSeq || this.loadingOlderThread) return;
    const roomID = this.host.room.id, rootID = this.host.threadRoot.id, thread = this.threadGeneration;
    this.loadingOlderThread = true; this.host.showThreadOlderStatus(true);
    try {
      const page = await request(`/api/rooms/${roomID}/messages?thread_root=${encodeURIComponent(rootID)}&before=${this.threadOldestSeq}&limit=${this.threadOlderPageSize}`);
      if (thread !== this.threadGeneration) return;
      if (page.length) this.threadOldestSeq = page[0].seq;
      this.threadHasMoreOlder = page.length === this.threadOlderPageSize;
      const place = this.host.holdThreadPlace();
      this.batchThreadEntries(() => { for (const message of page) { this.add(message, false, false, true); this.insertThreadEntry(message); } }, place);
    } catch (error) {
      if (thread === this.threadGeneration) this.host.threadError(error.message);
    } finally {
      if (thread === this.threadGeneration) { this.loadingOlderThread = false; this.host.showThreadOlderStatus(false); }
    }
  }

  // The message a plain send in the panel answers: the conversation's last reply that is not the sender's own (a reply to oneself addresses nobody) and not a system
  // message (nor does a reply to that), the first message when there is none. At the panel's end the newest are read from its entries; a panel showing an older window (or
  // holding replies it has not shown) asks the server for the newest 50. A page of nothing but the sender's own replies is not the end of the search: the page before it
  // is read, and so on, until one is found or the conversation is used up (at most 20 pages, a thousand replies, then the first message answers).
  async lastInThread(room, root) {
    const PAGE = 50, answerable = message => message.id !== root && message.author_kind !== 'system' && !(message.author_kind === 'user' && message.author_id === currentUser);
    const local = this.host.threadLatestHidden() && !this.threadNewReplyIDs.size;
    let page = local ? this.threadEntries : await request(`/api/rooms/${room}/messages?thread_root=${encodeURIComponent(root)}&limit=${PAGE}`);
    let found = page.findLast(answerable), more = local ? this.threadHasMoreOlder : page.length === PAGE;
    let oldest = page.find(message => message.id !== root)?.seq;
    for (let pages = 0; !found && more && oldest && pages < 20; pages++) {
      page = await request(`/api/rooms/${room}/messages?thread_root=${encodeURIComponent(root)}&before=${oldest}&limit=${PAGE}`);
      found = page.findLast(answerable); more = page.length === PAGE; oldest = page[0]?.seq;
    }
    return found?.id || root;
  }

  async sendThread() {
    if (!this.alive || !this.host.threadRoot || this.threadUploads || (!this.host.threadDraft.value.trim() && !this.threadPending.length)) return;
    const room = this.host.room.id, root = this.host.threadRoot.id, body = this.host.threadDraft.value, generation = this.openGeneration, attachments = this.threadPending.map(item => item.id);
    this.host.beginThreadSend();
    try {
      const message = await request(`/api/rooms/${room}/messages`, { method: 'POST', body: { body, reply_to: await this.lastInThread(room, root), ...(attachments.length ? { attachments } : {}) } });
      if (generation === this.openGeneration && this.host.threadRoot?.id === root) {
        this.threadPending = []; this.host.renderThreadPending();
        this.host.sentThreadReply();
        const atEnd = this.host.threadLatestHidden();
        if (atEnd) this.add(message, false, false, this.contextMode);
        else { this.markNewThreadReply(message); if (!this.contextMode) this.add(message); }
        if (atEnd) this.host.threadShowSent(message);
      }
    } catch (error) { if (generation === this.openGeneration) this.host.threadSendFailed(error.message); }
  }

  async send() {
    if (!this.alive || !this.host.room) return;
    const input = this.host.sendInput(); if (!input) return;
    const room = this.host.room.id, session = this.sessionGeneration, { body, reply, attachments } = input;
    this.host.beginSend();
    let sentID = null;
    try {
      if (await this.host.sendAsAnswer(reply, attachments, body, () => this.current(session, room))) return;
      const message = await request(`/api/rooms/${room}/messages`, { method: 'POST', body: { body, attachments, ...(reply ? { reply_to: reply.id } : {}) } });
      sentID = message.id;
      if (this.current(session, room)) {
        this.pendingAttachments = [];
        this.host.sentMessage();
        // A history window does not show the live end, so appending there would put the message among old ones: show the latest instead.
        if (this.contextMode) await this.reopenLatest(); else { this.add(message); this.trimLive(); }
      }
    } catch (error) { if (this.current(session, room)) this.host.sendFailed(error.message); }
    finally { this.host.sendFinished(sentID); }
  }

  // Reopening the room at the latest closes the thread panel; an open thread and its unsent reply survive it.
  async reopenLatest() {
    if (!this.alive) return;
    const root = this.host.threadRoot, draft = this.host.threadDraft.value, room = this.host.room, session = this.sessionGeneration;
    await this.open(room);
    if (!root || !this.current(session, room.id) || this.host.threadRoot) return;
    const thread = this.openThread(root);
    this.host.restoreThreadDraft(draft);
    await thread;
  }

  async addFiles(files) {
    const room = this.host.room;
    if (!this.alive || !room || room.lifecycle !== 'open') return;
    this.host.notice.clear(); // a new pick, paste or drop starts over: what was said about the last one no longer holds
    const roomID = room.id, session = this.sessionGeneration;
    for (const file of files) {
      if (!this.current(session, roomID)) return;
      const name = file.name || 'The file';
      if (this.pendingAttachments.length + this.pendingUploads >= 4) { this.host.notice.show('Up to four files per message.', { append: true }); break; }
      if (!file.size) { this.host.notice.show(`${name} is empty.`, { append: true }); continue; }
      if (file.size > 10 * 1024 * 1024) { this.host.notice.show(`${name} is too big (${megabytes(file.size)}). Use files up to 10 MB.`, { append: true }); continue; }
      const controller = new AbortController(); this.uploadRequests.add(controller);
      this.pendingUploads++; this.host.renderPending();
      try {
        const attachment = await upload(`/api/rooms/${roomID}/attachments`, file, { signal: controller.signal });
        if (this.current(session, roomID)) this.pendingAttachments = [...this.pendingAttachments, attachment];
        else await request(`/api/rooms/${roomID}/attachments/${attachment.id}`, { method: 'DELETE' }).catch(() => {});
      } catch (error) { if (this.current(session, roomID)) this.host.notice.show(`${name}: ${error.message}`, { append: true }); }
      finally { this.uploadRequests.delete(controller); if (session === this.sessionGeneration) { this.pendingUploads--; this.host.renderPending(); } }
    }
  }

  async removeAttachment(attachment) {
    if (!this.alive || !this.host.room) return;
    const roomID = this.host.room.id, session = this.sessionGeneration;
    try {
      await request(`/api/rooms/${roomID}/attachments/${attachment.id}`, { method: 'DELETE' });
      if (!this.current(session, roomID)) return;
      this.pendingAttachments = this.pendingAttachments.filter(item => item.id !== attachment.id); this.host.renderPending();
    } catch (error) { if (this.current(session, roomID)) this.host.say(error.message); }
  }
}
