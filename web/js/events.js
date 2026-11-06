export class EventStream {
  constructor(onEvent, onSnapshot, onState, seq = 0) { this.onEvent = onEvent; this.onSnapshot = onSnapshot; this.onState = onState; this.seq = seq; this.delay = 250; this.stopped = true; this.pending = Promise.resolve(); this.beat = 0; this.lastFrame = Date.now(); }
  start() {
    this.stopped = false; this.connect();
    // A tab that was asleep or offline cannot tell its socket died (nothing closes it for minutes), so waking up checks it at once.
    this.wake = () => { if (!this.stopped && (document.visibilityState === 'visible' || document.visibilityState === undefined)) this.check(); };
    document.addEventListener?.('visibilitychange', this.wake); window.addEventListener?.('online', this.wake); window.addEventListener?.('pageshow', this.wake);
  }
  stop() {
    this.stopped = true; clearTimeout(this.timer); clearInterval(this.watchdog); this.socket?.close();
    document.removeEventListener?.('visibilitychange', this.wake); window.removeEventListener?.('online', this.wake); window.removeEventListener?.('pageshow', this.wake);
  }
  // The server sends a heartbeat frame every `heartbeat` seconds (told in its ready frame); a socket that has been quiet for three of those is dead
  // however open it looks, so it is replaced. A server that sends no heartbeat is never judged by silence.
  check() {
    const socket = this.socket;
    if (!socket || !this.beat || Date.now() - this.lastFrame < this.beat * 3) return;
    this.socket = null; clearInterval(this.watchdog);
    try { socket.close(); } catch { /* already gone */ }
    this.onState('Reconnecting…');
    clearTimeout(this.timer); this.timer = setTimeout(() => this.connect(), 0);
  }
  connect() {
    if (this.stopped) return;
    this.ready = false;
    const socket = new WebSocket(`${location.origin.replace(/^http/, 'ws')}/api/events?after=${this.seq}`);
    this.socket = socket; this.beat = 0; this.lastFrame = Date.now(); clearInterval(this.watchdog);
    this.onState('Connecting…');
    socket.onmessage = ({ data }) => { this.lastFrame = Date.now(); this.pending = this.pending.then(async () => {
      if (socket !== this.socket || this.stopped) return;
      const frame = JSON.parse(data);
      if (frame.type === 'event') {
        if (frame.event.seq > this.seq) {
          if (this.ready) {
            await this.onEvent(frame.event);
            this.seq = frame.event.seq;
          } else this.replayDirty = true;
        }
      } else if (frame.type === 'ready') {
        // Catch-up frames invalidate the snapshot. Refresh once instead of
        // applying old activities and inbox alerts one event at a time. Do
        // not acknowledge replay before that refresh succeeds: a failed
        // snapshot must replay the same events on the next connection.
        if (this.replayDirty) this.seq = Math.max(this.seq, await this.onSnapshot());
        this.replayDirty = false;
        this.seq = Math.max(this.seq, frame.seq); this.delay = 250; this.onState('Live');
        this.ready = true;
        if (frame.heartbeat > 0 && socket === this.socket) { this.beat = frame.heartbeat * 1000; this.lastFrame = Date.now(); this.watchdog = setInterval(() => this.check(), this.beat); }
        // The build serving this page is the one it first heard from: a different one after a reconnect means the server was replaced (a deploy).
        if (frame.version) {
          // `baseline()` is the build that served the page (read from the server when the page signed in), so a page already older than the server
          // says so on its first ready; without one, the first build heard from is taken as the page's own.
          const known = this.baseline?.() || this.version;
          if (!known) this.version = frame.version;
          else if (frame.version !== known && !this.versionChanged) { this.versionChanged = true; this.onVersionChange?.(frame.version, known); }
        }
      } else if (frame.type === 'usages_updated') {
        // Transient hint with no seq: nothing to replay or acknowledge.
        if (this.ready) this.onUsagesUpdated?.();
      } else if (frame.type === 'snapshot_required') {
        this.resyncing = true;
        this.socket = null;
        socket.close();
        try {
          this.seq = await this.onSnapshot();
          this.replayDirty = false;
          this.resyncing = false;
          this.connect();
        } catch {
          this.resyncing = false;
          this.onState('Reconnecting…');
          this.timer = setTimeout(() => this.connect(), this.delay);
          this.delay = Math.min(this.delay * 2, 10000);
        }
      }
    }).catch(() => { socket.close(); }); };
    socket.onclose = () => {
      if (this.stopped || socket !== this.socket || this.resyncing) return;
      this.onState('Reconnecting…');
      this.timer = setTimeout(() => this.connect(), this.delay);
      this.delay = Math.min(this.delay * 2, 10000);
    };
  }
}
