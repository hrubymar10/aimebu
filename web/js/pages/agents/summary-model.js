// The stuck-or-dead summary over an agent's logs: its state in a word, how long since it last did something, how long its turn has been running,
// what its last process left behind and its health flags. Everything comes from the agent's card and the events the page already gets; nothing
// is asked of the server. Import-free, so scripts/test-agent-summary-model.cjs runs it in a vm.

// The flags the summary names, in the order it shows them.
export const SUMMARY_FLAGS = ['stuck', 'recovery_waiting', 'held_answer', 'out_of_quota', 'machine_offline'];
const FLAG_WORDS = { stuck: 'Stuck: no progress for a while', recovery_waiting: 'Waiting for a recovery decision', held_answer: 'An answer is held', out_of_quota: 'Out of quota', machine_offline: 'Machine offline' };

// A span of time as people read it: "45s", "3m 12s", "41m", "1h 30m", "2d 4h".
export function span(ms) {
  const seconds = Math.max(0, Math.floor(Number(ms) / 1000) || 0);
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 10) return `${minutes}m ${seconds % 60}s`;
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return minutes % 60 ? `${hours}h ${minutes % 60}m` : `${hours}h`;
  const days = Math.floor(hours / 24);
  return hours % 24 ? `${days}d ${hours % 24}h` : `${days}d`;
}

const time = value => { const at = Date.parse(value || ''); return Number.isFinite(at) ? at : 0; };
const clock = (at, format) => format ? format(at) : new Date(at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });

// The state chip. The order decides when several hold: an offline machine explains everything else, a stopped agent is stopped, one that needs a
// decision needs it before anything else, then working, sleeping, warm and idle.
export function chipOf(agent, { formatClock } = {}) {
  const flags = new Set((agent?.health_flags || []).map(flag => flag.code));
  if (flags.has('machine_offline')) return { kind: 'offline', label: 'Machine offline' };
  if (agent?.desired === 'stopped' || agent?.observed === 'stopped') return { kind: 'stopped', label: 'Stopped' };
  if (agent?.recovery_block || agent?.observed === 'lost' || flags.has('recovery_waiting')) return { kind: 'recovery', label: 'Needs recovery' };
  if (agent?.observed === 'in_turn' || agent?.observed === 'interrupting') return { kind: 'working', label: 'Working' };
  if (agent?.warm?.state === 'sleeping') return { kind: 'sleeping', label: 'Sleeping' };
  if (agent?.warm?.state === 'warm') {
    const until = time(agent.warm.sleeps_at);
    return { kind: 'idle', label: until ? `Idle, warm until ${clock(until, formatClock)}` : 'Idle, warm' };
  }
  return { kind: 'idle', label: 'Idle' };
}

// The whole summary at `now` (ms). `last_activity_at` is the server's; `activity_at` the newest event this page saw; the later one counts.
// `turn_started_at` is known only for a turn that started while the page was open, so the turn line says nothing it cannot know.
export function summaryOf(agent, now, options = {}) {
  const chip = chipOf(agent, options);
  const last = Math.max(time(agent?.last_activity_at), time(agent?.activity_at));
  const working = chip.kind === 'working' || (agent?.observed === 'in_turn');
  const started = working ? time(agent?.turn_started_at) : 0;
  const evidence = String(agent?.last_exit_evidence || '').trim();
  const flags = (agent?.health_flags || []).filter(flag => SUMMARY_FLAGS.includes(flag.code))
    .sort((a, b) => SUMMARY_FLAGS.indexOf(a.code) - SUMMARY_FLAGS.indexOf(b.code))
    .map(flag => ({ code: flag.code, label: flag.code === 'out_of_quota' && time(flag.reset_at) ? `Out of quota until ${clock(time(flag.reset_at), options.formatClock)}` : FLAG_WORDS[flag.code] }));
  return {
    chip,
    activity: last ? `Last activity ${span(now - last)} ago` : 'No activity recorded',
    turn: started ? `Turn running for ${span(now - started)}` : working ? 'In a turn' : '',
    // The last event's words, only when the page has them (a turn queued, replied, a tool): the tool's name needs the later activity log.
    event: String(agent?.activity || '').trim(),
    exit: evidence ? evidence.split('\n')[0].slice(0, 200) : '',
    exitFull: evidence,
    flags,
  };
}
