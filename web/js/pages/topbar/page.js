import { html, effect } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { TopbarStore } from './store.js';
import { MoreItems } from './components.js';
import { MOVE_KEYS, planTuck } from './model.js';

// "⋯" overflow menu for the top bar. On a phone the bar keeps every control that fits and the adapter measures it, tucking System events, Health,
// Memory and Sign out into the menu only as far as needed (a class on the real button, so CSS hides it); on a touch tablet the menu always holds
// them. Each menu item forwards a click to the real button, so their handlers, badges and popovers stay the single source of truth. The adapter
// watches those buttons and writes the toggle's aria and health mark and the menu's hidden. A menu with no bar around it has nothing to measure:
// all its items are on offer.
const owners = new WeakMap();
const ITEMS = [['#system-events-toggle', 'System events'], ['#health-toggle', 'Health'], ['#knowledge-toggle', 'Memory'], ['#logout', 'Sign out']];
const PHONE = '(max-width: 700px)';
const TABLET = '(min-width: 701px) and (max-width: 900px) and (pointer: coarse)';

export class TopbarMenu {
  constructor({ toggle = document.querySelector('#more-toggle'), menu = document.querySelector('#more-menu'), sources = ITEMS.map(([selector, label]) => ({ target: document.querySelector(selector), label })), health = document.querySelector('#health-toggle'), bar = toggle.closest('.workspace-bar') } = {}) {
    owners.get(menu)?.destroy(); owners.set(menu, this);
    this.toggle = toggle; this.menu = menu; this.targets = sources.map(source => source.target);
    this.bar = bar; this.nav = bar?.querySelector('nav') || null; this.health = health;
    this.tucked = new Set(); this.tablet = false; this.widths = new Map(); this.frame = 0;
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
      watch(target, { attributes: true, attributeFilter: ['hidden'] }, () => { this.syncItem(index); this.schedule(); });
      if (badges[index]) watch(badges[index], { attributes: true, childList: true, characterData: true, subtree: true }, () => this.store.setBadge(index, badges[index].textContent, badges[index].hidden));
    });
    // The bar button shows the health warning even while the Health button itself is tucked away.
    if (health) watch(health, { attributes: true, attributeFilter: ['class'] }, () => this.syncIssues());
    if (this.nav) this.measure();
    this.onToggle = event => { event.stopPropagation(); this.store.setOpen(menu.hidden); };
    this.onClick = event => { if (!menu.hidden && !menu.contains(event.target)) this.store.setOpen(false); };
    this.onKey = event => this.keydown(event);
    toggle.addEventListener('click', this.onToggle);
    document.addEventListener('click', this.onClick);
    document.addEventListener('keydown', this.onKey);
  }

  // Whether an item is on offer in the menu: always without a bar, on a touch tablet, or when the phone bar had to tuck its control.
  offered(index) { return !this.bar || this.tablet || this.tucked.has(index); }

  syncItem(index) { this.store.setHidden(index, this.targets[index].hidden || !this.offered(index)); }

  // The toggle carries the health warning only while the Health button itself is out of the bar.
  syncIssues() {
    const index = this.targets.indexOf(this.health);
    this.store.setIssues(!!this.health?.classList.contains('has-issues') && (!this.bar || this.tablet || this.tucked.has(index)));
  }

  schedule() {
    if (!this.bar || this.frame || this.destroyed) return;
    this.frame = requestAnimationFrame(() => { this.frame = 0; this.layout(); });
  }

  // Watches what changes the bar's room: its own and the pill's width, each control's, and what is added, removed, shown or hidden in the nav.
  // ResizeObserver runs before paint, so the bar is corrected in the same frame.
  measure() {
    // The bar's and the pill's size are measured at once. A control's own size changes as it is tucked and comes back, so that relayout waits
    // for the next frame instead of resizing what the observer is still delivering.
    const resize = new ResizeObserver(() => this.layout());
    resize.observe(this.bar);
    const pill = this.nav.querySelector('#ui-variant-toggle');
    if (pill) resize.observe(pill);
    const controls = new ResizeObserver(() => this.schedule());
    for (const target of this.targets) controls.observe(target);
    const content = new MutationObserver(() => this.schedule());
    content.observe(this.nav, { childList: true, subtree: true, characterData: true, attributes: true, attributeFilter: ['hidden', 'style'] });
    this.observers.push(resize, controls, content);
    this.schedule();
  }

  // The width a control takes in the nav, remembered while it is display:none (a tucked control keeps what it measured).
  width(element) {
    const rect = element.getBoundingClientRect(), css = getComputedStyle(element);
    if (rect.width) this.widths.set(element, rect.width);
    return (rect.width || parseFloat(css.width) || this.widths.get(element) || 40) + (parseFloat(css.marginLeft) || 0) + (parseFloat(css.marginRight) || 0);
  }

  layout() {
    if (!this.bar || this.destroyed || !this.bar.getBoundingClientRect().width) return;
    let tucked = [];
    if (matchMedia(PHONE).matches) {
      const entries = [...this.nav.children]
        .filter(element => element !== this.toggle && !element.hidden && (element.classList.contains('topbar-tucked') || getComputedStyle(element).display !== 'none'))
        .map(element => ({ item: this.targets.indexOf(element), width: this.width(element) }));
      tucked = planTuck({ available: this.nav.clientWidth, gap: parseFloat(getComputedStyle(this.nav).columnGap) || 0, moreWidth: this.width(this.toggle), entries });
    }
    const before = this.tucked;
    this.tucked = new Set(tucked);
    this.tablet = !matchMedia(PHONE).matches && matchMedia(TABLET).matches;
    this.targets.forEach((target, index) => { target.classList.toggle('topbar-tucked', this.tucked.has(index)); this.syncItem(index); });
    this.bar.classList.toggle('topbar-has-overflow', this.tucked.size > 0);
    this.syncIssues();
    if (!this.tucked.size && !this.tablet && !this.menu.hidden) {
      // Every control fits again, so the menu has nothing to offer. Focus that was in it goes to a control that came back to the bar.
      const focused = this.menu.contains(document.activeElement);
      this.store.setOpen(false);
      if (focused) [...before].map(index => this.targets[index]).find(target => !target.hidden)?.focus();
    }
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
    if (this.frame) cancelAnimationFrame(this.frame);
    for (const observer of this.observers) observer.disconnect();
    for (const stop of this.stops) stop();
    for (const target of this.targets) target.classList.remove('topbar-tucked');
    this.bar?.classList.remove('topbar-has-overflow');
    this.toggle.removeEventListener('click', this.onToggle);
    document.removeEventListener('click', this.onClick);
    document.removeEventListener('keydown', this.onKey);
    this.page.destroy();
  }
}
