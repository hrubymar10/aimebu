// Screen-reader announcements for live message arrivals. The message list itself is not a live
// region (that would read every appended message, its toolbar, and whole pages of older history),
// so this visually hidden polite region announces only messages that arrive while the room is open.
const WINDOW_MS = 900, MIN_GAP_MS = 3000, SNIPPET = 120;

export class LiveAnnouncer {
  constructor(region) {
    this.region = region; this.pending = []; this.timer = null; this.speakTimer = null; this.lastAt = 0; this.room = '';
    this.own = new Set(); this.sending = 0; this.held = [];
  }
  // Drops anything queued or about to be spoken and clears the region; used when the room changes or the chat resets.
  reset() { clearTimeout(this.timer); this.timer = null; clearTimeout(this.speakTimer); this.speakTimer = null; this.pending = []; this.held = []; this.lastAt = 0; this.region.textContent = ''; }
  // A message from the signed-in user is silent when this tab sent it, but another tab of the same
  // account is a real arrival. Which one it is can only be told by message id, and the live event for
  // a send can beat its response, so such messages wait out any send in flight: the response names
  // the ones that were ours, and the rest are announced.
  sendStarted() { this.sending++; }
  sendFinished(id) {
    this.sending = Math.max(0, this.sending - 1);
    if (id) { this.own.add(id); if (this.own.size > 200) this.own.delete(this.own.values().next().value); this.held = this.held.filter(item => item.id !== id); }
    if (!this.sending) { const held = this.held; this.held = []; for (const item of held) this.queue(item); }
  }
  arrival(item) {
    if (item.mine) {
      if (this.own.has(item.id)) return;
      if (this.sending) { this.held.push(item); return; }
    }
    this.queue(item);
  }
  queue({ from, body, room }) {
    this.room = room || this.room;
    this.pending.push({ from, body: String(body || '').replace(/\s+/g, ' ').trim() });
    if (this.timer) return;
    const wait = Math.max(WINDOW_MS, this.lastAt + MIN_GAP_MS - Date.now());
    this.timer = setTimeout(() => this.flush(), wait);
  }
  flush() {
    this.timer = null;
    const items = this.pending; this.pending = [];
    if (!items.length) return;
    this.lastAt = Date.now();
    const text = items.length === 1
      ? `New message from ${items[0].from}${items[0].body ? `: ${items[0].body.slice(0, SNIPPET)}${items[0].body.length > SNIPPET ? '…' : ''}` : ''}`
      : `${items.length} new messages${this.room ? ` in ${this.room}` : ''}`;
    // Clearing first makes an identical announcement fire again.
    this.region.textContent = '';
    // The text is set a moment after the clear; reset() cancels that too, or a flush just before a room change
    // would announce the old room's batch in the new room.
    clearTimeout(this.speakTimer);
    this.speakTimer = setTimeout(() => { this.speakTimer = null; this.region.textContent = text; }, 50);
  }
}
