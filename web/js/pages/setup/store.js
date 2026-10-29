import { batch, signal } from '../../ui.js';
import { request, setSession } from '../../api.js';
import { loadPreferences, savePreference } from '../../preferences.js';
import { THEMES } from '../general/model.js';
import { checkOwner, nextTheme, problemText } from './model.js';

// Before anyone has signed in there is no stored theme: the setup pages follow the operating system's light or dark setting.
export const systemTheme = () => matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark';

// The first-run owner setup as one run: the welcome, the setup token, the owner, the theme. Every request takes a ticket; a later submit,
// and destroy(), start a new one, so an answer that comes late changes nothing. The host shows the steps and ends with the session.
export class SetupStore {
  constructor({ send = request, welcome = async () => {}, linked = '', replaceUrl = () => {} } = {}) {
    this.send = send; this.welcome = welcome; this.linked = linked; this.replaceUrl = replaceUrl;
    this.step = signal('idle');
    this.hintPath = signal('');
    this.tokenError = signal('');
    this.tokenValue = signal('');
    this.tokenFocus = signal({ n: 0, select: false });
    this.ownerError = signal('');
    this.busy = { token: signal(false), owner: signal(false) };
    this.theme = signal(document.documentElement.dataset.theme || 'dark');
    this.ticket = 0; this.alive = true; this.completed = false; this.token = '';
    this.result = new Promise(resolve => { this.resolve = resolve; });
  }

  current(ticket) { return this.alive && ticket === this.ticket; }

  next() { return ++this.ticket; }

  focusToken(select) { this.tokenFocus.value = { n: this.tokenFocus.value.n + 1, select }; }

  // Resolves with the session once the owner is made and a theme is chosen.
  async run() {
    this.theme.value = systemTheme();
    await this.welcome();
    if (!this.alive) return this.result;
    const ticket = this.next();
    let path = '';
    try { path = (await this.send('/api/setup/status')).token_path || ''; } catch { /* the hint then names only the server output */ }
    if (!this.current(ticket)) return this.result;
    this.hintPath.value = path;
    if (this.linked) { await this.check(this.linked, true); return this.result; }
    batch(() => { this.step.value = 'token'; this.focusToken(false); });
    return this.result;
  }

  // A token from the link is checked at once and, when it is right, the token step is skipped; a wrong one is put in the field.
  async check(value, linked) {
    const ticket = this.next();
    if (!linked) this.busy.token.value = true;
    try {
      await this.send('/api/setup/token', { method: 'POST', body: { token: value } });
      if (!this.current(ticket)) return;
      this.token = value; this.step.value = 'owner';
    } catch (problem) {
      if (!this.current(ticket)) return;
      batch(() => {
        if (linked) this.tokenValue.value = value;
        this.tokenError.value = problemText(problem); this.busy.token.value = false; this.step.value = 'token'; this.focusToken(!linked);
      });
    }
  }

  submitToken(typed) {
    if (this.busy.token.value) return Promise.resolve();
    this.tokenError.value = '';
    const value = typed.trim();
    if (!value) { this.tokenError.value = 'Enter the setup token.'; return Promise.resolve(); }
    return this.check(value, false);
  }

  async submitOwner(login, password, confirm) {
    if (this.busy.owner.value) return;
    this.ownerError.value = '';
    const checked = checkOwner(login, password, confirm);
    if (checked.error) { this.ownerError.value = checked.error; return; }
    const ticket = this.next();
    this.busy.owner.value = true;
    try {
      const session = await this.send('/api/setup', { method: 'POST', body: { token: this.token, login: checked.name, password } });
      if (!this.current(ticket)) return;
      // The welcome was already shown, so the app must not show it a second time.
      setSession(session);
      await this.send('/api/welcome/seen', { method: 'POST', body: {} }).catch(() => {});
      if (!this.current(ticket)) return;
      this.replaceUrl(); this.step.value = 'idle';
      await loadPreferences(session.user_id, { send: this.send });
      if (!this.current(ticket)) return;
      await this.startTheme(session.user_id);
      if (this.current(ticket)) this.resolve({ ...session, first_run: false });
    } catch (problem) {
      if (!this.current(ticket)) return;
      batch(() => { this.ownerError.value = problemText(problem); this.busy.owner.value = false; });
    }
  }

  // The theme step: a tile per theme, applied as soon as it is picked and saved to the profile. Resolves with the theme when Continue is pressed.
  startTheme(userID) {
    this.userID = userID;
    return new Promise(resolve => {
      this.themeDone = resolve;
      this.step.value = 'theme';
      this.select(this.theme.value);
    });
  }

  select(value) {
    if (!this.alive) return;
    this.theme.value = value;
    try { localStorage.setItem(`aimebu2.ui.theme.${this.userID}`, value); } catch { /* the saved copy is what counts */ }
    savePreference('theme', value, { send: this.send, live: () => this.alive || this.completed });
  }

  moveTheme(key) {
    const value = this.step.value === 'theme' ? nextTheme(THEMES, this.theme.value, key) : null;
    if (value) this.select(value);
    return value;
  }

  continueTheme() {
    if (!this.alive || this.step.value !== 'theme') return;
    this.step.value = 'done'; this.completed = true;
    this.themeDone(this.theme.value);
  }

  destroy() { this.alive = false; this.ticket++; }
}
