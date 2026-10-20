export class EventStream {
  constructor(onEvent, onSnapshot, onState, seq = 0) { this.onEvent = onEvent; this.onSnapshot = onSnapshot; this.onState = onState; this.seq = seq; this.delay = 250; this.stopped = true; this.pending = Promise.resolve(); }
  start() { this.stopped = false; this.connect(); }
  stop() { this.stopped = true; clearTimeout(this.timer); this.socket?.close(); }
  connect() {
    if (this.stopped) return;
    this.ready = false;
    const socket = new WebSocket(`${location.origin.replace(/^http/, 'ws')}/api/events?after=${this.seq}`);
    this.socket = socket;
    this.onState('Connecting…');
    socket.onmessage = ({ data }) => { this.pending = this.pending.then(async () => {
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
