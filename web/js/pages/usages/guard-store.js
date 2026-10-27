import { batch } from '../../ui.js';
import { ToolStore } from './tool-store.js';
import { GUARD_TOOLS, guardProblem } from '../../limit-guard-format.js';
import { same, clone } from './quota-model.js';

// Settings -> Usages -> Limits before 100%: per tool, the soft threshold (no new turns, and the automatic switcher leaves the profile for
// a usable one) and the hard one (a notice that it is at its limit), with per-profile overrides. Only the owner sees it, and only where the
// server runs the guard.
export class GuardStore extends ToolStore {
  constructor({ source, isEditing }) { super({ source, tools: GUARD_TOOLS, isEditing }); }

  async sync({ owner }) {
    if (!owner) { this.hide(); return; }
    await this.load();
  }

  async load() {
    if (!this.alive) return;
    const generation = ++this.generation;
    let data;
    try { data = await this.source.load(); } catch (error) { if (this.alive && generation === this.generation && [403, 404, 503].includes(error.status)) this.hide(); return; }
    if (!this.alive || generation !== this.generation) return;
    const configChanged = !!this.data.value && GUARD_TOOLS.some(([tool]) => !same(this.data.value.tools?.[tool]?.config, data.tools?.[tool]?.config));
    if (this.data.value && same(this.data.value, data) && this.visible.value) return;
    this.accept(data, { freeze: this.visible.value && this.isEditing() && configChanged });
  }

  async save(tool) {
    const problem = guardProblem(this.drafts.value[tool]);
    if (problem) { batch(() => { this.setText('errors', tool, problem); this.setText('notes', tool, ''); }); return; }
    if (this.busy.value.has(tool)) return;
    batch(() => { this.busy.value = new Map(this.busy.value).set(tool, ''); this.setText('errors', tool, ''); this.setText('notes', tool, ''); });
    try {
      const result = await this.source.save(tool, this.drafts.value[tool]);
      if (!this.alive) return;
      batch(() => {
        if (this.data.value) {
          this.data.value = { ...this.data.value, tools: { ...this.data.value.tools, [tool]: result } };
          this.bases[tool] = clone(result.config);
          this.drafts.value = { ...this.drafts.value, [tool]: clone(result.config) };
        }
        this.setText('notes', tool, 'Saved.');
      });
    } catch (error) { if (this.alive) this.setText('errors', tool, error.message || 'The change was not accepted'); } finally {
      if (this.alive) { const left = new Map(this.busy.value); left.delete(tool); this.busy.value = left; }
    }
  }
}
