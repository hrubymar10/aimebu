// The v1 import wizard's state and calls: the phase, the step, the preview, the owner's choices (the mapping, kept in the browser as a draft
// per backup), the dry run and the import job. It draws nothing: `host` is the adapter (import-v1.js), which renders the components at once
// on draw() and does what needs the page (the dialog, focus, scroll). The mapping is one mutable object, edited in place by the pages, as
// the import's own format is; what the owner types lives there or in newProject, never only in the DOM.
import { request } from '../../api.js';
import { confirmDialog } from '../../dialogs.js';
import { dmProblems, peopleProblems, projectProblems } from '../../import-problems.js';
import { newProjectAlias } from '../../import-tree.js';
import { SYSTEM_ROOM, IMPORT_PHASES, defaultPeople, draftKey, dmTitle, stepsFor } from './model.js';

const KEYED = ['macro_conflicts', 'role_conflicts', 'project_members', 'memory_projects', 'enable_project_memory', 'humans', 'projects', 'new_users'];

export class ImportStore {
  constructor(host, currentSession) {
    this.host = host;
    this.currentSession = currentSession;
    this.phase = 'review'; // review, changed (the backup cannot be verified) or import
    this.preview = null;
    this.mapping = null;
    this.steps = [];
    this.stepID = 'projects';
    this.destinationMacros = {};
    this.changedMessage = '';
    this.restored = false;
    this.staleNote = null; // { step }: the page a draft with dropped choices was restored to
    this.newProject = null; // the "New project" form: { then, name, folder, message }
    this.justAdded = null; // a project just made on the Projects page, to focus
    this.error = null; // null, a message, or { problems } from Next
    this.checking = null; // the dry run: null, 'running' or 'failed'
    this.report = null; this.digest = null;
    this.jobID = null;
    this.resetImport();
    this.title = 'Import v1 history';
    this.confirmingSkip = false;
    this.checkGeneration = 0; this.pollGeneration = 0;
  }

  resetImport() { this.job = { state: 'running', progress: 'Scanning backup…', result: null }; }

  // render() is a fresh view of the current state, which drops the last error (as a redraw always did); draw() keeps it.
  render() { this.error = null; this.draw(); }
  refresh() { this.render(); }
  draw() {
    // The note about dropped choices stays on the page the draft was restored to, and goes when the owner moves on.
    if (this.phase === 'review' && this.staleNote && this.staleNote.step !== this.stepID) this.staleNote = null;
    this.host.draw();
  }
  fail(error) { this.error = error?.message || String(error); this.draw(); }

  get stepIndex() { return this.steps.findIndex(step => step.id === this.stepID); }

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
    if (status.state === 'ready' || status.state === 'resume') return this.open(signal, isCurrent);
    return false;
  }

  showBackupChanged(message, signal, isCurrent = this.currentSession()) {
    if (signal?.aborted || !isCurrent()) return false;
    this.preview = null; this.mapping = null;
    this.changedMessage = message;
    this.phase = 'changed';
    this.render();
    this.host.showModal();
    return true;
  }

  async open(signal, isCurrent = this.currentSession()) {
    this.error = null;
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
    const mapping = structuredClone(preview.suggested);
    for (const key of [...KEYED, 'rooms']) mapping[key] ||= {};
    mapping.skip_macros ||= [];
    // Every room the pages list has a choice from the start, so a page never has to make one while it draws.
    for (const room of preview.rooms || []) if (room.id !== SYSTEM_ROOM) mapping.rooms[room.id] ||= { skip: true };
    defaultPeople(preview, mapping);
    this.mapping = mapping;
    this.steps = stepsFor(preview);
    this.stepID = this.steps[0].id;
    this.restored = this.restoreDraft();
    this.phase = 'review';
    this.render();
    if (signal?.aborted || !isCurrent()) return false;
    this.host.showModal();
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
    mapping.rooms ||= {};
    for (const [id, spec] of Object.entries(this.mapping.rooms)) mapping.rooms[id] ||= spec;
    if (this.mapping.rooms[SYSTEM_ROOM]) mapping.rooms[SYSTEM_ROOM] = this.mapping.rooms[SYSTEM_ROOM]; // never imported, whatever an old draft says
    for (const key of KEYED) mapping[key] ||= {};
    mapping.skip_macros ||= [];
    // A person the draft has no choice for gets the suggestion (and its default); a person it does have a choice for keeps it, a skip included.
    const undecided = new Set((this.preview.humans || []).filter(human => mapping.humans[human] === undefined));
    for (const human of undecided) {
      mapping.humans[human] = this.preview.suggested.humans?.[human] ?? '-';
      const account = this.preview.suggested.new_users?.[human];
      if (account) mapping.new_users[human] = { ...account }; // the suggested login is a new account, not a link to an existing one
    }
    defaultPeople(this.preview, mapping, human => undecided.has(human));
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

  // A choice was made on a page: keep it, and draw the page again (a choice that changes what the page shows) or only what adds it up.
  changed(redraw = true) { this.save(); if (redraw) this.refresh(); else this.draw(); }

  // Starting over throws away the choices of every page, so it asks first, in the app's own dialog (its focus starts on Keep editing, and Escape is Keep editing).
  async startOver() {
    if (!await confirmDialog('Your choices on every page will be discarded.', { title: 'Start over?', confirm: 'Discard', cancel: 'Keep editing', danger: true })) return;
    this.clearDraft();
    this.report = null; this.digest = null; this.newProject = null; this.staleNote = null;
    await this.open();
  }

  async go(id) {
    this.stepID = id; this.newProject = null; this.save();
    if (id === 'summary') this.report = null;
    this.render();
    this.host.scrollToTop();
    if (id === 'summary') await this.check();
  }

  problems(stepID = this.stepID) {
    if (stepID === 'projects') return projectProblems(this.mapping.projects);
    if (stepID === 'people') return peopleProblems(this.mapping.new_users, this.preview.destination_users);
    if (stepID === 'dms') return dmProblems(this.preview.rooms, this.mapping.rooms, room => dmTitle(room, this.mapping));
    return [];
  }

  // Next stops while anything on the page cannot be imported as it stands, and lists all of it at once: a link to each field and a message
  // beside each, so the owner fixes them together instead of finding them one Next at a time.
  async next() {
    this.error = null;
    const problems = this.problems();
    if (problems.length) { this.error = { problems }; this.draw(); this.host.focusProblem(problems[0]); return; }
    await this.go(this.steps[this.stepIndex + 1].id);
  }

  // "+ New project…" opens a small form in place; the project it makes is handed to `then`, so nothing sends the owner back a page.
  askNewProject(then) { this.newProject = { then, name: '', folder: '', message: '' }; this.refresh(); }
  cancelNewProject() { this.newProject = null; this.refresh(); }

  // Makes the project the form describes, or says what is missing and returns the field to fix ('name' or 'folder').
  createProject(name, folder) {
    const title = name.trim();
    if (!title) { this.newProject.message = 'Give the project a name.'; this.draw(); return 'name'; }
    if (!folder.startsWith('/')) { this.newProject.message = 'The folder must be an absolute path such as /Users/you/code/project.'; this.draw(); return 'folder'; }
    const alias = newProjectAlias(title, new Set(Object.keys(this.mapping.projects)));
    this.mapping.projects[alias] = { name: title, folder };
    this.mapping.enable_project_memory[alias] = !!this.preview.v1_memory_enabled;
    const then = this.newProject.then; this.newProject = null;
    this.justAdded = alias; then?.(alias); this.save(); this.refresh();
    return '';
  }

  // A removed project takes nothing with it: its rooms and direct messages become "don't import" and its memory scopes are skipped.
  removeProject(alias) {
    delete this.mapping.projects[alias]; delete this.mapping.project_members[alias]; delete this.mapping.enable_project_memory[alias];
    for (const spec of Object.values(this.mapping.rooms)) if (spec.project === alias) { spec.skip = true; spec.project = ''; delete spec.target; delete spec.title; }
    for (const [scope, target] of Object.entries(this.mapping.memory_projects)) if (target === alias) this.mapping.memory_projects[scope] = '-';
    if (this.mapping.default_project === alias) this.mapping.default_project = '';
    this.save(); this.refresh();
  }

  // ---- summary: the dry run ----

  async check() {
    this.error = null; this.report = null; this.checking = 'running';
    this.draw();
    const generation = ++this.checkGeneration, wanted = this.stepID;
    const current = () => generation === this.checkGeneration && this.stepID === wanted;
    let result;
    try { result = await request('/api/import-v1/dry-run', { method: 'POST', body: this.mapping }); }
    catch (error) { if (current()) { this.checking = 'failed'; this.error = error.message; this.draw(); } return; }
    if (!current()) return;
    this.checking = null; this.digest = result.digest; this.report = result.report;
    this.render();
  }

  async commit() {
    this.error = null;
    const result = await request('/api/import-v1/commit', { method: 'POST', body: { mapping: this.mapping, digest: this.digest } });
    this.jobID = result.job_id; this.resetImport(); this.phase = 'import'; this.clearDraft(); this.render(); this.poll();
  }

  // ---- skipping ----

  confirmSkip() { this.confirmingSkip = true; this.draw(); }
  closeSkipConfirm() { if (!this.confirmingSkip) return; this.confirmingSkip = false; this.draw(); }
  async skipForever() {
    try { await this.skip(); this.closeSkipConfirm(); }
    catch (error) { this.closeSkipConfirm(); this.fail(error); }
  }
  async skip() {
    await request('/api/import-v1/skip', { method: 'POST' });
    if (this.preview) this.clearDraft();
    this.host.close();
  }

  // ---- the import job ----

  // Each poll has a generation: closing the dialog, leaving the import phase or starting another poll ends it, even mid-request.
  async poll() {
    const generation = ++this.pollGeneration;
    const live = () => generation === this.pollGeneration && this.host.isOpen() && this.phase === 'import';
    while (live()) {
      let job;
      try { job = await request(`/api/import-v1/jobs/${encodeURIComponent(this.jobID)}`); }
      catch (error) { if (generation === this.pollGeneration && this.phase === 'import') this.pollFailed(error); return; }
      if (!live()) return;
      const words = IMPORT_PHASES[job.phase] || job.phase;
      this.job = { ...this.job, progress: job.error ? `${words}: ${job.error}` : words };
      if (job.phase === 'done') {
        this.job = { state: 'done', progress: this.job.progress, result: job };
        this.title = 'Import finished'; // the dialog's own heading says so; the invite links stay the first thing in the content
        this.draw();
        return;
      }
      if (job.phase === 'error') { this.job = { ...this.job, state: 'paused' }; this.draw(); return; }
      this.draw();
      await new Promise(resolve => setTimeout(resolve, 700));
    }
  }
  stopPolling() { this.pollGeneration++; }

  // The status call failed (a network error, or a 404 after a restart cleared the in-memory job). The import may still be
  // running or may have finished, so the modal offers to ask again or to reload from the server's state; it is never stuck.
  pollFailed(error) {
    this.job = { ...this.job, state: 'failed' };
    this.error = `${error.message || 'Could not read the import status'}. The import may still be running or may have finished.`;
    this.draw();
  }
  retryPoll() { this.error = null; this.job = { ...this.job, state: 'running' }; this.draw(); this.poll(); }
  reviewChoices() { this.phase = 'review'; this.render(); }
}
