// The usages-ports experiment's B, as a signal: the Usages view and its
// settings read it, so the B-only controls appear and disappear with the pill
// instead of waiting for a reload. A keeps the current state.
import { signal } from './ui.js';
import { activeVariant } from './ui-variant.js';

export const usagesPorts = signal(activeVariant('usages-ports'));

document.addEventListener('ui-variant-change', event => {
  if (event.detail?.experiment === 'usages-ports') usagesPorts.value = event.detail.variant;
});
