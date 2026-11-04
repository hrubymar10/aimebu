import { html, effect } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { rights } from '../../api.js';
import { RoomNotifySettings } from '../../room-notify-settings.js';
import { Schedules } from '../../schedules.js';
import { RoomMembers } from '../members/page.js';
import { RoomScopeControl, RoomUsage } from '../project/room-panels.js';
import { InfoError, MemorySection, NotesSection, RenameForm, RoomActions, RoomFacts, ViewActions, WorktreeSection } from './room-panel.js';
import { canRename, headerStatus, kindIcon, membersLabel, notesView, roomPath } from './model.js';
import { RoomPanelStore } from './store.js';

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
      chatError: text => { chat.error.textContent = text; }, infoOpen: () => this.infoDialog.open, closeInfo: () => this.infoDialog.close(), closeExport: () => this.exportDialog.close(),
    };
    this.store = new RoomPanelStore(host);
    this.worktreeSection = section('room-worktree'); this.notesSection = section('room-notes', 'room-notes-title'); this.memorySection = section('room-memory-setting');
    const error = document.querySelector('#room-info-error');
    error.before(this.worktreeSection, this.notesSection, this.memorySection);
    const mount = (container, view) => { container.replaceChildren(); return mountPreactPage(container, view); };
    const store = this.store;
    this.roots = [
      mount(this.renameForm, () => html`<${RenameForm} store=${store} />`),
      mount(document.querySelector('#room-info-facts'), () => html`<${RoomFacts} store=${store} />`),
      mount(document.querySelector('#room-view-actions'), () => html`<${ViewActions} store=${store} />`),
      mount(document.querySelector('.room-info-actions'), () => html`<${RoomActions} store=${store} onClose=${() => this.infoDialog.close()} />`),
      mount(error, () => html`<${InfoError} store=${store} />`),
      mount(this.worktreeSection, () => html`<${WorktreeSection} store=${store} />`),
      mount(this.memorySection, () => html`<${MemorySection} store=${store} />`),
      mount(this.notesSection, () => html`<${NotesSection} store=${store} onOpen=${() => this.openNotes()} />`),
    ];
    this.renameForm.onsubmit = event => { event.preventDefault(); this.saveTitle(); };
    this.notifySettings = new RoomNotifySettings(document.querySelector('#room-member-list'), (field, value) => chat.setRoomPreference?.(chat.roomView?.(chat.room?.id) || chat.room, field, value), () => chat.roomView?.(chat.room?.id) || chat.room);
    this.scopeControl = new RoomScopeControl(document.querySelector('#room-scope-control'));
    this.schedules = new Schedules(document.querySelector('#room-schedules'));
    this.memberList = new RoomMembers(document.querySelector('#room-member-list'));
    this.roomUsage = new RoomUsage(document.querySelector('#room-usage'));
    document.querySelector('#room-info').onclick = () => this.show();
    this.onInfoClose = () => chat.projectSettings?.release(this.projectSection);
    this.infoDialog.addEventListener('close', this.onInfoClose);
    document.querySelector('#room-export').onclick = () => this.exportDialog.showModal();
    document.querySelector('#room-export-close').onclick = () => this.exportDialog.close();
    for (const button of this.exportDialog.querySelectorAll('[data-format]')) button.onclick = () => this.exportRoom(button.dataset.format);
    this.stopEffects = [this.header(), effect(() => { this.worktreeSection.hidden = store.worktree.value.hidden; }), effect(() => { store.version.value; const room = store.room.value; this.notesSection.hidden = !room || notesView(room).hidden; })];
  }

  // The header: the room's kind icon, title, member count and what its lifecycle or project says.
  header() {
    const title = document.querySelector('#room-title'), crumbs = document.querySelector('#room-crumbs'), icon = document.querySelector('#room-icon'), members = document.querySelector('#room-members');
    return effect(() => {
      this.store.version.value;
      const room = this.store.room.value;
      if (!room) { title.textContent = 'Choose a room'; icon.hidden = true; members.hidden = true; crumbs.replaceChildren(); crumbs.hidden = true; return; }
      this.crumbs(crumbs, room);
      title.textContent = room.title; // a rename by someone else reaches here through the room list
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

  setRoom(room) { this.store.setRoom(room); }

  reset() {
    this.store.reset();
    this.infoDialog.close(); this.exportDialog.close();
    this.scopeControl.reset(); this.schedules.reset(); this.memberList.reset();
  }

  show() {
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
    this.infoDialog.classList.toggle('room-info-home', home);
    this.projectSection.hidden = !home || !this.chat.projectSettings;
    this.infoDialog.showModal();
    // The home room's settings carry the project's own settings.
    if (!this.projectSection.hidden) this.chat.projectSettings.mount(this.projectSection, room.project_id);
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
    for (const node of [this.worktreeSection, this.notesSection, this.memorySection]) node.remove();
  }
}
