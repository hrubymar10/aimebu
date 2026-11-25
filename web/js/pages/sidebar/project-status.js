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

export function ProjectStatus({ counts, id, projectName, folded, onExpand }) {
  if (!counts || !totalOf(counts)) return null;
  const sentence = statusSentence(counts);
  return html`<span class="project-status-wrap"><button type="button" class="row-btn project-status" title=${sentence} aria-label=${folded ? `Expand ${projectName}` : `${projectName} status`} aria-describedby=${id} onClick=${onExpand}><${VariantA} counts=${counts} /></button><span class="sr-only" id=${id}>${sentence}</span></span>`;
}
