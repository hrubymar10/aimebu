// Import-free: scripts/test-claude-session-import.cjs runs this module in a vm.

// The first session of a folder works there (a folder has one writer), the others review, so the first import does not run into that rule.
export function presetRoles(sessions, roles) {
  const writer = roles.includes('worker') ? 'worker' : roles[0] || '';
  const other = roles.includes('reviewer') ? 'reviewer' : writer;
  const seen = new Set();
  return sessions.map(session => {
    const folder = session.cwd || '';
    if (seen.has(folder)) return other;
    seen.add(folder);
    return writer;
  });
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
