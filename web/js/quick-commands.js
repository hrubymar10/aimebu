// Composer quick commands: typing "/" at the start of the draft opens a
// popup listing the built-in /archive and /spawn.
// Arrow keys move, Enter picks, Esc closes. Picking one clears the draft and
// triggers its existing flow.
import { Picker } from './picker.js';

export const BUILTIN_COMMANDS = [
  { name: '/archive', label: 'Archive this room', builtin: 'archive' },
  { name: '/spawn', label: 'Spawn an agent', builtin: 'spawn' },
];

export class QuickCommands {
  constructor(draft, onArchive, onSpawn, canArchive, canSpawn) {
    this.draft = draft;
    this.onArchive = onArchive;
    this.onSpawn = onSpawn;
    this.canArchive = canArchive;
    this.canSpawn = canSpawn;
    this.items = [];
    this.list = new Picker(draft, '', index => this.pick(index));
    this.popup = this.list.popup;
    draft.addEventListener('input', () => this.refresh());
    draft.addEventListener('keydown', event => this.keydown(event));
    draft.addEventListener('blur', () => { setTimeout(() => { if (!draft.hasFocus?.()) this.close(); }, 120); });
  }

  reset() { this.items = []; this.close(); }

  // The popup is active only while the draft is exactly a slash command at
  // the start of the message: "/", "/arc…", but not "hi /arc" or "/a b".
  match() {
    const value = this.draft.value;
    if (!value.startsWith('/') || /\s/.test(value)) return null;
    return value.slice(1).toLowerCase();
  }

  refresh() {
    const query = this.match();
    if (query === null) { this.close(); return; }
    this.items = BUILTIN_COMMANDS.filter(command => command.name.slice(1).startsWith(query)
      && (command.builtin === 'archive' ? this.canArchive?.() : this.canSpawn?.()))
      .map(command => ({ name: command.name.slice(1), builtin: command.builtin, label: command.label }));
    if (!this.items.length) { this.close(); return; }
    this.list.show(this.items.map(item => ({ key: `/${item.name}`, preview: item.label, className: 'quick-command-builtin' })));
  }

  keydown(event) { this.list.keydown(event); }

  pick(index) {
    const item = this.items[index];
    if (!item) return;
    // The room or permissions may have changed while the popup was open.
    if (item.builtin === 'archive' && !this.canArchive?.() || item.builtin === 'spawn' && !this.canSpawn?.()) { this.close(); return; }
    this.close();
    this.draft.value = '';
    this.draft.dispatchEvent(new Event('input', { bubbles: true }));
    if (item.builtin === 'archive') this.onArchive?.();
    if (item.builtin === 'spawn') this.onSpawn?.();
  }

  close() { this.list.close(); this.items = []; }
}
