import { html } from '../../ui.js';
import { TABS } from './store.js';

export function MobileTabs({ store }) {
  const current = store.current.value;
  return TABS.map(([name, label]) => html`<button type="button" class=${name === current ? 'row-btn mobile-tab active' : 'row-btn mobile-tab'} data-tab=${name} aria-current=${name === current ? 'page' : undefined} onClick=${() => store.show(name)}>${label}</button>`);
}
