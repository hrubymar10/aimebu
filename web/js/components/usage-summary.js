import { html, batch, signal } from '../ui.js';
import { request } from '../api.js';
import { totalsRows, totalsNotes, groupLine, inputTooltip } from '../message-usage-format.js';
import { SettingsGroup, SettingsRow } from './settings-row.js';

const RANGES = [['today', 'Today'], ['7d', 'Last 7 days'], ['all', 'All time']];

export class UsageSummaryStore {
  constructor(path) {
    this.path = path;
    this.id = '';
    this.range = signal('today');
    this.status = signal('');
    this.totals = signal(null);
    this.alive = true;
    this.generation = 0;
  }

  load(id) { this.id = id || ''; this.range.value = 'today'; return this.refresh(); }

  setRange(range) { this.range.value = range; return this.refresh(); }

  async refresh() {
    if (!this.alive) return;
    const id = this.id, generation = ++this.generation;
    batch(() => { this.totals.value = null; this.status.value = id ? 'Loading…' : ''; });
    if (!id) return;
    try {
      const totals = await request(`${this.path(id)}?range=${encodeURIComponent(this.range.peek())}`);
      if (!this.alive || generation !== this.generation) return;
      batch(() => { this.status.value = ''; this.totals.value = totals; });
    } catch (failure) { if (this.alive && generation === this.generation) this.status.value = failure.message; }
  }

  destroy() { this.alive = false; this.generation++; }
}

function Groups({ title, groups, roles = false }) {
  if (!groups?.length) return null;
  return html`<h4>${title}</h4><ul class="usage-summary-groups" aria-label=${title}>${groups.map(group => html`<li><span class="usage-summary-group-name">${group.label || group.key}${roles ? ' · ' : ''}</span>${roles ? '' : ' '}<span class="usage-summary-group-figures" title=${inputTooltip(group)}>${groupLine(group)}</span></li>`)}</ul>`;
}

export function UsageSummary({ store, title = 'Usage' }) {
  const totals = store.totals.value;
  return html`<h3>${title}</h3>
    <${SettingsGroup}><${SettingsRow} label="Range" desc="Today is the server's local day.">
      <select value=${store.range.value} onChange=${event => store.setRange(event.currentTarget.value)}>${RANGES.map(([value, text]) => html`<option value=${value}>${text}</option>`)}</select><//><//>
    <p class="usage-summary-status" role="status">${store.status.value}</p>
    <div class="usage-summary-body">${totals && html`<dl class="usage-popover-rows">${totalsRows(totals).map(([label, value]) => html`<dt>${label}</dt><dd title=${label === 'Input' ? inputTooltip(totals) : ''}>${value}</dd>`)}</dl>
      ${totalsNotes(totals).map(note => html`<p class="usage-popover-note">${note}</p>`)}
      <${Groups} title="By role" groups=${totals.by_role} roles=${true} /><${Groups} title="By model" groups=${totals.by_model} />`}</div>`;
}
