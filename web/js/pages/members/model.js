// The words the Members section uses for an agent: the same ones the Agents panel shows for its control and observed state.
export function agentState(agent) {
  if (agent.revoked) return 'Revoked';
  const interrupted = agent.observed === 'interrupting' || (agent.desired === 'stopped' && agent.observed !== 'stopped');
  const control = interrupted ? 'Interrupt requested' : agent.desired === 'stopped' ? 'Stopped' : agent.desired === 'not_accepting' ? 'Not accepting' : 'Active';
  const observed = agent.observed === 'in_turn' ? 'Working' : agent.observed === 'lost' ? 'Lost' : agent.observed === 'stopped' ? 'Stopped' : agent.observed === 'interrupting' ? 'Interrupting' : 'Idle';
  // Said once when both say the same (a stopped agent that is stopped), not as "Stopped · Stopped".
  return control === observed ? control : `${control} · ${observed}`;
}

// The server calls a singleton role vacant once its agent is stopped (a new one may be started in its place), but the list above names that agent with the
// role, so "vacant" beside it reads as a contradiction: a stopped agent of the role in this room is named as the holder, marked stopped.
export function stoppedHolder(role, agents) {
  const agent = (agents || []).find(item => item.role_key === role.role_key && !item.revoked);
  return agent ? { slug: agent.slug, stopped: true } : null;
}

// Who holds a singleton role, as the list says it.
export function holderText(holder) {
  if (!holder) return 'vacant';
  return `held by ${holder.slug}${holder.stopped ? ' (stopped)' : holder.in_this_room ? '' : ' (another room)'}`;
}
