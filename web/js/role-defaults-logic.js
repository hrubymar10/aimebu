// Pure helpers over a role's ordered default agents ({launcher_id, model}), shared by the Harnesses page and its tests.
export const MAX_ROLE_DEFAULTS = 32;
export const PICK_MODES = [['in_turn', 'In turn'], ['randomly', 'Randomly']];

const same = (item, launcherID, model) => item.launcher_id === launcherID && item.model === model;

// Where a harness model sits in a role's list (0 is first), or -1 when the role does not default to it.
export const positionOf = (role, launcherID, model) => (role?.default_agents || []).findIndex(item => same(item, launcherID, model));

// The roles that default to one harness model, in the order the roles are listed.
export const rolesFor = (roles, launcherID, model) => (roles || []).filter(role => positionOf(role, launcherID, model) >= 0);

// The list after turning one model on or off for a role: on appends it (a model already listed keeps its place), off removes it.
export function withModel(list, launcherID, model, on) {
  const items = (list || []).map(item => ({ launcher_id: item.launcher_id, model: item.model }));
  if (!on) return items.filter(item => !same(item, launcherID, model));
  return items.some(item => same(item, launcherID, model)) ? items : [...items, { launcher_id: launcherID, model }];
}

// The list with one entry moved by `by` places. The entry is named by its launcher and model, never by where it was drawn: a save is applied
// to the list as it is when its turn comes, and a position from an earlier draw could name a different entry by then. A move off either end,
// or of an entry that is gone, changes nothing.
export function movedByKey(list, launcherID, model, by) {
  const items = (list || []).map(item => ({ launcher_id: item.launcher_id, model: item.model }));
  const index = items.findIndex(item => same(item, launcherID, model)), to = index + by;
  if (index < 0 || to < 0 || to >= items.length) return items;
  const [item] = items.splice(index, 1); items.splice(to, 0, item);
  return items;
}

// Whether a role may take one more default.
export const canAddDefault = role => (role?.default_agents || []).length < MAX_ROLE_DEFAULTS;

// The request body that saves a role with some fields changed: everything the role editor sends, so nothing else about the role moves.
export function roleBody(role, patch = {}) {
  return { id: role.id, rev: role.rev, role_key: role.role_key, label: role.label, emoji: role.emoji || '', body: role.body || '', extends: role.extends || '',
    cardinality: role.cardinality, cap_is_lead: !!role.cap_is_lead, cap_can_spawn: !!role.cap_can_spawn, cap_singleton_writer: !!role.cap_singleton_writer,
    default_agents: (role.default_agents || []).map(item => ({ launcher_id: item.launcher_id, model: item.model })), pick_mode: role.pick_mode || 'in_turn', ...patch };
}

// The short mark of a role: its emoji, or the first two letters of its label (or key).
export const roleMark = role => role.emoji || (role.label || role.role_key).slice(0, 2);
