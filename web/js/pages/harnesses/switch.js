import { HarnessSettings } from '../../harness-settings.js';
import { defineExperiment, activeVariant, chooseVariant } from '../../ui-variant.js';
import { confirmDialog } from '../../dialogs.js';
import { PreactHarnessSettings } from './page.js';

export const EXPERIMENT = 'preact-harnesses';
defineExperiment(EXPERIMENT, 'Preact: Harnesses', ['Legacy', 'Preact'], { hidden: true });

// The legacy page has no API for its view; its machine select is the only state worth carrying over.
const viewSnapshot = page => page.viewSnapshot ? page.viewSnapshot() : { machineID: page.select?.value || '' };

const build = (variant, dialog) => variant === 'b' ? new PreactHarnessSettings(dialog) : new HarnessSettings(dialog);

export class HarnessSettingsSwitch {
  constructor(dialog) {
    this.dialog = dialog;
    this.variant = activeVariant(EXPERIMENT);
    this.page = this.watch(build(this.variant, dialog));
    document.addEventListener('ui-variant-change', async event => {
      const { experiment, variant } = event.detail || {};
      if (experiment !== EXPERIMENT || variant === this.variant) return;
      if (this.unsaved() && !await confirmDialog('Switching to the other Harnesses page throws away the changes that are not saved yet.', { title: 'Switch the Harnesses page', confirm: 'Switch', danger: true })) {
        chooseVariant(EXPERIMENT, this.variant);
        return;
      }
      if (variant !== this.variant) this.swap(variant);
    });
  }

  // The legacy page cannot report unsaved input, so anything typed since it opened counts.
  watch(page) { this.typed = false; page.section.addEventListener('input', () => { this.typed = true; }); return page; }
  unsaved() { return this.page.hasUnsavedChanges ? this.page.hasUnsavedChanges() : this.typed; }

  get tab() { return this.page.tab; }
  get section() { return this.page.section; }
  get onReady() { return this.page.onReady; }
  set onReady(callback) { this.page.onReady = callback; }
  get wanted() { return this.page.wanted; }
  set wanted(machineID) { this.page.wanted = machineID; }
  open() { this.typed = false; return this.page.open(); }

  swap(variant) {
    const old = this.page, next = this.watch(build(variant, this.dialog));
    next.tab.onclick = old.tab.onclick; next.tab.hidden = old.tab.hidden; next.tab.className = old.tab.className;
    if (old.tab.hasAttribute('aria-current')) next.tab.setAttribute('aria-current', old.tab.getAttribute('aria-current'));
    old.tab.replaceWith(next.tab);
    next.section.hidden = old.section.hidden;
    next.onReady = old.onReady;
    next.wanted = viewSnapshot(old).machineID || old.wanted;
    old.section.replaceWith(next.section);
    old.destroy?.();
    this.page = next; this.variant = variant;
    if (!next.section.hidden) next.open();
  }
}
