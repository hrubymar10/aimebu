import { request } from './api.js';

// Human presence heartbeat. The server counts a person online while one of
// their browser sessions has shown real activity in the last ten minutes, and
// what counts is decided here: a heartbeat is sent only while the page is
// visible AND the person has interacted with it within the last minute. Plain
// background polling (every periodic read the page makes is a GET) and the
// merely open event-stream socket never count, so an account nobody has used
// for a while goes offline on the member cards without anyone signing it out.
const SEND_INTERVAL_MS = 60000; // at most one heartbeat a minute while the person keeps working
const INPUT_WINDOW_MS = 60000; // input older than this is not activity the page still vouches for

// A tap, a click, a key, a scroll: the ways a person shows they are using the page.
const INPUT_EVENTS = ['keydown', 'pointerdown', 'wheel', 'touchstart'];

export class PresenceHeartbeat {
  constructor() { this.lastInput = 0; this.lastSent = 0; this.timer = 0; this.stopped = true; }

  start() {
    if (!this.stopped) return;
    this.stopped = false;
    this.onInput = () => { this.lastInput = Date.now(); this.maybeSend(); };
    this.onVisible = () => { if (document.visibilityState === 'visible') this.maybeSend(); };
    for (const name of INPUT_EVENTS) document.addEventListener(name, this.onInput, { passive: true, capture: true });
    document.addEventListener('visibilitychange', this.onVisible);
    this.timer = setInterval(() => this.maybeSend(), SEND_INTERVAL_MS);
    this.maybeSend();
  }

  stop() {
    if (this.stopped) return;
    this.stopped = true;
    clearInterval(this.timer);
    for (const name of INPUT_EVENTS) document.removeEventListener(name, this.onInput, { capture: true });
    document.removeEventListener('visibilitychange', this.onVisible);
  }

  // The one gate: a visible page and input the page saw recently. Everything
  // else — a hidden tab, an idle keyboard, a socket left open — stays silent.
  maybeSend() {
    if (this.stopped || document.visibilityState !== 'visible') return;
    const now = Date.now();
    if (now - this.lastInput > INPUT_WINDOW_MS || now - this.lastSent < SEND_INTERVAL_MS) return;
    this.lastSent = now;
    // A refused or unreachable heartbeat is no error worth showing: the next
    // input tries again, and the server answers honestly in the meantime.
    request('/api/presence', { method: 'POST', body: {} }).catch(() => {});
  }
}
