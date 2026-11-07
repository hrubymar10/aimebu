// Import-free: scripts/test-users-model.cjs runs this module in a vm.

export const ROLES = ['viewer', 'operator', 'owner'];

export const chosenProject = (allowed, selected) => (allowed.some(project => project.id === selected) ? selected : (allowed[0]?.id ?? ''));

export const uniqueAccounts = users => { const seen = new Set(); return users.filter(user => !seen.has(user.id) && seen.add(user.id)); };

// The owner can reset or deactivate anyone and appoint admins; an admin only acts on ordinary accounts (the server enforces the same).
export function accountActions(user, { globalOwner, currentUser }) {
  const mayAct = globalOwner || !(user.owner || user.admin), live = !user.disabled;
  return {
    badge: user.owner ? 'Owner' : user.admin ? 'Admin' : '',
    adminToggle: globalOwner && !user.owner && live,
    reset: live && mayAct,
    deactivate: live && mayAct && user.id !== currentUser && !user.owner,
  };
}

// Whether a member can be taken off the project from this row, and if not, why: nobody removes themselves (they would lock themselves out),
// and a project keeps at least one owner. The server still has the last word.
export function memberRemoval(user, members, currentUser) {
  if (user.id === currentUser) return { disabled: true, reason: 'You cannot remove yourself from the project' };
  if (user.role === 'owner' && members.filter(member => member.role === 'owner').length < 2) return { disabled: true, reason: 'A project needs at least one owner' };
  return { disabled: false, reason: '' };
}

// A link made on this computer only works here, and the person it is for is usually elsewhere.
export function inviteHint(url) {
  let host = '';
  try { host = new URL(url).hostname; } catch { /* a link that does not parse gets no hint */ }
  const local = host === 'localhost' || host === '127.0.0.1' || host === '::1' || host === '[::1]';
  return local ? `This address (${host}) only works on this computer. For someone else, replace it with an address they can reach: keep everything from "/#invite=" on.` : '';
}
