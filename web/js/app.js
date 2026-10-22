import './dialog-focus.js';
import { request, setSession, clearSession, currentUser, rights } from './api.js';
import { Sidebar } from './sidebar.js';
import { Chat } from './chat.js';
import { setProjectNames } from './sender-identity.js';
import { ReadReceipts } from './read-receipts.js';
import { EventStream } from './events.js';
import { Inbox } from './inbox.js';
import { createInboxSource } from './inbox-source.js';
import { InboxNotifications } from './inbox-notifications.js';
import { AgentsPanel } from './agents.js';
import { createAgentSource } from './agent-source.js';
import { SettingsPanel } from './settings.js';
import { UsagesPopover } from './usages-popover.js';
import { TopbarMenu } from './topbar-menu.js';
import { createUsagesSource } from './usages-source.js';
import { KnowledgePanel } from './knowledge.js';
import { Health } from './health.js';
import { SystemEvents } from './system-events.js';
import { retention } from './retention.js';
import { RetentionSettings } from './retention-settings.js';
import { showWelcome } from './welcome.js';
import { ownerSetup } from './setup.js';
import { Onboarding } from './onboarding.js';
import { loadPreferences } from './preferences.js';
import { SettingsBackup } from './settings-backup.js';
import { RoomMenu } from './room-menu.js';
import { MessageDebug } from './message-debug.js';
import { initMobileTabs } from './mobile-tabs.js';
import { initUIVariants } from './ui-variant.js';
import { copyText } from './markdown.js';
import { ImportV1Wizard } from './import-v1.js';
import { offerHarnessSetup, anyHarnessConfigured, HARNESS_SKIP_PREFERENCE } from './harness-offer.js';
import { DiskUsage } from './disk-usage.js';
import { NotificationBanner, BANNER_DISMISSED_PREFERENCE } from './notification-banner.js';


initUIVariants();

const loginView = document.querySelector('#login-view'), appView = document.querySelector('#app-view');
const bootView = document.querySelector('#boot-view');
const loginForm = document.querySelector('#login-form');
const startupStatus = document.createElement('div');
startupStatus.id = 'startup-status'; startupStatus.setAttribute('role', 'status'); startupStatus.hidden = true;
const startupText = document.createElement('span');
const startupRetry = document.createElement('button'); startupRetry.type = 'button'; startupRetry.className = 'btn btn-primary'; startupRetry.textContent = 'Retry'; startupRetry.hidden = true;
startupRetry.onclick = () => location.reload();
startupStatus.append(startupText, startupRetry); appView.append(startupStatus);
function setStartupState(state) {
  const busy = state === 'loading';
  for (const child of appView.children) if (child !== appView.querySelector('.workspace-bar') && child !== startupStatus) child.inert = busy;
  for (const button of appView.querySelectorAll('.workspace-bar button:not(#logout)')) button.inert = busy;
  if (busy) appView.setAttribute('aria-busy', 'true'); else appView.removeAttribute('aria-busy');
  startupStatus.hidden = state === 'ready'; startupStatus.dataset.loading = String(busy);
  startupText.textContent = busy ? 'Finishing sign-in…' : state === 'timeout' ? "Some checks didn't finish." : '';
  startupRetry.hidden = state !== 'timeout';
}
let pendingInvite = new URLSearchParams(location.hash.slice(1)).get('invite') || '';
let redeemedInviteLogin = '';
// A pending invitation link is its own card, in place of the sign-in card: what the link is for, the login (chosen for a new account,
// shown for a reset) and a password, and one action. Someone who already has an account can still switch to signing in.
const invitePanel = document.createElement('form'); invitePanel.className = 'invite-entry'; invitePanel.hidden = true; invitePanel.noValidate = false;
invitePanel.innerHTML = '<h1>aimebu</h1><p class="invite-lead"></p><label class="invite-login">Login<input name="login" autocomplete="username"></label><label>Password<input name="password" type="password" autocomplete="new-password" required minlength="12"></label><button type="submit" class="btn btn-primary"></button><p class="error" role="alert"></p><button type="button" class="btn invite-switch">I already have an account</button>';
loginView.append(invitePanel);
const inviteLead = invitePanel.querySelector('.invite-lead'), inviteSubmit = invitePanel.querySelector('button[type=submit]'), inviteSwitch = invitePanel.querySelector('.invite-switch');
let inviteInfo = null;
const roleWords = { owner: 'an owner', operator: 'an operator', viewer: 'a viewer' };
// Shows the sign-in screen, or the invitation card when a link is pending. The link is read first, so the card can say what it is for.
async function showLogin() {
  loginView.hidden = false;
  if (!pendingInvite) { invitePanel.hidden = true; loginForm.hidden = false; return; }
  const token = pendingInvite;
  inviteInfo = await request('/api/invites/inspect', { method: 'POST', body: { token } }).catch(error => ({ error }));
  if (token !== pendingInvite) return;
  const info = inviteInfo, login = invitePanel.elements.login;
  invitePanel.querySelector('.error').textContent = '';
  if (info.error) {
    inviteLead.textContent = info.error.status === 429 ? 'Too many tries. Wait a moment and reload this link.' : 'This invitation is not valid: it was already used, it expired, or the person who made it can no longer invite.';
    for (const node of invitePanel.querySelectorAll('label, button[type=submit]')) node.hidden = true;
    inviteSwitch.textContent = 'Sign in'; inviteSwitch.hidden = false;
  } else {
    for (const node of invitePanel.querySelectorAll('label, button[type=submit]')) node.hidden = false;
    const reset = info.kind === 'reset';
    inviteLead.textContent = reset ? `Choose a new password for ${info.login}.` : `You are invited to ${info.project_name} as ${roleWords[info.role] || info.role}. Choose a login and a password to create your account.`;
    invitePanel.querySelector('.invite-login').hidden = false;
    login.value = reset ? info.login : login.value; login.readOnly = reset; login.required = !reset;
    invitePanel.querySelector('.invite-login').firstChild.textContent = reset ? 'Account' : 'Login';
    inviteSubmit.textContent = reset ? 'Set password and sign in' : 'Create account and sign in';
    inviteSwitch.textContent = 'I already have an account'; inviteSwitch.hidden = reset;
  }
  invitePanel.hidden = false; loginForm.hidden = true;
}
const inviteDialog = document.createElement('dialog'); inviteDialog.className = 'invite-dialog dialog-card dialog-card-primary';
inviteDialog.innerHTML = '<h2>Use invitation</h2><p>Join this project with your signed-in account. If this is a password reset link, enter a new password.</p><form><label>New password (reset links only)<input name="password" type="password" autocomplete="new-password"></label><div class="dialog-actions"><button type="button" class="btn invite-cancel">Cancel</button><button type="submit" class="btn btn-primary">Use invitation</button></div></form><p class="error" role="alert"></p>';
document.body.append(inviteDialog);
inviteDialog.querySelector('.invite-cancel').onclick = () => inviteDialog.close();
const chat = new Chat(async room => {
  const generation = sessionGeneration;
  await sidebar.refreshProject(room.project_id);
  if (generation !== sessionGeneration) return;
  const current = sidebar.rooms.get(room.id) || room;
  chat.setRoomFacts(current); agents.setRoomFacts(current);
});
const receipts = new ReadReceipts(chat);
const sidebar = new Sidebar(document.querySelector('#projects'), (room, messageID) => selectRoom(room, messageID));
// Pin and hide are set from the room settings; the room lists keep the per-user state.
chat.roomView = id => sidebar.rooms.get(id);
chat.setRoomPreference = (room, field, value) => sidebar.setPreference(room, field, value);
sidebar.bindSort(document.querySelector('#rooms-sort'));
sidebar.bindCollapseAll(document.querySelector('#rooms-fold'));
chat.projectRole = projectID => sidebar.projects.get(projectID)?.role;
setProjectNames(projectID => sidebar.projects.get(projectID)?.name || '');
// A history window hides the room's newest messages, so refreshes while it is shown do not mark the open room read; returning to
// the latest view does. (Entering a room through a jump still marks it read on selection, before the jump.)
sidebar.holdRead = id => chat.room?.id === id && (chat.contextMode || chat.catchingUp);
sidebar.shownSeq = id => chat.room?.id === id ? chat.newestShownSeq() : 0;
chat.onLatest = room => { if (sidebar.active === room.id) sidebar.markSeen(sidebar.rooms.get(room.id)); };
// A removed room is gone for everyone: refresh its project and, if it was open here, go to the project's home room.
async function roomRemoved(projectID, roomID) {
  const generation = sessionGeneration;
  sidebar.forgetRoom(roomID);
  await sidebar.refreshProject(projectID);
  if (generation !== sessionGeneration || chat.room?.id !== roomID) return;
  const home = [...sidebar.rooms.values()].find(room => room.project_id === projectID && room.kind === 'home');
  if (home) await selectRoom(home);
}
chat.onRoomRemoved = room => roomRemoved(room.project_id, room.id);
// A "#N" link to a message of another room: open that room on the message (the server told which room holds it).
chat.onOpenMessage = async found => {
  const room = sidebar.rooms.get(found.room_id);
  if (!room) { chat.error.textContent = `Message #${found.no} is in a room that is not in your list.`; return; }
  await selectRoom(room, found.id);
};
const notifications = new InboxNotifications(item => {
  inbox.show();
  requestAnimationFrame(() => document.querySelector(`[data-inbox-id="${CSS.escape(item.id)}"]`)?.scrollIntoView({ block: 'center' }));
}, (room, messageID) => { void selectRoom(sidebar.rooms.get(room.id) || room, messageID); });
// The bar that offers browser notifications the first time something needs the person's attention; dismissing it is the account's choice.
const notificationBanner = new NotificationBanner({ notifications, saveDismissed: () => request('/api/me/preferences', { method: 'PUT', body: { preferences: { [BANNER_DISMISSED_PREFERENCE]: '1' } } }) });
notifications.onAttention = () => notificationBanner.attention();
notifications.onPermission = () => notificationBanner.sync();
const inbox = new Inbox(createInboxSource(), sidebar.projects, sidebar.rooms, async item => {
  const room = sidebar.rooms.get(item.room_id); if (!room) return;
  const generation = sessionGeneration;
  inbox.hide(); if (!await selectRoom(room) || generation !== sessionGeneration) return;
  if (item.message_id) await chat.jumpToMessage(item.message_id); // loads an older message or a thread reply too, then highlights it
}, notifications, items => chat.updateAnswerControls(items));
chat.setInbox(inbox);
const agents = new AgentsPanel(createAgentSource(), agent => {
  agents.setMobileOpen(false);
  setRoomsOpen(false);
  inbox.show();
  const item = inbox.items?.find(entry => entry.id === agent.recovery_inbox_id || (entry.agent_id === agent.id && entry.kind === 'recovery'));
  if (item) document.querySelector(`[data-inbox-id="${CSS.escape(item.id)}"]`)?.scrollIntoView({ block: 'center' });
}, cards => chat.setAgents(cards), async agent => {
  const generation = sessionGeneration;
  try {
    const room = await request(`/api/agents/${encodeURIComponent(agent.id)}/dm`, { method: 'POST' });
    if (generation !== sessionGeneration) return;
    await sidebar.refreshProject(room.project_id);
    if (generation !== sessionGeneration) return;
    await selectRoom(sidebar.rooms.get(room.id) || room);
  } catch (error) { if (generation === sessionGeneration) agents.error.textContent = error.message || 'Could not open private chat'; }
}, async human => {
  const generation = sessionGeneration;
  const projectID = chat.room?.project_id; if (!projectID) return;
  try {
    const room = await request(`/api/projects/${encodeURIComponent(projectID)}/dms`, { method: 'POST', body: { user_id: human.id } });
    if (generation !== sessionGeneration) return;
    await sidebar.refreshProject(room.project_id || projectID);
    if (generation !== sessionGeneration) return;
    await selectRoom(sidebar.rooms.get(room.id) || room);
  } catch (error) { if (generation === sessionGeneration) agents.error.textContent = error.message || 'Could not open private chat'; }
});
const health = new Health(sidebar, agents, async (room, agentID) => {
  // Without an Inbox item behind it, a health entry opens the agent: its room, then its profile.
  const generation = sessionGeneration;
  if (!await selectRoom(room) || generation !== sessionGeneration) return;
  if (agentID && agents.cards.has(agentID)) agents.openProfile(agentID);
}, async (row, flag) => {
  const kind = { recovery_waiting: 'recovery', held_answer: 'delivery_failed' }[flag.code];
  const item = kind && inbox.items?.find(entry => (flag.code === 'recovery_waiting' && entry.id === agents.cards.get(row.id)?.recovery_inbox_id) || (entry.agent_id === row.id && entry.kind === kind));
  if (!item) return false;
  agents.setMobileOpen(false); setRoomsOpen(false); inbox.show();
  requestAnimationFrame(() => document.querySelector(`[data-inbox-id="${CSS.escape(item.id)}"]`)?.scrollIntoView({ block: 'center' }));
  return true;
}, new DiskUsage());
const systemEvents = new SystemEvents({ agentName: id => agents.cards.get(id)?.name, roomTitle: id => sidebar.rooms.get(id)?.title });
new RetentionSettings();
new SettingsBackup();
const messageDebug = new MessageDebug();
chat.setDebugInspector(message => messageDebug.open(message));
// The inspector walks the room's messages (v1's previous and next) and carries the actions the message bubble leaves out in style A.
messageDebug.setContext({ messages: () => chat.orderedMessages(), thread: message => chat.openThread(message), copy: text => copyText(text) });
setInterval(() => {
  if (appView.hidden) return;
  health.refresh();
  agents.refresh();
}, 30000);
chat.setSpawnOpener(() => agents.openSpawn());
chat.setLegacyActions(start => agents.openSpawn({ resume: start }), id => { agents.setMobileOpen(true); agents.openProfile(id); });
agents.projectRole = chat.projectRole;
agents.projectRooms = projectID => [...sidebar.rooms.values()].filter(room => room.project_id === projectID);
agents.showRoom = room => selectRoom(room);
const settings = new SettingsPanel(createUsagesSource(), () => ({ projects: [...sidebar.projects.values()], selectedProjectID: chat.room?.project_id }), notifications);
inbox.openUsages = () => settings.open('usages');
chat.projectSettings = settings.project; // the home room's settings show the same project form
settings.onRights = () => applyRights();
document.addEventListener('aimebu:open-settings', event => settings.open(event.detail?.section || 'general', event.detail));
const importWizard = new ImportV1Wizard(() => {
  const generation = sessionGeneration;
  return () => generation === sessionGeneration;
});
// No room selected: the composer, members and room controls give way to a card that says what to do next.
function refreshEmptyState() {
  const none = !chat.room;
  appView.classList.toggle('no-room', none);
  if (!none) return;
  const text = document.querySelector('#chat-empty-text'), action = document.querySelector('#chat-empty-action');
  const first = sidebar.projects.size === 0;
  text.textContent = first ? 'Create a project to give your agents a place to work.' : 'Pick a room from the list to read it and talk to its agents.';
  action.textContent = first ? 'Create your first project' : 'Show rooms';
  action.onclick = first ? () => onboarding.show() : () => { if (matchMedia('(max-width: 700px)').matches) setRoomsOpen(true); else setRoomsCollapsed(false); };
}
chat.onEmptyChange = refreshEmptyState;
async function createFirstProject({ name, folder }) {
  const generation = sessionGeneration;
  const project = await request('/api/projects', { method: 'POST', body: { name, folder } });
  if (generation !== sessionGeneration) return;
  // The server creates every project with its home room and a "general" task room beside it, where nearly all work happens.
  await sidebar.addProject(project);
  if (generation !== sessionGeneration) return;
  const general = [...sidebar.rooms.values()].find(room => room.project_id === project.id && room.title === 'general');
  if (general) await selectRoom(general);
}
// Whether some approved, connected machine has a harness that can start an agent now and a model to start it with; the onboarding
// offers Spawn only then. The owner and admins read every machine, anyone else the machines of the open room's project.
async function anyHarnessReady() {
  const project = chat.room?.project_id;
  if (!rights.admin && !project) return true;
  const machines = (await request(rights.admin ? '/api/machines' : `/api/machines?project_id=${encodeURIComponent(project)}`)).machines || [];
  for (const machine of machines.filter(item => item.approved && !item.revoked_at && item.connected !== false)) {
    const catalog = await request(`/api/spawn-catalog?machine_id=${encodeURIComponent(machine.id)}`);
    if ((catalog?.tools || []).some(tool => tool.available && (tool.models || []).length)) return true;
  }
  return false;
}
// Skipping the harness step is a preference of the account, so it holds on every browser until a harness exists. `harnessSkipStored` is what
// the server holds for whoever is signed in. `generation` is the session a decision was made in: after a sign-out or another person signing in,
// the answer no longer belongs to the account now signed in, so nothing is sent (as a queued preference save is dropped).
let harnessSkipStored = false;
const setHarnessSkipped = async (skipped, generation = sessionGeneration) => {
  if (generation !== sessionGeneration) return;
  const before = harnessSkipStored; harnessSkipStored = skipped;
  try { await request('/api/me/preferences', { method: 'PUT', body: { preferences: { [HARNESS_SKIP_PREFERENCE]: skipped ? '1' : '0' } } }); } catch (error) { if (generation === sessionGeneration) harnessSkipStored = before; throw error; }
};
// Onboarding or Settings sees a harness that can start an agent: a skip from earlier no longer applies, so it is reset now rather than at the
// next sign-in (a later removal of every harness then offers the step again).
const harnessSeen = () => { if (harnessSkipStored) void setHarnessSkipped(false).catch(() => {}); };
settings.harnessSettings.onReady = harnessSeen;
const onboarding = new Onboarding({ create: createFirstProject, spawn: () => agents.openSpawn(), harnessReady: anyHarnessReady, canSetUp: () => rights.admin, onSkip: () => setHarnessSkipped(true), onReady: harnessSeen });
// Skipping or postponing the v1 import closes its dialog; with no project yet the onboarding takes its place.
importWizard.dialog.addEventListener('close', () => { if (!appView.hidden && sidebar.projects.size === 0 && !onboarding.open) onboarding.show(); });
const usagesPopover = new UsagesPopover(settings.source, () => settings.open('usages'));
new TopbarMenu();
new RoomMenu();
const knowledge = new KnowledgePanel(() => ({ projects: [...sidebar.projects.values()], selectedProjectID: chat.room?.project_id }), () => chat.room);
chat.openProjectMemory = projectID => knowledge.openProjectFacts(projectID);
chat.openTaskNotes = () => knowledge.openTask();
let stream;
let startupController;
let sessionGeneration = 0;
let roomSelectionGeneration = 0;
let selectionsInFlight = 0;

function cancelStartup() {
  startupController?.abort();
  startupController = null;
  if (importWizard.dialog.open) importWizard.dialog.close();
}
function resetWorkspace() {
  cancelStartup();
  sessionGeneration++;
  stream?.stop(); stream = null;
  sidebar.reset(); chat.reset(); inbox.reset(); agents.reset(); knowledge.setRoom(null);
  health.ticket++;
  systemEvents.reset(); messageDebug.reset();
}

function setRoomsOpen(open) {
  appView.classList.toggle('rooms-open', open);
  document.querySelector('#rooms-toggle').setAttribute('aria-expanded', String(open));
  document.querySelector('#back').setAttribute('aria-expanded', String(open));
  if (open) agents.setMobileOpen(false);
}
async function selectRoom(room, messageID) {
  selectionsInFlight++;
  try { return await openRoom(room, messageID); } finally { selectionsInFlight--; }
}
async function openRoom(room, messageID) {
  const generation = sessionGeneration;
  const selection = ++roomSelectionGeneration;
  const current = () => generation === sessionGeneration && selection === roomSelectionGeneration;
  inbox.hide();
  setRoomsOpen(false);
  await sidebar.refreshProject(room.project_id);
  if (!current()) return false;
  room = sidebar.rooms.get(room.id) || room;
  sidebar.setActive(room.id);
  inbox.setRoom(room);
  // An older agent-list request must not restore spawn controls for the
  // previous room while this room's messages are still loading.
  agents.setRoomFacts(room);
  history.replaceState(null, '', `${location.pathname}${location.search}#${encodeURIComponent(room.id)}`);
  try { localStorage.setItem('aimebu2.lastRoom', room.id); } catch { /* private mode: nothing to remember */ }
  await chat.open(room);
  if (!current() || chat.room?.id !== room.id) return false;
  knowledge.setRoom(room);
  await agents.setRoom(room);
  if (!current()) return false;
  appView.classList.add('chat-open');
  if (messageID) await chat.jumpToMessage(messageID);
  if (!current()) return false;
  return true;
}
async function snapshot(generation = sessionGeneration) {
  // Take the cursor before the HTTP snapshots. Events committed while those
  // reads run are caught by the socket; its replay is coalesced on ready.
  const { seq } = await request('/api/events/cursor');
  if (generation !== sessionGeneration) return null;
  // A room the reader opens while the snapshot loads is newer than the room this snapshot would restore. Re-selecting
  // the old one would bump the selection generation and silently cancel the reader's navigation (a new private chat
  // stayed on the previous room when a resync landed in the same moment).
  const selection = roomSelectionGeneration;
  await sidebar.load();
  if (generation !== sessionGeneration) return null;
  if (selection === roomSelectionGeneration && selectionsInFlight === 0) {
    const remembered = (() => { try { return localStorage.getItem('aimebu2.lastRoom') || ''; } catch { return ''; } })();
    const selected = sidebar.rooms.get(chat.room?.id || decodeURIComponent(location.hash.slice(1))) || sidebar.rooms.get(remembered);
    if (selected) await selectRoom(selected);
    else { const first = [...sidebar.rooms.values()][0]; if (first) await selectRoom(first); }
  }
  if (generation !== sessionGeneration) return null;
  refreshEmptyState();
  return seq;
}
async function resync(generation = sessionGeneration) {
  const seq = await snapshot(generation);
  if (generation !== sessionGeneration || seq === null) return null;
  await Promise.all([inbox.refresh({ notify: false }), health.refresh()]);
  if (generation !== sessionGeneration) return null;
  return seq;
}
async function signedIn(session) {
  resetWorkspace();
  const generation = sessionGeneration;
  let startupExpired = false;
  let preStreamTimedOut = false;
  const active = () => generation === sessionGeneration;
  const startupAbort = new AbortController();
  const startupActive = () => active() && !startupExpired && !startupAbort.signal.aborted;
  startupController = startupAbort;
  setSession(session); void retention.load(); const storedPreferences = await loadPreferences(session.id || session.user_id); harnessSkipStored = storedPreferences?.[HARNESS_SKIP_PREFERENCE] === '1'; settings.setUser(session.id || session.user_id); settings.setRights(rights); applyRights(); systemEvents.setUser(); chat.applyMarkdownMode(); notificationBanner.reset(storedPreferences?.[BANNER_DISMISSED_PREFERENCE] === '1'); document.title = 'aimebu'; loginView.hidden = true;
  // The owner's first sign-in on an install shows the welcome once; the app waits behind it.
  if (session.first_run) { await showWelcome(() => request('/api/welcome/seen', { method: 'POST', body: {} })); if (!active()) return; }
  setStartupState('loading'); appView.hidden = false;
  const expireStartup = () => {
    if (!startupActive()) return;
    startupExpired = true; startupAbort.abort(); setStartupState('timeout');
  };
  // HTTP snapshots and inbox reads run before the stream exists. Bound that
  // phase separately so a stuck read still exposes Retry.
  const preStreamDeadline = setTimeout(() => {
    if (!active()) return;
    preStreamTimedOut = true;
    setStartupState('timeout');
  }, 30000);
  let startupDeadline;
  let startupSucceeded = false;
  try {
    const seq = await snapshot(generation);
    if (!active() || seq === null) return;
    if (['mock_inbox', 'mock_delivery', 'mock_resolved'].some(key => new URLSearchParams(location.search).has(key))) {
      const room = (new URLSearchParams(location.search).has('mock_delivery') && chat.room) || (rooms => rooms.find(item => item.kind === 'task' && item.title !== 'general') || rooms.find(item => item.kind === 'task'))([...sidebar.rooms.values()]); // the mocks point at a room with work in it, not the empty general every project starts with
      if (room) { const messages = await request(`/api/rooms/${room.id}/messages?after=0&limit=1`); if (!active()) return; inbox.source.setLocation(room.project_id, room.id, messages[0]?.id || ''); }
    }
    // Independent reads, so they run together rather than one after the other.
    await Promise.all([inbox.refresh({ notify: false }), health.refresh()]);
    if (!active()) return;
    systemEvents.load();
    stream = new EventStream(async event => {
      if (!active()) return;
      if (event.kind === 'snapshot_required') { await resync(generation); return; }
      let payload = {}; try { payload = JSON.parse(event.payload_json); } catch {}
      if (event.kind === 'project_created' || event.kind === 'project_updated') { const projects = await request('/api/projects'); if (!active()) return; const project = projects.find(p => p.id === event.project_id); if (project) { await sidebar.addProject(project); if (!active()) return; refreshEmptyState(); const open = chat.room && sidebar.rooms.get(chat.room.id); if (open && open.project_id === event.project_id && event.kind === 'project_updated') { chat.title.textContent = open.title; chat.setRoomFacts(open); } } }
      else if ((event.kind === 'room_preference_changed' || event.kind === 'room_read_changed') && payload.user_id === currentUser) await sidebar.refreshProject(event.project_id);
      else if (event.kind === 'room_removed') await roomRemoved(event.project_id, payload.room_id);
      else if (event.kind === 'room_created' || ['room_renamed', 'room_closing', 'room_closed', 'room_reopened', 'room_close_blocked'].includes(event.kind)) {
        await sidebar.refreshProject(event.project_id);
        if (!active()) return;
        if (event.kind === 'room_close_blocked') chat.showCloseBlocked(event.room_id, payload.reason);
        if (chat.room?.id === event.room_id) {
          const room = sidebar.rooms.get(event.room_id) || chat.room;
          chat.setRoomFacts(room); agents.setRoomFacts(room);
        }
        if (event.kind !== 'room_created') await inbox.refresh();
      }
      else if (event.kind === 'message_created' || event.kind === 'room_member_joined' || event.kind === 'room_member_left') {
        // The room's own level decides before the list refreshes; the room being read, in a focused tab, needs no alert.
        if (event.kind === 'message_created') notifications.message(payload, sidebar.rooms.get(event.room_id), sidebar.projects, currentUser, chat.room?.id === event.room_id && document.visibilityState === 'visible' && document.hasFocus());
        await sidebar.refreshProject(event.project_id);
        if (!active()) return;
        if (chat.room?.id === event.room_id) {
          chat.setRoomFacts(sidebar.rooms.get(event.room_id) || chat.room);
          if (event.kind === 'message_created') await chat.fetchMessage(payload.message_id);
        }
      }
      else if (event.kind === 'reaction_changed' && chat.nodes.has(payload.message_id)) await chat.refreshReactions(payload.message_id);
      if ((event.kind === 'project_member_changed' || event.kind === 'user_invite_redeemed') && chat.room && chat.room.project_id === event.project_id) chat.loadMentionLogins(chat.room, true);
      if (!active()) return;
      if (event.room_id && event.room_id === chat.room?.id && ['room_read_changed', 'message_created', 'turn_recorded', 'turn_process_exit'].includes(event.kind)) receipts.schedule();
      if (event.kind.startsWith('inbox_item_')) { await inbox.source.changed?.(); if (active() && event.project_id) await sidebar.refreshProject(event.project_id); }
      if (!active()) return;
      if (event.agent_id || event.kind.startsWith('inbox_item_') || event.kind.startsWith('machine_')) health.schedule();
      knowledge.onEvent(event);
      systemEvents.onEvent(event);
      await agents.source.onEvent?.(event);
    }, () => resync(generation), state => {
      if (!active()) return;
      const dot = document.querySelector('#connection');
      const label = state === 'Live' ? 'Connected' : 'Disconnected';
      dot.classList.toggle('connected', label === 'Connected');
      dot.classList.toggle('disconnected', label !== 'Connected');
      dot.title = label; dot.setAttribute('aria-label', label);
    }, seq);
    stream.onUsagesUpdated = () => { settings.usagesChanged(); usagesPopover.changed(); inbox.refresh().catch(() => {}); };
    stream.start();
    clearTimeout(preStreamDeadline);
    // A slow read can still recover; the first-run import decision remains
    // part of startup even after the Retry state becomes visible.
    if (!preStreamTimedOut) startupDeadline = setTimeout(expireStartup, 10000);
    // The v1 import is the owner's; nobody else asks (a 403 on every sign-in).
    if (rights.owner) try { await importWizard.maybeOpen(startupAbort.signal, active); } catch (error) { if (startupActive()) console.error('v1 import status:', error); }
    if (!startupActive()) return;
    refreshEmptyState();
    // The last step of the first run: with no project yet (nothing imported, or the import skipped) the owner makes the first one.
    if (!importWizard.dialog.open && sidebar.projects.size === 0) onboarding.show();
    if (pendingInvite && !inviteDialog.open) inviteDialog.showModal();
    startupSucceeded = true;
    // Items already waiting for the person are something that needs them too: the bar is offered once, the first time.
    if (inbox.items?.length) notificationBanner.attention();
    // An owner or admin with no harness that has a model is offered the Harnesses step (after a v1 import, which carries no harness over; on
    // a first sign-in from another browser; when every harness was removed): nothing can be spawned without one. The answer comes from the
    // server, and so does a skip. Outside the startup deadline: a slow supervisor must not turn a loaded workspace into a timeout.
    if (!importWizard.dialog.open && !onboarding.open && sidebar.projects.size > 0) {
      void offerHarnessSetup({
        user: currentUser, canSetUp: rights.admin, skipped: harnessSkipStored, configured: () => anyHarnessConfigured(request),
        show: async () => { if (active() && !onboarding.open) await onboarding.showHarnesses('harness-only'); }, setSkipped: value => setHarnessSkipped(value, generation),
      }).catch(error => { if (active()) console.error('harness setup:', error); });
    }
  } catch (error) {
    if (!active()) return;
    if (error.status === 401 || error.status === 403) {
      resetWorkspace(); clearSession(); document.title = 'aimebu';
      appView.hidden = true; void showLogin();
      return;
    }
    console.error('workspace startup:', error);
    setStartupState('timeout');
  } finally {
    clearTimeout(preStreamDeadline);
    clearTimeout(startupDeadline);
    if (startupSucceeded && startupActive()) setStartupState('ready');
  }
}
invitePanel.onsubmit = async event => {
  event.preventDefault();
  const errorNode = invitePanel.querySelector('.error'); errorNode.textContent = '';
  const login = invitePanel.elements.login.value.trim(), password = invitePanel.elements.password.value;
  try {
    if (!redeemedInviteLogin) {
      const reset = inviteInfo?.kind === 'reset';
      const result = await request('/api/invites/redeem', { method: 'POST', body: { token: pendingInvite, ...(reset ? {} : { login }), password } });
      pendingInvite = ''; history.replaceState(null, '', location.pathname + location.search);
      redeemedInviteLogin = result.login;
      invitePanel.elements.login.value = result.login; invitePanel.elements.login.readOnly = true;
      inviteSubmit.textContent = 'Sign in'; inviteSwitch.hidden = true;
    }
    const session = await request('/api/login', { method: 'POST', body: { login: redeemedInviteLogin, password } });
    invitePanel.hidden = true; await signedIn(session);
  } catch (error) { errorNode.textContent = error.message; }
};
// "I already have an account": the sign-in card, with the way back to the invitation; the link stays pending and is offered after sign-in.
const backToInvite = document.createElement('button'); backToInvite.type = 'button'; backToInvite.className = 'btn invite-back'; backToInvite.textContent = 'Back to the invitation'; backToInvite.hidden = true;
loginForm.append(backToInvite);
backToInvite.onclick = () => { backToInvite.hidden = true; void showLogin(); };
inviteSwitch.onclick = () => {
  if (inviteInfo?.error) pendingInvite = '';
  invitePanel.hidden = true; loginForm.hidden = false; backToInvite.hidden = !pendingInvite;
};
inviteDialog.querySelector('form').onsubmit = async event => {
  event.preventDefault();
  const errorNode = inviteDialog.querySelector('.error'); errorNode.textContent = '';
  try {
    const result = await request('/api/invites/redeem', { method: 'POST', body: { token: pendingInvite, password: event.currentTarget.elements.password.value } });
    pendingInvite = ''; inviteDialog.close();
    if (result.kind === 'reset') { resetWorkspace(); clearSession(); appView.hidden = true; void showLogin(); }
    else await resync();
  } catch (error) { errorNode.textContent = error.message; }
};
loginForm.onsubmit = async event => {
  event.preventDefault(); document.querySelector('#login-error').textContent = '';
  try { const session = await request('/api/login', { method: 'POST', body: { login: loginForm.elements.login.value, password: loginForm.elements.password.value } }); await signedIn(session); }
  catch (error) { document.querySelector('#login-error').textContent = error.message; }
};
const projectDialog = document.querySelector('#project-dialog');
const projectForm = document.querySelector('#project-form');
// Creating a project is the owner's and the admins'; for anyone else the button is off, with the reason, instead of ending in a 403.
function applyRights() {
  const button = document.querySelector('#project-open'), allowed = rights.owner || rights.admin, label = allowed ? 'New project' : 'Only the owner or an admin can create projects';
  button.disabled = !allowed; button.title = label; button.setAttribute('aria-label', label);
}
document.querySelector('#project-open').onclick = () => {
  document.querySelector('#project-error').textContent = '';
  projectDialog.showModal();
  projectForm.elements.name.focus();
};
for (const id of ['#project-cancel', '#project-cancel-footer']) document.querySelector(id).onclick = () => projectDialog.close();
projectForm.onsubmit = async event => {
  event.preventDefault();
  const generation = sessionGeneration;
  const errorNode = document.querySelector('#project-error');
  errorNode.textContent = '';
  const name = projectForm.elements.name.value.trim();
  const folder = projectForm.elements.folder.value.trim();
  if (!name || !folder) {
    errorNode.textContent = 'Enter a project name and folder path.';
    (name ? projectForm.elements.folder : projectForm.elements.name).focus();
    return;
  }
  try {
    const project = await request('/api/projects', { method: 'POST', body: { name, folder } });
    if (generation !== sessionGeneration) return;
    projectForm.reset(); projectDialog.close();
    const rooms = await sidebar.addProject(project); if (rooms[0]) await selectRoom(rooms[0]);
  } catch (error) { if (generation === sessionGeneration) errorNode.textContent = error.message; }
};
document.querySelector('#logout').onclick = async () => { cancelStartup(); try { await request('/api/logout', { method: 'POST' }); } finally { resetWorkspace(); clearSession(); document.title = 'aimebu'; appView.hidden = true; void showLogin(); } };
document.querySelector('#inbox-toggle').onclick = () => { setRoomsOpen(false); agents.setMobileOpen(false); if (!inbox.view.hidden) inbox.hide(); else inbox.show(); };
document.querySelector('#rooms-toggle').onclick = () => { inbox.hide(); if (matchMedia('(max-width: 700px)').matches) { appView.classList.add('chat-open'); setRoomsOpen(!appView.classList.contains('rooms-open')); } else { setRoomsCollapsed(!appView.classList.contains('rooms-collapsed')); } };
// Desktop left-panel collapse persists under the v1 localStorage key.
// On a touch tablet (701-900px) only one side panel is open at a time, and that is not remembered (see agents.js).
const TABLET = matchMedia('(min-width: 701px) and (max-width: 900px) and (pointer: coarse)');
function setRoomsCollapsed(collapsed, remember = !TABLET.matches) {
  appView.classList.toggle('rooms-collapsed', collapsed);
  document.querySelector('#rooms-toggle').setAttribute('aria-expanded', String(!collapsed));
  if (remember) localStorage.setItem('aimebu_left_collapsed', String(collapsed));
  if (!collapsed && TABLET.matches) document.dispatchEvent(new CustomEvent('side-panel-open', { detail: 'rooms' }));
}
document.addEventListener('side-panel-open', event => { if (event.detail === 'agents' && TABLET.matches) setRoomsCollapsed(true, false); });
if (localStorage.getItem('aimebu_left_collapsed') === 'true') setRoomsCollapsed(true);
document.querySelector('#edge-rooms').onclick = () => setRoomsCollapsed(true);
document.querySelector('#edge-rooms-expand').onclick = () => setRoomsCollapsed(false);
document.querySelector('#back').onclick = () => setRoomsOpen(!appView.classList.contains('rooms-open'));
document.querySelector('#rooms-scrim').onclick = () => setRoomsOpen(false);
document.addEventListener('keydown', event => { if (event.key === 'Escape') { setRoomsOpen(false); agents.setMobileOpen(false); } });
document.querySelector('#boot-retry').onclick = () => location.reload();
try {
  // The first-run owner setup (welcome, setup token, Create owner, theme) runs on /setup, and also on / while the server has no owner and
  // is waiting for its setup token: no session exists yet, and it ends signed in as the new owner.
  const runSetup = () => ownerSetup({ view: document.querySelector('#setup-view'), form: document.querySelector('#setup-form'), error: document.querySelector('#setup-error'), tokenView: document.querySelector('#setup-token-view'), tokenForm: document.querySelector('#setup-token-form'), themeView: document.querySelector('#theme-view') });
  let session = null;
  if (location.pathname === '/setup') { bootView.hidden = true; session = await runSetup(); }
  if (!session) {
    try {
      session = await request('/api/session'); // 200 either way: a 401 here would be a console error on every visit to the sign-in page
      if (session.signed_in === false) throw Object.assign(new Error('not signed in'), { status: 401 });
    } catch (error) {
      if (error.status !== 401) throw error;
      // Not signed in. With no owner yet the page is the setup, not a login that cannot work.
      const status = await request('/api/setup/status').catch(() => null);
      if (!status?.open) throw error;
      bootView.hidden = true; session = await runSetup();
    }
  }
  bootView.hidden = true;
  await signedIn(session);
} catch (error) {
  if (error.status === 401) { bootView.hidden = true; void showLogin(); }
  else if (!bootView.hidden) {
    document.querySelector('#boot-status').textContent = 'Could not load workspace.';
    document.querySelector('#boot-retry').hidden = false;
  } else document.querySelector('#chat-error').textContent = error.message;
}
initMobileTabs();
