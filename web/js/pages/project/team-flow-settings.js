import { createPortal, html, useEffect, useRef, useState } from '../../ui.js';
import { TeamWizard } from './team-setup-page.js';
import { flowLetter, MIGRATION_TERMINAL } from './team-wizard-parts.js';

const conflictName = item => `${item.slug || item.agent_id} (${item.role_label || item.role_key || 'agent'})`;

// Project settings -> Team flow: the current flow, and the same wizard the home room uses to change it.
export function TeamFlowSettings({ flow, canEdit, project }) {
  const state = flow.state.value, templates = flow.templates.value;
  const [open, setOpen] = useState(false);
  const dialog = useRef(null), settings = useRef(null), openedProject = useRef(''), restoreSettings = useRef(false);
  if (!dialog.current) {
    dialog.current = document.createElement('dialog');
    dialog.current.id = 'team-flow-modal';
    dialog.current.className = 'team-flow-modal';
    dialog.current.setAttribute('aria-labelledby', 'team-flow-home-title');
  }
  useEffect(() => { const node = dialog.current; document.body.append(node); return () => { node.onclose = null; if (node.open) node.close(); node.remove(); }; }, []);
  useEffect(() => { if (flow.projectID && dialog.current?.open && openedProject.current !== flow.projectID) dialog.current.close(); }, [flow.projectID]);
  useEffect(() => { if (open && !dialog.current?.open) dialog.current?.showModal(); }, [open]);
  // A change that is already running (started from another tab or by someone else) shows its progress straight away.
  useEffect(() => {
    const id = flow.projectID;
    if (!id || !canEdit || !state?.key) return undefined;
    let current = true;
    Promise.resolve(flow.source.migration?.(id)).then(answer => {
      const running = answer?.migration || answer;
      if (current && running?.id && !MIGRATION_TERMINAL.includes(running.state) && document.querySelector('#room-info-dialog')?.open) show();
    }, () => {});
    return () => { current = false; };
  }, [flow.projectID, canEdit, state?.key]);
  const show = () => {
    if (open) return;
    settings.current = document.querySelector('#room-info-dialog');
    openedProject.current = flow.projectID;
    restoreSettings.current = !!settings.current?.open;
    if (restoreSettings.current) settings.current.close();
    setOpen(true);
  };
  const close = () => {
    setOpen(false);
    if (state?.key) void flow.select(state.key);
    if (restoreSettings.current && settings.current && openedProject.current === flow.projectID && !settings.current.open) settings.current.showModal();
    settings.current = null;
    restoreSettings.current = false;
  };
  const dismiss = () => dialog.current?.close();
  dialog.current.onclose = close;
  const current = templates.find(item => item.key === state?.key);
  return html`<section id="project-team-flow" class="settings-subsection" aria-labelledby="project-team-flow-title"><h3 id="project-team-flow-title">Team flow</h3>
    <div class="settings-card team-flow-settings">
      <p class="settings-desc">Choose who coordinates work. Switching keeps the leader and your rooms and messages; other agents are kept, added, or hand off and stop.</p>
      ${flow.loading.value && html`<p role="status">Loading team flows…</p>`}
      ${state && html`<p class="team-flow-current">Current: <strong>${current ? `${flowLetter(templates, current.key)} · ${current.label}` : 'Not chosen'}</strong>${state.review_pending ? ' · Setup needs review' : ''}</p>`}
      ${!open && state?.conflicts?.length > 0 && html`<div class="team-flow-preview team-flow-warning" role="alert"><div class="team-flow-change-group"><strong>⚠ Agents needing action</strong><ul>${state.conflicts.map((item, index) => html`<li key=${index}>${conflictName(item)} in ${item.room_title || 'a room'}: ${item.reason} <button type="button" class="btn" onClick=${show}>Review in wizard</button></li>`)}</ul></div></div>`}
      ${!open && html`<button type="button" class="btn btn-primary" disabled=${!canEdit || !state} onClick=${show}>Change team flow</button>`}
      ${open && createPortal(html`
        <header class="team-flow-modal-header"><button type="button" class="btn team-flow-modal-close" onClick=${dismiss}>Back to settings</button></header>
        ${!state && html`<p class="team-flow-modal-status" role=${flow.error.value ? 'alert' : 'status'}>${flow.error.value || 'Loading team flows…'}</p>`}
        <${TeamWizard} flow=${flow} canEdit=${canEdit} project=${project} embedded=${true} onClose=${dismiss} />
      `, dialog.current)}
      <p class="error" role="alert">${open ? '' : flow.error.value}</p>
    </div>
  </section>`;
}
