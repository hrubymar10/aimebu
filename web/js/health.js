import { request, currentUser, rights } from './api.js';
import { focusPopover, returnFocus } from './popover-focus.js';
import { HttpLimitGuardSource } from './limit-guard-source.js';
import { GUARD_TOOLS, guardHealthRow, heldTools } from './limit-guard-format.js';

// Plain words for the short pill on an agent card (the internal state names stay in the API and the inspector).
const labels = {
  stuck: 'Not responding',
  out_of_quota: 'Out of quota',
  machine_offline: 'Machine offline',
  held_answer: 'Reply not delivered',
  recovery_waiting: 'Needs your decision',
};

// One line saying what the problem means and what to do about it, shown under each entry of the health list.
export function healthExplanation(flag) {
  switch (flag.code) {
    case 'stuck': return 'A turn has run with no sign of activity for a while. Open the agent to stop it or wait.';
    case 'out_of_quota': return 'The account hit its usage limit, so the agent cannot work until it resets or you switch accounts.';
    case 'machine_offline': return 'The computer this agent runs on is not connected to the server. Check that its supervisor is running.';
    case 'held_answer': return 'The agent wrote a reply that could not be posted. Open the Inbox to dismiss it or see the room.';
    case 'recovery_waiting': return 'A turn ended unexpectedly. Choose in the Inbox whether to resume, retry or abandon it.';
    default: return 'Open the agent to see more.';
  }
}

export function healthLabel(flag) {
  const reset = new Date(flag.reset_at);
  if (flag.code === 'out_of_quota' && flag.reset_at && !Number.isNaN(reset.getTime())) return `Out of quota until ${reset.toLocaleString()}`;
  return labels[flag.code] || flag.code;
}

// One plain sentence per problem, for the health list. (The agent card keeps the short pill labels above.)
export function healthSentence(name, flag) {
  switch (flag.code) {
    case 'stuck': return `${name} looks stuck`;
    case 'out_of_quota': {
      const reset = new Date(flag.reset_at);
      return flag.reset_at && !Number.isNaN(reset.getTime()) ? `${name} is out of quota until ${reset.toLocaleString()}` : `${name} is out of quota`;
    }
    case 'machine_offline': return `${name}'s machine is offline`;
    case 'held_answer': return `An answer to ${name} could not be delivered`;
    case 'recovery_waiting': return `${name} needs a recovery decision`;
    default: return `${name}: ${healthLabel(flag)}`;
  }
}

export class Health {
  // onItem(row, flag) opens the matching Inbox item or callout for flags that have one and answers true when it did;
  // every other flag, and a flag whose item is gone, opens the agent's room.
  constructor(sidebar, agents, onRoom, onItem, disk = null) {
    this.disk = disk; this.sidebar = sidebar; this.agents = agents; this.onRoom = onRoom; this.onItem = onItem;
    this.toggle = document.querySelector('#health-toggle');
    this.count = document.querySelector('#health-count');
    this.popover = document.querySelector('#health-popover');
    this.toggle.setAttribute('aria-haspopup', 'dialog'); this.toggle.setAttribute('aria-expanded', 'false');
    this.popover.setAttribute('role', 'dialog'); this.popover.setAttribute('aria-label', 'Health');
    this.setOpen = open => { if (!open) returnFocus(this.popover, this.toggle); this.popover.hidden = !open; this.toggle.setAttribute('aria-expanded', String(open)); if (open) { this.disk?.refresh(); focusPopover(this.popover); } };
    this.toggle.onclick = () => { this.setOpen(this.popover.hidden); if (!this.popover.hidden) this.refreshLimits(); };
    document.addEventListener('keydown', event => { if (event.key === 'Escape' && !this.popover.hidden) { this.setOpen(false); this.toggle.focus(); } });
    this.ticket = 0;
    this.limitGuard = new HttpLimitGuardSource();
    this.agentCount = 0;
    // The limit guard is read on its own slow rhythm, not on every rebuild of the agent list: usage moves without any agent
    // event, so the rows are re-read on a timer and when the list opens, and a rebuild draws the copy kept here. It is the
    // owner's: once the server says this person is not the owner, it is not asked again for that sign-in.
    this.guard = null; this.guardUser = null; this.guardGone = false;
    this.limitTimer = setInterval(() => this.refreshLimits(), 30000);
  }
  schedule() {
    if (this.pending) return;
    this.pending = setTimeout(() => { this.pending = null; this.refresh(); }, 500);
  }
  async refresh() {
    const ticket = ++this.ticket;
    let rows;
    try {
      if (new URLSearchParams(location.search).has('mock_agents')) {
        rows = [...this.agents.cards.values()].map(card => ({ id: card.id, name: card.name, room_ids: [card.room_id], flags: card.health_flags || [] }));
      } else rows = (await request('/api/health')).agents || [];
    } catch { return; }
    if (ticket !== this.ticket || document.querySelector('#app-view').hidden) return;
    const affected = rows.filter(row => row.flags?.length);
    // The first rebuild of a sign-in reads the limit guard once; later ones draw what the timer last read.
    if (this.guardUser !== currentUser) { this.guard = null; this.guardGone = false; this.guardUser = currentUser; await this.refreshLimits(false); if (ticket !== this.ticket) return; }
    this.agentCount = affected.length; this.setBadge(heldTools(this.guard));
    this.sidebar.setHealth(affected);
    const nodes = affected.flatMap(row => row.flags.map(flag => {
      const button = document.createElement('button'); button.type = 'button'; button.className = 'row-btn health-item'; button.dataset.healthAgent = row.id; button.dataset.code = flag.code;
      const title = document.createElement('span'); title.className = 'health-item-title'; title.textContent = healthSentence(row.name, flag);
      const hint = document.createElement('span'); hint.className = 'health-item-hint'; hint.textContent = healthExplanation(flag);
      button.append(title, hint);
      button.onclick = async () => {
        this.setOpen(false);
        if (await this.onItem?.(row, flag)) return;
        const room = row.room_ids.map(id => this.sidebar.rooms.get(id)).find(Boolean)
          || (row.room_ids.includes(this.agents.room?.id) ? this.agents.room : null);
        this.onRoom(room, row.id);
      };
      return button;
    }));
    if (!nodes.length) { const empty = document.createElement('p'); empty.textContent = 'All agents healthy'; nodes.push(empty); }
    const limits = this.limitRows(this.guard);
    this.popover.replaceChildren(...(this.disk ? [this.disk.node] : []), ...(limits ? [limits] : []), ...nodes);
  }
  setBadge(held) {
    const total = this.agentCount + held;
    this.count.textContent = String(total);
    this.count.hidden = !total;
    this.toggle.classList.toggle('has-issues', !!total);
    this.toggle.title = total ? `${total} need attention` : 'All agents healthy';
    this.toggle.setAttribute('aria-label', total ? `Health, ${total} need attention` : 'Health');
  }
  // Re-reads only the limit guard: keeps the answer, replaces its block and the badge, never the agent entries a click may be on.
  async refreshLimits(draw = true) {
    if (this.guardGone || document.querySelector('#app-view').hidden) return;
    if (!rights.owner) { this.guard = null; this.guardGone = true; } // the limit guard is the owner's: no request that can only answer 403
    else {
      try { this.guard = await this.limitGuard.load(); }
      catch (error) {
        this.guard = null;
        if (error.status === 403 || error.status === 404) this.guardGone = true; // not the owner, or not served: do not ask again
      }
    }
    if (!draw) return;
    this.setBadge(heldTools(this.guard));
    const next = this.limitRows(this.guard), current = this.popover.querySelector('.health-limits');
    if (current && next) current.replaceWith(next); else if (current) current.remove(); else if (next) { if (this.disk?.node.parentNode === this.popover) this.disk.node.after(next); else this.popover.prepend(next); }
  }
  // One row per tool whose active profile has a clear reading: the sentence and a bar with the thresholds ticked.
  limitRows(guard) {
    const rows = GUARD_TOOLS.map(([tool, label]) => guard?.tools?.[tool] && guardHealthRow(tool, label, guard.tools[tool])).filter(Boolean);
    if (!rows.length) return null;
    const list = document.createElement('div'); list.className = 'health-limits'; list.setAttribute('role', 'group'); list.setAttribute('aria-label', 'Usage limits');
    for (const row of rows) {
      const item = document.createElement('div'); item.className = 'health-limit'; item.dataset.tool = row.tool; item.dataset.state = row.state;
      const text = document.createElement('p'); text.className = 'health-limit-text'; text.textContent = row.text;
      const bar = document.createElement('div'); bar.className = 'health-limit-bar'; bar.setAttribute('aria-hidden', 'true');
      const fill = document.createElement('div'); fill.className = 'health-limit-fill'; fill.style.width = `${Math.max(0, Math.min(100, row.percent))}%`;
      bar.append(fill);
      for (const at of [row.soft, row.hard]) { const tick = document.createElement('div'); tick.className = 'health-limit-tick'; tick.style.left = `${Math.max(0, Math.min(100, at))}%`; bar.append(tick); }
      item.append(text, bar); list.append(item);
    }
    return list;
  }
}
