// Import-free: scripts/test-machines-model.cjs runs this module in a vm.

// The line a machine card shows about the agents it keeps warm: how many have a live process, and what the machine's processes use. The memory is not
// per agent (the harness container is shared by all of them and Docker reports no finer): the supervisor's own warm processes, and, labelled as such,
// everything running in containers.
export function warmLine(warm) {
  if (!warm) return '';
  const size = bytes => (bytes >= 1 << 30 ? `${(bytes / (1 << 30)).toFixed(1)} GB` : `${Math.round(bytes / (1 << 20))} MB`);
  const parts = [`${warm.idle} warm${warm.busy ? `, ${warm.busy} working` : ''}`];
  if (warm.rss_bytes > 0) parts.push(`${size(warm.rss_bytes)} in warm processes`);
  if (warm.container_wide_bytes > 0) parts.push(`${size(warm.container_wide_bytes)} container memory, all containers`);
  if (warm.unconfirmed_ping_ends > 0) parts.push(`${warm.unconfirmed_ping_ends} unconfirmed ping ends`);
  return `Warm agents: ${parts.join(' · ')}`;
}

export function cardView(machine, owner) {
  const open = !machine.revoked_at;
  return {
    title: machine.name + (machine.is_host ? ' · Host' : ''),
    status: machine.revoked_at ? 'Revoked' : !machine.approved ? 'Pending approval' : machine.connected ? 'Connected' : 'Offline',
    detail: [machine.supervisor_version && `Supervisor ${machine.supervisor_version}`, machine.last_seen && `Last seen ${new Date(machine.last_seen).toLocaleString()}`, warmLine(machine.warm)].filter(Boolean).join(' · '),
    approve: !machine.approved && open,
    revoke: !machine.is_host && open,
    stop: machine.approved && open,
    locked: !owner,
  };
}
