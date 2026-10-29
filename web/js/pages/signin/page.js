import { html, effect } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { SigninStore } from './store.js';
import { SigninPage, InviteDialogBody } from './components.js';

// The adapter writes the two elements the page draws into (the view's hidden, the dialog's open state); the store holds the rest.
export class Signin {
  constructor({ view, signedIn, afterReset, resync, onError, hash }) {
    this.view = view;
    this.store = new SigninStore({ signedIn, afterReset, resync, dropInviteLink: () => history.replaceState(null, '', location.pathname + location.search), hash });
    this.dialog = document.createElement('dialog'); this.dialog.className = 'invite-dialog dialog-card dialog-card-primary';
    document.body.append(this.dialog);
    this.dialog.addEventListener('close', () => this.store.closeDialog());
    this.pages = [mountPreactPage(view, () => html`<${SigninPage} store=${this.store} />`, { onError }), mountPreactPage(this.dialog, () => html`<${InviteDialogBody} store=${this.store} />`, { onError })];
    this.stops = [
      effect(() => { view.hidden = !this.store.shown.value; }),
      effect(() => { const open = this.store.dialogOpen.value; if (open && !this.dialog.open) this.dialog.showModal(); else if (!open && this.dialog.open) this.dialog.close(); }),
    ];
  }

  get pendingInvite() { return this.store.pending; }
  show() { return this.store.show(); }
  hide() { this.store.hide(); }
  openDialog() { this.store.openDialog(); }

  destroy() { for (const stop of this.stops) stop(); this.store.destroy(); for (const page of this.pages) page.destroy(); this.dialog.remove(); }
}
