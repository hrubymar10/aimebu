import { html } from '../../ui.js';
import { SwitcherControls } from './switcher-controls.js';
import { QuotaBlock } from './quota-block.js';
import { GuardBlock } from './guard-block.js';
import { LimitDefault } from './limit-default.js';
import { UsagesList } from './usages-list.js';

// The whole Usages section, in the order it has always had.
export function UsagesPage({ page }) {
  return html`
    <h3>Usages</h3>
    <p class="settings-intro">Provider limits and account profiles</p>
    <p id="usages-error" class="error" role="alert">${page.store.error.value}</p>
    <div class="usage-switcher-controls" id="usages-switcher"><${SwitcherControls} store=${page.store} /></div>
    <${QuotaBlock} store=${page.quota} />
    <${GuardBlock} store=${page.guard} />
    <${LimitDefault} store=${page.limitDefault} />
    <div id="usages-list"><${UsagesList} store=${page.store} /></div>
  `;
}
