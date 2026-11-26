import { createPortal, html, useLayoutEffect, useRef, useState } from '../../ui.js';

// The reason a room stands behind its count, in words: a room that needs you is
// either waiting on an answer (an open attention item) or holds an agent's
// unseen finish; a held room is paused; a working room is working.
const REASONS = {
  needs: room => (room.attention_open || 0) > 0 ? 'needs an answer' : 'finished',
  waiting: () => 'paused',
  working: () => 'working',
};
const KIND_LABEL = { needs: 'Needs you', waiting: 'Waiting', working: 'Working' };
const CAP = 5;
const kindOf = counts => counts.needs ? 'needs' : counts.waiting ? 'waiting' : 'working';

// The rooms the pill stands for: the highest-priority count's rooms, first the one a click opens.
export const statusFocus = counts => counts.rooms[kindOf(counts)][0] || null;

// The rooms line of the tooltip and accessible name: the rooms behind the shown status, each with its reason in words, at most five named then "and N more".
export const statusRoomsLine = counts => {
  const kind = kindOf(counts), list = counts.rooms[kind];
  const named = list.slice(0, CAP).map(room => `${room.title} (${REASONS[kind](room)})`);
  const more = list.length - named.length;
  return `${KIND_LABEL[kind]}: ${named.join(', ')}${more > 0 ? ` and ${more} more` : ''}`;
};

// The overview under the rooms line: every count, in the pill's own words.
export const statusSummary = ({ needs, waiting, workingAgents }) => `${needs} need you, ${waiting} waiting, ${workingAgents} working`;

function Mark({ kind, size = 11 }) {
  const shape = kind === 'needs' ? html`<path d="M6 1 11 11H1Z" />`
    : kind === 'waiting' ? html`<rect x="2" y="2" width="8" height="8" rx="1" />`
      : html`<circle cx="6" cy="6" r="4" />`;
  return html`<svg class=${`project-status-mark project-status-${kind}`} width=${size} height=${size} viewBox="0 0 12 12" fill="currentColor" aria-hidden="true">${shape}</svg>`;
}

function VariantA({ counts }) {
  const kind = kindOf(counts);
  const label = kind === 'needs' ? counts.needs === 1 ? 'Needs you' : `${counts.needs} need you`
    : kind === 'waiting' ? `${counts.waiting} waiting` : `${counts.workingAgents} working`;
  return html`<span class=${`project-status-a project-status-${kind}`}><${Mark} kind=${kind} />${label}</span>`;
}

export function ProjectStatus({ counts, id, projectName, folded, onOpen }) {
  if (!counts || !(counts.needs + counts.waiting + counts.working)) return null;
  const rooms = statusRoomsLine(counts), summary = statusSummary(counts);
  const working = kindOf(counts) === 'working';
  const label = working ? `Open ${projectName} room — ${counts.workingAgents} ${counts.workingAgents === 1 ? 'agent' : 'agents'} working`
    : folded ? `Expand ${projectName}: ${rooms}.` : `${projectName}: ${rooms}. Click to open.`;
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
  }, [shown, rooms, label]);
  return html`<span class="project-status-wrap"><button ref=${pillRef} type="button" class="row-btn project-status" aria-label=${label} aria-describedby=${id} onClick=${() => { setShown(false); onOpen?.(statusFocus(counts)); }} onMouseEnter=${() => setShown(true)} onMouseLeave=${() => setShown(false)} onFocus=${() => setShown(true)} onBlur=${() => setShown(false)}><${VariantA} counts=${counts} /></button><span class="sr-only" id=${id}>${summary}</span>${shown && createPortal(html`<span ref=${tooltipRef} class="project-status-tooltip" role="tooltip"><span class="project-status-tooltip-rooms">${working ? label : rooms}</span><span class="project-status-tooltip-summary">${summary}</span></span>`, document.body)}</span>`;
}
