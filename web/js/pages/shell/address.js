// The address of a settings dialog (the global Settings, a room's, a project's): each level has its own hash, so Back, Forward and a copied link work. One
// implementation for all of them; what differs is the host:
//   parse(hash) -> {section, category, sub} | null   (null when the hash is not this dialog's address)
//   pathOf(target) -> string                          (the hash, without '#', of a level)
//   resolve(path) -> target                           (cut down to what the viewer may open)
//   show(target)                                      (draws the level)
//   open(path, {fromAddress})                         (opens the dialog on a path; the address was already there)
//   listPath() -> string                              (the address of the list of categories, for the phone's back arrow)
//   requestClose() -> Promise<boolean>                (closes, asking about unsaved edits; false when the person kept them)
//   currentRoom() -> string, ready() -> boolean, dialog
// depth is how many entries the dialog put on the history since it opened (each is marked in its state), trail what each showed, deep that it was opened by a link,
// with no room entry behind.
export class SettingsAddress {
  constructor(host, marker = 'aimebuSettings') {
    this.host = host; this.marker = marker;
    this.depth = 0; this.trail = []; this.deep = false; this.path = ''; this.unwinding = this.unwound = this.unwindTimer = null;
    this.onPop = event => void this.follow(event);
    window.addEventListener('popstate', this.onPop);
  }
  destroy() { window.removeEventListener('popstate', this.onPop); clearTimeout(this.unwindTimer); }

  // A click in the navigation: a new entry in the history, and the level shown.
  navigate(path) {
    const target = this.host.resolve(path);
    path = this.host.pathOf(target);
    if (path === this.path) return;
    this.push(path);
    this.host.show(target);
  }
  push(path) {
    this.depth += 1;
    this.trail[this.depth] = path; this.path = path;
    history.pushState({ [this.marker]: this.depth }, '', `${location.pathname}${location.search}#${path}`);
  }
  // The arrow on a phone: one level up. When the entry behind is the list it is a step back in the history; otherwise the list is a new entry.
  back() {
    const before = this.depth > 0 && this.trail[this.depth - 1] && this.host.parse(this.trail[this.depth - 1]);
    if (before && !before.category) history.back();
    else this.navigate(this.host.listPath());
  }
  // The dialog has just been shown: its first entry. Opened by a link the address is already this dialog's and is kept (no room behind it); otherwise a new entry.
  enter(path) {
    this.depth = 0; this.trail = [path]; this.path = path;
    if (this.host.parse(location.hash)) { this.deep = true; history.replaceState({ [this.marker]: 0 }, '', `${location.pathname}${location.search}#${path}`); return; }
    this.deep = false;
    this.push(path);
  }
  // The dialog closed: the address goes back to the room. Entries it pushed are walked back (so Forward is the way back in); a link that opened it has no
  // room behind it, so the entry is replaced by the room that is open.
  leave() {
    const depth = this.depth, deep = this.deep;
    this.depth = 0; this.trail = []; this.path = ''; this.deep = false;
    if (!this.host.parse(location.hash)) return; // the Back button already did it
    const room = this.host.currentRoom?.() || '';
    const base = `${location.pathname}${location.search}`;
    if (deep || depth === 0) history.replaceState(null, '', room ? `${base}#${encodeURIComponent(room)}` : base);
    else this.unwind(depth);
  }
  // history.go is asynchronous: its popstate arrives later, and must neither close a dialog opened in the meantime nor race its first push. The traversal is
  // tracked (settled by that popstate, or by a timeout when the browser never fires one) and open() waits for it.
  unwind(depth) {
    this.settleUnwind();
    this.unwinding = new Promise(resolve => { this.unwound = resolve; });
    this.unwindTimer = setTimeout(() => this.settleUnwind(), 1000);
    history.go(-depth);
  }
  settleUnwind() {
    clearTimeout(this.unwindTimer);
    const resolve = this.unwound;
    this.unwinding = this.unwound = null;
    resolve?.();
  }
  // The address changed under the dialog (Back, Forward, a typed link): its own address opens or moves it, any other closes it.
  async follow(event) {
    if (event && this.unwinding) { this.settleUnwind(); return; } // the popstate of our own close
    const target = this.host.parse(location.hash), dialog = this.host.dialog;
    if (!target) { if (dialog.open) { this.depth = 0; await this.closeAsked(); } return; }
    if (!this.host.ready()) return; // not signed in yet: the host looks again
    const state = event?.state?.[this.marker];
    this.depth = Number.isInteger(state) ? state : this.depth;
    this.path = this.host.pathOf(target); this.trail[this.depth] = this.path;
    if (!dialog.open) { this.deep = !Number.isInteger(state) || state === 0; void this.host.open(this.path, { fromAddress: true }); return; }
    this.host.show(this.host.resolve(this.path));
  }
  // Back with edits unsaved asks like the close button does; keeping them puts the address back.
  async closeAsked() {
    const path = this.path || this.host.listPath();
    if (await this.host.requestClose()) return;
    if (this.host.dialog.open) { this.depth = Math.max(this.depth, 1); history.pushState({ [this.marker]: this.depth }, '', `${location.pathname}${location.search}#${path}`); }
  }
}
