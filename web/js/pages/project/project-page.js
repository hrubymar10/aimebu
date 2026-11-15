import { html } from '../../ui.js';
import { useLayoutEffect, useRef } from '../../ui.js';
import { attachFolderField } from '../../folder-field.js';
import { IconButton } from '../../components/icon-button.js';
import { ScopeControl } from '../../components/scope-control.js';
import { UsageSummary } from '../../components/usage-summary.js';
import { UsageLimit } from './usage-limit.js';
import { RoleDefaults } from './role-defaults.js';
import { TeamFlowSettings } from './team-flow-settings.js';

function Toggle({ id, label, desc, risk, flag, onChange }) {
  const described = risk ? `${id}-desc ${id}-risk` : `${id}-desc`;
  return html`<div class="settings-row settings-row-toggle"><div class="settings-row-info"><label class="settings-label" for=${id}>${label}</label><span class="settings-desc" id=${`${id}-desc`}>${desc}</span>${risk && html`<span class="settings-risk" id=${`${id}-risk`}>${risk}</span>`}</div>
    <div class="settings-control"><input id=${id} type="checkbox" aria-describedby=${described} checked=${flag.checked} disabled=${flag.disabled} onChange=${event => onChange(event.currentTarget.checked)} /></div></div>`;
}

function Details({ store }) {
  const folderInput = useRef(null);
  useLayoutEffect(() => { attachFolderField(folderInput.current, () => ''); }, []);
  const view = store.view.value, project = store.project.value;
  return html`<form id="project-details" class="settings-group" role="group" aria-label="Project details" onSubmit=${event => { event.preventDefault(); const form = event.currentTarget.elements; store.saveDetails({ name: form.name.value, folder: form.folder.value }); }}>
    <div class="settings-row"><div class="settings-row-info"><label class="settings-label" for="project-details-name">Name</label><span class="settings-desc" id="project-details-name-desc">How the project is listed in the sidebar.</span></div>
      <div class="settings-control"><input class="settings-text-input" id="project-details-name" name="name" autocomplete="off" aria-describedby="project-details-name-desc" required value=${store.name.value} disabled=${!view.canName} onInput=${event => { store.name.value = event.currentTarget.value; }} /></div></div>
    <div class="settings-row"><div class="settings-row-info"><label class="settings-label" for="project-details-folder">Folder</label><span class="settings-desc" id="project-details-folder-desc">An absolute path to a folder on the machine running the server. It can change only while the project has no agents.</span><p id="project-details-status" class="settings-desc" role="status">${store.detailsStatus.value}</p></div>
      <div class="settings-control"><input ref=${folderInput} class="settings-text-input" id="project-details-folder" name="folder" autocomplete="off" aria-describedby="project-details-folder-desc" required value=${store.folder.value} disabled=${!view.canFolder} onInput=${event => { store.folder.value = event.currentTarget.value; }} />
        <button type="submit" class="btn btn-primary" id="project-details-save" disabled=${(!view.canName && !view.canFolder) || view.archived || store.detailsBusy.value}>Save</button></div></div></form>`;
}

function LifecycleRow({ id, title, desc, label, glyph, danger, hidden, disabled, onClick }) {
  return html`<div class="settings-row" id=${`${id}-row`} hidden=${hidden}><div class="settings-row-info"><label class="settings-label" for=${id}>${title}</label><span class="settings-desc" id=${`${id}-desc`}>${desc}</span></div>
    <div class="settings-control"><${IconButton} id=${id} name=${glyph} label=${label} danger=${danger} disabled=${disabled} aria-describedby=${`${id}-desc`} onClick=${onClick} /></div></div>`;
}

export function ProjectPage({ store }) {
  const lifecycle = store.lifecycle(), allowed = store.view.value.canName, busy = store.lifecycleBusy.value, flags = store.flags;
  const off = key => !allowed || busy === key;
  return html`<${Details} store=${store} />
    <section id="project-scope-control" class="scope-control settings-subsection" aria-labelledby="project-scope-control-title" data-desired=${store.scope.state.value?.desired || ''}>
      <h3 id="project-scope-control-title">Agents in this project</h3><div class="settings-card"><${ScopeControl} store=${store.scope} /></div></section>
    <section id="project-usage-limit" class="usage-limit settings-subsection" aria-labelledby="project-usage-limit-title"><${UsageLimit} store=${store.limit} /></section>
    <${RoleDefaults} store=${store} />
    <${TeamFlowSettings} flow=${store.flow} project=${store.project.value} canEdit=${!!store.project.value && ['owner', 'operator'].includes(store.project.value.role)} />
    <section id="project-usage" class="usage-summary settings-subsection" aria-label="Project usage"><${UsageSummary} store=${store.usage} /></section>
    <div class="settings-subsection"><h3>Home room</h3><div class="settings-group" role="group" aria-label="Home room">
      <${Toggle} id="project-home-lead-only" label="Home room: lead only" desc="Only roles that lead can be spawned into the project's home room; other agents go to task rooms such as general. Agents already in the home room stay." flag=${flags.homeLeadOnly.value} onChange=${checked => store.saveFlag('homeLeadOnly', checked)} /></div></div>
    <div class="settings-subsection"><h3>Recovery</h3><div class="settings-group" role="group" aria-label="Recovery">
      <${Toggle} id="project-auto-resume" label="Auto-resume after a clean diagnostic report" desc="Restart a stopped agent automatically when its diagnostic report finds nothing wrong." risk="A model's report can miss unobserved effects, such as a network write. Resuming may repeat an action." flag=${flags.autoResume.value} onChange=${checked => store.saveFlag('autoResume', checked)} />
      <${Toggle} id="project-worktrees" label="Separate Git worktree for each task room" desc=${html`Agents in a task room work in their own checkout under <code>.aimebu/worktrees</code>, so rooms do not overwrite each other. A folder that is not a Git repository keeps using the project folder. Merged, clean worktrees are removed automatically; anything unmerged or uncommitted is kept.`} flag=${flags.worktrees.value} onChange=${checked => store.saveFlag('worktrees', checked)} /></div></div>
    <div class="settings-subsection" id="project-session-import" hidden=${!lifecycle.importer}><h3>Claude Code sessions</h3><div class="settings-group" role="group" aria-label="Claude Code sessions"><div class="settings-row"><div class="settings-row-info"><label class="settings-label" for="project-session-import-open">Import sessions</label><span class="settings-desc" id="project-session-import-desc">Turn the Claude Code sessions a machine has for this project's folder into agents: one room and one agent per session, each working on a copy and idle until it gets a message. The project's agent and spawn-rate limits apply.</span></div>
      <div class="settings-control"><${IconButton} id="project-session-import-open" name="play" label="Import Claude Code sessions" disabled=${!lifecycle.importer} aria-describedby="project-session-import-desc" onClick=${() => store.openSessionImport()} /></div></div></div></div>
    <div class="settings-subsection" id="project-lifecycle" hidden=${!lifecycle.show}><h3>Archive</h3><div class="settings-group" role="group" aria-label="Archive and delete">
      <${LifecycleRow} id="project-archive" title="Archive project" label="Archive project" glyph="archive" hidden=${!lifecycle.archive} disabled=${off('archive')} onClick=${() => store.archive()} desc="Make the project and all its rooms read-only, stop its agents and switch its schedules off. It moves to Archived projects in the room list, and can be reopened." />
      <${LifecycleRow} id="project-reopen" title="Reopen project" label="Reopen project" glyph="reopen" hidden=${!lifecycle.reopen} disabled=${off('reopen')} onClick=${() => store.reopen()} desc="Let the project accept changes again. Its agents stay stopped and its schedules stay off until you start them." />
      <${LifecycleRow} id="project-delete" title="Delete project" label="Delete project" glyph="trash" danger hidden=${!lifecycle.remove} disabled=${off('delete')} onClick=${() => store.remove()} desc="Stop its agents, then delete this project for good with every room, message, attachment, agent, memory and setting in it. Files in the project folder are not touched." /></div>
      <p class="settings-desc" role="status">${store.lifecycleNote.value}</p></div>
    <p id="project-settings-error" class="error" role="alert">${store.error.value}</p>`;
}
