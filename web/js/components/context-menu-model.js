// The plain rules of the shared context menu (components/context-menu.js): where it goes on the screen, which item a key moves to, and which
// key opens it. Import-free, so `make test-web` runs it in a vm.

// The gap kept between the menu and the edge of the viewport.
export const MENU_MARGIN = 8;

// Where a menu of `size` ({ width, height }) opens for a point ({ x, y }) inside a viewport ({ width, height }): at the point, flipped to the
// left or above it when it would cross the right or bottom edge (moved only as far as needed when neither side fits), and never closer than MENU_MARGIN to any edge. A menu taller than the viewport
// gets `maxHeight` and scrolls inside itself.
export function placeMenu(point, size, viewport, margin = MENU_MARGIN) {
  const room = { width: Math.max(0, viewport.width - 2 * margin), height: Math.max(0, viewport.height - 2 * margin) };
  const width = Math.min(size.width, room.width), height = Math.min(size.height, room.height);
  let left = point.x, top = point.y;
  // Past the right or bottom edge: the other side of the point when it fits there, else as far right or as low as fits.
  if (left + width > viewport.width - margin) left = point.x - width >= margin ? point.x - width : viewport.width - margin - width;
  if (top + height > viewport.height - margin) top = point.y - height >= margin ? point.y - height : viewport.height - margin - height;
  left = Math.min(Math.max(left, margin), Math.max(margin, viewport.width - margin - width));
  top = Math.min(Math.max(top, margin), Math.max(margin, viewport.height - margin - height));
  return { left: Math.round(left), top: Math.round(top), maxHeight: size.height > room.height ? room.height : null };
}

// The item a key moves the focus to among `count` items, from `index` (-1 when none has it): the arrows wrap around, Home and End go to the
// ends. Null for any other key.
export function stepIndex(key, index, count) {
  if (!count) return null;
  if (key === 'ArrowDown') return index < 0 ? 0 : (index + 1) % count;
  if (key === 'ArrowUp') return index < 0 ? count - 1 : (index - 1 + count) % count;
  if (key === 'Home') return 0;
  if (key === 'End') return count - 1;
  return null;
}

// Whether a key press asks for the context menu of what has the focus: the context-menu key, or Shift+F10.
export const opensMenu = event => event.key === 'ContextMenu' || (event.key === 'F10' && !!event.shiftKey && !event.ctrlKey && !event.altKey && !event.metaKey);

// The rows as drawn. An item is { id, label, icon?, onSelect, disabled?, checked?, hidden? } (`checked` makes it a radio item), a separator is
// { separator: true }, and a group is { group: 'its name', items: [...] }, drawn as one labelled set (the notification levels). Hidden items are
// dropped, then empty groups, and separators are kept only between rows (never first, last or twice in a row).
export function menuRows(items) {
  const rows = [];
  for (const item of items || []) {
    if (!item || item.hidden) continue;
    if (item.separator) { if (rows.length && !rows[rows.length - 1].separator) rows.push(item); continue; }
    if (item.group !== undefined) {
      const shown = (item.items || []).filter(inner => inner && !inner.hidden && !inner.separator && inner.group === undefined);
      if (shown.length) rows.push({ ...item, items: shown });
      continue;
    }
    rows.push(item);
  }
  while (rows.length && rows[rows.length - 1].separator) rows.pop();
  return rows;
}
