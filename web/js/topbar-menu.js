// "⋯" overflow menu for the top bar on very narrow screens. Below 480px the Events, Health, Knowledge and
// Sign out buttons are hidden from the bar (CSS) and offered here instead; each menu item forwards a
// click to the real button, so their handlers, badges and popovers stay the single source of truth.
const ITEMS = [['#system-events-toggle', 'System events'], ['#health-toggle', 'Health'], ['#knowledge-toggle', 'Memory'], ['#logout', 'Sign out']];

export class TopbarMenu {
  constructor() {
    this.toggle = document.querySelector('#more-toggle');
    this.menu = document.querySelector('#more-menu');
    this.toggle.setAttribute('aria-haspopup', 'menu'); this.toggle.setAttribute('aria-expanded', 'false');
    this.items = ITEMS.map(([selector, label]) => this.item(selector, label));
    this.menu.replaceChildren(...this.items.map(item => item.button));
    this.toggle.onclick = event => { event.stopPropagation(); this.setOpen(this.menu.hidden); };
    document.addEventListener('click', event => { if (!this.menu.hidden && !this.menu.contains(event.target) && !this.toggle.contains(event.target)) this.setOpen(false); });
    document.addEventListener('keydown', event => this.keydown(event));
    // The bar button shows the health warning even while the Health button itself is tucked away.
    const health = document.querySelector('#health-toggle');
    const sync = () => this.toggle.classList.toggle('has-issues', health.classList.contains('has-issues'));
    new MutationObserver(sync).observe(health, { attributes: true, attributeFilter: ['class'] }); sync();
  }
  item(selector, label) {
    const target = document.querySelector(selector);
    const button = document.createElement('button'); button.type = 'button'; button.className = 'row-btn more-item'; button.setAttribute('role', 'menuitem');
    const text = document.createElement('span'); text.textContent = label; button.append(text);
    const syncHidden = () => { button.hidden = target.hidden; };
    new MutationObserver(syncHidden).observe(target, { attributes: true, attributeFilter: ['hidden'] }); syncHidden();
    const count = target.querySelector?.('.badge');
    if (count) {
      const badge = document.createElement('span'); badge.className = 'badge'; button.append(badge);
      const sync = () => { badge.textContent = count.textContent; badge.hidden = count.hidden; };
      new MutationObserver(sync).observe(count, { attributes: true, childList: true, characterData: true, subtree: true }); sync();
    }
    button.onclick = () => { this.setOpen(false); target.click(); };
    return { button, target };
  }
  setOpen(open) {
    this.menu.hidden = !open;
    this.toggle.setAttribute('aria-expanded', String(open));
    if (open) this.items.find(item => !item.button.hidden)?.button.focus();
  }
  keydown(event) {
    if (this.menu.hidden) return;
    if (event.key === 'Escape') { event.preventDefault(); this.setOpen(false); this.toggle.focus(); return; }
    if (!['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key) || !this.menu.contains(document.activeElement)) return;
    event.preventDefault();
    const buttons = this.items.filter(item => !item.button.hidden).map(item => item.button), at = buttons.indexOf(document.activeElement);
    const next = event.key === 'Home' ? 0 : event.key === 'End' ? buttons.length - 1 : (at + (event.key === 'ArrowDown' ? 1 : buttons.length - 1)) % buttons.length;
    buttons[next].focus();
  }
}
