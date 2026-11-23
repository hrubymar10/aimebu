// Reads of an agent's output, shared. The server lets a person read agent output 30 times a minute, and a popup that follows an agent reads
// up to 15 times a minute, so two tabs following the same agent would spend the whole budget. Here a read already out for at least as many
// lines answers a second ask in this tab, and the tabs of this browser tell each other through a BroadcastChannel when they start a read and
// what it answered: another tab waits for that answer instead of asking the server again, and a follow read takes an answer that is still
// fresh. The text only crosses between tabs of the same signed-in person (the message carries the user id); it is never stored.
// Everything here belongs to one signed-in person: a read in flight, an answer kept, a read another tab announced and every message are keyed
// by the user id the read began under. When the signed-in person changes (a sign-out, another account signing in on the same page) all of it
// is dropped, a read that began under the earlier person keeps its answer to itself, and a message of another person is ignored, so an
// answer the server gave one account never reaches another.
// Import-free apart from what is passed in, so scripts/test-agent-log-model.cjs runs it in a vm.

// How long a tab waits for another tab's announced read before it reads for itself (the server gives a machine 15 s to answer).
export const REMOTE_WAIT_MS = 16000;

function lineCount(text) { const value = String(text ?? ''); const body = value.endsWith('\n') ? value.slice(0, -1) : value; return body ? body.split('\n').length : 0; }

// `fetch(roomID, agentID, lines)` reads from the server. `channel` is a BroadcastChannel (or null); `user()` the signed-in person's id.
export class OutputShare {
  constructor({ fetch, channel = null, user = () => '', now = () => Date.now(), timers = globalThis }) {
    this.fetch = fetch; this.channel = channel; this.user = user; this.now = now; this.timers = timers;
    this.owner = ''; // the person the maps below belong to
    this.inflight = new Map(); // agentID -> { lines, promise }
    this.recent = new Map(); // agentID -> { lines, view, at }, answers of this tab and the others
    this.remote = new Map(); // agentID -> { lines, promise, resolve, timer }, reads another tab announced
    if (this.channel) this.channel.onmessage = event => this.heard(event.data);
  }

  // The signed-in person now. Another one than the maps belong to drops them all first.
  current() {
    const user = String(this.user() || '');
    if (user !== this.owner) { this.reset(); this.owner = user; }
    return user;
  }

  // Drops every read and answer of the person signed in until now (sign-out, or another account signing in): a read waiting on another tab
  // reads for itself, and a read in flight here keeps its answer to the one who asked for it.
  reset() {
    for (const agentID of [...this.remote.keys()]) this.settle(agentID, null);
    this.inflight.clear(); this.recent.clear();
  }

  // A view for `lines` or more lines: a read in flight here or in another tab for at least that many, else an answer at most `fresh` ms old,
  // else a read of its own. A shared answer longer than asked keeps all its lines; the popup shows the last `lines` of it.
  async read(roomID, agentID, lines, { fresh = 0 } = {}) {
    const user = this.current();
    if (!user) return this.fetch(roomID, agentID, lines); // signed out: nothing to share with
    const own = this.inflight.get(agentID);
    if (own && own.lines >= lines) return own.promise;
    const remote = this.remote.get(agentID);
    if (remote && remote.lines >= lines) {
      const view = await remote.promise;
      // An answer heard for this person; a person who changed while it was awaited reads for themselves.
      if (view && this.current() === user) return view;
    }
    const recent = this.recent.get(agentID);
    if (fresh > 0 && recent && recent.lines >= lines && this.now() - recent.at <= fresh) return recent.view;
    return this.start(roomID, agentID, lines, user);
  }

  // A read of the server for `user`. Its answer is kept and told to the other tabs under `user`, the person it was read for, and kept here
  // only while that person is still signed in; a read the reset dropped is no longer what a later ask joins.
  start(roomID, agentID, lines, user) {
    this.post({ type: 'reading', agentID, lines }, user);
    const entry = { lines, promise: null };
    const mine = () => this.owner === user && this.inflight.get(agentID) === entry;
    entry.promise = Promise.resolve().then(() => this.fetch(roomID, agentID, lines)).then(view => {
      if (mine()) this.recent.set(agentID, { lines, view, at: this.now() });
      this.post({ type: 'result', agentID, lines, view }, user);
      return view;
    }, error => { this.post({ type: 'failed', agentID }, user); throw error; }).finally(() => { if (mine()) this.inflight.delete(agentID); });
    this.inflight.set(agentID, entry);
    return entry.promise;
  }

  // A message always names the person it was read for, never whoever is signed in when it is sent.
  post(message, user) { if (!user) return; try { this.channel?.postMessage({ ...message, user }); } catch { /* a closed channel only loses the sharing */ } }

  heard(message) {
    if (!message || typeof message !== 'object' || !message.agentID) return;
    const user = this.current();
    if (!user || message.user !== user) return;
    const { agentID } = message;
    if (message.type === 'reading') {
      if (this.remote.has(agentID)) return;
      let resolve;
      const promise = new Promise(done => { resolve = done; });
      const timer = this.timers.setTimeout(() => this.settle(agentID, null), REMOTE_WAIT_MS);
      this.remote.set(agentID, { lines: Number(message.lines) || 0, promise, resolve, timer });
    } else if (message.type === 'result') {
      const view = message.view && typeof message.view === 'object' ? message.view : null;
      if (view) this.recent.set(agentID, { lines: Number(message.lines) || lineCount(view.text), view, at: this.now() });
      this.settle(agentID, view);
    } else if (message.type === 'failed') this.settle(agentID, null);
  }

  // Another tab's read ended: whoever waited on it takes its answer, or none (and then reads for itself).
  settle(agentID, view) {
    const remote = this.remote.get(agentID);
    if (!remote) return;
    this.remote.delete(agentID); this.timers.clearTimeout(remote.timer); remote.resolve(view);
  }

  close() {
    for (const agentID of [...this.remote.keys()]) this.settle(agentID, null);
    try { this.channel?.close(); } catch { /* already closed */ }
    this.channel = null;
  }
}
