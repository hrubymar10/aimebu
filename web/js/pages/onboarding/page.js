import { html } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { confirmDialog } from '../../dialogs.js';
import { HarnessSettings } from '../harnesses/page.js';
import { OnboardingStore } from './store.js';
import { OnboardingPage } from './components.js';

export class Onboarding {
  // create(project) makes the project (and opens its home room); spawn() opens the Spawn dialog in it; harnessReady() answers whether a
  // harness with a model can start an agent right now (true when it cannot be told). canSetUp says whether this person may add harnesses
  // (an owner or an admin): only they get the Harnesses step, anyone else is told to ask one.
  constructor({ create, spawn, harnessReady, canSetUp, onSkip, onReady, confirm = confirmDialog, root = document.querySelector('#onboarding-view'), pollMs }) {
    this.root = root;
    this.store = new OnboardingStore({ create, spawn, harnessReady, canSetUp, onSkip, onReady, confirm, pollMs });
    root.replaceChildren();
    this.page = mountPreactPage(root, () => html`<${OnboardingPage} store=${this.store} root=${root} />`, { onError: error => console.error('onboarding:', error) });
    this.store.flush = () => { root.hidden = !this.store.open.value; this.page.update({}); };
    this.harnesses = this.store.harnesses = new HarnessSettings(null, { embedded: true, host: root.querySelector('#onboarding-harnesses-host') });
    this.harnesses.onReady = () => this.store.onReady();
    // Ticking a detected harness is enough to go on: Continue adds what is ticked first.
    this.harnesses.onTick = () => void this.store.checkHarness(true);
    this.onKey = event => { if (event.key === 'Escape') this.store.close(); };
    root.addEventListener('keydown', this.onKey);
    this.store.flush();
  }

  get open() { return !this.root.hidden; }
  get watch() { return this.store.watch; }
  show() { this.store.show(); }
  close() { this.store.close(); }
  showHarnesses(mode) { return this.store.showHarnesses(mode); }
  stopWatching() { return this.store.stopWatching(); }
  continueHarnesses() { return this.store.continueHarnesses(); }

  destroy() { this.root.removeEventListener('keydown', this.onKey); this.store.destroy(); this.harnesses.clear(); this.page.destroy(); }
}
