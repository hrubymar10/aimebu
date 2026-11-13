import { html } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { HandbookStore } from './handbook-store.js';
import { HandbookPage } from './handbook-page.js';

// The project handbook is content, so it has its own page beside the home
// room's Settings button rather than living among configuration controls.
export class HandbookView {
  constructor(projectName) {
    this.projectName = projectName;
    this.room = null;
    this.store = new HandbookStore();
    this.button = document.createElement('button');
    this.button.id = 'room-handbook'; this.button.type = 'button';
    this.button.className = 'btn-icon quiet';
    this.button.title = 'Project handbook'; this.button.setAttribute('aria-label', 'Project handbook');
    this.button.innerHTML = '<span class="icon-mask icon-mask-book-open" aria-hidden="true"></span>';
    this.button.hidden = true;
    document.querySelector('#room-info').after(this.button);
    this.dialog = document.createElement('dialog');
    this.dialog.id = 'handbook-dialog'; this.dialog.className = 'handbook-dialog';
    this.dialog.setAttribute('aria-labelledby', 'project-handbook-title');
    document.body.append(this.dialog);
    this.page = mountPreactPage(this.dialog, () => html`<${HandbookPage} store=${this.store} projectName=${this.projectName(this.room?.project_id) || 'Project'} onClose=${() => this.dialog.close()} />`);
    this.button.onclick = () => this.open();
    this.dialog.addEventListener('close', () => { if (!this.dialog.open) this.store.reset(); });
  }

  setRoom(room) {
    if (this.room?.id !== room?.id && this.dialog.open) this.dialog.close();
    this.room = room?.kind === 'home' ? room : null;
    this.button.hidden = !this.room;
  }

  open() {
    if (!this.room || this.dialog.open) return;
    this.dialog.showModal();
    void this.store.load(this.room.project_id);
  }

  destroy() { if (this.dialog.open) this.dialog.close(); this.store.destroy(); this.page.destroy(); this.dialog.remove(); this.button.remove(); }
}
