// Personal interface preferences live on the server (per user, the same on every device). The browser keeps a copy under its own keys
// only so the page can paint the right theme and sound before anyone signs in; the server's value wins once it is loaded.
import { request, currentUser } from './api.js';

// Preference name -> the browser key that caches it. Layout state of one device (collapsed panels and rooms) is deliberately not here.
export const PREFERENCE_KEYS = {
  theme: user => `aimebu2.ui.theme.${user}`,
  inbox_muted: user => `aimebu2:inbox-muted:${user}`,
  inbox_sound: user => `aimebu2:inbox-sound:${user}`,
  inbox_volume: user => `aimebu2:inbox-volume:${user}`,
  system_events: user => `aimebu2.ui.show-system-events.${user}`,
  debug_inspector: user => `aimebu2.ui.debug-inspector.${user}`,
  composer_formatting: () => 'aimebu2.composer.formatting',
  sidebar_sort: user => `aimebu2.ui.sidebar_sort.${user}`,
  sidebar_project_order: user => `aimebu2.ui.sidebar_project_order.${user}`,
};
// How long a value may be on the server (a word or a number, except the manual project order, which is a list of ids).
export const PREFERENCE_LIMITS = { sidebar_project_order: 4096 };
const limitOf = name => PREFERENCE_LIMITS[name] || 256;

let user = '';

// Called once after sign-in: server values replace the cache; values only the cache has (set before this feature, or offline) are
// uploaded, so an existing browser's choices are not lost.
export async function loadPreferences(userID, { send = request, storage = localStorage } = {}) {
  user = userID;
  let stored;
  try { stored = (await send('/api/me/preferences')).preferences || {}; } catch { return; }
  const upload = {};
  for (const [name, keyOf] of Object.entries(PREFERENCE_KEYS)) {
    const key = keyOf(userID);
    try {
      if (Object.hasOwn(stored, name)) storage.setItem(key, stored[name]);
      else { const local = storage.getItem(key); if (local !== null && local.length <= limitOf(name)) upload[name] = local; }
    } catch { /* private browsing: the server copy still applies on the next load */ }
  }
  if (Object.keys(upload).length) { try { await send('/api/me/preferences', { method: 'PUT', body: { preferences: upload } }); } catch { /* retried on the next sign-in */ } }
}

// Stores one preference on the server; the caller has already written its own cache. Failures are silent: the next change or sign-in retries.
// The saves of one name go out one after another, so a slow older value (the manual project order, say) can never land after a newer one.
// A queued save belongs to the person who made it: if the session has changed by the time it would be sent (signed out, or someone else
// signed in on this page), it is dropped, so it can never reach another person's preferences with their cookie.
const chains = new Map();
export function savePreference(name, value, { send = request } = {}) {
  if (!user || !Object.hasOwn(PREFERENCE_KEYS, name)) return Promise.resolve();
  const owner = user, key = `${owner}\n${name}`;
  const next = (chains.get(key) || Promise.resolve())
    .then(() => owner === user && owner === currentUser ? send('/api/me/preferences', { method: 'PUT', body: { preferences: { [name]: String(value) } } }) : undefined)
    .catch(() => {});
  chains.set(key, next);
  next.then(() => { if (chains.get(key) === next) chains.delete(key); });
  return next;
}
