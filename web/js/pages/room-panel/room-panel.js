import { html, useLayoutEffect, useRef } from '../../ui.js';
import { IconButton } from '../../components/icon-button.js';
import { canLeave, facts, lifecycleControl, notesView, removeControl } from './model.js';

export function RenameForm({ store }) {
  const input = useRef(null), status = store.renameStatus.value, epoch = store.renameEpoch.value;
  useLayoutEffect(() => { const room = store.room.peek(); if (input.current && room) input.current.value = room.title; }, [epoch]);
  return html`<label for="room-rename-title">Title</label><div class="room-rename-row"><input id="room-rename-title" name="title" maxlength="80" autocomplete="off" required aria-describedby="room-rename-status" ref=${input} onInput=${() => store.clearRenameStatus()} /><${IconButton} id="room-rename-save" type="submit" name="check" label="Save title" disabled=${store.renameBusy.value} /></div><p id="room-rename-status" class=${status.error ? 'room-rename-status error' : 'room-rename-status'} role="status">${status.text}</p>`;
}

export function RoomFacts({ store }) {
  store.version.value;
  const room = store.room.value;
  if (!room) return null;
  return facts(room, store.renamable.value).map(fact => html`<dt class=${fact.debug ? 'room-fact-debug' : undefined}>${fact.label}</dt><dd class=${fact.debug ? 'room-fact-debug' : undefined}>${fact.value}</dd>`);
}

export function ViewActions({ store }) {
  return html`<button id="room-hide" class="btn room-hide-btn" type="button" title="Hide this room from your list. It stays for everyone else." disabled=${store.busy.value.hide} onClick=${() => store.hide()}><span class="icon-mask icon-mask-eye-off" aria-hidden="true"></span>Hide room</button>`;
}

export function RoomActions({ store }) {
  store.version.value;
  const room = store.room.value, busy = store.busy.value;
  const life = room ? lifecycleControl(room) : { hidden: true, disabled: false, glyph: 'archive', label: 'Archive room' };
  const remove = room ? removeControl(room, store.host.role(room.project_id)) : { hidden: true, disabled: true, title: '' };
  return html`<${IconButton} id="room-lifecycle" name=${life.glyph} label=${life.label} hidden=${life.hidden} disabled=${life.disabled || busy.lifecycle} onClick=${() => store.changeLifecycle()} />
    <${IconButton} id="room-leave" name="left" label="Leave chat" hidden=${!room || !canLeave(room)} disabled=${busy.leave} onClick=${() => store.leave()} />
    <${IconButton} id="room-remove" name="trash" label="Delete room" danger title=${remove.title} hidden=${remove.hidden} disabled=${remove.disabled || busy.remove} onClick=${() => store.remove()} />`;
}

export const InfoError = ({ store }) => store.infoError.value;

export function WorktreeSection({ store }) {
  const state = store.worktree.value, view = state.view;
  return html`<h3>Git worktree</h3><p role="status">${state.status}</p><button type="button" class="btn btn-primary" hidden=${state.home || !view || view.createHidden} onClick=${() => store.createWorktree()}>${view?.createLabel || 'Create worktree'}</button><button type="button" class="btn btn-danger" hidden=${state.home || !view || view.removeHidden} onClick=${() => store.removeWorktree()}>${view?.removeLabel || 'Remove worktree'}</button>`;
}

export function MemorySection({ store }) {
  const memory = store.memory.value;
  return html`<h3>Room memory</h3><label>Message memory <select value=${memory.value} disabled=${memory.disabled} onChange=${event => store.changeMemory(event.currentTarget.value)}><option value="inherit">Use project setting</option><option value="off">Off in this room</option></select></label><p role="status">${memory.status}</p><p>Turning this off prevents this room’s messages from feeding memory or recall. It does not erase saved memory.</p>`;
}

export function NotesSection({ store, onOpen }) {
  store.version.value;
  const room = store.room.value;
  if (!room) return null;
  const view = notesView(room);
  return html`<h3 id="room-notes-title">${view.heading}</h3><p class="settings-intro">${view.text}</p><button type="button" class="btn" onClick=${onOpen}>${view.button}</button>`;
}
