import { html, useEffect, useRef, useState } from '../../ui.js';
import { queueMermaid } from '../../visual-plan.js';

function workerPath(template) {
  const rules = template?.spawn_rules || [];
  const actors = rules.map(rule => rule.actor_role);
  const children = new Set(rules.flatMap(rule => rule.allowed_roles || []));
  const root = actors.find(role => !children.has(role)) || actors[0];
  const visit = (role, path) => {
    if (role === 'worker') return path;
    for (const next of rules.find(rule => rule.actor_role === role)?.allowed_roles || []) {
      if (!path.includes(next)) {
        const found = visit(next, [...path, next]);
        if (found.length) return found;
      }
    }
    return [];
  };
  return root ? visit(root, [root]) : [];
}

const roleLabel = role => role.charAt(0).toUpperCase() + role.slice(1);

function diagramSource(template) {
  const path = workerPath(template);
  if (path.length < 2) return '';
  const lines = ['flowchart LR'];
  path.slice(0, -1).forEach((role, index) => lines.push(`  role${index}["${roleLabel(role)}"]`));
  for (let index = 0; index < 3; index++) lines.push(`  worker${index}["Worker"]`);
  path.slice(0, -2).forEach((_, index) => lines.push(`  role${index} --> role${index + 1}`));
  for (let index = 0; index < 3; index++) lines.push(`  role${path.length - 2} --> worker${index}`);
  return lines.join('\n');
}

function Diagram({ template }) {
  const source = diagramSource(template), node = useRef(null);
  useEffect(() => { if (source && node.current) queueMermaid(node.current, source); }, [source]);
  if (!source) return html`<p class="team-flow-diagram team-flow-empty-diagram">Choose your own agent topology.</p>`;
  return html`<div class="team-flow-diagram" aria-label="Agent topology diagram"><div ref=${node} class="team-flow-mermaid-source">${source}</div></div>`;
}

function DiagramPreview({ template }) {
  const path = workerPath(template);
  if (path.length < 2) return html`<p class="team-flow-diagram team-flow-empty-diagram">Choose your own agent topology.</p>`;
  const boxWidth = 70, boxHeight = 28, gapX = 18, gapY = 8, pad = 8;
  const centerY = pad + boxHeight + gapY + boxHeight / 2;
  const nodes = [
    ...path.slice(0, -1).map((role, index) => ({ key: `role${index}`, label: roleLabel(role), x: pad + index * (boxWidth + gapX), y: centerY - boxHeight / 2 })),
    ...Array.from({ length: 3 }, (_, index) => ({ key: `worker${index}`, label: 'Worker', x: pad + (path.length - 1) * (boxWidth + gapX), y: pad + index * (boxHeight + gapY) })),
  ];
  const boxes = new Map(nodes.map(node => [node.key, node]));
  const edges = [...path.slice(0, -2).map((_, index) => ({ from: `role${index}`, to: `role${index + 1}` })),
    ...Array.from({ length: 3 }, (_, index) => ({ from: `role${path.length - 2}`, to: `worker${index}` }))];
  const width = pad * 2 + path.length * boxWidth + (path.length - 1) * gapX;
  const height = pad * 2 + 3 * boxHeight + 2 * gapY;
  const arrow = edge => {
    const from = boxes.get(edge.from), to = boxes.get(edge.to);
    const x1 = from.x + boxWidth, y1 = from.y + boxHeight / 2;
    const x2 = to.x, y2 = to.y + boxHeight / 2;
    const length = Math.hypot(x2 - x1, y2 - y1) || 1, ux = (x2 - x1) / length, uy = (y2 - y1) / length;
    return html`<g key=${`${edge.from}:${edge.to}`} class="team-flow-thumbnail-arrow">
      <line x1=${x1} y1=${y1} x2=${x2 - ux * 7} y2=${y2 - uy * 7} />
      <polygon points=${`${x2},${y2} ${x2 - ux * 9 - uy * 4},${y2 - uy * 9 + ux * 4} ${x2 - ux * 9 + uy * 4},${y2 - uy * 9 - ux * 4}`} />
    </g>`;
  };
  return html`<div class="team-flow-diagram team-flow-thumbnail" aria-label="Agent topology diagram preview">
    <svg viewBox=${`0 0 ${width} ${height}`} role="img" aria-label=${`${path.slice(0, -1).map(roleLabel).join(' to ')} to three workers`}>
      ${edges.map(arrow)}
      ${nodes.map(box => html`<g key=${box.key} class="team-flow-thumbnail-agent">
        <title>${box.label}</title>
        <rect x=${box.x} y=${box.y} width=${boxWidth} height=${boxHeight} rx="6" />
        <text x=${box.x + boxWidth / 2} y=${box.y + boxHeight / 2 + 3}>${box.label}</text>
      </g>`)}
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
        <${DiagramPreview} template=${template} />
      </button><button type="button" class="team-flow-diagram-open" aria-label=${`Expand ${template.label} diagram`} title="Expand diagram" onClick=${() => setOpenKey(template.key)}><span>Expand diagram ↗</span></button>
    </div>`)}
    <dialog class="team-flow-lightbox" ref=${dialog} onClose=${() => setOpenKey('')} aria-label=${opened ? `${opened.label} diagram` : 'Team flow diagram'}>
      ${opened && html`<button type="button" class="btn team-flow-lightbox-close" onClick=${() => dialog.current?.close()}>Close diagram</button><h2>${opened.label}</h2><${Diagram} template=${opened} />`}
    </dialog>
  </div>`;
}

function ChangeList({ title, items, render }) {
  return items?.length > 0 && html`<div class="team-flow-change-group"><strong>${title}</strong><ul>${items.map((item, index) => html`<li key=${index}>${render(item)}</li>`)}</ul></div>`;
}

const conflictName = item => `${item.slug || item.agent_id} (${item.role_label || item.role_key || 'agent'})`;
const conflictText = item => `${conflictName(item)} in ${item.room_title || 'a room'}: ${item.reason}`;

export function TeamFlowPreview({ preview, busy, selectedRemovals = [], onToggleRemoval }) {
  if (busy) return html`<p class="team-flow-preview" role="status">Checking changes…</p>`;
  if (!preview) return null;
  const rooms = preview.create_rooms || [], roles = preview.role_changes || [], conflicts = preview.conflicts || [];
  return html`<div class="team-flow-preview" aria-label="Team flow change preview">
    ${!rooms.length && !roles.length && !conflicts.length && html`<p>No rooms or roles need changing.</p>`}
    <${ChangeList} title="Rooms to create" items=${rooms} render=${item => item.title || item.key} />
    <${ChangeList} title="Role changes" items=${roles} render=${item => item.description || item.action || item.role_key || JSON.stringify(item)} />
    <${ChangeList} title="Agents needing action" items=${conflicts} render=${item => onToggleRemoval && item.room_id ? html`<label class="team-flow-removal"><input type="checkbox" checked=${selectedRemovals.some(selected => selected.agent_id === item.agent_id && selected.room_id === item.room_id)} onChange=${event => onToggleRemoval(item, event.currentTarget.checked)} />Remove ${conflictName(item)} from ${item.room_title} when applying</label>` : conflictText(item)} />
    <p class="team-flow-preserve">Existing rooms, messages, and agents are kept unless you select a membership to remove.</p>
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
      <${TeamFlowPreview} preview=${flow.preview.value} busy=${flow.previewing.value} selectedRemovals=${flow.removals.value} onToggleRemoval=${(item, checked) => flow.toggleRemoval(item, checked)} />
      ${!flow.preview.value && state?.conflicts?.length > 0 && html`<div class="team-flow-preview"><${ChangeList} title="Agents needing action" items=${state.conflicts} render=${conflictText} /></div>`}
      <button type="button" class="btn btn-primary" disabled=${!canEdit || !state || !selected || !flow.preview.value || flow.applying.value || flow.previewing.value || state.key === selected && !state.review_pending}
        onClick=${() => flow.apply()}>${flow.applying.value ? 'Applying…' : state?.key === selected && !state?.review_pending ? 'Current flow' : 'Apply team flow'}</button>
      <p class="error" role="alert">${flow.error.value}</p>
    </div>
  </section>`;
}
