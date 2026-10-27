import { html } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { HarnessesPage } from './harnesses-page.js';

// What the settings shell and the onboarding hold: a tab and a section, with Preact rendering only inside the section. Embedded (the
// onboarding step) there is no tab, the section shows at once inside `host`, and its ids carry a prefix so they never repeat Settings'.
export class HarnessSettings {
  constructor(dialog, { embedded = false, host = null } = {}) {
    const prefix = embedded ? 'onboarding-' : '';
    if (!embedded) {
      this.tab = document.createElement('button');
      this.tab.type = 'button';
      this.tab.className = 'row-btn';
      this.tab.textContent = 'Harnesses';
      this.tab.id = 'settings-harnesses-tab';
      dialog.querySelector('nav').append(this.tab);
    }
    this.section = document.createElement('section');
    this.section.className = 'settings-content';
    this.section.id = `${prefix}settings-harnesses`;
    this.section.hidden = !embedded;
    (embedded ? host : dialog).append(this.section);
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
    this.clearing = false;
    this.settle();
  }

  // How many detected harnesses are ticked in the open detect panel, and adding them (whether it went through).
  tickedCount() { return this.controls.current?.count() ?? 0; }
  async addTicked() { return this.controls.current ? this.controls.current.add() : false; }

  viewSnapshot() { return this.page.viewSnapshot(); }
  hasUnsavedChanges() { return this.page.hasUnsavedChanges(); }
  destroy() { this.page.destroy(); }
}
