import { html } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { TeamFlowStore } from './team-flow-store.js';
import { TeamFlowHomePage } from './team-flow-page.js';

export class TeamFlowHome {
  constructor({ canEdit, onApplied }) {
    this.canEdit = canEdit;
    this.flow = new TeamFlowStore({ onApplied });
    this.host = document.createElement('div');
    this.host.id = 'team-flow-home';
    this.host.hidden = true;
    document.querySelector('#messages').before(this.host);
    this.page = mountPreactPage(this.host, () => html`<${TeamFlowHomePage} flow=${this.flow} canEdit=${this.canEdit(this.flow.projectID)} />`);
  }

  setRoom(room) {
    if (room?.kind !== 'home') { this.reset(); return; }
    this.host.hidden = false;
    void this.flow.load(room.project_id);
  }

  refreshIfProject(projectID) {
    if (this.flow.projectID === projectID) void this.flow.load(projectID);
  }

  reset() { this.host.hidden = true; this.flow.reset(); }

  destroy() { this.flow.destroy(); this.page.destroy(); this.host.remove(); }
}
