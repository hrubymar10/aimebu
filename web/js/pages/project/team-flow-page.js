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

// Core roles outside the spawn path (the advisor) hang off the root with a dashed "consults" edge.
const attachedRoles = (template, path) => (template?.core_roles || []).filter(role => !path.includes(role));

function diagramSource(template) {
  const path = workerPath(template);
  if (path.length < 2) return '';
  const lines = ['flowchart LR'];
  path.slice(0, -1).forEach((role, index) => lines.push(`  role${index}["${roleLabel(role)}"]`));
  for (let index = 0; index < 3; index++) lines.push(`  worker${index}["Worker"]`);
  path.slice(0, -2).forEach((_, index) => lines.push(`  role${index} --> role${index + 1}`));
  for (let index = 0; index < 3; index++) lines.push(`  role${path.length - 2} --> worker${index}`);
  attachedRoles(template, path).forEach((role, index) => {
    lines.push(`  attached${index}["${roleLabel(role)}"]`, `  role0 -. consults .-> attached${index}`);
  });
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
  const boxWidth = 62, boxHeight = 22, gapX = 14, gapY = 5, pad = 4, attachedDrop = 8;
  const centerY = pad + boxHeight + gapY + boxHeight / 2;
  // Attached roles (the advisor) sit under the root in the bottom row, a little lower so the dashed edge has room for its arrow; more than one stack
  // downward, each edge running behind the boxes above it.
  const attached = attachedRoles(template, path);
  const attachedY = index => pad + 2 * (boxHeight + gapY) + attachedDrop + index * (boxHeight + gapY);
  const nodes = [
    ...path.slice(0, -1).map((role, index) => ({ key: `role${index}`, label: roleLabel(role), x: pad + index * (boxWidth + gapX), y: centerY - boxHeight / 2 })),
    ...Array.from({ length: 3 }, (_, index) => ({ key: `worker${index}`, label: 'Worker', x: pad + (path.length - 1) * (boxWidth + gapX), y: pad + index * (boxHeight + gapY) })),
  ];
  attached.forEach((role, index) => nodes.push({ key: `attached${index}`, label: roleLabel(role), x: pad, y: attachedY(index) }));
  const boxes = new Map(nodes.map(node => [node.key, node]));
  const edges = [...path.slice(0, -2).map((_, index) => ({ from: `role${index}`, to: `role${index + 1}` })),
    ...Array.from({ length: 3 }, (_, index) => ({ from: `role${path.length - 2}`, to: `worker${index}` }))];
  const width = pad * 2 + path.length * boxWidth + (path.length - 1) * gapX;
  const height = pad * 2 + 3 * boxHeight + 2 * gapY + (attached.length ? attachedDrop + (attached.length - 1) * (boxHeight + gapY) : 0);
  const consults = attached.map((_, index) => {
    const x = pad + boxWidth / 2, y1 = boxes.get('role0').y + boxHeight, y2 = attachedY(index);
    return html`<g key=${`consults${index}`} class="team-flow-thumbnail-arrow team-flow-thumbnail-consults">
      <line x1=${x} y1=${y1} x2=${x} y2=${y2 - 7} />
      <polygon points=${`${x},${y2} ${x - 4},${y2 - 9} ${x + 4},${y2 - 9}`} />
      <title>consults</title>
    </g>`;
  });
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
    <svg viewBox=${`0 0 ${width} ${height}`} preserveAspectRatio="xMidYMid meet" role="img" aria-label=${`${path.slice(0, -1).map(roleLabel).join(' to ')} to three workers${attached.length ? `, consulting ${attached.map(roleLabel).join(' and ')}` : ''}`}>
      ${edges.map(arrow)}
      ${consults}
      ${nodes.map(box => html`<g key=${box.key} class="team-flow-thumbnail-agent">
        <title>${box.label}</title>
        <rect x=${box.x} y=${box.y} width=${boxWidth} height=${boxHeight} rx="6" />
        <text x=${box.x + boxWidth / 2} y=${box.y + boxHeight / 2 + 3}>${box.label}</text>
      </g>`)}
    </svg>
  </div>`;
}

// One line under each flow saying when it fits. The server may send its own `recommendation`; these are the defaults by key.
const RECOMMENDATIONS = {
  'leader-workers': 'Recommended for small to medium projects.',
  'leader-dispatcher': "Recommended for large projects; the dispatcher handles routing so the leader's context stays lean.",
  'leader-advisor': 'Recommended when a fast, cheaper leader should get a stronger model\'s judgment on key decisions.',
  custom: 'For setting up rooms and roles yourself.',
};
const COMING_LATER = { key: 'coming-later', label: 'Leader, advisor and dispatcher', description: 'Coming later.' };

export function TeamFlowCards({ templates, selected, currentKey = '', onSelect, disabled = false }) {
  const [openKey, setOpenKey] = useState(''), dialog = useRef(null);
  const opened = templates.find(template => template.key === openKey);
  // Letters follow the display order; the "coming later" card has none, and sits right before Custom (always last).
  const lettered = templates.map((template, index) => ({ template, letter: String.fromCharCode(65 + index) }));
  const customAt = lettered.findIndex(item => item.template.key === 'custom');
  const cards = customAt < 0 ? lettered : [...lettered.slice(0, customAt), { template: COMING_LATER, coming: true }, ...lettered.slice(customAt)];
  useEffect(() => { if (opened && dialog.current && !dialog.current.open) dialog.current.showModal(); }, [openKey]);
  return html`<div class="team-flow-cards" role="radiogroup" aria-label="Team flow">
    ${cards.map(({ template, letter, coming }) => coming ? html`<div key=${template.key} data-flow-key=${template.key} class="team-flow-option team-flow-coming">
      <button type="button" class="btn team-flow-card" role="radio" aria-checked="false" aria-disabled="true" disabled>
        <span class="team-flow-card-heading"><strong><span class="team-flow-letter" aria-hidden="true">·</span>${template.label}</strong><span>${template.description}</span></span>
        <p class="team-flow-diagram team-flow-empty-diagram">Not available yet.</p>
      </button>
    </div>` : html`<div key=${template.key} data-flow-key=${template.key} class=${`team-flow-option${selected === template.key ? ' selected' : ''}`}>
      <button type="button" class="btn team-flow-card" role="radio" aria-checked=${String(selected === template.key)} disabled=${disabled} onClick=${() => onSelect(template.key)}>
        <span class="team-flow-card-heading"><strong><span class="team-flow-letter" aria-hidden="true">${letter}</span>${template.label}</strong>${currentKey === template.key && html`<small class="team-flow-current-badge">Current</small>`}<span>${template.description}</span>${(template.recommendation || RECOMMENDATIONS[template.key]) && html`<small class="team-flow-recommendation">${template.recommendation || RECOMMENDATIONS[template.key]}</small>`}</span>
        <${DiagramPreview} template=${template} />
      </button>${diagramSource(template) && html`<button type="button" class="team-flow-diagram-open" aria-label=${`Expand ${template.label} diagram`} title="Expand diagram" onClick=${() => setOpenKey(template.key)}><span>Expand diagram ↗</span></button>`}
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
  const rooms = preview.create_rooms || [], restricted = preview.preserved_restricted_rooms || [];
  const roles = preview.role_changes || [], conflicts = preview.conflicts || [];
  return html`<div class="team-flow-preview" aria-label="Team flow change preview">
    ${!rooms.length && !restricted.length && !roles.length && !conflicts.length && html`<p>No rooms or roles need changing.</p>`}
    <${ChangeList} title="Rooms to create" items=${rooms} render=${item => item.title || item.key} />
    <${ChangeList} title="Stays restricted" items=${restricted} render=${item => item.title || item.room_id} />
    <${ChangeList} title="Role changes" items=${roles} render=${item => item.description || item.action || item.role_key || JSON.stringify(item)} />
    <${ChangeList} title="Agents needing action" items=${conflicts} render=${item => onToggleRemoval && item.room_id ? html`<label class="team-flow-removal"><input type="checkbox" checked=${selectedRemovals.some(selected => selected.agent_id === item.agent_id && selected.room_id === item.room_id)} onChange=${event => onToggleRemoval(item, event.currentTarget.checked)} />Remove ${conflictName(item)} from ${item.room_title} when applying</label>` : conflictText(item)} />
    <p class="team-flow-preserve">Existing rooms, messages, and agents are kept unless you select a membership to remove.</p>
  </div>`;
}
