import { html, useEffect, useState } from '../../ui.js';
import { TeamWizard } from './team-setup-page.js';
import { flowLetter, MIGRATION_TERMINAL } from './team-wizard-parts.js';

const conflictName = item => `${item.slug || item.agent_id} (${item.role_label || item.role_key || 'agent'})`;

// Project settings -> Team flow: the current flow, and the same wizard the home room uses to change it.
export function TeamFlowSettings({ flow, canEdit, project }) {
  const state = flow.state.value, templates = flow.templates.value;
  const [open, setOpen] = useState(false);
  useEffect(() => { setOpen(false); }, [flow.projectID]);
  // A change that is already running (started from another tab or by someone else) shows its progress straight away.
  useEffect(() => {
    const id = flow.projectID;
    if (!id || !canEdit || !state?.key) return undefined;
    let current = true;
    Promise.resolve(flow.source.migration?.(id)).then(answer => {
      const running = answer?.migration || answer;
      if (current && running?.id && !MIGRATION_TERMINAL.includes(running.state)) setOpen(true);
    }, () => {});
    return () => { current = false; };
  }, [flow.projectID, canEdit, state?.key]);
  const close = () => { setOpen(false); if (state?.key) void flow.select(state.key); };
  const current = templates.find(item => item.key === state?.key);
  return html`<section id="project-team-flow" class="settings-subsection" aria-labelledby="project-team-flow-title"><h3 id="project-team-flow-title">Team flow</h3>
    <div class="settings-card team-flow-settings">
      <p class="settings-desc">Choose who coordinates work. Switching keeps the leader and your rooms and messages; other agents are kept, added, or hand off and stop.</p>
      ${flow.loading.value && html`<p role="status">Loading team flows…</p>`}
      ${state && html`<p class="team-flow-current">Current: <strong>${current ? `${flowLetter(templates, current.key)} · ${current.label}` : 'Not chosen'}</strong>${state.review_pending ? ' · Setup needs review' : ''}</p>`}
      ${!open && state?.conflicts?.length > 0 && html`<div class="team-flow-preview"><div class="team-flow-change-group"><strong>Agents needing action</strong><ul>${state.conflicts.map((item, index) => html`<li key=${index}>${conflictName(item)} in ${item.room_title || 'a room'}: ${item.reason}</li>`)}</ul></div></div>`}
      ${open && state ? html`<${TeamWizard} flow=${flow} canEdit=${canEdit} project=${project} embedded=${true} onClose=${close} />`
        : html`<button type="button" class="btn btn-primary" disabled=${!canEdit || !state} onClick=${() => setOpen(true)}>Change team flow</button>`}
      <p class="error" role="alert">${open ? '' : flow.error.value}</p>
    </div>
  </section>`;
}
