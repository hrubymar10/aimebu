// The pure part of the import wizard's Rooms and Direct messages pages: how a room's "Import into" choice reads and is applied, and
// the tree of v2 rooms a mapping would make. No DOM and no network, so a test can run it in a vm.

export const GENERAL = 'general';

// A choice is written 'kind:<project>' with the project alias URI-encoded, so an alias that holds ':' or '|' (a mapping file may use any
// text) can never be read as something else. choiceValue builds one and parseChoice takes it apart.
export function choiceValue(kind, alias) { return `${kind}:${encodeURIComponent(alias)}`; }
export function parseChoice(value) {
  const at = value.indexOf(':');
  if (at < 0) return { kind: value, alias: '' };
  let alias = '';
  try { alias = decodeURIComponent(value.slice(at + 1)); } catch { alias = ''; }
  return { kind: value.slice(0, at), alias };
}

// roomChoice is the "Import into" value of a v1 room: 'skip', or 'root', 'general', 'new' (a sub-room named like the v1 room) or
// 'custom' (a sub-room with another title, which the wizard cannot offer but a saved mapping may hold) written with choiceValue.
export function roomChoice(spec, roomId) {
  if (!spec || spec.skip || !spec.project) return 'skip';
  if (spec.target === 'root') return choiceValue('root', spec.project);
  if (spec.target === 'sub') {
    const title = (spec.title || GENERAL).trim() || GENERAL;
    if (title === GENERAL) return choiceValue('general', spec.project);
    return choiceValue(title === roomId ? 'new' : 'custom', spec.project);
  }
  return choiceValue('new', spec.project); // the old "separate room" target is a sub-room named like the v1 room
}

// applyChoice writes a chosen value into a room spec, keeping its lifecycle.
export function applyChoice(spec, value, roomId) {
  const { kind, alias: project } = parseChoice(value);
  if (kind === 'skip' || !project) { spec.skip = true; return spec; }
  spec.skip = false; spec.project = project;
  if (kind === 'root') { spec.target = 'root'; delete spec.title; }
  else if (kind === 'general') { spec.target = 'sub'; spec.title = GENERAL; }
  else if (kind === 'new') { spec.target = 'sub'; spec.title = roomId; }
  return spec; // 'custom' keeps the title it already has
}

// planTree turns a mapping into what it makes: for each project its root room and sub-rooms with the v1 rooms (and their message
// counts) that land there, the direct messages that will be imported, and what is dropped and why. rooms are the preview's
// { id, messages, dm } entries.
export function planTree(mapping, rooms) {
  const projects = new Map(Object.entries(mapping.projects || {}).map(([alias, project]) => [alias, { alias, name: project.name || alias, root: { rooms: [], messages: 0, memoryOff: [] }, subs: new Map() }]));
  const dropped = [], direct = [];
  for (const room of rooms) {
    const spec = mapping.rooms?.[room.id] || {};
    const messages = room.messages || 0;
    if (room.dm) {
      if (!spec.skip) direct.push({ id: room.id, messages });
      continue;
    }
    if (spec.skip) { dropped.push({ id: room.id, messages, reason: 'not imported' }); continue; }
    const project = projects.get(spec.project || mapping.default_project || '');
    if (!project) { dropped.push({ id: room.id, messages, reason: 'no project' }); continue; }
    if (spec.target === 'root') { project.root.rooms.push(room.id); project.root.messages += messages; if (room.memory_disabled) project.root.memoryOff.push(room.id); continue; }
    const title = spec.target === 'sub' ? ((spec.title || GENERAL).trim() || GENERAL) : room.id;
    if (!project.subs.has(title)) project.subs.set(title, { title, rooms: [], messages: 0, archived: true, memoryOff: [] });
    const sub = project.subs.get(title);
    sub.rooms.push(room.id); sub.messages += messages;
    if (room.memory_disabled) sub.memoryOff.push(room.id); // memory is off for the target when any room merged into it had it off
    if (spec.lifecycle === 'open') sub.archived = false; // a shared sub-room is open when any of its rooms is
  }
  return {
    projects: [...projects.values()].map(project => ({ ...project, subs: [...project.subs.values()] })).filter(project => project.root.rooms.length || project.subs.length),
    direct, dropped,
  };
}

// mergeGroups lists the targets that more than one v1 room lands in, for the warning above the room list.
export function mergeGroups(tree) {
  const groups = [];
  for (const project of tree.projects) {
    if (project.root.rooms.length > 1) groups.push({ where: `the root of ${project.name}`, rooms: project.root.rooms });
    for (const sub of project.subs) if (sub.rooms.length > 1) groups.push({ where: `"${sub.title}" under ${project.name}`, rooms: sub.rooms });
  }
  return groups;
}

// slugify makes a project alias from a name the owner typed, unique among the existing aliases.
export function newProjectAlias(name, existing) {
  const base = (name || '').trim().toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '') || 'project';
  let alias = base, n = 2;
  while (existing.has(alias)) alias = `${base}-${n++}`;
  return alias;
}
