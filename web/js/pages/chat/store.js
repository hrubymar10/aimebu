import { request, upload, currentUser } from '../../api.js';
import { megabytes } from '../../notice.js';
import { liveArrival, liveRoute } from './model.js';

// The room's messages and the calls that load them; `host` draws. A call says nothing and starts nothing once its generation or the store is gone.
export class ChatStore {
  constructor(host) {
    this.host = host; this.alive = true;
    this.openGeneration = 0; this.sessionGeneration = 0; this.jumpGeneration = 0; this.pageSize = 100; this.olderPageSize = 50;
    this.threadGeneration = 0; this.threadOldestSeq = 0; this.threadHasMoreOlder = false; this.loadingOlderThread = false; this.threadPageSize = 100; this.threadOlderPageSize = 50;
    this.initialLoading = false; this.announcedIDs = null; this.reactionTickets = new Map();
    this.pendingAttachments = []; this.pendingUploads = 0; this.uploadRequests = new Set();
    this.clearRoom();
  }

  clearRoom() {
    this.answeredMessages = new Set(); this.repliedTo = new Set(); this.messages = new Map(); this.historicalAuthors = new Set(); this.reactions = new Map();
    this.threadCounts = new Map(); this.serverThreadCounts = new Map(); this.missedIDs = new Set(); this.announcedIDs = null;
    this.newestSeq = 0; this.oldestSeq = 0; this.hasMoreOlder = false; this.loadingOlder = false; this.loadingNewer = false; this.contextMode = false; this.catchingUp = false; this.windowSeq = 0;
  }

  current(session, roomID) { return session === this.sessionGeneration && this.host.room?.id === roomID; }

  reset() {
    for (const controller of this.uploadRequests) controller.abort();
    this.uploadRequests.clear(); this.pendingAttachments = []; this.pendingUploads = 0;
    this.openGeneration++; this.sessionGeneration++; this.threadGeneration++; this.clearRoom(); this.initialLoading = false; }

  destroy() { this.reset(); this.alive = false; }

  async open(room) {
    if (!this.alive) return;
    const generation = ++this.openGeneration;
    this.host.beginOpen(room);
    const messages = await request(`/api/rooms/${room.id}/messages?limit=${this.pageSize}`);
    if (generation !== this.openGeneration) return;
    this.host.drawPage(messages);
    this.newestSeq = messages.at(-1)?.seq || 0;
    this.oldestSeq = messages[0]?.seq || 0;
    this.hasMoreOlder = messages.length === this.pageSize;
    this.host.showNewest();
    this.initialLoading = false; this.host.loadOlderIfShort();
    this.host.onLatest?.(room);
    await this.refresh();
  }

  async refresh() {
    if (!this.alive || !this.host.room || this.initialLoading) return;
    const id = this.host.room.id, generation = this.openGeneration;
    for (;;) {
      const messages = await request(`/api/rooms/${id}/messages?after=${this.newestSeq || 0}&limit=${this.pageSize}`);
      if (generation !== this.openGeneration) return;
      for (const message of messages) {
        const route = liveRoute(message, { hasMoreOlder: this.hasMoreOlder, oldestSeq: this.oldestSeq, contextMode: this.contextMode, known: this.messages.has(message.id), missed: this.missedIDs.has(message.id), ...this.host.threadState(message) });
        if (route === 'new-reply') this.host.markNewThreadReply(message);
        else if (route === 'add') { const known = this.messages.has(message.id); this.host.add(message); if (!known && liveArrival(message, this) && !this.announcedIDs?.has(message.id)) this.host.announce(message); }
        else if (route === 'missed') {
          this.missedIDs.add(message.id); this.host.updateLatestLabel();
          if (!this.initialLoading && message.author_kind !== 'system') this.host.announce(message);
        }
      }
      if (messages.length) this.newestSeq = messages.at(-1).seq;
      if (messages.length) this.host.trimLive();
      // The project refresh for the same event ran before this render and could only mark up to the previous newest message.
      if (messages.length && !this.contextMode && !this.catchingUp) this.host.onLatest?.(this.host.room);
      if (messages.length < this.pageSize) return;
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
      this.host.prependPage(messages);
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
      for (const message of messages) this.host.add(message, false, true);
      if (messages.length) this.windowSeq = messages.at(-1).seq;
      // A short page is the live end of the room: the window is the latest view again.
      if (messages.length < this.olderPageSize) { await this.leaveWindow(); return; }
      this.host.trimOldest();
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
      if (!message) {
        const messages = await request(`/api/rooms/${roomID}/messages?around=${encodeURIComponent(key)}&limit=${this.pageSize}`);
        if (generation !== this.openGeneration || jump !== this.jumpGeneration) return;
        message = messages.find(item => item.id === key);
        if (!message) throw new Error('Message is not in this room.');
        generation = ++this.openGeneration; this.announcedIDs = null;
        // A page in flight belongs to the old view and will not clear its own flag.
        this.hasMoreOlder = false; this.loadingOlder = false; this.loadingNewer = false; this.catchingUp = false; this.oldestSeq = 0;
        this.host.beginWindow();
        this.messages.clear(); this.historicalAuthors = new Set(); this.reactions.clear(); this.threadCounts.clear(); this.serverThreadCounts.clear();
        this.host.drawWindow(messages);
        this.oldestSeq = messages[0]?.seq || 0; this.hasMoreOlder = messages.length === this.pageSize;
        this.host.showWindow();
        this.contextMode = true; this.host.showLatestButton(true); this.windowSeq = messages.at(-1)?.seq || 0;
      }
      if (message.thread_root) {
        let root = this.messages.get(message.thread_root);
        if (!root) {
          const roots = await request(`/api/rooms/${roomID}/messages?around=${encodeURIComponent(message.thread_root)}&limit=1`);
          if (generation !== this.openGeneration || jump !== this.jumpGeneration) return;
          root = roots.find(item => item.id === message.thread_root);
        }
        if (root) await this.openThread(root, message);
        if (generation !== this.openGeneration || jump !== this.jumpGeneration) return;
        this.host.showReply(message);
      } else this.host.showRow(message);
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

  async openThread(root, focus = null) {
    if (!this.alive) return;
    const roomID = this.host.room?.id, thread = ++this.threadGeneration;
    this.host.beginThread(root, focus);
    try {
      const context = focus ? `&around=${encodeURIComponent(focus.id)}` : '';
      const page = await request(`/api/rooms/${roomID}/messages?thread_root=${encodeURIComponent(root.id)}${context}&limit=${this.threadPageSize}`);
      if (thread !== this.threadGeneration) return;
      this.host.drawThreadPage(page);
      this.threadOldestSeq = page[0]?.seq || 0;
      this.threadHasMoreOlder = page.length === this.threadPageSize;
      this.host.showThreadPage(focus);
      const lastSeq = focus ? page.at(-1)?.seq : null;
      if (lastSeq) {
        const newer = await request(`/api/rooms/${roomID}/messages?thread_root=${encodeURIComponent(root.id)}&after=${lastSeq}&limit=1`);
        if (thread !== this.threadGeneration) return;
        if (!newer.length) this.host.settleThreadLatest();
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
      this.host.prependThreadPage(page);
    } catch (error) {
      if (thread === this.threadGeneration) this.host.threadError(error.message);
    } finally {
      if (thread === this.threadGeneration) { this.loadingOlderThread = false; this.host.showThreadOlderStatus(false); }
    }
  }

  async sendThread() {
    if (!this.alive || !this.host.threadRoot || !this.host.threadDraft.value.trim()) return;
    const room = this.host.room.id, root = this.host.threadRoot.id, body = this.host.threadDraft.value, generation = this.openGeneration;
    this.host.beginThreadSend();
    try {
      const message = await request(`/api/rooms/${room}/messages`, { method: 'POST', body: { body, thread_root: root, reply_to: root } });
      if (generation === this.openGeneration && this.host.threadRoot?.id === root) this.host.sentThreadReply(message);
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
        this.host.sentMessage();
        // A history window does not show the live end, so appending there would put the message among old ones: show the latest instead.
        if (this.contextMode) await this.reopenLatest(); else this.host.addSent(message);
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
