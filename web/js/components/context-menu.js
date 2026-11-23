import { html, render, useLayoutEffect, useRef } from '../ui.js';
import { menuRows, opensMenu, placeMenu, stepIndex } from './context-menu-model.js';

// The one context menu of the page (a room row's, a message's): a body-level Preact root that holds at most one open menu. It is a real menu
// (`role=menu` with `menuitem` and `menuitemradio` items): the first item takes the focus, the arrows, Home and End move between the items and a
// letter jumps to the next item starting with it, Escape and Tab close it and give the focus back to where it was, and a click elsewhere, a
// scroll outside it, a resize or leaving the window close it. It opens at the point it was asked for, flipped and clamped into the viewport
// (context-menu-model.js). An item's icon is an icon-mask name from web/icons/, or a vnode for the inline glyphs of the message toolbar.

let host = null, current = null, opened = 0;

function Glyph({ icon }) {
  if (!icon) return html`<span class="context-menu-icon" aria-hidden="true"></span>`;
  if (typeof icon === 'string') return html`<span class=${`context-menu-icon icon-mask icon-mask-${icon}`} aria-hidden="true"></span>`;
  return html`<span class="context-menu-icon" aria-hidden="true">${icon}</span>`;
}

function Item({ item }) {
  const radio = item.checked !== undefined;
  const pick = () => { closeContextMenu(true); item.onSelect?.(); };
  return html`<button type="button" class=${item.danger ? 'context-menu-item danger' : 'context-menu-item'} role=${radio ? 'menuitemradio' : 'menuitem'} aria-checked=${radio ? String(!!item.checked) : undefined} data-action=${item.id} disabled=${!!item.disabled} title=${item.title} onClick=${pick}><${Glyph} icon=${radio ? (item.checked ? 'check' : '') : item.icon} /><span class="context-menu-label">${item.label}</span></button>`;
}

function Menu({ state }) {
  const box = useRef(null);
  useLayoutEffect(() => {
    const node = box.current;
    node.style.maxHeight = '';
    const rect = node.getBoundingClientRect(), root = document.documentElement;
    const place = placeMenu(state.point, { width: Math.ceil(rect.width), height: Math.ceil(node.scrollHeight + rect.height - node.clientHeight) }, { width: root.clientWidth || innerWidth, height: root.clientHeight || innerHeight });
    Object.assign(node.style, { left: `${place.left}px`, top: `${place.top}px`, maxHeight: place.maxHeight ? `${place.maxHeight}px` : '', visibility: 'visible' });
    node.querySelector('[role^="menuitem"]:not(:disabled)')?.focus({ preventScroll: true });
  }, [state]);
  const keys = event => {
    const items = [...box.current.querySelectorAll('[role^="menuitem"]:not(:disabled)')], index = items.indexOf(document.activeElement);
    if (event.key === 'Escape' || event.key === 'Tab' || opensMenu(event)) { event.preventDefault(); event.stopPropagation(); closeContextMenu(true); return; }
    const next = stepIndex(event.key, index, items.length);
    if (next !== null) { event.preventDefault(); items[next].focus(); return; }
    if (event.key.length === 1 && /\S/.test(event.key) && !event.ctrlKey && !event.metaKey && !event.altKey) {
      const letter = event.key.toLocaleLowerCase(), order = [...items.slice(index + 1), ...items.slice(0, index + 1)];
      order.find(item => item.textContent.trim().toLocaleLowerCase().startsWith(letter))?.focus();
    }
  };
  return html`<div class="context-menu popover-panel" role="menu" aria-label=${state.label} ref=${box} style="left:0;top:0;visibility:hidden" onKeyDown=${keys} onContextMenu=${event => event.preventDefault()}>
    ${menuRows(state.items).map((row, index) => row.separator ? html`<div key=${`sep${index}`} class="context-menu-separator" role="separator"></div>`
      : row.group !== undefined ? html`<div key=${`group${index}`} class="context-menu-group" role="group" aria-label=${row.group}><div class="context-menu-caption" aria-hidden="true">${row.group}</div>${row.items.map(item => html`<${Item} key=${item.id} item=${item} />`)}</div>`
        : html`<${Item} key=${row.id} item=${row} />`)}
  </div>`;
}

const outside = event => { if (current && !host.contains(event.target)) closeContextMenu(false); };
// A scroll that was already under way when the menu opened (the row brought into view, momentum) lands just after it: only a later one closes it.
const scrolled = event => { if (current && performance.now() - current.at > 150 && !(event.target instanceof Node && host.contains(event.target))) closeContextMenu(false); };
const gone = () => closeContextMenu(false);

// Opens a menu: { point: { x, y } in viewport pixels, label (its accessible name), items (see menuRows), returnFocus? (where the focus goes when it
// closes; by default the element that had it), onClose? }. A menu already open is closed first. Answers a number that names this opening.
export function openContextMenu(options) {
  closeContextMenu(false);
  if (!host) { host = document.createElement('div'); host.className = 'context-menu-host'; }
  if (!host.isConnected) document.body.append(host);
  const active = document.activeElement;
  current = { ...options, returnFocus: options.returnFocus || (active && active !== document.body ? active : null), opening: ++opened, at: performance.now() };
  render(html`<${Menu} state=${current} />`, host);
  document.addEventListener('pointerdown', outside, true);
  document.addEventListener('scroll', scrolled, true);
  addEventListener('resize', gone); addEventListener('blur', gone);
  return current.opening;
}

// Closes the open menu, if any; `refocus` gives the focus back to where it was when the menu opened (Escape, Tab, an item chosen).
export function closeContextMenu(refocus = false) {
  if (!current) return;
  const { returnFocus, onClose } = current;
  current = null;
  document.removeEventListener('pointerdown', outside, true);
  document.removeEventListener('scroll', scrolled, true);
  removeEventListener('resize', gone); removeEventListener('blur', gone);
  render(null, host);
  if (refocus && returnFocus?.isConnected) returnFocus.focus({ preventScroll: true });
  onClose?.();
}

export const contextMenuOpen = () => !!current;

// What a right-click or a long press leaves to the browser's own menu: a link, a field, an image, or text the person has selected inside the target.
function native(event, target) {
  if (event.target.closest?.('a[href], input, textarea, select, img, [contenteditable="true"]')) return true;
  const selection = getSelection?.();
  return !!selection && !selection.isCollapsed && String(selection).trim() !== '' && target.contains(selection.anchorNode);
}

const LONG_PRESS_MS = 500, SLOP_PX = 10;

// Opens the menu of the element matching `selector` inside `container` on a right-click, a long press on a touch screen, or the context-menu key
// or Shift+F10 while the focus is inside it. `menu(target, { keyboard })` answers the menu's options (without `point`) or null for none. The click
// that ends a long press is swallowed, so it does not also open the room or select the message. Answers a function that removes the listeners.
export function bindContextMenu(container, { selector, menu }) {
  let press = null, swallowUntil = 0, longPressAt = 0;
  const show = (target, point, keyboard) => {
    const options = menu(target, { keyboard });
    if (!options) return false;
    openContextMenu({ ...options, point });
    return true;
  };
  const find = node => { const target = node?.closest?.(selector); return target && container.contains(target) ? target : null; };
  const clear = () => { if (press) clearTimeout(press.timer); press = null; };
  const listeners = {
    contextmenu: event => {
      const target = find(event.target);
      if (!target || native(event, target)) return;
      // A long press already opened it (a touch browser sends its own contextmenu event for the same press).
      if (Date.now() - longPressAt < 800) { event.preventDefault(); return; }
      if (show(target, { x: event.clientX, y: event.clientY }, false)) event.preventDefault();
    },
    keydown: event => {
      if (!opensMenu(event)) return;
      const target = find(event.target);
      if (!target) return;
      const box = (event.target.getBoundingClientRect ? event.target : target).getBoundingClientRect();
      if (show(target, { x: box.left + Math.min(16, box.width / 2), y: box.bottom }, true)) { event.preventDefault(); event.stopPropagation(); }
    },
    pointerdown: event => {
      clear();
      if (event.pointerType !== 'touch' || !event.isPrimary) return;
      const target = find(event.target);
      if (!target || native(event, target)) return;
      press = { target, x: event.clientX, y: event.clientY };
      press.timer = setTimeout(() => {
        const held = press; press = null;
        if (!held?.target.isConnected) return;
        if (show(held.target, { x: held.x, y: held.y }, false)) { longPressAt = Date.now(); swallowUntil = Date.now() + 1000; getSelection?.()?.removeAllRanges(); }
      }, LONG_PRESS_MS);
    },
    pointermove: event => { if (press && Math.hypot(event.clientX - press.x, event.clientY - press.y) > SLOP_PX) clear(); },
    pointerup: clear,
    pointercancel: clear,
  };
  const swallow = event => { if (Date.now() < swallowUntil) { swallowUntil = 0; event.preventDefault(); event.stopPropagation(); } };
  for (const [type, listener] of Object.entries(listeners)) container.addEventListener(type, listener);
  container.addEventListener('click', swallow, true);
  return () => {
    clear();
    for (const [type, listener] of Object.entries(listeners)) container.removeEventListener(type, listener);
    container.removeEventListener('click', swallow, true);
  };
}
