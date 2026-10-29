import { batch, signal } from '../../ui.js';
import { request } from '../../api.js';
import { inviteToken } from './model.js';

// Every answer is checked against `ticket`: show(), hide(), a submit, closing the dialog and destroy() start a new one, so a reply that arrives after the person
// moved on (another view, a replaced sign-in, a destroyed page) changes nothing. The host does the effects that leave this page.
export class SigninStore {
  constructor({ signedIn, afterReset, resync, dropInviteLink, hash = location.hash }) {
    this.host = { signedIn, afterReset, resync, dropInviteLink };
    this.pending = inviteToken(hash.slice(1));
    this.shown = signal(false);
    this.mode = signal('login');
    this.info = signal(null);
    this.redeemed = signal('');
    this.back = signal(false);
    this.loginError = signal('');
    this.cardError = signal('');
    this.dialogError = signal('');
    this.dialogOpen = signal(false);
    this.ticket = 0;
    this.busy = { login: false, card: false, dialog: false };
    this.alive = true;
  }

  current(ticket) { return this.alive && ticket === this.ticket; }

  next() { this.ticket++; this.busy = { login: false, card: false, dialog: false }; return this.ticket; }

  async show() {
    const ticket = this.next();
    this.shown.value = true;
    if (!this.pending) { this.mode.value = 'login'; return; }
    const token = this.pending;
    const info = await request('/api/invites/inspect', { method: 'POST', body: { token } }).catch(error => ({ error }));
    if (!this.current(ticket) || token !== this.pending) return;
    batch(() => { this.info.value = info; this.cardError.value = ''; this.mode.value = 'card'; });
  }

  hide() { this.next(); this.shown.value = false; }

  async guarded(kind, error, run) {
    if (!this.alive || this.busy[kind]) return;
    const ticket = this.next();
    this.busy[kind] = true; error.value = '';
    try { await run(ticket); }
    catch (failure) { if (this.current(ticket)) error.value = failure.message; }
    finally { if (this.current(ticket)) this.busy[kind] = false; }
  }

  submitLogin(login, password) {
    return this.guarded('login', this.loginError, async ticket => {
      const session = await request('/api/login', { method: 'POST', body: { login, password } });
      if (this.current(ticket)) await this.host.signedIn(session);
    });
  }

  submitCard(login, password) {
    return this.guarded('card', this.cardError, async ticket => {
      if (!this.redeemed.value) {
        const reset = this.info.value?.kind === 'reset';
        const result = await request('/api/invites/redeem', { method: 'POST', body: { token: this.pending, ...(reset ? {} : { login: login.trim() }), password } });
        if (!this.current(ticket)) return;
        this.pending = ''; this.host.dropInviteLink(); this.redeemed.value = result.login;
      }
      const session = await request('/api/login', { method: 'POST', body: { login: this.redeemed.value, password } });
      if (this.current(ticket)) await this.host.signedIn(session);
    });
  }

  submitDialog(password) {
    return this.guarded('dialog', this.dialogError, async ticket => {
      const result = await request('/api/invites/redeem', { method: 'POST', body: { token: this.pending, password } });
      if (!this.current(ticket)) return;
      this.pending = ''; this.dialogOpen.value = false;
      if (result.kind === 'reset') this.host.afterReset(); else await this.host.resync();
    });
  }

  // "I already have an account": the link stays pending (offered after sign-in) unless it could not be used at all.
  toLogin() {
    this.next();
    if (this.info.value?.error) this.pending = '';
    batch(() => { this.mode.value = 'login'; this.back.value = !!this.pending; });
  }

  toInvite() { this.back.value = false; return this.show(); }

  openDialog() { if (!this.dialogOpen.value) this.dialogOpen.value = true; }

  closeDialog() { if (!this.dialogOpen.value) return; this.next(); this.dialogOpen.value = false; }

  destroy() { this.alive = false; this.ticket++; }
}
