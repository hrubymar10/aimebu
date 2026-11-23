// The installed app's icon badge: the number of open Inbox items, where the browser has the Badging API (an installed PWA). Anywhere else
// this does nothing. A count of 0, a signed-out person (a null count) and an unsupported API all clear the badge instead of setting it.
export function updateAppBadge(count, nav = globalThis.navigator) {
  if (!nav || typeof nav.setAppBadge !== 'function') return Promise.resolve(false);
  const n = Math.max(0, Math.floor(Number(count) || 0));
  try {
    const pending = n > 0 ? nav.setAppBadge(n) : typeof nav.clearAppBadge === 'function' ? nav.clearAppBadge() : nav.setAppBadge(0);
    return Promise.resolve(pending).then(() => true, () => false);
  } catch { return Promise.resolve(false); }
}
