import { html } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { UsagesStore } from './store.js';
import { QuotaStore } from './quota-store.js';
import { GuardStore } from './guard-store.js';
import { LimitDefaultStore } from './limit-default.js';
import { UsagesPage } from './usages-page.js';

const focused = (node, selector) => !!node && node.contains(document.activeElement) && document.activeElement.matches(selector);

// Settings -> Usages: the provider cards and the account switcher, the automatic-switching and limit-guard blocks, and the default daily
// limits, all one tree in the section. The stores are objects of signals made once per page, because the shell and the event stream reach
// them too; a read of the usages is followed by a read of the two blocks that depend on whether the owner has the switcher on.
export class UsagesSettings {
  constructor({ source, quotaSource, guardSource, dialog, section }) {
    this.quota = new QuotaStore({ source: quotaSource, isEditing: () => focused(this.quota.node, 'input, select, textarea') });
    this.guard = new GuardStore({ source: guardSource, isEditing: () => focused(this.guard.node, 'input, select') });
    this.limitDefault = new LimitDefaultStore();
    this.store = new UsagesStore({ source, section, isOpen: () => dialog.open, isShown: () => dialog.open && !section.hidden,
      afterLoad: async state => { await this.quota.sync({ owner: !!state?.owner, enabled: !!state?.enabled }); if (this.store.alive) await this.guard.sync({ owner: !!state?.owner }); } });
    this.page = mountPreactPage(section, () => html`<${UsagesPage} page=${this} />`);
  }

  load() { return this.store.load(); }
  changed() { return this.store.changed(); }
  setError(text) { this.store.setError(text); }
  // The default daily limits do not depend on the switcher or the usage snapshots, so they load on their own when Settings opens.
  syncDefault() { return this.limitDefault.sync(); }

  destroy() {
    for (const store of [this.store, this.quota, this.guard, this.limitDefault]) store.destroy();
    this.page.destroy();
  }
}
