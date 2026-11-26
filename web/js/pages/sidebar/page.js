import { html, effect } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { setGlyph, iconGlyph } from '../../icon-button.js';
import { SORTS } from './model.js';
import { SidebarList } from './sidebar-list.js';
import { SortMenu } from './sort-menu.js';
import { SidebarStore } from './store.js';
import { bindContextMenu } from '../../components/context-menu.js';
import { roomRowMenu } from './row-menu.js';

export const SIDEBAR_PREVIEW_EVENT = 'aimebu:sidebar-preview';

// The adapter keeps the list's public face for app.js: the Maps of rooms and projects, the calls that load and refresh them, and the header's
// buttons (sort, collapse all, New project), which stay in index.html and are written from here alone.
export class Sidebar {
  constructor(container, onSelect) {
    this.container = container; this.onSelect = onSelect;
    this.store = new SidebarStore({ onSelect: (room, messageID) => this.onSelect(room, messageID) });
    this.projectButton = document.querySelector('#project-open');
    this.page = mountPreactPage(container, () => html`<${SidebarList} store=${this.store} projectButton=${this.projectButton} />`);
    // A room row's context menu: right-click, a long press, or the context-menu key or Shift+F10 on the focused row (components/context-menu.js).
    this.stopMenu = bindContextMenu(container, { selector: '.room-group[data-room-id] > .room-item', menu: row => {
      const room = this.store.rooms.get(row.parentElement.dataset.roomId);
      return room ? { ...roomRowMenu(this.store, room), returnFocus: document.activeElement && document.activeElement !== document.body ? document.activeElement : row.querySelector('.room-item-name') } : null;
    } });
  }

  get rooms() { return this.store.rooms; }
  get projects() { return this.store.projects; }
  get joinable() { return this.store.joinable; }
  get generation() { return this.store.generation; }
  get active() { return this.store.active.peek(); }
  get sort() { return this.store.sort.peek(); }
  get filter() { return this.store.filter.peek(); }
  get manual() { return this.store.manual.peek(); }
  get collapsedRooms() { return this.store.collapsedRooms; }
  set collapsedRooms(value) { this.store.collapsedRooms = value; }
  get seenRooms() { return this.store.seenRooms; }
  set seenRooms(value) { this.store.seenRooms = value; }
  get reads() { return this.store.reads; }
  get health() { return this.store.health; }
  set holdRead(value) { this.store.holdRead = value; }
  get holdRead() { return this.store.holdRead; }
  set shownSeq(value) { this.store.shownSeq = value; }
  // What the row menu does through the open room (its settings, archiving it, leaving a chat): { settings(room), archive(room), leave(room) }.
  set roomActions(value) { this.store.roomActions = value; }
  get held() { return this.store.held.peek(); }
  get shownSeq() { return this.store.shownSeq; }

  reset() { this.store.reset(); }
  load() { return this.store.load(); }
  expect(...ids) { this.store.expect(...ids); }
  addProject(project) { return this.store.addProject(project); }
  refreshProject(id, options) { return this.store.refreshProject(id, options); }
  markRead(room) { return this.store.markRead(room); }
  markUnread(room, seq) { return this.store.markUnread(room, seq); }
  scheduleRefresh(id) { this.store.scheduleRefresh(id); }
  refreshAgentChats() { return this.store.refreshAgentChats(); }
  forgetRoom(id) { this.store.forgetRoom(id); }
  forgetProject(id) { this.store.forgetProject(id); }
  setActive(id) { this.store.setActive(id); }
  releaseFolds() { this.store.releaseFolds(); }
  setHealth(agents) { this.store.setHealth(agents); }
  setPreference(room, field, value) { return this.store.setPreference(room, field, value); }
  isAgentChatProject(id) { return this.store.isAgentChatProject(id); }
  markSeen(room) { this.store.markSeen(room); }
  unreadCount(room) { return this.store.unreadCount(room); }
  roomRequest(path, options) { return this.store.roomRequest(path, options); }
  render() { this.store.touch(); }

  // Settings announces a change of the preview choice; the rows are drawn again from the rooms already loaded.
  watchPreview() { this.onPreview = () => this.store.touch(); document.addEventListener(SIDEBAR_PREVIEW_EVENT, this.onPreview); }

  // The collapse button in the ROOMS header folds every project's tree or opens them all again. It shows a chevron down while something is open (the
  // click folds everything) and a chevron right once everything is folded (the click opens everything).
  bindCollapseAll(button) {
    this.collapseButton = button;
    button.onclick = () => this.store.toggleAll(this.store.collapsible.peek());
    this.stopCollapse = effect(() => {
      this.store.version.value;
      const ids = this.store.collapsible.value, folded = ids.size > 0 && [...ids].every(id => this.store.collapsedRooms.has(id)), label = folded ? 'Expand all projects' : 'Collapse all projects';
      button.disabled = ids.size === 0;
      button.classList.toggle('is-collapsed', folded);
      if (!button.querySelector('.icon-mask')) button.replaceChildren(iconGlyph('chevron-right'));
      button.title = label; button.setAttribute('aria-label', label);
    });
  }

  // The sort button opens a menu of the three orders: a choice reorders the list, and the arrows move between the items.
  bindSort(button) {
    this.sortButton = button;
    setGlyph(button, 'arrow-up-down', 'Sort rooms');
    button.setAttribute('aria-haspopup', 'menu'); button.setAttribute('aria-expanded', 'false');
    const menu = this.sortMenu = document.createElement('div'); menu.className = 'sort-menu'; menu.id = 'rooms-sort-menu'; menu.setAttribute('role', 'menu'); menu.setAttribute('aria-label', 'Sort rooms'); menu.hidden = true;
    button.setAttribute('aria-controls', menu.id);
    button.after(menu);
    this.menuPage = mountPreactPage(menu, () => html`<${SortMenu} store=${this.store} onPick=${key => { this.store.setSort(key); this.closeSortMenu(true); }} />`);
    button.onclick = () => { if (menu.hidden) this.openSortMenu(); else this.closeSortMenu(true); };
    this.onOutside = event => { if (!menu.hidden && !menu.contains(event.target) && !button.contains(event.target)) this.closeSortMenu(false); };
    document.addEventListener('click', this.onOutside);
    menu.addEventListener('keydown', event => {
      const items = [...menu.querySelectorAll('.sort-item')], index = items.indexOf(document.activeElement);
      if (event.key === 'Escape') { event.preventDefault(); this.closeSortMenu(true); }
      else if (event.key === 'ArrowDown') { event.preventDefault(); items[(index + 1) % items.length].focus(); }
      else if (event.key === 'ArrowUp') { event.preventDefault(); items[(index - 1 + items.length) % items.length].focus(); }
    });
    this.stopSort = effect(() => { button.title = `Sort rooms: ${SORTS.find(([key]) => key === this.store.sort.value)?.[1] || ''}`; });
  }
  openSortMenu() { this.sortMenu.hidden = false; this.sortButton.setAttribute('aria-expanded', 'true'); this.sortMenu.querySelector(`.sort-item[data-sort="${this.store.sort.peek()}"]`)?.focus(); }
  closeSortMenu(refocus) { if (!this.sortMenu || this.sortMenu.hidden) return; this.sortMenu.hidden = true; this.sortButton.setAttribute('aria-expanded', 'false'); if (refocus) this.sortButton.focus(); }

  destroy() {
    this.store.destroy(); this.stopMenu?.();
    if (this.onPreview) document.removeEventListener(SIDEBAR_PREVIEW_EVENT, this.onPreview);
    if (this.onOutside) document.removeEventListener('click', this.onOutside);
    this.stopCollapse?.(); this.stopSort?.(); this.menuPage?.destroy(); this.page.destroy();
  }
}
