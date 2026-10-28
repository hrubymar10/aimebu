import { html } from '../../ui.js';
import { SORTS } from './model.js';

// The sort button in the ROOMS header opens a small menu of the three orders.
export function SortMenu({ store, onPick }) {
  const current = store.sort.value;
  return html`${SORTS.map(([key, label]) => html`<button type="button" key=${key} class=${key === current ? 'row-btn sort-item is-selected' : 'row-btn sort-item'} data-sort=${key} role="menuitemradio" aria-checked=${String(key === current)} onClick=${() => onPick(key)}><span class="icon-mask icon-mask-check" aria-hidden="true"></span>${label}</button>`)}`;
}
