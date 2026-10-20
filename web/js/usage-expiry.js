// The token-expiry icons of v1's usages panel (copied from v1): healthy, expiring soon, expired. Each is a
// currentColor SVG; the expired one is coloured red by its wrapper's CSS class.
const ICONS = {
  ok: '<svg fill="currentColor" width="14" height="14" viewBox="0 0 20 20" aria-hidden="true"><path d="M2.93 17.07A10 10 0 1 1 17.07 2.93 10 10 0 0 1 2.93 17.07zm12.73-1.41A8 8 0 1 0 4.34 4.34a8 8 0 0 0 11.32 11.32zM6.7 9.29L9 11.6l4.3-4.3 1.4 1.42L9 14.4l-3.7-3.7 1.4-1.42z"/></svg>',
  expiring: '<svg fill="currentColor" width="14" height="14" viewBox="0 0 24 24" aria-hidden="true"><path d="M8.827 6.956c2.265.662 5.109-.295 8.172-1.867l.001.079c0 1.3-1.642 2.897-3.248 4.288a3.818 3.818 0 0 0-.752.898v6.726c1.321.372 2.815 2.089 3.827 3.655-.027.088-.043.179-.077.265h-8.5c-.034-.087-.057-.17-.082-.254 1.01-1.57 2.509-3.293 3.833-3.666v-6.729a3.819 3.819 0 0 0-.73-.88 17.898 17.898 0 0 1-2.443-2.515zM17.922 2H20v1h-1.516A5.594 5.594 0 0 1 19 5.319c0 2.15-1.479 4.294-3.545 6.092a1.544 1.544 0 0 0-.62 1.089 1.544 1.544 0 0 0 .62 1.089C17.521 15.387 19 17.53 19 19.68a5.595 5.595 0 0 1-.516 2.32H20v1H5v-1h1.5a5.666 5.666 0 0 1-.5-2.319c0-2.15 1.479-4.294 3.545-6.092a1.544 1.544 0 0 0 .62-1.089 1.544 1.544 0 0 0-.62-1.089C7.479 9.613 6 7.47 6 5.32A5.666 5.666 0 0 1 6.5 3H5V2zm-.545 1H7.624A4.68 4.68 0 0 0 7 5.32c0 1.645 1.137 3.54 3.2 5.336a2.435 2.435 0 0 1 .966 1.844 2.432 2.432 0 0 1-.965 1.843c-2.064 1.797-3.2 3.692-3.2 5.338A4.68 4.68 0 0 0 7.623 22h9.753A4.646 4.646 0 0 0 18 19.68c0-1.645-1.137-3.54-3.2-5.336a2.435 2.435 0 0 1-.966-1.844 2.432 2.432 0 0 1 .965-1.843c2.064-1.797 3.2-3.692 3.2-5.338A4.646 4.646 0 0 0 17.378 3z"/><path fill="none" d="M0 0h24v24H0z"/></svg>',
  expired: '<svg fill="currentColor" width="14" height="14" viewBox="0 0 24 24" aria-hidden="true"><path d="M7.085 2H5v1h1.5A5.666 5.666 0 0 0 6 5.319c0 2.15 1.479 4.294 3.545 6.092a1.544 1.544 0 0 1 .62 1.089 1.544 1.544 0 0 1-.62 1.089C7.479 15.387 6 17.53 6 19.68A5.666 5.666 0 0 0 6.5 22H5v1h15v-1h-1.516A5.595 5.595 0 0 0 19 19.681c0-2.15-1.479-4.294-3.545-6.092a1.544 1.544 0 0 1-.62-1.089 1.544 1.544 0 0 1 .62-1.089C17.521 9.613 19 7.47 19 5.32A5.594 5.594 0 0 0 18.484 3H20V2zm10.292 1A4.646 4.646 0 0 1 18 5.32c0 1.645-1.137 3.54-3.201 5.337a2.432 2.432 0 0 0-.965 1.843 2.435 2.435 0 0 0 .965 1.844c2.064 1.796 3.2 3.691 3.2 5.337A4.646 4.646 0 0 1 17.378 22H7.624A4.68 4.68 0 0 1 7 19.68c0-1.645 1.137-3.54 3.201-5.337a2.432 2.432 0 0 0-.965-1.843 2.435 2.435 0 0 0-.965-1.844C8.137 8.86 7 6.965 7 5.32A4.68 4.68 0 0 1 7.623 3zM8.102 18.925a3.246 3.246 0 0 1 1.35-1.9l2.593-1.722a.823.823 0 0 1 .91 0l2.594 1.722a3.248 3.248 0 0 1 1.35 1.901 3.051 3.051 0 0 1 .1.755A3.645 3.645 0 0 1 16.75 21h-8.5A3.713 3.713 0 0 1 8 19.68a3.05 3.05 0 0 1 .102-.755z"/><path fill="none" d="M0 0h24v24H0z"/></svg>',
};

function span(seconds) {
  const abs = Math.abs(seconds), hours = Math.floor(abs / 3600), minutes = Math.floor((abs % 3600) / 60);
  if (!hours && !minutes) return seconds > 0 ? 'in < 1m' : 'expired < 1m ago';
  const text = `${hours > 0 ? `${hours}h ` : ''}${minutes}m`;
  return seconds > 0 ? `in ${text}` : `expired ${text} ago`;
}

// expiryBadge turns a credential target row ({expires_at, seconds_left, level}) into the icon node and its tooltip,
// or null when the row carries no expiry.
export function expiryBadge(target) {
  if (!target?.expires_at) return null;
  const expires = new Date(target.expires_at);
  if (Number.isNaN(expires.getTime())) return null;
  const level = target.level === 'expired' || target.seconds_left <= 0 ? 'expired' : target.level === 'expiring' ? 'expiring' : 'ok';
  const left = Number.isFinite(target.seconds_left) ? target.seconds_left : Math.round((expires.getTime() - Date.now()) / 1000);
  const node = document.createElement('span');
  node.className = `switcher-expiry-icon${level === 'expired' ? ' switcher-expiry-icon--expired' : ''}`;
  node.dataset.level = level;
  node.title = `Token expires: ${expires.toISOString()} (${span(left)})`;
  node.innerHTML = ICONS[level];
  return node;
}
