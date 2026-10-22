import { previewText } from './preview.js';
import { setGlyph, iconGlyph } from './icon-button.js';
import { rowUnread, ReadMarker } from './room-unread.js';
import { roomSilenced, mutedNow } from './room-notify.js';
import { request, currentUser } from './api.js';
import { savePreference } from './preferences.js';
import { SORTS, DEFAULT_SORT, orderProjects, orderRooms, moveProject, placeProject } from './sidebar-order.js';
import { formDialog, noticeDialog } from './dialogs.js';

// Keep existing rows attached during ordinary refreshes. Moving a node while
// the user presses its action button can discard the click before it fires.
function reconcileChildren(parent, wanted) {
  const keep = new Set(wanted);
  for (const node of [...parent.children]) if (!keep.has(node)) node.remove();
  let next = null;
  for (let index = wanted.length - 1; index >= 0; index--) {
    const node = wanted[index];
    if (node.parentElement !== parent || node.nextElementSibling !== next) parent.insertBefore(node, next);
    next = node;
  }
}

const CHEVRONS = { down: '6 9 12 15 18 9', right: '9 18 15 12 9 6' };
function setChevron(button, direction) {
  button.innerHTML = `<svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><polyline points="${CHEVRONS[direction]}"></polyline></svg>`;
}
const collapsedKey = 'aimebu2.rooms.collapsed';
// The sort and the manual project order are the person's own preferences (they follow them across devices); the browser keeps a
// cache under these keys, filled from the server at sign-in (see preferences.js).
const sortKey = () => `aimebu2.ui.sidebar_sort.${currentUser}`, orderKey = () => `aimebu2.ui.sidebar_project_order.${currentUser}`;
function loadSort() { try { const value = localStorage.getItem(sortKey()); return SORTS.some(([key]) => key === value) ? value : DEFAULT_SORT; } catch { return DEFAULT_SORT; } }
function loadManual() { try { const list = JSON.parse(localStorage.getItem(orderKey()) || '[]'); return Array.isArray(list) ? list.filter(id => typeof id === 'string') : []; } catch { return []; } }
function loadCollapsed() { try { const list = JSON.parse(localStorage.getItem(collapsedKey) || '[]'); return new Set(Array.isArray(list) ? list.filter(id => typeof id === 'string') : []); } catch { return new Set(); } }

export class Sidebar {
  constructor(container, onSelect) { this.container = container; this.onSelect = onSelect; this.projects = new Map(); this.rooms = new Map(); this.roomNodes = new Map(); this.collapsedRooms = loadCollapsed(); this.archiveNodes = new Map(); this.hiddenNodes = new Map(); this.archivedOpen = new Map(); this.archiveSearchNodes = new Map(); this.archiveHitNodes = new Map(); this.archiveFilter = new Map(); this.archiveTimers = new Map(); this.archiveRequests = new Map(); this.roomRequests = new Set(); this.generation = 0; this.reads = new ReadMarker((roomID, seq) => request(`/api/rooms/${roomID}/read`, { method: 'POST', body: seq > 0 ? { seq } : {} })); this.health = new Map(); this.active = ''; this.loadedUser = null; this.filter = ''; this.createBox = null; this.sort = DEFAULT_SORT; this.manual = []; this.sortButton = null; this.sortMenu = null; this.dragging = null; this.collapseButton = null; this.collapsible = new Set(); }
  reset() {
    this.reads.reset();
    this.generation++;
    for (const timer of this.archiveTimers.values()) clearTimeout(timer);
    for (const requestID of this.archiveRequests.values()) requestID.abort();
    for (const requestID of this.roomRequests) requestID.abort();
    this.roomRequests.clear();
    for (const cache of [this.projects, this.rooms, this.roomNodes, this.archiveNodes, this.hiddenNodes, this.archivedOpen, this.archiveSearchNodes, this.archiveHitNodes, this.archiveFilter, this.archiveTimers, this.archiveRequests, this.health]) cache.clear();
    this.active = ''; this.loadedUser = null; this.filter = ''; this.createBox = null; this.directNode = null; this.container.replaceChildren();
  }
  async roomRequest(path, options = {}) {
    const controller = new AbortController();
    this.roomRequests.add(controller);
    try { return await request(path, { ...options, signal: controller.signal }); }
    finally { this.roomRequests.delete(controller); }
  }
  setHealth(agents) {
    this.health.clear();
    for (const agent of agents) for (const id of agent.room_ids || []) this.health.set(id, [...(this.health.get(id) || []), agent]);
    this.render();
  }
  unreadCount(room) { return Math.max(0, room.unread_count || 0); }
  // Read position lives on the server so every device shares it; the room
  // refresh that follows the event clears the badge.
  // Only the open room is marked read, and only up to the newest message its chat has rendered (shownSeq), so no path can
  // move the cursor past what the reader saw. Nothing is marked while the chat has rendered nothing of the room yet. The
  // unread count left after the mark comes back from the server, so a later render can mark the rest.
  markSeen(room) {
    if (!room) return;
    const seq = this.shownSeq ? this.shownSeq(room.id) : 0;
    // A room set to mentions only has nothing to count for most of what it shows, yet it is read all the same: the position moves on, so
    // switching the room back to counting everything does not bring the old messages back as unread.
    if (!this.unreadCount(room) && !(room.notify === 'mentions' && seq > (room.read_cursor || 0))) return;
    if (this.shownSeq && !(seq > 0)) return;
    // One request per room at a time; a further mark asked for meanwhile follows it (ReadMarker).
    this.reads.mark(room.id, seq, state => { if (this.rooms.get(room.id) !== room) return; if (Number.isInteger(state?.unread_count)) room.unread_count = state.unread_count; if (Number.isInteger(state?.read_cursor)) room.read_cursor = state.read_cursor; });
  }
  async load() {
    const generation = this.generation;
    const resync = this.loadedUser === currentUser;
    let projects;
    const projectRooms = [];
    try {
      projects = await this.roomRequest('/api/projects');
      if (generation !== this.generation) return [];
      // One request per project, all at once: waiting for each in turn made the first screen take the sum of them.
      const lists = await Promise.all(projects.map(project => this.roomRequest(`/api/projects/${project.id}/rooms`)));
      if (generation !== this.generation) return [];
      projects.forEach((project, index) => projectRooms.push([project, lists[index]]));
    } catch (error) {
      if (generation !== this.generation) return [];
      throw error;
    }
    for (const [project, rooms] of projectRooms) {
      this.projects.set(project.id, project);
      for (const room of rooms) this.rooms.set(room.id, room);
    }
    this.loadedUser = currentUser;
    this.sort = loadSort(); this.manual = loadManual();
    this.render(); this.syncSortMenu();
    return projects;
  }
  async addProject(project) { const generation = this.generation; let rooms; try { rooms = await this.roomRequest(`/api/projects/${project.id}/rooms`); } catch (error) { if (generation !== this.generation) return []; throw error; } if (generation !== this.generation) return []; this.projects.set(project.id, project); for (const room of rooms) this.rooms.set(room.id, room); this.render(); return rooms; }
  async refreshProject(id) { const generation = this.generation; let rooms; try { rooms = await this.roomRequest(`/api/projects/${id}/rooms`); } catch (error) { if (generation !== this.generation) return; throw error; } if (generation !== this.generation) return; for (const room of rooms) { this.rooms.set(room.id, room); if (room.id === this.active && !this.holdRead?.(room.id)) this.markSeen(room); } this.render(); }
  // A room removed on the server is dropped here by name: a project refresh only adds and updates.
  forgetRoom(id) { if (this.rooms.delete(id)) this.render(); }
  // Selecting does not mark read by itself: the chat marks the room once it has rendered the latest page (Chat.open).
  setActive(id) { this.active = id; this.render(); }
  async setPreference(room, field, value) {
    const generation = this.generation;
    try {
      await this.roomRequest(`/api/rooms/${room.id}/preferences`, { method: 'PATCH', body: { [field]: value } });
      if (generation !== this.generation) return false;
      await this.refreshProject(room.project_id);
      if (generation !== this.generation) return false;
      return true;
    } catch (error) { if (generation === this.generation) void noticeDialog(error.message, { title: 'Could not change the room' }); return false; }
  }
  // One list across projects. A project's home room is its parent row (no section label) with its task rooms nested under
  // it, one level; one Direct messages section closes the list. Hide lives in the room settings; in the manual order a
  // project is moved from its home row. An "Archived (N)" row under the home room lists a project's archived rooms inline.
  activityOrder(a, b) { return (b.last_activity_at || '').localeCompare(a.last_activity_at || ''); }
  matches(room) {
    const text = this.filter.trim().toLocaleLowerCase();
    return !text || (room.title || '').toLocaleLowerCase().includes(text) || (this.projects.get(room.project_id)?.name || '').toLocaleLowerCase().includes(text);
  }
  // A mute ends by itself: draw the list again when the first running one runs out, so its bell goes without a reload.
  scheduleMuteEnd() {
    clearTimeout(this.muteTimer);
    const now = Date.now();
    const ends = [...this.rooms.values()].map(room => Date.parse(room.muted_until || '')).filter(end => end > now);
    if (!ends.length) return;
    this.muteTimer = setTimeout(() => this.render(), Math.min(Math.min(...ends) - now + 250, 2 ** 31 - 1));
  }
  render() {
    this.scheduleMuteEnd();
    const projects = [...this.projects.values()], many = projects.length > 1;
    const all = [...this.rooms.values()], filtering = !!this.filter.trim();
    const live = all.filter(room => !room.hidden && room.lifecycle !== 'closed' && this.matches(room));
    const direct = live.filter(room => room.kind === 'dm').sort((a, b) => this.activityOrder(a, b));
    const ordinary = live.filter(room => room.kind !== 'dm');
    const liveParent = room => { const parent = room.parent_id && this.rooms.get(room.parent_id); return !!parent && parent.lifecycle !== 'closed' && !parent.hidden; };
    const tails = new Map(projects.map(project => [project.id, filtering ? [] : this.projectTail(project, all)]));
    for (const project of projects) if (!all.some(room => room.project_id === project.id && room.lifecycle === 'closed' && !room.hidden && room.kind !== 'dm')) this.dropArchiveNodes(project.id);
    this.collapsible = new Set(); // the roots that show a chevron this time: what collapse all and expand all work on
    const wanted = [this.createBoxNode()], tailed = new Set();
    const place = (room, nest, extra) => {
      const tail = room.kind === 'home' && tails.get(room.project_id)?.length && !tailed.has(room.project_id) ? tails.get(room.project_id) : null;
      if (tail) tailed.add(room.project_id);
      return this.roomNode(room, { chip: many, nest, tail, ...extra });
    };
    // Projects in the chosen order (name, latest activity, or the person's own manual order), each with its whole tree.
    const ordered = orderProjects(projects, all, this.sort, this.manual);
    for (const [index, project] of ordered.entries()) {
      const roots = orderRooms(ordinary.filter(room => room.project_id === project.id && (filtering || !liveParent(room))), this.sort);
      for (const room of roots) wanted.push(place(room, !filtering, room.kind === 'home' && !filtering ? { project: { first: index === 0, last: index === ordered.length - 1 } } : {}));
      // A project with no visible home room still gets its archived and hidden rows, after its rooms.
      if (!tailed.has(project.id)) wanted.push(...tails.get(project.id));
    }
    // A filter that matches nothing says so, instead of leaving an empty list.
    if (filtering && !live.length) { const none = this.noMatchNode ||= Object.assign(document.createElement('p'), { className: 'rooms-empty' }); none.textContent = `No rooms match “${this.filter.trim()}”.`; wanted.push(none); }
    // One direct-messages section for every project, always last.
    let directSection = this.directNode;
    if (!directSection) {
      directSection = this.directNode = document.createElement('div'); directSection.className = 'direct-rooms';
      const heading = document.createElement('h3'); heading.textContent = 'Direct messages';
      directSection.append(heading);
    }
    reconcileChildren(directSection, [directSection.querySelector('h3'), ...direct.map(room => this.roomNode(room, { chip: many, nest: false }))]);
    wanted.push(directSection);
    reconcileChildren(this.container, wanted);
    this.syncCollapseAll();
  }
  // The "Archived (N)" and "Hidden (N)" rows that close a project's room tree.
  projectTail(project, all) {
    const nodes = [];
    const closed = all.filter(room => room.project_id === project.id && !room.hidden && room.lifecycle === 'closed' && room.kind !== 'dm');
    const hiddenRooms = all.filter(room => room.project_id === project.id && room.hidden);
    if (closed.length) {
      let fold = this.archiveNodes.get(project.id);
      if (!fold) {
        fold = document.createElement('details'); fold.className = 'archived-rooms'; fold.open = this.archivedOpen.get(project.id) || false;
        fold.ontoggle = () => this.archivedOpen.set(project.id, fold.open);
        fold.append(document.createElement('summary'));
        this.archiveNodes.set(project.id, fold);
      }
      const summary = fold.querySelector('summary'); summary.textContent = `Archived (${closed.length})`;
      const search = this.archiveSearchNode(project.id);
      const hits = this.archiveHitsNode(project.id);
      const text = (this.archiveFilter.get(project.id) || '').trim().toLocaleLowerCase();
      const shown = text ? closed.filter(room => (room.title || '').toLocaleLowerCase().includes(text)) : closed;
      reconcileChildren(fold, [summary, search, hits, ...shown.map(room => this.roomNode(room, { chip: false, nest: false }))]);
      nodes.push(fold);
    }
    if (hiddenRooms.length) {
      let fold = this.hiddenNodes.get(project.id);
      if (!fold) { fold = document.createElement('details'); fold.className = 'hidden-rooms'; this.hiddenNodes.set(project.id, fold); }
      let summary = fold.querySelector('summary');
      if (!summary) { summary = document.createElement('summary'); fold.append(summary); }
      summary.textContent = `Hidden (${hiddenRooms.length})`;
      const rows = hiddenRooms.map(room => {
        let button = fold.querySelector(`[data-hidden-room-id="${CSS.escape(room.id)}"]`);
        if (!button) { button = document.createElement('button'); button.type = 'button'; button.className = 'row-btn hidden-room-show'; button.dataset.hiddenRoomId = room.id; }
        button.replaceChildren(iconGlyph('eye'), room.title); button.title = `Show ${room.title}`; button.setAttribute('aria-label', `Show ${room.title}`);
        button.onclick = async () => { if (await this.setPreference(room, 'hidden', false)) this.onSelect(this.rooms.get(room.id) || room); };
        return button;
      });
      reconcileChildren(fold, [summary, ...rows]);
      nodes.push(fold);
    } else this.hiddenNodes.get(project.id)?.remove();
    return nodes;
  }
  // The box on top is a plain filter: typing narrows the list to matching rooms and projects. Rooms are created from a row's +.
  createBoxNode() {
    if (!this.createBox) {
      const form = document.createElement('form'); form.className = 'room-filter';
      const input = document.createElement('input'); input.type = 'text'; input.className = 'room-filter-input'; input.placeholder = 'Filter rooms…'; input.autocomplete = 'off'; input.setAttribute('aria-label', 'Filter rooms');
      form.append(input);
      input.oninput = () => { this.filter = input.value; this.render(); };
      form.onsubmit = event => event.preventDefault(); // Enter creates nothing
      this.createBox = form;
    }
    return this.createBox;
  }
  // The project's own controls on its home row: in the manual order a drag handle's keyboard twin, move up and move down.
  projectControls(row, room, opts) {
    const project = room.kind === 'home' ? this.projects.get(room.project_id) : null;
    const wanted = !!project && !!opts.project;
    if (!wanted) { row.querySelector('.project-move-up')?.remove(); row.querySelector('.project-move-down')?.remove(); row.draggable = false; row.classList.remove('project-draggable'); return; }
    const add = row.querySelector('.add-room');
    for (const [name, delta, glyph, label] of [['project-move-up', -1, 'arrow-up', 'Move project up'], ['project-move-down', 1, 'arrow-down', 'Move project down']]) {
      let button = row.querySelector(`.${name}`);
      if (this.sort !== 'manual') { button?.remove(); continue; }
      if (!button) { button = document.createElement('button'); button.type = 'button'; button.className = `row-btn project-move ${name}`; row.insertBefore(button, add); }
      setGlyph(button, glyph, `${label}: ${project.name}`);
      button.disabled = delta < 0 ? opts.project.first : opts.project.last;
      button.onclick = () => this.moveProject(project.id, delta);
    }
    row.draggable = this.sort === 'manual'; row.classList.toggle('project-draggable', this.sort === 'manual');
    if (!row.dataset.dragBound) {
      row.dataset.dragBound = '1';
      const same = event => { const from = this.dragging && this.projects.get(this.dragging), to = this.projects.get(room.project_id); return from && to && from.id !== to.id && this.sort === 'manual'; };
      const clear = () => document.querySelectorAll('.drop-before, .drop-after').forEach(node => node.classList.remove('drop-before', 'drop-after'));
      row.addEventListener('dragstart', event => { if (this.sort !== 'manual') { event.preventDefault(); return; } this.dragging = room.project_id; event.dataTransfer?.setData('text/plain', room.project_id); if (event.dataTransfer) event.dataTransfer.effectAllowed = 'move'; row.classList.add('dragging'); });
      row.addEventListener('dragend', () => { this.dragging = null; row.classList.remove('dragging'); clear(); });
      row.addEventListener('dragover', event => { if (!same(event)) return; event.preventDefault(); const after = event.clientY > row.getBoundingClientRect().top + row.getBoundingClientRect().height / 2; clear(); row.classList.add(after ? 'drop-after' : 'drop-before'); });
      row.addEventListener('dragleave', () => row.classList.remove('drop-before', 'drop-after'));
      row.addEventListener('drop', event => {
        if (!same(event)) return; event.preventDefault();
        const after = event.clientY > row.getBoundingClientRect().top + row.getBoundingClientRect().height / 2, id = this.dragging; clear(); this.dragging = null;
        this.setManual(placeProject([...this.projects.values()], this.manual, id, room.project_id, after));
      });
    }
  }
  setManual(order) { this.manual = order; try { localStorage.setItem(orderKey(), JSON.stringify(order)); } catch { /* private mode */ } savePreference('sidebar_project_order', JSON.stringify(order)); this.render(); }
  moveProject(id, delta) { this.setManual(moveProject([...this.projects.values()], this.manual, id, delta)); }
  setSort(sort) {
    if (!SORTS.some(([key]) => key === sort)) return;
    // Manual keeps the order the list shows now, so choosing it after another sort starts from what the person sees.
    if (sort === 'manual' && this.sort !== 'manual') this.manual = orderProjects([...this.projects.values()], [...this.rooms.values()], this.sort, this.manual).map(project => project.id);
    this.sort = sort;
    try { localStorage.setItem(sortKey(), sort); if (sort === 'manual') localStorage.setItem(orderKey(), JSON.stringify(this.manual)); } catch { /* private mode */ }
    savePreference('sidebar_sort', sort); if (sort === 'manual') savePreference('sidebar_project_order', JSON.stringify(this.manual));
    this.render(); this.syncSortMenu();
  }
  // The collapse button in the ROOMS header folds every project's tree or opens them all again. It shows a chevron down while
  // something is open (the click folds everything) and a chevron right once everything is folded (the click opens everything).
  // The folded state is the one the row chevrons keep, in this browser.
  bindCollapseAll(button) {
    this.collapseButton = button;
    button.onclick = () => this.toggleAll();
    this.syncCollapseAll();
  }
  allCollapsed() { return this.collapsible.size > 0 && [...this.collapsible].every(id => this.collapsedRooms.has(id)); }
  toggleAll() {
    if (this.allCollapsed()) for (const id of this.collapsible) this.collapsedRooms.delete(id);
    else for (const id of this.collapsible) this.collapsedRooms.add(id);
    try { localStorage.setItem(collapsedKey, JSON.stringify([...this.collapsedRooms])); } catch { /* private mode */ }
    this.render();
  }
  syncCollapseAll() {
    const button = this.collapseButton;
    if (!button) return;
    const folded = this.allCollapsed(), label = folded ? 'Expand all projects' : 'Collapse all projects';
    button.disabled = this.collapsible.size === 0;
    button.classList.toggle('is-collapsed', folded);
    if (!button.querySelector('.icon-mask')) button.replaceChildren(iconGlyph('chevron-right'));
    button.title = label; button.setAttribute('aria-label', label);
  }
  // The sort button in the ROOMS header opens a small menu of the three orders.
  bindSort(button) {
    this.sortButton = button;
    setGlyph(button, 'arrow-up-down', 'Sort rooms');
    button.setAttribute('aria-haspopup', 'menu'); button.setAttribute('aria-expanded', 'false');
    const menu = this.sortMenu = document.createElement('div'); menu.className = 'sort-menu'; menu.id = 'rooms-sort-menu'; menu.setAttribute('role', 'menu'); menu.setAttribute('aria-label', 'Sort rooms'); menu.hidden = true;
    button.setAttribute('aria-controls', menu.id);
    for (const [key, label] of SORTS) {
      const item = document.createElement('button'); item.type = 'button'; item.className = 'row-btn sort-item'; item.dataset.sort = key; item.setAttribute('role', 'menuitemradio');
      item.append(iconGlyph('check'), document.createTextNode(label));
      item.onclick = () => { this.setSort(key); this.closeSortMenu(true); };
      menu.append(item);
    }
    button.after(menu);
    button.onclick = () => { if (menu.hidden) this.openSortMenu(); else this.closeSortMenu(true); };
    document.addEventListener('click', event => { if (!menu.hidden && !menu.contains(event.target) && !button.contains(event.target)) this.closeSortMenu(false); });
    menu.addEventListener('keydown', event => {
      const items = [...menu.querySelectorAll('.sort-item')], index = items.indexOf(document.activeElement);
      if (event.key === 'Escape') { event.preventDefault(); this.closeSortMenu(true); }
      else if (event.key === 'ArrowDown') { event.preventDefault(); items[(index + 1) % items.length].focus(); }
      else if (event.key === 'ArrowUp') { event.preventDefault(); items[(index - 1 + items.length) % items.length].focus(); }
    });
    this.syncSortMenu();
  }
  openSortMenu() { this.sortMenu.hidden = false; this.sortButton.setAttribute('aria-expanded', 'true'); this.syncSortMenu(); this.sortMenu.querySelector(`.sort-item[data-sort="${this.sort}"]`)?.focus(); }
  closeSortMenu(refocus) { if (!this.sortMenu || this.sortMenu.hidden) return; this.sortMenu.hidden = true; this.sortButton.setAttribute('aria-expanded', 'false'); if (refocus) this.sortButton.focus(); }
  syncSortMenu() { for (const item of this.sortMenu?.querySelectorAll('.sort-item') || []) { const on = item.dataset.sort === this.sort; item.setAttribute('aria-checked', String(on)); item.classList.toggle('is-selected', on); } if (this.sortButton) this.sortButton.title = `Sort rooms: ${SORTS.find(([key]) => key === this.sort)?.[1] || ''}`; }
  roomNode(room, opts = {}) {
    let group = this.roomNodes.get(room.id);
    if (!group) { group = document.createElement('div'); group.className = 'room-group'; group.dataset.roomId = room.id; const row = document.createElement('div'); row.className = 'room-row'; group.append(row); this.roomNodes.set(room.id, group); }
    const row = group.querySelector(':scope > .room-row'); row.classList.toggle('active', room.id === this.active); row.classList.toggle('archived', room.lifecycle === 'closed'); row.classList.toggle('closing', room.lifecycle === 'closing');
    let select = row.querySelector('.room-name');
    if (!select) {
      select = document.createElement('button'); select.type = 'button'; select.className = 'row-btn room-name'; select.onclick = () => this.onSelect(this.rooms.get(room.id) || room); row.append(select);
      const add = document.createElement('button'); add.type = 'button'; add.className = 'row-btn add-room'; setGlyph(add, 'plus', `New task room under ${room.title}`);
      add.onclick = async () => {
        const answer = await formDialog({ title: `New task room under ${room.title}`, confirm: 'Create room', fields: [
          { name: 'title', label: 'Room name', required: true, requiredMessage: 'Give the room a name.' },
          { name: 'worktree', type: 'checkbox', label: 'Create a Git worktree for this room', hint: 'It starts from committed HEAD; uncommitted base changes stay in the project folder.' },
          { name: 'base_ref', label: 'Base branch or ref (optional)', placeholder: 'Current HEAD', showIf: values => values.worktree },
        ] });
        if (!answer) return;
        const generation = this.generation;
        try {
          const child = await this.roomRequest(`/api/projects/${room.project_id}/rooms`, { method: 'POST', body: { title: answer.title, parent_id: room.id, worktree: { enabled: answer.worktree, base_ref: answer.worktree ? answer.base_ref : '' } } });
          if (generation !== this.generation) return;
          this.rooms.set(child.id, child); this.render(); this.onSelect(child);
          if (child.worktree?.state === 'failed') void noticeDialog(`Room created, but its worktree needs attention: ${child.worktree.reason}`, { title: 'Worktree needs attention' });
        } catch (error) { if (generation === this.generation) void noticeDialog(error.message, { title: 'Could not create the room' }); }
      };
      row.append(add);
    }
    select.textContent = `${room.kind === 'home' ? '⌂' : room.kind === 'dm' ? '@' : '#'} ${room.title}`;
    this.projectControls(row, room, opts);
    let chip = row.querySelector('.room-project');
    const projectName = this.projects.get(room.project_id)?.name || '';
    if (opts.chip && projectName && !(room.kind === 'home' && room.title.trim().toLowerCase() === projectName.trim().toLowerCase())) {
      if (!chip) { chip = document.createElement('span'); chip.className = 'room-project'; row.insertBefore(chip, row.querySelector('.add-room')); }
      chip.textContent = projectName; chip.title = `Project ${projectName}`;
    } else chip?.remove();
    // The member count is a tooltip on the name (the row shows the unread count and the attention bell, as v1 does).
    const members = room.member_count || 0; select.title = `${room.title} · ${members} ${members === 1 ? 'member' : 'members'}`;
    let health = row.querySelector('.room-health');
    const affected = this.health.get(room.id) || [];
    if (affected.length && !health) { health = document.createElement('span'); health.className = 'room-health'; row.insertBefore(health, row.querySelector('.add-room')); }
    if (health) { health.textContent = `⚠ ${affected.length}`; health.title = affected.map(agent => `${agent.name}: ${agent.flags.map(flag => flag.code.replaceAll('_', ' ')).join(', ')}`).join('\n'); health.hidden = !affected.length; }
    // A room that is quiet for this person (level Nothing, or a mute that is still running) shows a disabled bell.
    let muted = row.querySelector('.room-muted');
    const quiet = roomSilenced(room);
    if (quiet && !muted) { muted = document.createElement('span'); muted.className = 'room-muted'; muted.setAttribute('role', 'img'); muted.append(iconGlyph('bell-disabled')); row.insertBefore(muted, row.querySelector('.add-room')); }
    if (muted) {
      muted.hidden = !quiet;
      muted.title = mutedNow(room) ? `Muted until ${new Date(room.muted_until).toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' })}` : 'Notifications are off for this room';
      muted.setAttribute('aria-label', muted.title);
    }
    const add = row.querySelector('.add-room');
    add.disabled = !!room.lifecycle && room.lifecycle !== 'open';
    add.hidden = room.kind === 'dm' || room.lifecycle === 'closed';
    let reopen = row.querySelector('.reopen-room');
    if (room.lifecycle === 'closed') {
      if (!reopen) {
        reopen = document.createElement('button'); reopen.type = 'button'; reopen.className = 'row-btn reopen-room'; setGlyph(reopen, 'reopen'); row.append(reopen);
      }
      reopen.title = `Reopen ${room.title}`; reopen.setAttribute('aria-label', reopen.title);
      reopen.onclick = async () => {
        const generation = this.generation;
        reopen.disabled = true;
        try {
          const updated = await this.roomRequest(`/api/rooms/${room.id}/reopen`, { method: 'POST', body: { lifecycle_rev: room.lifecycle_rev || 0 } });
          if (generation !== this.generation) return;
          this.rooms.set(updated.id, updated); await this.refreshProject(room.project_id);
          if (generation !== this.generation) return;
          if (this.active === room.id) this.onSelect(updated);
        } catch (error) { if (generation === this.generation) { void noticeDialog(error.message, { title: 'Could not reopen the room' }); reopen.disabled = false; } }
      };
    } else reopen?.remove();
    let status = row.querySelector('.room-state');
    if (room.lifecycle === 'closing') {
      if (!status) { status = document.createElement('span'); status.className = 'room-state'; row.insertBefore(status, add); }
      status.textContent = 'Closing…';
    } else status?.remove();
    // The last message is previewed only under the room that is open.
    let preview = group.querySelector(':scope > .room-preview');
    if (room.id === this.active) {
      if (!preview) { preview = document.createElement('div'); preview.className = 'room-preview'; group.insertBefore(preview, row.nextSibling); }
      preview.textContent = previewText(room.last_message_body) || 'No messages yet';
    } else preview?.remove();
    // Task rooms nest one level under their home room, followed by that project's Archived and Hidden rows. A root room's
    // chevron hides or shows them (remembered per browser); a collapsed root shows its children's unread total.
    const children = opts.nest && room.lifecycle !== 'closed' ? orderRooms([...this.rooms.values()].filter(r => r.parent_id === room.id && r.lifecycle !== 'closed' && !r.hidden && r.kind !== 'dm' && this.matches(r)), this.sort) : [];
    const kids = [...children.map(child => this.roomNode(child, { chip: false, nest: false })), ...(opts.tail || [])];
    // A room with sub-rooms has the chevron. A root without any shows no chevron, only an invisible spacer of the same width, so the
    // names of all roots still start at the same indent.
    if (opts.nest && kids.length) this.collapsible.add(room.id);
    const collapsed = kids.length > 0 && this.collapsedRooms.has(room.id);
    group.classList.toggle('collapsed', collapsed);
    let toggle = row.querySelector(':scope > .room-collapse');
    let spacer = row.querySelector(':scope > .branch-spacer');
    if (kids.length) {
      spacer?.remove();
      if (!toggle) {
        toggle = document.createElement('button'); toggle.type = 'button'; toggle.className = 'row-btn branch room-collapse'; row.insertBefore(toggle, row.firstChild);
        toggle.onclick = () => { if (this.collapsedRooms.has(room.id)) this.collapsedRooms.delete(room.id); else this.collapsedRooms.add(room.id); try { localStorage.setItem(collapsedKey, JSON.stringify([...this.collapsedRooms])); } catch { /* private mode */ } this.render(); };
      }
      const label = `${collapsed ? 'Expand' : 'Collapse'} ${room.title}`;
      toggle.title = label; toggle.setAttribute('aria-label', label); toggle.setAttribute('aria-expanded', String(!collapsed));
      setChevron(toggle, collapsed ? 'right' : 'down');
    } else {
      toggle?.remove();
      if (opts.nest && !spacer) { spacer = document.createElement('span'); spacer.className = 'branch-spacer'; spacer.setAttribute('aria-hidden', 'true'); row.insertBefore(spacer, row.firstChild); }
      else if (!opts.nest) spacer?.remove();
    }
    const unread = rowUnread(room, children, collapsed, this.active);
    let badge = row.querySelector('.unread-count');
    if (unread && !badge) { badge = document.createElement('span'); badge.className = 'unread-count'; badge.setAttribute('role', 'img'); row.insertBefore(badge, row.querySelector('.add-room')); }
    if (badge) { badge.textContent = String(unread); badge.setAttribute('aria-label', `${unread} unread messages`); badge.hidden = !unread; }
    const attention = room.attention_count || 0;
    let bell = row.querySelector('.room-item-attention');
    if (attention && !bell) { bell = document.createElement('span'); bell.className = 'room-item-attention'; bell.setAttribute('role', 'img'); bell.textContent = '🔔'; row.insertBefore(bell, row.querySelector('.unread-count') || row.querySelector('.add-room')); }
    if (bell) { bell.title = `${attention} waiting for you`; bell.setAttribute('aria-label', `${attention} items need your attention`); bell.hidden = !attention; }
    let nest = group.querySelector(':scope > .room-children');
    if (kids.length) {
      if (!nest) { nest = document.createElement('div'); nest.className = 'room-children'; group.append(nest); }
      reconcileChildren(nest, kids);
    } else nest?.remove();
    return group;
  }
  archiveSearchNode(projectID) {
    let search = this.archiveSearchNodes.get(projectID);
    if (!search) {
      search = document.createElement('input');
      search.type = 'search'; search.className = 'archive-search';
      search.placeholder = 'Search archived rooms and messages';
      search.setAttribute('aria-label', 'Search archived rooms and messages');
      search.addEventListener('input', () => {
        this.archiveFilter.set(projectID, search.value);
        clearTimeout(this.archiveTimers.get(projectID));
        this.archiveRequests.get(projectID)?.abort();
        const query = search.value.trim();
        this.renderArchiveHits(projectID, []);
        this.render();
        if (!query) return;
        const generation = this.generation;
        this.archiveTimers.set(projectID, setTimeout(async () => {
          if (generation !== this.generation) return;
          const requestID = new AbortController();
          this.archiveRequests.set(projectID, requestID);
          try {
            const hits = await request(`/api/projects/${projectID}/archive/search?q=${encodeURIComponent(query)}`, { signal: requestID.signal });
            if (generation === this.generation && !requestID.signal.aborted && (this.archiveFilter.get(projectID) || '').trim() === query) this.renderArchiveHits(projectID, hits);
          } catch (error) {
            if (generation === this.generation && !requestID.signal.aborted && error.name !== 'AbortError' && (this.archiveFilter.get(projectID) || '').trim() === query) this.renderArchiveError(projectID);
          } finally {
            if (this.archiveRequests.get(projectID) === requestID) this.archiveRequests.delete(projectID);
          }
        }, 250));
      });
      this.archiveSearchNodes.set(projectID, search);
    }
    const wanted = this.archiveFilter.get(projectID) || '';
    if (search.value !== wanted) search.value = wanted;
    return search;
  }
  archiveHitsNode(projectID) {
    let hits = this.archiveHitNodes.get(projectID);
    if (!hits) { hits = document.createElement('div'); hits.className = 'archive-hits'; hits.hidden = true; this.archiveHitNodes.set(projectID, hits); }
    return hits;
  }
  renderArchiveHits(projectID, hits) {
    const list = this.archiveHitNodes.get(projectID);
    if (!list) return;
    list.replaceChildren();
    for (const hit of hits || []) {
      const row = document.createElement('button'); row.type = 'button'; row.className = 'row-btn archive-hit';
      const name = document.createElement('strong'); name.textContent = hit.room_name || '';
      const snippet = document.createElement('span'); snippet.textContent = hit.snippet || '';
      row.append(name, snippet);
      row.onclick = async () => {
        const generation = this.generation;
        let room = this.rooms.get(hit.room_id);
        if (!room) {
          try { room = await this.roomRequest(`/api/rooms/${hit.room_id}`); if (generation !== this.generation) return; this.rooms.set(room.id, room); } catch { return; }
        }
        if (generation !== this.generation) return;
        await this.onSelect(room, hit.message_id);
      };
      list.append(row);
    }
    list.hidden = !hits?.length;
  }
  renderArchiveError(projectID) {
    const list = this.archiveHitNodes.get(projectID);
    if (!list) return;
    list.replaceChildren();
    const error = document.createElement('p'); error.className = 'archive-search-error'; error.textContent = 'Could not search archived messages.';
    list.append(error); list.hidden = false;
  }
  dropArchiveNodes(projectID) {
    clearTimeout(this.archiveTimers.get(projectID)); this.archiveTimers.delete(projectID);
    this.archiveRequests.get(projectID)?.abort(); this.archiveRequests.delete(projectID);
    this.archiveSearchNodes.delete(projectID); this.archiveHitNodes.delete(projectID); this.archiveFilter.delete(projectID);
  }
}
