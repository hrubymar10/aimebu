// Import-free: scripts/test-machines-model.cjs runs this module in a vm.

export function cardView(machine, owner) {
  const open = !machine.revoked_at;
  return {
    title: machine.name + (machine.is_host ? ' · Host' : ''),
    status: machine.revoked_at ? 'Revoked' : !machine.approved ? 'Pending approval' : machine.connected ? 'Connected' : 'Offline',
    detail: [machine.supervisor_version && `Supervisor ${machine.supervisor_version}`, machine.last_seen && `Last seen ${new Date(machine.last_seen).toLocaleString()}`].filter(Boolean).join(' · '),
    approve: !machine.approved && open,
    revoke: !machine.is_host && open,
    stop: machine.approved && open,
    locked: !owner,
  };
}
