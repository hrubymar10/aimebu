import { html } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { PickerDialog } from './picker-dialog.js';
import { SessionPickerStore } from './store.js';

// The picker of the Claude Code sessions a machine already has: the ones that worked in the project's folder (found by the machine's
// supervisor from Claude's own files), or one pasted by id. Picking one hands it to the Spawn dialog, which starts an agent that continues
// it. Only the title Claude gave the session is shown, never what was said in it. It draws into the empty #cc-sessions-dialog of index.html.
export class ClaudeSessionPicker {
  constructor(source) {
    this.dialog = document.querySelector('#cc-sessions-dialog');
    this.store = new SessionPickerStore(source, this.dialog);
    this.input = { current: null }; // the id box, a ref Preact fills
    this.page = mountPreactPage(this.dialog, () => html`<${PickerDialog} store=${this.store} inputRef=${this.input} />`);
  }

  open(options) { return this.store.open(options, () => { if (this.input.current) this.input.current.value = ''; }); }
  destroy() { this.store.destroy(); }
}
