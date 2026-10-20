import { request } from './api.js';
import { EXPIRY_DEFAULT_SECONDS } from './expiry.js';

// The message-expiry window, shared by the chat (which marks expired messages) and the
// Settings row. It is read from the server for any signed-in user.
export const retention = {
  seconds: EXPIRY_DEFAULT_SECONDS,
  canEdit: false,
  listeners: new Set(),
  async load() {
    try {
      const body = await request('/api/retention');
      this.apply(body);
    } catch {}
    return this;
  },
  apply(body) {
    const seconds = Number(body?.messages_considered_expired_after_seconds);
    if (Number.isFinite(seconds)) this.seconds = seconds;
    this.canEdit = !!body?.can_edit;
    for (const listener of this.listeners) listener(this);
  },
  async save(seconds) {
    const body = await request('/api/retention', { method: 'PUT', body: { messages_considered_expired_after_seconds: seconds } });
    this.apply(body);
    return body;
  },
  onChange(listener) { this.listeners.add(listener); return () => this.listeners.delete(listener); },
};
