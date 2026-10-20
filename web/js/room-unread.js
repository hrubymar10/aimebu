// The unread badge of a room row. A collapsed root also stands for the rooms nested under it, so collapsing
// never hides new activity: its badge is its own unread count plus its children's. The open room counts as read.
export function roomUnread(room, activeID) { return room.id === activeID ? 0 : Math.max(0, room.unread_count || 0); }

export function rowUnread(room, children, collapsed, activeID) {
  return roomUnread(room, activeID) + (collapsed ? children.reduce((sum, child) => sum + roomUnread(child, activeID), 0) : 0);
}

// Sends read marks with at most one request per room in flight. A mark asked for while one is in flight is kept (the
// highest seq wins; 0 means the room's newest) and sent when the first one settles, if it goes further than that one.
// reset() (sign-out) forgets everything, and a request from before it neither re-issues nor reports its result.
export class ReadMarker {
  constructor(post) { this.post = post; this.generation = 0; this.inFlight = new Map(); this.wanted = new Map(); }
  reset() { this.generation++; this.inFlight.clear(); this.wanted.clear(); }
  mark(roomID, seq, onState) {
    const further = (a, b) => a === 0 ? b !== 0 : b !== 0 && a > b;
    if (this.inFlight.has(roomID)) {
      const sent = this.inFlight.get(roomID), wanted = this.wanted.get(roomID);
      if (further(seq, sent) && (!wanted || further(seq, wanted.seq))) this.wanted.set(roomID, { seq, onState });
      return;
    }
    const generation = this.generation;
    this.inFlight.set(roomID, seq);
    Promise.resolve().then(() => this.post(roomID, seq)).then(state => { if (generation === this.generation) onState?.(state); }, () => {}).finally(() => {
      if (generation !== this.generation) return;
      this.inFlight.delete(roomID);
      const next = this.wanted.get(roomID); this.wanted.delete(roomID);
      if (next && further(next.seq, seq)) this.mark(roomID, next.seq, next.onState);
    }).catch(() => {}); // an onState that throws must not surface as an unhandled rejection
  }
}
