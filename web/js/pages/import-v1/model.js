// Import-free: scripts/test-import-v1-model.cjs runs this module in a vm. The v1 import wizard's pure rules: the step list, the summary's
// labels and rows, the people and room wording, and the footer totals. The room-choice rules stay in import-tree.js; the callers pass them in.

// v1's own event room is never imported, so the wizard does not list it.
export const SYSTEM_ROOM = '_system';

// What each line of the summary table means, in the owner's words. An unknown key falls back to its raw name.
export const COUNT_LABELS = {
  agent_session_history: ['Agent session history', 'Every harness session an agent name ran in, so "Start as v2 agent" can offer the one that did a sender\'s work.'],
  agent_sessions: ['Agent sessions', 'Where each agent ran and its harness session; shown on the profile of its name in old messages.'],
  attachments: ['Attachments', 'Files attached to messages.'],
  historical_members: ['Past members', 'People and agents who were in a room; kept as names so old messages still show who wrote them.'],
  home_rooms: ['Project home rooms', 'The top room created for each new project.'],
  inline_plan_appendix: ['Inline plans setting', 'Whether leads are told to always attach the full plan.'],
  macro_conflicts: ['Macro conflicts', 'Macros that exist in both v1 and v2.'],
  macros: ['Macros', 'Shared prompt texts such as <TASK>.'],
  accounts: ['Accounts created', 'People who get a new account; each gets a one-time link after the import.'],
  memory: ['Memory records', 'Project facts, user profiles and shared agent notes.'],
  message_expiry: ['Message expiry setting', 'How long before a message counts as expired.'],
  messages: ['Messages', 'Chat history, with its original numbers.'],
  orphan_attachments: ['Unused attachment files', 'Files in the backup that no message refers to; left behind.'],
  presence_lines: ["Bookkeeping lines", "v1's own join, leave and memory lines; v2 keeps presence elsewhere, so they are not imported."],
  preferences: ['Personal settings', 'Your theme, notification volume and interface switches from v1, saved to your profile.'],
  project_memory_settings: ['Project memory switches', 'Projects that start with v2 memory turned on.'],
  projects: ['Projects', 'Workspaces the imported rooms belong to.'],
  reactions: ['Reactions', 'Emoji reactions on messages.'],
  read_cursor_baselines: ['Caught-up positions', 'Everyone except you starts caught up in imported rooms, so only your own unread messages show.'],
  read_cursors: ['Your read positions', 'How far you had read in each room, so unread counts carry over.'],
  role_conflicts: ['Role conflicts', 'Roles that exist in both v1 and v2.'],
  role_links: ['Role links flattened', 'Roles whose link to a parent role was merged into their own text.'],
  roles: ['Roles', 'Role definitions.'],
  room_memory_settings: ['Room memory switches', 'Rooms where v1 had memory turned off.'],
  room_prefs: ['Room view settings', 'Rooms you had hidden, and your project order (a room you had pinned puts its project first).'],
  rooms: ['Rooms', 'Chat rooms.'],
  settings: ['Settings', 'v1 settings that v2 does not use; they stay in the backup.'],
  sounds: ['Custom sounds', 'Notification sounds you uploaded.'],
  unresolved_memory_source_links: ['Memory links not traced', 'Memory records whose source message was not imported keep their text but lose the link back to it.'],
};

// The summary's rows in the order an owner reads them: what holds the history first, then the rest, then settings and bookkeeping last.
const COUNT_ORDER = ['projects', 'home_rooms', 'rooms', 'messages', 'attachments', 'reactions', 'memory', 'macros', 'macro_conflicts', 'roles', 'role_conflicts', 'role_links', 'accounts'];
const COUNT_LAST = new Set(['inline_plan_appendix', 'message_expiry', 'preferences', 'settings', 'room_prefs', 'project_memory_settings', 'room_memory_settings', 'read_cursors', 'read_cursor_baselines', 'historical_members', 'agent_sessions', 'agent_session_history', 'orphan_attachments', 'unresolved_memory_source_links', 'sounds']);
export function countRank(key) {
  if (COUNT_ORDER.includes(key)) return COUNT_ORDER.indexOf(key);
  return COUNT_LAST.has(key) ? 200 : 100;
}

// What happened to each part of a reconciled count (report.accounting), in the owner's words. `rooms` words its own skips, since a room is not a record in one.
export const ACCOUNT_GROUPS = { rooms: 'Rooms', messages: 'Messages', attachments: 'Attachments', reactions: 'Reactions', macros: 'Macros' };
export const ACCOUNT_PARTS = {
  imported: 'Imported',
  already_present: 'Already in v2',
  presence_lines: "v1's own join, leave and memory bookkeeping lines (v2 keeps presence elsewhere)",
  system_room: "In v1's own event room (not conversation)",
  dm_not_imported: 'In direct messages you did not tick',
  no_project: 'In rooms with no project to go into',
  room_skipped: 'In rooms you chose not to import',
  in_skipped_rooms: 'On messages in rooms not imported',
  person_skipped: 'By people you did not map to an account',
  kept_v2_text: 'Kept the v2 text (a key both have)',
  unticked: 'Unticked on the Macros page',
  v1_builtin_example: "v1's built-in example macros you never changed (v2 has its own)",
  deleted_in_v1: 'Built-in macros you deleted in v1',
  unused_files: 'Files no message refers to',
  other: 'Other',
};
export const ROOM_PARTS = { system_room: "v1's own event room", dm_not_imported: 'Direct messages you did not tick', no_project: 'Rooms with no project to go into', room_skipped: 'Rooms you chose not to import' };
export const number = value => Number(value || 0).toLocaleString('en-US');

// The pages of the wizard, in order. A page that has nothing to show for this backup is skipped.
const STEPS = [
  ['projects', 'Projects', () => true],
  ['people', 'People', preview => (preview.humans || []).length > 0],
  ['rooms', 'Rooms', preview => (preview.rooms || []).some(room => !room.dm && room.id !== SYSTEM_ROOM)],
  ['dms', 'Direct messages', preview => (preview.rooms || []).some(room => room.dm)],
  ['memory', 'Memory', preview => (preview.counts?.memory || 0) > 0 || (preview.memory_projects || []).length > 0],
  ['macros', 'Macros', preview => Object.keys(preview.macros || {}).length > 0],
  ['roles', 'Roles', preview => (preview.roles || []).length > 0],
  ['summary', 'Summary', () => true],
];
export function stepsFor(preview) { return STEPS.filter(([, , applies]) => applies(preview)).map(([id, title]) => ({ id, title })); }

// What the import is doing, in words, as the server reports its phase.
export const IMPORT_PHASES = { scan: 'Reading the backup…', validate: 'Checking your choices…', copy: 'Copying files…', commit: 'Writing your history to v2 (this can take a minute)…', done: 'Done.' };

// Said once as a warning on the first page and again, in the same words, when skipping is confirmed.
export const ONLY_CHANCE = 'This is the only chance to import v1 history. Skipping closes the import permanently, and the original backup stays on disk.';

export const NEW_PROJECT = '__new_project__';
export const NEW_ACCOUNT = '__new_account__';

// The owner's choices and place are kept in the browser per backup under this key; changing it would lose every saved draft.
export function draftKey(sourceKey) { return `aimebu2.import-draft.${sourceKey}`; }

// What choosing "Skip person" costs, said next to the choice: their messages keep their name as a label, but what belongs to the person's
// account is not imported at all, and an import cannot be repeated. `stakes` counts it (see human_stakes in the preview).
export function skippedPersonWarning(stakes) {
  const plural = (count, one, many = `${one}s`) => `${count} ${count === 1 ? one : many}`;
  const lost = [];
  if (stakes?.memory_profiles) lost.push(plural(stakes.memory_profiles, 'memory profile'));
  if (stakes?.reactions) lost.push(plural(stakes.reactions, 'reaction'));
  if (stakes?.read_positions) lost.push(plural(stakes.read_positions, 'read position'));
  if (stakes?.room_view) lost.push(plural(stakes.room_view, 'pinned or hidden room'));
  return 'No account: their messages keep their name as a label, and nobody can sign in as them.'
    + (lost.length ? ` Their ${lost.join(', ')} would not be imported, and an import cannot be repeated.` : '')
    + ' Choose an account, or "Create a new account…", to keep them.';
}

// A login for a new account from a v1 name: lowercase, 3 to 32 characters, not one already in use here or chosen for someone else.
export function newLogin(name, preview, mapping) {
  const taken = new Set([...(preview.destination_users || []), ...Object.values(mapping.new_users || {}).map(user => user.login)].map(login => String(login).toLowerCase()));
  let base = name.toLowerCase().replace(/[^a-z0-9_-]+/g, '-').replace(/^[^a-z]+/, '').slice(0, 28);
  if (base.length < 3) base = `user-${base}`.replace(/-$/, '');
  if (base.length < 3) base = 'user';
  let login = base, n = 2;
  while (taken.has(login.toLowerCase())) login = `${base}-${n++}`;
  return login;
}

// Someone who is not the owner starts as a new account instead of being merged into the owner's (they can switch to a skip or an existing
// account on the People page). Applied only where nobody has decided: a fresh suggestion, and a person a saved draft has no choice for.
// Changes `mapping` in place.
export function defaultPeople(preview, mapping, eligible = () => true) {
  mapping.new_users ||= {};
  for (const human of preview.humans || []) {
    if (!eligible(human) || mapping.humans[human] !== '-' || mapping.new_users[human]) continue;
    const login = newLogin(human, preview, mapping);
    mapping.new_users[human] = { login, role: 'viewer' }; mapping.humans[human] = login;
  }
}

// A DM is titled by the participants other than the owner's own accounts (and skipped people): the agent's name, "a · b" for two or more.
// It follows the current person mapping; the server applies the same rule when it imports.
export function dmTitle(room, mapping) {
  const people = room.participants || [];
  const owner = mapping.owner_login;
  let others = people.filter(person => !(person.kind === 'user' && [owner, '-', '', undefined].includes(mapping.humans[person.id])));
  if (!others.length) others = people;
  return others.length ? others.map(person => person.name).join(' · ') : (room.title || room.id);
}

// Where a v1 room goes, in words, for the Summary: its place in v2, and whether it is archived. `rules` is import-tree.js's
// { roomChoice, parseChoice }. "(archived)" is the merged target's state (open when any room in it is open), not the room's own flag.
export function roomDestination(room, mapping, tree, rules) {
  const spec = mapping.rooms[room.id] || {};
  if (room.dm) return spec.skip || !spec.project ? 'not imported' : `direct message in ${mapping.projects[spec.project]?.name || spec.project}`;
  const value = rules.roomChoice(spec, room.id);
  if (value === 'skip') return 'not imported';
  const { kind, alias } = rules.parseChoice(value);
  const project = mapping.projects[alias]?.name || alias;
  if (kind === 'root') return `⌂ ${project}`;
  const sub = tree.projects.find(item => item.alias === alias)?.subs.find(item => item.rooms.includes(room.id));
  // Only a named sub-room can merge rooms; a room kept as itself is archived by its own flag.
  const archived = spec.target === 'sub' && sub ? sub.archived : spec.lifecycle !== 'open';
  return `${project} › ${sub?.title || (kind === 'general' ? 'general' : spec.title || room.id)}${archived ? ' (archived)' : ''}`;
}

// How a macro's text is shown: { kind: 'empty' } for none, { kind: 'short', text } in full, or { kind: 'long', text, summary } folded behind
// its first line, so a page of macros stays a page.
export function macroBody(text) {
  if (!text) return { kind: 'empty', text: '(empty)' };
  const lines = text.split('\n');
  if (text.length <= 300 && lines.length <= 6) return { kind: 'short', text };
  const first = lines.find(line => line.trim()) || '';
  return { kind: 'long', text, summary: `${first.length > 80 ? `${first.slice(0, 80)}…` : first} (${text.length.toLocaleString('en-US')} characters)` };
}

// What the long pages add up to, kept in view beside the buttons: how much is selected and how much is left out. '' for a page with none.
export function footerTotals(stepID, preview, mapping) {
  const p = preview, m = mapping, n = (count, one, many = `${one}s`) => `${count.toLocaleString('en-US')} ${count === 1 ? one : many}`;
  if (stepID === 'projects') {
    const kept = Object.keys(m.projects).length, dropped = Object.keys(p.suggested?.projects || {}).filter(alias => !m.projects[alias]).length;
    return `${n(kept, 'project')} selected${dropped ? ` · ${n(dropped, 'project')} removed, their rooms are not imported` : ''}`;
  }
  if (stepID === 'rooms') {
    const rooms = (p.rooms || []).filter(room => !room.dm && room.id !== SYSTEM_ROOM);
    const left = rooms.filter(room => m.rooms[room.id]?.skip || !m.rooms[room.id]?.project);
    const messages = rooms.filter(room => !left.includes(room)).reduce((sum, room) => sum + (room.messages || 0), 0);
    return `${rooms.length - left.length} of ${n(rooms.length, 'room')} selected · ${n(messages, 'message')} go in${left.length ? ` · ${n(left.length, 'room')} not imported` : ''}`;
  }
  if (stepID === 'memory') {
    const c = p.memory_counts || {}, all = (p.counts?.memory || 0);
    let kept = 0;
    for (const [scope, count] of Object.entries(c.projects || {})) if ((m.memory_projects[scope] || '-') !== '-') kept += count;
    // A person's profile goes with the person: one mapped to nobody ("Skip person") is not imported, whatever the checkbox says.
    if (!m.skip_profiles) kept += Object.entries(c.profile_owners || {}).reduce((sum, [person, count]) => sum + (m.humans[person] === '-' ? 0 : count), 0);
    if (!m.skip_shared_notes) kept += c.shared || 0;
    return `${kept} of ${n(all, 'memory record')} selected${kept < all ? ` · ${n(all - kept, 'record')} not imported` : ''}`;
  }
  return '';
}

// The reconciled totals (report.accounting), or null when there are none: a headline, how many messages are left out, and a block per count
// whose parts add up to the backup's own number. A part is only ever listed as a positive count; a skipped part keeps the rooms behind it.
export function accountingRows(report) {
  const groups = (report.accounting || []).filter(group => group.total > 0);
  if (!groups.length) return null;
  const part = (group, key) => group.parts.find(item => item.key === key)?.count || 0;
  const brought = group => part(group, 'imported') + part(group, 'already_present');
  const bits = [];
  for (const [key, word] of [['messages', 'messages'], ['rooms', 'rooms'], ['attachments', 'attachments']]) {
    const group = groups.find(item => item.key === key);
    if (group) bits.push(`${number(brought(group))} of ${number(group.total)} ${word}`);
  }
  const left = groups.reduce((sum, group) => sum + (group.key === 'messages' ? group.total - brought(group) : 0), 0);
  const kept = key => key === 'imported' || key === 'already_present';
  return {
    headline: `Bringing across ${bits.join(', ')}.`,
    left,
    leftNote: left > 0 ? `${number(left)} messages are left out; the lists below say why.` : '',
    groups: groups.map(group => {
      const dropped = group.parts.filter(item => !kept(item.key) && item.count > 0);
      return {
        key: group.key,
        title: `${ACCOUNT_GROUPS[group.key] || group.key}: ${number(group.total)} in v1`,
        note: dropped.length ? `${number(dropped.reduce((sum, item) => sum + item.count, 0))} left out` : 'all brought across',
        open: dropped.length > 0,
        total: number(group.total),
        rows: group.parts.filter(item => item.count > 0).map(item => ({
          key: item.key,
          label: (group.key === 'rooms' && ROOM_PARTS[item.key]) || ACCOUNT_PARTS[item.key] || item.key.replaceAll('_', ' '),
          count: number(item.count),
          skipped: !kept(item.key),
          items: item.items || [],
          itemsLabel: item.items?.length ? `Show ${item.items.length === 1 ? 'it' : `all ${number(item.items.length)}`}` : '',
        })),
      };
    }),
  };
}

// The counts table: the rows and columns that are all zero are left out; "Skipped" shows only when something is in it.
export function reportCountRows(report) {
  const keys = [...new Set([...Object.keys(report.imported || {}), ...Object.keys(report.skipped || {})])].sort((a, b) => countRank(a) - countRank(b) || a.localeCompare(b));
  const value = (map, key) => map?.[key] || 0;
  const shown = keys.filter(key => value(report.imported, key) || value(report.skipped, key));
  const showSkipped = shown.some(key => value(report.skipped, key));
  return {
    showSkipped,
    rows: shown.map(key => {
      const [title, explanation] = COUNT_LABELS[key] || [key.replaceAll('_', ' '), ''];
      return { key, title, explanation, imported: number(value(report.imported, key)), skipped: number(value(report.skipped, key)), skippedNonZero: value(report.skipped, key) > 0 };
    }),
  };
}

// The line under the progress bar for a review page: what the page is about and how much of it is chosen. The Macros page's comes with its
// cards (macroCards), and the Summary's follows the dry run, so both are worded where they are drawn.
export function stepStatus(stepID, preview, mapping) {
  const p = preview, m = mapping;
  if (stepID === 'projects') {
    const counts = p.counts || {};
    return `The backup holds ${counts.rooms || 0} rooms, ${counts.messages || 0} messages, ${counts.attachments || 0} attachments and ${counts.memory || 0} memory records. First, the projects the history goes into: each needs a name and an absolute workspace folder.`;
  }
  if (stepID === 'people') return 'Each v1 person\'s messages show under the account you map them to, so people sharing one destination account are merged, in access and in how their old messages are labelled. A person can get a new account made by the import (no password: you get a one-time link for each at the end). A skipped person has no account: their messages keep their v1 name as a label, and they receive no live room membership.';
  if (stepID === 'rooms') {
    const rooms = (p.rooms || []).filter(room => room.id !== SYSTEM_ROOM && !room.dm);
    const kept = rooms.filter(room => !m.rooms[room.id]?.skip && m.rooms[room.id]?.project).length;
    return `${kept} of ${rooms.length} rooms will be imported. Every room starts open; archive individual rooms or use Archive all if you want. Rooms sent to the same place merge, their messages interleaved by their v1 numbers.`;
  }
  if (stepID === 'dms') {
    const direct = (p.rooms || []).filter(room => room.dm);
    const picked = direct.filter(room => !m.rooms[room.id]?.skip).length, total = direct.length;
    const count = total === 1 ? `The chat is${picked ? '' : ' not'}` : picked === 0 ? `None of the ${total} chats are` : picked === total ? `All ${total} chats are` : `${picked} of ${total} chats are`;
    return `${count} going to be imported. Each private chat becomes a v2 direct message titled by the other person or agent, always open. A chat between two agents is imported as history you can read. No chat is imported unless you tick it; a ticked chat goes into its agent's project when there is one, and you can pick another.`;
  }
  if (stepID === 'memory') return `The backup holds ${p.counts?.memory || 0} memory records. Imported records arrive as ordinary, active ones.`;
  if (stepID === 'roles') return 'Importable v1 roles become global v2 definitions. Existing v2 definitions win unless you choose the v1 version.';
  return '';
}

// The People page's warning when nobody is mapped to the owner (the backup has several people and none with the owner's name), or ''.
export function ownerHint(preview, mapping) {
  const owner = mapping.owner_login, humans = preview.humans || [];
  const noSuggestedOwner = !Object.values(preview.suggested?.humans || {}).includes(owner);
  const noMappedOwner = !humans.some(human => mapping.humans[human] === owner);
  if (humans.length <= 1 || !noSuggestedOwner || !noMappedOwner) return '';
  return `None of these people is mapped to your account (${owner}): the backup has no one with your name, and more than one person. Choose which one is you to carry that person's read positions, memory profile, pins and reactions into ${owner}. New accounts keep their own importable data; skipping a person drops theirs.`;
}

// The Macros page: the status line (the same buckets the Summary reports: new ones, ones that replace v2's text, ones that keep it, ones
// already the same) and one card per macro, the ones that need a decision (a key both have with different text) first.
export function macroCards(preview, mapping, destination) {
  const p = preview, dest = destination || {};
  const keys = Object.keys(p.macros || {}).sort();
  const skip = new Set(mapping.skip_macros || []);
  const done = new Set(p.imported_macros || []);
  const conflict = key => mapping.macro_conflicts?.[key] || 'keep_v2';
  const stateOf = key => {
    if (done.has(key)) return ['imported', 'Already imported'];
    if (!Object.hasOwn(dest, key)) return ['new', 'New'];
    if (dest[key] === p.macros[key]) return ['same', 'Already in v2'];
    return conflict(key) === 'use_v1' ? ['conflict', 'Conflict: replacing the v2 text'] : ['conflict', 'Conflict: keeping the v2 text'];
  };
  const tally = { new: 0, replace: 0, keep: 0, same: 0, imported: 0 };
  for (const key of keys.filter(key => !skip.has(key))) {
    const state = stateOf(key)[0];
    if (state === 'conflict') tally[conflict(key) === 'use_v1' ? 'replace' : 'keep']++;
    else tally[state]++;
  }
  const parts = [[tally.new, 'new'], [tally.replace, 'replace the v2 text'], [tally.keep, 'keep the v2 text (a key both have)'], [tally.same, 'already match v2'], [tally.imported, 'already imported from this backup']].filter(([n]) => n).map(([n, text]) => `${n} ${text}`);
  const leftOut = Object.entries(p.macros_left_out || {}).sort(([a], [b]) => a.localeCompare(b));
  const rank = { conflict: 0, new: 1, same: 2, imported: 3 };
  return {
    keys,
    status: `${keys.length - skip.size} of ${keys.length} macros chosen${parts.length ? `: ${parts.join(', ')}` : ''}.`,
    leftOut: leftOut.length ? `Left out: ${leftOut.map(([key, why]) => `<${key}> (${why === 'deleted_in_v1' ? 'deleted in v1' : "v1's built-in example, unchanged; v2 has its own"})`).join(', ')}.` : '',
    cards: [...keys].sort((a, b) => rank[stateOf(a)[0]] - rank[stateOf(b)[0]] || a.localeCompare(b)).map(key => {
      const [state, label] = stateOf(key), exists = Object.hasOwn(dest, key);
      return { key, state, label, legend: `<${key}>${(p.macro_defaults || []).includes(key) ? ' · v1 seeded default' : ''} `, imported: done.has(key), included: !skip.has(key), body: p.macros[key],
        current: exists ? dest[key] : null, exists, same: exists && dest[key] === p.macros[key], conflict: conflict(key) };
    }),
  };
}

// The Summary's folded lists, in reading order, each { title, items, note }; an empty list is left out. `rules` is import-tree.js's
// { roomChoice, parseChoice, planTree }.
export function summaryLists(report, preview, mapping, rules) {
  const lists = [];
  const list = (title, items, note = '') => { if (items.length) lists.push({ title, items, note }); };
  const byKey = map => Object.entries(map || {}).sort(([a], [b]) => a.localeCompare(b));
  const placed = (preview.rooms || []).filter(room => room.id !== SYSTEM_ROOM);
  const tree = rules.planTree(mapping, placed);
  list('Where each room goes', placed.map(room => `${room.dm ? dmTitle(room, mapping) : room.id} → ${roomDestination(room, mapping, tree, rules)}${room.dm ? ` · ${room.id}` : ''}`), 'Check each placement before importing.');
  list('Rooms not imported: no project', report.skipped_no_project || [], 'These have no project to go into. Choose one on the Rooms page to bring them across.');
  list('Memory switched off', report.memory_off_targets || [], 'A room that several v1 rooms merge into has memory off when any one of them did.');
  list('Merged rooms', report.target_warnings || [], 'More than one v1 room lands in the same v2 room; their messages interleave by their v1 numbers.');
  list('Accounts created', (report.new_accounts || []).map(account => `${account.login} (${account.role}) for ${account.v1_name}`), 'Each is created without a password. After the import you get a one-time link per account to hand over.');
  list('Access through direct messages', report.dm_only_access || [], 'Nothing else in the backup puts these people in the project. Untick the chat to leave them out.');
  list('Direct messages not imported', (preview.rooms || []).filter(room => room.dm && mapping.rooms[room.id]?.skip).map(room => `${dmTitle(room, mapping)} · ${room.id}`));
  list('Direct messages opened', report.opened_dms || [], 'v2 has no archive for direct messages, so these import open.');
  list('Memory skipped', (report.skipped_memory || []).map(entry => `${entry.scope} · ${entry.scope_key} · ${entry.id}: ${entry.reason}`));
  list('Roles skipped', byKey(report.skipped_roles).map(([key, reason]) => `${key}: ${reason}`));
  list('Role conflicts', byKey(report.role_conflicts).map(([key, choice]) => `${key}: ${choice === 'use_v1' ? 'use v1 role' : 'keep v2 role'}`));
  list('Macro conflicts', byKey(report.macro_conflicts).map(([key, choice]) => `<${key}>: ${choice === 'use_v1' ? 'use v1 text' : 'keep v2 text'}`));
  list('Sounds skipped', Object.values(report.skipped_sounds || {}));
  list('Settings skipped', (report.skipped_settings || []).map(entry => `${entry.key}: ${entry.reason}`));
  list('Not carried over', byKey(report.not_carried).map(([name, count]) => `${name}: ${count} rows`), 'These records or fields remain only in the v1 backup on disk.');
  list('Files kept for v2', report.carried_over_files || []);
  return lists;
}
