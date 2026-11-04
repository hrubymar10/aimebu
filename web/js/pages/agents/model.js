// What the agent cards and the profile say, as plain functions of an agent, so the page only places the words.

// A refresh keeps the record it already holds for an agent whose data is unchanged, so only a card that changed renders again.
export function reconcile(previous, list) {
  let same = previous.size === list.length;
  const next = new Map(), order = [...previous.keys()];
  let position = 0;
  for (const agent of list) {
    const held = previous.get(agent.id);
    const keep = held && JSON.stringify(held) === JSON.stringify(agent);
    next.set(agent.id, keep ? held : agent);
    if (!keep || order[position] !== agent.id) same = false;
    position++;
  }
  return same ? previous : next;
}

export function stateOf(agent) {
  const interrupted = agent.observed === 'interrupting' || (agent.desired === 'stopped' && agent.observed !== 'stopped');
  const observed = agent.observed === 'in_turn' ? 'Working' : agent.observed === 'lost' ? 'Lost' : agent.observed === 'stopped' ? 'Stopped' : agent.observed === 'interrupting' ? 'Interrupting' : 'Idle';
  const name = agent.boundary_pending ? 'awaiting approval' : interrupted ? 'stopping' : agent.observed === 'in_turn' ? 'working' : agent.observed === 'lost' ? 'lost' : agent.desired === 'stopped' ? 'stopped' : agent.desired === 'not_accepting' ? 'paused' : 'idle';
  const kind = agent.boundary_pending ? 'paused' : interrupted || agent.observed === 'in_turn' ? 'working' : agent.observed === 'lost' ? 'error' : agent.desired === 'stopped' ? 'stopped' : agent.desired === 'not_accepting' ? 'paused' : 'idle';
  let title = agent.boundary_pending ? 'Waiting for project owner approval in Inbox.'
    : agent.desired === 'stopped' ? 'Stopped; no new work is accepted.'
    : agent.desired === 'not_accepting' ? 'Paused; no new work is accepted.'
    : agent.observed === 'in_turn' ? 'Working on a turn.'
    : agent.observed === 'lost' ? 'The previous process is unconfirmed; recovery is needed.'
    : 'Idle; waiting for new work.';
  if (agent.recovery_block && agent.last_exit_evidence) title += `\n${agent.last_exit_evidence}`;
  // A working agent counts from the start of its turn, any other state from the last thing it did.
  const since = (agent.observed === 'in_turn' ? agent.activity_at || agent.last_activity_at : agent.last_activity_at) || '';
  // An idle agent that accepts work but will not start is not "idle": its last turn needs the person's decision, or the limit guard is
  // holding new turns of its tool. The dot goes orange and the state says which, and `why` is the sentence the card shows beside it.
  const free = agent.desired === 'active' && agent.observed === 'idle' && !agent.boundary_pending;
  if (free && agent.recovery_block) {
    const why = 'Needs your decision: its last turn did not finish cleanly, so nothing new starts until you decide what to do in Inbox.';
    return { name: 'needs decision', kind: 'attention', title: agent.last_exit_evidence ? `${why}\n${agent.last_exit_evidence}` : why, since, observed, why };
  }
  if (free && agent.hold) return { name: `held: ${agent.hold.text}`, kind: 'attention', title: agent.hold.detail, since, observed, why: agent.hold.detail };
  return { name, kind, title, since, observed };
}

export function activityOf(agent, state) {
  const woken = agent.woken_by === 'reply' ? 'Woken by reply · ' : '';
  const text = state.why || woken + (agent.activity || (state.observed === 'Working' ? 'Working…' : 'No recent activity'));
  const last = agent.last_activity_at || agent.activity_at, seen = agent.machine_last_seen;
  return { text, last, seen };
}

export function switchStatusOf(agent, harness) {
  const handoff = agent.switch;
  const phase = { draining: 'Finishing current turn', stopping: 'Stopping old session', starting: 'Starting new session', failed: 'Switch failed' }[handoff?.state];
  if (agent.boundary_pending) return { hidden: false, text: 'Folder or machine approval needed · Open Inbox', inbox: true };
  if (phase) return { hidden: false, text: `${phase}: ${harness(handoff.tool)} · ${handoff.model}${handoff.state === 'failed' && handoff.error ? ` — ${handoff.error}` : ''}`, inbox: false };
  return { hidden: true, text: '', inbox: false };
}

export function grantsOf(agent) {
  const grants = agent.boundary_grants || [];
  return { hidden: !grants.length, text: grants.map(grant => `Approved: ${grant.machine_id} · ${grant.path}`).join(' · ') };
}

// The Memory bar: the share of the context window in use, and the title that spells out the numbers and the cost so far.
export function contextOf(usage) {
  const used = usage?.context_used, window = usage?.context_window;
  const usedKnown = Number.isFinite(used) && used >= 0, windowKnown = Number.isFinite(window) && window > 0, known = usedKnown && windowKnown;
  const number = amount => (Number.isFinite(amount) && amount >= 0 ? new Intl.NumberFormat().format(amount) : '—');
  const cost = Number.isFinite(usage?.cost_so_far) && usage.cost_so_far >= 0 ? `$${usage.cost_so_far.toFixed(usage.cost_so_far > 0 && usage.cost_so_far < 0.01 ? 4 : 2)}` : '—';
  const title = `Context ${usedKnown ? number(used) : '—'} / ${windowKnown ? number(window) : '—'} tokens · Input ${number(usage?.tokens_in)} · Output ${number(usage?.tokens_out)} · Cost so far ${cost}${usage?.cost_estimated ? ' (includes estimates)' : ''}`;
  return { known, width: known ? `${Math.min(100, used / window * 100)}%` : '0%', value: known ? `${Math.round(Math.min(100, used / window * 100))}%` : '—', title };
}

export function liveOf(agent) {
  return { at: agent.observed === 'in_turn' ? (agent.activity_at || agent.last_activity_at || '') : '', label: /^tool\b/i.test(agent.activity || '') ? 'tool' : 'working' };
}

// Every action says what it does; a disabled one says why. `down` is the reason when the controls are unavailable altogether.
export function actionsOf(agent, { down = '', lifecycle = 'open' } = {}) {
  const quotaFlag = (agent.health_flags || []).some(flag => flag.code === 'out_of_quota');
  const handoff = agent.switch;
  const reasons = {
    message: ['Send this agent a private message', ''],
    'act-pause': ['Pause: finish the current turn, then accept no new work', down || (agent.desired === 'not_accepting' ? 'it is already paused' : agent.desired === 'stopped' ? 'it is stopped; reactivate it first' : '')],
    'act-stop': ['Stop: interrupt the current turn and accept no new work', down || (agent.desired === 'stopped' ? 'it is already stopped' : '')],
    switch: ['Switch: change this agent\'s tool, model or machine', down || (lifecycle && lifecycle !== 'open' ? 'reopen the room first' : agent.recovery_block ? 'resolve the recovery item first' : agent.desired === 'stopped' ? 'reactivate it first' : agent.boundary_pending ? 'it is waiting for folder or machine approval' : handoff && handoff.state !== 'failed' ? 'a switch is already in progress' : '')],
    reactivate: ['Reactivate: accept new work again', down || (agent.boundary_pending ? 'it is waiting for folder or machine approval' : agent.role_deleted ? 'its role was deleted' : agent.desired === 'active' && !quotaFlag ? 'it is already active' : '')],
  };
  const labels = { message: 'Message', 'act-pause': 'Pause', 'act-stop': 'Stop', reactivate: 'Reactivate', switch: 'Switch' };
  return Object.entries(reasons).map(([name, [what, why]]) => ({ name, label: labels[name], title: why ? `${labels[name]} is unavailable: ${why}` : what, disabled: !!why, hint: why ? `${labels[name]} is unavailable: ${why}.` : '' }));
}

// The rows of an open profile: what the card knows, then what the profile read added. Each is [label, value, { mono, copy }].
export function profileRows(agent, profile, { harness, relative }) {
  const data = profile || {};
  const status = agent.desired === 'stopped' ? 'Stopped' : agent.desired === 'not_accepting' ? 'Paused' : agent.observed === 'in_turn' ? 'Working' : agent.observed === 'lost' ? 'Lost' : agent.observed === 'stopped' ? 'Stopped' : 'Online';
  const seen = agent.last_activity_at || agent.machine_last_seen;
  const variant = agent.variant && agent.variant !== agent.tool ? `${harness(agent.tool)}/${agent.variant}` : harness(agent.tool);
  const today = data.turns_today || data.cost_today ? `${data.turns_today || 0} turn${data.turns_today === 1 ? '' : 's'}${data.cost_today ? ` · $${data.cost_today.toFixed(data.cost_today < 0.01 ? 4 : 2)}${data.cost_today_estimated ? ' estimated' : ''}` : ''}` : '';
  const rows = [
    ['Status', status], ['Presence', agent.activity || (agent.observed === 'in_turn' ? 'Working on a turn' : agent.observed === 'lost' ? 'Unconfirmed; recovery needed' : 'Idle')],
    ['Last seen', seen ? relative(seen) : ''], ['Runtime', `${agent.model || 'unknown'} · ${variant || 'unknown'}`],
    ['Role', `${agent.role_emoji ? agent.role_emoji + ' ' : ''}${agent.role_key || agent.role || ''}`.trim()],
    ['Session', data.native_session_id, { mono: true }], ['Resume', data.resume_command, { mono: true, copy: true }],
    ['CWD', data.cwd, { mono: true }], ['Branch', data.branch, { mono: true }],
    ['Machine', data.machine_name], ['Launcher', data.launcher_name],
    ['Project', data.project_name], ['Repo', data.repo_url, { mono: true }],
    ['Spawned', data.spawned_by ? `${data.spawned_by}${data.spawned_at ? ` · ${relative(data.spawned_at)}` : ''}` : ''],
    data.resumed_from ? ['Continues', `the v1 session of ${data.resumed_from}`] : null,
    ['Last 24 h', today], ['Agent ID', agent.id, { mono: true }],
  ];
  return rows.filter(row => row && row[1] !== undefined && row[1] !== null && row[1] !== '');
}

export function outputStatus(view) {
  if (!view.text) return 'No output has been captured for this agent yet.';
  return view.truncated ? 'Earlier output is not shown.' : '';
}

export function outputFailure(error) {
  return error.code === 'no_output' ? 'No output has been captured for this agent yet.' : (error.message || 'The output could not be read.');
}
