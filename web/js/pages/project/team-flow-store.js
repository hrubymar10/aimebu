import { batch, signal } from '../../ui.js';
import { teamFlowSource } from '../../team-flow-source.js';

// Shared by the inline home setup and Project settings. Every async response
// belongs to a project and selection generation, so a late preview cannot
// overwrite the one the person is looking at now.
export class TeamFlowStore {
  constructor({ source = teamFlowSource, onApplied = null, canPreview = () => true } = {}) {
    this.source = source;
    this.onApplied = onApplied;
    this.canPreview = canPreview;
    this.projectID = '';
    this.templates = signal([]);
    this.state = signal(null);
    this.selected = signal('');
    this.preview = signal(null);
	this.removals = signal([]);
    this.loading = signal(false);
    this.previewing = signal(false);
    this.applying = signal(false);
    this.error = signal('');
    this.generation = 0;
    this.previewGeneration = 0;
    this.alive = true;
  }

  reset() {
    this.generation++;
    this.previewGeneration++;
    this.projectID = '';
    batch(() => {
      this.templates.value = []; this.state.value = null; this.selected.value = '';
      this.preview.value = null; this.removals.value = []; this.loading.value = false; this.previewing.value = false;
      this.applying.value = false; this.error.value = '';
    });
  }

  async load(projectID) {
    if (!this.alive) return;
    this.reset();
    if (!projectID) return;
    this.projectID = projectID;
    const generation = this.generation;
    this.loading.value = true;
    try {
      const [catalog, state] = await Promise.all([this.source.templates(), this.source.get(projectID)]);
      if (!this.alive || generation !== this.generation) return;
      const templates = catalog.templates || [];
      const selected = state.key || templates[0]?.key || '';
      batch(() => { this.templates.value = templates; this.state.value = state; this.selected.value = selected; this.loading.value = false; });
      if (selected && this.canPreview(projectID)) await this.select(selected);
    } catch (error) {
      if (this.alive && generation === this.generation) batch(() => { this.loading.value = false; this.error.value = error.message; });
    }
  }

  async select(key) {
    if (!this.alive || !this.projectID || !this.canPreview(this.projectID) || !this.templates.peek().some(item => item.key === key)) return;
    const generation = this.generation, previewGeneration = ++this.previewGeneration, projectID = this.projectID;
    batch(() => { this.selected.value = key; this.preview.value = null; this.removals.value = []; this.previewing.value = true; this.error.value = ''; });
    try {
      const preview = await this.source.preview(projectID, key);
      if (this.alive && generation === this.generation && previewGeneration === this.previewGeneration) this.preview.value = preview;
    } catch (error) {
      if (this.alive && generation === this.generation && previewGeneration === this.previewGeneration) this.error.value = error.message;
    } finally {
      if (this.alive && generation === this.generation && previewGeneration === this.previewGeneration) this.previewing.value = false;
    }
  }

  toggleRemoval(conflict, checked) {
    const matches = item => item.agent_id === conflict.agent_id && item.room_id === conflict.room_id;
    const remaining = this.removals.peek().filter(item => !matches(item));
    this.removals.value = checked ? [...remaining, { agent_id: conflict.agent_id, room_id: conflict.room_id }] : remaining;
  }

  async apply() {
    const projectID = this.projectID, state = this.state.peek(), key = this.selected.peek(), generation = this.generation;
    if (!this.alive || !projectID || !state || !key || !this.preview.peek() || this.applying.peek() || this.previewing.peek()) return;
    this.applying.value = true; this.error.value = '';
    try {
      const result = await this.source.apply(projectID, key, state.rev, this.removals.peek());
      if (!this.alive || generation !== this.generation) return;
      const template = this.templates.peek().find(item => item.key === result.key);
      batch(() => {
        this.state.value = { ...state, ...result, template, review_pending: false };
        this.preview.value = null; this.removals.value = []; this.applying.value = false;
      });
      await this.onApplied?.(projectID, result);
    } catch (error) {
      if (this.alive && generation === this.generation) batch(() => { this.error.value = error.message; this.applying.value = false; });
    }
  }

  async completeTeamSetup(projectID, result) {
    const state = this.state.peek();
    if (!this.alive || !projectID || this.projectID !== projectID || !state) return;
    const template = this.templates.peek().find(item => item.key === result.flow?.key);
    batch(() => {
      this.state.value = { ...state, ...result.flow, template, review_pending: false };
      this.preview.value = null;
    });
    await this.onApplied?.(projectID, result.flow);
  }

  destroy() { this.alive = false; this.reset(); }
}
