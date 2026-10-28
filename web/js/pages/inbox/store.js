import { batch, signal } from '../../ui.js';
import { uuid } from '../../uuid.js';
import { resolvedItems } from './model.js';

// `source` stays the one place requests and mutations go through.
export class InboxStore {
  constructor({ source, projects, rooms, notifications, onItems }) {
    this.source = source; this.projects = projects; this.rooms = rooms; this.notifications = notifications; this.onItems = onItems;
    this.items = signal(undefined);
    this.error = signal('');
    this.pending = signal(new Set());
    this.bulkBusy = signal(false);
    this.openUsages = signal(null);
    this.keys = new Map();
    this.requestSeq = 0;
    this.generation = 0;
    this.alive = true;
  }

  // A mutation belongs to the view as it was when it began: a reset or a destroy ends it.
  owns(generation) { return this.alive && generation === this.generation; }

  setError(message) { this.error.value = message; }

  setPending(id, on) { const next = new Set(this.pending.peek()); if (on) next.add(id); else next.delete(id); this.pending.value = next; }

  async refresh({ notify = true } = {}) {
    if (!this.alive) return;
    const ticket = ++this.requestSeq;
    const items = await this.source.list();
    if (!this.alive || ticket !== this.requestSeq) return;
    const previous = this.items.peek() && new Set(this.items.peek().map(item => item.id));
    batch(() => {
      this.items.value = items;
      const ids = new Set(items.map(item => item.id));
      const stale = [...this.pending.peek()].filter(id => !ids.has(id));
      if (stale.length) { const next = new Set(this.pending.peek()); for (const id of stale) { next.delete(id); this.keys.delete(id); } this.pending.value = next; }
    });
    this.onItems?.(items);
    if (notify && previous) this.notifications?.arrived(items.filter(item => !previous.has(item.id)), this.projects, this.rooms);
  }

  reset() {
    this.requestSeq++; this.generation++;
    batch(() => { this.items.value = undefined; this.pending.value = new Set(); this.bulkBusy.value = false; this.error.value = ''; });
    this.keys.clear();
    this.onItems?.([]);
  }

  // One attempt per item and value: the same answer sent again keeps its idempotency key.
  attempt(id, value) {
    let attempt = this.keys.get(id);
    if (!attempt || attempt.value !== value) { attempt = { value, key: uuid() }; this.keys.set(id, attempt); }
    return attempt.key;
  }

  async answer(item, value) {
    if (!this.alive || !value) return undefined;
    this.setError('');
    if (this.pending.peek().has(item.id)) return undefined;
    this.setPending(item.id, true);
    const key = this.attempt(item.id, value), generation = this.generation;
    try { await this.source.answer(item.id, value, key, item.rev); }
    catch (failure) { if (this.owns(generation)) { this.setError(failure.message); this.setPending(item.id, false); } return false; }
    if (!this.owns(generation)) return false;
    this.keys.delete(item.id);
    await this.refresh();
    return true;
  }

  async decide(item, choice) {
    if (!this.alive) return;
    this.setError('');
    if (this.pending.peek().has(item.id)) return;
    this.setPending(item.id, true);
    const key = this.attempt(item.id, choice), generation = this.generation;
    try { await this.source.decision(item.id, choice, key, item.rev); }
    catch (failure) {
      if (!this.owns(generation)) return;
      this.setError(failure.code === 'stale_rev' ? 'This item changed, reload item.' : failure.message);
      this.setPending(item.id, false);
      if (failure.code === 'stale_rev') { this.keys.delete(item.id); await this.refresh(); }
      return;
    }
    if (!this.owns(generation)) return;
    this.keys.delete(item.id);
    await this.refresh();
  }

  async dismiss(item) {
    if (!this.alive) return;
    this.setError('');
    if (this.pending.peek().has(item.id)) return;
    this.setPending(item.id, true);
    const generation = this.generation;
    try { await this.source.dismiss(item.id, item.rev); }
    catch (failure) { if (this.owns(generation)) { this.setError(failure.message); this.setPending(item.id, false); } return; }
    if (this.owns(generation)) await this.refresh();
  }

  async dismissResolved() {
    const items = resolvedItems(this.items.peek());
    if (!this.alive || !items.length || this.bulkBusy.peek()) return;
    const generation = this.generation;
    batch(() => { this.setError(''); this.bulkBusy.value = true; });
    try { for (const item of items) { if (!this.owns(generation)) return; await this.source.dismiss(item.id, item.rev); } }
    catch (failure) { if (this.owns(generation)) this.setError(failure.message); }
    if (!this.owns(generation)) { if (this.alive) this.bulkBusy.value = false; return; }
    this.bulkBusy.value = false;
    await this.refresh({ notify: false });
  }

  destroy() { this.alive = false; this.requestSeq++; this.generation++; }
}
