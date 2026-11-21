// A quiet bar that tells the admins what the server's own update check found: a newer
// release for the channel (`available`), or an update that was applied while the server
// kept running (`installed`, a restart picks it up), or a checkout that moved while the built binary did not (`unbuilt`, rebuild first). The wrapper does the updating on
// the host (`aimebu update`); the server never prompts, so the bar only reports and
// offers copy, remind (24 h) and skip. A skip hides that version and state for good, a
// remind hides it until the deadline; both are remembered on the account.
import { iconButton, iconGlyph } from './icon-button.js';
import { copyText } from './markdown.js';

// Whether the bar may be shown now: the server reported a state, the person is an admin
// or the owner, and the answer they gave last does not still cover this version and
// state.
export function updateBannerWanted({ status, rights, ack, now = Date.now() }) {
  if (!status || !status.state || !(rights.admin || rights.owner)) return false;
  if (!ack || ack.version !== status.version || ack.state !== status.state) return true;
  if (ack.kind === 'skip') return false;
  if (ack.kind === 'remind') return !ack.until || Date.parse(ack.until) <= now;
  return true;
}

export class UpdateBanner {
  // `getStatus` reads GET /api/update-status; `ack` posts skip or remind; `isMenuRoom`
  // keeps the bar off screens where it has no place (the sign-in page).
  constructor({ getStatus, ack, anchor = document.querySelector('.chat > header') }) {
    this.getStatus = getStatus; this.acknowledge = ack;
    this.status = null; this.ackRecord = null; this.timer = null; this.rights = { admin: false, owner: false };
    this.bar = document.createElement('div'); this.bar.className = 'update-banner'; this.bar.hidden = true;
    this.bar.setAttribute('role', 'region'); this.bar.setAttribute('aria-label', 'aimebu update');
    this.text = document.createElement('span'); this.text.className = 'update-banner-text';
    this.copy = iconButton('copy', 'Copy the update command', { className: 'update-banner-copy' });
    this.remind = iconButton('bell', 'Remind me again in 24 hours', { className: 'update-banner-remind' });
    this.skip = iconButton('close', 'Skip this version and state', { className: 'update-banner-skip' });
    this.bar.append(iconGlyph('health'), this.text, this.copy, this.remind, this.skip);
    if (anchor) anchor.after(this.bar);
    this.copy.onclick = () => void copyText('aimebu update');
    this.remind.onclick = () => void this.answer('remind');
    this.skip.onclick = () => void this.answer('skip');
  }
  // The account's rights and saved ack, read at sign-in; a new sign-in starts from the
  // server's own answer again.
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
    // A remind hides the bar only until its deadline; wake up then, so a page left
    // open shows it again without waiting for the next server event.
    clearTimeout(this.timer); this.timer = null;
    const until = this.ackRecord?.kind === 'remind' && this.ackRecord.version === this.status?.version && this.ackRecord.state === this.status?.state ? Date.parse(this.ackRecord.until) : NaN;
    if (Number.isFinite(until) && until > Date.now()) this.timer = setTimeout(() => this.sync(), Math.min(until - Date.now() + 50, 2 ** 31 - 1));
    const wanted = updateBannerWanted({ status: this.status, rights: this.rights, ack: this.ackRecord });
    if (!wanted) { this.bar.hidden = true; return false; }
    const version = this.status.version || '';
    this.text.replaceChildren();
    const versionPart = document.createElement('strong'); versionPart.textContent = version;
    if (this.status.state === 'unbuilt') {
      this.text.append(versionPart, ' is checked out but the built binary is older — delete the cached binary in the clone (', Object.assign(document.createElement('code'), { textContent: 'aimebu-<os>-<arch>' }), '), run ', Object.assign(document.createElement('code'), { textContent: 'aimebu version' }), ' once to rebuild it, then restart the server.');
    } else if (this.status.state === 'installed') {
      this.text.append(versionPart, ' is installed on disk but this server still runs the old build — restart the server to pick it up.');
    } else {
      this.text.append(versionPart, ' is available on this channel — run ', Object.assign(document.createElement('code'), { textContent: 'aimebu update' }), ' on the server host.');
    }
    this.bar.hidden = false;
    return true;
  }
  async answer(kind) {
    // Remember the answer and close the bar only once the server took it; a refused or
    // lost answer keeps the bar and says why in its tooltip.
    try { await this.acknowledge(kind); } catch (error) {
      this.bar.title = `Could not save your answer: ${error?.message || 'the request failed'}`;
      return;
    }
    this.bar.title = '';
    if (kind === 'remind') {
      this.ackRecord = { version: this.status?.version, state: this.status?.state, kind, until: new Date(Date.now() + 24 * 3600 * 1000).toISOString() };
    } else {
      this.ackRecord = { version: this.status?.version, state: this.status?.state, kind, until: null };
    }
    this.sync();
  }
}
