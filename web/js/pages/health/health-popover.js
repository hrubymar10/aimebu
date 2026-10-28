import { html } from '../../ui.js';
import { GUARD_TOOLS, guardHealthRow } from '../../limit-guard-format.js';
import { DiskSection } from './disk.js';
import { healthExplanation, healthSentence } from './model.js';

export function LimitRows({ guard }) {
  const rows = GUARD_TOOLS.map(([tool, label]) => guard?.tools?.[tool] && guardHealthRow(tool, label, guard.tools[tool])).filter(Boolean);
  if (!rows.length) return null;
  const clamp = value => Math.max(0, Math.min(100, value));
  return html`<div class="health-limits" role="group" aria-label="Usage limits">${rows.map(row => html`<div class="health-limit" data-tool=${row.tool} data-state=${row.state}>
    <p class="health-limit-text">${row.text}</p><div class="health-limit-bar" aria-hidden="true"><div class="health-limit-fill" style=${`width:${clamp(row.percent)}%`}></div>${[row.soft, row.hard].map(at => html`<div class="health-limit-tick" style=${`left:${clamp(at)}%`}></div>`)}</div></div>`)}</div>`;
}

export function HealthPopover({ store, disk, onEntry }) {
  const rows = store.rows.value;
  const entries = (rows || []).flatMap(row => row.flags.map(flag => html`<button type="button" class="row-btn health-item" data-health-agent=${row.id} data-code=${flag.code} onClick=${() => onEntry(row, flag)}>
    <span class="health-item-title">${healthSentence(row.name, flag)}</span><span class="health-item-hint">${healthExplanation(flag)}</span></button>`));
  return html`${disk && rows && html`<${DiskSection} disk=${disk} />`}<${LimitRows} guard=${store.guard.value} />${entries}${!entries.length && html`<p>All agents healthy</p>`}`;
}
