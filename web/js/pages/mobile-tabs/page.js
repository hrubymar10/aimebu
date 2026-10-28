import { html } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { MobileTabs } from './mobile-tabs.js';
import { MobileTabsStore } from './store.js';

// The bar's element stays in index.html and the page fills it; the handle's destroy() empties it again.
export function initMobileTabs() {
  const bar = document.querySelector('#mobile-tabs');
  if (!bar) return null;
  const store = new MobileTabsStore({ app: document.querySelector('#app-view'), agentsPanel: document.querySelector('#agents-panel') });
  const page = mountPreactPage(bar, () => html`<${MobileTabs} store=${store} />`);
  return { store, destroy() { store.destroy(); page.destroy(); } };
}
