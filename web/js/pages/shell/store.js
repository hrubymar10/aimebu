import { batch, signal } from '../../ui.js';

export class ShellStore {
  constructor({ users }) {
    this.section = signal('general');
    this.owner = signal(false);
    this.admin = signal(false);
    this.usagesHidden = signal(false);
    this.users = users;
  }

  setRights({ owner, admin }) { batch(() => { this.owner.value = owner; this.admin.value = admin; this.usagesHidden.value = !owner; }); }
  setUser() { this.usagesHidden.value = false; }
  hideUsages() { this.usagesHidden.value = true; }
  select(section) { this.section.value = section; }
}
