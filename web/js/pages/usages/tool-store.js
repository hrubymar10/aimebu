import { signal, batch } from '../../ui.js';
import { clone, same } from './quota-model.js';

// What the two per-tool blocks of the Usages page (automatic switching, limits before 100%) have in common: the server's copy of each
// tool's configuration, the owner's drafts of it, one error and note per tool, and the rule for when the server's copy may replace a draft.
// A draft that has been edited is never replaced. While a field of the block has focus, the server's copy of the other tools waits until
// focus leaves, so what is under the reader's hands does not move. A read carries a generation: only the latest answer counts.
export class ToolStore {
  constructor({ source, tools, isEditing }) {
    this.source = source;
    this.tools = tools;
    this.isEditing = isEditing;
    this.data = signal(null);
    this.drafts = signal({});
    this.visible = signal(false);
    this.errors = signal({});
    this.notes = signal({});
    this.busy = signal(new Map());
    this.generation = 0;
    this.pending = null;
    // The server's configuration each draft was made from: a draft is edited when it differs from that, whatever the server says now.
    this.bases = {};
    this.alive = true;
  }

  edited(tool) { return !!this.drafts.value[tool] && !same(this.drafts.value[tool], this.bases[tool]); }
  dirty(tool) { return !!this.data.value?.tools?.[tool] && !!this.drafts.value[tool] && !same(this.drafts.value[tool], this.data.value.tools[tool].config); }
  savable(tool) { return this.dirty(tool) || this.data.value?.tools?.[tool]?.saved === false; }

  hide() {
    this.generation++;
    this.pending = null;
    this.bases = {};
    batch(() => { this.visible.value = false; this.data.value = null; this.drafts.value = {}; });
  }

  // Applies a server answer. `freeze` tells whether the reader is typing in this block: then the drafts of tools they have not edited are
  // kept aside and applied once focus leaves.
  accept(data, { freeze }) {
    const next = { ...this.drafts.value }, bases = { ...this.bases };
    for (const [tool] of this.tools) if (data.tools?.[tool] && !this.edited(tool)) { next[tool] = clone(data.tools[tool].config); bases[tool] = clone(data.tools[tool].config); }
    batch(() => {
      this.data.value = data;
      this.visible.value = true;
      if (freeze) this.pending = { drafts: next, bases };
      else { this.pending = null; this.bases = bases; this.drafts.value = next; }
    });
  }

  // Focus left the block: the server's copy that waited is applied to the tools that were not edited meanwhile.
  settle() {
    if (!this.pending || this.isEditing()) return;
    const waiting = this.pending, next = { ...this.drafts.value };
    this.pending = null;
    for (const [tool] of this.tools) if (waiting.drafts[tool] && !this.edited(tool)) { next[tool] = waiting.drafts[tool]; this.bases[tool] = waiting.bases[tool]; }
    this.drafts.value = next;
  }

  // A change to a draft: messages that belonged to the earlier state go, unless it is only a number being typed.
  edit(tool, change, { typing = false } = {}) {
    const next = { ...this.drafts.value, [tool]: clone(this.drafts.value[tool]) };
    change(next[tool]);
    batch(() => {
      this.drafts.value = next;
      if (!typing) { this.setText('errors', tool, ''); this.setText('notes', tool, ''); }
    });
  }
  setText(which, tool, text) { this[which].value = { ...this[which].value, [tool]: text }; }
  discard(tool) {
    this.bases[tool] = clone(this.data.value.tools[tool].config);
    batch(() => { this.drafts.value = { ...this.drafts.value, [tool]: clone(this.data.value.tools[tool].config) }; this.setText('errors', tool, ''); this.setText('notes', tool, ''); });
  }

  destroy() { this.alive = false; this.generation++; }
}
