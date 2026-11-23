import { applyChatPreferences } from './chat-preferences.js';
import { focusPopover, returnFocus } from './popover-focus.js';
import { request } from './api.js';
import { relativeTime } from './time.js';
import { captureError } from './telemetry.js';

function node(tag, className = '', text = '') { const item = document.createElement(tag); if (className) item.className = className; if (text) item.textContent = text; return item; }

const labels = {
  agent_created: 'Agent created', agent_stopped: 'Agent stopped', agent_revoked: 'Agent revoked', agent_key_rotated: 'Agent key rotated', agent_role_changed: 'Agent role changed',
  agent_spawn_requested: 'Agent spawn requested', agent_spawn_confirmed: 'Agent spawned', agent_spawn_failed: 'Agent spawn failed',
  agent_control_requested: 'Agent control requested', agent_control_applied: 'Agent control applied',
  agent_context_cleared: 'Context cleared', agent_context_compacted: 'Context compacted',
  agent_switch_requested: 'Agent switch requested', agent_switch_starting: 'Agent switch starting', agent_switch_completed: 'Agent switched', agent_switch_failed: 'Agent switch failed',
  agent_turn_budget_exceeded: 'Agent paused: turn budget exceeded', loop_guard: 'Agents stopped answering each other',
  room_created: 'Room created', room_renamed: 'Room renamed', room_closing: 'Room closing', room_closed: 'Room closed', room_reopened: 'Room reopened', room_removed: 'Room removed', room_close_blocked: 'Room close blocked',
  room_member_joined: 'Joined the room', room_member_left: 'Left the room',
  room_worktree_requested: 'Worktree requested', room_worktree_updated: 'Worktree updated', room_worktree_cancelled: 'Worktree cancelled',
  project_created: 'Project created', project_updated: 'Project changed', project_archived: 'Project archived', project_reopened: 'Project reopened', project_removed: 'Project removed', project_member_joined: 'Joined the project', project_member_changed: 'Project member changed', project_settings_updated: 'Project settings updated',
  provider_limit_detected: 'Provider usage limit reached', provider_limit_cleared: 'Provider usage limit cleared', native_session_reset: 'Started a fresh session',
  turn_lost: 'Turn lost', turn_never_started: 'Turn never started', turn_process_exit: 'Turn process exited', special_turn_abandoned: 'Special turn abandoned',
  diagnostic_requested: 'Diagnostic requested', answer_held: 'Answer held',
  recovery_decision: 'Recovery decision', recovery_override: 'Recovery override', recovery_overlap_override: 'Recovery overlap override', recovery_quiescent: 'Recovery quiescent',
  recovery_continuation_cancelled: 'Recovery continuation cancelled', recovery_continuation_requeued: 'Recovery continuation requeued',
  turn_limit_continuation: 'Turn limit reached: continuing on its own',
  turn_interrupted_continuation: 'Turn interrupted: continuing on its own',
  turn_interrupt_requested: 'Turn interrupt requested',
  usage_limit_threshold: 'Usage limit threshold reached', usage_limit_reset: 'Usage limit reset', usage_limit_decided: 'Usage limit decision',
  scope_control_requested: 'Scope control requested', notes_dropped: 'Queued notes dropped',
  machine_created: 'Machine added', machine_approved: 'Machine approved', machine_revoked: 'Machine revoked', machine_key_rotated: 'Machine key rotated', machine_pairing_created: 'Machine pairing created',
  user_created: 'User created', user_deactivated: 'User deactivated', user_admin_changed: 'Admin changed', user_invite_created: 'Invite created', user_invite_redeemed: 'Invite redeemed',
  credential_alert: 'Credential alert', update_status: 'Update status',
  quota_switch: 'Automatic account switch', quota_switch_dry_run: 'Automatic account switch due, not made: dry run is on', quota_hold: 'Automatic account switch held', quota_switch_failed: 'Automatic account switch failed',
  switch_overwritten: 'A running session wrote the old login back after a switch: the new login was installed again', quota_reset_seen: 'Weekly window reset seen', quota_switcher_tripped: 'Automatic switching tripped',
  limit_guard_soft: 'Usage limit near: new turns held', limit_guard_hard: 'Usage limit reached', limit_guard_clear: 'Usage back under the limit: new turns resume',
};

// Payload fields worth showing next to the label, in order. Everything else
// stays available in the message debug inspector and the raw event.
const detailKeys = ['reason', 'held_reason', 'exit_code', 'desired', 'reset_at', 'error', 'code', 'role', 'name'];

export function describeEvent(event, names = {}) {
  let payload = {}; try { payload = JSON.parse(event.payload_json || '{}') || {}; } catch {}
  const parts = [labels[event.kind] || event.kind.replaceAll('_', ' ')];
  // Who an id is: what the server named it for this event (an agent as slug@project, a user by login, whichever room it is in), else what the open room knows.
  const agent = event.agent_id && (event.names?.[event.agent_id] || names.agentName?.(event.agent_id) || event.agent_id);
  const room = event.room_id && names.roomTitle?.(event.room_id);
  const where = [agent, room].filter(Boolean).join(' · ');
  const person = id => (id && event.names?.[id]) || '';
  if (event.kind.startsWith('user_')) {
    // The people of an account event, named (the payload holds ids): "Invite redeemed by wren", "Admin changed: wren is now an admin, by owner".
    const subject = person(payload.user_id), by = person(payload.by);
    const text = {
      user_invite_redeemed: subject && `${parts[0]} by ${subject}`,
      user_invite_created: by && `${parts[0]} by ${by}`,
      user_created: subject && `${parts[0]}: ${subject}`,
      user_deactivated: subject && `${parts[0]}: ${subject}`,
      user_admin_changed: subject && `${parts[0]}: ${subject} is ${payload.admin === 'true' ? 'now an admin' : 'no longer an admin'}${by ? `, by ${by}` : ''}`,
    }[event.kind] || parts[0];
    const kind = event.kind === 'user_invite_created' && payload.kind ? `${String(payload.kind).replaceAll('_', ' ')} link` : '';
    return { text, where, details: kind };
  }
  if (event.kind === 'agent_control_requested') {
    // One row for a start, stop or pause: "owen@test: started by wren". (The control being applied is the same fact and is not shown; the server decides what is notable.)
    const what = { active: 'started', stopped: 'stopped', not_accepting: 'paused' }[payload.desired] || 'changed';
    const by = payload.actor_kind === 'user' || (!payload.actor_kind && payload.actor) ? person(payload.actor_id || payload.actor) || 'someone' : payload.reason === 'provider_limit_retry' ? 'the limit guard' : 'the system';
    return { text: `${agent || 'An agent'}: ${what} by ${by}`, where: room || '', details: payload.reason ? `reason: ${String(payload.reason).replaceAll('_', ' ')}` : '' };
  }
  if (event.kind === 'agent_context_cleared') {
    const by = person(payload.actor_id);
    return { text: `Context cleared · ${agent || 'An agent'}`, where: room || '', details: by ? `requested by ${by}` : '' };
  }
  if (event.kind === 'agent_context_compacted') {
    return { text: `Context compacted · ${agent || 'An agent'}`, where: room || '', details: payload.pre_tokens && payload.post_tokens ? `${payload.pre_tokens} → ${payload.post_tokens} tokens` : '' };
  }
  if (event.kind === 'credential_alert') {
    // The payload is {kind, detail}: `kind` names the alert (login imported, refresh
    // needs a human, expiring, ...) and `detail` is already redacted by the server.
    // A dedicated branch, not detailKeys, so a `kind` key never leaks into the other
    // events. The caller renders it with textContent, so it is shown as plain text.
    // The row names the alert and, beside it, whose login it is (the tool and account the server's sentence starts with: "claude/main: ..."); the rest of the sentence is behind the row.
    const what = String(payload.kind ?? '').replaceAll('_', ' ');
    const detail = String(payload.detail ?? '');
    const split = detail.indexOf(': '), subject = split > 0 && split <= 40 ? detail.slice(0, split) : '';
    return { text: what ? `${parts[0]}: ${what}` : parts[0], where: where || subject, details: subject ? detail.slice(split + 2) : detail };
  }
  if (event.kind === 'update_status') {
    // {state, version}: what the server's own update check found. An empty state means a later check found nothing and the notice cleared. Rendered with textContent by the caller.
    const text = {
      available: 'Update available', installed: 'Update installed: restart the server to pick it up',
      unbuilt: 'Checkout is newer than the built binary', '': 'Update notice cleared',
    }[payload.state ?? ''] ?? parts[0];
    return { text, where, details: payload.version ? String(payload.version) : '' };
  }
  if (event.kind.startsWith('quota_')) {
    // {tool, from, to, profile, reason}: the tool, the move or the watched profile, and
    // the server's fixed reason sentence. A branch, not detailKeys, so these keys do not
    // leak into other events. Rendered with textContent by the caller.
    const move = payload.from && payload.to ? `${payload.from} → ${payload.to}` : (payload.profile ?? '');
    return { text: parts[0], where, details: [payload.tool, move, payload.reason].filter(Boolean).map(String).join(' · ') };
  }
  if (event.kind === 'switch_overwritten') return { text: parts[0], where, details: payload.tool ? `tool: ${payload.tool}` : '' };
  if (event.kind.startsWith('limit_guard_')) {
    // {tool, profile, percent, window, soft, hard, next}: which profile is how full, and what happens next.
    const full = payload.percent ? `${payload.percent}% ${payload.window ?? ''}`.trim() : '';
    const next = { held: 'new turns stay held', switching: 'the quota switcher moves off it', dry_run: 'the quota switcher would move off it, but it is in dry run' }[payload.next] ?? '';
    return { text: parts[0], where, details: [payload.tool, payload.profile, full, next].filter(Boolean).map(String).join(' · ') };
  }
  const details = detailKeys.filter(key => payload[key] !== undefined && payload[key] !== '').map(key => `${key.replace('_', ' ')}: ${payload[key]}`);
  return { text: parts[0], where, details: details.join(', ') };
}

const keep = 200;

// A feed of operational events (joins, spawns, recoveries, quota and machine
// changes). History comes from /api/system-events; later events arrive on the
// live stream, filtered by the kind list the server returned.
export class SystemEvents {
  constructor(names) {
    this.names = names; this.events = []; this.unread = 0; this.ticket = 0;
    this.toggle = node('button', 'btn-icon'); this.toggle.id = 'system-events-toggle'; this.toggle.type = 'button';
    this.toggle.title = 'System events'; this.toggle.setAttribute('aria-label', 'System events'); this.toggle.setAttribute('aria-haspopup', 'dialog'); this.toggle.setAttribute('aria-expanded', 'false');
    this.toggle.innerHTML = '<svg width="14" height="18" viewBox="0 0 400 512" aria-hidden="true"><path fill="none" stroke="currentColor" stroke-width="25" stroke-linecap="round" stroke-linejoin="round" d="M222,240l92,248 -180-126 108-53m-84,0 108,53-180,126 92-248M80,318a172,172 0 1,1 240,0M130,266a100,100 0 1,1 140,0M199,144a50,50 0 1,0 2,0"/></svg>';
    this.badge = node('span', 'badge'); this.badge.id = 'system-events-count'; this.badge.hidden = true; this.toggle.append(this.badge);
    this.popover = node('div', 'system-events-popover popover-panel popover-panel-sheet'); this.popover.id = 'system-events-popover'; this.popover.setAttribute('role', 'dialog'); this.popover.setAttribute('aria-label', 'System events'); this.popover.hidden = true;
    const head = node('header'); head.append(node('strong', '', 'System events'));
    // A phone shows the panel as a sheet over the page, with no click-outside to reach: it has its own close control.
    this.closeButton = node('button', 'btn-icon system-events-close'); this.closeButton.type = 'button'; this.closeButton.title = 'Close system events'; this.closeButton.setAttribute('aria-label', 'Close system events');
    this.closeButton.innerHTML = '<span class="icon-mask icon-mask-close" aria-hidden="true"></span>'; this.closeButton.onclick = () => { this.close(); this.toggle.focus(); };
    head.append(this.closeButton);
    this.list = node('div', 'system-events-list'); this.popover.append(head, this.list);
    const health = document.querySelector('#health-toggle');
    health.before(this.toggle);
    document.querySelector('.topbar').append(this.popover);
    this.toggle.onclick = event => { event.stopPropagation(); if (this.popover.hidden) this.open(); else this.close(); };
    // A click outside closes the panel. The More menu on a phone forwards its System events item to the bell, so a click in that menu is not outside:
    // it would close the panel the same click just opened.
    document.addEventListener('click', event => { if (!this.popover.hidden && !this.popover.contains(event.target) && !this.toggle.contains(event.target) && !event.target.closest?.('#more-menu')) this.close(); });
    document.addEventListener('keydown', event => { if (event.key === 'Escape' && !this.popover.hidden) { this.close(); this.toggle.focus(); } });
    applyChatPreferences();
  }

  reset() { this.ticket++; this.events = []; this.unread = 0; this.close(); this.renderBadge(); this.renderList(); }
  setUser() { applyChatPreferences(); }

  open() {
    this.popover.hidden = false; this.toggle.setAttribute('aria-expanded', 'true');
    this.unread = 0; this.renderBadge(); this.renderList(); focusPopover(this.popover);
  }
  close() { returnFocus(this.popover, this.toggle); this.popover.hidden = true; this.toggle.setAttribute('aria-expanded', 'false'); }

  async load() {
    const ticket = ++this.ticket;
    try {
      const data = await request('/api/system-events?limit=100&notable=1'); // the server's list of what is worth a look (see the store's IsNotableSystemEvent); the web keeps none
      if (ticket !== this.ticket) return;
      const known = new Set(this.events.map(event => event.id));
      this.events = [...this.events, ...(data.events || []).filter(event => !known.has(event.id))].sort((a, b) => b.seq - a.seq).slice(0, keep);
      this.renderList();
    } catch (error) { captureError(error, 'system-events.load'); /* The feed is optional; the live stream still fills it. */ }
  }

  onEvent(event) {
    if (!event.notable || this.events.some(item => item.id === event.id)) return; // the server marks the events the bell shows; only those are listed and counted
    this.events.unshift(event); this.events.length = Math.min(this.events.length, keep);
    if (this.popover.hidden) { this.unread++; this.renderBadge(); } else this.renderList();
  }

  renderBadge() { this.badge.textContent = String(this.unread); this.badge.hidden = !this.unread; }

  // One line per event: what happened, which agent, which room, how long ago. The detail (an exit code, a reason) is behind the row: a click, Enter or Space opens it.
  renderList() {
    if (!this.events.length) { this.list.replaceChildren(node('p', 'system-events-empty', 'Nothing needs a look.')); return; }
    // Identical neighbours (the same alert again and again) are one row with a count and the newest time.
    const groups = [];
    for (const event of this.events) {
      const view = describeEvent(event, this.names), last = groups.at(-1);
      if (last && last.view.text === view.text && last.view.where === view.where && last.view.details === view.details && last.event.kind === event.kind) last.count++;
      else groups.push({ event, view, count: 1 });
    }
    this.list.replaceChildren(...groups.map(({ event, view, count }) => {
      const row = node('div', 'system-event-row'); row.dataset.kind = event.kind;
      const line = node('div', 'system-event-line');
      const main = node('div', 'system-event-main');
      main.append(node('span', 'system-event-text', view.text));
      if (count > 1) { const times = node('span', 'system-event-count', `×${count}`); times.title = `${count} identical events in a row; the newest is shown`; main.append(times); }
      if (view.where) main.append(node('span', 'system-event-where', view.where));
      const time = node('time', 'system-event-time', relativeTime(event.created_at)); time.dateTime = event.created_at; time.title = new Date(event.created_at).toLocaleString();
      line.append(main, time); row.append(line);
      if (view.details) {
        const details = node('div', 'system-event-details', view.details); details.hidden = true; row.append(details);
        row.classList.add('system-event-expandable'); row.tabIndex = 0; row.setAttribute('role', 'button'); row.setAttribute('aria-expanded', 'false');
        const toggle = () => { details.hidden = !details.hidden; row.setAttribute('aria-expanded', String(!details.hidden)); };
        row.addEventListener('click', toggle);
        row.addEventListener('keydown', key => { if (key.target === row && (key.key === 'Enter' || key.key === ' ')) { key.preventDefault(); toggle(); } });
      }
      return row;
    }));
  }
}
