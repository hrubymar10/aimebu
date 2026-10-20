// Pure parts of the settings backup UI: which browser preferences travel, and how a section is described.
export const SECTION_LABELS = { macros: 'Macros', retention: 'Message expiry', quota_switcher: 'Quota switcher', limit_guard: 'Usage limits', roles: 'Roles', ui_prefs: 'Interface preferences' };

// A preference is named by what it is, not by the key this browser stores it under (some keys carry the user's id).
export function backupPrefKeys(userID) {
  return { theme: `aimebu2.ui.theme.${userID}`, inbox_muted: `aimebu2:inbox-muted:${userID}`, inbox_sound: `aimebu2:inbox-sound:${userID}`, inbox_volume: `aimebu2:inbox-volume:${userID}`,
    rooms_collapsed: 'aimebu2.rooms.collapsed', usages_group: 'aimebu2.usages.group', left_collapsed: 'aimebu_left_collapsed', right_collapsed: 'aimebu_right_collapsed' };
}

export function collectPrefs(storage, userID) {
  const prefs = {};
  for (const [name, key] of Object.entries(backupPrefKeys(userID))) {
    const value = storage.getItem(key);
    if (value !== null && value.length <= 256) prefs[name] = value;
  }
  return prefs;
}

export function applyPrefs(storage, userID, prefs) {
  const keys = backupPrefKeys(userID);
  for (const [name, value] of Object.entries(prefs || {})) {
    if (Object.hasOwn(keys, name) && typeof value === 'string' && value.length <= 256) storage.setItem(keys[name], value);
  }
}

export function describeSection(section) {
  const parts = [];
  if (section.new) parts.push(`${section.new} new`);
  if (section.overwrite) parts.push(`${section.overwrite} to overwrite`);
  if (section.unchanged) parts.push(`${section.unchanged} already the same`);
  if (section.invalid?.length) parts.push(`${section.invalid.length} skipped`);
  if (section.unavailable) parts.push(section.unavailable);
  return `${SECTION_LABELS[section.section] || section.section}: ${parts.join(', ') || 'nothing'}`;
}
