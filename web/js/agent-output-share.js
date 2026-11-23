// Reads of an agent's output, shared. The server lets a person read agent output 30 times a minute, and a popup that follows an agent reads
// up to 15 times a minute, so two tabs following the same agent would spend the whole budget. Here a read already out for at least as many
// lines answers a second ask in this tab, and the tabs of this browser tell each other through a BroadcastChannel when they start a read and
// what it answered: another tab waits for that answer instead of asking the server again, and a follow read takes an answer that is still
// fresh. The text only crosses between tabs of the same signed-in person (the message carries the user id); it is never stored.
// Import-free apart from what is passed in, so scripts/test-agent-log-model.cjs runs it in a vm.

// How long a tab waits for another tab's announced read before it reads for itself (the server gives a machine 15 s to answer).
export const REMOTE_WAIT_MS = 16000;

function lineCount(text) { const value = String(text ?? ''); const body = value.endsWith('\n') ? value.slice(0, -1) : value; return body ? body.split('\n').length : 0; }

// `fetch(roomID, agentID, lines)` reads from the server. `channel` is a BroadcastChannel (or null); `user()` the signed-in person's id.
export class OutputShare {
  constructor({ fetch, channel = null, user = () => '', now = () => Date.now(), timers = globalThis }) {
    this.fetch = fetch; this.channel = channel; this.user = user; this.now = now; this.timers = timers;
    this.inflight = new Map(); // agentID -> { lines, promise }
    this.recent = new Map(); // agentID -> { lines, view, at }, answers of this tab and the others
    this.remote = new Map(); // agentID -> { lines, promise, resolve, timer }, reads another tab announced
    if (this.channel) this.channel.onmessage = event => this.heard(event.data);
  }

  // A view for `lines` or more lines: a read in flight here or in another tab for at least that many, else an answer at most `fresh` ms old,
  // else a read of its own. A shared answer longer than asked keeps all its lines; the popup shows the last `lines` of it.
  async read(roomID, agentID, lines, { fresh = 0 } = {}) {
    const own = this.inflight.get(agentID);
    if (own && own.lines >= lines) return own.promise;
    const remote = this.remote.get(agentID);
    if (remote && remote.lines >= lines) {
      const view = await remote.promise;
      if (view) return view;
    }
    const recent = this.recent.get(agentID);
    if (fresh > 0 && recent && recent.lines >= lines && this.now() - recent.at <= fresh) return recent.view;
    return this.start(roomID, agentID, lines);
  }

  start(roomID, agentID, lines) {
    this.post({ type: 'reading', agentID, lines });
    const promise = Promise.resolve().then(() => this.fetch(roomID, agentID, lines)).then(view => {
      this.recent.set(agentID, { lines, view, at: this.now() });
      this.post({ type: 'result', agentID, lines, view });
      return view;
    }, error => { this.post({ type: 'failed', agentID }); throw error; }).finally(() => { if (this.inflight.get(agentID)?.promise === promise) this.inflight.delete(agentID); });
    this.inflight.set(agentID, { lines, promise });
    return promise;
  }

  post(message) { try { this.channel?.postMessage({ ...message, user: this.user() }); } catch { /* a closed channel only loses the sharing */ } }

  heard(message) {
    if (!message || typeof message !== 'object' || !message.agentID || message.user !== this.user()) return;
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
