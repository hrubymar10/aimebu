import { html } from '../../ui.js';

export const statusSentence = ({ needs, waiting, working }) => `${needs} need you, ${waiting} waiting, ${working} working`;
const totalOf = counts => counts.needs + counts.waiting + counts.working;

function Mark({ kind, size = 11 }) {
  const shape = kind === 'needs' ? html`<path d="M6 1 11 11H1Z" />`
    : kind === 'waiting' ? html`<rect x="2" y="2" width="8" height="8" rx="1" />`
      : html`<circle cx="6" cy="6" r="4" />`;
  return html`<svg class=${`project-status-mark project-status-${kind}`} width=${size} height=${size} viewBox="0 0 12 12" fill="currentColor" aria-hidden="true">${shape}</svg>`;
}

function VariantA({ counts }) {
  const kind = counts.needs ? 'needs' : counts.waiting ? 'waiting' : 'working';
  const label = kind === 'needs' ? counts.needs === 1 ? 'Needs you' : `${counts.needs} need you`
    : kind === 'waiting' ? `${counts.waiting} waiting` : `${counts.working} working`;
  return html`<span class=${`project-status-a project-status-${kind}`}><${Mark} kind=${kind} />${label}</span>`;
}
function VariantB({ counts }) {
  return html`<span class="project-status-b">${['needs', 'waiting', 'working'].filter(kind => counts[kind]).map(kind => html`<span class="project-status-b-part" key=${kind}><${Mark} kind=${kind} />${counts[kind]}</span>`)}<span class="project-status-b-total">${totalOf(counts)}</span></span>`;
}
function VariantC({ counts }) {
  const kinds = ['needs', 'waiting', 'working'].flatMap(kind => Array(Math.min(counts[kind], 8)).fill(kind)).slice(0, 8);
  return html`<span class="project-status-c">${kinds.map((kind, index) => html`<${Mark} key=${index} kind=${kind} size=${8} />`)}${totalOf(counts) > 8 && html`<span class="project-status-overflow">+${totalOf(counts) - 8}</span>`}</span>`;
}
function VariantD({ counts }) {
  const total = totalOf(counts), size = Math.max(21, String(total).length * 6 + 10);
  return html`<span class="project-status-d" style=${`width:${size}px;height:${size}px`}><svg width=${size} height=${size} viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><circle cx="12" cy="12" r="10" /></svg><span>${total}</span></span>`;
}
const variants = { a: VariantA, b: VariantB, c: VariantC, d: VariantD };

export function ProjectStatus({ counts, variant, id, projectName, folded, onExpand }) {
  if (!counts || !totalOf(counts)) return null;
  const sentence = statusSentence(counts), Variant = variants[variant] || VariantA;
  return html`<span class="project-status-wrap"><button type="button" class=${`row-btn project-status project-status-variant-${variant}`} title=${sentence} aria-label=${folded ? `Expand ${projectName}` : `${projectName} status`} aria-describedby=${id} onClick=${onExpand}><${Variant} counts=${counts} /></button><span class="sr-only" id=${id}>${sentence}</span></span>`;
}
