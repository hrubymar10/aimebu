import { createPortal, html, useLayoutEffect, useRef, useState } from '../../ui.js';

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
  const pillRef = useRef(null), tooltipRef = useRef(null), [shown, setShown] = useState(false);
  useLayoutEffect(() => {
    if (!shown) return;
    const place = () => {
      const pill = pillRef.current?.getBoundingClientRect(), tooltip = tooltipRef.current;
      if (!pill || !tooltip) return;
      const tip = tooltip.getBoundingClientRect();
      tooltip.style.left = `${Math.max(6, Math.min(pill.right - tip.width, window.innerWidth - tip.width - 6))}px`;
      tooltip.style.top = `${pill.bottom + tip.height + 6 <= window.innerHeight ? pill.bottom + 4 : Math.max(6, pill.top - tip.height - 4)}px`;
    };
    place();
    window.addEventListener('resize', place);
    window.addEventListener('scroll', place, true);
    return () => { window.removeEventListener('resize', place); window.removeEventListener('scroll', place, true); };
  }, [shown, sentence]);
  return html`<span class="project-status-wrap"><button ref=${pillRef} type="button" class="row-btn project-status" aria-label=${folded ? `Expand ${projectName}` : `${projectName} status`} aria-describedby=${id} onClick=${onExpand} onMouseEnter=${() => setShown(true)} onMouseLeave=${() => setShown(false)} onFocus=${() => setShown(true)} onBlur=${() => setShown(false)}><${VariantA} counts=${counts} /></button><span class="sr-only" id=${id}>${sentence}</span>${shown && createPortal(html`<span ref=${tooltipRef} class="project-status-tooltip" role="tooltip">${sentence}</span>`, document.body)}</span>`;
}
