// Import-free: scripts/test-shell-model.cjs runs this module in a vm.

// 'agents' is the caption that starts the second group.
export const TABS = [['general', 'General'], ['usages', 'Usages'], ['notifications', 'Notifications'], ['telemetry', 'Telemetry'], ['roles', 'Roles'], ['users', 'Users'],
  ['prompts', 'Prompts'], ['macros', 'Macros'], ['agents'], ['machines', 'Machines'], ['harnesses', 'Harnesses & models']];

const INSTANCE = new Set(['roles', 'prompts', 'macros', 'machines', 'harnesses']);

// usages: the tab is the owner's and the admins' (an admin reads, only the owner changes), and a 403 on the usage reads takes it away. users: 'pending' until the access check answers.
export function tabHidden(key, { owner, admin, usagesHidden, users }) {
  if (key === 'usages') return usagesHidden;
  if (key === 'users') return users === 'hidden';
  if (key === 'backup') return !(owner || admin); // the settings backup: an admin exports, the owner imports
  if (key === 'agents') return !(owner || admin);
  return INSTANCE.has(key) ? !(owner || admin) : false;
}

export const sectionTitle = section => (TABS.find(([key]) => key === section)?.[1]) || '';
