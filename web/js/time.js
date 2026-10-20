// Relative timestamps, ported from v1 relativeTime: seconds/minutes/hours/days,
// then a locale date once a message is older than 30 days.
export function relativeTime(isoString) {
  if (!isoString) return 'never';
  const then = new Date(isoString).getTime();
  const diff = Date.now() - then;
  if (diff < 0) return 'just now';
  const seconds = Math.floor(diff / 1000);
  if (seconds < 5) return 'just now';
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  if (days < 30) return `${days}d ago`;
  return new Date(isoString).toLocaleDateString();
}

// How long an agent has been in its state, kept short enough for the card: M:SS under an hour ("0:44", "59:59"), whole
// hours under a day ("3h"), then whole days ("4d"). v1 ran the clock on as H:MM:SS, which at 100+ hours overflowed the card.
export function stateAge(seconds) {
  const total = Math.max(0, Math.floor(seconds));
  if (total < 3600) return `${Math.floor(total / 60)}:${String(total % 60).padStart(2, '0')}`;
  if (total < 86400) return `${Math.floor(total / 3600)}h`;
  return `${Math.floor(total / 86400)}d`;
}

// Fill a <time> element: relative text, full date as tooltip, ISO kept for refresh.
export function stampTime(node, isoString) {
  node.dateTime = isoString;
  node.dataset.relative = '1';
  node.title = new Date(isoString).toLocaleString();
  node.textContent = relativeTime(isoString);
  return node;
}

export function refreshRelativeTimes(root) {
  for (const node of root.querySelectorAll('time[data-relative]')) node.textContent = relativeTime(node.dateTime);
}
