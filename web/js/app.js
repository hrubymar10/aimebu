import './dialog-focus.js';
import { request, setSession, clearSession, currentUser, rights } from './api.js';
import { Sidebar } from './pages/sidebar/page.js';
import { Chat } from './chat.js';
import { setProjectNames } from './sender-identity.js';
import { ReadReceipts } from './read-receipts.js';
import { EventStream } from './events.js';
import { Inbox } from './pages/inbox/page.js';
import { createInboxSource } from './inbox-source.js';
import { InboxNotifications } from './inbox-notifications.js';
import { AgentsPanel } from './pages/agents/page.js';
import { createAgentSource } from './agent-source.js';
import { SettingsPanel } from './settings.js';
import { UsagesPopover } from './usages-popover.js';
import { TopbarMenu } from './pages/topbar/page.js';
import { createUsagesSource } from './usages-source.js';
import { KnowledgePanel } from './pages/knowledge/page.js';
import { Health } from './pages/health/page.js';
import { SystemEvents } from './system-events.js';
import { retention } from './retention.js';
import { Signin } from './pages/signin/page.js';
import { showWelcome } from './pages/setup/welcome.js';
import { ownerSetup } from './pages/setup/page.js';
import { Onboarding } from './pages/onboarding/page.js';
import { loadPreferences } from './preferences.js';
import { RoomMenu } from './pages/room-menu/page.js';
import { MessageDebug } from './message-debug.js';
import { initMobileTabs } from './pages/mobile-tabs/page.js';
import { initUIVariants } from './ui-variant.js';
import { copyText } from './markdown.js';
import { ImportV1Wizard } from './import-v1.js';
import { offerHarnessSetup, anyHarnessConfigured, HARNESS_SKIP_PREFERENCE } from './harness-offer.js';
import { DiskUsage } from './pages/health/disk.js';
import { showBuildVersion } from './build-version.js';
import { NotificationBanner, BANNER_DISMISSED_PREFERENCE } from './notification-banner.js';


initUIVariants();

const loginView = document.querySelector('#login-view'), appView = document.querySelector('#app-view');
const bootView = document.querySelector('#boot-view');
const startupStatus = document.createElement('div');
startupStatus.id = 'startup-status'; startupStatus.setAttribute('role', 'status'); startupStatus.hidden = true;
const startupText = document.createElement('span');
const startupRetry = document.createElement('button'); startupRetry.type = 'button'; startupRetry.className = 'btn btn-primary'; startupRetry.textContent = 'Retry'; startupRetry.hidden = true;
startupRetry.onclick = () => location.reload();
startupStatus.append(startupText, startupRetry); appView.append(startupStatus);
const appToast = document.createElement('p'); appToast.id = 'app-toast'; appToast.setAttribute('role', 'status'); appToast.hidden = true; appView.append(appToast);
let toastTimer;
function showToast(message) {
  clearTimeout(toastTimer); appToast.textContent = message; appToast.hidden = false;
  toastTimer = setTimeout(() => { appToast.hidden = true; }, 5000);
}
function setStartupState(state) {
  const busy = state === 'loading';
  for (const child of appView.children) if (child !== appView.querySelector('.workspace-bar') && child !== startupStatus) child.inert = busy;
  for (const button of appView.querySelectorAll('.workspace-bar button:not(#logout)')) button.inert = busy;
  if (busy) appView.setAttribute('aria-busy', 'true'); else appView.removeAttribute('aria-busy');
  startupStatus.hidden = state === 'ready'; startupStatus.dataset.loading = String(busy);
  startupText.textContent = busy ? 'Finishing sign-in…' : state === 'timeout' ? "Some checks didn't finish." : '';
  startupRetry.hidden = state !== 'timeout';
}
const signin = new Signin({
  view: loginView,
  signedIn: session => signedIn(session),
  afterReset: () => { resetWorkspace(); clearSession(); appView.hidden = true; void signin.show(); },
  resync: () => resync(),
  onError: error => console.error('sign-in page:', error),
});
const chat = new Chat(async room => {
  const generation = sessionGeneration;
  await sidebar.refreshProject(room.project_id);
  if (generation !== sessionGeneration) return;
  const current = sidebar.rooms.get(room.id) || room;
  chat.setRoomFacts(current); agents.setRoomFacts(current);
});
const receipts = new ReadReceipts(chat);
const sidebar = new Sidebar(document.querySelector('#projects'), (room, messageID) => selectRoom(room, messageID));
sidebar.watchPreview();
// Pin and hide are set from the room settings; the room lists keep the per-user state.
chat.roomView = id => sidebar.rooms.get(id);
chat.setRoomPreference = (room, field, value) => sidebar.setPreference(room, field, value);
sidebar.bindSort(document.querySelector('#rooms-sort'));
sidebar.bindCollapseAll(document.querySelector('#rooms-fold'));
chat.projectRole = projectID => sidebar.projects.get(projectID)?.role;
chat.projectArchived = projectID => !!sidebar.projects.get(projectID)?.archived_at;
setProjectNames(projectID => sidebar.projects.get(projectID)?.name || '');
// A history window hides the room's newest messages, so refreshes while it is shown do not mark the open room read; returning to
// the latest view does. (Entering a room through a jump still marks it read on selection, before the jump.)
sidebar.holdRead = id => chat.room?.id === id && (chat.contextMode || chat.catchingUp);
sidebar.shownSeq = id => chat.room?.id === id ? chat.newestShownSeq() : 0;
chat.onLatest = room => { if (sidebar.active === room.id) sidebar.markSeen(sidebar.rooms.get(room.id)); };
// A room that was open here is closed: no chat, no knowledge, no room in the address.
function closeOpenRoom() {
  chat.reset(); knowledge.setRoom(null);
  history.replaceState(null, '', `${location.pathname}${location.search}`);
}
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
// A room the person opened that the server no longer has: a short notice, then the same way out as a removal seen live.
async function roomGone(room) {
  showToast(`${room.title || 'That room'} no longer exists.`);
  // A chat between agents is filed under a reserved project that is not a project: only that chat goes, never the project.
  if (sidebar.isAgentChatProject(room.project_id)) { await chat.onChatLeft(room); return; }
  try { await roomRemoved(room.project_id, room.id); } catch (error) {
    if ([403, 404].includes(error?.status)) await projectRemoved(room.project_id); else throw error;
  }
}
// Leaving a chat between agents: it drops out of the list, and if it was open the person goes to a project's home room.
chat.onChatLeft = async room => {
  const generation = sessionGeneration;
  sidebar.forgetRoom(room.id);
  try { await sidebar.refreshAgentChats(); } catch { /* the list is read again with the next event; the chat is already gone from here */ }
  if (generation !== sessionGeneration || chat.room?.id !== room.id) return;
  const home = [...sidebar.rooms.values()].find(item => item.kind === 'home');
  if (home) await selectRoom(home); else { chat.reset(); refreshEmptyState(); }
};
// A deleted project: its rooms leave the list, and a room of it that was open gives way to the empty state.
async function projectRemoved(projectID) {
  if (sidebar.isAgentChatProject(projectID)) return; // the reserved project of the chats between agents is never removed
  const generation = sessionGeneration;
  const wasOpen = chat.room?.project_id === projectID;
  sidebar.forgetProject(projectID);
  if (generation !== sessionGeneration) return;
  if (wasOpen) closeOpenRoom();
  refreshEmptyState();
}
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
if (new URLSearchParams(location.search).has('mock_agents')) window.__mockAgentsPanel = agents;
const health = new Health(sidebar, agents, async (room, agentID) => {
  // Without an Inbox item behind it, a health entry opens the agent: its room, then its profile.
  if (!room) { showToast('That agent’s room is no longer available.'); return; }
  const generation = sessionGeneration;
  if (!await selectRoom(room) || generation !== sessionGeneration) return;
  const selection = roomSelectionGeneration;
  if (!agentID) return;
  const found = await agents.waitForCard(agentID, room.id);
  if (generation !== sessionGeneration || selection !== roomSelectionGeneration) return;
  if (found) agents.openProfile(agentID);
  else showToast('That agent is no longer available in this room.');
}, async (row, flag) => {
  const kind = { recovery_waiting: 'recovery', held_answer: 'delivery_failed' }[flag.code];
  const item = kind && inbox.items?.find(entry => (flag.code === 'recovery_waiting' && entry.id === agents.cards.get(row.id)?.recovery_inbox_id) || (entry.agent_id === row.id && entry.kind === kind));
  if (!item) return false;
  agents.setMobileOpen(false); setRoomsOpen(false); inbox.show();
  requestAnimationFrame(() => document.querySelector(`[data-inbox-id="${CSS.escape(item.id)}"]`)?.scrollIntoView({ block: 'center' }));
  return true;
}, new DiskUsage());
const systemEvents = new SystemEvents({ agentName: id => agents.cards.get(id)?.name, roomTitle: id => sidebar.rooms.get(id)?.title });
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
// A room the server made (a continued Claude Code session's): refresh the project's list so it is there, then open it.
agents.showRoomByID = async (projectID, roomID) => { await sidebar.refreshProject(projectID); const room = sidebar.rooms.get(roomID); if (room) await selectRoom(room); };
const settings = new SettingsPanel(createUsagesSource(), () => ({ projects: [...sidebar.projects.values()], rooms: [...sidebar.rooms.values()], selectedProjectID: chat.room?.project_id }), notifications);
inbox.openUsages = () => settings.open('usages');
chat.projectSettings = settings.project; // the project's settings form, shown in the home room's settings
// Archiving or reopening a project in Settings: take its new state from the server now, as the event that follows would.
settings.project.onLifecycle = async projectID => {
  const generation = sessionGeneration;
  const projects = await request('/api/projects'); if (generation !== sessionGeneration) return;
  const project = projects.find(item => item.id === projectID);
  if (!project) return;
  await sidebar.addProject(project); if (generation !== sessionGeneration) return;
  const open = chat.room && sidebar.rooms.get(chat.room.id);
  if (open && open.project_id === projectID) chat.setRoomFacts(open);
  settings.project.load(projectID);
};
settings.project.onRemoved = async projectID => { await projectRemoved(projectID); };
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
  // The server creates every project with its home room and a "general" task room beside it. Work starts in the home room: the leader there
  // opens a task room for each request.
  await sidebar.addProject(project);
  if (generation !== sessionGeneration) return;
  const home = [...sidebar.rooms.values()].find(room => room.project_id === project.id && room.kind === 'home');
  if (home) await selectRoom(home);
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
  try { await sidebar.refreshProject(room.project_id); } catch (error) {
    // The project itself is gone (or no longer reachable): it leaves the list, as when its removal arrives live. A chat between agents
    // belongs to no project, so a failed read of their list proves nothing about it: the room opens from what is already known.
    if (!sidebar.isAgentChatProject(room.project_id)) {
      if (![403, 404].includes(error?.status) || !current()) throw error;
      await projectRemoved(room.project_id);
      return false;
    }
  }
  if (!current()) return false;
  room = sidebar.rooms.get(room.id) || room;
  sidebar.setActive(room.id);
  inbox.setRoom(room);
  // An older agent-list request must not restore spawn controls for the
  // previous room while this room's messages are still loading.
  agents.setRoomFacts(room);
  history.replaceState(null, '', `${location.pathname}${location.search}#${encodeURIComponent(room.id)}`);
  try { localStorage.setItem('aimebu2.lastRoom', room.id); } catch { /* private mode: nothing to remember */ }
  try { await chat.open(room); } catch (error) {
    // The room was removed (or taken away) after the list was read, in another tab or by another person: say so and go to the project's home
    // room. A 403 is the same: the person no longer reaches it.
    if (![403, 404].includes(error?.status) || !current()) throw error;
    await roomGone(room);
    return false;
  }
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
    // A room that is open here but gone from the lists gives way to its project's home room.
    const open = chat.room;
    const home = open && !sidebar.rooms.has(open.id) ? [...sidebar.rooms.values()].find(room => room.project_id === open.project_id && room.kind === 'home') : null;
    const selected = home || sidebar.rooms.get(chat.room?.id || decodeURIComponent(location.hash.slice(1))) || sidebar.rooms.get(remembered);
    if (selected) await selectRoom(selected);
    else {
      const first = [...sidebar.rooms.values()][0];
      if (first) await selectRoom(first);
      else if (chat.room && !sidebar.rooms.has(chat.room.id)) closeOpenRoom(); // the last project is gone: nothing is left to show
    }
  }
  if (generation !== sessionGeneration) return null;
  sidebar.releaseFolds(); // a room was opened above if there was one: only now may the projects fold (the one holding the open room does not)
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
  setSession(session); void showBuildVersion(request, document.querySelector('#build-version')); void retention.load(); const storedPreferences = await loadPreferences(session.id || session.user_id); harnessSkipStored = storedPreferences?.[HARNESS_SKIP_PREFERENCE] === '1'; settings.setUser(session.id || session.user_id); settings.setRights(rights); applyRights(); systemEvents.setUser(); chat.applyMarkdownMode(); notificationBanner.reset(storedPreferences?.[BANNER_DISMISSED_PREFERENCE] === '1'); document.title = 'aimebu'; signin.hide();
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
      if (event.kind === 'project_created' || event.kind === 'project_updated' || event.kind === 'project_archived' || event.kind === 'project_reopened') { const projects = await request('/api/projects'); if (!active()) return; const project = projects.find(p => p.id === event.project_id); if (project) { await sidebar.addProject(project); if (!active()) return; refreshEmptyState(); const open = chat.room && sidebar.rooms.get(chat.room.id); if (open && open.project_id === event.project_id && event.kind !== 'project_created') { chat.title.textContent = open.title; chat.setRoomFacts(open); } } }
      else if ((event.kind === 'room_preference_changed' || event.kind === 'room_read_changed') && payload.user_id === currentUser) await sidebar.refreshProject(event.project_id);
      else if (event.kind === 'room_removed') await roomRemoved(event.project_id, payload.room_id);
      else if (event.kind === 'project_removed') await projectRemoved(payload.project_id);
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
      if ((event.agent_id && agents.cards.has(event.agent_id)) || event.kind.startsWith('inbox_item_') || event.kind.startsWith('machine_')) health.schedule();
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
    if (signin.pendingInvite) signin.openDialog();
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
      appView.hidden = true; void signin.show();
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
document.querySelector('#logout').onclick = async () => { cancelStartup(); try { await request('/api/logout', { method: 'POST' }); } finally { resetWorkspace(); clearSession(); document.title = 'aimebu'; appView.hidden = true; void signin.show(); } };
document.querySelector('#inbox-toggle').onclick = () => { setRoomsOpen(false); agents.setMobileOpen(false); if (!inbox.view.hidden) inbox.hide(); else inbox.show(); };
document.querySelector('#rooms-toggle').onclick = () => { inbox.hide(); if (matchMedia('(max-width: 700px)').matches) { appView.classList.add('chat-open'); setRoomsOpen(!appView.classList.contains('rooms-open')); } else { setRoomsCollapsed(!appView.classList.contains('rooms-collapsed')); } };
// Desktop left-panel collapse persists under the v1 localStorage key.
// On a touch tablet (701-900px) only one side panel is open at a time; the panel one closes to make room is not remembered as a choice, but a
// person's own expand or collapse is (see pages/agents/page.js).
const TABLET = matchMedia('(min-width: 701px) and (max-width: 900px) and (pointer: coarse)');
function setRoomsCollapsed(collapsed, remember = true) {
  appView.classList.toggle('rooms-collapsed', collapsed);
  document.querySelector('#rooms-toggle').setAttribute('aria-expanded', String(!collapsed));
  document.querySelector('#edge-rooms').setAttribute('aria-expanded', String(!collapsed));
  if (remember) localStorage.setItem('aimebu_left_collapsed', String(collapsed));
  if (!collapsed && TABLET.matches) document.dispatchEvent(new CustomEvent('side-panel-open', { detail: 'rooms' }));
}
document.addEventListener('side-panel-open', event => { if (event.detail === 'agents' && TABLET.matches) setRoomsCollapsed(true, false); });
// With no choice of its own, a narrow window (701-900px) starts with the rooms column collapsed, and that default is not stored; whoever expands
// it keeps it expanded, and a stored choice always wins. (A phone has its rooms panel as an overlay, closed until it is opened.)
const storedLeft = localStorage.getItem('aimebu_left_collapsed');
if (storedLeft === 'true') setRoomsCollapsed(true);
else if (storedLeft === null && matchMedia('(min-width: 701px) and (max-width: 900px)').matches) setRoomsCollapsed(true, false);
document.querySelector('#edge-rooms').onclick = () => setRoomsCollapsed(true);
document.querySelector('#edge-rooms-expand').onclick = () => setRoomsCollapsed(false);
document.querySelector('#back').onclick = () => setRoomsOpen(!appView.classList.contains('rooms-open'));
document.querySelector('#rooms-scrim').onclick = () => setRoomsOpen(false);
document.addEventListener('keydown', event => { if (event.key === 'Escape') { setRoomsOpen(false); agents.setMobileOpen(false); } });
document.querySelector('#boot-retry').onclick = () => location.reload();
try {
  // The first-run owner setup (welcome, setup token, Create owner, theme) runs on /setup, and also on / while the server has no owner and
  // is waiting for its setup token: no session exists yet, and it ends signed in as the new owner.
  const runSetup = () => ownerSetup({ views: { token: document.querySelector('#setup-token-view'), owner: document.querySelector('#setup-view'), theme: document.querySelector('#theme-view') } });
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
  if (error.status === 401) { bootView.hidden = true; void signin.show(); }
  else if (!bootView.hidden) {
    document.querySelector('#boot-status').textContent = 'Could not load workspace.';
    document.querySelector('#boot-retry').hidden = false;
  } else document.querySelector('#chat-error').textContent = error.message;
}
initMobileTabs();
