// What the room list shows and in which order, as plain functions of the projects and rooms: the order of projects and rooms, the unread badge of
// a row, the read marks and the tree of rows. No DOM: the page draws what this decides.
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

// A project's system rooms: general, the routing room and any other flow room (advice, ...), as the server's `purpose` and `team_flow_room_key` say.
// They lead a project's rooms in that fixed order: general, routing, then the other flow rooms; task and research rooms follow in their own order.
export function systemRank(room) {
  if (room.kind === 'dm' || room.kind === 'home') return -1;
  // The server marks routing and advice rooms with purpose general too, so the general flow key must win before them.
  if (room.team_flow_room_key === 'general') return 0;
  if (room.team_flow_room_key === 'routing') return 1;
  if (room.team_flow_room_key) return 2;
  return room.kind === 'lobby' || room.purpose === 'general' ? 0 : -1;
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

// The unread badge of a room row. Only a project's home room and a direct message carry one, and each counts only its own unread messages, folded
// or not (no sum over the rooms nested under a home row). Every other room, at any depth (general, routing, advice, task and research rooms),
// shows none: what happens there is told by its status and by the 🔔 of the Inbox items that wait on the person. The open room counts as read.
export const tracksStatus = room => room.kind === 'task' && room.purpose !== 'general' && room.team_flow_room_key !== 'routing';
export function roomUnread(room, activeID) {
  if (room.id === activeID || room.lifecycle === 'closed') return 0;
  if (room.kind !== 'home' && room.kind !== 'dm') return 0;
  return Math.max(0, room.unread_count || 0);
}

// What kind of room a row is, as the server's `kind`, `purpose` and `agent_chat` say, never guessed from who is in it: the glyph (an icon-mask name) and the tooltip.
export function roomType(room) {
  if (room.kind === 'home') return { key: 'home', icon: 'house', label: 'Project home room' };
  if (room.kind === 'dm') return room.agent_chat ? { key: 'agent-chat', icon: 'switch', label: 'Chat between agents' } : { key: 'dm', icon: 'human', label: 'Direct message' };
  if (room.team_flow_room_key === 'routing') return { key: 'routing', icon: 'route', label: 'Routing room' };
  if (room.kind === 'lobby' || room.purpose === 'general') return { key: 'general', icon: 'hash', label: 'General room' };
  if (room.purpose === 'research') return { key: 'research', icon: 'search', label: 'Research room' };
  return { key: 'task', icon: 'list-checks', label: 'Task room' };
}

// The small status a task or research room shows instead of a count; null for any other room (and an archived one). Closing comes first, then a person
// being waited on (an open attention item, seen or not), then an agent working (a pending or running turn). What is left is idle; a research room that
// has been written in reads as done (the server has no separate report-delivered mark, so it is the quiet room after the work).
export function roomStatus(room) {
  if (!tracksStatus(room) || room.lifecycle === 'closed') return null;
  if (room.lifecycle === 'closing') return { key: 'closing', label: 'Closing…', title: 'This room is closing' };
  if ((room.attention_open || 0) > 0) return { key: 'waiting', label: 'Waiting', title: 'Waiting for a person: an item needs an answer' };
  if (room.working) return { key: 'working', label: 'Working', title: 'An agent in this room is working' };
  if (room.purpose === 'research' && (room.user_message_count || 0) > 0) return { key: 'done', label: 'Done', title: 'Quiet: no agent is working here' };
  return { key: 'idle', label: 'Idle', title: 'Nothing is running in this room' };
}

// Sends read marks with at most one request per room in flight. A mark asked for while one is in flight is kept (the
// highest seq wins; 0 means the room's newest) and sent when the first one settles, if it goes further than that one.
// reset() (sign-out) forgets everything, and a request from before it neither re-issues nor reports its result.
export class ReadMarker {
  constructor(post) { this.post = post; this.generation = 0; this.inFlight = new Map(); this.wanted = new Map(); }
  reset() { this.generation++; this.inFlight.clear(); this.wanted.clear(); }
  mark(roomID, seq, onState) {
    const further = (a, b) => a === 0 ? b !== 0 : b !== 0 && a > b;
    if (this.inFlight.has(roomID)) {
      const sent = this.inFlight.get(roomID), wanted = this.wanted.get(roomID);
      if (further(seq, sent) && (!wanted || further(seq, wanted.seq))) this.wanted.set(roomID, { seq, onState });
      return;
    }
    const generation = this.generation;
    this.inFlight.set(roomID, seq);
    Promise.resolve().then(() => this.post(roomID, seq)).then(state => { if (generation === this.generation) onState?.(state); }, () => {}).finally(() => {
      if (generation !== this.generation) return;
      this.inFlight.delete(roomID);
      const next = this.wanted.get(roomID); this.wanted.delete(roomID);
      if (next && further(next.seq, seq)) this.mark(roomID, next.seq, next.onState);
    }).catch(() => {}); // an onState that throws must not surface as an unhandled rejection
  }
}

export const AGENT_CHAT_PROJECT = 'prj_agentdm';

// Whether a room shows under the text typed in the filter: its title or its project's name contains it.
export function matchesRoom(room, filter, projects) {
  const text = filter.trim().toLocaleLowerCase();
  return !text || (room.title || '').toLocaleLowerCase().includes(text) || (projects.get(room.project_id)?.name || '').toLocaleLowerCase().includes(text);
}

export const activityOrder = (a, b) => (b.last_activity_at || '').localeCompare(a.last_activity_at || '');

// The rows that close a project's room tree: its archived rooms (narrowed by the project's own archive search) and its hidden ones.
function tailOf(project, all, archiveFilter) {
  const closed = all.filter(room => room.project_id === project.id && !room.hidden && room.lifecycle === 'closed' && room.kind !== 'dm');
  const hidden = all.filter(room => room.project_id === project.id && room.hidden);
  const nodes = [];
  if (closed.length) {
    const text = (archiveFilter.get(project.id) || '').trim().toLocaleLowerCase();
    nodes.push({ kind: 'archived', project, count: closed.length, rooms: text ? closed.filter(room => (room.title || '').toLocaleLowerCase().includes(text)) : closed });
  }
  if (hidden.length) nodes.push({ kind: 'hidden', project, rooms: hidden });
  return nodes;
}

// The tree of rows the list draws. `state` is { projects: Map, rooms: Map, filter, sort, manual, collapsed: Set, active, archiveFilter: Map, offers }.
// Every row is a view: { room, nest, chip, controls, children, tail, collapsible, collapsed, unread }.
export function treeOf(state) {
  const { projects, rooms, filter, sort, manual, collapsed, active, archiveFilter, offers } = state;
  // While the lists are still arriving (`pending`: projects with no list yet), a project is drawn as a placeholder in its place, and the projects that
  // would be folded the first time they are seen are drawn folded now (`provisional`, without saving it), so what lands later does not reflow the list:
  // the project holding the room the app means to open (`expected`) stays open.
  const pending = state.pending || new Set(), provisional = !!state.provisional, seen = state.seen || new Set(), expected = state.expected || '';
  const archivedProjects = [...projects.values()].filter(project => project.archived_at && !pending.has(project.id));
  const live = [...projects.values()].filter(project => !project.archived_at), many = live.length > 1;
  const all = [...rooms.values()].filter(room => !projects.get(room.project_id)?.archived_at), filtering = !!filter.trim();
  const shown = all.filter(room => !room.hidden && room.lifecycle !== 'closed' && matchesRoom(room, filter, projects));
  const direct = shown.filter(room => room.kind === 'dm').sort(activityOrder);
  const ordinary = shown.filter(room => room.kind !== 'dm');
  const homes = new Map();
  for (const room of all) if (room.kind === 'home' && room.lifecycle !== 'closed' && !room.hidden && !homes.has(room.project_id)) homes.set(room.project_id, room.id);
  const displayParentID = room => {
    const parent = room.parent_id && rooms.get(room.parent_id);
    if (parent && parent.project_id === room.project_id && parent.lifecycle !== 'closed' && !parent.hidden) return parent.id;
    return room.kind !== 'home' && room.kind !== 'dm' ? homes.get(room.project_id) || '' : '';
  };
  const tails = new Map(live.map(project => [project.id, filtering ? [] : tailOf(project, all, archiveFilter)]));
  const collapsible = new Set(), nested = new Set(), holdsActive = new Set(), tailed = new Set();
  // The open room and everything above it, however deep and whether or not the rooms in between are shown (archived or hidden ones count too).
  const holders = new Set();
  const focus = active || (provisional ? expected : '');
  for (let room = focus && rooms.get(focus), guard = 0; room && guard < 64; room = rooms.get(displayParentID(room)), guard++) holders.add(room.id);
  if (focus) holders.add(focus);
  // A room nested under a project row (opts.nest) or under another nested room (opts.deep) lists its open child rooms, at any depth, and
  // gets an expander when it has any. Only the project-level rows take part in the first-seen folding (`collapsible`, `nested`, `holdsActive`).
  const view = (room, opts) => {
    const expands = opts.nest || opts.deep;
    let children = expands && room.lifecycle !== 'closed' ? orderRooms([...rooms.values()].filter(other => displayParentID(other) === room.id && other.lifecycle !== 'closed' && !other.hidden && other.kind !== 'dm' && matchesRoom(other, filter, projects)), sort) : [];
    // Under a project's home row the system rooms lead, then a divider, then the rest (the divider only when both groups are there).
    let divider = -1;
    if (opts.nest && room.kind === 'home') {
      const rank = other => { const value = systemRank(other); return value < 0 ? Infinity : value; };
      children = children.map((child, index) => ({ child, index })).sort((a, b) => rank(a.child) - rank(b.child) || a.index - b.index).map(item => item.child);
      const system = children.filter(child => systemRank(child) >= 0).length;
      if (system > 0 && system < children.length) divider = system;
    }
    const tail = opts.tail || [];
    const kids = children.length + tail.length;
    if (opts.nest) nested.add(room.id);
    if (opts.nest && holders.has(room.id)) holdsActive.add(room.id);
    if (opts.nest && kids) collapsible.add(room.id);
    const folded = kids > 0 && (collapsed.has(room.id) || (provisional && opts.nest && !seen.has(room.id) && !holders.has(room.id)));
    const childViews = children.map(child => view(child, { chip: false, nest: false, deep: true }));
    return { room, nest: !!opts.nest, chip: !!opts.chip, controls: opts.controls || null, children: childViews, ...(divider > 0 ? { divider } : {}), tail, collapsible: expands && kids > 0, folded, unread: roomUnread(room, active) };
  };
  const place = (room, nest, extra) => {
    const tail = room.kind === 'home' && tails.get(room.project_id)?.length && !tailed.has(room.project_id) ? tails.get(room.project_id) : null;
    if (tail) tailed.add(room.project_id);
    return view(room, { chip: many, nest, tail, ...extra });
  };
  const sections = [];
  const ordered = orderProjects(live, all, sort, manual);
  for (const [index, project] of ordered.entries()) {
    if (pending.has(project.id) && !filtering) { sections.push({ placeholder: true, project }); continue; }
    const roots = orderRooms(ordinary.filter(room => room.project_id === project.id && (filtering || !displayParentID(room))), sort);
    for (const room of roots) sections.push(place(room, !filtering, room.kind === 'home' && !filtering ? { controls: { first: index === 0, last: index === ordered.length - 1 } } : {}));
    // A project with no visible home room still gets its archived and hidden rows, after its rooms.
    if (!tailed.has(project.id)) sections.push(...tails.get(project.id).map(node => ({ tailOnly: true, node })));
  }
  const archivedShown = [];
  for (const project of archivedProjects) {
    const visible = [...rooms.values()].filter(room => room.project_id === project.id && !room.hidden);
    const matching = visible.filter(room => matchesRoom(room, filter, projects));
    const home = visible.find(room => room.kind === 'home');
    const own = (filtering && matching.length && home && !matching.includes(home) ? [home, ...matching] : matching)
      .sort((a, b) => (a.kind === 'home' ? 0 : 1) - (b.kind === 'home' ? 0 : 1) || (a.title || '').localeCompare(b.title || ''));
    if (!filtering || own.length) archivedShown.push({ project, rooms: own });
  }
  const text = filter.trim().toLocaleLowerCase();
  const joinable = (offers || []).filter(offer => !text || (offer.title || '').toLocaleLowerCase().includes(text));
  return { filtering, many, sections, noMatch: filtering && !shown.length && !pending.size ? `No rooms match “${filter.trim()}”.` : '', archived: archivedShown, direct, offers: joinable, collapsible, nested, holdsActive, closedProjects: live.filter(project => !all.some(room => room.project_id === project.id && room.lifecycle === 'closed' && !room.hidden && room.kind !== 'dm')).map(project => project.id) };
}

// A row with nothing under it (the rooms of a tail, an archived project's rooms, a direct message).
export function leafView(room, active, chip = false) {
  return { room, nest: false, chip, controls: null, children: [], tail: [], collapsible: false, folded: false, unread: roomUnread(room, active) };
}
