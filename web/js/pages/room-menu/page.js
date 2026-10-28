import { html } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { RoomMenu as RoomMenuList, RoomMenuToggle } from './room-menu.js';
import { RoomMenuStore } from './store.js';

// The overflow button sits after the header's controls and the menu on the body (it is positioned against the window); both are Preact roots in
// hosts that take no box of their own, so the header lays out as before.
export class RoomMenu {
  constructor() {
    const controls = document.querySelector('.room-controls');
    this.store = new RoomMenuStore({ controls, settings: document.querySelector('#room-info') }); // beside the member count in the title; on a phone its place is this menu
    const host = () => { const node = document.createElement('div'); node.style.display = 'contents'; return node; };
    this.toggleHost = host(); this.menuHost = host();
    controls.after(this.toggleHost); document.body.append(this.menuHost);
    this.toggle = mountPreactPage(this.toggleHost, () => html`<${RoomMenuToggle} store=${this.store} />`);
    this.menu = mountPreactPage(this.menuHost, () => html`<${RoomMenuList} store=${this.store} />`);
  }

  destroy() { this.store.destroy(); this.toggle.destroy(); this.menu.destroy(); this.toggleHost.remove(); this.menuHost.remove(); }
}
