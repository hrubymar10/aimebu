import { request, rights } from './api.js';
import { attachFolderField } from './folder-field.js';
import { dmProblems, peopleProblems, problemSummary, projectProblems } from './import-problems.js';
import { applyChoice, choiceValue, mergeGroups, newProjectAlias, parseChoice, planTree, roomChoice } from './import-tree.js';

// v1's own event room is never imported, so the wizard does not list it.
const SYSTEM_ROOM = '_system';

function node(tag, text = '', className = '') {
  const item = document.createElement(tag);
  item.textContent = text;
  if (className) item.className = className;
  return item;
}

// A collapsible block: long lists (the not-carried records, the skipped items) stay folded until the owner opens them.
function section(title, summary, open = false) {
  const box = node('details', '', 'import-section');
  box.open = open;
  const head = node('summary'); head.append(node('h3', title));
  if (summary) head.append(node('span', summary, 'import-section-note'));
  box.append(head);
  return box;
}

// What each line of the summary table means, in the owner's words. An unknown key falls back to its raw name.
const COUNT_LABELS = {
  agent_session_history: ['Agent session history', 'Every harness session an agent name ran in, so "Start as v2 agent" can offer the one that did a sender\'s work.'],
  agent_sessions: ['Agent sessions', 'Where each agent ran and its harness session; shown on the profile of its name in old messages.'],
  attachments: ['Attachments', 'Files attached to messages.'],
  historical_members: ['Past members', 'People and agents who were in a room; kept as names so old messages still show who wrote them.'],
  home_rooms: ['Project home rooms', 'The top room created for each new project.'],
  inline_plan_appendix: ['Inline plans setting', 'Whether leads are told to always attach the full plan.'],
  macro_conflicts: ['Macro conflicts', 'Macros that exist in both v1 and v2.'],
  macros: ['Macros', 'Shared prompt texts such as <TASK>.'],
  accounts: ['Accounts created', 'People who get a new account; each gets a one-time link after the import.'],
  memory: ['Memory records', 'Project facts, user profiles and shared agent notes.'],
  message_expiry: ['Message expiry setting', 'How long before a message counts as expired.'],
  messages: ['Messages', 'Chat history, with its original numbers.'],
  orphan_attachments: ['Unused attachment files', 'Files in the backup that no message refers to; left behind.'],
  presence_lines: ["Bookkeeping lines", "v1's own join, leave and memory lines; v2 keeps presence elsewhere, so they are not imported."],
  preferences: ['Personal settings', 'Your theme, notification volume and interface switches from v1, saved to your profile.'],
  project_memory_settings: ['Project memory switches', 'Projects that start with v2 memory turned on.'],
  projects: ['Projects', 'Workspaces the imported rooms belong to.'],
  reactions: ['Reactions', 'Emoji reactions on messages.'],
  read_cursor_baselines: ['Caught-up positions', 'Everyone except you starts caught up in imported rooms, so only your own unread messages show.'],
  read_cursors: ['Your read positions', 'How far you had read in each room, so unread counts carry over.'],
  role_conflicts: ['Role conflicts', 'Roles that exist in both v1 and v2.'],
  role_links: ['Role links flattened', 'Roles whose link to a parent role was merged into their own text.'],
  roles: ['Roles', 'Role definitions.'],
  room_memory_settings: ['Room memory switches', 'Rooms where v1 had memory turned off.'],
  room_prefs: ['Room view settings', 'Rooms you had hidden, and your project order (a room you had pinned puts its project first).'],
  rooms: ['Rooms', 'Chat rooms.'],
  settings: ['Settings', 'v1 settings that v2 does not use; they stay in the backup.'],
  sounds: ['Custom sounds', 'Notification sounds you uploaded.'],
  unresolved_memory_source_links: ['Memory links not traced', 'Memory records whose source message was not imported keep their text but lose the link back to it.'],
};

// The summary's rows in the order an owner reads them: what holds the history first, then the rest, then settings and bookkeeping last.
const COUNT_ORDER = ['projects', 'home_rooms', 'rooms', 'messages', 'attachments', 'reactions', 'memory', 'macros', 'macro_conflicts', 'roles', 'role_conflicts', 'role_links', 'accounts'];
const COUNT_LAST = new Set(['inline_plan_appendix', 'message_expiry', 'preferences', 'settings', 'room_prefs', 'project_memory_settings', 'room_memory_settings', 'read_cursors', 'read_cursor_baselines', 'historical_members', 'agent_sessions', 'agent_session_history', 'orphan_attachments', 'unresolved_memory_source_links', 'sounds']);
function countRank(key) {
  if (COUNT_ORDER.includes(key)) return COUNT_ORDER.indexOf(key);
  return COUNT_LAST.has(key) ? 200 : 100;
}

// What happened to each part of a reconciled count (report.accounting), in the owner's words. `rooms` words its own skips, since a room is not a record in one.
const ACCOUNT_GROUPS = { rooms: 'Rooms', messages: 'Messages', attachments: 'Attachments', reactions: 'Reactions', macros: 'Macros' };
const ACCOUNT_PARTS = {
  imported: 'Imported',
  already_present: 'Already in v2',
  presence_lines: "v1's own join, leave and memory bookkeeping lines (v2 keeps presence elsewhere)",
  system_room: "In v1's own event room (not conversation)",
  dm_not_imported: 'In direct messages you did not tick',
  no_project: 'In rooms with no project to go into',
  room_skipped: 'In rooms you chose not to import',
  in_skipped_rooms: 'On messages in rooms not imported',
  person_skipped: 'By people you did not map to an account',
  kept_v2_text: 'Kept the v2 text (a key both have)',
  unticked: 'Unticked on the Macros page',
  v1_builtin_example: "v1's built-in example macros you never changed (v2 has its own)",
  deleted_in_v1: 'Built-in macros you deleted in v1',
  unused_files: 'Files no message refers to',
  other: 'Other',
};
const ROOM_PARTS = { system_room: "v1's own event room", dm_not_imported: 'Direct messages you did not tick', no_project: 'Rooms with no project to go into', room_skipped: 'Rooms you chose not to import' };
const number = value => Number(value || 0).toLocaleString('en-US');

// The pages of the wizard, in order. A page that has nothing to show for this backup is skipped.
const STEPS = [
  ['projects', 'Projects', () => true],
  ['people', 'People', preview => (preview.humans || []).length > 0],
  ['rooms', 'Rooms', preview => (preview.rooms || []).some(room => !room.dm && room.id !== SYSTEM_ROOM)],
  ['dms', 'Direct messages', preview => (preview.rooms || []).some(room => room.dm)],
  ['memory', 'Memory', preview => (preview.counts?.memory || 0) > 0 || (preview.memory_projects || []).length > 0],
  ['macros', 'Macros', preview => Object.keys(preview.macros || {}).length > 0],
  ['roles', 'Roles', preview => (preview.roles || []).length > 0],
  ['summary', 'Summary', () => true],
];

// What the import is doing, in words, as the server reports its phase.
const IMPORT_PHASES = { scan: 'Reading the backup…', validate: 'Checking your choices…', copy: 'Copying files…', commit: 'Writing your history to v2 (this can take a minute)…', done: 'Done.' };

// Said once as a warning on the first page and again, in the same words, when skipping is confirmed.
const ONLY_CHANCE = 'This is the only chance to import v1 history. Skipping closes the import permanently, and the original backup stays on disk.';

const NEW_PROJECT = '__new_project__';
const NEW_ACCOUNT = '__new_account__';

function draftKey(sourceKey) { return `aimebu2.import-draft.${sourceKey}`; }

// What choosing "Skip person" costs, said next to the choice: their messages keep their name as a label, but what belongs to the person's
// account is not imported at all, and an import cannot be repeated. `stakes` counts it (see human_stakes in the preview).
export function skippedPersonWarning(stakes) {
  const plural = (count, one, many = `${one}s`) => `${count} ${count === 1 ? one : many}`;
  const lost = [];
  if (stakes?.memory_profiles) lost.push(plural(stakes.memory_profiles, 'memory profile'));
  if (stakes?.reactions) lost.push(plural(stakes.reactions, 'reaction'));
  if (stakes?.read_positions) lost.push(plural(stakes.read_positions, 'read position'));
  if (stakes?.room_view) lost.push(plural(stakes.room_view, 'pinned or hidden room'));
  return 'No account: their messages keep their name as a label, and nobody can sign in as them.'
    + (lost.length ? ` Their ${lost.join(', ')} would not be imported, and an import cannot be repeated.` : '')
    + ' Choose an account, or "Create a new account…", to keep them.';
}
export class ImportV1Wizard {
  constructor(currentSession = () => () => true) {
    this.currentSession = currentSession;
    this.dialog = node('dialog', '', 'import-v1-dialog');
    this.heading = node('h2', 'Import v1 history');
    this.progress = node('div', '', 'import-progress'); this.progress.hidden = true;
    this.progressBar = node('div', '', 'import-progress-bar'); this.progressFill = node('span');
    this.progressBar.setAttribute('role', 'progressbar'); this.progressBar.append(this.progressFill);
    this.progressLabel = node('p', '', 'import-progress-label');
    this.progress.append(this.progressLabel, this.progressBar);
    this.status = node('p', '', 'settings-intro');
    this.error = node('div', '', 'error'); this.error.setAttribute('role', 'alert'); // a div: it can hold the list of problems
    this.content = node('div', '', 'import-v1-content');
    this.totals = node('p', '', 'import-totals'); this.totals.hidden = true; this.totals.setAttribute('role', 'status');
    this.actions = node('div', '', 'dialog-actions');
    this.skipZone = node('div', '', 'import-skip-zone');
    this.dialog.append(this.heading, this.progress, this.status, this.error, this.content, this.totals, this.actions, this.skipZone);
    this.dialog.addEventListener('cancel', event => event.preventDefault());
    document.body.append(this.dialog);
    this.phase = 'review';
    this.preview = null;
    this.mapping = null;
    this.stepID = 'projects';
    this.newProject = null;
  }

  async maybeOpen(signal, isCurrent = () => true) {
    let status;
    try { status = await request('/api/import-v1/status', { signal }); }
    catch (error) {
      if (error.status === 403) return false;
      // The backup was modified after it was made, so the status itself refuses (409): show why, with the Skip choice,
      // instead of leaving the owner with no wizard and no explanation.
      if (error.status === 409 && error.code === 'backup_changed') return this.showBackupChanged(error.message, signal, isCurrent);
      throw error;
    }
    if (signal?.aborted || !isCurrent()) return false;
    if (status.state === 'ready' || status.state === 'resume') { return this.open(signal, isCurrent); }
    return false;
  }

  showBackupChanged(message, signal, isCurrent = this.currentSession()) {
    if (signal?.aborted || !isCurrent()) return false;
    this.preview = null; this.mapping = null;
    this.changedMessage = message;
    this.phase = 'changed';
    this.render();
    if (!this.dialog.open) this.dialog.showModal();
    return true;
  }

  async open(signal, isCurrent = this.currentSession()) {
    this.error.textContent = '';
    let preview;
    try { preview = await request('/api/import-v1/preview', { signal }); }
    catch (error) {
      if (error.status === 409 && error.code === 'backup_changed') return this.showBackupChanged(error.message, signal, isCurrent);
      throw error;
    }
    if (signal?.aborted || !isCurrent()) return false;
    const destinationMacros = await request('/api/macros', { signal });
    if (signal?.aborted || !isCurrent()) return false;
    this.preview = preview;
    this.destinationMacros = destinationMacros;
    this.mapping = structuredClone(this.preview.suggested);
    for (const key of ['macro_conflicts', 'role_conflicts', 'project_members', 'memory_projects', 'enable_project_memory', 'rooms', 'projects', 'humans', 'new_users']) this.mapping[key] ||= {};
    this.mapping.skip_macros ||= [];
    this.defaultPeople(this.mapping);
    this.steps = STEPS.filter(([, , applies]) => applies(preview)).map(([id, title]) => ({ id, title }));
    this.stepID = this.steps[0].id;
    this.restored = this.restoreDraft();
    this.phase = 'review';
    this.render();
    if (signal?.aborted || !isCurrent()) return false;
    if (!this.dialog.open) this.dialog.showModal();
    // A reload on the summary step restores the step but not the dry run it shows; without this it sat on "Checking…" for good.
    if (this.stepID === 'summary') this.check();
    return true;
  }

  // The owner's choices and place are kept in the browser, per backup, so Back and Next lose nothing and neither does a reload.
  // What the draft lacks (a room the preview has but the draft does not) comes from the suggestion.
  restoreDraft() {
    let draft;
    try { draft = JSON.parse(localStorage.getItem(draftKey(this.preview.source_key)) || 'null'); } catch { draft = null; }
    if (!draft?.mapping || draft.mapping.source_key !== this.mapping.source_key || draft.mapping.owner_login !== this.mapping.owner_login) return false;
    const mapping = draft.mapping;
    for (const [id, spec] of Object.entries(this.mapping.rooms)) mapping.rooms[id] ||= spec;
    if (this.mapping.rooms[SYSTEM_ROOM]) mapping.rooms[SYSTEM_ROOM] = this.mapping.rooms[SYSTEM_ROOM]; // never imported, whatever an old draft says
    for (const key of ['macro_conflicts', 'role_conflicts', 'project_members', 'memory_projects', 'enable_project_memory', 'humans', 'projects', 'new_users']) mapping[key] ||= {};
    mapping.skip_macros ||= [];
    // A person the draft has no choice for gets the suggestion (and its default); a person it does have a choice for keeps it, a skip included.
    const undecided = new Set((this.preview.humans || []).filter(human => mapping.humans[human] === undefined));
    for (const human of undecided) {
      mapping.humans[human] = this.preview.suggested.humans?.[human] ?? '-';
      const account = this.preview.suggested.new_users?.[human];
      if (account) mapping.new_users[human] = { ...account }; // the suggested login is a new account, not a link to an existing one
    }
    this.defaultPeople(mapping, human => undecided.has(human));
    // A choice for something the preview no longer offers (a macro that is now left out, a room or person the backup no longer has) is dropped: the import
    // would refuse it, and the owner could not tell why. The first page shown says so once.
    const macros = new Set(Object.keys(this.preview.macros || {})), rooms = new Set((this.preview.rooms || []).map(room => room.id)), humans = new Set(this.preview.humans || []);
    let dropped = 0;
    const keepOffered = (choices, offered) => { for (const key of Object.keys(choices)) if (!offered.has(key)) { delete choices[key]; dropped++; } };
    keepOffered(mapping.macro_conflicts, macros); keepOffered(mapping.rooms, rooms); keepOffered(mapping.humans, humans); keepOffered(mapping.new_users, humans);
    const skipped = mapping.skip_macros.filter(key => macros.has(key)); dropped += mapping.skip_macros.length - skipped.length; mapping.skip_macros = skipped;
    this.mapping = mapping;
    if (this.steps.some(step => step.id === draft.step)) this.stepID = draft.step;
    this.staleNote = dropped > 0 ? { step: this.stepID } : null;
    return true;
  }

  save() {
    if (!this.mapping || !this.preview) return;
    try { localStorage.setItem(draftKey(this.preview.source_key), JSON.stringify({ step: this.stepID, mapping: this.mapping })); } catch {}
  }

  clearDraft() { try { localStorage.removeItem(draftKey(this.preview.source_key)); } catch {} }

  button(label, action, secondary = false) {
    const button = node('button', label, secondary ? 'btn' : 'btn btn-primary');
    button.type = 'button'; button.onclick = () => Promise.resolve(action()).catch(error => { this.error.textContent = error.message; });
    return button;
  }

  // A labelled text box or select whose change is saved with the draft.
  field(label, value, update, options, onEnter = () => this.next()) {
    const row = node('label', label);
    let input;
    if (options) {
      input = node('select');
      for (const [key, title] of options) { const option = node('option', title); option.value = key; input.append(option); }
    } else {
      input = node('input'); input.type = 'text';
    }
    input.value = value ?? '';
    input.onchange = () => { update(input.value); this.save(); };
    if (!options) {
      // A typed value counts at once (not only after the field loses focus), and Enter in a text field goes on to the next page.
      input.oninput = () => { update(input.value); this.save(); };
      input.onkeydown = event => { if (event.key === 'Enter' && !event.isComposing) { event.preventDefault(); update(input.value); this.save(); onEnter(); } };
    }
    row.append(input);
    return row;
  }

  checkbox(label, checked, update) {
    const row = node('label', label); const box = node('input'); box.type = 'checkbox'; box.checked = !!checked;
    box.onchange = () => { update(box.checked); this.save(); this.refreshTotals(); };
    row.prepend(box);
    return row;
  }

  get stepIndex() { return this.steps.findIndex(step => step.id === this.stepID); }

  // Re-draw the current page without losing the scroll position of a long list.
  refresh() {
    const top = this.content.scrollTop, focus = this.focusTarget(document.activeElement);
    this.render();
    this.content.scrollTop = top;
    if (focus) (this.content.querySelector(focus.exact) || (focus.fallback && this.content.querySelector(focus.fallback)))?.focus({ preventScroll: true });
  }

  // Where focus was before a redraw, so the keyboard user lands on the same thing after it: a room's select, its archive checkbox or a bulk
  // button.
  focusTarget(element) {
    if (!element || !this.content.contains(element)) return null;
    const quote = value => CSS.escape(value);
    if (element.dataset.importInto) return { exact: `select[data-import-into="${quote(element.dataset.importInto)}"]` };
    if (element.dataset.archived) return { exact: `input[data-archived="${quote(element.dataset.archived)}"]` };
    const bulk = element.closest('.import-bulk');
    if (bulk && element.matches('button')) return { exact: `.import-bulk button:nth-child(${[...bulk.children].indexOf(element) + 1})` };
    return null;
  }

  // Where a v1 room goes, in words, for the Summary: its place in v2, and whether it is archived.
  // "(archived)" is the merged target's state (open when any room in it is open), not the room's own flag.
  roomDestination(room, tree) {
    const spec = this.mapping.rooms[room.id] || {};
    if (room.dm) return spec.skip || !spec.project ? 'not imported' : `direct message in ${this.mapping.projects[spec.project]?.name || spec.project}`;
    const value = roomChoice(spec, room.id);
    if (value === 'skip') return 'not imported';
    const { kind, alias } = parseChoice(value);
    const project = this.mapping.projects[alias]?.name || alias;
    if (kind === 'root') return `⌂ ${project}`;
    const sub = tree.projects.find(item => item.alias === alias)?.subs.find(item => item.rooms.includes(room.id));
    // Only a named sub-room can merge rooms; a room kept as itself is archived by its own flag.
    const archived = spec.target === 'sub' && sub ? sub.archived : spec.lifecycle !== 'open';
    return `${project} › ${sub?.title || (kind === 'general' ? 'general' : spec.title || room.id)}${archived ? ' (archived)' : ''}`;
  }

  render() {
    this.error.textContent = '';
    this.actions.replaceChildren(); this.content.replaceChildren(); this.skipZone.replaceChildren();
    this.progress.hidden = this.phase !== 'review';
    this.dialog.dataset.phase = this.phase; // the review pages share one height, so the dialog does not jump between steps
    this.totals.hidden = true; this.totals.textContent = '';
    if (this.phase === 'changed') this.renderChanged();
    else if (this.phase === 'review') {
      this.renderStep();
      // The note about dropped choices stays on the page the draft was restored to, and goes when the owner moves on.
      if (this.staleNote && this.staleNote.step === this.stepID) this.content.prepend(node('p', 'Some earlier choices no longer apply.', 'import-restored import-stale-note'));
      else this.staleNote = null;
    }
    else this.renderImport();
  }

  // The backup can no longer be verified, so it cannot be imported as it is. Skipping closes the import for good;
  // closing the dialog leaves it open, so the owner can restore a pristine copy and the wizard offers it again.
  renderChanged() {
    this.status.textContent = this.changedMessage || 'The v1 backup was modified after it was made, so it can no longer be verified and will not be imported.';
    this.actions.append(this.button('Decide later', () => this.dialog.close(), true));
    this.offerSkip();
  }

  // Skipping is irreversible, so it lives apart from the forward buttons, behind a confirmation, on the first page only.
  offerSkip() {
    this.skipZone.append(node('span', 'Not going to import?', 'import-skip-note'), this.button('Skip the import…', () => this.confirmSkip(), true));
    this.skipZone.lastChild.classList.add('import-skip-open');
  }

  renderStep() {
    const p = this.preview;
    if (p.reservation_missed) {
      // Something already took message numbers inside the v1 range, so the history cannot keep its numbers.
      this.status.textContent = 'This install can no longer import v1 history: part of the v1 message-number range is already in use here, and imported messages must keep their v1 numbers. Skipping closes the import permanently; the original backup stays on disk.';
      this.progress.hidden = true;
      this.offerSkip();
      return;
    }
    const index = this.stepIndex, step = this.steps[index];
    this.progressLabel.textContent = `Step ${index + 1} of ${this.steps.length} · ${step.title}`;
    this.progressBar.setAttribute('aria-valuemin', '1'); this.progressBar.setAttribute('aria-valuemax', String(this.steps.length)); this.progressBar.setAttribute('aria-valuenow', String(index + 1));
    this.progressBar.setAttribute('aria-label', this.progressLabel.textContent);
    this.progressFill.style.width = `${Math.round(((index + 1) / this.steps.length) * 100)}%`;
    this.status.textContent = '';
    if (index > 0) this.actions.append(this.button('Back', () => this.go(this.steps[index - 1].id), true));
    if (this.newProject && step.id !== 'projects') this.content.append(this.renderNewProject()); // the projects page puts it above its own button
    this[`page_${step.id}`]();
    this.refreshTotals();
    if (step.id !== 'summary') this.actions.append(this.button('Next', () => this.next()));
    this.actions.append(this.button('Start over', () => this.startOver(), true));
    if (index === 0) {
      this.offerSkip();
    }
  }

  async startOver() {
    this.clearDraft();
    this.report = null; this.digest = null; this.newProject = null; this.staleNote = null;
    await this.open();
  }

  async go(id) {
    this.stepID = id; this.newProject = null; this.save();
    if (id === 'summary') this.report = null;
    this.render();
    this.content.scrollTop = 0;
    if (id === 'summary') await this.check();
  }

  // Next stops while anything on the page cannot be imported as it stands, and lists all of it at once: a link to each field and a message
  // beside each, so the owner fixes them together instead of finding them one Next at a time.
  async next() {
    this.error.textContent = '';
    const problems = this[`problem_${this.stepID}`]?.() || [];
    if (problems.length) { this.showProblems(problems); return; }
    await this.go(this.steps[this.stepIndex + 1].id);
  }

  showProblems(problems) {
    for (const old of this.content.querySelectorAll('[aria-invalid]')) old.removeAttribute('aria-invalid');
    for (const old of this.content.querySelectorAll('[aria-describedby^="import-problem-"]')) old.removeAttribute('aria-describedby');
    for (const old of this.content.querySelectorAll('.import-field-error')) old.remove();
    const list = node('ul', '', 'import-problems');
    problems.forEach((problem, index) => {
      const item = node('li'); const link = node('button', 'Go to the field', 'btn btn-link'); link.type = 'button';
      link.onclick = () => this.focusProblem(problem);
      item.append(node('span', `${problem.message} `), link); list.append(item);
      // The same message sits at the field, tied to it for a screen reader, and the field is marked.
      const field = this.content.querySelector(problem.selector);
      if (!field) return;
      const message = node('small', problem.message, 'import-field-error'); message.id = `import-problem-${index}`;
      if (field.matches('input, select, textarea')) { field.setAttribute('aria-invalid', 'true'); field.setAttribute('aria-describedby', message.id); (field.closest('label') || field).after(message); }
      else field.after(message);
    });
    this.error.replaceChildren(node('p', problemSummary(problems.length), 'import-problems-summary'), list);
    this.focusProblem(problems[0]);
  }

  focusProblem(problem) {
    const input = this.content.querySelector(problem.selector);
    if (!input) return;
    input.closest('details')?.setAttribute('open', '');
    input.scrollIntoView({ block: 'center' }); input.focus();
  }

  // ---- shared pieces ----

  projectOptions() { return Object.entries(this.mapping.projects).map(([alias, project]) => [alias, project.name || alias]); }

  // "+ New project…" opens a small form in place; the project it makes is handed to `then`, so nothing sends the owner back a page.
  askNewProject(then) { this.newProject = { then }; this.refresh(); }

  renderNewProject() {
    const box = node('fieldset', '', 'import-new-project'); box.append(node('legend', 'New project'));
    const create = () => make.click(); // Enter in either field makes the project; it must not skip to the next page
    const name = this.field('Name', '', () => {}, undefined, create); const folder = this.field('Workspace folder (absolute path)', '', () => {}, undefined, create);
    const nameInput = name.querySelector('input'), folderInput = folder.querySelector('input');
    nameInput.dataset.newProjectName = ''; folderInput.dataset.newProjectFolder = ''; folderInput.placeholder = '/path/to/the/project';
    attachFolderField(folderInput, () => rights.hostHome);
    const message = node('p', '', 'error'); message.setAttribute('role', 'alert');
    const make = this.button('Create project', () => {
      const title = nameInput.value.trim();
      if (!title) { message.textContent = 'Give the project a name.'; nameInput.focus(); return; }
      if (!folderInput.value.startsWith('/')) { message.textContent = 'The folder must be an absolute path such as /Users/you/code/project.'; folderInput.focus(); return; }
      const alias = newProjectAlias(title, new Set(Object.keys(this.mapping.projects)));
      this.mapping.projects[alias] = { name: title, folder: folderInput.value };
      this.mapping.enable_project_memory[alias] = !!this.preview.v1_memory_enabled;
      const then = this.newProject.then; this.newProject = null;
      this.justAdded = alias; then?.(alias); this.save(); this.refresh();
    });
    const cancel = this.button('Cancel', () => { this.newProject = null; this.refresh(); }, true);
    const actions = node('div', '', 'dialog-actions'); actions.append(make, cancel);
    box.append(name, folder, message, actions);
    queueMicrotask(() => nameInput.focus());
    return box;
  }

  // A select of the projects with "+ New project…" at the end. `none` adds a first choice that means no project.
  projectSelect(label, value, update, none) {
    const options = [...(none ? [['', none]] : []), ...this.projectOptions(), [NEW_PROJECT, '+ New project…']];
    const row = this.field(label, value, () => {}, options);
    const select = row.querySelector('select');
    select.onchange = () => {
      if (select.value === NEW_PROJECT) { select.value = value ?? ''; this.askNewProject(alias => update(alias)); return; }
      update(select.value); this.save(); this.refresh();
    };
    return row;
  }

  // ---- pages ----

  page_projects() {
    const p = this.preview, counts = p.counts || {};
    this.status.textContent = `The backup holds ${counts.rooms || 0} rooms, ${counts.messages || 0} messages, ${counts.attachments || 0} attachments and ${counts.memory || 0} memory records. First, the projects the history goes into: each needs a name and an absolute workspace folder.`;
    this.content.append(node('p', ONLY_CHANCE, 'import-warning import-only-chance'));
    if (this.restored) this.content.append(node('p', 'Continuing where you left off.', 'import-restored'));
    for (const [alias, project] of Object.entries(this.mapping.projects)) {
      const box = node('fieldset'); box.append(node('legend', alias));
      const name = this.field('Name', project.name, value => { project.name = value; });
      name.querySelector('input').dataset.projectName = alias; box.append(name);
      const folder = this.field('Workspace folder (absolute path)', project.folder, value => { project.folder = value; });
      folder.querySelector('input').dataset.projectFolder = alias; folder.querySelector('input').placeholder = '/path/to/the/project';
      attachFolderField(folder.querySelector('input'), () => rights.hostHome);
      box.append(folder);
      // v1 did not record where this project's agents worked (they are gone from its registry) and no folder of that name was found beside the
      // others, so the owner says. Any existing folder will do for now: it can be changed in the project's settings until its first agent starts.
      if (!project.folder) box.append(node('p', 'v1 did not record a folder for this project. Enter the folder its agents will work in; you can change it in the project\'s settings until its first agent starts. Or remove the project: its rooms are then not imported.', 'settings-intro import-folder-hint'));
      for (const login of p.destination_users || []) {
        if (login === this.mapping.owner_login) continue;
        box.append(this.checkbox(`Grant ${login} project viewer access`, (this.mapping.project_members[alias] || []).includes(login), checked => {
          const members = new Set(this.mapping.project_members[alias] || []); if (checked) members.add(login); else members.delete(login);
          this.mapping.project_members[alias] = [...members];
        }));
      }
      box.append(this.button('Remove project', () => this.removeProject(alias), true));
      this.content.append(box);
      if (this.justAdded === alias) { this.justAdded = null; queueMicrotask(() => { box.scrollIntoView({ block: 'nearest' }); name.querySelector('input').focus({ preventScroll: true }); }); }
    }
    this.justAdded = null;
    if (!Object.keys(this.mapping.projects).length) this.content.append(node('p', 'No project yet: everything that needs one will be left out. Add a project to bring rooms across.', 'settings-intro'));
    if (this.newProject) {
      const form = this.renderNewProject(); this.content.append(form);
      queueMicrotask(() => form.scrollIntoView({ block: 'nearest' }));
    }
    this.content.append(this.button('Add destination project', () => this.askNewProject(() => {}), true));
  }

  // A removed project takes nothing with it: its rooms and direct messages become "don't import" and its memory scopes are skipped.
  removeProject(alias) {
    delete this.mapping.projects[alias]; delete this.mapping.project_members[alias]; delete this.mapping.enable_project_memory[alias];
    for (const spec of Object.values(this.mapping.rooms)) if (spec.project === alias) { spec.skip = true; spec.project = ''; delete spec.target; delete spec.title; }
    for (const [scope, target] of Object.entries(this.mapping.memory_projects)) if (target === alias) this.mapping.memory_projects[scope] = '-';
    if (this.mapping.default_project === alias) this.mapping.default_project = '';
    this.save(); this.refresh();
  }

  problem_projects() { return projectProblems(this.mapping.projects); }

  page_people() {
    const p = this.preview;
    this.mapping.new_users ||= {};
    this.status.textContent = 'Each v1 person\'s messages show under the account you map them to, so people sharing one destination account are merged, in access and in how their old messages are labelled. A person can get a new account made by the import (no password: you get a one-time link for each at the end). A skipped person has no account: their messages keep their v1 name as a label, and they receive no live room membership.';
    const people = [['-', 'Skip person (legacy label only)'], ...(p.destination_users || [this.mapping.owner_login]).map(login => [login, login === this.mapping.owner_login ? `${login} (owner)` : login]), [NEW_ACCOUNT, 'Create a new account…']];
    const owner = this.mapping.owner_login;
    const noSuggestedOwner = !Object.values(p.suggested?.humans || {}).includes(owner);
    const noMappedOwner = !(p.humans || []).some(human => this.mapping.humans[human] === owner);
    if ((p.humans || []).length > 1 && noSuggestedOwner && noMappedOwner) {
      this.content.append(node('p', `None of these people is mapped to your account (${owner}): the backup has no one with your name, and more than one person. Choose which one is you to carry that person's read positions, memory profile, pins and reactions into ${owner}. New accounts keep their own importable data; skipping a person drops theirs.`, 'import-warning people-owner-hint'));
    }
    for (const human of p.humans || []) {
      const created = this.mapping.new_users[human];
      const row = this.field(human, created ? NEW_ACCOUNT : this.mapping.humans[human], () => {}, people);
      row.querySelector('select').dataset.person = human;
      row.querySelector('select').onchange = event => {
        const value = event.target.value;
        if (value === NEW_ACCOUNT) {
          const login = this.newLogin(human);
          this.mapping.new_users[human] = { login, role: 'viewer' }; this.mapping.humans[human] = login;
        } else { delete this.mapping.new_users[human]; this.mapping.humans[human] = value; }
        this.save(); this.refresh();
      };
      this.content.append(row);
      if (!created && this.mapping.humans[human] === '-') this.content.append(node('p', skippedPersonWarning(this.preview.human_stakes?.[human]), 'import-warning'));
      if (created) {
        const box = node('div', '', 'import-new-account');
        const login = this.field('Login for the new account', created.login, value => { created.login = value.trim(); this.mapping.humans[human] = created.login; });
        login.querySelector('input').dataset.newLogin = human;
        login.querySelector('input').onfocus = event => event.target.select(); // typing replaces the suggested login instead of adding to it
        const role = this.field('Role', created.role, value => { created.role = value; }, [['viewer', 'Viewer: reads the project\'s rooms'], ['operator', 'Operator: also works with agents']]);
        role.querySelector('select').dataset.newRole = human;
        box.append(login, role, node('p', 'The role applies to every project this person\'s rooms are imported into. The account has no password until its owner follows the one-time link shown after the import.', 'settings-intro'));
        this.content.append(box);
      }
    }
  }

  // Someone who is not the owner starts as a new account instead of being merged into the owner's (they can switch to a skip or an existing
  // account on the People page). Applied only where nobody has decided: a fresh suggestion, and a person a saved draft has no choice for.
  defaultPeople(mapping, eligible = () => true) {
    mapping.new_users ||= {};
    for (const human of this.preview.humans || []) {
      if (!eligible(human) || mapping.humans[human] !== '-' || mapping.new_users[human]) continue;
      const login = this.newLogin(human, mapping);
      mapping.new_users[human] = { login, role: 'viewer' }; mapping.humans[human] = login;
    }
  }

  // A login for a new account from a v1 name: lowercase, 3 to 32 characters, not one already in use here or chosen for someone else.
  newLogin(name, mapping = this.mapping) {
    const taken = new Set([...(this.preview.destination_users || []), ...Object.values(mapping.new_users || {}).map(user => user.login)].map(login => String(login).toLowerCase()));
    let base = name.toLowerCase().replace(/[^a-z0-9_-]+/g, '-').replace(/^[^a-z]+/, '').slice(0, 28);
    if (base.length < 3) base = `user-${base}`.replace(/-$/, '');
    if (base.length < 3) base = 'user';
    let login = base, n = 2;
    while (taken.has(login.toLowerCase())) login = `${base}-${n++}`;
    return login;
  }

  problem_people() { return peopleProblems(this.mapping.new_users, this.preview.destination_users); }

  page_rooms() {
    const p = this.preview;
    const shown = (p.rooms || []).filter(room => room.id !== SYSTEM_ROOM);
    const rooms = shown.filter(room => !room.dm);
    const tree = planTree(this.mapping, shown);
    const kept = rooms.filter(room => !this.mapping.rooms[room.id]?.skip && this.mapping.rooms[room.id]?.project).length;
    this.status.textContent = `${kept} of ${rooms.length} rooms will be imported. Every room starts open; archive individual rooms or use Archive all if you want. Rooms sent to the same place merge, their messages interleaved by their v1 numbers.`;
    const layout = node('div', '', 'import-rooms'); const list = node('div', '', 'import-room-list'), aside = node('aside', '', 'import-room-tree');
    aside.setAttribute('aria-label', 'Rooms in v2');
    for (const group of mergeGroups(tree)) { const warning = node('p', `${group.rooms.length} v1 rooms will be merged into ${group.where}: ${group.rooms.join(', ')}.`, 'import-warning'); warning.setAttribute('role', 'note'); list.append(warning); }
    const bulk = node('div', '', 'import-bulk');
    for (const [label, lifecycle] of [['Open all', 'open'], ['Archive all', 'closed']]) bulk.append(this.button(label, () => {
      for (const room of rooms) { const spec = this.mapping.rooms[room.id]; if (spec && !spec.skip) spec.lifecycle = lifecycle; }
      this.save(); this.refresh();
    }, true));
    list.append(bulk);
    for (const room of rooms) list.append(this.roomRow(room));
    this.drawTree(aside, tree);
    layout.append(list, aside);
    this.content.append(layout);
    if ((p.rooms || []).some(room => room.id === SYSTEM_ROOM)) this.content.append(node('p', 'v1 system events are not imported.', 'settings-intro'));
  }

  // One v1 room: "Import into" (a project's root, its general room, a new sub-room named like the room, or nowhere) and whether it is archived.
  roomRow(room) {
    const spec = this.mapping.rooms[room.id] ||= { skip: true };
    const row = node('div', '', 'import-room-row'); row.dataset.room = room.id;
    row.append(node('strong', room.id), node('span', `${room.messages} messages`, 'import-room-count'));
    const current = roomChoice(spec, room.id);
    const select = node('select'); select.dataset.importInto = room.id; select.setAttribute('aria-label', `Import ${room.id} into`);
    const add = (parent, value, title) => { const option = node('option', title); option.value = value; parent.append(option); };
    add(select, 'skip', "Don't import");
    for (const [alias, project] of this.projectOptions()) {
      const group = node('optgroup'); group.label = project;
      add(group, choiceValue('root', alias), `⌂ ${project}`); add(group, choiceValue('general', alias), `${project} › general`); add(group, choiceValue('new', alias), `${project} › new sub-room "${room.id}"`);
      select.append(group);
    }
    if (parseChoice(current).kind === 'custom') { const alias = parseChoice(current).alias; add(select, current, `${this.mapping.projects[alias]?.name || alias} › "${spec.title}"`); }
    add(select, NEW_PROJECT, '+ New project…');
    select.value = current;
    select.onchange = () => {
      if (select.value === NEW_PROJECT) { select.value = current; this.askNewProject(alias => { applyChoice(spec, choiceValue('general', alias), room.id); }); return; }
      applyChoice(spec, select.value, room.id); this.save(); this.refresh();
    };
    row.append(select);
    if (!spec.skip && spec.project && spec.target !== 'root') {
      const archived = this.checkbox('Archived after import', spec.lifecycle !== 'open', checked => { spec.lifecycle = checked ? 'closed' : 'open'; });
      archived.querySelector('input').dataset.archived = room.id;
      archived.querySelector('input').onchange = event => { spec.lifecycle = event.target.checked ? 'closed' : 'open'; this.save(); this.refresh(); };
      row.append(archived);
    } else if (!spec.skip && spec.project && spec.target === 'root') row.append(node('p', "A project's home room is never archived.", 'import-count-note'));
    if (room.memory_disabled && !spec.skip && spec.project) row.append(node('p', `v1 had memory off for this room, so v2 will have memory off for ${spec.target === 'root' ? `⌂ ${this.mapping.projects[spec.project]?.name || spec.project}` : `its room "${spec.target === 'sub' ? (spec.title || 'general') : room.id}"`}${spec.target ? ', and for any other v1 room merged into it' : ''}.`, 'settings-intro'));
    return row;
  }

  // The v2 rooms this mapping makes, as a tree with message counts; what is left out is listed under it.
  drawTree(aside, tree) {
    aside.append(node('h4', 'In v2'));
    const list = node('ul', '', 'import-tree');
    for (const project of tree.projects) {
      const item = node('li'); item.append(node('strong', `⌂ ${project.name}`), node('span', project.root.rooms.length ? ` · ${project.root.messages} messages${project.root.memoryOff.length ? ` · memory off (v1 room ${project.root.memoryOff.join(', ')} had it off)` : ''}` : '', 'import-room-count'));
      if (project.subs.length) {
        const children = node('ul');
        for (const sub of project.subs) children.append(node('li', `${sub.title} · ${sub.messages} messages${sub.archived ? ' · archived' : ''}${sub.memoryOff.length ? ` · memory off (v1 room ${sub.memoryOff.join(', ')} had it off)` : ''}`));
        item.append(children);
      }
      list.append(item);
    }
    if (tree.direct.length) list.append(node('li', `Direct messages · ${tree.direct.length} chats`));
    if (!list.children.length) list.append(node('li', 'Nothing will be imported yet.'));
    aside.append(list);
    if (tree.dropped.length) {
      const dropped = node('div', '', 'import-dropped'); dropped.append(node('h4', `Not imported (${tree.dropped.length})`));
      dropped.append(node('p', tree.dropped.map(room => room.id).join(', '), 'import-room-count'));
      aside.append(dropped);
    }
  }

  page_dms() {
    const p = this.preview;
    const direct = (p.rooms || []).filter(room => room.dm);
    const picked = direct.filter(room => !this.mapping.rooms[room.id]?.skip).length;
    const total = direct.length;
    const count = total === 1 ? `The chat is${picked ? '' : ' not'}` : picked === 0 ? `None of the ${total} chats are` : picked === total ? `All ${total} chats are` : `${picked} of ${total} chats are`;
    this.status.textContent = `${count} going to be imported. Each private chat becomes a v2 direct message titled by the other person or agent, always open. A chat between two agents is imported as history you can read. No chat is imported unless you tick it; a ticked chat goes into its agent's project when there is one, and you can pick another.`;
    for (const room of direct) {
      const spec = this.mapping.rooms[room.id] ||= { skip: true };
      const box = node('fieldset'); const legend = node('legend', this.dmTitle(room)); legend.dataset.dmRoom = room.id; box.append(legend);
      // Titles can collide (two chats with the same agent): then the v1 id tells them apart; otherwise it stays in the tooltip.
      const same = direct.filter(other => this.dmTitle(other) === this.dmTitle(room)).length > 1;
      const size = node('p', `${room.messages} ${room.messages === 1 ? 'message' : 'messages'}${same ? ` · ${room.id}` : ''}`, 'import-source-id'); size.title = `v1 chat ${room.id}`;
      box.append(size);
      const toggle = this.checkbox('Import this chat', !spec.skip, checked => { spec.skip = !checked; });
      toggle.querySelector('input').onchange = event => { spec.skip = !event.target.checked; this.save(); this.refresh(); };
      box.append(toggle);
      if (!spec.skip) { const choice = this.projectSelect('Project', spec.project || '', alias => { spec.project = alias; }, 'Choose a project'); choice.querySelector('select').dataset.dmProject = room.id; box.append(choice); }
      this.content.append(box);
    }
  }

  // The one-line total beside the buttons, redrawn whenever a choice that feeds it changes.
  refreshTotals() {
    const totals = this.stepID ? this.footerTotals(this.stepID) : '';
    this.totals.textContent = totals; this.totals.hidden = !totals;
  }

  // What the long pages add up to, kept in view beside the buttons: how much is selected and how much is left out.
  footerTotals(stepID) {
    const p = this.preview, m = this.mapping, n = (count, one, many = `${one}s`) => `${count.toLocaleString('en-US')} ${count === 1 ? one : many}`;
    if (stepID === 'projects') {
      const kept = Object.keys(m.projects).length, dropped = Object.keys(p.suggested?.projects || {}).filter(alias => !m.projects[alias]).length;
      return `${n(kept, 'project')} selected${dropped ? ` · ${n(dropped, 'project')} removed, their rooms are not imported` : ''}`;
    }
    if (stepID === 'rooms') {
      const rooms = (p.rooms || []).filter(room => !room.dm && room.id !== SYSTEM_ROOM);
      const left = rooms.filter(room => m.rooms[room.id]?.skip || !m.rooms[room.id]?.project);
      const messages = rooms.filter(room => !left.includes(room)).reduce((sum, room) => sum + (room.messages || 0), 0);
      return `${rooms.length - left.length} of ${n(rooms.length, 'room')} selected · ${n(messages, 'message')} go in${left.length ? ` · ${n(left.length, 'room')} not imported` : ''}`;
    }
    if (stepID === 'memory') {
      const c = p.memory_counts || {}, all = (p.counts?.memory || 0);
      let kept = 0;
      for (const [scope, count] of Object.entries(c.projects || {})) if ((m.memory_projects[scope] || '-') !== '-') kept += count;
      // A person's profile goes with the person: one mapped to nobody ("Skip person") is not imported, whatever the checkbox says.
      if (!m.skip_profiles) kept += Object.entries(c.profile_owners || {}).reduce((sum, [person, count]) => sum + (m.humans[person] === '-' ? 0 : count), 0);
      if (!m.skip_shared_notes) kept += c.shared || 0;
      return `${kept} of ${n(all, 'memory record')} selected${kept < all ? ` · ${n(all - kept, 'record')} not imported` : ''}`;
    }
    return '';
  }

  // A DM is titled by the participants other than the owner's own accounts (and skipped people): the agent's name, "a · b" for two or more.
  // It follows the current person mapping; the server applies the same rule when it imports.
  dmTitle(room) {
    const people = room.participants || [];
    const owner = this.mapping.owner_login;
    let others = people.filter(person => !(person.kind === 'user' && [owner, '-', '', undefined].includes(this.mapping.humans[person.id])));
    if (!others.length) others = people;
    return others.length ? others.map(person => person.name).join(' · ') : (room.title || room.id);
  }

  problem_dms() { return dmProblems(this.preview.rooms, this.mapping.rooms, room => this.dmTitle(room)); }

  page_memory() {
    const p = this.preview;
    this.status.textContent = `The backup holds ${p.counts?.memory || 0} memory records. Imported records arrive as ordinary, active ones.`;
    const counts = p.memory_counts || {};
    const records = n => `${n || 0} ${n === 1 ? 'record' : 'records'}`;
    for (const scope of p.memory_projects || []) this.content.append(this.projectSelect(`Project facts: ${scope} (${records(counts.projects?.[scope])})`, (this.mapping.memory_projects[scope] || '-') === '-' ? '' : this.mapping.memory_projects[scope], value => { this.mapping.memory_projects[scope] = value || '-'; }, "Don't import"));
    for (const [key, title] of [['skip_profiles', `Import global user profiles (${records(counts.profiles)})`], ['skip_shared_notes', `Import shared agent notes (${records(counts.shared)})`]]) {
      const row = this.checkbox(title, !this.mapping[key], checked => { this.mapping[key] = !checked; });
      row.querySelector('input').dataset.importChoice = key; row.querySelector('input').dataset.inverted = '1';
      this.content.append(row);
    }
    if (p.v1_memory_enabled) {
      this.content.append(node('h3', 'Project memory after import'));
      this.content.append(node('p', 'v1 global memory was on, so each project below starts with v2 memory on. Untick any project that should stay off.', 'settings-intro'));
      for (const [alias, project] of Object.entries(this.mapping.projects)) this.content.append(this.checkbox(`Enable project memory after import: ${project.name || alias}`, this.mapping.enable_project_memory[alias], checked => { this.mapping.enable_project_memory[alias] = checked; }));
    } else this.content.append(node('p', 'v1 memory was off or unset; destination project memory stays off.', 'settings-intro'));
  }

  // A macro's text: short ones in full, long ones folded behind their first line so a page of macros stays a page.
  macroBody(text) {
    if (!text) return node('pre', '(empty)', 'import-macro-body import-macro-empty');
    const lines = text.split('\n');
    if (text.length <= 300 && lines.length <= 6) return node('pre', text, 'import-macro-body');
    const details = node('details', '', 'import-macro-long');
    const first = lines.find(line => line.trim()) || '';
    details.append(node('summary', `${first.length > 80 ? `${first.slice(0, 80)}…` : first} (${text.length.toLocaleString('en-US')} characters)`), node('pre', text, 'import-macro-body'));
    return details;
  }

  page_macros() {
    const p = this.preview;
    const keys = Object.keys(p.macros || {}).sort();
    const skip = new Set(this.mapping.skip_macros);
    // The same buckets the Summary reports: new ones, ones that replace v2's text, ones that keep it, and ones already the same.
    const done = new Set(p.imported_macros || []);
    const tally = { new: 0, replace: 0, keep: 0, same: 0, imported: 0 };
    for (const key of keys.filter(key => !skip.has(key))) {
      const exists = Object.hasOwn(this.destinationMacros, key);
      if (done.has(key)) tally.imported++;
      else if (!exists) tally.new++;
      else if (this.destinationMacros[key] === p.macros[key]) tally.same++;
      else if ((this.mapping.macro_conflicts[key] || 'keep_v2') === 'use_v1') tally.replace++;
      else tally.keep++;
    }
    const parts = [[tally.new, 'new'], [tally.replace, 'replace the v2 text'], [tally.keep, 'keep the v2 text (a key both have)'], [tally.same, 'already match v2'], [tally.imported, 'already imported from this backup']].filter(([n]) => n).map(([n, text]) => `${n} ${text}`);
    this.status.textContent = `${keys.length - skip.size} of ${keys.length} macros chosen${parts.length ? `: ${parts.join(', ')}` : ''}.`;
    const leftOut = Object.entries(p.macros_left_out || {}).sort(([a], [b]) => a.localeCompare(b));
    if (leftOut.length) this.content.append(node('p', `Left out: ${leftOut.map(([key, why]) => `<${key}> (${why === 'deleted_in_v1' ? 'deleted in v1' : "v1's built-in example, unchanged; v2 has its own"})`).join(', ')}.`, 'settings-intro import-macros-left-out'));
    const bulk = node('div', '', 'import-bulk');
    bulk.append(this.button('Import all', () => { this.mapping.skip_macros = []; this.save(); this.refresh(); }, true), this.button('Import none', () => { this.mapping.skip_macros = [...keys]; this.save(); this.refresh(); }, true));
    this.content.append(bulk);
    // Each macro's state, said on its card: the ones that need a decision (a key both have with different text) come first.
    const stateOf = key => {
      if (done.has(key)) return ['imported', 'Already imported'];
      if (!Object.hasOwn(this.destinationMacros, key)) return ['new', 'New'];
      if (this.destinationMacros[key] === p.macros[key]) return ['same', 'Already in v2'];
      return (this.mapping.macro_conflicts[key] || 'keep_v2') === 'use_v1' ? ['conflict', 'Conflict: replacing the v2 text'] : ['conflict', 'Conflict: keeping the v2 text'];
    };
    const rank = key => ({ conflict: 0, new: 1, same: 2, imported: 3 })[stateOf(key)[0]];
    for (const key of [...keys].sort((a, b) => rank(a) - rank(b) || a.localeCompare(b))) {
      const body = p.macros[key];
      const [state, stateLabel] = stateOf(key);
      const box = node('fieldset'); const legend = node('legend', `<${key}>${(p.macro_defaults || []).includes(key) ? ' · v1 seeded default' : ''} `);
      const badge = node('span', stateLabel, `import-badge import-badge-${state}`); badge.dataset.state = state; legend.append(badge); box.append(legend);
      box.dataset.macro = key;
      const imported = (p.imported_macros || []).includes(key);
      if (imported) box.append(node('p', 'Already imported from this backup; later edits are preserved.'));
      else {
        const include = this.checkbox('Import this macro', !skip.has(key), checked => { const set = new Set(this.mapping.skip_macros); if (checked) set.delete(key); else set.add(key); this.mapping.skip_macros = [...set]; });
        include.querySelector('input').onchange = event => { const set = new Set(this.mapping.skip_macros); if (event.target.checked) set.delete(key); else set.add(key); this.mapping.skip_macros = [...set]; this.save(); this.refresh(); };
        box.append(include);
      }
      box.append(node('p', 'v1 text'), this.macroBody(body));
      const current = this.destinationMacros[key];
      if (Object.hasOwn(this.destinationMacros, key)) {
        box.append(node('p', current === body ? 'Already matches v2.' : 'Existing v2 text:'));
        if (current !== body) {
          box.append(this.macroBody(current));
          if (!imported) box.append(this.field('On conflict', this.mapping.macro_conflicts[key] || 'keep_v2', value => { this.mapping.macro_conflicts[key] = value; }, [['keep_v2', 'Keep v2 text'], ['use_v1', 'Use v1 text']]));
        }
      }
      this.content.append(box);
    }
  }

  page_roles() {
    const p = this.preview;
    this.status.textContent = 'Importable v1 roles become global v2 definitions. Existing v2 definitions win unless you choose the v1 version.';
    for (const role of p.roles || []) {
      const box = node('fieldset'); box.append(node('legend', `${role.emoji || ''} ${role.key}`.trim()));
      const imported = (p.imported_roles || []).includes(role.key);
      if (imported) box.append(node('p', 'Already imported from this backup; later edits are preserved.'));
      if (role.extends) box.append(node('p', `Extends ${role.extends}: it keeps this link, and the body below is only its own text.`));
      if (role.flattened_from) box.append(node('p', `Its parent ${role.flattened_from} is not available in v2, so it cannot keep the link.`));
      if (role.skipped_reason) box.append(node('p', `Skipped: ${role.skipped_reason}`));
      else {
        box.append(node('p', `${role.label} · ${role.cardinality} · v1 body:`));
        box.append(node('pre', role.body, 'import-macro-body'));
        if (role.conflict) {
          box.append(node('p', role.current_extends ? `Existing v2 body (extends ${role.current_extends}):` : 'Existing v2 body:'));
          box.append(node('pre', role.current_body || '', 'import-macro-body'));
          if (!imported) box.append(this.field('On conflict', this.mapping.role_conflicts[role.key] || 'keep_v2', value => { this.mapping.role_conflicts[role.key] = value; }, [['keep_v2', 'Keep v2 role'], ['use_v1', 'Use v1 role']]));
        }
      }
      this.content.append(box);
    }
  }

  // ---- summary: the dry run, in the owner's words ----

  async check() {
    this.error.textContent = '';
    this.report = null; this.status.textContent = 'Checking what would be imported…';
    this.content.replaceChildren(node('p', 'Checking…', 'import-checking'));
    const wanted = this.stepID;
    let result;
    try { result = await request('/api/import-v1/dry-run', { method: 'POST', body: this.mapping }); }
    catch (error) { if (this.stepID === wanted) { this.status.textContent = ''; this.content.replaceChildren(); this.error.textContent = error.message; } return; }
    if (this.stepID !== wanted) return;
    this.digest = result.digest; this.report = result.report;
    this.render();
  }

  page_summary() {
    if (!this.report) { this.status.textContent = this.status.textContent || 'Checking…'; return; }
    const report = this.report;
    this.status.textContent = 'Dry run complete. This is what the import will do; nothing has been written yet.';
    this.renderReportCounts(report);
    const left = node('div', '', 'import-left-out');
    const list = (title, items, note) => {
      if (!items.length) return;
      const box = section(title, `${items.length}`, true);
      if (note) box.append(node('p', note, 'settings-intro'));
      for (const item of items) box.append(node('p', item));
      left.append(box);
    };
    const placed = (this.preview.rooms || []).filter(room => room.id !== SYSTEM_ROOM);
    const tree = planTree(this.mapping, placed);
    list('Where each room goes', placed.map(room => `${room.dm ? this.dmTitle(room) : room.id} → ${this.roomDestination(room, tree)}${room.dm ? ` · ${room.id}` : ''}`), 'Check each placement before importing.');
    list('Rooms not imported: no project', report.skipped_no_project || [], 'These have no project to go into. Choose one on the Rooms page to bring them across.');
    list('Memory switched off', report.memory_off_targets || [], 'A room that several v1 rooms merge into has memory off when any one of them did.');
    list('Merged rooms', report.target_warnings || [], 'More than one v1 room lands in the same v2 room; their messages interleave by their v1 numbers.');
    const dms = (this.preview.rooms || []).filter(room => room.dm);
    const skippedDMs = dms.filter(room => this.mapping.rooms[room.id]?.skip);
    list('Accounts created', (report.new_accounts || []).map(account => `${account.login} (${account.role}) for ${account.v1_name}`), 'Each is created without a password. After the import you get a one-time link per account to hand over.');
    list('Access through direct messages', report.dm_only_access || [], 'Nothing else in the backup puts these people in the project. Untick the chat to leave them out.');
    list('Direct messages not imported', skippedDMs.map(room => `${this.dmTitle(room)} · ${room.id}`));
    list('Direct messages opened', report.opened_dms || [], 'v2 has no archive for direct messages, so these import open.');
    list('Memory skipped', (report.skipped_memory || []).map(entry => `${entry.scope} · ${entry.scope_key} · ${entry.id}: ${entry.reason}`));
    list('Roles skipped', Object.entries(report.skipped_roles || {}).sort(([a], [b]) => a.localeCompare(b)).map(([key, reason]) => `${key}: ${reason}`));
    list('Role conflicts', Object.entries(report.role_conflicts || {}).sort(([a], [b]) => a.localeCompare(b)).map(([key, choice]) => `${key}: ${choice === 'use_v1' ? 'use v1 role' : 'keep v2 role'}`));
    list('Macro conflicts', Object.entries(report.macro_conflicts || {}).sort(([a], [b]) => a.localeCompare(b)).map(([key, choice]) => `<${key}>: ${choice === 'use_v1' ? 'use v1 text' : 'keep v2 text'}`));
    list('Sounds skipped', Object.values(report.skipped_sounds || {}));
    list('Settings skipped', (report.skipped_settings || []).map(entry => `${entry.key}: ${entry.reason}`));
    const notCarried = { ...(report.not_carried || {}) };
    const notes = [...Object.entries(notCarried).sort(([a], [b]) => a.localeCompare(b)).map(([name, count]) => `${name}: ${count} rows`)];
    list('Not carried over', notes, 'These records or fields remain only in the v1 backup on disk.');
    const files = report.carried_over_files || [];
    list('Files kept for v2', files);
    this.content.append(left);
    const access = Object.entries(report.project_readers || {});
    if (access.length) {
      this.content.append(node('h3', 'Project access'));
      for (const [project, readers] of access) this.content.append(node('p', `${project}: ${readers} readers; ${report.project_access_gaps?.[project] || 0} access gaps; ${report.unreadable_dm_participants?.[project] || 0} DM access gaps.`));
    }
    this.content.append(node('p', 'Import pauses writes and agent dispatch until the database transaction commits. Keep this page open to watch progress.', 'settings-intro'));
    this.actions.append(this.button('Import now', () => this.commit()));
  }

  // The reconciled totals first: one headline line, then a block per count whose parts add up to the backup's own number, each skipped part
  // with its reason and, when there is something to look at, the rooms behind it (never message text).
  renderAccounting(report) {
    const groups = (report.accounting || []).filter(group => group.total > 0);
    if (!groups.length) return;
    const part = (group, key) => group.parts.find(item => item.key === key)?.count || 0;
    const messages = groups.find(group => group.key === 'messages'), rooms = groups.find(group => group.key === 'rooms');
    const headline = node('p', '', 'import-headline');
    const bits = [];
    if (messages) bits.push(`${number(part(messages, 'imported') + part(messages, 'already_present'))} of ${number(messages.total)} messages`);
    if (rooms) bits.push(`${number(part(rooms, 'imported') + part(rooms, 'already_present'))} of ${number(rooms.total)} rooms`);
    const attachments = groups.find(group => group.key === 'attachments');
    if (attachments) bits.push(`${number(part(attachments, 'imported') + part(attachments, 'already_present'))} of ${number(attachments.total)} attachments`);
    headline.textContent = `Bringing across ${bits.join(', ')}.`;
    const left = groups.reduce((sum, group) => sum + (group.key === 'messages' ? group.total - part(group, 'imported') - part(group, 'already_present') : 0), 0);
    this.content.append(headline);
    if (left > 0) this.content.append(node('p', `${number(left)} messages are left out; the lists below say why.`, 'import-warning'));
    const box = node('div', '', 'import-accounting');
    for (const group of groups) {
      const dropped = group.parts.filter(item => item.key !== 'imported' && item.key !== 'already_present' && item.count > 0);
      const details = section(`${ACCOUNT_GROUPS[group.key] || group.key}: ${number(group.total)} in v1`, dropped.length ? `${number(dropped.reduce((sum, item) => sum + item.count, 0))} left out` : 'all brought across', dropped.length > 0);
      details.dataset.key = group.key; details.classList.add('import-accounting-group');
      const table = node('table');
      for (const item of group.parts) {
        if (!(item.count > 0)) continue; // a part is only ever drawn as a positive count
        const label = (group.key === 'rooms' && ROOM_PARTS[item.key]) || ACCOUNT_PARTS[item.key] || item.key.replaceAll('_', ' ');
        const row = node('tr', '', item.key === 'imported' || item.key === 'already_present' ? '' : 'import-skipped-row');
        row.dataset.part = item.key;
        const name = node('td', label);
        if (item.items?.length) {
          const list = node('details', '', 'import-items'); list.append(node('summary', `Show ${item.items.length === 1 ? 'it' : `all ${number(item.items.length)}`}`));
          for (const line of item.items) list.append(node('p', line));
          name.append(list);
        }
        row.append(name, node('td', number(item.count)));
        table.append(row);
      }
      const total = node('tr', '', 'import-accounting-total'); total.append(node('td', `Total in the backup`), node('td', number(group.total)));
      table.append(total);
      details.append(table);
      box.append(details);
    }
    this.content.append(box);
  }

  // The counts, with the columns and rows that are all zero left out: "Skipped" shows only when something is in it, and a non-zero one is marked.
  // Rows already in v2 (the report's already_present) are not shown.
  renderReportCounts(report) {
    this.renderAccounting(report);
    const keys = [...new Set([...Object.keys(report.imported || {}), ...Object.keys(report.skipped || {})])].sort((a, b) => countRank(a) - countRank(b) || a.localeCompare(b));
    const value = (map, key) => map?.[key] || 0;
    const rows = keys.filter(key => value(report.imported, key) || value(report.skipped, key));
    const showSkipped = rows.some(key => value(report.skipped, key));
    const table = node('table'); const head = node('tr');
    for (const label of ['Record', 'New', ...(showSkipped ? ['Skipped'] : [])]) head.append(node('th', label));
    table.append(head);
    for (const key of rows) {
      const [title, explanation] = COUNT_LABELS[key] || [key.replaceAll('_', ' '), ''];
      const row = node('tr'); const name = node('td', title); name.dataset.key = key;
      if (explanation) name.append(node('div', explanation, 'import-count-note'));
      row.append(name, node('td', number(value(report.imported, key))));
      if (showSkipped) row.append(node('td', number(value(report.skipped, key)), value(report.skipped, key) ? 'import-skipped' : ''));
      table.append(row);
    }
    this.content.append(table);
  }

  renderSkippedMemory(report) {
    if (!(report.skipped_memory || []).length) return;
    this.content.append(node('h3', 'Skipped memory'));
    for (const entry of report.skipped_memory) this.content.append(node('p', `${entry.scope} · ${entry.scope_key} · ${entry.id}: ${entry.reason}`));
  }

  renderFinishedSummary(report) {
    const counts = report.imported || {};
    this.content.append(node('h3', 'Imported from v1'));
    this.content.append(node('p', `${number(counts.rooms)} rooms · ${number(counts.messages)} messages · Historical agents: ${number(report.historical_agents)}`, 'import-finished-summary'));
    this.content.append(node('h3', 'Destination projects'));
    if (!(report.destinations || []).length) this.content.append(node('p', 'No projects were selected.'));
    for (const destination of report.destinations || []) this.content.append(node('p', `${destination.name} · ${destination.folder}`, 'import-destination'));
  }

  async commit() {
    this.error.textContent = '';
    const result = await request('/api/import-v1/commit', { method: 'POST', body: { mapping: this.mapping, digest: this.digest } });
    this.jobID = result.job_id; this.phase = 'import'; this.clearDraft(); this.render(); this.poll();
  }

  // A second dialog names what skipping costs; only its own button posts the skip, and focus starts on the safe choice.
  confirmSkip() {
    const confirm = node('dialog', '', 'import-skip-confirm');
    const close = () => { confirm.close(); confirm.remove(); };
    const keep = node('button', 'Keep reviewing', 'btn'); keep.type = 'button'; keep.onclick = close;
    const skip = node('button', 'Skip import forever', 'btn btn-danger'); skip.type = 'button';
    skip.onclick = () => Promise.resolve(this.skip()).then(close).catch(error => { close(); this.error.textContent = error.message; });
    const actions = node('div', '', 'dialog-actions'); actions.append(keep, skip);
    confirm.append(node('h3', 'Skip the v1 import forever?'),
      node('p', ONLY_CHANCE, 'import-warning'),
      node('p', 'Your v1 rooms, messages, memory, macros, roles and settings will not be brought into v2, and this wizard will not be offered again. Nothing in the app can import the backup later.'),
      actions);
    this.dialog.append(confirm);
    confirm.showModal(); keep.focus();
  }

  async skip() {
    await request('/api/import-v1/skip', { method: 'POST' });
    if (this.preview) this.clearDraft();
    this.dialog.close();
  }

  renderImport() {
    this.status.textContent = 'Import running';
    this.content.append(node('p', 'Scanning backup…', 'import-v1-progress'));
  }

  // The one-time links for the accounts the import made. Each works once and expires in an hour, and they are shown here only.
  renderInvites(job) {
    if (job.invite_error) this.content.append(node('p', `${job.invite_error}. Make the links in Settings → Users.`, 'error'));
    if (!(job.invites || []).length) return;
    const box = node('div', '', 'import-invites');
    box.append(node('h3', 'Invite links for the new accounts'), node('p', 'Give each person their link. It works once, sets their password, and expires in an hour; it is shown only here. Lost a link? Make a new one in Settings → Users → Reset link.', 'settings-intro'));
    for (const invite of job.invites) {
      const row = node('div', '', 'import-invite');
      const input = node('input'); input.type = 'text'; input.readOnly = true; input.value = invite.url; input.setAttribute('aria-label', `Invite link for ${invite.login}`); input.dataset.inviteFor = invite.login;
      const copy = node('button', 'Copy', 'btn'); copy.type = 'button';
      copy.onclick = async () => { try { await navigator.clipboard.writeText(invite.url); copy.textContent = 'Copied'; } catch { input.select(); copy.textContent = 'Press Ctrl+C'; } };
      row.append(node('strong', invite.login), input, copy); box.append(row);
    }
    this.content.append(box);
  }

  // The status call failed (a network error, or a 404 after a restart cleared the in-memory job). The import may still be
  // running or may have finished, so the modal offers to ask again or to reload from the server's state; it is never stuck.
  pollFailed(error) {
    this.status.textContent = 'Import status unavailable';
    this.error.textContent = `${error.message || 'Could not read the import status'}. The import may still be running or may have finished.`;
    this.actions.replaceChildren(
      this.button('Retry', () => { this.error.textContent = ''; this.actions.replaceChildren(); this.status.textContent = 'Import running'; this.poll(); }),
      this.button('Reload', () => location.reload()));
  }

  async poll() {
    while (this.dialog.open && this.phase === 'import') {
      let job;
      try { job = await request(`/api/import-v1/jobs/${encodeURIComponent(this.jobID)}`); }
      catch (error) { this.pollFailed(error); return; }
      this.content.querySelector('.import-v1-progress').textContent = job.error ? `${IMPORT_PHASES[job.phase] || job.phase}: ${job.error}` : (IMPORT_PHASES[job.phase] || job.phase);
      if (job.phase === 'done') {
        this.status.textContent = 'v1 import complete';
        this.content.replaceChildren();
        this.heading.textContent = 'Import finished'; // the dialog's own heading says so; the invite links stay the first thing in the content
        // The links come first: they are shown only here, so they must not sit below a long report where a scroll would hide them.
        this.renderInvites(job);
        if (!(job.invites || []).length && !job.invite_error) this.content.append(node('p', 'No new accounts were created, so there are no invite links.', 'settings-intro import-no-accounts'));
        if (job.report) { this.renderFinishedSummary(job.report); this.renderReportCounts(job.report); this.renderSkippedMemory(job.report); this.content.append(...(job.report.carried_over_files?.length ? [node('h3', 'Files kept for v2'), ...job.report.carried_over_files.map(path => node('p', path))] : [])); }
        else this.content.append(node('p', 'Import completed. Refresh to see the imported rooms.'));
        this.actions.append(this.button('Close and refresh', () => location.reload()));
        return;
      }
      if (job.phase === 'error') { this.status.textContent = 'Import paused'; this.actions.append(this.button('Review choices', () => { this.phase = 'review'; this.render(); })); return; }
      await new Promise(resolve => setTimeout(resolve, 700));
    }
  }
}
