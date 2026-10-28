import { signal, computed } from '../../ui.js';

// On phones the room header cannot hold six controls next to the title and the Agents button, so below the breakpoint the controls collapse into
// one overflow menu. The menu holds no logic of its own: each item clicks the real header button, so hidden and disabled state stay in one place.
export const PHONE = '(max-width: 700px), (max-width: 900px) and (pointer: coarse)';

const sameEntries = (a, b) => a.length === b.length && a.every((entry, index) => entry.button === b[index].button && entry.label === b[index].label && entry.disabled === b[index].disabled);

export class RoomMenuStore {
  constructor({ controls, settings }) {
    this.controls = controls; this.settings = settings;
    this.phone = matchMedia(PHONE);
    this.entries = signal([]); this.items = signal([]); this.open = signal(false); this.position = signal({ top: 0, right: 0 });
    this.toggle = null;
    this.onPhone = signal(this.phone.matches);
    // Hidden unless the layout moves the controls into the menu and at least one of them can be used.
    this.usable = computed(() => this.onPhone.value && this.entries.value.some(entry => !entry.disabled));
    this.listen();
    this.sync();
  }

  listen() {
    const outside = event => { if (!event.target.closest?.('#room-menu')) this.close(); };
    const escape = event => { if (event.key === 'Escape' && this.open.peek()) { this.close(); this.toggle?.focus(); } };
    const resize = () => { if (this.open.peek()) this.place(); };
    const change = () => { this.onPhone.value = this.phone.matches; this.sync(); };
    document.addEventListener('click', outside); document.addEventListener('keydown', escape);
    window.addEventListener('resize', resize); this.phone.addEventListener('change', change);
    // The overflow button exists only where controls overflow into it (phones) and only while it has something to open.
    this.observer = new MutationObserver(() => this.sync());
    this.observer.observe(this.controls, { attributes: true, subtree: true, attributeFilter: ['hidden', 'disabled'] });
    if (this.settings) this.observer.observe(this.settings, { attributes: true, attributeFilter: ['hidden', 'disabled'] });
    this.unlisten = () => {
      document.removeEventListener('click', outside); document.removeEventListener('keydown', escape);
      window.removeEventListener('resize', resize); this.phone.removeEventListener('change', change); this.observer.disconnect();
    };
  }

  // One entry per header button that is currently shown, labelled with its accessible name.
  read() {
    return [...(this.settings ? [this.settings] : []), ...this.controls.querySelectorAll('button')].filter(button => !button.hidden).map(button => ({
      button, disabled: button.disabled, label: button.getAttribute('aria-label') || button.title || button.textContent.trim(),
    }));
  }

  sync() {
    const next = this.read();
    if (!sameEntries(next, this.entries.peek())) this.entries.value = next;
    if (this.open.peek() && !this.usable.peek()) this.close();
  }

  show() {
    this.items.value = this.read(); this.place(); this.open.value = true;
  }

  close() { this.open.value = false; }
  press() { if (this.open.peek()) this.close(); else this.show(); }
  choose(entry) { this.close(); entry.button.click(); }

  place() {
    const box = this.toggle.getBoundingClientRect();
    this.position.value = { top: Math.round(box.bottom + 4), right: Math.max(8, Math.round(innerWidth - box.right)) };
  }

  destroy() { this.unlisten(); this.open.value = false; }
}
