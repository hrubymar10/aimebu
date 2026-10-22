// First-run onboarding and the empty state. With no project yet, the owner is taken through "Create your first project" (a project
// with its home room and a "general" room) and a short "How aimebu works"; the same flow opens from the empty state that shows when
// no room is selected.
import { request } from './api.js';
import { HarnessSettings } from './harness-settings.js';

export const PRINCIPLES = [
  ['A project is one repo or folder.', 'Everything you do with agents happens inside a project.'],
  ['Every project has a home room ⌂ and a general room.', 'Almost all work happens in general.'],
  ['Extra sub-rooms are rare.', 'Your leader proposes one and you agree to it.'],
  ['Agents are spawned into a room.', 'Use Spawn; they keep working there until you stop them.'],
  ['Agents take roles.', 'The leader plans and coordinates, workers build, reviewers check.'],
  ['Anything that needs you lands in the Inbox.', 'You decide there: approve, answer or redirect.'],
  ['@mentions and replies wake agents.', 'A reaction acknowledges without waking anyone.'],
];

export class Onboarding {
  // create(project) makes the project (and returns its general room, already selected); spawn() opens the Spawn dialog in it;
  // harnessReady() answers whether a harness with a model can start an agent right now (true when it cannot be told). canSetUp says
  // whether this person may add harnesses (an owner or an admin): only they get the Harnesses step, anyone else is told to ask one.
  constructor({ create, spawn, harnessReady = async () => true, canSetUp = () => true, send = request, root = document.querySelector('#onboarding-view'), pollMs = 2000 }) {
    this.create = create; this.spawn = spawn; this.harnessReady = harnessReady; this.canSetUp = canSetUp; this.send = send; this.root = root; this.pollMs = pollMs;
    this.form = root.querySelector('#onboarding-form'); this.error = root.querySelector('#onboarding-error');
    this.steps = { project: root.querySelector('[data-step="project"]'), harnesses: root.querySelector('[data-step="harnesses"]'), how: root.querySelector('[data-step="how"]') };
    this.harnesses = new HarnessSettings(null, { embedded: true, host: root.querySelector('#onboarding-harnesses-host') });
    this.harnessStatus = root.querySelector('#onboarding-harness-status'); this.harnessContinue = root.querySelector('#onboarding-harnesses-continue');
    // 'first': the step after creating the first project (Spawn follows). 'harness-only': opened on its own, for an import that left no
    // harness: there is no new room to spawn into, so both buttons just close it.
    this.mode = 'first';
    root.querySelector('.onboarding-principles').replaceChildren(...PRINCIPLES.map(([head, rest]) => {
      const item = document.createElement('li'); const strong = document.createElement('strong'); strong.textContent = head; item.append(strong, ` ${rest}`); return item;
    }));
    this.form.onsubmit = event => { event.preventDefault(); void this.submit(); };
    root.querySelector('.onboarding-skip').onclick = () => this.close();
    root.querySelector('#onboarding-spawn').onclick = () => { this.close(); this.spawn(); };
    root.querySelector('#onboarding-setup-harness').onclick = () => void this.showHarnesses('first');
    this.harnessContinue.onclick = () => void this.leaveHarnesses(true);
    root.querySelector('#onboarding-harnesses-skip').onclick = () => void this.leaveHarnesses(false);
    root.querySelector('#onboarding-done').onclick = () => this.close();
    root.addEventListener('keydown', event => { if (event.key === 'Escape') this.close(); });
  }
  get open() { return !this.root.hidden; }
  show() {
    this.error.textContent = ''; this.form.reset(); this.mode = 'first'; this.showStep('project'); this.root.hidden = false;
    this.form.elements.name.focus();
  }
  close() { this.stopWatching(); this.root.hidden = true; this.harnesses.clear(); }
  showStep(name) { for (const [key, node] of Object.entries(this.steps)) node.hidden = key !== name; }
  // The Harnesses step: the Settings → Harnesses component itself (add by hand, auto-detect, models), and Continue once a harness with a
  // model exists. It checks that every couple of seconds while it is shown, since the component saves on its own.
  async showHarnesses(mode = this.mode) {
    this.stopWatching();
    const token = this.openToken; // leaving the step (or opening it again) changes the token, so what is still loading here is dropped
    this.mode = mode; this.root.hidden = false; this.showStep('harnesses');
    this.harnessContinue.disabled = true; this.harnessStatus.textContent = 'No harness with a model yet.';
    await this.harnesses.open();
    if (token !== this.openToken) return;
    await this.checkHarness();
    if (token !== this.openToken) return;
    this.watch = setInterval(() => void this.checkHarness(), this.pollMs);
  }
  // Stops the readiness poll and invalidates a showHarnesses() that is still loading.
  stopWatching() { clearInterval(this.watch); this.watch = null; this.openToken = (this.openToken || 0) + 1; }
  async checkHarness() {
    let ready = false; try { ready = await this.harnessReady(); } catch { ready = false; }
    if (this.steps.harnesses.hidden) return;
    this.harnessContinue.disabled = !ready;
    this.harnessStatus.textContent = ready ? 'A harness with a model is ready.' : 'No harness with a model yet.';
  }
  async leaveHarnesses(ready) {
    this.stopWatching(); this.harnesses.clear();
    if (this.mode === 'harness-only') { this.close(); return; }
    this.showStep('how'); await this.offerSpawn(ready);
  }
  // The last step offers to spawn only when something can be spawned: with no harness yet, an agent has nothing to run on, so it says
  // so and offers to go back and set one up (or, for someone who cannot change harnesses, to ask an admin) instead of a dialog that
  // cannot start.
  async offerSpawn(known) {
    const spawn = this.root.querySelector('#onboarding-spawn'), setup = this.root.querySelector('#onboarding-setup-harness'), note = this.root.querySelector('#onboarding-harness-note');
    let ready = known; if (ready === undefined) { try { ready = await this.harnessReady(); } catch { ready = true; } }
    const manage = this.canSetUp();
    spawn.hidden = !ready; setup.hidden = ready || !manage; note.hidden = ready;
    note.textContent = ready ? '' : manage ? 'No harness with a model is set up yet, so there is nothing for an agent to run on. Set one up, then spawn your first agent.'
      : 'No harness is set up yet, so there is nothing for an agent to run on. Ask an admin to add one, then spawn your first agent.';
    (ready ? spawn : manage ? setup : this.root.querySelector('#onboarding-done')).focus();
  }
  async submit() {
    const name = this.form.elements.name.value.trim(), folder = this.form.elements.folder.value.trim();
    this.error.textContent = '';
    if (!name || !folder) { this.error.textContent = 'Enter a project name and a workspace folder.'; (name ? this.form.elements.folder : this.form.elements.name).focus(); return; }
    const button = this.form.querySelector('button[type=submit]'); button.disabled = true;
    try {
      await this.create({ name, folder });
      let ready = true; try { ready = await this.harnessReady(); } catch { ready = true; }
      if (!ready && this.canSetUp()) await this.showHarnesses('first');
      else { this.showStep('how'); await this.offerSpawn(ready); }
    } catch (error) { this.error.textContent = error.message; }
    finally { button.disabled = false; }
  }
}
