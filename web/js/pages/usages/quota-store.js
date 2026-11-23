import { signal, batch } from '../../ui.js';
import { ToolStore } from './tool-store.js';
import { TOOLS, clone, same, numbersProblem } from './quota-model.js';

// Settings -> Usages -> Automatic account switching: the owner's per-tool ordered profile list, limits, protected profiles, after-reset
// order, and the server's decisions. Only the owner of an enabled account switcher sees it.
export class QuotaStore extends ToolStore {
  constructor({ source, isEditing }) {
    super({ source, tools: TOOLS, isEditing });
    this.loadError = signal('');
    // Which <details> are open, by `tool:name`: a redraw must not close what the reader opened.
    this.open = signal(new Set());
    // The control that should hold focus once the next render has put it there: { key }.
    this.focus = signal(null);
  }

  // Called by the Usages page after every read: only the owner of an enabled account switcher sees this block.
  async sync({ owner, enabled }) {
    if (!owner || !enabled) { this.hide(); return; }
    await this.load();
  }

  async load() {
    if (!this.alive) return;
    const generation = ++this.generation;
    let data;
    try { data = await this.source.load(); } catch (error) {
      if (!this.alive || generation !== this.generation) return;
      if (error.status === 403 || error.status === 404) { this.hide(); return; } // not an owner, or not served
      this.loadError.value = error.message || 'Could not load automatic switching';
      return;
    }
    if (!this.alive || generation !== this.generation) return;
    const unchanged = this.data.value && !this.loadError.value && same(this.data.value, data) && this.visible.value;
    const editedBefore = new Set(TOOLS.filter(([tool]) => this.edited(tool)).map(([tool]) => tool));
    const configChanged = !!this.data.value && TOOLS.some(([tool]) => !same(this.data.value.tools?.[tool]?.config, data.tools?.[tool]?.config) || this.data.value.tools?.[tool]?.saved !== data.tools?.[tool]?.saved);
    this.loadError.value = '';
    if (unchanged) return; // a live push that changed nothing must not rebuild the block under the reader
    // The server's status changes on every evaluation: a reader typing keeps their field, and a tool they have edited keeps its draft. A
    // focused field whose tool has no typed edits is brought up to date at once when the configuration changed; otherwise the server's copy
    // waits until focus leaves.
    const focusedTool = document.activeElement?.closest?.('.quota-tool')?.dataset.tool;
    this.accept(data, { freeze: this.visible.value && this.isEditing() && (!configChanged || editedBefore.has(focusedTool)) });
  }

  setOpen(key, open) {
    const next = new Set(this.open.value);
    if (open) next.add(key); else next.delete(key);
    this.open.value = next;
  }

  change(tool, change, focus = '') {
    batch(() => { this.edit(tool, change); if (focus) this.focus.value = { key: focus }; });
  }

  async save(tool) {
    const problem = numbersProblem(this.drafts.value[tool]);
    if (problem) { batch(() => { this.setText('errors', tool, problem); this.setText('notes', tool, ''); }); return; }
    await this.run(tool, () => this.source.save(tool, this.drafts.value[tool]), 'Saved.', 'Saving… (waits for a running switch)');
  }
  async act(tool, action) { await this.run(tool, () => this.source[action](tool), action === 'rearm' ? 'Re-armed.' : 'Re-enabled.', 'Applying… (waits for a running switch)', true); }

  // The server takes the same lock a running switch holds, so a save, re-arm or re-enable can wait for that switch: say so while it does.
  async run(tool, call, done, pending, keepDraft = false) {
    if (this.busy.value.has(tool)) return;
    const busy = new Map(this.busy.value).set(tool, pending);
    batch(() => { this.busy.value = busy; this.setText('errors', tool, ''); this.setText('notes', tool, ''); });
    try {
      const result = await call();
      if (!this.alive) return;
      batch(() => {
        if (this.data.value) this.data.value = { ...this.data.value, tools: { ...this.data.value.tools, [tool]: result } };
        if (this.data.value && (!keepDraft || !this.edited(tool))) { this.bases[tool] = clone(result.config); this.drafts.value = { ...this.drafts.value, [tool]: clone(result.config) }; }
        this.setText('notes', tool, done);
      });
    } catch (error) { if (this.alive) this.setText('errors', tool, error.message || 'The change was not accepted'); } finally {
      if (this.alive) { const left = new Map(this.busy.value); left.delete(tool); batch(() => { this.busy.value = left; this.focus.value = { key: `${tool}-note` }; }); }
    }
  }
}
