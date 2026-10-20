// How the room list orders its projects and the rooms inside them. No DOM: the sidebar renders what this decides.
//
// Projects are ordered by the chosen sort: name, latest activity, or the person's own manual order (nothing is pinned). Rooms inside
// a project follow the same sort, except that a project's home room always leads and "manual" leaves them in the order the server
// lists them (only projects can be moved).
export const SORTS = [['name', 'By name'], ['activity', 'By latest activity'], ['manual', 'Manual (drag projects)']];
export const DEFAULT_SORT = 'manual';

const byName = (a, b) => String(a.name ?? a.title ?? '').localeCompare(String(b.name ?? b.title ?? ''), undefined, { numeric: true, sensitivity: 'base' });
const latest = (a, b) => String(b.last_activity_at || '').localeCompare(String(a.last_activity_at || ''));

// The newest activity among a project's rooms, '' for a project with none.
export function projectActivity(project, rooms) {
  let newest = '';
  for (const room of rooms) if (room.project_id === project.id && room.kind !== 'dm' && (room.last_activity_at || '') > newest) newest = room.last_activity_at;
  return newest;
}

// `projects` arrive in creation order. `manual` is the saved list of project ids; ids it does not know (new projects) follow the
// known ones in creation order, and ids that no longer exist are ignored.
const rankOf = manual => { const rank = new Map(manual.map((id, index) => [id, index])); return id => rank.has(id) ? rank.get(id) : Infinity; };
export function orderProjects(projects, rooms, sort, manual = []) {
  const rank = rankOf(manual);
  const position = new Map(projects.map((project, index) => [project.id, index]));
  const activity = new Map(projects.map(project => [project.id, projectActivity(project, rooms)]));
  const compare = sort === 'name' ? byName
    : sort === 'activity' ? (a, b) => (activity.get(b.id) || '').localeCompare(activity.get(a.id) || '') || byName(a, b)
      : (a, b) => rank(a.id) - rank(b.id);
  return [...projects].sort((a, b) => {
    const order = compare(a, b);
    return (order && !Number.isNaN(order) ? order : 0) || position.get(a.id) - position.get(b.id);
  });
}

// Every project id in manual order: what a move edits (ids the saved order does not know follow it, in creation order).
export function manualSequence(projects, manual = []) {
  const rank = rankOf(manual), position = new Map(projects.map((project, index) => [project.id, index]));
  return [...projects].sort((a, b) => (rank(a.id) - rank(b.id) || 0) || position.get(a.id) - position.get(b.id)).map(project => project.id);
}

// Rooms that are not nested under a project row, or the rooms nested under one (home room first, then the sort).
export function orderRooms(rooms, sort) {
  const compare = sort === 'name' ? byName : sort === 'activity' ? latest : () => 0;
  return [...rooms].sort((a, b) => (a.kind === 'home' ? 0 : 1) - (b.kind === 'home' ? 0 : 1) || compare(a, b));
}

// The whole manual order after `id` moves one step up (-1) or down (+1).
export function moveProject(projects, manual, id, delta) {
  const sequence = manualSequence(projects, manual);
  const target = sequence[sequence.indexOf(id) + delta];
  return sequence.includes(id) && target ? placeProject(projects, manual, id, target, delta > 0) : manual;
}

// Moves `id` before (or after) `targetID` in the manual order.
export function placeProject(projects, manual, id, targetID, after) {
  const known = new Set(projects.map(project => project.id));
  if (!known.has(id) || !known.has(targetID) || id === targetID) return manual;
  const sequence = manualSequence(projects, manual).filter(other => other !== id);
  sequence.splice(sequence.indexOf(targetID) + (after ? 1 : 0), 0, id);
  return sequence;
}
