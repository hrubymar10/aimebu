import { html, effect, signal, untracked } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { rights } from '../../api.js';
import { RoomNotifySettings } from '../../room-notify-settings.js';
import { SchedulesPanel } from './schedules.js';
import { RoomMembers } from '../members/page.js';
import { RoomScopeControl, RoomUsage } from '../project/room-panels.js';
import { InfoError, MemorySection, NotesSection, RenameForm, RoomActions, RoomFacts, ViewActions, WorktreeSection } from './room-panel.js';
import { canRename, headerStatus, kindIcon, membersLabel, notesView, roomFacts, roomPath } from './model.js';
import { RoomPanelStore } from './store.js';
import { SettingsShell } from '../shell/page.js';
import { SettingsAddress } from '../shell/address.js';
import { reveal } from '../shell/highlight.js';
import { ROOM_REGISTRY, settingsAddress, addressFor } from '../shell/room-registry.js';
import { iconButton } from '../../icon-button.js';

const section = (className, label) => { const node = document.createElement('section'); node.className = className; if (label) node.setAttribute('aria-labelledby', label); return node; };

// The room's header and its settings dialog. The header's elements and the dialog's frame stay in index.html (other code reads them) and are written
// from here alone; the parts of the dialog that change with the room are Preact roots in containers this page empties first. `chat` gives the
// panel what only the chat knows (roles, the app's callbacks, the composer's state).
export class RoomPanel {
  constructor(chat) {
    this.chat = chat;
    this.infoDialog = document.querySelector('#room-info-dialog'); this.exportDialog = document.querySelector('#room-export-dialog');
    this.renameForm = document.querySelector('#room-rename-form'); this.projectSection = document.querySelector('#room-project');
    const host = {
      role: id => chat.projectRole?.(id), canArchive: () => chat.canArchiveRoom(), setFacts: room => chat.setRoomFacts(room),
      mutation: room => chat.onRoomMutation?.(room), left: room => chat.onChatLeft?.(room), removed: room => chat.onRoomRemoved?.(room),
      hide: room => chat.setRoomPreference?.(chat.roomView?.(room.id) || room, 'hidden', true),
      chatError: text => { chat.error.textContent = text; }, infoOpen: () => this.infoDialog.open, closeInfo: () => this.infoDialog.close(), unwound: () => this.address.unwinding, closeExport: () => this.exportDialog.close(),
    };
    this.store = new RoomPanelStore(host);
    this.worktreeSection = section('room-worktree'); this.notesSection = section('room-notes', 'room-notes-title'); this.memorySection = section('room-memory-setting');
    const error = document.querySelector('#room-info-error');
    this.projectSection.before(document.querySelector('#room-usage'), this.worktreeSection, this.notesSection, this.memorySection);
    const mount = (container, view) => { container.replaceChildren(); return mountPreactPage(container, view); };
    const store = this.store;
    this.roots = [
      mount(this.renameForm, () => html`<${RenameForm} store=${store} />`),
      mount(document.querySelector('#room-info-facts'), () => html`<${RoomFacts} store=${store} />`),
      mount(document.querySelector('#room-view-actions'), () => html`<${ViewActions} store=${store} />`),
      mount(document.querySelector('.room-info-actions'), () => html`<${RoomActions} store=${store} />`),
      mount(error, () => html`<${InfoError} store=${store} />`),
      mount(this.worktreeSection, () => html`<${WorktreeSection} store=${store} />`),
      mount(this.memorySection, () => html`<${MemorySection} store=${store} />`),
      mount(this.notesSection, () => html`<${NotesSection} store=${store} onOpen=${() => this.openNotes()} />`),
    ];
    this.renameForm.onsubmit = event => { event.preventDefault(); this.saveTitle(); };
    this.notifySettings = new RoomNotifySettings(document.querySelector('#room-member-list'), (field, value) => chat.setRoomPreference?.(chat.roomView?.(chat.room?.id) || chat.room, field, value), () => chat.roomView?.(chat.room?.id) || chat.room);
    this.scopeControl = new RoomScopeControl(document.querySelector('#room-scope-control'));
    this.schedules = new SchedulesPanel(document.querySelector('#room-schedules'));
    this.memberList = new RoomMembers(document.querySelector('#room-member-list'));
    this.roomUsage = new RoomUsage(document.querySelector('#room-usage'));
    this.usageLink = this.projectUsageLink();
    document.querySelector('#room-usage').before(this.usageLink);
    this.shellSetup();
    document.querySelector('#room-info').onclick = () => this.show();
    this.onInfoClose = () => { if (!this.infoDialog.open) { chat.projectSettings?.release(this.projectSection); this.afterClose(); } };
    this.infoDialog.addEventListener('close', this.onInfoClose);
    document.querySelector('#room-export').onclick = () => this.exportDialog.showModal();
    document.querySelector('#room-export-close').onclick = () => this.exportDialog.close();
    for (const button of this.exportDialog.querySelectorAll('[data-format]')) button.onclick = () => this.exportRoom(button.dataset.format);
    this.stopEffects = [this.header(), this.watchFacts(), this.watchBlocks(), effect(() => { this.worktreeSection.hidden = store.worktree.value.hidden; }), effect(() => { store.version.value; const room = store.room.value; this.notesSection.hidden = !room || notesView(room).hidden; })];
  }


  // The settings dialog is the shared settings shell over one registry (shell/room-registry.js): the room's own categories, and in a home room the project's above
  // them. What is in a category are the dialog's existing blocks, shown and hidden by the category (applyBlocks); nothing a block does has changed.
  shellSetup() {
    const registry = ROOM_REGISTRY, empty = { section: '', category: '', sub: '' };
    this.facts = signal({}); this.closed = true; this.pending = !!settingsAddress(location.hash); this.empty = empty;
    const own = category => registry.settings.filter(setting => setting.category === category.id);
    const hidden = category => {
      const facts = this.facts.value;
      if (category.section === 'project' && !facts.home) return true;
      const items = own(category);
      return items.length > 0 && items.every(setting => setting.when && !facts[setting.when]);
    };
    const settingHidden = setting => {
      const facts = this.facts.value;
      return (registry.categoryById(setting.category).section === 'project' && !facts.home) || !!(setting.when && !facts[setting.when]);
    };
    this.shell = new SettingsShell(this.infoDialog, {}, {
      store: { registry, hidden, settingHidden, first: 'general', title: 'Room settings', prefix: 'room-info' }, onCategory: category => this.applyBlocks(category),
      onSelect: path => this.address.navigate(this.pathOf(this.shell.store.resolve(path))), onClose: () => void this.requestClose(), onResult: result => this.openResult(result),
      onBack: () => this.address.back(),
    });
    this.address = new SettingsAddress({
      dialog: this.infoDialog, parse: hash => this.parseHash(hash), pathOf: target => this.pathOf(target), resolve: path => this.shell.store.resolve(this.parseHash(path) || empty),
      show: target => this.showTarget(target), open: (path, options) => this.show(this.parseHash(path) || empty, options),
      listPath: () => this.pathOf({ section: this.shell.store.route.value.section, category: '', sub: '' }), requestClose: () => this.requestClose(),
      currentRoom: () => this.chat.room?.id || '', ready: () => true,
    }, 'aimebuRoomSettings');
  }

  // The link from a room's usage to the project's (a home room only: a task room has no project settings of its own).
  projectUsageLink() {
    const node = document.createElement('div'); node.id = 'room-usage-link'; node.className = 'room-usage-link'; node.hidden = true;
    const button = iconButton('chart-column', 'Project usage', { className: 'room-usage-project' }); button.id = 'room-usage-project';
    button.onclick = () => this.address.navigate(this.pathOf({ section: 'project', category: 'limits', sub: 'usage' }));
    node.append(button);
    return node;
  }

  // What the viewer may see follows the room (its kind, its lifecycle, the viewer's role) and, in a home room, the project's lifecycle once it has loaded.
  watchFacts() {
    return effect(() => {
      this.store.version.value;
      const room = this.store.room.value, renamable = this.store.renamable.value, worktree = this.store.worktree.value;
      const settings = this.chat.projectSettings, loaded = !!room && !!settings && settings.store.project.value?.id === room.project_id;
      const project = room?.kind === 'home' && loaded ? settings.store.lifecycle() : null;
      this.facts.value = roomFacts(room, { renamable, role: room ? this.chat.projectRole?.(room.project_id) : '', worktreeHidden: worktree.hidden, project });
      untracked(() => { this.shell.store.settle(); this.usageLink.hidden = !this.facts.value.home; });
    });
  }

  // A project's form is drawn by Preact into #project-settings-body and fills in as the project loads, so the blocks are put right again when it does.
  watchBlocks() {
    let queued = false;
    const observer = new MutationObserver(() => { if (queued) return; queued = true; queueMicrotask(() => { queued = false; this.applyBlocks(); }); });
    observer.observe(this.projectSection, { childList: true, subtree: true });
    return () => observer.disconnect();
  }

  // The category shows its blocks (CSS selectors of the registry) and the rest of the dialog's blocks are off.
  applyBlocks(category = this.shell.store.registry.categoryById(this.shell.store.category.value)) {
    if (!category) return;
    const all = new Set(ROOM_REGISTRY.categories.flatMap(item => item.blocks));
    for (const selector of all) for (const node of this.infoDialog.querySelectorAll(selector)) node.classList.toggle('shell-off', !category.blocks.includes(selector));
    this.projectSection.classList.toggle('shell-off', category.section !== 'project');
  }

  ids() { const room = this.store.room.peek(); return { roomID: room?.id || '', projectID: room?.project_id || '' }; }

  // The address of a level (the hash without '#'): a room's categories live under the room, a project's under the project.
  pathOf(target) {
    const ids = this.ids(), category = target.category && ROOM_REGISTRY.categoryById(target.category);
    if (category) return addressFor(category, target.sub, ids);
    return target.section === 'project' ? `project/${encodeURIComponent(ids.projectID)}/settings` : `room/${encodeURIComponent(ids.roomID)}/settings`;
  }

  // A hash as {section, category, sub}; null when it is not an address of this room's settings (or of its project's, in a home room).
  parseHash(hash) {
    const address = settingsAddress(hash), room = this.store.room.peek();
    if (!address || !room) return null;
    if (address.kind === 'room' ? address.id !== room.id : !(address.id === room.project_id && room.kind === 'home')) return null;
    const target = ROOM_REGISTRY.parsePath(address.path) || this.empty;
    return address.kind === 'project' && !target.category ? { section: 'project', category: '', sub: '' } : target;
  }

  // Shows a level: the shell follows, and a subcategory scrolls to its place.
  showTarget(target) {
    target = this.shell.store.apply(target);
    const category = ROOM_REGISTRY.categoryById(this.shell.store.category.value);
    const sub = target.sub && (category.subs || []).find(item => item.id === target.sub);
    if (sub) void reveal(this.infoDialog, sub);
  }

  // A search result: its category opens (a new entry, as a click in the navigation) and the setting is brought into view and marked.
  async openResult(result) {
    this.shell.store.query.value = '';
    const path = this.pathOf(this.shell.store.target(result.category.id));
    if (path === this.address.path) this.showTarget(this.shell.store.resolve(this.parseHash(path) || this.empty)); else this.address.navigate(path);
    const row = result.setting ? await reveal(this.infoDialog, result.setting) : null;
    if (!row) await reveal(this.infoDialog, { anchor: '', selector: '.settings-cat-head' });
  }

  // Closing hands the address back at once: the close event is a task later, and a dialog opened in between must not be taken for the one that closed.
  requestClose() {
    if (!this.infoDialog.open) return false;
    this.infoDialog.close(); this.afterClose();
    return true;
  }
  afterClose() {
    if (this.closed) return;
    this.closed = true; this.address.leave();
  }

  // The header: the room's kind icon, title, member count and what its lifecycle or project says.
  header() {
    const title = document.querySelector('#room-title'), crumbs = document.querySelector('#room-crumbs'), icon = document.querySelector('#room-icon'), members = document.querySelector('#room-members');
    return effect(() => {
      this.store.version.value;
      const room = this.store.room.value;
      if (!room) { title.textContent = 'Choose a room'; document.querySelector('#room-restricted')?.setAttribute('hidden', ''); icon.hidden = true; members.hidden = true; crumbs.replaceChildren(); crumbs.hidden = true; return; }
      this.crumbs(crumbs, room);
      title.textContent = room.title; // a rename by someone else reaches here through the room list
      // A restricted room says so beside its title (the sidebar row has the same lock): the header is where a person looks to know who can read this.
      let lock = document.querySelector('#room-restricted');
      if (!lock) {
        lock = document.createElement('span'); lock.id = 'room-restricted'; lock.className = 'room-restricted'; lock.setAttribute('role', 'img'); lock.title = 'Restricted room'; lock.setAttribute('aria-label', 'Restricted room');
        lock.innerHTML = '<svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true"><rect x="3" y="7" width="10" height="7" rx="1.5" /><path d="M5 7V5a3 3 0 0 1 6 0v2" /></svg>';
        title.after(lock);
      }
      lock.hidden = !room.restricted;
      const count = room.member_count || 0, status = headerStatus(room, !!this.chat.projectArchived?.(room.project_id));
      icon.textContent = kindIcon(room); icon.classList.toggle('dm', room.kind === 'dm'); icon.hidden = false;
      members.querySelector('.room-members-count').textContent = String(count);
      members.querySelector('.room-header-status')?.remove();
      if (status) { const node = document.createElement('span'); node.className = 'room-header-status'; node.textContent = status; members.append(node); }
      const label = membersLabel(count, status);
      members.title = label; members.setAttribute('aria-label', label); members.hidden = false;
    });
  }

  // The path beside the title (before it): project › room › … The crumbs are buttons to those rooms; on a narrow header they shrink to an ellipsis (the style
  // sheet), and the whole path is the tooltip of each.
  crumbs(host, room) {
    const path = this.chat.roomPath ? roomPath(room, this.chat.roomPath) : [];
    host.replaceChildren(); host.hidden = path.length === 0;
    for (const crumb of path) {
      const button = document.createElement('button');
      button.type = 'button'; button.className = 'room-crumb'; button.textContent = crumb.label; button.title = path.map(item => item.label).concat(room.title).join(' › ');
      button.disabled = !crumb.roomID; button.onclick = () => this.chat.openCrumb?.(crumb.roomID);
      const sep = document.createElement('span'); sep.className = 'room-crumb-sep'; sep.textContent = '›'; sep.setAttribute('aria-hidden', 'true');
      host.append(button, sep);
    }
  }

  setRoom(room) {
    this.store.setRoom(room);
    // A link to a room's (or project's) settings: the room is open now, so the dialog opens where the address points.
    if (this.pending && room && this.parseHash(location.hash)) { this.pending = false; queueMicrotask(() => { if (!this.infoDialog.open) void this.address.follow(); }); }
  }

  reset() {
    this.store.reset();
    this.infoDialog.close(); this.exportDialog.close();
    this.scopeControl.reset(); this.schedules.reset(); this.memberList.reset();
  }

  async show(target = null, options = {}) {
    if (this.address.unwinding && !this.infoDialog.open) await this.address.unwinding;
    const room = this.store.room.peek();
    if (!room) return;
    this.roomUsage.load(room.id);
    const controls = document.querySelector('#room-scope-control');
    controls.hidden = room.kind === 'dm';
    const renamable = canRename(room, this.chat.projectRole?.(room.project_id), rights.admin);
    this.renameForm.hidden = !renamable;
    this.store.open(renamable);
    this.notifySettings.render();
    const home = room.kind === 'home';
    this.shell.store.title.value = home ? 'Project settings' : 'Room settings';
    this.shell.store.first = home ? 'details' : 'general';
    this.infoDialog.setAttribute('aria-label', home ? 'Project settings' : 'Room settings');
    this.infoDialog.classList.toggle('room-info-home', home);
    this.projectSection.hidden = !home || !this.chat.projectSettings;
    this.usageLink.hidden = !home;
    this.facts.value = roomFacts(room, { renamable, role: this.chat.projectRole?.(room.project_id), worktreeHidden: this.store.worktree.peek().hidden, project: null });
    const resolved = this.shell.store.resolve(target || this.empty);
    if (!this.infoDialog.open) { this.infoDialog.showModal(); this.closed = false; if (!options.fromAddress) this.address.enter(this.pathOf(resolved)); }
    this.showTarget(resolved);
    // On a touch screen the dialog opens on its heading, not in the title field: focus in a text field raises the keyboard over most of the dialog.
    if (matchMedia('(pointer: coarse)').matches && /^(INPUT|TEXTAREA)$/.test(document.activeElement?.tagName || '')) { this.infoDialog.tabIndex = -1; this.infoDialog.focus({ preventScroll: true }); }
    // The home room's settings carry the project's own settings.
    if (!this.projectSection.hidden) this.chat.projectSettings.mount(this.projectSection, room.project_id, room.id);
    this.store.loadWorktree();
    // What is said in a chat between agents never feeds memory or recall, so there is no setting to show.
    this.memorySection.hidden = !!room.agent_chat;
    if (!room.agent_chat) this.store.loadMemory();
    if (!controls.hidden) this.scopeControl.load(room.id);
    this.schedules.load(room);
    this.memberList.load(room.id);
  }

  canRename(room) { return canRename(room, this.chat.projectRole?.(room.project_id), rights.admin); }

  async saveTitle() {
    const input = this.renameForm.elements.title, result = await this.store.saveTitle(input.value);
    if (result === 'empty' || result === 'failed') input.focus();
    else if (result === 'same') input.value = input.value.trim();
    else if (result === 'saved') input.value = this.store.room.peek()?.title ?? input.value;
  }

  changeMemory() { return this.store.changeMemory(this.memorySection.querySelector('select').value); }

  openNotes() {
    const room = this.store.room.peek();
    if (!room) return;
    this.infoDialog.close();
    if (room.kind === 'home') this.chat.openProjectMemory?.(room.project_id); else this.chat.openTaskNotes?.();
  }

  exportRoom(format) { return this.store.exportRoom(format); }

  destroy() {
    for (const stop of this.stopEffects) stop();
    for (const root of this.roots) root.destroy();
    this.store.destroy();
    this.infoDialog.removeEventListener('close', this.onInfoClose); this.infoDialog.close(); this.exportDialog.close();
    this.address.destroy(); this.shell.destroy();
    for (const node of [this.worktreeSection, this.notesSection, this.memorySection, this.usageLink]) node.remove();
  }
}
