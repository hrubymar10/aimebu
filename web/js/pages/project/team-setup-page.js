import { html, useEffect, useRef, useState } from '../../ui.js';
import { request } from '../../api.js';
import { uuid } from '../../uuid.js';
import { SpawnDefaults } from '../../components/spawn-defaults.js';
import { TeamFlowCards, TeamFlowPreview } from './team-flow-page.js';
import {
  projectPath, cleanTarget, roleLabel, flowLetter, loadChoices, initialTarget, initialDefaults, describeTarget,
  TargetPicker, StatusBanner, RecommendedModels, KeptAgent, MigrationSummary, MigrationProgress, isModelChange, MIGRATION_TERMINAL,
} from './team-wizard-parts.js';

const SECOND_ROLE = { 'leader-dispatcher': 'Dispatcher', 'leader-advisor': 'Advisor' };
const CORE = ['leader', 'worker', 'reviewer'];
const TERMINAL = MIGRATION_TERMINAL;

function Summary({ body, flowLabel, preview, options, selectedRemovals, onToggleRemoval }) {
  const describe = target => `${target?.name ? `${target.name} · ` : ''}${describeTarget(options, target)}`;
  return html`<div class="team-setup-summary">
    <p><strong>Team flow:</strong> ${flowLabel}</p>
    <p><strong>Leader:</strong> ${describe(body.leader)}</p>
    ${body.dispatcher && html`<p><strong>Dispatcher:</strong> ${describe(body.dispatcher)}</p>`}
    ${body.advisor && html`<p><strong>Advisor:</strong> ${describe(body.advisor)}</p>`}
    <p><strong>Worker defaults:</strong> ${body.role_defaults.worker.default_agents.length} choice(s)</p>
    <p><strong>Reviewer defaults:</strong> ${body.role_defaults.reviewer.default_agents.length} choice(s)</p>
    <p><strong>Project:</strong> ${body.settings.worktree_mode === 'auto' ? 'Separate task worktrees' : 'Shared folder'} · Memory ${body.settings.memory_enabled ? 'on' : 'off'} · Up to ${body.settings.max_live_agents} workers and reviewers</p>
    ${preview?.created_rooms?.length > 0 && html`<p><strong>Rooms to create:</strong> ${preview.created_rooms.map(room => room.title || room.key || room).join(', ')}</p>`}
    ${preview?.conflicts?.length > 0 && html`<div class="team-flow-change-group"><strong>Agents needing action</strong><ul>${preview.conflicts.map(item => html`<li key=${`${item.agent_id}:${item.room_id}`}><label class="team-flow-removal"><input type="checkbox" checked=${selectedRemovals.some(selected => selected.agent_id === item.agent_id && selected.room_id === item.room_id)} onChange=${event => onToggleRemoval(item, event.currentTarget.checked)} />Remove ${item.slug || item.agent_id} (${item.role_label || item.role_key || 'agent'}) from ${item.room_title || 'this room'} when applying</label></li>`)}</ul></div>`}
  </div>`;
}

// The home room shows the wizard until a flow is chosen (or a migrated project keeps its custom setup).
export function TeamSetupPage({ flow, canEditProject, projectForID, onRetry }) {
  const state = flow.state.value;
  if (!state && flow.loading.value) return html`<section class="team-flow-home-card" role="status">Loading team flows…</section>`;
  if (!state && flow.error.value) return html`<section class="team-flow-home-card"><p class="error" role="alert">Could not load team flows: ${flow.error.value}</p><button type="button" class="btn" onClick=${onRetry}>Retry</button></section>`;
  if (!state || !state.review_pending && state.key) return null;
  return html`<${TeamWizard} flow=${flow} canEdit=${canEditProject(flow.projectID)} project=${projectForID(flow.projectID)} />`;
}

// One wizard for both jobs. Setup (a project with no flow yet) starts a fresh team; a switch (the project already has a flow) keeps
// the leader, adds or hands off the other core roles, and asks the server for a plan before it changes anything.
export function TeamWizard({ flow, canEdit, project, embedded = false, onClose = () => {} }) {
  const state = flow.state.value, templates = flow.templates.value, selected = flow.selected.value;
  // The store is emptied while another project loads and this component may render once more before its parent removes it, so state can be null.
  const setupMode = !state?.key;
  const [step, setStep] = useState(1), [choices, setChoices] = useState(null), [loadError, setLoadError] = useState('');
  const [leader, setLeader] = useState(null), [dispatcher, setDispatcher] = useState(null), [advisor, setAdvisor] = useState(null);
  const [worker, setWorker] = useState(null), [reviewer, setReviewer] = useState(null);
  // null: not asked for yet; '' is a name the person cleared (the server then picks one).
  const [names, setNames] = useState({}), [nameBusy, setNameBusy] = useState({}), [nameErrors, setNameErrors] = useState({}), nameRequests = useRef({});
  const [settings, setSettings] = useState({ worktree_mode: 'auto', memory_enabled: false, max_live_agents: 10 });
  const [preview, setPreview] = useState(null), [busy, setBusy] = useState(false), [error, setError] = useState('');
  const [plan, setPlan] = useState(null), [changes, setChanges] = useState({}), [recommended, setRecommended] = useState([]);
  const [migration, setMigration] = useState(null), [cancelling, setCancelling] = useState(false);
  const commitKey = useRef(''), generation = useRef(0), settingsTouched = useRef(false);
  useEffect(() => {
    const id = flow.projectID, turn = ++generation.current;
    setStep(1); setChoices(null); setLeader(null); setDispatcher(null); setAdvisor(null); setNames({}); setNameBusy({}); setNameErrors({}); nameRequests.current = {}; setWorker(null); setReviewer(null);
    setSettings({ worktree_mode: 'auto', memory_enabled: false, max_live_agents: 10 });
    setLoadError(''); setPreview(null); setBusy(false); setError(''); commitKey.current = ''; settingsTouched.current = false;
    setPlan(null); setChanges({}); setMigration(null); setCancelling(false);
    if (!id) return undefined;
    Promise.resolve(flow.source.recommendedModels?.()).then(answer => { if (turn === generation.current) setRecommended(Array.isArray(answer?.rows) ? answer.rows : []); }, () => {});
    // A change already under way keeps its progress on screen, whoever started it.
    if (state?.key && canEdit) Promise.resolve(flow.source.migration?.(id)).then(answer => {
      const running = answer?.migration || answer;
      if (turn === generation.current && running?.id && !TERMINAL.includes(running.state)) setMigration(running);
    }, () => {});
    loadChoices(id).then(result => {
      if (turn !== generation.current) return;
      setChoices(result);
      setLeader(initialTarget(result.options, result.defaults, 'leader'));
      setDispatcher(initialTarget(result.options, result.defaults, 'dispatcher'));
      setAdvisor(initialTarget(result.options, result.defaults, 'advisor'));
      setWorker(initialDefaults(result.options, result.defaults, 'worker'));
      setReviewer(initialDefaults(result.options, result.defaults, 'reviewer'));
      const key = flow.selected.peek(), current = flow.state.peek();
      // The store may have been reset (another project opened) while the choices loaded, which leaves no flow state to preview against.
      if (current && !current.key && key !== 'custom' && result.options.length) {
        request(`${projectPath(id)}/team-setup/preview`, { method: 'POST', body: {
          flow_key: key, flow_rev: current.rev,
          leader: initialTarget(result.options, result.defaults, 'leader'),
          ...(key === 'leader-dispatcher' ? { dispatcher: initialTarget(result.options, result.defaults, 'dispatcher') } : {}),
          ...(key === 'leader-advisor' ? { advisor: initialTarget(result.options, result.defaults, 'advisor') } : {}),
          role_defaults: {
            worker: { default_agents: initialDefaults(result.options, result.defaults, 'worker').items, pick_mode: initialDefaults(result.options, result.defaults, 'worker').mode },
            reviewer: { default_agents: initialDefaults(result.options, result.defaults, 'reviewer').items, pick_mode: initialDefaults(result.options, result.defaults, 'reviewer').mode },
          },
        } }).then(answer => { if (turn === generation.current && !settingsTouched.current && answer.settings) setSettings(answer.settings); }, () => {});
      }
    }, failure => { if (turn === generation.current) setLoadError(failure.message); });
    return () => { generation.current++; };
  }, [flow.projectID]);
  // Following a running change: ask for its progress until it ends; a finished change reloads the flow, a cancelled or failed one stays on screen.
  useEffect(() => {
    if (!migration?.id || TERMINAL.includes(migration.state)) return undefined;
    const id = flow.projectID, turn = generation.current;
    const timer = setInterval(async () => {
      try {
        const answer = await flow.source.migration(id), latest = answer?.migration || answer;
        if (turn !== generation.current || !latest?.id) return;
        setMigration(latest);
      } catch { /* the next poll tries again */ }
    }, 2000);
    return () => clearInterval(timer);
  }, [migration?.id, migration?.state]);
  const roleEntries = plan?.roles || [];
  const entryOf = role => roleEntries.find(item => item.role_key === role);
  const secondRoles = roleEntries.filter(item => !CORE.includes(item.role_key));
  const second = SECOND_ROLE[selected];
  const steps = setupMode
    ? (second ? ['Team flow', 'Leader', second, 'Workers and reviewers', 'Project settings', 'Summary'] : ['Team flow', 'Leader', 'Workers and reviewers', 'Project settings', 'Summary'])
    : selected === 'custom' || !plan ? ['Team flow'] : ['Team flow', 'Leader', ...secondRoles.map(item => roleLabel(item.role_key)), 'Workers and reviewers', 'Summary'];
  const stepLabel = steps[step - 1], isLast = step === steps.length && steps.length > 1;
  const roleKey = { Leader: 'leader', Dispatcher: 'dispatcher', Advisor: 'advisor' }[stepLabel] || '';
  const roleStatus = setupMode ? 'new' : entryOf(roleKey)?.status || '';
  const needsTarget = !!roleKey && roleStatus === 'new';
  // A stopped agent of a role the new flow needs is started again, with its own name, rather than a new one being created.
  const reuse = roleStatus === 'new' && !setupMode ? entryOf(roleKey)?.agents?.[0] : null;
  // A free name from the server (the same pool as Spawn), asked for when a new agent's step first opens and again by the dice. Nothing is
  // reserved, so a name taken meanwhile is refused when the team is created. A late answer for a replaced request or another project is dropped.
  const suggestName = async (role, exclude) => {
    const id = flow.projectID, roomID = flow.roomID, turn = generation.current, ticket = nameRequests.current[role] = (nameRequests.current[role] || 0) + 1;
    if (!roomID) return;
    setNameBusy(old => ({ ...old, [role]: true }));
    try {
      const answer = await request(`/api/rooms/${encodeURIComponent(roomID)}/agent-name${exclude ? `?exclude=${encodeURIComponent(exclude)}` : ''}`);
      if (turn === generation.current && flow.projectID === id && ticket === nameRequests.current[role] && answer.name) { setNames(old => ({ ...old, [role]: answer.name })); setNameErrors(old => ({ ...old, [role]: '' })); }
    } catch (failure) { if (turn === generation.current && ticket === nameRequests.current[role]) setNameErrors(old => ({ ...old, [role]: `Could not get a name: ${failure.message}` })); }
    finally { if (turn === generation.current && ticket === nameRequests.current[role]) setNameBusy(old => ({ ...old, [role]: false })); }
  };
  const typeName = (role, value) => { nameRequests.current[role] = (nameRequests.current[role] || 0) + 1; setNames(old => ({ ...old, [role]: value })); setNameErrors(old => ({ ...old, [role]: '' })); setNameBusy(old => ({ ...old, [role]: false })); };
  useEffect(() => { if (needsTarget && !reuse && choices && names[roleKey] == null) void suggestName(roleKey, roleKey === 'leader' ? '' : names.leader || ''); }, [roleKey, needsTarget, choices]);
  if (!state) return null;
  const flowLabel = (key = selected) => { const index = templates.findIndex(item => item.key === key); return index < 0 ? '' : `${flowLetter(templates, key)} · ${templates[index].label}`; };
  const minimumAgents = 1;
  const coreRoles = templates.find(item => item.key === selected)?.core_roles || [];
  const coreNames = coreRoles.map((role, index) => index ? role : role.charAt(0).toUpperCase() + role.slice(1)).join(' and ');
  const coreHint = coreNames ? `${coreNames} ${coreRoles.length === 1 ? 'is' : 'are'} not counted.` : '';
  const named = (target, role) => target && (names[role]?.trim() ? { ...target, name: names[role].trim() } : target);
  const roleTargets = { leader, dispatcher, advisor };
  const defaults = { worker: { default_agents: worker?.items || [], pick_mode: worker?.mode || 'in_turn' }, reviewer: { default_agents: reviewer?.items || [], pick_mode: reviewer?.mode || 'in_turn' } };
  const removals = flow.removals.value.length ? { remove_memberships: flow.removals.value } : {};
  // Only a pick that differs from what the agent runs is a change: an opened but untouched "Change model or harness" must not become a switch the server refuses after the drain.
  const planAgents = roleEntries.flatMap(item => item.agents || []);
  const realChanges = Object.fromEntries(Object.entries(changes).filter(([id, target]) => isModelChange(planAgents.find(agent => agent.agent_id === id), target)));
  const newRoles = ['leader', 'dispatcher', 'advisor'].filter(role => entryOf(role)?.status === 'new');
  const newTargets = Object.fromEntries(newRoles.map(role => [role, entryOf(role)?.agents?.length ? roleTargets[role] : named(roleTargets[role], role)]));
  const body = setupMode
    ? { flow_key: selected, flow_rev: state.rev, leader: named(leader, 'leader'), ...(selected === 'leader-dispatcher' ? { dispatcher: named(dispatcher, 'dispatcher') } : {}), ...(selected === 'leader-advisor' ? { advisor: named(advisor, 'advisor') } : {}), role_defaults: defaults, settings, ...removals }
    : { flow_key: selected, flow_rev: state.rev, ...newTargets, changes: Object.entries(realChanges).map(([agent_id, target]) => ({ agent_id, target })), role_defaults: defaults, ...removals };
  const run = async (work, fail = true) => {
    const projectID = flow.projectID, turn = generation.current;
    setBusy(true);
    try { const done = await work(); if (turn === generation.current && flow.projectID === projectID) return done; }
    catch (failure) { if (fail && turn === generation.current && flow.projectID === projectID) setError(failure.message); }
    finally { if (turn === generation.current && flow.projectID === projectID) setBusy(false); }
    return undefined;
  };
  const go = async next => {
    const projectID = flow.projectID;
    setError(''); setPreview(null); commitKey.current = '';
    if (!setupMode && step === 1 && next === 2) {
      const answer = await run(() => request(`${projectPath(projectID)}/team-flow/migration/preview`, { method: 'POST', body: { flow_key: selected, flow_rev: state.rev } }));
      if (answer) { setPlan(answer); setChanges({}); setStep(2); }
    } else if (next === steps.length) {
      const answer = await run(() => request(`${projectPath(projectID)}/${setupMode ? 'team-setup' : 'team-flow/migration'}/preview`, { method: 'POST', body }));
      if (answer) { setPreview(answer); setStep(next); }
    } else setStep(next);
  };
  const create = async () => {
    if (busy || (setupMode ? !preview?.can_create : !preview?.can_apply)) return;
    const projectID = flow.projectID;
    if (!commitKey.current) commitKey.current = uuid();
    setError('');
    const result = await run(() => request(`${projectPath(projectID)}/${setupMode ? 'team-setup' : 'team-flow/migrate'}`, { method: 'POST', headers: { 'Idempotency-Key': commitKey.current }, body }));
    if (!result) return;
    if (setupMode) { await flow.completeTeamSetup(projectID, result); return; }
    const started = result.migration || result;
    setMigration(started);
  };
  const finishMigration = async () => {
    if (migration?.state !== 'done' || busy) return;
    setBusy(true); setError('');
    try { await flow.completeMigration(flow.projectID, migration); onClose(); }
    catch (failure) { setError(failure.message); }
    finally { setBusy(false); }
  };
  const cancel = async () => {
    const projectID = flow.projectID;
    setCancelling(true); setError('');
    try { const answer = await flow.source.cancelMigration(projectID); setMigration(answer?.migration || answer); }
    catch (failure) { setError(failure.message); }
    finally { setCancelling(false); }
  };
  const finishCustom = async () => { await flow.apply(); if (!flow.error.peek()) onClose(); };
  const options = choices?.options || [];
  const optionsList = options.map(item => ({ launcher_id: item.launcher_id, model: item.model, label: `${item.machine_name} · ${item.tool_label} · ${item.model_label}` }));
  const from = state.key, fromLetter = flowLetter(templates, from), toLetter = flowLetter(templates, selected);
  const keptOf = role => (entryOf(role)?.status === 'kept' || role === 'worker' || role === 'reviewer') ? entryOf(role)?.agents || [] : [];
  // The harness and machine a role's panel is about: the new agent's pick, else the kept agent as it will be (current, or the model change chosen).
  const keptAgent = roleStatus === 'new' ? null : entryOf(roleKey)?.agents?.[0];
  const stepTarget = roleStatus === 'new' ? roleTargets[roleKey] : keptAgent && (changes[keptAgent.agent_id] || cleanTarget(keptAgent));
  const renderKept = (agent, prefix) => html`<${KeptAgent} key=${agent.agent_id} agent=${agent} options=${options} changed=${changes[agent.agent_id]} prefix=${prefix}
    onChange=${target => setChanges(old => ({ ...old, [agent.agent_id]: target }))} onReset=${() => setChanges(old => { const next = { ...old }; delete next[agent.agent_id]; return next; })} />`;
  const switching = !setupMode && migration;
  const heading = setupMode ? 'Project setup' : 'Team flow change';
  const nextDisabled = busy || needsTarget && !roleTargets[roleKey] || stepLabel === 'Project settings' && (!Number.isInteger(settings.max_live_agents) || settings.max_live_agents < minimumAgents || settings.max_live_agents > 100);
  const sameFlow = !setupMode && selected === from;
  return html`<section class=${`team-flow-home-card team-setup-wizard${embedded ? ' team-setup-embedded' : ''}`} aria-labelledby="team-flow-home-title">
    <div class="team-flow-heading"><small>${heading}${step > 1 && !switching ? ` · Step ${step} of ${steps.length}` : ''}</small><h2 id="team-flow-home-title">${switching ? migration.state === 'done' ? 'Team flow changed' : 'Changing your team flow' : step === 1 ? (setupMode ? 'Choose how your AI team works' : 'Choose a team flow') : stepLabel}</h2>
      ${step === 1 && !switching && html`<p>${state.key === 'custom' && state.review_pending ? 'Your existing setup is kept. Choose another flow, or keep the current custom setup.' : setupMode ? 'Pick a starting team flow. You can change it later in Project settings.' : 'Switching keeps the leader and your rooms and messages; other agents are kept, added, or hand off and stop.'}</p>`}</div>
    ${switching ? html`<${MigrationProgress} migration=${migration} onCancel=${cancel} cancelling=${cancelling} />${migration.state === 'done' ? html`<div class="team-flow-actions"><button type="button" class="btn btn-primary" disabled=${busy} onClick=${finishMigration}>${busy ? 'Closing…' : 'Done'}</button></div>` : TERMINAL.includes(migration.state) && html`<div class="team-flow-actions"><button type="button" class="btn" onClick=${() => { setMigration(null); setStep(1); setPlan(null); }}>Back to the wizard</button></div>`}` : html`
    ${step === 1 && html`<${TeamFlowCards} templates=${templates} selected=${selected} onSelect=${key => flow.select(key)} disabled=${flow.applying.value || !canEdit} />
      <${TeamFlowPreview} preview=${flow.preview.value} busy=${flow.previewing.value} selectedRemovals=${flow.removals.value} onToggleRemoval=${(item, checked) => flow.toggleRemoval(item, checked)} />`}
    ${step > 1 && roleKey && html`${!setupMode && roleStatus && html`<${StatusBanner} status=${roleStatus} role=${roleKey} from=${fromLetter} to=${toLetter} reactivate=${!!reuse} />`}
      ${!choices && html`<p role="status">Loading machines and tools…</p>`}${loadError && html`<p class="error" role="alert">${loadError}</p>`}
      ${roleStatus === 'new' && html`${choices && !options.length && html`<p class="error" role="alert">No available machine, tool, and model combination is ready for this project.</p>`}
        ${roleKey === 'advisor' && html`<p class="team-setup-hint">Pick a stronger model than the leader's.</p>`}
        ${reuse && html`<p class="team-wizard-current">${reuse.slug || reuse.agent_id} keeps its name; pick the model it should run now.</p>`}
        ${options.length > 0 && html`<${TargetPicker} label=${stepLabel} prefix=${`team-setup-${roleKey}`} options=${options} value=${roleTargets[roleKey]} withName=${!reuse} onChange=${{ leader: setLeader, dispatcher: setDispatcher, advisor: setAdvisor }[roleKey]}
          name=${names[roleKey]} nameBusy=${!!nameBusy[roleKey]} nameError=${nameErrors[roleKey] || ''} onName=${value => typeName(roleKey, value)} onDice=${() => suggestName(roleKey, names[roleKey] || '')} />`}`}
      ${roleStatus === 'kept' && html`<div class="team-wizard-kept-list">${(entryOf(roleKey)?.agents || []).map(agent => renderKept(agent, `team-setup-${roleKey}`))}</div>`}
      ${roleStatus === 'leaving' && html`<ul class="team-wizard-leaving">${(entryOf(roleKey)?.agents || []).map(agent => html`<li key=${agent.agent_id}><strong>${agent.slug || agent.agent_id}</strong> · ${describeTarget(options, agent)}</li>`)}</ul>`}
      <${RecommendedModels} rows=${recommended} role=${roleKey} flowKey=${selected} options=${options} target=${stepTarget} />`}
    ${stepLabel === 'Workers and reviewers' && html`${!setupMode && (keptOf('worker').length > 0 || keptOf('reviewer').length > 0) && html`<${StatusBanner} status="kept" role="workers" plural=${true} from=${fromLetter} to=${toLetter} />
        <div class="team-wizard-kept-list">${['worker', 'reviewer'].map(role => keptOf(role).length > 0 && html`<div key=${role}><h3>${roleLabel(role)}s</h3>${keptOf(role).map(agent => renderKept(agent, `team-setup-${role}`))}</div>`)}</div>`}
      ${choices && html`<p>${setupMode ? 'These defaults are saved for future spawns. No workers or reviewers start now.' : 'Defaults for workers and reviewers started from now on.'}</p><div class="team-setup-defaults"><div><h3>${setupMode ? 'Workers' : 'Default for new workers'}</h3><${SpawnDefaults} items=${worker?.items || []} mode=${worker?.mode || 'in_turn'} options=${optionsList} onChange=${setWorker} /></div><div><h3>${setupMode ? 'Reviewers' : 'Default for new reviewers'}</h3><${SpawnDefaults} items=${reviewer?.items || []} mode=${reviewer?.mode || 'in_turn'} options=${optionsList} onChange=${setReviewer} /></div></div>`}
      ${[['worker', worker, 'workers'], ['reviewer', reviewer, 'reviewers']].map(([role, picked, label]) => html`<${RecommendedModels} key=${role} rows=${recommended} role=${role} flowKey=${selected} label=${label} target=${options.find(item => item.launcher_id === picked?.items?.[0]?.launcher_id && item.model === picked.items[0].model)} options=${options} />`)}`}
    ${stepLabel === 'Project settings' && html`<div class="team-setup-settings">
      <label>Project folder<input value=${project?.folder || ''} readonly aria-label="Project folder" /></label>
      <label><input type="checkbox" checked=${settings.worktree_mode === 'auto'} onChange=${event => { settingsTouched.current = true; setSettings({ ...settings, worktree_mode: event.currentTarget.checked ? 'auto' : 'off' }); }} /> Separate Git worktree for each task room</label>
      <label><input type="checkbox" checked=${settings.memory_enabled} onChange=${event => { settingsTouched.current = true; setSettings({ ...settings, memory_enabled: event.currentTarget.checked }); }} /> Enable project memory</label>
      <label>Maximum workers and reviewers<input type="number" min=${minimumAgents} max="100" value=${settings.max_live_agents} onInput=${event => { settingsTouched.current = true; setSettings({ ...settings, max_live_agents: Number(event.currentTarget.value) }); }} />${coreHint && html`<small class="team-setup-limit-hint">${coreHint}</small>`}</label>
    </div>`}
    ${stepLabel === 'Summary' && (setupMode
      ? html`<${Summary} body=${body} flowLabel=${flowLabel()} preview=${preview} options=${options} selectedRemovals=${flow.removals.value} onToggleRemoval=${(item, checked) => { flow.toggleRemoval(item, checked); commitKey.current = ''; }} />`
      : html`<${MigrationSummary} plan=${plan} preview=${preview} options=${options} changes=${realChanges} newTargets=${newTargets} flowLabel=${flowLabel()} fromLabel=${flowLabel(from)} selectedRemovals=${flow.removals.value} onToggleRemoval=${(item, checked) => { flow.toggleRemoval(item, checked); commitKey.current = ''; }} />`)}
    <div class="team-flow-actions">
      ${step > 1 && html`<button type="button" class="btn" disabled=${busy} onClick=${() => go(step - 1)}>Back</button>`}
      ${embedded && step === 1 && html`<button type="button" class="btn" onClick=${onClose}>Close</button>`}
      ${step === 1 && selected === 'custom' && html`<button type="button" class="btn btn-primary" disabled=${!canEdit || flow.applying.value || flow.previewing.value || !flow.preview.value || sameFlow && !state.review_pending} onClick=${embedded || !setupMode ? finishCustom : () => flow.apply()}>${flow.applying.value ? 'Saving…' : setupMode || state.key === 'custom' ? 'Keep current' : 'Use Custom'}</button>`}
      ${step === 1 && selected !== 'custom' && html`<button type="button" class="btn btn-primary" disabled=${busy || !canEdit || !selected || sameFlow || !flow.preview.value || flow.previewing.value || !choices?.options.length} onClick=${() => go(2)}>Next</button>`}
      ${step > 1 && !isLast && html`<button type="button" class="btn btn-primary" disabled=${nextDisabled} onClick=${() => go(step + 1)}>Next</button>`}
      ${isLast && html`<button type="button" class="btn btn-primary" disabled=${busy || !(setupMode ? preview?.can_create : preview?.can_apply)} onClick=${create}>${busy ? (setupMode ? 'Creating…' : 'Starting…') : setupMode ? 'Create team' : 'Apply team flow'}</button>`}
      ${!canEdit && html`<span>Ask a project owner or operator to choose a flow.</span>`}
    </div>`}
    <p class="error" role="alert">${error || flow.error.value}</p>
  </section>`;
}
