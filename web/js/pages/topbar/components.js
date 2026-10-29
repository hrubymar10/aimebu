import { html, useLayoutEffect } from '../../ui.js';

function Item({ store, item, index }) {
  return html`<button type="button" class="row-btn more-item" role="menuitem" hidden=${item.hidden} onClick=${() => store.activate(index)}><span>${item.label}</span>${item.badge && html`<span class="badge" hidden=${item.badge.hidden}>${item.badge.text}</span>`}</button>`;
}

export function MoreItems({ store, root }) {
  const focus = store.focus.value;
  useLayoutEffect(() => { if (focus.index >= 0) root.querySelectorAll('[role=menuitem]')[focus.index]?.focus(); }, [focus.n]);
  return store.items.value.map((item, index) => html`<${Item} key=${item.label} store=${store} item=${item} index=${index} />`);
}
