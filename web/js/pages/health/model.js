// Import-free: scripts/test-health-model.cjs runs this module in a vm.

// Plain words for the short pill on an agent card (the internal state names stay in the API and the inspector).
const labels = {
  stuck: 'Not responding',
  out_of_quota: 'Out of quota',
  machine_offline: 'Machine offline',
  held_answer: 'Reply not delivered',
  recovery_waiting: 'Needs your decision',
};

export function healthExplanation(flag) {
  switch (flag.code) {
    case 'stuck': return 'A turn has run with no sign of activity for a while. Open the agent to stop it or wait.';
    case 'out_of_quota': return 'The account hit its usage limit, so the agent cannot work until it resets or you switch accounts.';
    case 'machine_offline': return 'The computer this agent runs on is not connected to the server. Check that its supervisor is running.';
    case 'held_answer': return 'The agent wrote a reply that could not be posted. Open the Inbox to dismiss it or see the room.';
    case 'recovery_waiting': return 'A turn ended unexpectedly. Choose in the Inbox whether to resume, retry or abandon it.';
    default: return 'Open the agent to see more.';
  }
}

export function healthLabel(flag) {
  const reset = new Date(flag.reset_at);
  if (flag.code === 'out_of_quota' && flag.reset_at && !Number.isNaN(reset.getTime())) return `Out of quota until ${reset.toLocaleString()}`;
  return labels[flag.code] || flag.code;
}

export function healthSentence(name, flag) {
  switch (flag.code) {
    case 'stuck': return `${name} looks stuck`;
    case 'out_of_quota': {
      const reset = new Date(flag.reset_at);
      return flag.reset_at && !Number.isNaN(reset.getTime()) ? `${name} is out of quota until ${reset.toLocaleString()}` : `${name} is out of quota`;
    }
    case 'machine_offline': return `${name}'s machine is offline`;
    case 'held_answer': return `An answer to ${name} could not be delivered`;
    case 'recovery_waiting': return `${name} needs a recovery decision`;
    default: return `${name}: ${healthLabel(flag)}`;
  }
}

export function formatBytes(bytes) {
  const value = Number(bytes) || 0;
  if (value < 1024) return `${value} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let size = value / 1024, unit = 0;
  while (size >= 1024 && unit < units.length - 1) { size /= 1024; unit++; }
  return `${size >= 100 ? size.toFixed(0) : size >= 10 ? size.toFixed(1) : size.toFixed(2)} ${units[unit]}`;
}

export const STATE_LABELS = { active: 'In use', dirty: 'Uncommitted work', merged: 'Merged', orphan: 'Room closed, unmerged', idle: 'Idle', ready: 'Ready', unknown: 'Not checked' };
export const ACTION_LABELS = { would_remove: 'Will remove', removed: 'Removed', kept: 'Kept' };

const plural = (count, word) => `${count} ${word}${count === 1 ? '' : 's'}`;
export const removedLine = (items, bytes) => `Removed ${plural(items.filter(item => item.action === 'removed').length, 'item')}, ${formatBytes(bytes)}.`;

export function previewSummary(items, bytes) {
  const removable = items.filter(item => item.action === 'would_remove');
  return removable.length ? `${plural(removable.length, 'item')}, ${formatBytes(bytes)} would be removed. Nothing has been removed yet.` : 'Nothing to remove right now.';
}

export function badgeView(total) {
  return { text: String(total), hidden: !total, issues: !!total, title: total ? `${total} need attention` : 'All agents healthy', label: total ? `Health, ${total} need attention` : 'Health' };
}

export const retentionDays = value => { const days = Math.round(Number(value)); return Number.isFinite(days) && days >= 0 && days <= 3650 ? days : null; };
