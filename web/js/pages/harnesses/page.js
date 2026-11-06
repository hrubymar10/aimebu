import { html } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { HarnessesPage } from './harnesses-page.js';

// Embedded (the onboarding step) the section shows at once inside `host`, and its ids carry a prefix so they never repeat Settings'.
export class HarnessSettings {
  constructor(dialog, { embedded = false, host = null } = {}) {
    const prefix = embedded ? 'onboarding-' : '';
    this.section = document.createElement('section');
    this.section.className = embedded ? 'settings-section active' : 'settings-section';
    this.section.id = `${prefix}settings-harnesses`;
    this.section.hidden = !embedded;
    (embedded ? host : dialog.querySelector('.settings-body') || dialog).append(this.section);
    this.embedded = embedded;
    this.wanted = '';
    this.onReady = null;
    this.onTick = null;
    this.controls = { current: null };
    this.waiting = [];
    this.clearing = false;
    const Page = ({ onView, ...props }) => html`<${HarnessesPage} ...${props}
      controls=${this.controls}
      onView=${machineID => onView({ wanted: machineID })}
      onReady=${() => this.onReady?.()}
      onTick=${count => { if (!this.clearing) this.onTick?.(count); }}
      onOpened=${() => this.settle()}
      isHidden=${() => this.section.hidden} />`;
    this.page = mountPreactPage(this.section, Page, { idPrefix: prefix, embedded });
  }

  settle() {
    for (const resolve of this.waiting.splice(0)) resolve();
  }

  // Resolves when the machines and the first machine's harnesses have been read.
  open() {
    return new Promise(resolve => {
      this.waiting.push(resolve);
      this.page.update({ wanted: this.wanted });
      this.wanted = '';
      this.page.open();
    });
  }

  // Forgets what was shown (the embedded copy, when its step is left), so no element of it is left in the page.
  clear() {
    this.clearing = true;
    this.page.clear();
    if (this.embedded) this.page.update({ result: false, showRoles: false });
    this.clearing = false;
    this.settle();
  }

  // The onboarding's result step: the harnesses and their models as they are now, and the role defaults (which have nothing to tick before a model exists).
  showResult(on) { this.page.update({ result: on, showRoles: on }); }

  // How many detected harnesses are ticked in the open detect panel, and adding them (whether it went through).
  tickedCount() { return this.controls.current?.count() ?? 0; }
  async addTicked() { return this.controls.current ? this.controls.current.add() : false; }

  viewSnapshot() { return this.page.viewSnapshot(); }
  hasUnsavedChanges() { return this.page.hasUnsavedChanges(); }
  destroy() { this.page.destroy(); }
}
