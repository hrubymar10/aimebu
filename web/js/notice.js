import { html, render } from './ui.js';
import { IconButton } from './components/icon-button.js';

// A short-lived notice under a composer: "<file> is too big", a failed upload, a failed send. It is the one place those messages
// go, so none of them can stay on screen after it stopped being true: it clears itself after a while, on the next action
// (the caller calls clear()), and with its own dismiss button. The markup is a Preact component; the class below only hosts it (where it goes, its text and its timer),
// so the composer's code keeps the same three calls.
export const noticeSettings = { timeout: 8000 };

function NoticeView({ id, text, shown, onClose }) {
  return html`<div class="composer-notice error" id=${id} role="alert" hidden=${!shown}><span class="composer-notice-text">${text}</span><${IconButton} name="close" label="Dismiss message" className="composer-notice-close" onClick=${onClose} /></div>`;
}

export class Notice {
  // `after` is the element the notice is inserted after (the composer's form).
  constructor(after, id) {
    this.id = id; this.message = ''; this.shown = false;
    this.host = document.createElement('div'); this.host.className = 'composer-notice-host'; // the notice itself is the one box; this only holds it
    after.after(this.host);
    this.paint();
  }
  paint() { render(html`<${NoticeView} id=${this.id} text=${this.message} shown=${this.shown} onClose=${() => this.clear()} />`, this.host); }
  // The notice's element (the composer's tests and styles find it by its id).
  get node() { return this.host.firstElementChild; }
  // Shows `message`; with `append` it is added to what is already shown (several files rejected in one go). Restarts the timer.
  show(message, { append = false } = {}) {
    this.message = append && this.shown && this.message ? `${this.message} ${message}` : message;
    this.shown = true;
    this.paint();
    clearTimeout(this.timer);
    this.timer = setTimeout(() => this.clear(), noticeSettings.timeout);
  }
  clear() {
    clearTimeout(this.timer);
    if (!this.shown) return;
    this.shown = false; this.message = '';
    this.paint();
  }
}

// Rounded up: a file just over the limit must never read as exactly the limit.
export const megabytes = bytes => `${(Math.ceil(bytes / (1024 * 1024) * 10) / 10).toFixed(1)} MB`;
