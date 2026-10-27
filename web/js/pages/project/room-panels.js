import { html, effect } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { ScopeControl, ScopeStore } from '../../components/scope-control.js';
import { UsageSummary, UsageSummaryStore } from '../../components/usage-summary.js';

export class RoomScopeControl {
  constructor(section) {
    this.store = new ScopeStore('room');
    this.page = mountPreactPage(section, () => html`<h3>Agents homed in this room</h3><${ScopeControl} store=${this.store} />`);
    this.stop = effect(() => { section.dataset.desired = this.store.state.value?.desired || ''; });
  }

  load(id) { return this.store.load(id); }
  reset() { this.store.reset(); }
  destroy() { this.stop(); this.store.destroy(); this.page.destroy(); }
}

export class RoomUsage {
  constructor(section) {
    this.store = new UsageSummaryStore(id => `/api/rooms/${id}/usage`);
    this.page = mountPreactPage(section, () => html`<${UsageSummary} store=${this.store} />`);
  }

  load(id) { return this.store.load(id); }
  destroy() { this.store.destroy(); this.page.destroy(); }
}
