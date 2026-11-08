// Drag handles in the gaps between the panels: on the inner edge of the rooms column (left) and the members column (right). The widths are CSS variables on the app element
// (--rooms-w, --agents-w), so neither a drag nor a window resize redraws anything: the browser only lays the page out again. They are kept per
// browser in localStorage (screen sizes differ, so they are not an account preference), clamped to a range and so that the chat keeps a minimum
// width, and reset by a double click. A handle is a keyboard control too: a focusable vertical separator with arrow, Home and End keys.
const STORAGE_KEY = 'aimebu2.ui.column-widths';
export const COLUMNS = {
  rooms: { variable: '--rooms-w', label: 'Resize the rooms column', min: 180, max: 480, fallback: 240, side: 'left' },
  agents: { variable: '--agents-w', label: 'Resize the members column', min: 200, max: 520, fallback: 260, side: 'right' },
};
// The thread column is a third handle, on the thread panel's left edge; it is not part of the rooms/members layout maths, only of what the chat gives up.
export const THREAD = { variable: '--thread-w', label: 'Resize the thread column', min: 320, max: 760, fallback: 420, side: 'right' };
const SPECS = { ...COLUMNS, thread: THREAD };
export const CHAT_MIN = 360;
const STEP = 16;
// How long the grips stay after the pointer stops moving over the window (the CSS fades them out over a quarter of a second).
export const GRIP_HOLD_MS = 3000;
// The window width from which the thread panel sits beside the members column (below that it takes the column's place).
const THREAD_BESIDE = 1280;
const PHONE = '(max-width: 700px)';
// The panels' gaps (theme-v1.css, --gap): one at each side of the window, and one between neighbouring panels.
const GAP = 6;

export class ColumnResize {
  constructor({ app = document.querySelector('#app-view'), chat = document.querySelector('.chat'), thread = document.querySelector('#thread-panel'), storage = localStorage, gripHold = GRIP_HOLD_MS } = {}) {
    this.app = app; this.chat = chat; this.thread = thread; this.storage = storage;
    this.wanted = this.read();
    this.handles = {}; this.ends = [];
    for (const name of Object.keys(SPECS)) this.handles[name] = this.build(name);
    this.app.append(this.handles.rooms); this.chat.append(this.handles.agents, this.handles.thread);
    window.addEventListener('resize', () => this.apply());
    // The grips show while the pointer moves over the window and for `gripHold` ms after it stops (html.col-grips; the CSS fades them).
    let gripTimer = 0, lastMove = 0;
    window.addEventListener('pointermove', () => {
      const now = Date.now(); if (document.documentElement.classList.contains('col-grips') && now - lastMove < 100) return; lastMove = now;
      document.documentElement.classList.add('col-grips'); clearTimeout(gripTimer);
      gripTimer = setTimeout(() => document.documentElement.classList.remove('col-grips'), gripHold);
    }, { passive: true });
    // A drag that loses its pointer (the window lost focus, the pointer was taken away) ends like a release, so the page is never left in the dragging state.
    window.addEventListener('blur', () => { for (const end of this.ends) end(); });
    const watch = (node, attributeFilter) => node && new MutationObserver(() => this.apply()).observe(node, { attributes: true, attributeFilter });
    watch(this.app, ['class']); watch(this.chat, ['class']); watch(this.thread, ['hidden']);
    this.apply();
  }
  read() { try { const saved = JSON.parse(this.storage.getItem(STORAGE_KEY) || '{}'); return Object.fromEntries(Object.keys(SPECS).filter(name => Number.isFinite(saved[name])).map(name => [name, saved[name]])); } catch { return {}; } }
  write() { try { if (Object.keys(this.wanted).length) this.storage.setItem(STORAGE_KEY, JSON.stringify(this.wanted)); else this.storage.removeItem(STORAGE_KEY); } catch { /* private browsing: the widths last as long as the page */ } }
  phone() { return window.matchMedia(PHONE).matches; }
  threadOpen() { return !!this.thread && !this.thread.hidden && !this.app.classList.contains('no-room'); }
  // Whether the column takes room beside the chat: not on a phone (a drawer), not collapsed, and for the members column not while there is no room
  // or while an open thread has taken its place (below THREAD_BESIDE the thread is shown instead of the column).
  present(name) {
    if (this.phone()) return false;
    if (name === 'rooms') return !this.app.classList.contains('rooms-collapsed');
    return !this.chat.classList.contains('agents-collapsed') && !this.app.classList.contains('no-room') && !(this.threadOpen() && window.innerWidth < THREAD_BESIDE);
  }
  // Whether its handle is there to use: the column is present, and the Usages overview is not standing in for the members column.
  shown(name) { if (name === 'thread') return this.threadOpen() && !this.phone(); return this.present(name) && (name === 'rooms' || !this.chat.classList.contains('usages-open')); }
  // What the gaps take from the chat on a desktop window: a gap at each side of the window and one beside each column that is there.
  chrome(columns) { return 2 * GAP + columns * GAP; }
  // What the chat gives up besides the two columns: the thread panel, which sits at the chat's right edge whenever it is open.
  reserve() { return this.threadOpen() ? this.threadWidth() : 0; }
  // The thread's width: what is wanted within its own range, and within what the columns that are present leave the chat at their minimums.
  threadWidth() { return Math.max(THREAD.min, Math.min(this.width('thread'), this.threadRoom())); }
  threadRoom() {
    const present = Object.keys(COLUMNS).filter(name => this.present(name));
    return Math.min(THREAD.max, window.innerWidth - CHAT_MIN - this.chrome(present.length) - present.reduce((sum, name) => sum + COLUMNS[name].min, 0));
  }
  // The widths the columns get now, given what is wanted: each within its own range, and, when both are present, together leaving the chat its
  // minimum (and the thread its room). When they do not fit, the shortfall is taken from each in proportion to how far it is above its own minimum
  // (a window that shrinks pulls both in, and the wide one more); with one column present it simply may not leave the chat less than the minimum.
  resolve() {
    const widths = Object.fromEntries(Object.keys(COLUMNS).map(name => [name, this.width(name)])), present = Object.keys(COLUMNS).filter(name => this.present(name));
    const available = window.innerWidth - CHAT_MIN - this.reserve() - this.chrome(present.length);
    if (present.length === 2) {
      const excess = widths.rooms + widths.agents - available;
      const slack = { rooms: widths.rooms - COLUMNS.rooms.min, agents: widths.agents - COLUMNS.agents.min }, total = slack.rooms + slack.agents;
      if (excess > 0 && total > 0) {
        const cut = Math.min(excess, total), rooms = Math.round(cut * slack.rooms / total);
        widths.rooms -= rooms; widths.agents -= Math.min(slack.agents, cut - rooms);
      }
    } else if (present.length === 1) {
      const name = present[0]; widths[name] = Math.max(COLUMNS[name].min, Math.min(widths[name], available));
    }
    return widths;
  }
  // The range a column may be dragged within now: its own, and what the other column (as it is drawn) and the thread leave the chat.
  limits(name) {
    if (name === 'thread') return { min: THREAD.min, max: Math.max(THREAD.min, this.threadRoom()) };
    const spec = COLUMNS[name], other = name === 'rooms' ? 'agents' : 'rooms';
    const otherWidth = this.present(other) ? this.resolve()[other] : 0;
    return { min: spec.min, max: Math.max(spec.min, Math.min(spec.max, window.innerWidth - otherWidth - CHAT_MIN - this.reserve() - this.chrome(this.present(other) ? 2 : 1))) };
  }
  width(name) { const spec = SPECS[name], wanted = this.wanted[name] ?? spec.fallback; return Math.max(spec.min, Math.min(spec.max, wanted)); }
  effective(name) { return name === 'thread' ? this.threadWidth() : this.resolve()[name]; }
  build(name) {
    const spec = SPECS[name], handle = document.createElement('div');
    handle.className = `col-resizer col-resizer-${name}`; handle.setAttribute('role', 'separator'); handle.setAttribute('aria-orientation', 'vertical');
    handle.setAttribute('aria-label', spec.label); handle.tabIndex = 0; handle.dataset.column = name;
    let start = null;
    handle.addEventListener('pointerdown', event => {
      if (event.button !== 0 || !this.shown(name)) return;
      handle.setPointerCapture(event.pointerId); start = { x: event.clientX, width: this.effective(name) };
      handle.classList.add('dragging'); document.documentElement.classList.add('col-resizing'); // (no preventDefault: it would also swallow the double click)
    });
    handle.addEventListener('pointermove', event => {
      if (!start) return;
      const delta = event.clientX - start.x;
      this.set(name, start.width + (spec.side === 'left' ? delta : -delta), false);
    });
    const end = event => {
      if (!start) return;
      start = null; handle.classList.remove('dragging'); document.documentElement.classList.remove('col-resizing');
      if (event?.pointerId !== undefined) { try { handle.releasePointerCapture(event.pointerId); } catch { /* already released */ } }
      this.write();
    };
    this.ends.push(end);
    for (const type of ['pointerup', 'pointercancel', 'lostpointercapture']) handle.addEventListener(type, end);
    handle.addEventListener('dblclick', () => { delete this.wanted[name]; this.write(); this.apply(); });
    handle.addEventListener('keydown', event => {
      const grow = spec.side === 'left' ? 'ArrowRight' : 'ArrowLeft', shrink = spec.side === 'left' ? 'ArrowLeft' : 'ArrowRight';
      const { min, max } = this.limits(name), now = this.effective(name);
      let next = null;
      if (event.key === grow) next = now + STEP; else if (event.key === shrink) next = now - STEP; else if (event.key === 'Home') next = min; else if (event.key === 'End') next = max;
      if (next === null) return;
      event.preventDefault(); this.set(name, next, true);
    });
    return handle;
  }
  // Asks for a width (clamped when it is drawn); `persist` writes it down at once (a key press: a drag writes when the pointer is released).
  set(name, width, persist) {
    const { min, max } = this.limits(name);
    this.wanted[name] = Math.round(Math.max(min, Math.min(max, width)));
    if (persist) this.write();
    this.apply();
  }
  // Draws what is wanted within what fits now: the variables (kept for every column that takes room, so the layout that follows them, such as the
  // Usages overview in the members column's place, keeps its width), the handles' hidden state and their aria values.
  apply() {
    const widths = this.resolve();
    if (this.threadOpen() && !this.phone()) this.app.style.setProperty(THREAD.variable, `${this.threadWidth()}px`); else this.app.style.removeProperty(THREAD.variable);
    { const handle = this.handles.thread, { min, max } = this.limits('thread'), value = this.threadWidth();
      handle.hidden = !this.shown('thread');
      handle.setAttribute('aria-valuemin', String(min)); handle.setAttribute('aria-valuemax', String(max)); handle.setAttribute('aria-valuenow', String(value)); }
    for (const name of Object.keys(COLUMNS)) {
      const handle = this.handles[name], shown = this.shown(name), { min, max } = this.limits(name), value = widths[name];
      if (this.present(name)) this.app.style.setProperty(COLUMNS[name].variable, `${value}px`); else this.app.style.removeProperty(COLUMNS[name].variable);
      handle.hidden = !shown;
      handle.setAttribute('aria-valuemin', String(min)); handle.setAttribute('aria-valuemax', String(max)); handle.setAttribute('aria-valuenow', String(value));
    }
  }
}
