import { html } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { KnowledgePage } from './knowledge-page.js';
import { KnowledgeStore } from './store.js';

// The toolbar's two buttons stay in index.html: this adapter is the only writer of the room button's hidden, title and label.
export class KnowledgePanel {
  constructor(projectContext, roomContext) {
    this.dialog = document.querySelector('#knowledge-dialog');
    this.store = new KnowledgeStore({ projectContext, roomContext, dialog: this.dialog });
    this.taskButton = document.querySelector('#task-notes-toggle');
    this.page = mountPreactPage(this.dialog, () => html`<${KnowledgePage} store=${this.store} onClose=${() => this.dialog.close()} />`);
    document.querySelector('#knowledge-toggle').onclick = () => this.openProject();
    this.taskButton.onclick = () => { const room = roomContext(); if (room?.kind === 'home') this.openProjectFacts(room.project_id); else this.openTask(); };
    this.dialog.addEventListener('close', () => this.store.closed());
  }

  setRoom(room) {
    const home = room?.kind === 'home';
    this.taskButton.hidden = room?.kind !== 'task' && !home;
    const label = home ? 'Project memory' : 'Task notes';
    this.taskButton.title = label; this.taskButton.setAttribute('aria-label', label);
  }

  openProject(scope, projectID) { return this.store.openProject(scope, projectID); }
  openProjectFacts(projectID) { return this.store.openProjectFacts(projectID); }
  openTask() { return this.store.openTask(); }
  onEvent(event) { this.store.onEvent(event); }
  destroy() { this.store.destroy(); this.page.destroy(); }
}
