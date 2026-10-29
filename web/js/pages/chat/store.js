import { request, currentUser } from '../../api.js';
import { liveArrival, liveRoute } from './model.js';

// The room's messages and the calls that load them; `host` draws. A call says nothing and starts nothing once its generation or the store is gone.
export class ChatStore {
  constructor(host) {
    this.host = host; this.alive = true;
    this.openGeneration = 0; this.sessionGeneration = 0; this.jumpGeneration = 0; this.pageSize = 100; this.olderPageSize = 50;
    this.initialLoading = false; this.announcedIDs = null; this.reactionTickets = new Map();
    this.clearRoom();
  }

  clearRoom() {
    this.answeredMessages = new Set(); this.repliedTo = new Set(); this.messages = new Map(); this.historicalAuthors = new Set(); this.reactions = new Map();
    this.threadCounts = new Map(); this.serverThreadCounts = new Map(); this.missedIDs = new Set(); this.announcedIDs = null;
    this.newestSeq = 0; this.oldestSeq = 0; this.hasMoreOlder = false; this.loadingOlder = false; this.loadingNewer = false; this.contextMode = false; this.catchingUp = false; this.windowSeq = 0;
  }

  reset() { this.openGeneration++; this.sessionGeneration++; this.clearRoom(); this.initialLoading = false; }

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
    this.host.latestShown(room);
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
      if (messages.length && !this.contextMode && !this.catchingUp) this.host.latestShown(this.host.room);
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
    this.host.latestShown(room);
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
        if (found.room_id !== this.host.room.id) { this.host.say(''); this.host.openMessage(found); return; }
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
        if (root) await this.host.openThread(root, message);
        if (generation !== this.openGeneration || jump !== this.jumpGeneration) return;
        this.host.showReply(message);
      } else this.host.showRow(message);
      this.host.say('');
    } catch (error) { if (generation === this.openGeneration && jump === this.jumpGeneration) this.host.say(error.message); }
  }

  async refreshReactions(id) {
    if (!this.alive || !this.host.node(id)) return;
    const generation = this.openGeneration, ticket = (this.reactionTickets.get(id) || 0) + 1;
    this.reactionTickets.set(id, ticket);
    const entries = await request(`/api/messages/${id}/reactions`);
    if (generation !== this.openGeneration || this.reactionTickets.get(id) !== ticket) return;
    this.host.showReactions(id, entries);
  }

  async toggleReaction(id, emoji) {
    if (!this.alive || !this.host.room) return;
    const generation = this.openGeneration;
    const mine = this.reactions.get(id)?.some(entry => entry.emoji === emoji && entry.member_id === currentUser);
    try { await request(`/api/messages/${id}/reactions/${encodeURIComponent(emoji)}`, { method: mine ? 'DELETE' : 'PUT' }); if (generation !== this.openGeneration) return; await this.refreshReactions(id); }
    catch (error) { if (generation === this.openGeneration) this.host.say(error.message); }
  }
}
