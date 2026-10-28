import { signal } from '../../ui.js';
import { request } from '../../api.js';

// One open profile. The profile read carries a ticket of this opening, and closing the dialog, opening another profile or a destroy ends it, so
// an answer for a profile that is gone fills nothing.
export class SenderProfileStore {
  constructor(options) {
    this.options = options;
    this.phase = signal(options.roomID ? 'loading' : 'none');
    this.profile = signal(null);
    this.alive = true;
  }

  async load() {
    const { roomID, authorID } = this.options;
    if (!roomID) return;
    let profile = null, failed = false;
    try { profile = await request(`/api/rooms/${roomID}/agents/${authorID}/profile`); } catch { failed = true; }
    if (!this.alive) return;
    this.profile.value = profile; this.phase.value = failed ? 'failed' : 'done';
  }

  destroy() { this.alive = false; }
}
