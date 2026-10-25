import { html, render } from '../../ui.js';
import { HarnessesPage } from './harnesses-page.js';

// The settings shell's interface, the same as HarnessSettings; Preact renders only inside `section`.
export class PreactHarnessSettings {
  constructor(dialog) {
    this.tab = document.createElement('button');
    this.tab.type = 'button';
    this.tab.className = 'row-btn';
    this.tab.textContent = 'Harnesses';
    this.tab.id = 'settings-harnesses-tab';
    dialog.querySelector('nav').append(this.tab);
    this.section = document.createElement('section');
    this.section.className = 'settings-content';
    this.section.id = 'settings-harnesses';
    this.section.hidden = true;
    this.section.dataset.implementation = 'preact';
    dialog.append(this.section);
    this.wanted = '';
    this.openToken = 0;
    this.unsaved = new Set();
    this.machineID = '';
    this.onView = machineID => { this.machineID = machineID; };
    this.onReady = null;
    this.ready = () => this.onReady?.();
    this.onDirty = (key, unsaved) => {
      if (unsaved) this.unsaved.add(key);
      else this.unsaved.delete(key);
    };
    this.draw('');
  }

  draw(wanted) {
    render(html`<${HarnessesPage} openToken=${this.openToken} wanted=${wanted} onDirty=${this.onDirty} onView=${this.onView} onReady=${this.ready} isHidden=${() => this.section.hidden} />`, this.section);
  }

  viewSnapshot() { return { machineID: this.machineID }; }

  hasUnsavedChanges() { return this.unsaved.size > 0; }

  open() {
    this.openToken++;
    const wanted = this.wanted;
    this.wanted = '';
    this.draw(wanted);
  }

  destroy() { render(null, this.section); }
}
