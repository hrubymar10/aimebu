// The three bars' state and transitions, import-free: scripts/test-update-banner.cjs and scripts/test-notification-banner.cjs run this module in a
// vm. What a bar needs from the page (the document, timers, the notification permission, the clipboard, the server) is handed in; each controller
// calls `onChange()` after a change, and the adapter (page.js) draws the bar again at once.

// ---- The update bar: the server is a different build from the one that served this page.
// A page that was loaded from one build keeps running after the server is replaced by another (a deploy): its scripts no longer match what
// the server serves. The event stream tells the page when the server it reconnected to is a different build; this bar asks for a reload, and
// the page reloads by itself once nobody is looking at it and nothing typed would be lost.

// Whether a reload would lose nothing: no dialog is open, the chat holds no draft or pending file (`hasDraft`, since the formatted composer's
// textarea is out of sight), and no other text field on show holds a draft (a field out of sight, in a closed dialog or a
// section not drawn, keeps what the server gave it and is no draft).
export function reloadIsSafe(doc = document, hasDraft = () => false) {
  if (doc.querySelector('dialog[open]') || hasDraft()) return false;
  return ![...doc.querySelectorAll('textarea, input[type=text], input:not([type])')].some(field => !field.disabled && String(field.value || '').trim() !== '' && field.type !== 'password' && field.getClientRects().length > 0);
}

// `kind` is null (no bar), 'update' (the event stream saw a new build) or 'stale' (a part of the page could not be fetched from the new build).
// The first kind shown stays: the bar is not drawn twice.
export class UpdateBarModel {
  constructor({ doc, reload, hasDraft, onChange = () => {} }) {
    this.doc = doc; this.reload = reload; this.hasDraft = hasDraft; this.onChange = onChange;
    this.kind = null;
  }
  maybeReload() { if (this.doc.hidden && reloadIsSafe(this.doc, this.hasDraft)) this.reload(); }
  show() {
    if (this.kind) return;
    this.kind = 'update'; this.onChange();
    this.maybeReload();
  }
  // Reload at once when nothing typed would be lost, otherwise keep a bar up (with a reload icon button) until the person reloads.
  stale() {
    if (reloadIsSafe(this.doc, this.hasDraft)) { this.reload(); return 'reloaded'; }
    if (!this.kind) { this.kind = 'stale'; this.onChange(); }
    return 'bar';
  }
  // Looking away is the moment a reload costs nothing. Returns the cleanup.
  watch(doc = this.doc) {
    const changed = () => { if (this.kind && doc.hidden) this.maybeReload(); };
    doc.addEventListener('visibilitychange', changed);
    return () => doc.removeEventListener?.('visibilitychange', changed);
  }
}

// ---- The update banner: what the server's own update check found, for the admins.
// A newer release for the channel (`available`), an update that was applied while the server kept running (`installed`, a restart picks it
// up), or a checkout that moved while the built binary did not (`unbuilt`, rebuild first). The wrapper does the updating on the host
// (`aimebu update`); the server never prompts, so the bar only reports and offers copy, remind (24 h) and skip. A skip hides that version and
// state for good, a remind hides it until the deadline; both are remembered on the account.

// Whether the bar may be shown now: the server reported a state, the person is an admin or the owner, and the answer they gave last does not
// still cover this version and state.
export function updateBannerWanted({ status, rights, ack, now = Date.now() }) {
  if (!status || !status.state || !(rights.admin || rights.owner)) return false;
  if (!ack || ack.version !== status.version || ack.state !== status.state) return true;
  if (ack.kind === 'skip') return false;
  if (ack.kind === 'remind') return !ack.until || Date.parse(ack.until) <= now;
  return true;
}

// What the bar says for a status, as parts: a string, or [tag, text] for a 'strong' or a 'code'.
export function updateBannerText(status) {
  const version = ['strong', status.version || ''];
  if (status.state === 'unbuilt') {
    return [version, ' is checked out but the built binary is older — delete the cached binary in the clone (', ['code', 'aimebu-<os>-<arch>'], '), run ', ['code', 'aimebu version'], ' once to rebuild it, then restart the server.'];
  }
  if (status.state === 'installed') return [version, ' is installed on disk but this server still runs the old build — restart the server to pick it up.'];
  return [version, ' is available on this channel — run ', ['code', 'aimebu update'], ' on the server host.'];
}

export const UPDATE_COMMAND = 'aimebu update';

// `getStatus` reads GET /api/update-status; `ack` posts skip or remind; `copy` puts text on the clipboard.
export class UpdateBannerModel {
  constructor({ getStatus, ack, copy, setTimeout: set, clearTimeout: clear, onChange = () => {} }) {
    this.getStatus = getStatus; this.acknowledge = ack; this.copyText = copy; this.setTimer = set; this.clearTimer = clear; this.onChange = onChange;
    this.status = null; this.ackRecord = null; this.timer = null; this.rights = { admin: false, owner: false };
    this.hidden = true; this.title = ''; this.text = [];
  }
  // The account's rights and saved ack, read at sign-in; a new sign-in starts from the server's own answer again.
  setAccount(rights, ackRecord) { this.rights = rights || { admin: false, owner: false }; this.ackRecord = ackRecord || null; return this.refresh(); }
  // One server check; unknown state or an error just closes the bar.
  async refresh() {
    try {
      const answer = await this.getStatus();
      this.status = answer && answer.checked ? { state: answer.state, version: answer.version } : null;
      this.ackRecord = answer?.ack || null;
    } catch { this.status = null; }
    return this.sync();
  }
  sync() {
    // A remind hides the bar only until its deadline; wake up then, so a page left open shows it again without waiting for the next server event.
    this.clearTimer(this.timer); this.timer = null;
    const until = this.ackRecord?.kind === 'remind' && this.ackRecord.version === this.status?.version && this.ackRecord.state === this.status?.state ? Date.parse(this.ackRecord.until) : NaN;
    if (Number.isFinite(until) && until > Date.now()) this.timer = this.setTimer(() => this.sync(), Math.min(until - Date.now() + 50, 2 ** 31 - 1));
    const wanted = updateBannerWanted({ status: this.status, rights: this.rights, ack: this.ackRecord });
    if (wanted) this.text = updateBannerText(this.status);
    this.hidden = !wanted;
    this.onChange();
    return wanted;
  }
  copy() { return this.copyText(UPDATE_COMMAND); }
  // Remember the answer and close the bar only once the server took it; a refused or lost answer keeps the bar and says why in its tooltip.
  async answer(kind) {
    try { await this.acknowledge(kind); } catch (error) {
      this.title = `Could not save your answer: ${error?.message || 'the request failed'}`;
      this.onChange();
      return;
    }
    this.title = '';
    const until = kind === 'remind' ? new Date(Date.now() + 24 * 3600 * 1000).toISOString() : null;
    this.ackRecord = { version: this.status?.version, state: this.status?.state, kind, until };
    this.sync();
  }
  // The bar is gone: no wake-up is left behind.
  stop() { this.clearTimer(this.timer); this.timer = null; }
}

// ---- The notification banner: an offer of browser notifications.
// It shows once, the first time something needs the person's attention (an Inbox item, or a chat message that addresses them) while the
// browser has not been asked yet: the permission prompt is only ever raised by a click on it. Dismissing it is remembered on the account (a
// preference), so no browser shows it again; granting or refusing the permission, from here or from Settings, ends it too, since the browser
// then no longer says "default". Nothing about the message is put in it.

export const BANNER_DISMISSED_PREFERENCE = 'notification_banner_dismissed';

// Whether the bar may be shown now. `shown` is true once it has been shown in this page (it is not shown twice, so closing nothing and
// looking away does not bring it back on every message).
export function bannerWanted({ available, permission, dismissed, shown }) {
  return !!available && permission === 'default' && !dismissed && !shown;
}

// `notifications` answers `available` and `requestPermission()`; `saveDismissed()` stores the dismissal on the account; `permission()` is the
// browser's answer ('default', 'granted', 'denied'); `isHidden()` whether the tab is in the background.
export class NotificationBannerModel {
  constructor({ notifications, saveDismissed, permission, isHidden, onChange = () => {} }) {
    this.notifications = notifications; this.saveDismissed = saveDismissed; this.permission = permission; this.isHidden = isHidden; this.onChange = onChange;
    this.dismissed = false; this.shown = false; this.waiting = false; this.hidden = true; this.enabling = false;
  }
  // Drops the bar, shown or waiting, once the browser no longer says "default".
  sync() { if (this.permission() !== 'default') { this.hidden = true; this.waiting = false; this.onChange(); } }
  // The account's saved dismissal, read at sign-in; a new sign-in starts from a closed bar.
  reset(dismissed) { this.dismissed = !!dismissed; this.shown = false; this.waiting = false; this.hidden = true; this.onChange(); }
  // Something needs the person's attention.
  attention() {
    if (!bannerWanted({ available: this.notifications.available, permission: this.permission(), dismissed: this.dismissed, shown: this.shown })) return;
    if (this.isHidden()) { this.waiting = true; return; }
    this.show();
  }
  show() { this.shown = true; this.hidden = false; this.onChange(); }
  async ask() {
    this.enabling = true; this.onChange();
    try { await this.notifications.requestPermission(); } catch { /* the browser refused to ask; the bar is done either way */ }
    this.close(false);
    this.enabling = false; this.onChange();
  }
  close(forGood) {
    this.hidden = true;
    if (forGood) { this.dismissed = true; void Promise.resolve(this.saveDismissed?.()).catch(() => {}); }
    this.onChange();
  }
  // A message that arrives while the tab is in the background is offered when the person comes back to it.
  visibilityChanged() { this.sync(); if (!this.isHidden() && this.waiting) { this.waiting = false; this.attention(); } }
  // Follows the tab's visibility and the permission, which can change behind the bar's back (from Settings, or in the browser's own site
  // settings): the bar goes with it. Returns the cleanup.
  watch(doc, permissions) {
    let alive = true, status = null;
    const changed = () => this.visibilityChanged();
    doc.addEventListener('visibilitychange', changed);
    try {
      permissions?.query({ name: 'notifications' }).then(answer => { if (!alive) return; status = answer; status.onchange = () => this.sync(); }, () => {});
    } catch { /* not every browser can be asked */ }
    return () => { alive = false; doc.removeEventListener?.('visibilitychange', changed); if (status) status.onchange = null; };
  }
}
