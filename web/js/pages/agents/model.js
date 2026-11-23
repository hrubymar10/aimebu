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
  const since = kind === 'stopped' ? '' : (agent.observed === 'in_turn' ? agent.activity_at || agent.last_activity_at : agent.last_activity_at) || '';
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

// The class of the state badge for a state kind: v1's where v1 has the state (idle, thinking for a working agent, error, stopped), v2's own for the
// ones it does not (paused, attention).
export const stateBadge = kind => ({ idle: 'idle', working: 'thinking', error: 'error', stopped: 'stopped' })[kind] || kind;
// The colour class of the dot before the name (v1's .agent-online-dot): green while it is up, amber when it waits or is paused or needs a decision, red
// (v2's own `dead`) when it is stopped, lost or offline, muted for anything else.
export const stateDot = kind => ({ idle: 'active', working: 'active', paused: 'stale', attention: 'stale', error: 'dead', stopped: 'dead', offline: 'dead' })[kind] || 'offline';

// What a card says about warm mode, for an idle agent: "Warm · sleeps in N min" while its process is being kept up for the next message, "Sleeping"
// when warm mode is on for it and there is no live process (the next message starts it, a little slower). Nothing when warm mode does not apply.
// `now` is a parameter so the text is a function of the data and the time.
export function warmOf(agent, now = Date.now()) {
  const warm = agent.warm;
  if (!warm || warm.state === 'unknown' || agent.observed !== 'idle') return null;
  if (warm.state === 'na') return { text: 'Warm n/a', title: "This harness doesn't keep its process warm." };
  if (warm.state === 'warm') {
    const left = Date.parse(warm.sleeps_at) - now;
    if (!Number.isFinite(left)) return { text: 'Warm', title: 'Its process is kept running for the next message.' };
    const minutes = Math.max(1, Math.ceil(left / 60000));
    return { text: `Warm · sleeps in ${minutes} min`, title: 'Its process is kept running for the next message, so it answers faster. It goes to sleep when this ends.' };
  }
  return { text: 'Sleeping', title: 'Its process is stopped; the next message starts it, a few seconds slower than a warm agent.' };
}

// The one status dot of a card: it says whether the agent is online and, for an idle one, whether its process is warm (and is the only place the card says so).
// `kind` is what the dot looks like: `cold` (green), `warm` (green with a ring that pulses), `working` (green when no warm process is reported), `unknown`
// (orange: a paused or waiting agent, which says so in its own words, or a Claude agent whose warm state is not known) or `offline` (red: stopped, lost or gone). `title` is the sentence for the tooltip and for
// a screen reader. `state` is stateOf(agent); `now` is a parameter so the text is a function of the data and the time.
export function dotOf(agent, state, now = Date.now()) {
  const detail = state.title ? `\n${state.title}` : '';
  const dot = stateDot(state.kind);
  if (dot === 'dead') return { kind: 'offline', title: `Offline${detail}` };
  // Paused or waiting for a decision is a known state, with its own words, and the dot is orange all the same; only a kind this does not know is "State unknown".
  if (dot === 'stale') return { kind: 'unknown', title: state.title || state.name };
  if (dot === 'offline') return { kind: 'unknown', title: `State unknown${detail}` };
  if (state.kind === 'working') return agent.warm?.state === 'working'
    ? { kind: 'warm', title: `Online \u00b7 warm \u00b7 working${detail}` }
    : { kind: 'working', title: `Online \u00b7 working${detail}` };
  if (agent.warm?.state === 'na') return { kind: 'na', title: "Online \u00b7 warm n/a: this harness doesn't keep its process warm" };
  if (agent.warm?.state === 'unknown') return { kind: 'unknown', title: 'State unknown \u00b7 whether its process is kept warm is not known (an older supervisor)' };
  if (agent.warm?.state === 'warm') {
    const left = Date.parse(agent.warm.sleeps_at) - now, minutes = Number.isFinite(left) ? Math.max(1, Math.ceil(left / 60000)) : 0;
    return { kind: 'warm', title: minutes ? `Online \u00b7 warm, the next answer skips the start (\u2248${minutes} min left)` : 'Online \u00b7 warm, the next answer skips the start' };
  }
  const reason = agent.warm?.state === 'sleeping' && agent.warm.reason ? `; last warm attempt: ${agent.warm.reason}` : '';
  return { kind: 'cold', title: `Online \u00b7 cold, the next answer starts the agent${reason}` };
}

// What a card says about the prompt cache keep-alive: how often it is pinged, what a stretch of pings costs (measured from the last ping when there is
// one), and a warning when the last ping missed the cache or failed. Null when keep-alive does not apply to the agent.
export function keepAliveOf(agent) {
  const keep = agent.keep_alive;
  if (!keep) return null;
  const dollars = value => (value < 0.01 ? '<$0.01' : `$${value.toFixed(value < 1 ? 2 : 1)}`);
  const cost = keep.estimate_usd != null ? `, up to ${keep.pings_per_stretch} pings after its last turn ≈ ${dollars(keep.estimate_usd)}` : '';
  // Only while pings are still planned: after the stretch ends the agent sleeps and the warning would read as spending that goes on.
  const when = Date.parse(keep.last_ping_at || keep.last_at), time = Number.isFinite(when) ? new Date(when).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false }) : '';
  const at = time ? ` at ${time}` : '';
  let warning = '';
  if (keep.stretch_active === true) {
    if (keep.last_ok === false) warning = `The last ping${at} failed${keep.last_error ? `: ${keep.last_error}` : ''}.`;
    else if (keep.missed) warning = `The last ping${at} did not find the cache (it had expired, or the ping differed from a real turn): it paid to write it again.`;
  }
  return { text: `Cache kept warm every ${keep.interval_minutes} min${cost}`, warning };
}

export function activityOf(agent, state) {
  const woken = agent.woken_by === 'reply' ? 'Woken by reply · ' : '';
  // An idle agent that has done nothing has nothing to say here: the pill already says idle and for how long.
  const said = agent.activity || (state.observed === 'Working' ? 'Working…' : '');
  const warm = state.why ? null : warmOf(agent);
  const text = state.why || (state.kind === 'idle' && !woken && (said === 'Sleeping' || !said) ? '' : said ? woken + said : woken ? 'Woken by reply' : warm ? warm.text : '');
  const last = agent.last_activity_at || agent.activity_at, seen = agent.machine_last_seen;
  return { text, last, seen };
}

export function switchStatusOf(agent, harness) {
  const handoff = agent.switch;
  const status = handoff?.handoff_status;
  const phase = handoff?.state === 'failed' ? 'Switch failed' : handoff?.state === 'started' ? 'New session started' : handoff?.state === 'starting' ? 'Starting new session' : handoff?.state === 'stopping' ? 'Stopping old session' : status === 'waiting' ? 'Waiting for current turn' : status === 'pending' || status === 'handing_off' ? 'Handing off' : status === 'ready' ? 'Handoff ready · switching' : status === 'automatic' ? 'Automatic summary · switching' : handoff?.state === 'draining' ? 'Finishing current turn' : '';
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

// Which of the card's quick (hover) buttons make sense for the agent's state, besides the private message: an active or paused one has Pause and Stop; one that is
// being stopped (stop asked, the process not gone yet, or the harness interrupting) keeps a Stop that is already in effect, since it is not startable yet; a stopped one
// is started again, so Start takes the place of both (Pause means nothing on it and Stop is already done); a paused one is resumed, so Resume takes the place of its Pause.
// Start and Resume are the one action `reactivate`, named for the state it acts on.
export function quickActionsOf(agent) {
  if (agent.desired === 'stopped' && agent.observed === 'stopped') return ['start'];
  if (agent.desired === 'stopped' || agent.observed === 'interrupting') return ['stop'];
  if (agent.desired === 'not_accepting') return ['resume', 'stop'];
  if ((agent.health_flags || []).some(flag => flag.code === 'out_of_quota')) return ['start', 'pause', 'stop'];
  return ['pause', 'stop'];
}

// Every action says what it does; a disabled one says why. `down` is the reason when the controls are unavailable altogether.
export function actionsOf(agent, { down = '', lifecycle = 'open' } = {}) {
  const quotaFlag = (agent.health_flags || []).some(flag => flag.code === 'out_of_quota');
  const again = agent.desired === 'not_accepting' ? 'Resume' : 'Start'; // one action, `reactivate`, called by what it does to the state it is in
  const handoff = agent.switch;
  const reasons = {
    message: ['Send this agent a private message', ''],
    'act-pause': ['Pause: finish the current turn, then accept no new work', down || (agent.desired === 'not_accepting' ? 'it is already paused' : agent.desired === 'stopped' ? 'it is stopped; start it first' : '')],
    'act-stop': ['Stop: interrupt the current turn and accept no new work', down || (agent.desired === 'stopped' ? 'it is already stopped' : '')],
    switch: ['Switch: change this agent\'s tool, model or machine', down || (lifecycle && lifecycle !== 'open' ? 'reopen the room first' : agent.recovery_block ? 'resolve the recovery item first' : agent.desired === 'stopped' ? 'start it first' : agent.boundary_pending ? 'it is waiting for folder or machine approval' : handoff && handoff.state !== 'failed' && handoff.state !== 'started' ? 'a switch is already in progress' : '')],
    reactivate: [`${again}: ${agent.desired === 'not_accepting' ? 'accept new work again' : 'run this agent again and accept new work'}`, down || (agent.boundary_pending ? 'it is waiting for folder or machine approval' : agent.role_deleted ? 'its role was deleted' : agent.desired === 'active' && !quotaFlag ? 'it is already active' : '')],
    'start-fresh': ['Start a new session with recent room history', down || (agent.desired === 'stopped' && agent.observed === 'stopped' ? '' : 'stop this agent first')],
    'clear-context': ['Clear context: start a fresh session after the current turn finishes', down || (lifecycle && lifecycle !== 'open' ? 'reopen the room first' : agent.recovery_block ? 'resolve recovery first' : agent.desired !== 'active' ? 'start this agent first' : agent.switch && agent.switch.state !== 'failed' && agent.switch.state !== 'started' ? 'a switch is in progress' : '')],
    remove: ['Remove: take this agent out for good; its messages stay, and it cannot be started again', down || (agent.desired === 'stopped' && agent.observed === 'stopped' ? '' : 'stop it first')],
  };
  if (agent.desired === 'stopped' && agent.observed === 'stopped') reasons['start-fresh'][1] = reasons.reactivate[1];
  const labels = { message: 'Message', 'act-pause': 'Pause', 'act-stop': 'Stop', reactivate: again, 'start-fresh': 'Start fresh', 'clear-context': 'Clear context', switch: 'Switch', remove: 'Remove' };
  return Object.entries(reasons).map(([name, [what, why]]) => ({ name, label: labels[name], title: why ? `${labels[name]} is unavailable: ${why}` : what, disabled: !!why, hint: why ? `${labels[name]} is unavailable: ${why}.` : '' }));
}

// The rows of an open profile: what the card knows, then what the profile read added. Each is [label, value, { mono, copy }].
export function profileRows(agent, profile, { harness, relative }) {
  const data = profile || {};
  const variant = agent.variant && agent.variant !== agent.tool ? `${harness(agent.tool)}/${agent.variant}` : harness(agent.tool);
  const today = data.turns_today || data.cost_today ? `${data.turns_today || 0} turn${data.turns_today === 1 ? '' : 's'}${data.cost_today ? ` · $${data.cost_today.toFixed(data.cost_today < 0.01 ? 4 : 2)}${data.cost_today_estimated ? ' estimated' : ''}` : ''}` : '';
  // No Status, Presence or Last seen rows: the card's pill is the one status line (its state and how long it has been in it), and the line under it says what
  // the agent is doing when it is doing something.
  const rows = [
    ['Runtime', `${agent.model || 'unknown'} · ${variant || 'unknown'}`],
    ['Session', data.native_session_id, { mono: true }], ['Resume', data.resume_command, { mono: true, copy: true }],
    ['CWD', data.cwd, { mono: true }], ['Branch', data.branch, { mono: true }],
    ['Machine', data.machine_name], ['Launcher', data.launcher_name],
    ['Project', data.project_name], ['Repo', data.repo_url, { mono: true }],
    ['Spawned', data.spawned_by ? `${data.spawned_by}${data.spawned_at ? ` · ${relative(data.spawned_at)}` : ''}` : ''],
    data.resumed_from ? ['Continues', `the v1 session of ${data.resumed_from}`] : null,
    ['Last 24 h', today], ['Agent ID', agent.id, { mono: true, copy: true }],
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
