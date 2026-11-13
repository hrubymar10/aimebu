import { html } from '../../ui.js';

function Diagram({ diagram }) {
  const rooms = diagram?.rooms || [], edges = diagram?.edges || [];
  if (!rooms.length) return html`<p class="team-flow-diagram team-flow-empty-diagram">Choose your own rooms and roles.</p>`;
  return html`<div class="team-flow-diagram" aria-label="Room and role diagram">
    <div class="team-flow-diagram-rooms">${rooms.map(room => html`<div class="team-flow-diagram-room" key=${room.key}>
      <strong>${room.label}</strong><span>${(room.roles || []).join(' · ')}</span>
    </div>`)}</div>
    ${edges.length > 0 && html`<div class="team-flow-diagram-links">${edges.map((edge, index) => html`<span key=${index}>${edge.from} → ${edge.to}</span>`)}</div>`}
  </div>`;
}

export function TeamFlowCards({ templates, selected, onSelect, disabled = false }) {
  return html`<div class="team-flow-cards" role="radiogroup" aria-label="Team flow">
    ${templates.map((template, index) => html`<button type="button" key=${template.key} class=${`btn team-flow-card${selected === template.key ? ' selected' : ''}`}
      role="radio" aria-checked=${String(selected === template.key)} disabled=${disabled} onClick=${() => onSelect(template.key)}>
      <span class="team-flow-card-heading"><strong><span class="team-flow-letter" aria-hidden="true">${String.fromCharCode(65 + index)}</span>${template.label}</strong><span>${template.description}</span></span>
      <${Diagram} diagram=${template.diagram} />
    </button>`)}
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

export function TeamFlowHomePage({ flow, canEdit }) {
  const state = flow.state.value, templates = flow.templates.value, selected = flow.selected.value;
  if (!state && flow.loading.value) return html`<section class="team-flow-home-card" role="status">Loading team flows…</section>`;
  if (!state && flow.error.value) return html`<section class="team-flow-home-card"><p class="error" role="alert">Could not load team flows: ${flow.error.value}</p><button type="button" class="btn" onClick=${() => flow.load(flow.projectID)}>Retry</button></section>`;
  if (!state || !state.review_pending && state.key) return null;
  const migrated = state.key === 'custom' && state.review_pending, keepCurrent = migrated && selected === 'custom';
  return html`<section class="team-flow-home-card" aria-labelledby="team-flow-home-title">
    <div class="team-flow-heading"><small>Project setup</small><h2 id="team-flow-home-title">Choose how your AI team works</h2>
      <p>${migrated ? 'Your existing setup is kept. Choose another flow, or keep the current custom setup.' : 'Pick a starting team flow. You can change it later in Project settings.'}</p></div>
    <${TeamFlowCards} templates=${templates} selected=${selected} onSelect=${key => flow.select(key)} disabled=${flow.applying.value || !canEdit} />
    <${TeamFlowPreview} preview=${flow.preview.value} busy=${flow.previewing.value} />
    <div class="team-flow-actions"><button type="button" class="btn btn-primary" disabled=${!canEdit || flow.applying.value || flow.previewing.value || !selected || !flow.preview.value} onClick=${() => flow.apply()}>
      ${flow.applying.value ? 'Applying…' : keepCurrent ? 'Keep current setup' : 'Apply team flow'}
    </button>${!canEdit && html`<span>Ask a project owner or operator to choose a flow.</span>`}</div>
    <p class="error" role="alert">${flow.error.value}</p>
  </section>`;
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
