import { signal } from '../../ui.js';

// Which Usages view is drawn: A (the page as it was), B (v1's tiles) or C. Kept per browser, like the group-by choice of the usages panel; a value that is
// not one of the three (or storage that is denied) means A.
export const VARIANTS = ['a', 'b', 'c'];
const key = 'aimebu2.usages.variant';
const read = () => { try { const value = localStorage.getItem(key); return VARIANTS.includes(value) ? value : 'a'; } catch { return 'a'; } };

export const usagesVariant = signal(read());
export function setUsagesVariant(value) {
  value = VARIANTS.includes(value) ? value : 'a';
  try { localStorage.setItem(key, value); } catch { /* storage may be denied: the choice then lasts until the page closes */ }
  usagesVariant.value = value;
}
