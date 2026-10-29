// The item an arrow key or Home/End moves to among `count` visible items, from the one at `at`; -1 when there is nowhere to go.
export function nextIndex(count, at, key) {
  if (!count) return -1;
  if (key === 'Home') return 0;
  if (key === 'End') return count - 1;
  return (at + (key === 'ArrowDown' ? 1 : count - 1)) % count;
}

export const MOVE_KEYS = ['ArrowDown', 'ArrowUp', 'Home', 'End'];

export const visibleIndexes = items => items.flatMap((item, index) => (item.hidden ? [] : [index]));
