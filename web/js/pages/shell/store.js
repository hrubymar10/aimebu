import { batch, signal } from '../../ui.js';
import { tabHidden } from './model.js';

export class ShellStore {
  constructor({ users }) {
    this.section = signal('general');
    this.owner = signal(false);
    this.admin = signal(false);
    this.usagesHidden = signal(false);
    // On a phone the dialog is two screens: the list of sections, then one section with a way back. Wider screens show both and ignore this.
    this.view = signal('section');
    this.users = users;
  }

  rights() { return { owner: this.owner.value, admin: this.admin.value, usagesHidden: this.usagesHidden.value, users: this.users.value }; }
  // A section whose tab is not shown is never the selection: it would open blank, with nothing in the nav to say where the person is.
  visible(section) { return tabHidden(section, this.rights()) ? 'general' : section; }
  // Rights that take the selected section's tab away put the selection back on General.
  settle() { if (tabHidden(this.section.value, this.rights())) this.section.value = 'general'; }
  setRights({ owner, admin }) { batch(() => { this.owner.value = owner; this.admin.value = admin; this.usagesHidden.value = !owner; this.settle(); }); }
  setUser() { this.usagesHidden.value = false; }
  hideUsages() { batch(() => { this.usagesHidden.value = true; this.settle(); }); }
  select(section) { batch(() => { this.section.value = this.visible(section); this.view.value = 'section'; }); }
  showList() { this.view.value = 'list'; }
}
