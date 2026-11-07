import { html } from '../../ui.js';
import { GUARD_TOOLS, guardHealthRow } from '../../limit-guard-format.js';
import { DiskSection } from './disk.js';
import { healthExplanation, healthSentence } from './model.js';

export function LimitRows({ guard }) {
  const rows = GUARD_TOOLS.map(([tool, label]) => guard?.tools?.[tool] && guardHealthRow(tool, label, guard.tools[tool], guard.percent_display)).filter(Boolean);
  if (!rows.length) return null;
  // The bar shows what the sentence says: the share used, or the share left when the owner chose percent left (the soft and hard marks then flip too).
  const clamp = value => Math.max(0, Math.min(100, value)), left = row => row.display === 'left', place = (row, value) => clamp(left(row) ? 100 - value : value);
  return html`<div class="health-limits" role="group" aria-label="Usage limits">${rows.map(row => html`<div class="health-limit" data-tool=${row.tool} data-state=${row.state}>
    <p class="health-limit-text">${row.text}</p><div class="health-limit-bar" aria-hidden="true" title=${`The two marks are where new turns are held (${row.soft}% used) and where they are held until it resets (${row.hard}% used).`}><div class="health-limit-fill" style=${`width:${place(row, row.percent)}%`}></div>${[row.soft, row.hard].map(at => html`<div class="health-limit-tick" style=${`left:${place(row, at)}%`}></div>`)}</div></div>`)}<p class="health-limit-legend">The two marks on a bar are where the limit guard holds new turns: the first holds them, the second keeps them held until the window resets.</p></div>`;
}

export function HealthPopover({ store, disk, onEntry }) {
  const rows = store.rows.value;
  const entries = (rows || []).flatMap(row => row.flags.map(flag => html`<button type="button" class="row-btn health-item" data-health-agent=${row.id} data-code=${flag.code} onClick=${() => onEntry(row, flag)}>
    <span class="health-item-title">${healthSentence(row.name, flag)}</span><span class="health-item-hint">${healthExplanation(flag)}</span></button>`));
  return html`${disk && html`<${DiskSection} disk=${disk} />`}<${LimitRows} guard=${store.guard.value} />${entries}${!entries.length && html`<p>All agents healthy</p>`}`;
}
