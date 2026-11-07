import { html } from '../../ui.js';
import { USAGE_LEGEND } from '../../usage-format.js';
import { sortProviders } from './model.js';
import { ProviderCard } from './provider-card.js';

export function UsagesList({ store }) {
  const usage = store.usage.value, state = store.switcher.value;
  if (!usage && !state) return null;
  const providers = sortProviders(usage?.providers);
  const shown = state || { enabled: false, profiles: [], eligibility: [], affected_agents: {} };
  const usedDisplay = usage?.settings?.percent_display === 'used';
  return html`${providers.map((provider, order) => html`<${ProviderCard} key=${provider.provider_name} store=${store} provider=${provider} order=${order} providers=${providers} state=${shown} usedDisplay=${usedDisplay} refreshSec=${usage?.settings?.refresh_interval_sec} revision=${store.revision.value} />`)}
    ${!providers.length && html`<p class="usage-empty">No usage providers are available yet.</p>`}
    ${providers.length > 0 && html`<p class="settings-desc usages-legend">${USAGE_LEGEND}</p>`}`;
}
