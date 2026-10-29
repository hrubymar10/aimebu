// The item an arrow key or Home/End moves to among `count` visible items, from the one at `at`; -1 when there is nowhere to go.
export function nextIndex(count, at, key) {
  if (!count) return -1;
  if (key === 'Home') return 0;
  if (key === 'End') return count - 1;
  return (at + (key === 'ArrowDown' ? 1 : count - 1)) % count;
}

export const MOVE_KEYS = ['ArrowDown', 'ArrowUp', 'Home', 'End'];

export const visibleIndexes = items => items.flatMap((item, index) => (item.hidden ? [] : [index]));

// The menu's items in the order the bar gives them up when a phone is too narrow for every control: Sign out, System events, Memory, then Health
// (indexes into the host's items).
export const TUCK_ORDER = [3, 0, 2, 1];

// Which items move from the bar into the menu so that the bar fits `available` pixels. `entries` are the bar's shown controls, each with its
// width and the index of the menu item it stands for (-1 for one that is never tucked); `gap` separates neighbours and `moreWidth` is the menu's
// own button, which only takes room once something is tucked. Returns the tucked item indexes in the order they went, none when everything fits.
export function planTuck({ available, gap, moreWidth, entries, order = TUCK_ORDER }) {
  const need = (list, withMore) => list.reduce((sum, entry) => sum + entry.width, withMore ? moreWidth : 0) + Math.max(0, list.length + (withMore ? 1 : 0) - 1) * gap;
  if (need(entries, false) <= available + 0.5) return [];
  const tucked = [];
  let rest = entries;
  for (const index of order) {
    if (!rest.some(entry => entry.item === index)) continue;
    tucked.push(index);
    rest = rest.filter(entry => entry.item !== index);
    if (need(rest, true) <= available + 0.5) break;
  }
  return tucked;
}
