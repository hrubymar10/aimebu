import { savePreference } from './preferences.js';
import { currentUser } from './api.js';

// Browser-side preferences for the chat view. All default to the v1 defaults:
// system events visible, debug inspector off; the message layout defaults to B
// (every message on the left), with A only when this person chose it.
export const chatPreferences = {
  key(name) { return `aimebu2.ui.${name}.${currentUser}`; },
  get showSystemEvents() { try { return localStorage.getItem(this.key('show-system-events')) !== '0'; } catch { return true; } },
  set showSystemEvents(value) { try { localStorage.setItem(this.key('show-system-events'), value ? '1' : '0'); } catch {} savePreference('system_events', value ? '1' : '0'); },
  get debugInspectorChosen() { try { return localStorage.getItem(this.key('debug-inspector')) !== null; } catch { return true; } },
  get debugInspector() { try { return localStorage.getItem(this.key('debug-inspector')) === '1'; } catch { return false; } },
  set debugInspector(value) { try { localStorage.setItem(this.key('debug-inspector'), value ? '1' : '0'); } catch {} savePreference('debug_inspector', value ? '1' : '0'); },
  get messageLayout() { try { return localStorage.getItem(this.key('message-layout')) === 'a' ? 'a' : 'b'; } catch { return 'b'; } },
  set messageLayout(value) { value = value === 'b' ? 'b' : 'a'; try { localStorage.setItem(this.key('message-layout'), value); } catch {} savePreference('message_layout', value); },
};

export function applyChatPreferences() {
  document.querySelector('#system-events-toggle')?.toggleAttribute('hidden', !chatPreferences.showSystemEvents);
  if (!chatPreferences.showSystemEvents) document.querySelector('#system-events-popover')?.setAttribute('hidden', '');
  document.documentElement.dataset.debugInspector = chatPreferences.debugInspector ? 'on' : 'off';
  if (chatPreferences.messageLayout === 'b') document.documentElement.dataset.msgLayout = 'b';
  else delete document.documentElement.dataset.msgLayout;
}
