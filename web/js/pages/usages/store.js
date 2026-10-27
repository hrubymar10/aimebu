import { signal, batch } from '../../ui.js';
import { tokenStatesPending, flowExpired, pollDelay } from './model.js';

const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));

// What the Usages page shows and does. The shell, the event stream and the page's two Preact trees (the account switcher above the
// automatic-switching blocks, and the provider list below them) all reach it, so it is an object of signals made once per page, not a hook.
// A read carries a generation: only the latest answer draws.
export class UsagesStore {
  constructor({ source, section, isOpen, isShown, afterLoad = async () => {} }) {
    this.source = source;
    this.section = section;
    this.isOpen = isOpen;
    this.isShown = isShown;
    this.afterLoad = afterLoad;
    this.usage = signal(null);
    this.switcher = signal(null);
    this.error = signal('');
    this.flow = signal(null);
    // Counts the answers that were drawn: the forms rebuild from the server's values each time, as a page redraw always did.
    this.revision = signal(0);
    this.generation = 0;
    this.flowToken = 0;
    this.pushLoading = false;
    this.pushDirty = false;
    this.alive = true;
  }

  async load() {
    const ticket = ++this.generation;
    const [usage, state] = await Promise.all([this.source.list(), this.source.switcher()]);
    if (!this.alive || ticket !== this.generation) return;
    batch(() => { this.usage.value = usage; this.switcher.value = state; this.revision.value++; });
    await this.afterLoad(state);
  }

  setError(text) { this.error.value = text; }

  async mutate(action, failed) {
    this.error.value = '';
    try {
      await action();
      if (this.alive) await this.load();
    } catch (failure) { if (this.alive) this.error.value = failure.message || failed; }
  }
  mutateSwitcher(action) { return this.mutate(action, 'Account switch failed'); }
  mutateUsage(action) { return this.mutate(action, 'Usage settings could not be saved'); }

  async startCopilot(host) {
    this.error.value = '';
    try {
      const flow = await this.source.copilotStart(host);
      if (!this.alive) return;
      this.flowToken++;
      this.flow.value = flow;
      await this.load();
      this.pollCopilot();
    } catch (failure) { if (this.alive) this.error.value = failure.message || 'GitHub sign-in could not start'; }
  }

  async pollCopilot() {
    const flow = this.flow.peek(), token = this.flowToken;
    if (!this.alive || !flow || flowExpired(flow)) return;
    try {
      const result = await this.source.copilotPoll(flow.flow_id);
      if (!this.alive || token !== this.flowToken) return;
      if (result.status === 'success') { this.flowToken++; this.flow.value = null; await this.load(); return; }
      this.flow.value = { ...flow, status: result.status };
      if (result.status === 'denied' || result.status === 'expired') { await this.load(); return; }
      this.pollTimer = setTimeout(() => { if (this.alive && token === this.flowToken) this.pollCopilot(); }, pollDelay(result, flow) * 1000);
    } catch (failure) { if (this.alive) this.error.value = failure.message || 'GitHub sign-in polling failed'; }
  }

  // The server converts the logins in the background and answers "pending": read again until it settles, as long as Settings is open.
  async saveTokenAutoRefresh(value) {
    this.error.value = '';
    try {
      await this.source.settings({ token_auto_refresh: value });
      if (!this.alive) return;
      await this.load();
      for (let attempt = 0; attempt < 120 && tokenStatesPending(this.usage.peek()); attempt++) {
        await sleep(500);
        if (!this.alive || !this.isOpen()) return;
        await this.load();
      }
    } catch (failure) {
      if (!this.alive) return;
      this.error.value = failure.message || `Could not ${value ? 'enable' : 'disable'} token auto-refresh`;
      await this.load().catch(() => {});
    }
  }

  // A push from the event stream reloads the open page in place. One that cannot be applied right away (a read is running, or a field
  // in the page has focus and a redraw would drop what is being typed) is remembered and applied once the read finishes or focus leaves.
  async changed() {
    if (!this.alive || !this.isShown()) return;
    if (this.pushLoading) { this.pushDirty = true; return; }
    const focused = document.activeElement;
    if (focused && this.section.contains(focused) && focused.matches('input, select, textarea')) {
      this.pushDirty = true;
      if (!this.focusWatch) {
        this.focusWatch = () => { if (this.pushDirty) setTimeout(() => { if (this.pushDirty) this.changed(); }, 0); }; // focusout fires before the next element takes focus
        this.section.addEventListener('focusout', this.focusWatch);
      }
      return;
    }
    this.pushDirty = false;
    this.pushLoading = true;
    try { await this.load(); } catch { /* the next push or open reads again */ } finally {
      this.pushLoading = false;
      if (this.pushDirty) this.changed();
    }
  }

  destroy() {
    this.alive = false;
    clearTimeout(this.pollTimer);
    if (this.focusWatch) this.section.removeEventListener('focusout', this.focusWatch);
  }
}
