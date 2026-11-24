// Import-free: scripts/test-roles-model.cjs runs this module in a vm.

export const CAPS = ['cap_is_lead', 'cap_can_spawn', 'cap_singleton_writer'];
const TEXT = ['role_key', 'label', 'emoji', 'body', 'extends', 'body_extension'];

export const roleOptionLabel = role => `${role.label} (${role.role_key})`;

// The editor's fields for a saved role, or for a new one (no role).
export function formFromRole(role) {
  const form = { cardinality: role?.cardinality || 'multi', default_agents: (role?.default_agents || []).map(item => ({ launcher_id: item.launcher_id, model: item.model })), pick_mode: role?.pick_mode || 'in_turn' };
  for (const key of TEXT) form[key] = role?.[key] || '';
  for (const key of CAPS) form[key] = !!role?.[key];
  return form;
}

// What the form holds, to tell whether closing it would throw an edit away.
export const formState = form => JSON.stringify([...TEXT.map(key => form[key]), form.cardinality, ...CAPS.map(key => form[key]), form.default_agents, form.pick_mode]);

// The body an update or create sends. The extension rides along only for the owner (an admin's write must keep what is saved,
// and the field is the owner's alone); a nil keeps it.
export function roleData(form, selected, owner = false) {
  const data = { role_key: form.role_key.trim(), label: form.label.trim(), emoji: form.emoji.trim(), body: form.body, extends: form.extends, cardinality: form.cardinality };
  for (const key of CAPS) data[key] = form[key];
  data.default_agents = form.default_agents.map(item => ({ ...item }));
  data.pick_mode = form.pick_mode;
  if (owner) data.body_extension = form.body_extension;
  if (selected) { data.id = selected.id; data.rev = selected.rev; }
  return data;
}

// The roles a role may extend: every other role by key, with a parent that no longer exists kept so saving does not drop it silently.
export function extendsOptions(roles, role) {
  const keys = new Map();
  for (const item of roles || []) if (!role || item.id !== role.id) keys.set(item.role_key, item);
  const options = [['', 'None'], ...[...keys.values()].sort((a, b) => a.role_key.localeCompare(b.role_key)).map(item => [item.role_key, `${item.emoji ? `${item.emoji} ` : ''}${item.label} (${item.role_key})`])];
  const current = role?.extends || '';
  if (current && !keys.has(current)) options.push([current, `${current} (missing)`]);
  return options;
}

export function chipsOf(role, roles) {
  const chips = [[role.cardinality === 'singleton' ? 'one holder' : 'many holders']];
  if (role.extends) {
    const parent = (roles || []).find(item => item.role_key === role.extends);
    chips.push([`extends ${parent ? parent.label : role.extends}`, `Adds to the instructions of ${role.extends}`]);
  }
  if (role.cap_is_lead) chips.push(['leader']);
  if (role.cap_can_spawn) chips.push(['can spawn']);
  if (role.builtin) chips.push(['built in']);
  return chips;
}

export function summaryOf(role) {
  const text = (role.body || role.resolved_body || '').trim().replace(/\s+/g, ' ');
  return text.length > 160 ? `${text.slice(0, 157)}…` : text || 'No instructions of its own.';
}

export const editHeading = role => (role ? `Editing ${role.label}` : 'New role');
export const resetQuestion = role => `Reset ${role.label} to its default name, instructions and settings? Your edits to it are lost.`;
