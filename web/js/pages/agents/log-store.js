import { signal } from '../../ui.js';
import { FOLLOW_FALLBACK_MS, FOLLOW_GAP_MS, LINES_DEFAULT, clampLines, followEvent, lastLines, logFailure, logStatus, nextReadAt, retryAfterMs, stripAnsi } from './log-model.js';

// The agent log popup's state: which agent it shows, the lines asked for, Follow, and the text of the last read. Reads go through the
// source's shared reader (agent-output-share.js). Follow reads again when an event of the agent arrives (at most once in FOLLOW_GAP_MS), every
// FOLLOW_FALLBACK_MS while the agent is in a turn, never while the tab is hidden, and after a 429 not before its Retry-After. Every read carries
// a ticket: opening, closing and a change of lines take a new one, so an answer for an earlier one draws nothing.
export class LogStore {
  constructor({ source, roomID, agent, now = () => Date.now(), timers = globalThis, doc = globalThis.document }) {
    this.source = source; this.roomID = roomID; this.agent = agent; this.now = now; this.timers = timers; this.doc = doc;
    this.state = signal(null);
    this.ticket = 0; this.lastReadAt = 0; this.blockedUntil = 0; this.timer = 0; this.fallback = 0; this.wanted = false;
    this.unsubscribe = source.subscribe?.(event => this.onEvent(event));
    this.onVisible = () => { if (!this.hidden() && this.wanted) { this.wanted = false; this.wake(); } };
    this.doc?.addEventListener?.('visibilitychange', this.onVisible);
  }

  hidden() { return this.doc?.visibilityState === 'hidden'; }

  patch(ticket, change) { if (ticket === this.ticket && this.state.peek()) this.state.value = { ...this.state.peek(), ...change }; }

  open(agentID) {
    this.stopTimers(); this.ticket++;
    this.state.value = { agentID, lines: LINES_DEFAULT, follow: false, text: '', raw: '', status: '', loading: false, updatedAt: '', readAt: 0 };
    void this.read();
  }

  close() { this.stopTimers(); this.ticket++; this.wanted = false; this.state.value = null; }

  stopTimers() { this.timers.clearTimeout(this.timer); this.timers.clearTimeout(this.fallback); this.timer = 0; this.fallback = 0; }

  // The lines field, as typed: a new number reads again (the server's tail is what it asked for, not a cut of an earlier answer).
  setLines(value) {
    const state = this.state.peek();
    if (!state) return;
    const lines = clampLines(value);
    if (lines === state.lines) return;
    this.ticket++;
    this.state.value = { ...state, lines };
    void this.read();
  }

  setFollow(follow) {
    const state = this.state.peek();
    if (!state || state.follow === follow) return;
    this.state.value = { ...state, follow };
    this.stopTimers();
    if (!follow) { this.wanted = false; return; }
    this.armFallback();
    this.wake();
  }

  // An agent still in a turn with nothing in the stream for FOLLOW_FALLBACK_MS since the last read is read anyway, in case the events stopped
  // and the log did not. The timer starts again from every read.
  armFallback(wait = Math.max(0, (this.lastReadAt || this.now()) + FOLLOW_FALLBACK_MS - this.now())) {
    this.timers.clearTimeout(this.fallback); this.fallback = 0;
    if (!this.state.peek()?.follow) return;
    this.fallback = this.timers.setTimeout(() => {
      this.fallback = 0;
      const state = this.state.peek();
      if (!state?.follow) return;
      // A read re-arms the timer; an agent not in a turn is looked at again in another FOLLOW_FALLBACK_MS.
      if (this.agent(state.agentID)?.observed === 'in_turn') this.wake(); else this.armFallback(FOLLOW_FALLBACK_MS);
    }, wait);
  }

  onEvent(event) {
    const state = this.state.peek();
    if (state?.follow && followEvent(event, state.agentID)) this.wake();
  }

  // A follow read is due: now if the last read began FOLLOW_GAP_MS ago and no 429 is waiting, else once both are over (one timer at a time,
  // so a burst of events makes one read). A hidden tab reads when it is shown again.
  wake() {
    const state = this.state.peek();
    if (!state?.follow) return;
    if (this.hidden()) { this.wanted = true; return; }
    if (this.timer) return;
    const now = this.now(), at = nextReadAt({ now, lastReadAt: this.lastReadAt, blockedUntil: this.blockedUntil, gap: FOLLOW_GAP_MS });
    this.timer = this.timers.setTimeout(() => {
      this.timer = 0;
      if (!this.state.peek()?.follow) return;
      if (this.hidden()) { this.wanted = true; return; }
      void this.read({ fresh: FOLLOW_GAP_MS });
    }, at - now);
  }

  // Reads the tail now (opening, a change of lines, Refresh) or for Follow (`fresh`: an answer another tab got that recently will do).
  async read({ fresh = 0 } = {}) {
    const state = this.state.peek(), roomID = this.roomID();
    if (!state || !roomID) return;
    const ticket = this.ticket, now = this.now();
    // The server asked to wait (a 429): say so, and read once the wait is over (a following popup through wake, a read asked for once).
    if (now < this.blockedUntil) {
      this.patch(ticket, { status: logFailure({ status: 429 }, now, this.blockedUntil) });
      if (state.follow) this.wake();
      else if (!this.timer) this.timer = this.timers.setTimeout(() => { this.timer = 0; void this.read(); }, this.blockedUntil - now);
      return;
    }
    this.lastReadAt = now;
    this.armFallback();
    this.patch(ticket, { loading: true });
    try {
      const view = await this.source.readOutput(roomID, state.agentID, state.lines, { fresh });
      const raw = view?.text || '', text = stripAnsi(lastLines(raw, this.state.peek()?.lines || state.lines));
      this.patch(ticket, { raw, text, status: logStatus({ ...view, text }), updatedAt: view?.updated_at || '', readAt: this.now() });
    } catch (error) {
      if (error?.status === 429) {
        this.blockedUntil = this.now() + retryAfterMs(error.retryAfterHeader || error.retryAfter, this.now());
        if (this.state.peek()?.follow && ticket === this.ticket) this.wake();
      }
      // A follow read that fails, or a 429, keeps the text already shown (the view must not blank while the agent works); a read asked for shows only the refusal.
      const keep = ticket === this.ticket && !!this.state.peek()?.text && (fresh > 0 || error?.status === 429);
      this.patch(ticket, { ...(keep ? {} : { text: '', raw: '' }), status: logFailure(error, this.now(), this.blockedUntil) });
    } finally { this.patch(ticket, { loading: false }); }
  }

  destroy() {
    this.close();
    this.unsubscribe?.();
    this.doc?.removeEventListener?.('visibilitychange', this.onVisible);
  }
}
