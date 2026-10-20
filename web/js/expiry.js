// Message expiry for the chat. Expiry only affects what an AI agent is given; it
// never deletes a message, and the chat always shows everything. This file is
// the pure part (no DOM, no network) so a test can run it in a vm: the cutoff,
// and the plan of which messages carry the hourglass and where the divider goes.

export const EXPIRY_DEFAULT_SECONDS = 21600;
export const EXPIRY_MIN_SECONDS = 60;
export const EXPIRY_MAX_SECONDS = 2592000;

// expiryCutoffMs is the instant at or before which a message counts as expired;
// 0 means expiry is off. A message is expired when now - created_at >= window,
// the rule the server documents.
export function expiryCutoffMs(nowMs, seconds) {
  const window = Number(seconds);
  return Number.isFinite(window) && window > 0 ? nowMs - window * 1000 : 0;
}

export function isExpired(createdMs, cutoffMs) {
  return cutoffMs > 0 && Number.isFinite(createdMs) && createdMs <= cutoffMs;
}

// planExpiry takes the messages in display order (oldest first) as {id, createdMs, expirable} and returns
// the ids that are expired, the id of the first message newer than the cutoff that follows an older one
// (the label goes right above it; null when nothing is expired or nothing is newer), and the expired
// count. Only an expirable message (AI chatter; humans' messages are always delivered) is marked, but
// the boundary is by time for everyone, so human messages above it sit unmarked. The boundary latches at
// the first old-to-new transition even if clock skew interleaves the two later on, so there is only one.
export function planExpiry(items, cutoffMs) {
  const expired = new Set();
  let divider = null, sawOld = false;
  for (const item of items) {
    if (isExpired(item.createdMs, cutoffMs)) { sawOld = true; if (item.expirable !== false) expired.add(item.id); continue; }
    if (sawOld && divider === null) divider = item.id;
  }
  return { expired, divider: expired.size ? divider : null, count: expired.size };
}

// validExpirySeconds mirrors the server: 0 (never) or 60 to 2592000.
export function validExpirySeconds(value) {
  return Number.isInteger(value) && (value === 0 || (value >= EXPIRY_MIN_SECONDS && value <= EXPIRY_MAX_SECONDS));
}
