import { html } from '../../ui.js';
import { request } from '../../api.js';
import { ModelPicker } from '../agents/model-picker.js';
import { IconButton } from '../../components/icon-button.js';
import { confirmDialog } from '../../dialogs.js';

// Pieces of the team-flow wizard that carry no wizard state: choices of machine/tool/model, the pickers, and the switch-mode banners,
// recommendations, kept-agent rows, summary and progress. The wizard itself is team-setup-page.js.

export const projectPath = id => `/api/projects/${encodeURIComponent(id)}`;
const chosen = (items, old) => items.find(item => item.value === old) || items[0];
const targetOf = (machine, tool, model) => ({ machine_id: machine.id, tool: tool.tool, variant: tool.variant, launcher_id: tool.launcher_id, model: model.id });
export const cleanTarget = item => item && ({ machine_id: item.machine_id, tool: item.tool, variant: item.variant, launcher_id: item.launcher_id, model: item.model });
export const sameTarget = (a, b) => a?.machine_id === b?.machine_id && a?.launcher_id === b?.launcher_id && a?.model === b?.model;
export const roleLabel = role => role.charAt(0).toUpperCase() + role.slice(1);
export const flowLetter = (templates, key) => { const index = templates.findIndex(item => item.key === key); return index < 0 ? '' : String.fromCharCode(65 + index); };

export async function loadChoices(projectID) {
  const [machinesResult, defaults] = await Promise.all([
    request(`/api/machines?project_id=${encodeURIComponent(projectID)}`),
    request(`${projectPath(projectID)}/role-spawn-defaults`),
  ]);
  const machines = (machinesResult.machines || []).filter(machine => !machine.revoked_at && machine.approved && machine.connected && machine.project_allowed);
  const catalogs = await Promise.all(machines.map(async machine => {
    try { return (await request(`/api/spawn-catalog?machine_id=${encodeURIComponent(machine.id)}`)).tools || []; }
    catch { return []; }
  }));
  const options = machines.flatMap((machine, index) => catalogs[index].filter(tool => tool.available && tool.launcher_id).flatMap(tool =>
    (tool.models || []).filter(model => model.id && !model.unavailable).map(model => ({
      ...targetOf(machine, tool, model), machine_name: machine.name, tool_label: tool.label, model_label: model.label || model.id,
      default_model: !!model.default, containerized: !!tool.containerized, shares_claude_home: !!tool.shares_claude_home,
    }))));
  return { machines, options, defaults: Array.isArray(defaults) ? defaults : [] };
}

export function initialTarget(options, defaults, role) {
  const agent = defaults.find(item => item.role_key === role)?.default_agents?.[0];
  return cleanTarget(options.find(item => item.launcher_id === agent?.launcher_id && item.model === agent?.model) || options.find(item => item.default_model) || options[0]);
}
export function initialDefaults(options, defaults, role) {
  const row = defaults.find(item => item.role_key === role);
  const available = (row?.default_agents || []).filter(item => options.some(option => option.launcher_id === item.launcher_id && option.model === item.model));
  const first = initialTarget(options, defaults, role);
  return { items: available.length ? available : first ? [{ launcher_id: first.launcher_id, model: first.model }] : [], mode: row?.pick_mode || 'in_turn' };
}

// "Box · Claude · Opus" for a target, from the machine and catalog choices when they know it, else from what the agent card says.
export function describeTarget(options, target) {
  const item = options.find(option => sameTarget(option, target));
  if (item) return `${item.machine_name} · ${item.tool_label} · ${item.model_label}`;
  return target ? [target.machine_name || target.machine_id, target.launcher_label || target.tool, target.model].filter(Boolean).join(' · ') : 'No available agent';
}

export function TargetPicker({ label, prefix, options, value, onChange, withName = true, name, nameBusy, nameError, onName, onDice }) {
  const machines = [...new Map(options.map(option => [option.machine_id, { value: option.machine_id, label: option.machine_name }])).values()];
  const machine = chosen(machines, value?.machine_id);
  const tools = [...new Map(options.filter(option => option.machine_id === machine?.value).map(option => [option.launcher_id, { value: option.launcher_id, label: option.tool_label }])).values()];
  const tool = chosen(tools, value?.launcher_id);
  const models = options.filter(option => option.machine_id === machine?.value && option.launcher_id === tool?.value);
  const change = next => { if (next) onChange({ machine_id: next.machine_id, tool: next.tool, variant: next.variant, launcher_id: next.launcher_id, model: next.model }); };
  return html`<fieldset class="team-setup-target"><legend>${label}</legend>
    ${withName && html`<div class="team-setup-name"><label for=${`${prefix}-name`}>Name</label><div class="team-setup-name-row"><input id=${`${prefix}-name`} autocomplete="off" autocapitalize="none" spellcheck="false" maxlength="21" placeholder="Automatic" aria-describedby=${`${prefix}-name-error`} value=${name || ''} onInput=${event => onName(event.currentTarget.value)} /><${IconButton} name="dice" label="Another name" id=${`${prefix}-name-dice`} disabled=${nameBusy} onClick=${onDice} /></div><p id=${`${prefix}-name-error`} class="error team-setup-name-error" role="alert">${nameError}</p></div>`}
    <label>Machine<select aria-label=${`${label} machine`} value=${machine?.value || ''} onChange=${event => change(options.find(option => option.machine_id === event.currentTarget.value && option.default_model) || options.find(option => option.machine_id === event.currentTarget.value))}>
      ${machines.map(item => html`<option key=${item.value} value=${item.value}>${item.label}</option>`)}</select></label>
    <label>Tool<select aria-label=${`${label} tool`} value=${tool?.value || ''} onChange=${event => change(options.find(option => option.machine_id === machine?.value && option.launcher_id === event.currentTarget.value && option.default_model) || options.find(option => option.machine_id === machine?.value && option.launcher_id === event.currentTarget.value))}>
      ${tools.map(item => html`<option key=${item.value} value=${item.value}>${item.label}</option>`)}</select></label>
    <${ModelPicker} prefix=${prefix} models=${models.map(item => ({ value: item.model, label: item.model_label }))} value=${value?.model || ''} onChange=${model => change(models.find(item => item.model === model))} />
  </fieldset>`;
}

// What happens to a role between the current flow and the chosen one, in the words of the spec.
export function StatusBanner({ status, role, from, to, plural = false, reactivate = false }) {
  const name = roleLabel(role);
  const text = status === 'kept' ? `${plural ? `${name} are` : `${name} is`} kept between ${from} → ${to} migration`
    : status === 'new' ? (reactivate ? `${name} was stopped earlier and will be started again in ${to}` : `${name} is new in ${to} and will be added`)
      : status === 'leaving' ? `${name} isn't used in ${to}. It will hand off its context to the leader and will be stopped`
        : '';
  return text ? html`<p class=${`team-wizard-banner team-wizard-banner-${status}`} role="status">${text}</p>` : null;
}

// Show each available harness on the chosen machine against its own live catalog.
export function RecommendedModels({ rows, role, flowKey, options = [], target, label = '' }) {
  const machineID = target?.machine_id || options[0]?.machine_id;
  const launchers = [...new Map(options.filter(option => option.machine_id === machineID)
    .map(option => [option.launcher_id, option])).values()];
  const groups = launchers.map(launcher => {
    const catalog = options.filter(option => option.launcher_id === launcher.launcher_id && option.machine_id === machineID);
    const models = (rows || []).filter(row => row.role_key === role && row.flow_key === flowKey && (!row.tool || row.tool === launcher.tool))
      .flatMap(row => (row.models || []).map(item => ({ ...item, offered: catalog.find(option => option.model === item.model) })))
      .filter(item => item.offered);
    return { launcher, models };
  }).filter(group => group.models.length);
  if (!groups.length) return null;
  return html`<aside class="team-wizard-recommended" aria-label="Recommended models"><strong>💡 Recommended models${label ? ` for ${label}` : ''}</strong>
    ${groups.map(({ launcher, models }) => html`<div key=${launcher.launcher_id} class=${`team-wizard-recommended-group${target?.launcher_id === launcher.launcher_id ? ' selected' : ''}`}>
      <strong>${launcher.tool_label}${target?.launcher_id === launcher.launcher_id ? ' · Selected' : ''}</strong>
      <ul>${models.map(item => html`<li key=${item.model}><strong>${item.offered.model_label}</strong>${item.why ? ` — ${item.why}` : ''}</li>`)}</ul>
    </div>`)}</aside>`;
}

const originalTarget = agent => cleanTarget(agent);
// A change only counts when it names another machine, harness or model than the agent has; picking "Change" and leaving the choice alone is not one.
export const isModelChange = (agent, target) => !!agent && !!target && !sameTarget(agent, target);
export const MIGRATION_TERMINAL = ['done', 'cancelled', 'failed', 'lost'];

// Stopped agents stay as they are through a change, so the wizard does not list them one by one: the active ones are listed, the stopped
// ones are counted in a single line.
export const isStopped = agent => agent.state === 'stopped';
export const activeAgents = agents => (agents || []).filter(agent => !isStopped(agent));
export const stoppedCount = agents => (agents || []).filter(isStopped).length;
export const stoppedLine = count => `${count} stopped agent${count === 1 ? '' : 's'}, unchanged`;
export function StoppedLine({ agents }) {
  const count = stoppedCount(agents);
  return count > 0 ? html`<p class="team-wizard-stopped">${stoppedLine(count)}</p>` : null;
}

// A kept agent: what it runs now, and an optional change of harness or model (which becomes a full model switch for that agent).
export function KeptAgent({ agent, options, changed, onChange, onReset, prefix }) {
  return html`<div class="team-wizard-kept">
    <p><strong>${agent.slug || agent.agent_id}</strong> · ${describeTarget(options, agent)} <span class="team-wizard-current">${isModelChange(agent, changed) ? '(will switch)' : '(current)'}</span></p>
    ${changed ? html`<${TargetPicker} label=${`${agent.slug || agent.agent_id} agent`} prefix=${`${prefix}-${agent.agent_id}`} options=${options} value=${changed} onChange=${onChange} withName=${false} />
      <button type="button" class="btn" onClick=${onReset}>Keep current model</button>`
      : html`<button type="button" class="btn" disabled=${!options.length} onClick=${() => onChange(originalTarget(agent))}>Change model or harness</button>`}
  </div>`;
}

const ACTION = {
  kept: 'keeps running with the new rules (not restarted)',
  stopped: 'stopped, unchanged',
  switch: 'switches to the chosen model after its current turn',
  new: 'is added',
  reactivate: 'is started again (it was stopped by an earlier switch and keeps its name)',
  leaving: 'hands off to the leader and is stopped',
};

export function agentAction(status, changed, state) { return status === 'kept' ? (state === 'stopped' ? 'stopped' : changed ? 'switch' : 'kept') : status; }

export const MIGRATION_NOTICE = 'This can take a while: each agent first finishes its current turn before the change applies; you will have to wait until then.';

export function MigrationSummary({ plan, options, changes, newTargets, flowLabel, fromLabel, preview, selectedRemovals, onToggleRemoval }) {
  const roles = preview?.roles || plan?.roles || [];
  return html`<div class="team-setup-summary">
    <p><strong>Team flow:</strong> ${fromLabel} → ${flowLabel}</p>
    ${roles.map(role => html`<div key=${role.role_key} class="team-wizard-summary-role"><strong>${roleLabel(role.role_key)}</strong>
      ${role.status === 'new' && newTargets[role.role_key] ? html`<p>${role.agents?.[0] ? `${role.agents[0].slug || role.agents[0].agent_id} · ` : newTargets[role.role_key].resume_legacy ? `${newTargets[role.role_key].resume_legacy} (continues its v1 session) · ` : newTargets[role.role_key].name ? `${newTargets[role.role_key].name} · ` : ''}${describeTarget(options, newTargets[role.role_key])} ${role.agents?.[0] ? ACTION.reactivate : ACTION.new}</p>`
    : (role.agents || []).length ? html`${(role.status === 'kept' ? activeAgents(role.agents) : role.agents).length > 0 && html`<ul>${(role.status === 'kept' ? activeAgents(role.agents) : role.agents).map(agent => html`<li key=${agent.agent_id}>${agent.slug || agent.agent_id}: ${ACTION[agentAction(role.status, !!changes[agent.agent_id], agent.state)]}${changes[agent.agent_id] ? ` (${describeTarget(options, changes[agent.agent_id])})` : ''}</li>`)}</ul>`}${role.status === 'kept' && html`<${StoppedLine} agents=${role.agents} />`}` : html`<p>${role.status === 'leaving' ? 'No agent.' : 'None yet.'}</p>`}</div>`)}
    ${preview?.created_rooms?.length > 0 && html`<p><strong>Rooms to create:</strong> ${preview.created_rooms.map(room => room.title || room.key || room).join(', ')}</p>`}
    ${preview?.conflicts?.length > 0 && html`<div class="team-flow-change-group"><strong>Agents needing action</strong><ul>${preview.conflicts.map(item => html`<li key=${`${item.agent_id}:${item.room_id}`}><label class="team-flow-removal"><input type="checkbox" checked=${selectedRemovals.some(selected => selected.agent_id === item.agent_id && selected.room_id === item.room_id)} onChange=${event => onToggleRemoval(item, event.currentTarget.checked)} />Remove ${item.slug || item.agent_id} (${item.role_label || item.role_key || 'agent'}) from ${item.room_title || 'this room'} when applying</label></li>`)}</ul></div>`}
    <p class="team-wizard-notice" role="note">${MIGRATION_NOTICE}</p>
  </div>`;
}

const AGENT_STATE = { waiting: 'Waiting', draining: 'Finishing its current turn', running: 'Applying', done: 'Done', failed: 'Failed' };
const AGENT_ACTION = { notice: 'keeps running with the new rules', switch: 'switches model', spawn: 'is added', reactivate: 'is started again', handoff_stop: 'hands off to the leader, then stops' };

// Asks before cutting an agent's turn, then does it. Answers the migration as it stands, or null when the person said no.
export async function interruptAgent(source, projectID, agent) {
  const name = agent.slug || agent.agent_id;
  const message = agent.keeps
    ? `Interrupt ${name} now? Its current turn is cut off; ${name} stops the turn and restarts once the switch is through.`
    : `Interrupt ${name} now? Its current turn is cut off and ${name} stops; the new flow does not keep it.`;
  if (!await confirmDialog(message, { title: 'Interrupt agent', confirm: 'Interrupt', danger: true })) return null;
  const answer = await source.interruptMigration(projectID, agent.agent_id);
  return answer?.migration || answer;
}

// Live per-agent progress of a running migration, with Cancel while the server still allows it. A change that waits for an agent's turn can
// go on in the background (onBackground), and an agent it waits for can be interrupted (onInterrupt).
export function MigrationProgress({ migration, onCancel, cancelling, onBackground, onInterrupt, interrupting }) {
  const state = migration.state, running = !MIGRATION_TERMINAL.includes(state);
  const stalledMinutes = migration.stalled_since && running ? Math.max(1, Math.floor((Date.now() - Date.parse(migration.stalled_since)) / 60000)) : 0;
  const waiting = (migration.agents || []).filter(agent => agent.state === 'draining').map(agent => agent.slug || agent.agent_id);
  return html`<div class="team-wizard-progress" aria-live="polite">
    <p role="status"><strong>${state === 'done' ? 'Team flow changed.' : state === 'cancelled' ? 'Cancelled. Everything stays as it was.' : state === 'failed' ? 'The change failed.' : state === 'lost' ? 'The change could not be completed.' : 'Changing the team flow…'}</strong></p>
    ${migration.error && html`<p class="error" role="alert">${migration.error}</p>`}
    ${stalledMinutes > 0 && html`<p class="team-wizard-banner team-wizard-banner-leaving" role="alert">${waiting.length ? `${waiting.join(', ')} ${waiting.length === 1 ? 'has' : 'have'}` : 'An agent has'} not finished its current turn for ${stalledMinutes} minute${stalledMinutes === 1 ? '' : 's'}. Keep waiting, or cancel to leave everything as it was.</p>`}
    <ul>${(migration.agents || []).map(agent => html`<li key=${agent.agent_id} class=${`team-wizard-progress-${agent.state}`}>${agent.slug || agent.agent_id} (${agent.role_key}) ${AGENT_ACTION[agent.action] || agent.action}: ${AGENT_STATE[agent.state] || agent.state}${agent.error ? ` — ${agent.error}` : ''}${running && agent.blocking && onInterrupt ? html` <${IconButton} name="stop" label=${`Interrupt ${agent.slug || agent.agent_id} now`} className="team-wizard-interrupt" danger disabled=${interrupting} onClick=${() => onInterrupt(agent)} />` : ''}</li>`)}</ul>
    ${running && (migration.can_cancel || onBackground) && html`<div class="team-wizard-progress-actions">
      ${onBackground && html`<${IconButton} name="minimize" label="Continue in background" className="team-wizard-background" onClick=${onBackground} />`}
      ${migration.can_cancel && html`<button type="button" class="btn" disabled=${cancelling} onClick=${onCancel}>${cancelling ? 'Cancelling…' : 'Cancel'}</button>`}
    </div>`}
  </div>`;
}
