import { request } from './api.js';

// Draws v1's "Seen by" strip: each member's read position marks the newest
// loaded message they have read, except their own. Humans report their
// stored read cursor; agents report the newest message they took into a turn.
export class ReadReceipts {
  constructor(chat) {
    this.chat = chat;
    this.receipts = [];
    this.roomID = '';
    this.timer = 0;
    this.requestID = 0;
    // Messages arrive, prepend and reset through several chat paths; watching
    // the list keeps the strips in step without chat knowing about them.
    this.observer = new MutationObserver(() => this.schedule());
    this.observer.observe(chat.list, { childList: true });
  }
  schedule() {
    clearTimeout(this.timer);
    this.timer = setTimeout(() => this.refresh(), 300);
  }
  async refresh() {
    const room = this.chat.room;
    if (!room) { this.receipts = []; this.roomID = ''; return; }
    const requestID = ++this.requestID;
    let receipts;
    try { receipts = await request(`/api/rooms/${room.id}/read-receipts`); } catch { return; }
    if (requestID !== this.requestID || this.chat.room?.id !== room.id) return;
    this.roomID = room.id;
    this.receipts = Array.isArray(receipts) ? receipts : [];
    this.render();
  }
  // Incremental: one pass over the loaded messages finds every receipt's target (no sort, no per-receipt scan), and only
  // the rows whose strip changes are touched, the ones that had a strip last time and the ones that get one now. A row
  // that left the list takes its strip along; one paged back in is redrawn because its strip's signature is missing.
  render() {
    if (this.chat.room?.id !== this.roomID) return;
    const targets = this.receipts.map(() => null);
    for (const message of this.chat.messages.values()) {
      for (let i = 0; i < this.receipts.length; i++) {
        if (message.seq > this.receipts[i].seq) continue;
        if (!targets[i] || message.seq > targets[i].seq) targets[i] = message;
      }
    }
    const byMessage = new Map();
    this.receipts.forEach((receipt, i) => {
      const target = targets[i]; if (!target) return;
      if (!byMessage.has(target.id)) byMessage.set(target.id, []);
      byMessage.get(target.id).push(receipt);
    });
    const touched = new Set([...(this.shown || []), ...byMessage.keys()]);
    this.shown = new Set();
    for (const id of touched) {
      const node = this.chat.nodes.get(id); if (!node) continue;
      const seen = byMessage.get(id) || [];
      let strip = node.querySelector(':scope > .message-receipts');
      if (!seen.length) { strip?.remove(); continue; }
      this.shown.add(id);
      if (!strip) { strip = document.createElement('div'); strip.className = 'message-receipts'; strip.setAttribute('role', 'group'); node.append(strip); }
      const labels = seen.map(receipt => receipt.label);
      strip.setAttribute('aria-label', `Seen by ${labels.join(', ')}`);
      const signature = seen.map(receipt => `${receipt.member_kind}:${receipt.member_id}`).join('|');
      if (strip.dataset.signature === signature) continue;
      strip.dataset.signature = signature;
      strip.replaceChildren(...seen.map(receipt => {
        const avatar = document.createElement('span');
        avatar.className = 'receipt-avatar';
        avatar.dataset.member = receipt.member_id;
        avatar.textContent = receipt.label.slice(0, 2).toUpperCase();
        avatar.title = `Seen by ${receipt.label}`;
        return avatar;
      }));
    }
  }
}
