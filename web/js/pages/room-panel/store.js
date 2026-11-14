import { batch, signal } from '../../ui.js';
import { request, download } from '../../api.js';
import { confirmDialog, promptDialog } from '../../dialogs.js';
import { memoryValue, worktreeView } from './model.js';

const IDLE_WORKTREE = { hidden: true, status: '', home: false, view: null };

// The room's header and settings dialog: the facts of the room being shown and the calls that change it (title, memory setting, worktree, archive,
// reopen, leave, remove, hide, export). Every call carries the lifecycle of the panel (it moves when another room is shown, on a reset and on a destroy)
// and a ticket of its operation, and says nothing and starts nothing once either is gone, or once a newer call of its operation was started. The
// memory read and change share a ticket, and so do the worktree read, create and remove: each of them ends by showing the state the others would
// otherwise overwrite. `host` is what the chat gives it: roles, archived projects and the app's callbacks.
export class RoomPanelStore {
  constructor(host) {
    this.host = host; this.epoch = 0; this.alive = true; this.tickets = {};
    this.room = signal(null); this.version = signal(0);
    this.renamable = signal(false); this.renameEpoch = signal(0); this.renameStatus = signal({ text: '', error: false }); this.renameBusy = signal(false);
    this.infoError = signal(''); this.busy = signal({ lifecycle: false, leave: false, remove: false, hide: false });
    this.worktree = signal(IDLE_WORKTREE); this.memory = signal({ value: 'inherit', status: '', disabled: true });
  }

  touch() { this.version.value++; }
  begin(operation) { return { operation, epoch: this.epoch, ticket: this.tickets[operation] = (this.tickets[operation] || 0) + 1 }; }
  current(call, roomID) { return call.epoch === this.epoch && call.ticket === this.tickets[call.operation] && this.room.peek()?.id === roomID; }
  setBusy(patch) { this.busy.value = { ...this.busy.peek(), ...patch }; }

  setRoom(room) {
    if (!this.alive) return;
    batch(() => {
      if (this.room.peek()?.id !== room.id) { this.epoch++; this.renameBusy.value = false; this.setBusy({ lifecycle: false, leave: false, remove: false, hide: false }); }
      this.room.value = room; this.touch();
    });
  }

  reset() {
    batch(() => {
      this.room.value = null; this.renamable.value = false; this.renameStatus.value = { text: '', error: false }; this.renameBusy.value = false; this.infoError.value = '';
      this.setBusy({ lifecycle: false, leave: false, remove: false, hide: false });
      this.worktree.value = { ...this.worktree.peek(), status: '' }; this.memory.value = { ...this.memory.peek(), status: '' }; this.touch();
    });
  }

  destroy() { this.reset(); this.alive = false; }

  // The dialog opens on this room: the title field starts from its title, and what an earlier opening said is gone.
  open(renamable) {
    batch(() => { this.renamable.value = renamable; this.renameStatus.value = { text: '', error: false }; this.renameEpoch.value++; this.infoError.value = ''; this.setBusy({ lifecycle: false, leave: false, remove: false, hide: false }); });
  }

  clearRenameStatus() { if (this.renameStatus.peek().text) this.renameStatus.value = { text: '', error: false }; }

  async saveTitle(value) {
    const room = this.room.peek(), call = this.begin('title');
    if (!room) return;
    const title = value.trim();
    this.renameStatus.value = { text: '', error: false };
    if (!title) { this.renameStatus.value = { text: 'A room needs a title.', error: true }; return 'empty'; }
    // The home room's title is the project's name, which this field may not show (a title that drifted from it), so only the server can tell a home
    // room's rename from no change.
    if (room.kind !== 'home' && title === room.title) { this.renameStatus.value = { text: 'Nothing changed.', error: false }; return 'same'; }
    this.renameBusy.value = true;
    try {
      const updated = await request(`/api/rooms/${encodeURIComponent(room.id)}`, { method: 'PATCH', body: { title } });
      if (!this.current(call, room.id)) return;
      room.title = updated.title; this.renameStatus.value = { text: 'Saved.', error: false }; this.touch();
      return 'saved';
    } catch (error) {
      if (this.current(call, room.id)) { this.renameStatus.value = { text: error.message, error: true }; return 'failed'; }
    } finally { if (this.current(call, room.id)) this.renameBusy.value = false; }
  }

  async loadMemory() {
    const room = this.room.peek(), call = this.begin('memory');
    if (!room) return;
    this.memory.value = { ...this.memory.peek(), value: memoryValue(room), disabled: true };
    try {
      const project = await request(`/api/projects/${encodeURIComponent(room.project_id)}/memory`);
      if (!this.current(call, room.id)) return;
      this.memory.value = { ...this.memory.peek(), status: project.enabled ? 'Project memory is on.' : 'Project memory is off.', disabled: !project.can_edit };
    } catch (error) { if (this.current(call, room.id)) this.memory.value = { ...this.memory.peek(), status: error.message || 'Could not load memory setting' }; }
  }

  async changeMemory(value) {
    const room = this.room.peek(), call = this.begin('memory');
    if (!room) return;
    this.memory.value = { ...this.memory.peek(), value, disabled: true };
    try {
      const result = await request(`/api/rooms/${encodeURIComponent(room.id)}/memory-enabled`, { method: 'PUT', body: { enabled: value === 'off' ? false : null } });
      if (!this.current(call, room.id)) return;
      room.memory_enabled = result.memory_enabled;
      this.memory.value = { value, status: value === 'off' ? 'Room message memory is off.' : 'Room follows the project setting.', disabled: false };
    } catch (error) {
      if (this.current(call, room.id)) this.memory.value = { value: memoryValue(room), status: error.message || 'Could not change room memory', disabled: false };
    }
  }

  setWorktreeStatus(status) { this.worktree.value = { ...this.worktree.peek(), status }; }

  async loadWorktree() {
    const room = this.room.peek(), call = this.begin('worktree');
    if (room?.kind === 'home') {
      // The home room always works in the project folder; only task rooms get a worktree of their own.
      this.worktree.value = { hidden: false, home: true, view: null, status: 'The home room works in the project folder. Task rooms can have their own Git worktree.' };
      return;
    }
    if (!room || room.kind !== 'task') { this.worktree.value = { ...this.worktree.peek(), hidden: true }; return; }
    this.worktree.value = { hidden: false, home: false, view: null, status: '' };
    try {
      const item = await request(`/api/rooms/${room.id}/worktree`);
      if (!this.current(call, room.id)) return;
      this.worktree.value = { hidden: false, home: false, view: worktreeView(item, room), status: worktreeView(item, room).status };
    } catch (error) { if (this.current(call, room.id)) this.setWorktreeStatus(error.message); }
  }

  async createWorktree() {
    const room = this.room.peek(), call = this.begin('worktree');
    if (!room) return;
    const baseRef = await promptDialog('Base branch or ref (leave blank for current HEAD)', { title: 'Create worktree', label: 'Base branch or ref', placeholder: 'Current HEAD', confirm: 'Create' });
    if (baseRef === null || !this.current(call, room.id)) return;
    try { await request(`/api/rooms/${room.id}/worktree`, { method: 'POST', body: { enabled: true, base_ref: baseRef.trim() } }); if (!this.current(call, room.id)) return; await this.loadWorktree(); }
    catch (error) { if (this.current(call, room.id)) this.setWorktreeStatus(error.message); }
  }

  async removeWorktree() {
    const room = this.room.peek(), call = this.begin('worktree');
    const ready = this.worktree.peek().view?.removeLabel === 'Remove worktree';
    if (room?.lifecycle === 'open' && !ready) {
      try { await request(`/api/rooms/${room.id}/worktree`, { method: 'DELETE', body: {} }); if (!this.current(call, room.id)) return; await this.loadWorktree(); }
      catch (error) { if (this.current(call, room.id)) this.setWorktreeStatus(error.message); }
      return;
    }
    const prompt = room?.lifecycle === 'open' ? 'Archive this room and remove its worktree? Agents homed here stop. Clean merged worktrees are removed; a dirty or unmerged worktree will be preserved.'
      : 'Remove this archived room worktree? Clean merged worktrees are removed; a dirty or unmerged worktree will be preserved.';
    if (!room || !await confirmDialog(prompt, { title: 'Remove worktree', confirm: 'Remove', danger: true })) return;
    if (!this.current(call, room.id)) return;
    try {
      let result = await request(`/api/rooms/${room.id}/worktree`, { method: 'DELETE', body: { force: false } });
      if (!this.current(call, room.id)) return;
      if (result.state === 'preserved' && await confirmDialog(`Worktree is preserved: ${result.reason}. Remove its checkout and discard uncommitted local files? Committed changes remain on the room branch.`, { title: 'Discard local files', confirm: 'Discard and remove', danger: true })) {
        if (!this.current(call, room.id)) return;
        result = await request(`/api/rooms/${room.id}/worktree`, { method: 'DELETE', body: { force: true, confirm: `REMOVE ${room.id}` } });
        if (!this.current(call, room.id)) return;
      }
      await this.loadWorktree();
    } catch (error) { if (this.current(call, room.id)) this.setWorktreeStatus(error.message); }
  }

  async changeLifecycle(intent = 'toggle') {
    const room = this.room.peek();
    if (intent === 'archive' && !this.host.canArchive(room)) return;
    if (!room || room.kind === 'home' || room.lifecycle === 'closing') return;
    const call = this.begin('lifecycle'), closing = intent === 'archive' || room.lifecycle !== 'closed';
    if (closing && !await confirmDialog(`Archive ${room.title}? Agents homed here stop, and open questions are withdrawn. Its history remains available.`, { title: 'Archive room', confirm: 'Archive' })) return;
    if (!this.current(call, room.id)) return;
    this.setBusy({ lifecycle: true }); this.infoError.value = '';
    const path = `/api/rooms/${room.id}/${closing ? 'close' : 'reopen'}`;
    try {
      let updated;
      try { updated = await request(path, { method: 'POST', body: { lifecycle_rev: room.lifecycle_rev || 0 } }); }
      catch (error) {
        if (!this.current(call, room.id)) return;
        if (!closing || error.code !== 'open_children' || !await confirmDialog(`Archive ${room.title} and all its open child rooms? Agents homed in these rooms stop, and open questions are withdrawn.`, { title: 'Archive room and its child rooms', confirm: 'Archive all' })) throw error;
        if (!this.current(call, room.id)) return;
        updated = await request(path, { method: 'POST', body: { lifecycle_rev: room.lifecycle_rev || 0, cascade: true } });
      }
      if (!this.current(call, room.id)) return;
      this.setBusy({ lifecycle: false }); this.host.setFacts(updated);
      await this.host.mutation(updated);
      if (!this.current(call, room.id)) return;
      this.host.closeInfo();
    } catch (error) {
      if (this.current(call, room.id)) { this.host.chatError(error.message); if (this.host.infoOpen()) this.infoError.value = error.message; this.setBusy({ lifecycle: false }); }
    }
  }

  async leave() {
    const room = this.room.peek(), call = this.begin('leave');
    if (!room?.agent_chat) return;
    this.setBusy({ leave: true }); this.infoError.value = '';
    try {
      await request(`/api/rooms/${room.id}/leave`, { method: 'POST' });
      if (!this.current(call, room.id)) return;
      this.host.closeInfo();
      await this.host.left(room);
    } catch (error) { if (this.current(call, room.id)) this.infoError.value = error.message || 'Could not leave the chat'; }
    if (this.current(call, room.id)) this.setBusy({ leave: false });
  }

  async remove() {
    const room = this.room.peek();
    if (!room || room.kind === 'home' || room.kind === 'dm' || room.lifecycle === 'closing') return;
    const call = this.begin('remove');
    if (!await confirmDialog(`Delete ${room.title} for good? Agents homed here stop. Its messages, reactions, notes, attachments and its usage and cost records are deleted and cannot be restored.`, { title: 'Delete room', confirm: 'Delete for good', danger: true })) return;
    if (!this.current(call, room.id)) return;
    this.setBusy({ remove: true }); this.infoError.value = '';
    try {
      await request(`/api/rooms/${room.id}`, { method: 'DELETE' });
      if (!this.current(call, room.id)) return;
      this.host.closeInfo();
      await this.host.removed(room);
    } catch (error) { if (this.current(call, room.id)) { this.infoError.value = error.message || 'Could not remove the room'; this.setBusy({ remove: false }); } }
  }

  async hide() {
    const room = this.room.peek(), call = this.begin('hide');
    if (!room) return;
    this.setBusy({ hide: true });
    const done = await this.host.hide(room);
    if (this.current(call, room.id)) this.setBusy({ hide: false });
    if (done && this.current(call, room.id)) this.host.closeInfo();
  }

  async exportRoom(format) {
    const room = this.room.peek(), call = this.begin('export');
    if (!room) return;
    try {
      // The server writes the file (every message of the room, with replies, attachments and reactions); the page only saves it.
      const { blob, filename } = await download(`/api/rooms/${room.id}/export?format=${format === 'json' ? 'json' : 'markdown'}`);
      if (!this.current(call, room.id)) return;
      const url = URL.createObjectURL(blob);
      const anchor = document.createElement('a'); anchor.href = url;
      anchor.download = filename || `${room.title.replace(/[^a-z0-9_-]+/gi, '-').replace(/^-|-$/g, '') || 'room'}.${format === 'json' ? 'json' : 'md'}`;
      anchor.click(); setTimeout(() => URL.revokeObjectURL(url), 1000);
      this.host.closeExport();
    } catch (error) { if (this.current(call, room.id)) { this.host.chatError(error.message); this.host.closeExport(); } }
  }
}
