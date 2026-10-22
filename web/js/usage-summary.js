// "Usage" in Room settings and in Project settings: what the turns of a room, or of every room of a project, spent, over today, the last seven
// days or all time, in total and by agent and by model. The server sums each turn once; the wording is in message-usage-format.js.
// Everything is written with textContent.
import { request } from './api.js';
import { totalsRows, totalsNotes, groupLine } from './message-usage-format.js';
import { settingsRow, settingsGroup } from './settings-row.js';

function node(tag, className = '', text = '') { const item = document.createElement(tag); if (className) item.className = className; if (text) item.textContent = text; return item; }

export class UsageSummary {
  // `path(id)` is the usage route of the thing shown (a room or a project).
  constructor(root, path) {
    this.root = root; this.path = path; this.id = ''; this.generation = 0;
    this.title = node('h3', '', 'Usage');
    this.range = node('select');
    for (const [value, text] of [['today', 'Today'], ['7d', 'Last 7 days'], ['all', 'All time']]) { const option = node('option', '', text); option.value = value; this.range.append(option); }
    const picker = settingsGroup([settingsRow({ label: 'Range', desc: 'Today is the server\'s local day.', control: this.range })]);
    this.status = node('p', 'usage-summary-status'); this.status.setAttribute('role', 'status');
    this.body = node('div', 'usage-summary-body');
    this.root.replaceChildren(this.title, picker, this.status, this.body);
    this.range.onchange = () => this.refresh();
  }
  // Shows the usage of one room or project; called each time its settings open.
  load(id) { this.id = id || ''; this.range.value = 'today'; this.refresh(); }
  async refresh() {
    const id = this.id, generation = ++this.generation;
    this.body.replaceChildren();
    if (!id) { this.status.textContent = ''; return; }
    this.status.textContent = 'Loading\u2026';
    try {
      const totals = await request(`${this.path(id)}?range=${encodeURIComponent(this.range.value)}`);
      if (generation !== this.generation) return;
      this.status.textContent = '';
      this.render(totals);
    } catch (error) { if (generation === this.generation) this.status.textContent = error.message; }
  }
  render(totals) {
    const grid = node('dl', 'usage-popover-rows');
    for (const [label, value] of totalsRows(totals)) grid.append(node('dt', '', label), node('dd', '', value));
    this.body.append(grid);
    for (const note of totalsNotes(totals)) this.body.append(node('p', 'usage-popover-note', note));
    for (const [title, groups] of [['By agent', totals.by_agent], ['By model', totals.by_model]]) {
      if (!groups?.length) continue;
      const list = node('ul', 'usage-summary-groups'); list.setAttribute('aria-label', title);
      for (const group of groups) { const item = node('li'); item.append(node('span', 'usage-summary-group-name', group.label || group.key), document.createTextNode(' '), node('span', 'usage-summary-group-figures', groupLine(group))); list.append(item); }
      this.body.append(node('h4', '', title), list);
    }
  }
}
