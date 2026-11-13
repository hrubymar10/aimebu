import { html, useEffect, useRef, useState } from '../../ui.js';
import { queueMermaid } from '../../visual-plan.js';

function diagramSource(diagram) {
  const rooms = diagram?.rooms || [], edges = diagram?.edges || [];
  if (!rooms.length) return '';
  const names = new Map(rooms.map((room, index) => [room.key, `room${index}`]));
  const label = value => String(value || '').replace(/[\r\n]+/g, ' ').replace(/"/g, "'").replace(/[<>]/g, '');
  return ['flowchart LR', ...rooms.map((room, index) => `  room${index}["${label(room.label)}<br/>${label((room.roles || []).join(' · '))}"]`),
    ...edges.filter(edge => names.has(edge.from) && names.has(edge.to)).map(edge => `  ${names.get(edge.from)} --> ${names.get(edge.to)}`)].join('\n');
}

function Diagram({ diagram }) {
  const source = diagramSource(diagram), node = useRef(null);
  useEffect(() => { if (source && node.current) queueMermaid(node.current, source); }, [source]);
  if (!source) return html`<p class="team-flow-diagram team-flow-empty-diagram">Choose your own rooms and roles.</p>`;
  return html`<div class="team-flow-diagram" aria-label="Room and role diagram"><div ref=${node} class="team-flow-mermaid-source">${source}</div></div>`;
}

function DiagramPreview({ diagram }) {
  const rooms = diagram?.rooms || [], edges = diagram?.edges || [];
  if (!rooms.length) return html`<p class="team-flow-diagram team-flow-empty-diagram">Choose your own rooms and roles.</p>`;
  const columns = Math.min(rooms.length, 2), boxWidth = 96, boxHeight = 54, gap = 28, pad = 12;
  const boxes = new Map(rooms.map((room, index) => [room.key, { x: pad + index % columns * (boxWidth + gap), y: pad + Math.floor(index / columns) * (boxHeight + gap) }]));
  const width = pad * 2 + columns * boxWidth + (columns - 1) * gap;
  const height = pad * 2 + Math.ceil(rooms.length / columns) * boxHeight + (Math.ceil(rooms.length / columns) - 1) * gap;
  const short = value => String(value || '').length > 16 ? `${String(value).slice(0, 15)}…` : String(value || '');
  const arrow = edge => {
    const from = boxes.get(edge.from), to = boxes.get(edge.to);
    if (!from || !to || from === to) return null;
    const dx = to.x - from.x, dy = to.y - from.y;
    const across = Math.abs(dx) >= Math.abs(dy);
    const x1 = across ? from.x + (dx > 0 ? boxWidth : 0) : from.x + boxWidth / 2;
    const y1 = across ? from.y + boxHeight / 2 : from.y + (dy > 0 ? boxHeight : 0);
    const x2 = across ? to.x + (dx > 0 ? 0 : boxWidth) : to.x + boxWidth / 2;
    const y2 = across ? to.y + boxHeight / 2 : to.y + (dy > 0 ? 0 : boxHeight);
    const length = Math.hypot(x2 - x1, y2 - y1) || 1, ux = (x2 - x1) / length, uy = (y2 - y1) / length;
    return html`<g key=${`${edge.from}:${edge.to}`} class="team-flow-thumbnail-arrow">
      <line x1=${x1} y1=${y1} x2=${x2 - ux * 7} y2=${y2 - uy * 7} />
      <polygon points=${`${x2},${y2} ${x2 - ux * 9 - uy * 4},${y2 - uy * 9 + ux * 4} ${x2 - ux * 9 + uy * 4},${y2 - uy * 9 - ux * 4}`} />
    </g>`;
  };
  return html`<div class="team-flow-diagram team-flow-thumbnail" aria-label="Room and role diagram preview">
    <svg viewBox=${`0 0 ${width} ${height}`} role="img" aria-label=${rooms.map(room => `${room.label}: ${(room.roles || []).join(', ')}`).join('; ')}>
      ${edges.map(arrow)}
      ${rooms.map(room => { const box = boxes.get(room.key); return html`<g key=${room.key} class="team-flow-thumbnail-room">
        <title>${room.label}: ${(room.roles || []).join(', ')}</title>
        <rect x=${box.x} y=${box.y} width=${boxWidth} height=${boxHeight} rx="6" />
        <text x=${box.x + boxWidth / 2} y=${box.y + 22} class="team-flow-thumbnail-title">${short(room.label)}</text>
        <text x=${box.x + boxWidth / 2} y=${box.y + 39} class="team-flow-thumbnail-roles">${short((room.roles || []).join(' · '))}</text>
      </g>`; })}
    </svg>
  </div>`;
}

export function TeamFlowCards({ templates, selected, onSelect, disabled = false }) {
  const [openKey, setOpenKey] = useState(''), dialog = useRef(null);
  const opened = templates.find(template => template.key === openKey);
  useEffect(() => { if (opened && dialog.current && !dialog.current.open) dialog.current.showModal(); }, [openKey]);
  return html`<div class="team-flow-cards" role="radiogroup" aria-label="Team flow">
    ${templates.map((template, index) => html`<div key=${template.key} class=${`team-flow-option${selected === template.key ? ' selected' : ''}`}>
      <button type="button" class="btn team-flow-card" role="radio" aria-checked=${String(selected === template.key)} disabled=${disabled} onClick=${() => onSelect(template.key)}>
        <span class="team-flow-card-heading"><strong><span class="team-flow-letter" aria-hidden="true">${String.fromCharCode(65 + index)}</span>${template.label}</strong><span>${template.description}</span></span>
        <${DiagramPreview} diagram=${template.diagram} />
      </button><button type="button" class="team-flow-diagram-open" aria-label=${`Expand ${template.label} diagram`} title="Expand diagram" onClick=${() => setOpenKey(template.key)}><span>Expand diagram ↗</span></button>
    </div>`)}
    <dialog class="team-flow-lightbox" ref=${dialog} onClose=${() => setOpenKey('')} aria-label=${opened ? `${opened.label} diagram` : 'Team flow diagram'}>
      ${opened && html`<button type="button" class="btn team-flow-lightbox-close" onClick=${() => dialog.current?.close()}>Close diagram</button><h2>${opened.label}</h2><${Diagram} diagram=${opened.diagram} />`}
    </dialog>
  </div>`;
}

function ChangeList({ title, items, render }) {
  return items?.length > 0 && html`<div class="team-flow-change-group"><strong>${title}</strong><ul>${items.map((item, index) => html`<li key=${index}>${render(item)}</li>`)}</ul></div>`;
}

export function TeamFlowPreview({ preview, busy }) {
  if (busy) return html`<p class="team-flow-preview" role="status">Checking changes…</p>`;
  if (!preview) return null;
  const rooms = preview.create_rooms || [], roles = preview.role_changes || [], conflicts = preview.conflicts || [];
  return html`<div class="team-flow-preview" aria-label="Team flow change preview">
    ${!rooms.length && !roles.length && !conflicts.length && html`<p>No rooms or roles need changing.</p>`}
    <${ChangeList} title="Rooms to create" items=${rooms} render=${item => item.title || item.key} />
    <${ChangeList} title="Role changes" items=${roles} render=${item => item.description || item.action || item.role_key || JSON.stringify(item)} />
    <${ChangeList} title="Agents needing action" items=${conflicts} render=${item => `${item.agent_id}: ${item.reason}${item.suggested_action ? ` — ${item.suggested_action}` : ''}`} />
    <p class="team-flow-preserve">Existing rooms, messages, and agents are kept.</p>
  </div>`;
}

export function TeamFlowSettings({ flow, canEdit }) {
  const state = flow.state.value, templates = flow.templates.value, selected = flow.selected.value;
  return html`<section id="project-team-flow" class="settings-subsection" aria-labelledby="project-team-flow-title"><h3 id="project-team-flow-title">Team flow</h3>
    <div class="settings-card team-flow-settings">
      <p class="settings-desc">Choose who coordinates work. Switching keeps rooms, messages, and agents.</p>
      ${flow.loading.value && html`<p role="status">Loading team flows…</p>`}
      ${state && html`<p class="team-flow-current">Current: <strong>${templates.find(item => item.key === state.key)?.label || 'Not chosen'}</strong>${state.review_pending ? ' · Setup needs review' : ''}</p>`}
      <${TeamFlowCards} templates=${templates} selected=${selected} onSelect=${key => flow.select(key)} disabled=${flow.applying.value || !canEdit} />
      <${TeamFlowPreview} preview=${flow.preview.value} busy=${flow.previewing.value} />
      ${!flow.preview.value && state?.conflicts?.length > 0 && html`<div class="team-flow-preview"><${ChangeList} title="Agents needing action" items=${state.conflicts} render=${item => `${item.agent_id}: ${item.reason}${item.suggested_action ? ` — ${item.suggested_action}` : ''}`} /></div>`}
      <button type="button" class="btn btn-primary" disabled=${!canEdit || !state || !selected || !flow.preview.value || flow.applying.value || flow.previewing.value || state.key === selected && !state.review_pending}
        onClick=${() => flow.apply()}>${flow.applying.value ? 'Applying…' : state?.key === selected && !state?.review_pending ? 'Current flow' : 'Apply team flow'}</button>
      <p class="error" role="alert">${flow.error.value}</p>
    </div>
  </section>`;
}
