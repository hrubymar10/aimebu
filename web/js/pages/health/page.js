import { html, effect } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { HttpLimitGuardSource } from '../../limit-guard-source.js';
import { focusPopover, returnFocus } from '../../popover-focus.js';
import { HealthPopover } from './health-popover.js';
import { HealthStore } from './store.js';
import { badgeView } from './model.js';

const LIMIT_POLL_MS = 30000;

// The adapter is the one writer of everything outside the popover: the count, the toggle's title, label, has-issues and aria-expanded,
// and the popover's hidden. onItem(row, flag) answers true when a flag opened its Inbox item; otherwise the agent's room opens.
export class Health {
  constructor(sidebar, agents, onRoom, onItem, disk = null) {
    this.disk = disk; this.sidebar = sidebar; this.agents = agents; this.onRoom = onRoom; this.onItem = onItem;
    this.toggle = document.querySelector('#health-toggle');
    this.count = document.querySelector('#health-count');
    this.popover = document.querySelector('#health-popover');
    this.limitGuard = new HttpLimitGuardSource();
    this.store = new HealthStore({ sidebar, agents, limitGuard: this.limitGuard });
    this.toggle.setAttribute('aria-haspopup', 'dialog'); this.toggle.setAttribute('aria-expanded', 'false');
    this.popover.setAttribute('role', 'dialog'); this.popover.setAttribute('aria-label', 'Health');
    this.page = mountPreactPage(this.popover, () => html`<${HealthPopover} store=${this.store} disk=${this.disk} onEntry=${(row, flag) => this.open(row, flag)} />`);
    this.stopBadge = effect(() => {
      const view = badgeView(this.store.badge());
      this.count.textContent = view.text; this.count.hidden = view.hidden;
      this.toggle.classList.toggle('has-issues', view.issues); this.toggle.title = view.title; this.toggle.setAttribute('aria-label', view.label);
    });
    this.toggle.onclick = () => { this.setOpen(this.popover.hidden); if (!this.popover.hidden) this.refreshLimits(); };
    this.onKey = event => { if (event.key === 'Escape' && !this.popover.hidden) { this.setOpen(false); this.toggle.focus(); } };
    document.addEventListener('keydown', this.onKey);
    this.limitTimer = setInterval(() => this.refreshLimits(), LIMIT_POLL_MS);
    this.alive = true; this.turn = 0;
  }

  setOpen(open) {
    if (!open) returnFocus(this.popover, this.toggle);
    this.popover.hidden = !open; this.toggle.setAttribute('aria-expanded', String(open));
    if (open) { this.disk?.refresh(); focusPopover(this.popover); }
  }

  async open(row, flag) {
    this.setOpen(false);
    const turn = ++this.turn;
    const opened = await this.onItem?.(row, flag);
    if (!this.alive || turn !== this.turn) return;
    if (opened) return;
    const room = row.room_ids.map(id => this.sidebar.rooms.get(id)).find(Boolean)
      || (row.room_ids.includes(this.agents.room?.id) ? this.agents.room : null);
    this.onRoom(room, row.id);
  }

  get ticket() { return this.store.ticket; }
  set ticket(value) { this.store.ticket = value; }
  get guardGone() { return this.store.guardGone; }

  schedule() {
    if (this.pending) return;
    this.pending = setTimeout(() => { this.pending = null; this.refresh(); }, 500);
  }
  refresh() { return this.store.refresh(); }
  refreshLimits() { return this.store.refreshLimits(); }

  destroy() {
    this.alive = false; this.turn++;
    clearInterval(this.limitTimer); clearTimeout(this.pending); document.removeEventListener('keydown', this.onKey); this.stopBadge();
    this.store.destroy(); this.disk?.destroy(); this.page.destroy();
  }
}
