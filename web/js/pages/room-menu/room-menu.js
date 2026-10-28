import { html, useLayoutEffect, useRef } from '../../ui.js';
import { IconButton } from '../../components/icon-button.js';

export function RoomMenuToggle({ store }) {
  const open = store.open.value;
  return html`<${IconButton} name="more" label="Room actions" className="room-menu-toggle" id="room-menu-toggle" hidden=${!store.usable.value} aria-haspopup="menu" aria-expanded=${open ? 'true' : 'false'}
    ref=${node => { store.toggle = node; }}
    onClick=${event => { event.stopPropagation(); store.press(); }}
    onKeyDown=${event => { if (event.key === 'ArrowDown') { event.preventDefault(); store.show(); } }} />`;
}

export function RoomMenu({ store }) {
  const menu = useRef(null), open = store.open.value;
  useLayoutEffect(() => { if (open) menu.current?.querySelector('.room-menu-item:not(:disabled)')?.focus(); }, [open]);
  const onKeyDown = event => {
    const items = [...menu.current.querySelectorAll('.room-menu-item:not(:disabled)')];
    const index = items.indexOf(document.activeElement);
    const move = { ArrowDown: index + 1, ArrowUp: index - 1, Home: 0, End: items.length - 1 }[event.key];
    if (move === undefined || !items.length) return;
    event.preventDefault();
    items[(move + items.length) % items.length].focus();
  };
  const { top, right } = store.position.value;
  return html`<div class="room-menu" id="room-menu" role="menu" aria-label="Room actions" hidden=${!open} style=${open ? { top: `${top}px`, right: `${right}px` } : {}} ref=${menu} onKeyDown=${onKeyDown}>
    ${open && store.items.value.map(entry => html`<button type="button" class="row-btn room-menu-item" role="menuitem" disabled=${entry.disabled} onClick=${() => store.choose(entry)}>${entry.label}</button>`)}
  </div>`;
}
