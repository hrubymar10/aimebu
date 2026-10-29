import { batch, signal } from '../../ui.js';
import { HARNESS_MISSING, NO_MODEL_NOTE, checkProject, harnessStatusText, howView } from './model.js';

// The first-run onboarding: Create your first project, the Harnesses step, and How aimebu works. Every answer is checked against `ticket`:
// show(), close(), opening or leaving the Harnesses step, and destroy() start a new one, so a creation, a readiness poll, an addition or a
// skip that answers late changes nothing. The host does the effects that leave this page (creating, spawning, recording a skip).
export class OnboardingStore {
  constructor({ create, spawn, harnessReady = async () => true, canSetUp = () => true, onSkip = () => {}, onReady = () => {}, confirm, pollMs = 2000 }) {
    Object.assign(this, { create, spawn, harnessReady, canSetUp, onSkip, onReady, confirm, pollMs });
    this.open = signal(false);
    this.step = signal('project');
    this.error = signal('');
    this.creating = signal(false);
    this.harnessStatus = signal('');
    this.continueDisabled = signal(true);
    this.how = signal({ spawnHidden: false, setupHidden: true, noteHidden: true, noteText: '' });
    this.focus = signal({ n: 0, target: '' });
    this.resetTick = signal(0);
    // 'first': the step after creating the first project (Spawn follows). 'harness-only': opened on its own, for an import that left no
    // harness: there is no new room to spawn into, so both buttons just close it.
    this.mode = 'first';
    this.ticket = 0; this.alive = true; this.watch = null; this.adding = 0; this.harnessNote = ''; this.lastReady = undefined;
    this.harnesses = null; this.flush = () => {};
  }

  current(ticket) { return this.alive && ticket === this.ticket; }

  set(change) { batch(change); this.flush(); }

  focusOn(target) { this.focus.value = { n: this.focus.value.n + 1, target }; }

  // Stops the readiness poll and invalidates what is still waiting; answers the new ticket.
  stopWatching() { clearInterval(this.watch); this.watch = null; this.creating.value = false; return ++this.ticket; }

  show() {
    this.stopWatching(); this.mode = 'first';
    this.set(() => { this.error.value = ''; this.resetTick.value++; this.step.value = 'project'; this.open.value = true; this.focusOn('name'); });
  }

  close() { this.stopWatching(); this.set(() => { this.open.value = false; }); this.harnesses.clear(); }

  // The Harnesses step: the Settings → Harnesses component itself, and Continue once a harness with a model exists. It checks that every
  // couple of seconds while it is shown, since the component saves on its own.
  async showHarnesses(mode = this.mode) {
    this.stopWatching();
    const ticket = this.ticket;
    this.mode = mode; this.harnessNote = '';
    this.set(() => { this.open.value = true; this.step.value = 'harnesses'; this.continueDisabled.value = true; this.harnessStatus.value = HARNESS_MISSING; });
    await this.harnesses.open();
    if (!this.current(ticket)) return;
    await this.checkHarness();
    if (!this.current(ticket)) return;
    this.watch = setInterval(() => void this.checkHarness(), this.pollMs);
  }

  // Continue is possible with a harness ready, or with some detected harness ticked (Continue adds it). `quick` skips the server
  // question: ticking a box only changes the count, so the answer last given still holds. While Continue is adding harnesses
  // (`adding`, owned by the ticket of the step it started in), nothing here may re-enable it or rewrite its status: the poll could otherwise let a
  // second click add them again. A step opened afterwards is independent of an older add, and the older one cannot unlock or advance it.
  async checkHarness(quick = false) {
    if (this.adding === this.ticket) return;
    const ticket = this.ticket;
    let ready = quick ? this.lastReady : false;
    if (!quick) { try { ready = await this.harnessReady(); } catch { ready = false; } if (!this.current(ticket)) return; this.lastReady = ready; }
    if (this.step.value !== 'harnesses' || this.adding === this.ticket) return;
    const ticked = this.harnesses.tickedCount();
    this.set(() => { this.continueDisabled.value = !ready && !ticked; this.harnessStatus.value = harnessStatusText({ ready, ticked, note: this.harnessNote }); });
    if (ready && !quick) this.onReady();
  }

  // Continue adds the ticked harnesses (with their models) first, then goes on once one can start an agent. It is one run at a time.
  async continueHarnesses() {
    if (this.continueDisabled.value) return;
    const ticket = this.ticket;
    this.adding = ticket; this.harnessNote = '';
    this.set(() => { this.continueDisabled.value = true; });
    let proceed = false;
    try {
      if (this.harnesses.tickedCount()) {
        const added = await this.harnesses.addTicked();
        if (!this.current(ticket)) return;
        if (!added) { this.harnessNote = ''; return; }
        let ready = false; try { ready = await this.harnessReady(); } catch { ready = false; }
        if (!this.current(ticket)) return;
        this.lastReady = ready;
        if (!ready) { this.harnessNote = NO_MODEL_NOTE; return; }
      }
      proceed = true;
    } finally {
      if (this.adding === ticket) this.adding = 0;
      // The status and the button follow what is true now, with the note kept until the person does something about it.
      if (!proceed && this.current(ticket) && this.step.value === 'harnesses') await this.checkHarness();
    }
    await this.leaveHarnesses(true);
  }

  async skipHarnesses() {
    const ticket = this.ticket;
    if (!await this.confirm('Skip harness setup? Agents need a harness with a model to run on. You can set one up later in Settings → Harnesses.', { title: 'Skip harness setup', confirm: 'Skip for now' })) return;
    if (this.current(ticket)) await this.leaveHarnesses(false);
  }

  async leaveHarnesses(ready) {
    const ticket = this.stopWatching();
    this.harnesses.clear(); this.harnessNote = '';
    // A harness may have become ready while this step was open (it saves on its own): look again before calling this a skip, so a person
    // who set one up is neither recorded as having skipped nor told to set one up first.
    if (!ready) { try { ready = await this.harnessReady(); } catch { ready = false; } if (!this.current(ticket)) return; }
    // Skipping the step is remembered by the app (on the person's account), so it is not offered again until a harness exists.
    if (!ready) { try { await this.onSkip(this.mode); } catch { /* the offer may come back on the next sign-in */ } if (!this.current(ticket)) return; }
    if (this.mode === 'harness-only') { this.close(); return; }
    this.set(() => { this.step.value = 'how'; });
    await this.offerSpawn(ready, ticket);
  }

  // The last step offers to spawn only when something can be spawned. Asked again unless a caller just established it: a stale `false` must
  // not say "set a harness up first" to someone who did.
  async offerSpawn(known, ticket = this.ticket) {
    let ready = known; if (!ready) { try { ready = await this.harnessReady(); } catch { ready = known ?? true; } }
    if (!this.current(ticket)) return;
    const view = howView(ready, this.canSetUp());
    this.set(() => { this.how.value = view; this.focusOn(view.focus); });
  }

  async submit(name, folder) {
    if (this.creating.value) return;
    const checked = checkProject(name, folder);
    this.set(() => { this.error.value = checked.error; if (checked.error) this.focusOn(checked.focus); });
    if (checked.error) return;
    const ticket = this.ticket;
    this.set(() => { this.creating.value = true; });
    try {
      await this.create({ name: checked.name, folder: checked.folder });
      if (!this.current(ticket)) return;
      let ready = true; try { ready = await this.harnessReady(); } catch { ready = true; }
      if (!this.current(ticket)) return;
      if (!ready && this.canSetUp()) await this.showHarnesses('first');
      else { this.set(() => { this.step.value = 'how'; }); await this.offerSpawn(ready, ticket); }
    } catch (error) { if (this.current(ticket)) this.set(() => { this.error.value = error.message; }); }
    finally { if (this.current(ticket)) this.set(() => { this.creating.value = false; }); }
  }

  spawnNow() { this.close(); this.spawn(); }

  destroy() { this.alive = false; this.stopWatching(); }
}
