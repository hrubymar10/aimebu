import { savePreference } from './preferences.js';
import { focusPopover, returnFocus } from './popover-focus.js';
import { request, currentUser } from './api.js';
import { relativeTime } from './time.js';
import { settingsRow, settingsGroup } from './settings-row.js';

function node(tag, className = '', text = '') { const item = document.createElement(tag); if (className) item.className = className; if (text) item.textContent = text; return item; }

const labels = {
  agent_created: 'Agent created', agent_stopped: 'Agent stopped', agent_revoked: 'Agent revoked', agent_key_rotated: 'Agent key rotated',
  agent_spawn_requested: 'Agent spawn requested', agent_spawn_confirmed: 'Agent spawned', agent_spawn_failed: 'Agent spawn failed',
  agent_control_requested: 'Agent control requested', agent_control_applied: 'Agent control applied',
  agent_switch_requested: 'Agent switch requested', agent_switch_starting: 'Agent switch starting', agent_switch_completed: 'Agent switched', agent_switch_failed: 'Agent switch failed',
  agent_turn_budget_exceeded: 'Agent paused: turn budget exceeded', loop_guard: 'Agents stopped answering each other',
  room_created: 'Room created', room_renamed: 'Room renamed', room_closing: 'Room closing', room_closed: 'Room closed', room_reopened: 'Room reopened', room_removed: 'Room removed', room_close_blocked: 'Room close blocked',
  room_member_joined: 'Joined the room', room_member_left: 'Left the room',
  room_worktree_requested: 'Worktree requested', room_worktree_updated: 'Worktree updated', room_worktree_cancelled: 'Worktree cancelled',
  project_created: 'Project created', project_updated: 'Project changed', project_member_joined: 'Joined the project', project_member_changed: 'Project member changed', project_settings_updated: 'Project settings updated',
  provider_limit_detected: 'Provider usage limit reached', provider_limit_cleared: 'Provider usage limit cleared', native_session_reset: 'Started a fresh session',
  turn_lost: 'Turn lost', turn_never_started: 'Turn never started', turn_process_exit: 'Turn process exited', special_turn_abandoned: 'Special turn abandoned',
  diagnostic_requested: 'Diagnostic requested', answer_held: 'Answer held',
  recovery_decision: 'Recovery decision', recovery_override: 'Recovery override', recovery_overlap_override: 'Recovery overlap override', recovery_quiescent: 'Recovery quiescent',
  recovery_continuation_cancelled: 'Recovery continuation cancelled', recovery_continuation_requeued: 'Recovery continuation requeued',
  usage_limit_threshold: 'Usage limit threshold reached', usage_limit_reset: 'Usage limit reset', usage_limit_decided: 'Usage limit decision',
  scope_control_requested: 'Scope control requested', notes_dropped: 'Queued notes dropped',
  machine_created: 'Machine added', machine_approved: 'Machine approved', machine_revoked: 'Machine revoked', machine_key_rotated: 'Machine key rotated', machine_pairing_created: 'Machine pairing created',
  user_created: 'User created', user_deactivated: 'User deactivated', user_admin_changed: 'Admin changed', user_invite_created: 'Invite created', user_invite_redeemed: 'Invite redeemed',
  credential_alert: 'Credential alert',
  quota_switch: 'Automatic account switch', quota_switch_failed: 'Automatic account switch failed',
  quota_reset_seen: 'Weekly window reset seen', quota_switcher_tripped: 'Automatic switching tripped',
  limit_guard_soft: 'Usage limit near: new turns held', limit_guard_hard: 'Usage limit reached', limit_guard_clear: 'Usage back under the limit: new turns resume',
};

// Payload fields worth showing next to the label, in order. Everything else
// stays available in the message debug inspector and the raw event.
const detailKeys = ['reason', 'held_reason', 'exit_code', 'desired', 'reset_at', 'error', 'code', 'role', 'name'];

export function describeEvent(event, names = {}) {
  let payload = {}; try { payload = JSON.parse(event.payload_json || '{}') || {}; } catch {}
  const parts = [labels[event.kind] || event.kind.replaceAll('_', ' ')];
  const agent = event.agent_id && (names.agentName?.(event.agent_id) || event.agent_id);
  const room = event.room_id && names.roomTitle?.(event.room_id);
  const where = [agent, room].filter(Boolean).join(' · ');
  if (event.kind === 'credential_alert') {
    // The payload is {kind, detail}: `kind` names the alert (login imported, refresh
    // needs a human, expiring, ...) and `detail` is already redacted by the server.
    // A dedicated branch, not detailKeys, so a `kind` key never leaks into the other
    // events. The caller renders it with textContent, so it is shown as plain text.
    const what = String(payload.kind ?? '').replaceAll('_', ' ');
    const detail = String(payload.detail ?? '');
    return { text: parts[0], where, details: [what, detail].filter(Boolean).join(': ') };
  }
  if (event.kind.startsWith('quota_')) {
    // {tool, from, to, profile, reason}: the tool, the move or the watched profile, and
    // the server's fixed reason sentence. A branch, not detailKeys, so these keys do not
    // leak into other events. Rendered with textContent by the caller.
    const move = payload.from && payload.to ? `${payload.from} → ${payload.to}` : (payload.profile ?? '');
    return { text: parts[0], where, details: [payload.tool, move, payload.reason].filter(Boolean).map(String).join(' · ') };
  }
  if (event.kind.startsWith('limit_guard_')) {
    // {tool, profile, percent, window, soft, hard, next}: which profile is how full, and what happens next.
    const full = payload.percent ? `${payload.percent}% ${payload.window ?? ''}`.trim() : '';
    const next = { held: 'new turns stay held', switching: 'the quota switcher moves off it' }[payload.next] ?? '';
    return { text: parts[0], where, details: [payload.tool, payload.profile, full, next].filter(Boolean).map(String).join(' · ') };
  }
  const details = detailKeys.filter(key => payload[key] !== undefined && payload[key] !== '').map(key => `${key.replace('_', ' ')}: ${payload[key]}`);
  return { text: parts[0], where, details: details.join(', ') };
}

// Browser-side preferences for the chat view. Both default to the v1 defaults:
// system events visible, debug inspector off.
export const chatPreferences = {
  key(name) { return `aimebu2.ui.${name}.${currentUser}`; },
  get showSystemEvents() { try { return localStorage.getItem(this.key('show-system-events')) !== '0'; } catch { return true; } },
  set showSystemEvents(value) { try { localStorage.setItem(this.key('show-system-events'), value ? '1' : '0'); } catch {} savePreference('system_events', value ? '1' : '0'); },
  get debugInspectorChosen() { try { return localStorage.getItem(this.key('debug-inspector')) !== null; } catch { return true; } },
  get debugInspector() { try { return localStorage.getItem(this.key('debug-inspector')) === '1'; } catch { return false; } },
  set debugInspector(value) { try { localStorage.setItem(this.key('debug-inspector'), value ? '1' : '0'); } catch {} savePreference('debug_inspector', value ? '1' : '0'); },
};

export function applyChatPreferences() {
  document.querySelector('#system-events-toggle')?.toggleAttribute('hidden', !chatPreferences.showSystemEvents);
  if (!chatPreferences.showSystemEvents) document.querySelector('#system-events-popover')?.setAttribute('hidden', '');
  document.documentElement.dataset.debugInspector = chatPreferences.debugInspector ? 'on' : 'off';
}

const keep = 200;

// A feed of operational events (joins, spawns, recoveries, quota and machine
// changes). History comes from /api/system-events; later events arrive on the
// live stream, filtered by the kind list the server returned.
export class SystemEvents {
  constructor(names) {
    this.names = names; this.events = []; this.kinds = new Set(); this.unread = 0; this.ticket = 0;
    this.toggle = node('button', 'icon-btn'); this.toggle.id = 'system-events-toggle'; this.toggle.type = 'button';
    this.toggle.title = 'System events'; this.toggle.setAttribute('aria-label', 'System events'); this.toggle.setAttribute('aria-haspopup', 'dialog'); this.toggle.setAttribute('aria-expanded', 'false');
    this.toggle.innerHTML = '<svg width="14" height="18" viewBox="0 0 400 512" aria-hidden="true"><path fill="none" stroke="currentColor" stroke-width="25" stroke-linecap="round" stroke-linejoin="round" d="M222,240l92,248 -180-126 108-53m-84,0 108,53-180,126 92-248M80,318a172,172 0 1,1 240,0M130,266a100,100 0 1,1 140,0M199,144a50,50 0 1,0 2,0"/></svg>';
    this.badge = node('span', 'badge'); this.badge.id = 'system-events-count'; this.badge.hidden = true; this.toggle.append(this.badge);
    this.popover = node('div', 'system-events-popover popover-panel popover-panel-sheet'); this.popover.id = 'system-events-popover'; this.popover.setAttribute('role', 'dialog'); this.popover.setAttribute('aria-label', 'System events'); this.popover.hidden = true;
    const head = node('header'); head.append(node('strong', '', 'System events'));
    this.list = node('div', 'system-events-list'); this.popover.append(head, this.list);
    const health = document.querySelector('#health-toggle');
    health.before(this.toggle);
    document.querySelector('.workspace-bar').append(this.popover);
    this.toggle.onclick = event => { event.stopPropagation(); if (this.popover.hidden) this.open(); else this.close(); };
    // A click outside closes the panel. The More menu on a phone forwards its System events item to the bell, so a click in that menu is not outside:
    // it would close the panel the same click just opened.
    document.addEventListener('click', event => { if (!this.popover.hidden && !this.popover.contains(event.target) && !this.toggle.contains(event.target) && !event.target.closest?.('#more-menu')) this.close(); });
    document.addEventListener('keydown', event => { if (event.key === 'Escape' && !this.popover.hidden) { this.close(); this.toggle.focus(); } });
    this.installSettings();
    applyChatPreferences();
  }

  installSettings() {
    const general = document.querySelector('#settings-general');
    if (!general || general.querySelector('#chat-preferences')) return;
    const group = node('div', 'settings-section'); group.id = 'chat-preferences';
    group.append(node('h3', '', 'Chat'));
    const rows = [];
    const make = (id, text, hint, get, set) => {
      const input = node('input'); input.type = 'checkbox'; input.id = id;
      input.onchange = () => { set(input.checked); applyChatPreferences(); };
      rows.push(settingsRow({ label: text, desc: hint, control: input, id, className: 'settings-row-toggle' }));
      return () => { input.checked = get(); };
    };
    const syncEvents = make('pref-system-events', 'Show system events', 'Adds a bell to the top bar with a feed of joins, spawns, recoveries, quota limits, and machine changes.', () => chatPreferences.showSystemEvents, value => { chatPreferences.showSystemEvents = value; });
    const syncDebug = make('pref-debug-inspector', 'Message debug inspector', 'Adds a debug button to each message that shows its raw record, who it addressed, and what happened to each agent it woke.', () => chatPreferences.debugInspector, value => { chatPreferences.debugInspector = value; });
    this.syncSettings = () => { syncEvents(); syncDebug(); };
    this.syncSettings();
    group.append(settingsGroup(rows, 'Chat'));
    general.append(group);
    document.querySelector('#settings-general-tab')?.addEventListener('click', () => this.syncSettings());
  }

  reset() { this.ticket++; this.events = []; this.unread = 0; this.close(); this.renderBadge(); this.renderList(); }
  setUser() { applyChatPreferences(); this.syncSettings?.(); }

  open() {
    this.popover.hidden = false; this.toggle.setAttribute('aria-expanded', 'true');
    this.unread = 0; this.renderBadge(); this.renderList(); focusPopover(this.popover);
  }
  close() { returnFocus(this.popover, this.toggle); this.popover.hidden = true; this.toggle.setAttribute('aria-expanded', 'false'); }

  async load() {
    const ticket = ++this.ticket;
    try {
      const data = await request('/api/system-events?limit=100');
      if (ticket !== this.ticket) return;
      this.kinds = new Set(data.kinds || []);
      const known = new Set(this.events.map(event => event.id));
      this.events = [...this.events, ...(data.events || []).filter(event => !known.has(event.id))].sort((a, b) => b.seq - a.seq).slice(0, keep);
      this.renderList();
    } catch { /* The feed is optional; the live stream still fills it. */ }
  }

  onEvent(event) {
    if (!this.kinds.has(event.kind) || this.events.some(item => item.id === event.id)) return;
    this.events.unshift(event); this.events.length = Math.min(this.events.length, keep);
    if (this.popover.hidden) { this.unread++; this.renderBadge(); } else this.renderList();
  }

  renderBadge() { this.badge.textContent = String(this.unread); this.badge.hidden = !this.unread; }

  renderList() {
    if (!this.events.length) { this.list.replaceChildren(node('p', 'system-events-empty', 'No system events yet.')); return; }
    this.list.replaceChildren(...this.events.map(event => {
      const view = describeEvent(event, this.names);
      const row = node('div', 'system-event-row'); row.dataset.kind = event.kind;
      const main = node('div', 'system-event-main');
      main.append(node('span', 'system-event-text', view.text));
      if (view.where) main.append(node('span', 'system-event-where', view.where));
      if (view.details) main.append(node('span', 'system-event-details', view.details));
      const time = node('time', 'system-event-time', relativeTime(event.created_at)); time.dateTime = event.created_at; time.title = new Date(event.created_at).toLocaleString();
      row.append(main, time);
      return row;
    }));
  }
}
