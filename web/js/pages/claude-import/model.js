// Import-free: scripts/test-claude-session-import.cjs runs this module in a vm.

// Sessions in the same folder can all start as workers. The person may change
// each row's role before import.
export function presetRoles(sessions, roles) {
  const role = roles.includes('worker') ? 'worker' : roles[0] || '';
  return sessions.map(() => role);
}

// Only a native claude harness, or a Docker one that shares the host's Claude home, can continue (or copy) a session; any other container
// keeps a Claude home of its own.
export const usableTool = tool => tool.tool === 'claude' && tool.available && (!tool.containerized || tool.shares_claude_home);

export const defaultModel = tool => (tool?.models || []).find(model => model.default)?.id ?? tool?.models?.[0]?.id ?? '';

export function machineLabel(machine) {
  return `${machine.name}${machine.is_host ? ' · Host' : ''}${machine.connected ? '' : ' · Offline'}`;
}

export const finalStatus = (started, total, stopped) =>
  `${started} of ${total} ${total === 1 ? 'session' : 'sessions'} started${stopped ? '; the project\'s agent or spawn-rate limit was reached, import the rest later' : ''}.`;
