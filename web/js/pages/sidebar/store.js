import { batch, signal } from '../../ui.js';
import { request, currentUser } from '../../api.js';
import { savePreference } from '../../preferences.js';
import { noticeDialog } from '../../dialogs.js';
import { AGENT_CHAT_PROJECT, DEFAULT_SORT, ReadMarker, SORTS, moveProject, orderProjects, placeProject } from './model.js';

const collapsedKey = 'aimebu2.rooms.collapsed';
// The roots this browser has already shown with children. A root is folded the first time it is seen (a project starts collapsed), after which
// the person's own folding is all there is: the folded set remembers it, and a root they opened is not folded again.
const seenKey = 'aimebu2.rooms.seen';
// The sort and the manual project order are the person's own preferences (they follow them across devices); the browser keeps a cache under these
// keys, filled from the server at sign-in (see preferences.js).
const sortKey = () => `aimebu2.ui.sidebar_sort.${currentUser}`, orderKey = () => `aimebu2.ui.sidebar_project_order.${currentUser}`;
// Whether every room row shows its last message ('1'), or only the room that is open does (the default).
const previewKey = () => `aimebu2.ui.sidebar_preview.${currentUser}`;
export const loadPreviewAll = () => { try { return localStorage.getItem(previewKey()) === '1'; } catch { return false; } };
function loadSort() { try { const value = localStorage.getItem(sortKey()); return SORTS.some(([key]) => key === value) ? value : DEFAULT_SORT; } catch { return DEFAULT_SORT; } }
function loadManual() { try { const list = JSON.parse(localStorage.getItem(orderKey()) || '[]'); return Array.isArray(list) ? list.filter(id => typeof id === 'string') : []; } catch { return []; } }
function loadSeen() { try { const list = JSON.parse(localStorage.getItem(seenKey) || '[]'); return new Set(Array.isArray(list) ? list.filter(id => typeof id === 'string') : []); } catch { return new Set(); } }
function loadCollapsed() { try { const list = JSON.parse(localStorage.getItem(collapsedKey) || '[]'); return new Set(Array.isArray(list) ? list.filter(id => typeof id === 'string') : []); } catch { return new Set(); } }

// The projects and rooms of the person, as the list draws them. `rooms` and `projects` stay Maps (the app reads them), and `version` moves with every
// change, so the page draws again; a refresh keeps the record it already holds for a room whose data is unchanged. Every read carries the generation
// of the list it was asked in (a reset or destroy moves it) and every call and search a ticket of its own, so an answer for a list that is gone, or
// for a read that was passed by a newer one, changes nothing.
export class SidebarStore {
  constructor({ onSelect }) {
    this.onSelect = onSelect;
    this.projects = new Map(); this.rooms = new Map(); this.refreshTimers = new Map();
    this.version = signal(0);
    this.shown = signal(false); this.collapsible = signal(new Set()); this.seenHealth = new Set();
    this.filter = signal(''); this.sort = signal(DEFAULT_SORT); this.manual = signal([]); this.active = signal('');
    // The open room the person marked unread: it is not marked read again, and it shows its count, until they leave it or mark it read.
    this.held = signal('');
    this.collapsedRooms = loadCollapsed(); this.seenRooms = loadSeen(); this.revealedFor = '';
    this.health = new Map(); this.joinable = []; this.agentChatProject = '';
    this.folds = new Map(); this.archiveFilter = new Map(); this.archiveHits = new Map();
    this.roomRequests = new Set(); this.listSeqs = new Map(); this.archiveTimers = new Map(); this.archiveRequests = new Map(); this.saves = new Map();
    this.generation = 0; this.loadedUser = null; this.dragging = null; this.alive = true;
    // A cold load draws the list before the app has said which room it opens: the project holding that room must not be folded on the way.
    // No project is folded until a room is made active, or the app says there is none to open (`releaseFolds`).
    this.holdFolds = true;
    // Projects whose room list has not arrived on a cold load: drawn as placeholders in their final place. `expected` is the room the app means to open,
    // so the project that holds it is not drawn folded while the rest arrives.
    this.pending = new Set(); this.expected = []; this.touching = false;
    this.reads = new ReadMarker((roomID, seq) => request(`/api/rooms/${roomID}/read`, { method: 'POST', body: seq > 0 ? { seq } : {} }));
  }

  touch() { this.shown.value = true; this.version.value++; }
  // Lists that arrive in the same turn are drawn together: one draw however many landed.
  touchSoon(generation) {
    if (this.touching) return;
    this.touching = true;
    queueMicrotask(() => { this.touching = false; if (this.owns(generation)) this.touch(); });
  }
  // The room a start with nothing to restore opens: the first room of the first project, in the server's project order. The lists land in whatever
  // order the network gives them, so the first room in `rooms` is not it.
  firstRoom() {
    for (const project of this.projects.values()) for (const room of this.rooms.values()) if (room.project_id === project.id) return room;
    return [...this.rooms.values()][0];
  }
  // The rooms the app will try to open, in its order (the one in the address, then the last one). The project holding the first of them that exists is
  // not drawn folded while the lists arrive; when none of them exists, or there is none to try, the app opens the first room, and that is the one.
  expect(...ids) { this.expected = ids.flat().filter(Boolean); }
  focusRoom() {
    for (const id of this.expected) if (this.rooms.has(id)) return id;
    if (this.expected.length && this.pending.size) return ''; // one of them may still be in a list that has not landed
    // The first room, once every project before it has said what it has: a project still waiting could hold the first one.
    for (const project of this.projects.values()) {
      if (this.pending.has(project.id)) return '';
      for (const room of this.rooms.values()) if (room.project_id === project.id) return room.id;
    }
    return '';
  }
  owns(generation) { return this.alive && generation === this.generation; }

  reset() {
    this.reads.reset(); this.readQueue?.clear();
    this.generation++;
    clearTimeout(this.muteTimer);
    for (const timer of this.refreshTimers.values()) clearTimeout(timer);
    this.refreshTimers.clear();
    for (const timer of this.archiveTimers.values()) clearTimeout(timer);
    for (const controller of this.archiveRequests.values()) controller.abort();
    for (const controller of this.roomRequests) controller.abort();
    this.roomRequests.clear();
    for (const cache of [this.projects, this.rooms, this.health, this.folds, this.archiveFilter, this.archiveHits, this.archiveTimers, this.archiveRequests, this.listSeqs]) cache.clear();
    this.joinable = []; this.agentChatProject = ''; this.chatSeq = null; this.loadedUser = null; this.dragging = null;
    this.holdFolds = true; this.pending.clear(); this.expected = []; this.revealedFor = '';
    batch(() => { this.active.value = ''; this.held.value = ''; this.filter.value = ''; this.shown.value = false; this.collapsible.value = new Set(); this.seenHealth.clear(); this.version.value++; });
  }

  // Every call and read of the list goes through here: a destroyed list sends nothing, and a reset aborts what is out.
  async roomRequest(path, options = {}) {
    if (!this.alive) throw new DOMException('the list was destroyed', 'AbortError');
    const controller = new AbortController();
    this.roomRequests.add(controller);
    try { return await request(path, { ...options, signal: controller.signal }); }
    finally { this.roomRequests.delete(controller); }
  }

  // Reads of one project's room list can finish out of order within a session. A read that started before a newer one was applied is dropped, so a
  // room created in between is not removed by the older, shorter list.
  listTicket(projectID) { const state = this.listSeqs.get(projectID) || { issued: 0, applied: 0 }; state.issued++; this.listSeqs.set(projectID, state); return state.issued; }
  listCurrent(projectID, ticket) { const state = this.listSeqs.get(projectID); if (!state || ticket < state.applied) return false; state.applied = ticket; return true; }

  // The same for the chats between agents: a read that started before a newer one was applied is dropped. A join or a leave is followed by a read of
  // this list, and two of them can finish out of order; the older answer, which does not name a chat that was just joined, must not remove it.
  chatTicket() { this.chatSeq = this.chatSeq || { issued: 0, applied: 0 }; return ++this.chatSeq.issued; }
  chatCurrent(ticket) { if (ticket < this.chatSeq.applied) return false; this.chatSeq.applied = ticket; return true; }

  // A project's list names the rooms it has: a cached room of the project that the list no longer names was removed (or taken away).
  dropUnlisted(projectID, list) {
    const listed = new Set(list.map(room => room.id));
    for (const [id, room] of [...this.rooms]) if (room.project_id === projectID && !listed.has(id)) this.rooms.delete(id);
  }

  // The record the list holds for a room stays when the data is the same.
  keep(room) {
    const held = this.rooms.get(room.id);
    if (held && JSON.stringify(held) === JSON.stringify(room)) return held;
    this.rooms.set(room.id, room);
    return room;
  }

  isAgentChatProject(id) { return !!id && (id === AGENT_CHAT_PROJECT || id === this.agentChatProject); }

  // The chats between agents the person has joined, and the ones they could join. A room that is no longer listed (left, or taken away) leaves.
  applyAgentChats(data, mark = true) {
    if (!data?.project_id) return;
    this.agentChatProject = data.project_id;
    const listed = new Set((data.rooms || []).map(room => room.id));
    for (const [id, room] of [...this.rooms]) if (room.project_id === data.project_id && !listed.has(id)) this.rooms.delete(id);
    for (const room of data.rooms || []) { const kept = this.keep(room); if (mark && room.id === this.active.peek() && !this.holdRead?.(room.id)) this.markSeen(kept); }
    this.joinable = data.joinable || [];
  }

  // The list is drawn as its reads arrive. The projects come first, and with them their final order (the manual or by-name order needs no room), so each
  // project is drawn as a placeholder in its place and replaced by its rooms when its own list lands: nothing above or below it moves. By latest
  // activity the order depends on the rooms, so nothing is drawn until every list is in.
  async load() {
    const generation = this.generation;
    let projects, chats = null, chatsTicket = 0;
    // The order first: the streamed list is drawn in it from the start.
    batch(() => { this.sort.value = loadSort(); this.manual.value = loadManual(); });
    const streaming = this.sort.peek() !== 'activity';
    try {
      // The chats between agents belong to no project, so they come from their own list (a failure there leaves the project rooms usable).
      chatsTicket = this.chatTicket();
      const chatsRequest = this.roomRequest('/api/agent-dms').catch(() => null);
      projects = await this.roomRequest('/api/projects');
      if (!this.owns(generation)) return [];
      for (const project of projects) {
        this.projects.set(project.id, project);
        // A project with nothing drawn yet waits as a placeholder; one the list already holds (a resync) keeps what it shows until its new list lands.
        if (streaming && ![...this.rooms.values()].some(room => room.project_id === project.id)) this.pending.add(project.id);
      }
      if (streaming && this.pending.size) this.touch();
      // One request per project, all at once: waiting for each in turn made the first screen take the sum of them. Each list is applied as it lands.
      const tickets = projects.map(project => this.listTicket(project.id));
      await Promise.all(projects.map(async (project, index) => {
        const list = await this.roomRequest(`/api/projects/${project.id}/rooms`);
        if (!this.owns(generation)) return;
        this.pending.delete(project.id);
        // A project taken away while its list was out does not come back with it.
        if (!this.listSeqs.has(project.id)) return;
        this.projects.set(project.id, project);
        if (this.listCurrent(project.id, tickets[index])) { this.dropUnlisted(project.id, list); for (const room of list) this.keep(room); }
        if (streaming) this.touchSoon(generation);
      }));
      chats = await chatsRequest;
      if (!this.owns(generation)) return [];
    } catch (error) {
      if (!this.owns(generation)) return [];
      // What did arrive stays drawn; the projects still waiting are not drawn as placeholders for ever.
      this.pending.clear();
      if (this.shown.peek()) this.touch();
      throw error;
    }
    this.pending.clear();
    const listed = new Set(projects.map(project => project.id));
    // A project that is no longer listed (deleted, or the person was removed from it) leaves with its rooms.
    for (const id of new Set([...this.projects.keys(), ...[...this.rooms.values()].map(room => room.project_id)])) {
      if (!listed.has(id) && !this.isAgentChatProject(id)) this.forgetProject(id);
    }
    if (this.chatCurrent(chatsTicket)) this.applyAgentChats(chats);
    this.loadedUser = currentUser;
    this.touch();
    return projects;
  }

  async addProject(project) {
    const generation = this.generation, ticket = this.listTicket(project.id);
    let list;
    try { list = await this.roomRequest(`/api/projects/${project.id}/rooms`); } catch (error) { if (!this.owns(generation)) return []; throw error; }
    if (!this.owns(generation)) return [];
    this.projects.set(project.id, project);
    if (this.listCurrent(project.id, ticket)) { this.dropUnlisted(project.id, list); for (const room of list) this.keep(room); }
    this.touch();
    return list;
  }

  async refreshAgentChats({ mark = true } = {}) {
    const generation = this.generation, ticket = this.chatTicket();
    let data;
    try { data = await this.roomRequest('/api/agent-dms'); } catch (error) { if (!this.owns(generation)) return; throw error; }
    if (!this.owns(generation) || !this.chatCurrent(ticket)) return;
    this.applyAgentChats(data, mark); this.touch();
  }

  // `mark: false` reads the list without marking the open room read: the refresh that follows the person's own read position moving (another tab
  // or device marked a room read or unread) must not undo a mark unread made there by reading the room here at once.
  async refreshProject(id, { mark = true } = {}) {
    if (this.isAgentChatProject(id)) return this.refreshAgentChats({ mark });
    const generation = this.generation, ticket = this.listTicket(id);
    let list;
    try { list = await this.roomRequest(`/api/projects/${id}/rooms`); } catch (error) { if (!this.owns(generation)) return; throw error; }
    if (!this.listCurrent(id, ticket)) return;
    this.dropUnlisted(id, list);
    for (const room of list) { const kept = this.keep(room); if (mark && room.id === this.active.peek() && !this.holdRead?.(room.id)) this.markSeen(kept); }
    this.touch();
  }

  // Refreshes a project's rooms once, a moment from now, however many times it is asked for in between: a turn raises several events in a row and each
  // would otherwise refetch the whole list.
  scheduleRefresh(id, delay = 250) {
    if (this.refreshTimers.has(id)) return;
    const generation = this.generation;
    this.refreshTimers.set(id, setTimeout(() => {
      this.refreshTimers.delete(id);
      if (this.owns(generation)) this.refreshProject(id).catch(() => {});
    }, delay));
  }

  // A room removed on the server is dropped here by name: a project refresh only adds and updates.
  forgetRoom(id) { if (this.rooms.delete(id)) this.touch(); }

  // A project that was deleted: it and every one of its rooms leave the list.
  forgetProject(id) {
    this.listSeqs.delete(id); // a read of its rooms still in flight is then dropped when it lands
    for (const [roomID, room] of [...this.rooms]) if (room.project_id === id) this.rooms.delete(roomID);
    this.projects.delete(id);
    this.folds.delete(`archived-project:${id}`);
    this.dropArchive(id);
    this.touch();
  }

  // Selecting does not mark read by itself: the chat marks the room once it has rendered the latest page (Chat.open).
  // Leaving a room the person marked unread lets it go: it stays unread, and is read as usual when it is opened again.
  setActive(id) { this.holdFolds = false; batch(() => { this.shown.value = true; this.active.value = id; if (this.held.peek() !== id) this.held.value = ''; }); }

  // The app opened no room (none to restore): the projects may fold now.
  releaseFolds() { if (!this.holdFolds) return; this.holdFolds = false; this.version.value++; }

  unreadCount(room) { return Math.max(0, room.unread_count || 0); }

  // Read position lives on the server so every device shares it. Only the open room is marked read, and only up to the newest message its chat has
  // rendered (shownSeq), so no path can move the cursor past what the reader saw. The unread count left after the mark comes back from the server.
  markSeen(room) {
    if (!room || !this.alive || this.held.peek() === room.id) return;
    const seq = this.shownSeq ? this.shownSeq(room.id) : 0;
    // A room set to mentions only has nothing to count for most of what it shows, yet it is read all the same: the position moves on.
    if (!this.unreadCount(room) && !room.attention_count && !(room.notify === 'mentions' && seq > (room.read_cursor || 0))) return;
    if (this.shownSeq && !(seq > 0) && !room.attention_count) return;
    this.reads.mark(room.id, seq, state => {
      if (this.rooms.get(room.id) !== room) return;
      if (Number.isInteger(state?.unread_count)) room.unread_count = state.unread_count;
      if (Number.isInteger(state?.unread_addressed)) room.unread_addressed = state.unread_addressed;
      if (Number.isInteger(state?.read_cursor)) room.read_cursor = state.read_cursor;
      if (Number.isInteger(state?.attention_count)) room.attention_count = state.attention_count;
      this.touch();
    });
  }

  // The counts the server answered for a read position, written into the record the list holds for the room now.
  applyReadState(roomID, state) {
    const room = this.rooms.get(roomID);
    if (!room || !state) return;
    for (const field of ['unread_count', 'unread_addressed', 'read_cursor', 'attention_count']) if (Number.isInteger(state[field])) room[field] = state[field];
    this.touch();
  }

  // Mark as read and Mark unread, from a row's or a message's menu. Each waits for the automatic marks already on their way for the room (and for
  // the previous one of these), so the server applies them in the order the person made them. Mark as read reads up to the newest message. Mark
  // unread moves the person's own cursor back to just before message `seq` (the newest message when 0); in the open room it also holds the room
  // unread until the person leaves it (`held`), and a refused mark lets the room go again.
  markRead(room) { return this.moveRead(room, 'read', 0); }
  markUnread(room, seq = 0) { return this.moveRead(room, 'unread', seq); }
  moveRead(room, kind, seq) {
    if (!room || !this.alive) return Promise.resolve(false);
    const generation = this.generation, id = room.id;
    if (kind === 'unread' && this.active.peek() === id) this.held.value = id;
    if (kind === 'read' && this.held.peek() === id) this.held.value = '';
    this.readQueue ||= new Map();
    const run = async () => {
      await this.reads.idle(id);
      if (!this.owns(generation)) return false;
      try {
        const state = await this.roomRequest(`/api/rooms/${id}/${kind}`, { method: 'POST', body: seq > 0 ? { seq } : {} });
        if (!this.owns(generation)) return false;
        this.applyReadState(id, state);
        return true;
      } catch (error) {
        if (!this.owns(generation)) return false;
        if (kind === 'unread' && this.held.peek() === id) this.held.value = '';
        void noticeDialog(error.message, { title: kind === 'read' ? 'Could not mark the room read' : 'Could not mark the room unread' });
        return false;
      }
    };
    const next = (this.readQueue.get(id) || Promise.resolve()).then(run, run);
    this.readQueue.set(id, next);
    return next;
  }

  async setPreference(room, field, value) {
    const generation = this.generation;
    try {
      await this.roomRequest(`/api/rooms/${room.id}/preferences`, { method: 'PATCH', body: { [field]: value } });
      if (!this.owns(generation)) return false;
      await this.refreshProject(room.project_id);
      return this.owns(generation);
    } catch (error) { if (this.owns(generation)) void noticeDialog(error.message, { title: 'Could not change the room' }); return false; }
  }

  setHealth(agents) {
    this.health.clear();
    for (const agent of agents) for (const id of agent.room_ids || []) { this.health.set(id, [...(this.health.get(id) || []), agent]); this.seenHealth.add(id); }
    this.touch();
  }

  // A mute ends by itself: the list is drawn again when the first running one runs out, so its bell goes without a reload.
  scheduleMuteEnd() {
    clearTimeout(this.muteTimer);
    const now = Date.now();
    const ends = [...this.rooms.values()].map(room => Date.parse(room.muted_until || '')).filter(end => end > now);
    if (!ends.length) return;
    this.muteTimer = setTimeout(() => this.touch(), Math.min(Math.min(...ends) - now + 250, 2 ** 31 - 1));
  }

  // ----- the choices of the person: sort, manual order, folds -----
  // A save still queued when the list is reset or destroyed, or one a newer save of the same name replaced, is not sent.
  persist(name, value) {
    const generation = this.generation, ticket = (this.saves.get(name) || 0) + 1;
    this.saves.set(name, ticket);
    return savePreference(name, value, { live: () => this.owns(generation) && this.saves.get(name) === ticket });
  }

  setManual(order) { this.manual.value = order; try { localStorage.setItem(orderKey(), JSON.stringify(order)); } catch { /* private mode */ } this.persist('sidebar_project_order', JSON.stringify(order)); }
  moveProject(id, delta) { this.setManual(moveProject([...this.projects.values()], this.manual.peek(), id, delta)); }
  dropProject(id, targetID, after) { this.setManual(placeProject([...this.projects.values()], this.manual.peek(), id, targetID, after)); }

  setSort(sort) {
    if (!SORTS.some(([key]) => key === sort)) return;
    // Manual keeps the order the list shows now, so choosing it after another sort starts from what the person sees.
    if (sort === 'manual' && this.sort.peek() !== 'manual') this.manual.value = orderProjects([...this.projects.values()], [...this.rooms.values()], this.sort.peek(), this.manual.peek()).map(project => project.id);
    this.sort.value = sort;
    try { localStorage.setItem(sortKey(), sort); if (sort === 'manual') localStorage.setItem(orderKey(), JSON.stringify(this.manual.peek())); } catch { /* private mode */ }
    this.persist('sidebar_sort', sort); if (sort === 'manual') this.persist('sidebar_project_order', JSON.stringify(this.manual.peek()));
  }

  // Folds the roots with children that have not been seen before, so a project starts collapsed; answers whether it folded any (the list is drawn
  // again then). `nested` is every root on screen, with or without children: a project first seen with no rooms is open, and stays open when its first
  // room is added, so the room someone just made is not hidden from them. Nothing is folded again once seen.
  foldNew(collapsible, nested, holdsActive = new Set()) {
    let folded = false, changed = false;
    for (const id of nested) {
      if (this.seenRooms.has(id)) continue;
      this.seenRooms.add(id); changed = true;
      // The project that holds the open room is seen but not folded: its row would vanish from under the person.
      if (collapsible.has(id) && !holdsActive.has(id)) { this.collapsedRooms.add(id); folded = true; }
    }
    if (changed) try { localStorage.setItem(seenKey, JSON.stringify([...this.seenRooms])); localStorage.setItem(collapsedKey, JSON.stringify([...this.collapsedRooms])); } catch { /* private mode */ }
    return folded;
  }

  // A room that becomes the open one is shown: every folded room above it (its project and the rooms in between) opens, once for each time it becomes the open
  // room, so the person's own fold of that project afterwards stays. A room not loaded yet is tried again when the list is drawn next. Answers whether it opened any.
  revealActive(id) {
    if (!id || this.revealedFor === id) return false;
    const room = this.rooms.get(id);
    if (!room) return false;
    this.revealedFor = id;
    let changed = false;
    for (let above = room.parent_id && this.rooms.get(room.parent_id), guard = 0; above && guard < 64; above = above.parent_id && this.rooms.get(above.parent_id), guard++) if (this.collapsedRooms.delete(above.id)) changed = true;
    if (changed) try { localStorage.setItem(collapsedKey, JSON.stringify([...this.collapsedRooms])); } catch { /* private mode */ }
    return changed;
  }

  toggleCollapsed(id) {
    if (this.collapsedRooms.has(id)) this.collapsedRooms.delete(id); else this.collapsedRooms.add(id);
    try { localStorage.setItem(collapsedKey, JSON.stringify([...this.collapsedRooms])); } catch { /* private mode */ }
    this.touch();
  }

  toggleAll(ids) {
    const all = ids.size > 0 && [...ids].every(id => this.collapsedRooms.has(id));
    if (all) for (const id of ids) this.collapsedRooms.delete(id); else for (const id of ids) this.collapsedRooms.add(id);
    try { localStorage.setItem(collapsedKey, JSON.stringify([...this.collapsedRooms])); } catch { /* private mode */ }
    this.touch();
  }

  setFold(key, open) { this.folds.set(key, open); this.touch(); }

  setFilter(text) { batch(() => { this.shown.value = true; this.filter.value = text; }); }

  // ----- the archive search of one project -----
  dropArchive(projectID) {
    clearTimeout(this.archiveTimers.get(projectID)); this.archiveTimers.delete(projectID);
    this.archiveRequests.get(projectID)?.abort(); this.archiveRequests.delete(projectID);
    this.archiveFilter.delete(projectID); this.archiveHits.delete(projectID);
  }

  searchArchive(projectID, text) {
    if (!this.alive) return;
    this.archiveFilter.set(projectID, text);
    clearTimeout(this.archiveTimers.get(projectID));
    this.archiveRequests.get(projectID)?.abort();
    const query = text.trim();
    this.archiveHits.delete(projectID);
    this.touch();
    if (!query) return;
    this.archiveTimers.set(projectID, setTimeout(async () => {
      const controller = new AbortController();
      this.archiveRequests.set(projectID, controller);
      const current = () => !controller.signal.aborted;
      try {
        const hits = await request(`/api/projects/${projectID}/archive/search?q=${encodeURIComponent(query)}`, { signal: controller.signal });
        if (current()) { this.archiveHits.set(projectID, { hits }); this.touch(); }
      } catch (error) {
        if (current() && error.name !== 'AbortError') { this.archiveHits.set(projectID, { error: true }); this.touch(); }
      } finally {
        if (this.archiveRequests.get(projectID) === controller) this.archiveRequests.delete(projectID);
      }
    }, 250));
  }

  async openHit(hit) {
    const generation = this.generation;
    let room = this.rooms.get(hit.room_id);
    if (!room) {
      try { room = await this.roomRequest(`/api/rooms/${hit.room_id}`); if (!this.owns(generation)) return; this.rooms.set(room.id, room); } catch { return; }
    }
    await this.onSelect(room, hit.message_id);
  }

  // ----- calls that change a room -----
  async reopen(room) {
    const generation = this.generation;
    try {
      const updated = await this.roomRequest(`/api/rooms/${room.id}/reopen`, { method: 'POST', body: { lifecycle_rev: room.lifecycle_rev || 0 } });
      if (!this.owns(generation)) return false;
      this.rooms.set(updated.id, updated); await this.refreshProject(room.project_id);
      if (!this.owns(generation)) return false;
      if (this.active.peek() === room.id) this.onSelect(updated);
      return true;
    } catch (error) { if (this.owns(generation)) { void noticeDialog(error.message, { title: 'Could not reopen the room' }); return false; } return false; }
  }

  async join(offer) {
    const generation = this.generation;
    try {
      await this.roomRequest(`/api/rooms/${offer.id}/join`, { method: 'POST' });
      if (!this.owns(generation)) return;
      await this.refreshAgentChats();
      const room = this.rooms.get(offer.id);
      if (room) this.onSelect(room);
    } catch (error) { if (this.owns(generation)) void noticeDialog(error.message, { title: 'Could not join the chat' }); }
  }

  async createRoom(parent, answer, generation) {
    try {
      const child = await this.roomRequest(`/api/projects/${parent.project_id}/rooms`, { method: 'POST', body: { title: answer.title, parent_id: parent.id, worktree: { enabled: answer.worktree, base_ref: answer.worktree ? answer.base_ref : '' } } });
      if (!this.owns(generation)) return;
      this.rooms.set(child.id, child); this.touch(); this.onSelect(child);
      if (child.worktree?.state === 'failed') void noticeDialog(`Room created, but its worktree needs attention: ${child.worktree.reason}`, { title: 'Worktree needs attention' });
    } catch (error) { if (this.owns(generation)) void noticeDialog(error.message, { title: 'Could not create the room' }); }
  }

  destroy() { this.reset(); this.alive = false; }
}
