import { html } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { TeamFlowStore } from './team-flow-store.js';
import { TeamSetupPage } from './team-setup-page.js';

export class TeamFlowHome {
  constructor({ canEdit, onApplied, onSetupChange = () => {}, projectForID = () => null }) {
    this.canEdit = canEdit;
    this.projectForID = projectForID;
    this.onSetupChange = onSetupChange;
    this.loadGeneration = 0;
    this.flow = new TeamFlowStore({ onApplied: async (projectID, result) => {
      this.onSetupChange(false);
      await onApplied?.(projectID, result);
    }, canPreview: canEdit });
    this.host = document.createElement('div');
    this.host.id = 'team-flow-home';
    this.host.hidden = true;
    document.querySelector('#messages').before(this.host);
    this.page = mountPreactPage(this.host, () => html`<${TeamSetupPage} flow=${this.flow} canEditProject=${this.canEdit} projectForID=${this.projectForID} onRetry=${() => this.load(this.flow.projectID)} />`);
  }

  setRoom(room) {
    if (room?.kind !== 'home') { this.reset(); return; }
    this.flow.roomID = room.id;
    this.host.hidden = false;
    this.onSetupChange(true);
    void this.load(room.project_id);
  }

  refreshIfProject(projectID) {
    if (this.flow.projectID === projectID) void this.load(projectID);
  }

  async load(projectID) {
    const generation = ++this.loadGeneration;
    await this.flow.load(projectID);
    if (this.loadGeneration === generation && this.flow.projectID === projectID) {
      const state = this.flow.state.peek();
      const loadFailed = !state && !!this.flow.error.peek();
      this.onSetupChange(!loadFailed && (!state || !state.key || !!state.review_pending));
    }
  }

  reset() { this.loadGeneration++; this.host.hidden = true; this.flow.reset(); this.onSetupChange(false); }

  destroy() { this.flow.destroy(); this.page.destroy(); this.host.remove(); }
}
