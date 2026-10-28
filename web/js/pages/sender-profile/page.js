import { html } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { SenderProfile } from './sender-profile.js';
import { SenderProfileStore } from './store.js';

export { senderFacts } from './model.js';

let open = null;

function dispose(opened) {
  if (open === opened) open = null;
  opened.store.destroy(); opened.page.destroy(); opened.dialog.remove();
}

// The read-only profile a message's sender name opens when that sender is not a live member any more: an agent from imported v1 history (a
// "historical" sender, known only by what it wrote) or one that left a v2 room ("departed"). `kind` is "historical" or "departed".
// onStartAgent(start) opens the spawn dialog for a historical sender; onOpenAgent(id) shows a v2 agent that was already started from it.
// Resolves to the dialog once the profile was read, or to nothing when the dialog was closed first.
export async function openSenderProfile({ name, authorID, kind, roomID, messages, identity, onStartAgent = null, onOpenAgent = null }) {
  if (open) dispose(open);
  const dialog = document.createElement('dialog'); dialog.className = 'dialog-card sender-profile'; dialog.id = 'sender-profile-dialog'; dialog.setAttribute('aria-labelledby', 'sender-profile-title');
  document.body.append(dialog);
  const store = new SenderProfileStore({ name, authorID, kind, roomID, messages: [...messages], identity, onStartAgent, onOpenAgent });
  const page = mountPreactPage(dialog, () => html`<${SenderProfile} store=${store} dialog=${dialog} />`);
  const opened = open = { dialog, page, store };
  dialog.addEventListener('close', () => dispose(opened));
  dialog.showModal();
  await store.load();
  return store.alive ? dialog : undefined;
}
