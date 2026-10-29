import { html, effect } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { TopbarStore } from './store.js';
import { MoreItems } from './components.js';
import { MOVE_KEYS } from './model.js';

// "⋯" overflow menu for the top bar on very narrow screens. Below 480px the Events, Health, Knowledge and Sign out buttons are hidden from
// the bar (CSS) and offered here instead; each menu item forwards a click to the real button, so their handlers, badges and popovers stay the
// single source of truth. The adapter watches those buttons and writes the toggle's aria and health mark and the menu's hidden.
const owners = new WeakMap();
const ITEMS = [['#system-events-toggle', 'System events'], ['#health-toggle', 'Health'], ['#knowledge-toggle', 'Memory'], ['#logout', 'Sign out']];

export class TopbarMenu {
  constructor({ toggle = document.querySelector('#more-toggle'), menu = document.querySelector('#more-menu'), sources = ITEMS.map(([selector, label]) => ({ target: document.querySelector(selector), label })), health = document.querySelector('#health-toggle') } = {}) {
    owners.get(menu)?.destroy(); owners.set(menu, this);
    this.toggle = toggle; this.menu = menu; this.targets = sources.map(source => source.target);
    const badges = this.targets.map(target => target.querySelector?.('.badge') || null);
    this.store = new TopbarStore({ items: sources.map((source, index) => ({ label: source.label, badge: badges[index] })), click: index => this.targets[index].click() });
    toggle.setAttribute('aria-haspopup', 'menu');
    menu.replaceChildren();
    this.page = mountPreactPage(menu, () => html`<${MoreItems} store=${this.store} root=${menu} />`, { onError: error => console.error('top bar menu:', error) });
    this.store.flush = () => this.page.update({});
    this.stops = [
      effect(() => { const open = this.store.open.value; menu.hidden = !open; toggle.setAttribute('aria-expanded', String(open)); }),
      effect(() => { toggle.classList.toggle('has-issues', this.store.issues.value); }),
    ];
    this.observers = [];
    const watch = (node, options, callback) => { const observer = new MutationObserver(callback); observer.observe(node, options); this.observers.push(observer); callback(); };
    this.targets.forEach((target, index) => {
      watch(target, { attributes: true, attributeFilter: ['hidden'] }, () => this.store.setHidden(index, target.hidden));
      if (badges[index]) watch(badges[index], { attributes: true, childList: true, characterData: true, subtree: true }, () => this.store.setBadge(index, badges[index].textContent, badges[index].hidden));
    });
    // The bar button shows the health warning even while the Health button itself is tucked away.
    if (health) watch(health, { attributes: true, attributeFilter: ['class'] }, () => this.store.setIssues(health.classList.contains('has-issues')));
    this.onToggle = event => { event.stopPropagation(); this.store.setOpen(menu.hidden); };
    this.onClick = event => { if (!menu.hidden && !menu.contains(event.target)) this.store.setOpen(false); };
    this.onKey = event => this.keydown(event);
    toggle.addEventListener('click', this.onToggle);
    document.addEventListener('click', this.onClick);
    document.addEventListener('keydown', this.onKey);
  }

  keydown(event) {
    if (this.menu.hidden) return;
    if (event.key === 'Escape') { event.preventDefault(); this.store.setOpen(false); this.toggle.focus(); return; }
    if (!MOVE_KEYS.includes(event.key) || !this.menu.contains(document.activeElement)) return;
    event.preventDefault();
    this.store.move(event.key, [...this.menu.querySelectorAll('[role=menuitem]')].indexOf(document.activeElement));
  }

  destroy() {
    if (this.destroyed) return;
    this.destroyed = true;
    for (const observer of this.observers) observer.disconnect();
    for (const stop of this.stops) stop();
    this.toggle.removeEventListener('click', this.onToggle);
    document.removeEventListener('click', this.onClick);
    document.removeEventListener('keydown', this.onKey);
    this.page.destroy();
  }
}
