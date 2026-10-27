import { setGlyph } from './icon-button.js';
// On phones the room header cannot hold six controls next to the title and the Agents button, so
// below the breakpoint the controls collapse into one overflow menu. The menu holds no logic of its
// own: each item clicks the real header button, so hidden and disabled state stay in one place, and
// a toggle's own label (for example "Show raw Markdown" / "Show rendered Markdown") says what it does next.
const PHONE = '(max-width: 700px), (max-width: 900px) and (pointer: coarse)';

function node(tag, className = '', text = '') {
  const item = document.createElement(tag);
  if (className) item.className = className;
  if (text) item.textContent = text;
  return item;
}

export class RoomMenu {
  constructor() {
    this.controls = document.querySelector('.room-controls');
    this.phone = matchMedia(PHONE);
    this.toggle = node('button', 'icon-btn room-menu-toggle');
    this.toggle.id = 'room-menu-toggle'; this.toggle.type = 'button'; setGlyph(this.toggle, 'more', 'Room actions');
    this.toggle.setAttribute('aria-haspopup', 'menu'); this.toggle.setAttribute('aria-expanded', 'false');
    this.menu = node('div', 'room-menu'); this.menu.id = 'room-menu'; this.menu.setAttribute('role', 'menu');
    this.menu.setAttribute('aria-label', 'Room actions'); this.menu.hidden = true;
    this.controls.after(this.toggle);
    document.body.append(this.menu);
    this.toggle.onclick = event => { event.stopPropagation(); if (this.menu.hidden) this.open(); else this.close(); };
    document.addEventListener('click', event => { if (!this.menu.hidden && !this.menu.contains(event.target) && !this.toggle.contains(event.target)) this.close(); });
    this.menu.addEventListener('keydown', event => this.onKey(event));
    this.toggle.addEventListener('keydown', event => {
      if (event.key === 'ArrowDown' && this.menu.hidden) { event.preventDefault(); this.open(); }
    });
    document.addEventListener('keydown', event => { if (event.key === 'Escape' && !this.menu.hidden) { this.close(); this.toggle.focus(); } });
    // Leaving the phone layout while the menu is open would strand it over the desktop header.
    this.phone.addEventListener('change', () => { this.close(); this.sync(); });
    // The overflow button exists only where controls overflow into it (phones) and only while it has something to open.
    new MutationObserver(() => this.sync()).observe(this.controls, { attributes: true, subtree: true, attributeFilter: ['hidden', 'disabled'] });
    this.settings = document.querySelector('#room-info'); // beside the member count in the title; on a phone its place is this menu
    if (this.settings) new MutationObserver(() => this.sync()).observe(this.settings, { attributes: true, attributeFilter: ['hidden', 'disabled'] });
    this.sync();
    window.addEventListener('resize', () => { if (!this.menu.hidden) this.place(); });
  }

  // Hidden unless the layout moves the controls into the menu and at least one of them can be used.
  sync() {
    const usable = this.phone.matches && this.entries().some(entry => !entry.button.disabled);
    this.toggle.hidden = !usable;
    if (!usable && !this.menu.hidden) this.close();
  }

  // One entry per header button that is currently shown, labelled with its accessible name.
  entries() {
    return [...(this.settings ? [this.settings] : []), ...this.controls.querySelectorAll('button')].filter(button => !button.hidden).map(button => ({
      button,
      label: button.getAttribute('aria-label') || button.title || button.textContent.trim(),
    }));
  }

  open() {
    const entries = this.entries();
    if (!entries.length) return;
    this.menu.replaceChildren(...entries.map(entry => {
      const item = node('button', 'row-btn room-menu-item', entry.label);
      item.type = 'button';
      item.setAttribute('role', 'menuitem');
      item.disabled = entry.button.disabled;
      item.onclick = () => { this.close(); entry.button.click(); };
      return item;
    }));
    this.menu.hidden = false;
    this.toggle.setAttribute('aria-expanded', 'true');
    this.place();
    this.menu.querySelector('.room-menu-item:not(:disabled)')?.focus();
  }

  close() {
    this.menu.hidden = true;
    this.toggle.setAttribute('aria-expanded', 'false');
  }

  place() {
    const box = this.toggle.getBoundingClientRect();
    this.menu.style.top = `${Math.round(box.bottom + 4)}px`;
    this.menu.style.right = `${Math.max(8, Math.round(innerWidth - box.right))}px`;
  }

  onKey(event) {
    const items = [...this.menu.querySelectorAll('.room-menu-item:not(:disabled)')];
    const index = items.indexOf(document.activeElement);
    const move = { ArrowDown: index + 1, ArrowUp: index - 1, Home: 0, End: items.length - 1 }[event.key];
    if (move === undefined || !items.length) return;
    event.preventDefault();
    items[(move + items.length) % items.length].focus();
  }
}
