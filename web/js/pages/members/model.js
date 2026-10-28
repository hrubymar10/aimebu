// The words the Members section uses for an agent: the same ones the Agents panel shows for its control and observed state.
export function agentState(agent) {
  if (agent.revoked) return 'Revoked';
  const interrupted = agent.observed === 'interrupting' || (agent.desired === 'stopped' && agent.observed !== 'stopped');
  const control = interrupted ? 'Interrupt requested' : agent.desired === 'stopped' ? 'Stopped' : agent.desired === 'not_accepting' ? 'Not accepting' : 'Active';
  const observed = agent.observed === 'in_turn' ? 'Working' : agent.observed === 'lost' ? 'Lost' : agent.observed === 'stopped' ? 'Stopped' : agent.observed === 'interrupting' ? 'Interrupting' : 'Idle';
  return `${control} · ${observed}`;
}

// Who holds a singleton role, as the list says it.
export function holderText(holder) {
  return holder ? `held by ${holder.slug}${holder.in_this_room ? '' : ' (another room)'}` : 'vacant';
}
