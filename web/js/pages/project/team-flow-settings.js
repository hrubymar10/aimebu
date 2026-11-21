import { createPortal, html, useEffect, useRef, useState } from '../../ui.js';
import { lazyComponent } from '../../lazy.js';
import { IconButton } from '../../components/icon-button.js';
import { captureError } from '../../telemetry.js';
import { dismissedMigration, dismissMigration, flowLetter, interruptAgent, MigrationProgress, MIGRATION_TERMINAL } from './team-wizard-parts.js';

// The wizard is a lazily fetched chunk (it is drawn only when a team flow is set up or changed).
const TeamWizard = lazyComponent(() => import('./team-setup-page.js').then(module => module.TeamWizard), 'team-wizard');

const conflictName = item => `${item.slug || item.agent_id} (${item.role_label || item.role_key || 'agent'})`;

// Project settings -> Team flow: the current flow, and the same wizard the home room uses to change it.
export function TeamFlowSettings({ flow, canEdit, project }) {
  const state = flow.state.value, templates = flow.templates.value;
  const [open, setOpen] = useState(false);
  // A change running in the background: its progress shows here, outside the wizard, until it ends and the person dismisses it.
  const [background, setBackground] = useState(null), [working, setWorking] = useState(''), [progressError, setProgressError] = useState('');
  const backgrounded = useRef(''), generationRef = useRef(0);
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
  useEffect(() => { generationRef.current++; backgrounded.current = ''; setBackground(null); setWorking(''); setProgressError(''); }, [flow.projectID]);
  // A change that is already running (started from another tab or by someone else) shows its progress straight away, in the wizard when
  // settings is open (unless the person sent it to the background) and under the flow otherwise.
  const refreshMigration = (id, turn, showWizard) => Promise.resolve(flow.source.migration?.(id)).then(answer => {
    const running = answer?.migration || answer;
    if (turn !== generationRef.current || !running?.id) return;
    // A finished change shows its outcome here until it is dismissed.
    if (MIGRATION_TERMINAL.includes(running.state)) { if (dismissedMigration(id) !== running.id) setBackground(running); return; }
    if (showWizard && backgrounded.current !== running.id && document.querySelector('#room-info-dialog')?.open) show();
    else setBackground(running);
  }, () => {});
  useEffect(() => {
    const id = flow.projectID;
    if (!id || !canEdit || !state?.key) return undefined;
    const turn = generationRef.current;
    void refreshMigration(id, turn, true);
    return undefined;
  }, [flow.projectID, canEdit, state?.key]);
  // Following the background change until it ends; a finished one reloads the flow, as the wizard's Done does.
  useEffect(() => {
    if (open || !background?.id || MIGRATION_TERMINAL.includes(background.state)) return undefined;
    const id = flow.projectID, turn = generationRef.current;
    const timer = setInterval(async () => {
      try {
        const answer = await flow.source.migration(id), latest = answer?.migration || answer;
        if (turn !== generationRef.current || !latest?.id) return;
        setBackground(latest);
        if (latest.state === 'done') await flow.completeMigration(id, latest);
      } catch (error) { captureError(error, 'team-flow-settings.poll'); /* the next poll tries again */ }
    }, 2000);
    return () => clearInterval(timer);
  }, [open, background?.id, background?.state, flow.projectID]);
  const interruptBackground = async agent => {
    const id = flow.projectID, turn = generationRef.current;
    setWorking('interrupt'); setProgressError('');
    try { const latest = await interruptAgent(flow.source, id, agent); if (latest?.id && turn === generationRef.current) setBackground(latest); }
    catch (failure) { setProgressError(failure.message); }
    finally { setWorking(''); }
  };
  const cancelBackground = async () => {
    const id = flow.projectID, turn = generationRef.current;
    setWorking('cancel'); setProgressError('');
    try { const answer = await flow.source.cancelMigration(id); if (turn === generationRef.current) setBackground(answer?.migration || answer); }
    catch (failure) { setProgressError(failure.message); }
    finally { setWorking(''); }
  };
  const sendToBackground = migration => { backgrounded.current = migration.id; setBackground(migration); setProgressError(''); dialog.current?.close(); };
  const show = () => {
    if (open) return;
    settings.current = document.querySelector('#room-info-dialog');
    openedProject.current = flow.projectID;
    setBackground(null); setProgressError('');
    restoreSettings.current = !!settings.current?.open;
    if (restoreSettings.current) settings.current.close();
    setOpen(true);
  };
  const close = () => {
    setOpen(false);
    // The wizard goes away with a change still running when the person left it without choosing the background: that change still shows here.
    if (flow.projectID && canEdit) void refreshMigration(flow.projectID, generationRef.current, false);
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
      ${!open && background && html`<div id="team-flow-progress" class="team-flow-background-progress"><${MigrationProgress} migration=${background} onCancel=${cancelBackground} cancelling=${working === 'cancel'} onInterrupt=${interruptBackground} interrupting=${working === 'interrupt'} />
        ${MIGRATION_TERMINAL.includes(background.state) && html`<${IconButton} name="close" label="Dismiss" onClick=${() => { dismissMigration(flow.projectID, background.id); setBackground(null); }} />`}
        <p class="error" role="alert">${progressError}</p></div>`}
      ${!open && html`<button type="button" class="btn btn-primary" disabled=${!canEdit || !state} onClick=${show}>Change team flow</button>`}
      ${open && createPortal(html`
        <header class="team-flow-modal-header"><button type="button" class="btn team-flow-modal-close" onClick=${dismiss}>Back to settings</button></header>
        ${!state && html`<p class="team-flow-modal-status" role=${flow.error.value ? 'alert' : 'status'}>${flow.error.value || 'Loading team flows…'}</p>`}
        <${TeamWizard} flow=${flow} canEdit=${canEdit} project=${project} embedded=${true} onClose=${dismiss} onBackground=${sendToBackground} />
      `, dialog.current)}
      <p class="error" role="alert">${open ? '' : flow.error.value}</p>
    </div>
  </section>`;
}
