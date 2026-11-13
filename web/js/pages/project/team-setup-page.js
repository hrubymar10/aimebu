import { html, useEffect, useRef, useState } from '../../ui.js';
import { request } from '../../api.js';
import { uuid } from '../../uuid.js';
import { ModelPicker } from '../agents/model-picker.js';
import { SpawnDefaults } from '../../components/spawn-defaults.js';
import { TeamFlowCards, TeamFlowPreview } from './team-flow-page.js';

const projectPath = id => `/api/projects/${encodeURIComponent(id)}`;
const chosen = (items, old) => items.find(item => item.value === old) || items[0];
const targetOf = (machine, tool, model) => ({ machine_id: machine.id, tool: tool.tool, variant: tool.variant, launcher_id: tool.launcher_id, model: model.id });
const cleanTarget = item => item && ({ machine_id: item.machine_id, tool: item.tool, variant: item.variant, launcher_id: item.launcher_id, model: item.model });
const sameTarget = (a, b) => a?.machine_id === b?.machine_id && a?.launcher_id === b?.launcher_id && a?.model === b?.model;

async function loadChoices(projectID) {
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
      default_model: !!model.default,
    }))));
  return { machines, options, defaults: Array.isArray(defaults) ? defaults : [] };
}

function initialTarget(options, defaults, role) {
  const agent = defaults.find(item => item.role_key === role)?.default_agents?.[0];
  return cleanTarget(options.find(item => item.launcher_id === agent?.launcher_id && item.model === agent?.model) || options.find(item => item.default_model) || options[0]);
}
function initialDefaults(options, defaults, role) {
  const row = defaults.find(item => item.role_key === role);
  const available = (row?.default_agents || []).filter(item => options.some(option => option.launcher_id === item.launcher_id && option.model === item.model));
  const first = initialTarget(options, defaults, role);
  return { items: available.length ? available : first ? [{ launcher_id: first.launcher_id, model: first.model }] : [], mode: row?.pick_mode || 'in_turn' };
}
function TargetPicker({ label, prefix, options, value, onChange }) {
  const machines = [...new Map(options.map(option => [option.machine_id, { value: option.machine_id, label: option.machine_name }])).values()];
  const machine = chosen(machines, value?.machine_id);
  const tools = [...new Map(options.filter(option => option.machine_id === machine?.value).map(option => [option.launcher_id, { value: option.launcher_id, label: option.tool_label }])).values()];
  const tool = chosen(tools, value?.launcher_id);
  const models = options.filter(option => option.machine_id === machine?.value && option.launcher_id === tool?.value);
  const change = next => { if (next) onChange({ machine_id: next.machine_id, tool: next.tool, variant: next.variant, launcher_id: next.launcher_id, model: next.model }); };
  return html`<fieldset class="team-setup-target"><legend>${label}</legend>
    <label>Machine<select aria-label=${`${label} machine`} value=${machine?.value || ''} onChange=${event => change(options.find(option => option.machine_id === event.currentTarget.value && option.default_model) || options.find(option => option.machine_id === event.currentTarget.value))}>
      ${machines.map(item => html`<option key=${item.value} value=${item.value}>${item.label}</option>`)}</select></label>
    <label>Tool<select aria-label=${`${label} tool`} value=${tool?.value || ''} onChange=${event => change(options.find(option => option.machine_id === machine?.value && option.launcher_id === event.currentTarget.value && option.default_model) || options.find(option => option.machine_id === machine?.value && option.launcher_id === event.currentTarget.value))}>
      ${tools.map(item => html`<option key=${item.value} value=${item.value}>${item.label}</option>`)}</select></label>
    <${ModelPicker} prefix=${prefix} models=${models.map(item => ({ value: item.model, label: item.model_label }))} value=${value?.model || ''} onChange=${model => change(models.find(item => item.model === model))} />
  </fieldset>`;
}

function Summary({ body, preview, options }) {
  const name = target => options.find(item => sameTarget(item, target));
  const describe = target => { const item = name(target); return item ? `${item.machine_name} · ${item.tool_label} · ${item.model_label}` : 'No available agent'; };
  return html`<div class="team-setup-summary">
    <p><strong>Team flow:</strong> ${body.flow_key === 'leader-dispatcher' ? 'B · Leader and dispatcher' : 'A · Leader and workers'}</p>
    <p><strong>Leader:</strong> ${describe(body.leader)}</p>
    ${body.dispatcher && html`<p><strong>Dispatcher:</strong> ${describe(body.dispatcher)}</p>`}
    <p><strong>Worker defaults:</strong> ${body.role_defaults.worker.default_agents.length} choice(s)</p>
    <p><strong>Reviewer defaults:</strong> ${body.role_defaults.reviewer.default_agents.length} choice(s)</p>
    <p><strong>Project:</strong> ${body.settings.worktree_mode === 'auto' ? 'Separate task worktrees' : 'Shared folder'} · Memory ${body.settings.memory_enabled ? 'on' : 'off'} · Up to ${body.settings.max_live_agents} live agents</p>
    ${preview?.created_rooms?.length > 0 && html`<p><strong>Rooms to create:</strong> ${preview.created_rooms.map(room => room.title || room.key || room).join(', ')}</p>`}
    ${preview?.conflicts?.length > 0 && html`<p class="error" role="alert">${preview.conflicts.map(item => item.reason || item).join('; ')}</p>`}
  </div>`;
}

export function TeamSetupPage({ flow, canEditProject, projectForID }) {
  const state = flow.state.value, templates = flow.templates.value, selected = flow.selected.value;
  const canEdit = canEditProject(flow.projectID), project = projectForID(flow.projectID);
  const [step, setStep] = useState(1), [choices, setChoices] = useState(null), [loadError, setLoadError] = useState('');
  const [leader, setLeader] = useState(null), [dispatcher, setDispatcher] = useState(null);
  const [worker, setWorker] = useState(null), [reviewer, setReviewer] = useState(null);
  const [settings, setSettings] = useState({ worktree_mode: 'auto', memory_enabled: false, max_live_agents: 10 });
  const [preview, setPreview] = useState(null), [busy, setBusy] = useState(false), [error, setError] = useState('');
  const commitKey = useRef(''), generation = useRef(0), settingsTouched = useRef(false);
  useEffect(() => {
    const id = flow.projectID, turn = ++generation.current;
    setStep(1); setChoices(null); setLeader(null); setDispatcher(null); setWorker(null); setReviewer(null);
    setSettings({ worktree_mode: 'auto', memory_enabled: false, max_live_agents: 10 });
    setLoadError(''); setPreview(null); setBusy(false); setError(''); commitKey.current = ''; settingsTouched.current = false;
    if (!id) return undefined;
    loadChoices(id).then(result => {
      if (turn !== generation.current) return;
      setChoices(result);
      setLeader(initialTarget(result.options, result.defaults, 'leader'));
      setDispatcher(initialTarget(result.options, result.defaults, 'dispatcher'));
      setWorker(initialDefaults(result.options, result.defaults, 'worker'));
      setReviewer(initialDefaults(result.options, result.defaults, 'reviewer'));
      const key = flow.selected.peek(), current = flow.state.peek();
      if (key !== 'custom' && current && result.options.length) {
        request(`${projectPath(id)}/team-setup/preview`, { method: 'POST', body: {
          flow_key: key, flow_rev: current.rev,
          leader: initialTarget(result.options, result.defaults, 'leader'),
          ...(key === 'leader-dispatcher' ? { dispatcher: initialTarget(result.options, result.defaults, 'dispatcher') } : {}),
          role_defaults: {
            worker: { default_agents: initialDefaults(result.options, result.defaults, 'worker').items, pick_mode: initialDefaults(result.options, result.defaults, 'worker').mode },
            reviewer: { default_agents: initialDefaults(result.options, result.defaults, 'reviewer').items, pick_mode: initialDefaults(result.options, result.defaults, 'reviewer').mode },
          },
        } }).then(answer => { if (turn === generation.current && !settingsTouched.current && answer.settings) setSettings(answer.settings); }, () => {});
      }
    }, failure => { if (turn === generation.current) setLoadError(failure.message); });
    return () => { generation.current++; };
  }, [flow.projectID]);
  if (!state && flow.loading.value) return html`<section class="team-flow-home-card" role="status">Loading team flows…</section>`;
  if (!state && flow.error.value) return html`<section class="team-flow-home-card"><p class="error" role="alert">Could not load team flows: ${flow.error.value}</p><button type="button" class="btn" onClick=${() => flow.load(flow.projectID)}>Retry</button></section>`;
  if (!state || !state.review_pending && state.key) return null;
  const migrated = state.key === 'custom' && state.review_pending;
  const steps = selected === 'leader-dispatcher' ? ['Team flow', 'Leader', 'Dispatcher', 'Workers and reviewers', 'Project settings', 'Summary'] : ['Team flow', 'Leader', 'Workers and reviewers', 'Project settings', 'Summary'];
  const minimumAgents = selected === 'leader-dispatcher' ? 2 : 1;
  const stepLabel = steps[step - 1], isLast = step === steps.length;
  const body = { flow_key: selected, flow_rev: state.rev, leader, ...(selected === 'leader-dispatcher' ? { dispatcher } : {}), role_defaults: {
    worker: { default_agents: worker?.items || [], pick_mode: worker?.mode || 'in_turn' },
    reviewer: { default_agents: reviewer?.items || [], pick_mode: reviewer?.mode || 'in_turn' },
  }, settings };
  const go = async next => {
    const projectID = flow.projectID, turn = generation.current;
    setError(''); setPreview(null); commitKey.current = '';
    if (next === steps.length) {
      setBusy(true);
      try { const answer = await request(`${projectPath(projectID)}/team-setup/preview`, { method: 'POST', body }); if (turn === generation.current && flow.projectID === projectID) { setPreview(answer); setStep(next); } }
      catch (failure) { if (turn === generation.current && flow.projectID === projectID) setError(failure.message); }
      finally { if (turn === generation.current && flow.projectID === projectID) setBusy(false); }
    } else setStep(next);
  };
  const create = async () => {
    if (!preview?.can_create || busy) return;
    const projectID = flow.projectID, turn = generation.current;
    if (!commitKey.current) commitKey.current = uuid();
    setError(''); setBusy(true);
    try {
      const result = await request(`${projectPath(projectID)}/team-setup`, { method: 'POST', headers: { 'Idempotency-Key': commitKey.current }, body });
      if (turn === generation.current && flow.projectID === projectID) await flow.completeTeamSetup(projectID, result);
    } catch (failure) { if (turn === generation.current && flow.projectID === projectID) setError(failure.message); }
    finally { if (turn === generation.current && flow.projectID === projectID) setBusy(false); }
  };
  const targetStep = stepLabel === 'Leader' || stepLabel === 'Dispatcher';
  return html`<section class="team-flow-home-card team-setup-wizard" aria-labelledby="team-flow-home-title">
    <div class="team-flow-heading"><small>Project setup${step > 1 ? ` · Step ${step} of ${steps.length}` : ''}</small><h2 id="team-flow-home-title">${step === 1 ? 'Choose how your AI team works' : stepLabel}</h2>
      ${step === 1 && html`<p>${migrated ? 'Your existing setup is kept. Choose another flow, or keep the current custom setup.' : 'Pick a starting team flow. You can change it later in Project settings.'}</p>`}</div>
    ${step === 1 && html`<${TeamFlowCards} templates=${templates} selected=${selected} onSelect=${key => flow.select(key)} disabled=${flow.applying.value || !canEdit} />
      <${TeamFlowPreview} preview=${flow.preview.value} busy=${flow.previewing.value} />`}
    ${step > 1 && targetStep && html`${!choices && html`<p role="status">Loading machines and tools…</p>`}${loadError && html`<p class="error" role="alert">${loadError}</p>`}
      ${choices && !choices.options.length && html`<p class="error" role="alert">No available machine, tool, and model combination is ready for this project.</p>`}
      ${choices?.options.length > 0 && html`<${TargetPicker} label=${stepLabel} prefix=${`team-setup-${stepLabel.toLowerCase()}`} options=${choices.options} value=${stepLabel === 'Leader' ? leader : dispatcher} onChange=${stepLabel === 'Leader' ? setLeader : setDispatcher} />`}`}
    ${stepLabel === 'Workers and reviewers' && html`${choices && html`<p>These defaults are saved for future spawns. No workers or reviewers start now.</p><div class="team-setup-defaults"><div><h3>Workers</h3><${SpawnDefaults} items=${worker?.items || []} mode=${worker?.mode || 'in_turn'} options=${choices.options.map(item => ({ launcher_id: item.launcher_id, model: item.model, label: `${item.machine_name} · ${item.tool_label} · ${item.model_label}` }))} onChange=${setWorker} /></div><div><h3>Reviewers</h3><${SpawnDefaults} items=${reviewer?.items || []} mode=${reviewer?.mode || 'in_turn'} options=${choices.options.map(item => ({ launcher_id: item.launcher_id, model: item.model, label: `${item.machine_name} · ${item.tool_label} · ${item.model_label}` }))} onChange=${setReviewer} /></div></div>`}`}
    ${stepLabel === 'Project settings' && html`<div class="team-setup-settings">
      <label>Project folder<input value=${project?.folder || ''} readonly aria-label="Project folder" /></label>
      <label><input type="checkbox" checked=${settings.worktree_mode === 'auto'} onChange=${event => { settingsTouched.current = true; setSettings({ ...settings, worktree_mode: event.currentTarget.checked ? 'auto' : 'off' }); }} /> Separate Git worktree for each task room</label>
      <label><input type="checkbox" checked=${settings.memory_enabled} onChange=${event => { settingsTouched.current = true; setSettings({ ...settings, memory_enabled: event.currentTarget.checked }); }} /> Enable project memory</label>
      <label>Maximum live agents<input type="number" min=${minimumAgents} max="100" value=${settings.max_live_agents} onInput=${event => { settingsTouched.current = true; setSettings({ ...settings, max_live_agents: Number(event.currentTarget.value) }); }} /></label>
    </div>`}
    ${stepLabel === 'Summary' && html`<${Summary} body=${body} preview=${preview} options=${choices?.options || []} />`}
    <div class="team-flow-actions">
      ${step > 1 && html`<button type="button" class="btn" disabled=${busy} onClick=${() => go(step - 1)}>Back</button>`}
      ${step === 1 && selected === 'custom' && html`<button type="button" class="btn btn-primary" disabled=${!canEdit || flow.applying.value || flow.previewing.value || !flow.preview.value} onClick=${() => flow.apply()}>${flow.applying.value ? 'Saving…' : 'Keep current'}</button>`}
      ${step === 1 && selected !== 'custom' && html`<button type="button" class="btn btn-primary" disabled=${!canEdit || !selected || !flow.preview.value || flow.previewing.value || !choices?.options.length} onClick=${() => go(2)}>Next</button>`}
      ${step > 1 && !isLast && html`<button type="button" class="btn btn-primary" disabled=${busy || targetStep && !(stepLabel === 'Leader' ? leader : dispatcher) || stepLabel === 'Project settings' && (!Number.isInteger(settings.max_live_agents) || settings.max_live_agents < minimumAgents || settings.max_live_agents > 100)} onClick=${() => go(step + 1)}>Next</button>`}
      ${isLast && html`<button type="button" class="btn btn-primary" disabled=${busy || !preview?.can_create} onClick=${create}>${busy ? 'Creating…' : 'Create team'}</button>`}
      ${!canEdit && html`<span>Ask a project owner or operator to choose a flow.</span>`}
    </div>
    <p class="error" role="alert">${error || flow.error.value}</p>
  </section>`;
}
