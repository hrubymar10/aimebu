import { request } from './api.js';
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

const NEW_PROJECT = '__new_project__';
const NEW_ACCOUNT = '__new_account__';

function draftKey(sourceKey) { return `aimebu2.import-draft.${sourceKey}`; }

// Set when an import finishes, read once by the next page load: with no harness yet the owner is taken to set one up.
export const AFTER_IMPORT_KEY = 'aimebu2.import.check-harness';
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
    this.error = node('p', '', 'error'); this.error.setAttribute('role', 'alert');
    this.content = node('div', '', 'import-v1-content');
    this.actions = node('div', '', 'dialog-actions');
    this.skipZone = node('div', '', 'import-skip-zone');
    this.dialog.append(this.heading, this.progress, this.status, this.error, this.content, this.actions, this.skipZone);
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
    for (const human of undecided) mapping.humans[human] = this.preview.suggested.humans?.[human] ?? '-';
    this.defaultPeople(mapping, human => undecided.has(human));
    this.mapping = mapping;
    if (this.steps.some(step => step.id === draft.step)) this.stepID = draft.step;
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
    box.onchange = () => { update(box.checked); this.save(); };
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
    if (this.phase === 'changed') this.renderChanged();
    else if (this.phase === 'review') this.renderStep();
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
    if (step.id !== 'summary') this.actions.append(this.button('Next', () => this.next()));
    if (index === 0) {
      this.offerSkip();
      if (this.restored) this.skipZone.prepend(this.button('Start over', () => this.startOver(), true));
    }
  }

  startOver() {
    this.clearDraft();
    this.mapping = structuredClone(this.preview.suggested);
    for (const key of ['macro_conflicts', 'role_conflicts', 'project_members', 'memory_projects', 'enable_project_memory']) this.mapping[key] ||= {};
    this.mapping.skip_macros ||= [];
    this.defaultPeople(this.mapping);
    this.restored = false; this.stepID = this.steps[0].id;
    this.render();
  }

  async go(id) {
    this.stepID = id; this.newProject = null; this.save();
    if (id === 'summary') this.report = null;
    this.render();
    this.content.scrollTop = 0;
    if (id === 'summary') await this.check();
  }

  // Next stops on the first thing that cannot be imported as it stands, with a link to it.
  async next() {
    this.error.textContent = '';
    const problem = this[`problem_${this.stepID}`]?.();
    if (problem) { this.showProblem(problem); return; }
    await this.go(this.steps[this.stepIndex + 1].id);
  }

  showProblem(problem) {
    const link = node('button', 'Go to the field', 'btn btn-link'); link.type = 'button';
    link.onclick = () => this.focusProblem(problem);
    this.error.replaceChildren(node('span', `${problem.message} `), link);
    this.focusProblem(problem);
  }

  focusProblem(problem) {
    const input = this.content.querySelector(problem.selector);
    if (!input) return;
    for (const old of this.content.querySelectorAll('[aria-invalid]')) old.removeAttribute('aria-invalid');
    input.closest('details')?.setAttribute('open', '');
    input.setAttribute('aria-invalid', 'true');
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
    this.status.textContent = `This is the only chance to import v1 history; skipping closes the import permanently, and the original backup stays on disk. The backup holds ${counts.rooms || 0} rooms, ${counts.messages || 0} messages, ${counts.attachments || 0} attachments and ${counts.memory || 0} memory records. First, the projects the history goes into: each needs a name and an absolute workspace folder.`;
    if (this.restored) this.content.append(node('p', 'Continuing where you left off.', 'import-restored'));
    for (const [alias, project] of Object.entries(this.mapping.projects)) {
      const box = node('fieldset'); box.append(node('legend', alias));
      const name = this.field('Name', project.name, value => { project.name = value; });
      name.querySelector('input').dataset.projectName = alias; box.append(name);
      const folder = this.field('Workspace folder (absolute path)', project.folder, value => { project.folder = value; });
      folder.querySelector('input').dataset.projectFolder = alias; folder.querySelector('input').placeholder = '/path/to/the/project';
      box.append(folder);
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

  problem_projects() {
    for (const [alias, project] of Object.entries(this.mapping.projects)) {
      if (project.existing_id) continue;
      if (!(project.name || '').trim()) return { message: `Project "${alias}" needs a name.`, selector: `input[data-project-name="${CSS.escape(alias)}"]` };
      if (!(project.folder || '').startsWith('/')) return { message: `Project "${project.name || alias}" needs its workspace folder: an absolute path such as /Users/you/code/${alias}.`, selector: `input[data-project-folder="${CSS.escape(alias)}"]` };
    }
    return null;
  }

  page_people() {
    const p = this.preview;
    this.mapping.new_users ||= {};
    this.status.textContent = 'People sharing one destination account are merged for access. Their original names remain on historical messages. A person can get a new account made by the import (no password: you get a one-time link for each at the end). Skipped people keep a historical label and receive no live room membership.';
    const people = [['-', 'Skip person (legacy label only)'], ...(p.destination_users || [this.mapping.owner_login]).map(login => [login, login === this.mapping.owner_login ? `${login} (owner)` : login]), [NEW_ACCOUNT, 'Create a new account…']];
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
      if (!created && this.mapping.humans[human] === '-') this.content.append(node('p', 'No account: their messages keep their name as a label, and nobody can sign in as them. Choose "Create a new account…" to give them one.', 'import-warning'));
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
    const taken = new Set([...(this.preview.destination_users || []), ...Object.values(mapping.new_users || {}).map(user => user.login)]);
    let base = name.toLowerCase().replace(/[^a-z0-9_-]+/g, '-').replace(/^[^a-z]+/, '').slice(0, 28);
    if (base.length < 3) base = `user-${base}`.replace(/-$/, '');
    if (base.length < 3) base = 'user';
    let login = base, n = 2;
    while (taken.has(login)) login = `${base}-${n++}`;
    return login;
  }

  problem_people() {
    const taken = new Set(this.preview.destination_users || []);
    const seen = new Set();
    for (const [human, user] of Object.entries(this.mapping.new_users || {})) {
      const selector = `input[data-new-login="${CSS.escape(human)}"]`;
      if (!/^[a-z][a-z0-9_-]{2,31}$/.test(user.login || '')) return { message: `The login for ${human} must be 3 to 32 characters: a lowercase letter, then lowercase letters, digits, - or _.`, selector };
      if (taken.has(user.login)) return { message: `The login "${user.login}" is already an account here; map ${human} to it instead, or choose another login.`, selector };
      if (seen.has(user.login)) return { message: `Two people would both be "${user.login}"; give each their own login.`, selector };
      seen.add(user.login);
    }
    return null;
  }

  page_rooms() {
    const p = this.preview;
    const shown = (p.rooms || []).filter(room => room.id !== SYSTEM_ROOM);
    const rooms = shown.filter(room => !room.dm);
    const tree = planTree(this.mapping, shown);
    const kept = rooms.filter(room => !this.mapping.rooms[room.id]?.skip && this.mapping.rooms[room.id]?.project).length;
    this.status.textContent = `${kept} of ${rooms.length} rooms will be imported. Choose where each v1 room goes; a room with a message in the last 30 days starts open, an older one archived. Rooms sent to the same place merge, their messages interleaved by their v1 numbers.`;
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
    }
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
    this.status.textContent = `${picked} of ${direct.length} chats will be imported. Each private chat becomes a v2 direct message titled by the other person or agent, always open. A chat between two agents is imported as history you can read. No chat is imported unless you tick it; a ticked chat goes into its agent's project when there is one, and you can pick another.`;
    for (const room of direct) {
      const spec = this.mapping.rooms[room.id] ||= { skip: true };
      const box = node('fieldset'); const legend = node('legend', this.dmTitle(room)); legend.dataset.dmRoom = room.id; box.append(legend);
      // Titles can collide (two chats with the same agent), so the source id and size are always shown.
      box.append(node('p', `${room.id} · ${room.messages} messages`, 'import-source-id'));
      const toggle = this.checkbox('Import this chat', !spec.skip, checked => { spec.skip = !checked; });
      toggle.querySelector('input').onchange = event => { spec.skip = !event.target.checked; this.save(); this.refresh(); };
      box.append(toggle);
      if (!spec.skip) box.append(this.projectSelect('Project', spec.project || '', alias => { spec.project = alias; }, 'Choose a project'));
      this.content.append(box);
    }
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

  problem_dms() {
    for (const room of (this.preview.rooms || []).filter(item => item.dm)) {
      const spec = this.mapping.rooms[room.id];
      if (spec && !spec.skip && !spec.project) return { message: `Choose a project for the chat "${this.dmTitle(room)}", or turn it off.`, selector: `[data-dm-room="${CSS.escape(room.id)}"]` };
    }
    return null;
  }

  page_memory() {
    const p = this.preview;
    this.status.textContent = `The backup holds ${p.counts?.memory || 0} memory records. Imported records arrive as ordinary, active ones.`;
    for (const scope of p.memory_projects || []) this.content.append(this.projectSelect(`Project facts: ${scope}`, (this.mapping.memory_projects[scope] || '-') === '-' ? '' : this.mapping.memory_projects[scope], value => { this.mapping.memory_projects[scope] = value || '-'; }, "Don't import"));
    for (const [key, title] of [['skip_profiles', 'Import global user profiles'], ['skip_shared_notes', 'Import shared agent notes']]) {
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
    const bulk = node('div', '', 'import-bulk');
    bulk.append(this.button('Import all', () => { this.mapping.skip_macros = []; this.save(); this.refresh(); }, true), this.button('Import none', () => { this.mapping.skip_macros = [...keys]; this.save(); this.refresh(); }, true));
    this.content.append(bulk);
    for (const key of keys) {
      const body = p.macros[key];
      const box = node('fieldset'); box.append(node('legend', `<${key.toUpperCase()}>${(p.macro_defaults || []).includes(key) ? ' · v1 seeded default' : ''}`));
      const imported = (p.imported_macros || []).includes(key);
      if (imported) box.append(node('p', 'Already imported from this backup; later edits are preserved.'));
      else {
        const include = this.checkbox('Import this macro', !skip.has(key), checked => { const set = new Set(this.mapping.skip_macros); if (checked) set.delete(key); else set.add(key); this.mapping.skip_macros = [...set]; });
        include.querySelector('input').onchange = event => { const set = new Set(this.mapping.skip_macros); if (event.target.checked) set.delete(key); else set.add(key); this.mapping.skip_macros = [...set]; this.save(); this.refresh(); };
        box.append(include);
      }
      const source = node('pre', body, 'import-macro-body'); box.append(node('p', 'v1 text'), source);
      const current = this.destinationMacros[key];
      if (Object.hasOwn(this.destinationMacros, key)) {
        box.append(node('p', current === body ? 'Already matches v2.' : 'Existing v2 text:'));
        if (current !== body) {
          box.append(node('pre', current, 'import-macro-body'));
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
    list('Macro conflicts', Object.entries(report.macro_conflicts || {}).sort(([a], [b]) => a.localeCompare(b)).map(([key, choice]) => `<${key.toUpperCase()}>: ${choice === 'use_v1' ? 'use v1 text' : 'keep v2 text'}`));
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

  // The counts, with the columns and rows that are all zero left out: "Skipped" shows only when something is in it. Rows already in v2 (the report's already_present) are not shown.
  renderReportCounts(report) {
    const keys = [...new Set([...Object.keys(report.imported || {}), ...Object.keys(report.skipped || {})])].sort();
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
      row.append(name, node('td', String(value(report.imported, key))));
      if (showSkipped) row.append(node('td', String(value(report.skipped, key))));
      table.append(row);
    }
    this.content.append(table);
  }

  renderSkippedMemory(report) {
    if (!(report.skipped_memory || []).length) return;
    this.content.append(node('h3', 'Skipped memory'));
    for (const entry of report.skipped_memory) this.content.append(node('p', `${entry.scope} · ${entry.scope_key} · ${entry.id}: ${entry.reason}`));
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
      node('p', 'Your v1 rooms, messages, memory, macros, roles and settings will not be brought into v2, and this wizard will not be offered again. The original backup stays on disk, but nothing in the app can import it later.'),
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
        // The links come first: they are shown only here, so they must not sit below a long report where a scroll would hide them.
        this.renderInvites(job);
        if (job.report) { this.renderReportCounts(job.report); this.renderSkippedMemory(job.report); this.content.append(...(job.report.carried_over_files?.length ? [node('h3', 'Files kept for v2'), ...job.report.carried_over_files.map(path => node('p', path))] : [])); }
        else this.content.append(node('p', 'Import completed. Refresh to see the imported rooms.'));
        this.actions.append(this.button('Close and refresh', () => { try { localStorage.setItem(AFTER_IMPORT_KEY, '1'); } catch { /* the check after an import is only a convenience */ } location.reload(); }));
        return;
      }
      if (job.phase === 'error') { this.status.textContent = 'Import paused'; this.actions.append(this.button('Review choices', () => { this.phase = 'review'; this.render(); })); return; }
      await new Promise(resolve => setTimeout(resolve, 700));
    }
  }
}
