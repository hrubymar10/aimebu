// A short-lived notice under a composer: "<file> is too big", a failed upload, a failed send. It is the one place those messages
// go, so none of them can stay on screen after it stopped being true: it clears itself after a while, on the next action
// (the caller calls clear()), and with its own dismiss button.
export const noticeSettings = { timeout: 8000 };

export class Notice {
  // `after` is the element the notice is inserted after (the composer's form).
  constructor(after, id) {
    this.node = document.createElement('div'); this.node.className = 'composer-notice error'; this.node.id = id; this.node.setAttribute('role', 'alert'); this.node.hidden = true;
    this.text = document.createElement('span'); this.text.className = 'composer-notice-text';
    this.close = document.createElement('button'); this.close.type = 'button'; this.close.className = 'icon-btn composer-notice-close'; this.close.textContent = '×';
    this.close.setAttribute('aria-label', 'Dismiss message'); this.close.title = 'Dismiss';
    this.close.addEventListener('click', () => this.clear());
    this.node.append(this.text, this.close);
    after.after(this.node);
  }
  // Shows `message`; with `append` it is added to what is already shown (several files rejected in one go). Restarts the timer.
  show(message, { append = false } = {}) {
    this.text.textContent = append && !this.node.hidden && this.text.textContent ? `${this.text.textContent} ${message}` : message;
    this.node.hidden = false;
    clearTimeout(this.timer);
    this.timer = setTimeout(() => this.clear(), noticeSettings.timeout);
  }
  clear() {
    clearTimeout(this.timer);
    if (this.node.hidden) return;
    this.node.hidden = true; this.text.textContent = '';
  }
}

// Rounded up: a file just over the limit must never read as exactly the limit.
export const megabytes = bytes => `${(Math.ceil(bytes / (1024 * 1024) * 10) / 10).toFixed(1)} MB`;
