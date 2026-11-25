// Import-free: scripts/test-machines-model.cjs runs this module in a vm.

// The line a machine card shows about the agents it keeps warm: how many have a live process, and what the machine's processes use. The memory is not
// per agent (the harness container is shared by all of them and Docker reports no finer): the supervisor's own warm processes, and, labelled as such,
// everything running in containers.
export function warmLine(warm) {
  if (!warm) return '';
  const size = bytes => (bytes >= 1 << 30 ? `${(bytes / (1 << 30)).toFixed(1)} GB` : `${Math.round(bytes / (1 << 20))} MB`);
  const parts = [`${warm.idle}${warm.busy ? `, ${warm.busy} working` : ''}`];
  if (warm.rss_bytes > 0) parts.push(`${size(warm.rss_bytes)} in warm processes`);
  if (warm.container_wide_bytes > 0) parts.push(`${size(warm.container_wide_bytes)} container memory, all containers`);
  if (warm.unconfirmed_ping_ends > 0) parts.push(`${warm.unconfirmed_ping_ends} unconfirmed ping ends`);
  return `Warm agents: ${parts.join(' · ')}`;
}

// How long ago, as the rest of the app says it ("5m ago"; import-free, so it is not time.js's relativeTime).
export function ago(iso, now = Date.now()) {
  const seconds = Math.floor((now - new Date(iso).getTime()) / 1000);
  if (!(seconds >= 5)) return 'just now';
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  return `${Math.floor(hours / 24)}d ago`;
}

export function cardView(machine, owner) {
  const open = !machine.revoked_at;
  return {
    title: machine.name + (machine.is_host ? ' · Host' : ''),
    status: machine.revoked_at ? 'Revoked' : !machine.approved ? 'Pending approval' : machine.upgrade_required ? 'Restart server to finish upgrade' : machine.restart_pending ? 'Supervisor restart pending' : machine.connected ? 'Connected' : 'Offline',
    detail: [machine.supervisor_version && `Supervisor ${machine.supervisor_version}`, machine.last_seen && `Last seen ${ago(machine.last_seen)}`, warmLine(machine.warm)].filter(Boolean).join(' · '),
    approve: !machine.approved && open,
    revoke: !machine.is_host && open,
    stop: machine.approved && open,
    restart: machine.is_host && machine.restart_pending && !machine.upgrade_required && owner,
    locked: !owner,
  };
}

// A journal ID is long; the card shows its first part, the dialog and the tooltip say which.
const shortJournal = id => (id.length > 12 ? `${id.slice(0, 12)}…` : id);
const plural = (n, word) => `${n} ${word}${n === 1 ? '' : 's'}`;

// One supervisor instance of a remote machine, as its row shows it. Only an instance that is neither connected nor gone can be written off.
export function instanceView(instance, now = Date.now()) {
  const gone = instance.state === 'gone';
  const status = gone ? 'Gone' : instance.connected ? (instance.state === 'retiring' ? 'Connected, retiring' : 'Connected') : `Not connected, last seen ${ago(instance.last_seen_at, now)}`;
  const owns = gone ? '' : instance.turns + instance.commands > 0 ? `owns ${plural(instance.turns, 'turn')} and ${plural(instance.commands, 'command')}` : 'owns no work';
  return {
    title: `Instance ${shortJournal(instance.journal_id)}`,
    detail: [instance.build && `Build ${instance.build}`, status, owns].filter(Boolean).join(' · '),
    canWriteOff: !gone && !instance.connected,
  };
}

// What the confirmation says before an instance is written off: what is lost, whose it is, and that it cannot be undone.
export function writeOffWarning(machine, instance) {
  const agents = instance.agents.map(agent => agent.slug).join(', ');
  const owned = instance.turns + instance.commands > 0
    ? `It owns ${plural(instance.turns, 'open turn')} and ${plural(instance.commands, 'running command')}${agents ? `, from the agents ${agents}` : ''}. The turns are lost and each of those agents is held for recovery; the commands end and will never report a result.`
    : 'It owns no open turns or commands.';
  return `Mark instance ${instance.journal_id} of ${machine.name} as gone? Do this only when its journal is lost for good (a replaced disk or a reinstall). ${owned} This is irreversible.`;
}
