import { Lazy, bridgeClick, whenIdle, lazySession } from './lazy.js';

// What app.js holds in place of the screens that are lazily fetched chunks: Knowledge, the project handbook, Health, and Onboarding. (The sign-in page is in the entry: a signed-out page must always be able to draw.)
// Each keeps the few things that must be there at once (a button, its visibility, a flag the app reads) and queues the calls made before the chunk
// arrives, replaying them when the object is made (see lazy.js). Each chunk is fetched when the browser is idle after the first paint, or at once when
// the screen is used, whichever comes first.

export class LazyKnowledge {
  constructor(projectContext, roomContext) {
    this.lazy = new Lazy(() => import('./pages/knowledge/page.js').then(module => new module.KnowledgePanel(projectContext, roomContext)), 'knowledge');
    this.taskButton = document.querySelector('#task-notes-toggle');
    bridgeClick(document.querySelector('#knowledge-toggle'), this.lazy);
    bridgeClick(this.taskButton, this.lazy);
    this.setRoom(null);
  }
  // The task-notes button follows the room at once (the panel does the same once it is there).
  setRoom(room) {
    const home = room?.kind === 'home';
    this.taskButton.hidden = room?.kind !== 'task' && !home;
    const label = home ? 'Project memory' : 'Task notes';
    this.taskButton.title = label; this.taskButton.setAttribute('aria-label', label);
    this.lazy.run(panel => panel.setRoom(room), { key: 'room', load: false });
  }
  openProject(scope, projectID) { return this.lazy.run(panel => panel.openProject(scope, projectID)); }
  openProjectFacts(projectID) { return this.lazy.run(panel => panel.openProjectFacts(projectID)); }
  openTask() { return this.lazy.run(panel => panel.openTask()); }
  // An event only matters to a panel that is showing something, and one that is not loaded shows nothing.
  onEvent(event) { this.lazy.instance?.onEvent(event); }
}

export class LazyHandbook {
  constructor(projectName) {
    this.lazy = new Lazy(() => import('./pages/project/handbook-view.js').then(module => new module.HandbookView(projectName, this.button)), 'handbook');
    this.button = document.createElement('button');
    this.button.id = 'room-handbook'; this.button.type = 'button';
    this.button.className = 'btn-icon quiet';
    this.button.title = 'Project handbook'; this.button.setAttribute('aria-label', 'Project handbook');
    this.button.innerHTML = '<span class="icon-mask icon-mask-book-open" aria-hidden="true"></span>';
    this.button.hidden = true;
    document.querySelector('#room-info').after(this.button);
    bridgeClick(this.button, this.lazy);
  }
  setRoom(room) {
    this.button.hidden = room?.kind !== 'home';
    this.lazy.run(view => view.setRoom(room), { key: 'room', load: false });
  }
}

export class LazyHealth {
  constructor(make) {
    this.lazy = new Lazy(make, 'health');
    this.ticketValue = 0;
    bridgeClick(document.querySelector('#health-toggle'), this.lazy);
  }
  get ticket() { return this.lazy.instance ? this.lazy.instance.ticket : this.ticketValue; }
  set ticket(value) { this.ticketValue = value; if (this.lazy.instance) this.lazy.instance.ticket = value; }
  // The refreshes are the app's own (a timer, a stream event, sign-in): a chunk that cannot be fetched is not told to the person, only the toggle is.
  refresh() { return this.lazy.run(health => health.refresh(), { key: 'refresh', background: true }); }
  refreshLimits() { return this.lazy.run(health => health.refreshLimits(), { key: 'limits', background: true }); }
  schedule() { this.lazy.run(health => health.schedule(), { key: 'schedule', background: true }); }
  // Closing what was never opened is nothing to do.
  setOpen(open) { if (this.lazy.instance) this.lazy.instance.setOpen(open); }
}

export class LazyOnboarding {
  constructor(options) {
    this.lazy = new Lazy(() => import('./pages/onboarding/page.js').then(module => new module.Onboarding(options)), 'onboarding');
  }
  get open() { return !!this.lazy.instance?.open; }
  show() { this.lazy.run(onboarding => onboarding.show()); }
  // Closing also drops an open that was queued: the card must not appear for a session that ended.
  close() { this.lazy.pending.clear(); this.lazy.instance?.close(); }
  showHarnesses(mode) { const session = lazySession(); return this.lazy.load().then(onboarding => (session === lazySession() ? onboarding.showHarnesses(mode) : undefined)); }
}

// Fetches the chunks a signed-in session is likely to open, one after the other, when the browser is idle (Settings, and the screens of the toolbar).
export function prefetchWhenIdle(importers) {
  const next = () => { const importer = importers.shift(); if (importer) { void importer().catch(() => {}).finally(() => whenIdle(next)); } };
  whenIdle(next);
}
