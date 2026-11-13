import { html } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { ProjectPage } from './project-page.js';
import { ProjectStore } from './store.js';

export class ProjectSettings {
  constructor(source, projectContext) {
    this.store = new ProjectStore(source, projectContext);
    this.body = document.querySelector('#project-settings-body');
    this.mounts = [];
    this.page = mountPreactPage(this.body, () => html`<${ProjectPage} store=${this.store} />`);
  }

  set onLifecycle(callback) { this.store.onLifecycle = callback; }
  set onRemoved(callback) { this.store.onRemoved = callback; }
  set onRenamed(callback) { this.store.onRenamed = callback; }
  set onFlowApplied(callback) { this.store.onFlowApplied = callback; }

  load(id) { return this.store.load(id); }

  mount(parent, projectID) {
    this.mounts = this.mounts.filter(item => item.parent !== parent);
    this.mounts.push({ parent, projectID });
    if (this.body.parentElement !== parent) parent.append(this.body);
    this.load(projectID);
  }

  release(parent) {
    const top = this.mounts.at(-1);
    this.mounts = this.mounts.filter(item => item.parent !== parent);
    const previous = this.mounts.at(-1);
    if (top?.parent === parent && previous) { previous.parent.append(this.body); this.load(previous.projectID); }
  }

  isMountedIn(parent) { return this.body.parentElement === parent; }
  destroy() { this.store.destroy(); this.page.destroy(); }
}
