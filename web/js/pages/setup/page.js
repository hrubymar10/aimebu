import { html, effect } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';
import { request } from '../../api.js';
import { SetupStore } from './store.js';
import { TokenStep, OwnerStep, ThemeStep } from './components.js';
import { showWelcome } from './welcome.js';

// The adapter writes what is outside the steps: each view's hidden and the page's theme. The steps are drawn into the views themselves.
export class Setup {
  constructor({ views, welcome = showWelcome, send = request, search = location.search } = {}) {
    this.views = views;
    this.store = new SetupStore({ send, welcome: () => welcome(), linked: new URLSearchParams(search).get('token') || '', replaceUrl: () => history.replaceState(null, '', '/') });
    const onError = error => console.error('setup:', error), steps = { token: TokenStep, owner: OwnerStep, theme: ThemeStep };
    this.pages = Object.entries(steps).filter(([name]) => views[name]).map(([name, Step]) => mountPreactPage(views[name], () => html`<${Step} store=${this.store} />`, { onError }));
    this.stops = [
      ...Object.keys(steps).filter(name => views[name]).map(name => effect(() => { views[name].hidden = this.store.step.value !== name; })),
      effect(() => { const theme = this.store.theme.value; if (theme === 'dark') delete document.documentElement.dataset.theme; else document.documentElement.dataset.theme = theme; }),
    ];
  }

  run() { return this.store.run(); }
  chooseTheme(userID) { return this.store.startTheme(userID); }

  destroy() { for (const stop of this.stops) stop(); this.store.destroy(); for (const page of this.pages) page.destroy(); }
}

export async function ownerSetup(options) {
  const setup = new Setup(options);
  try { return await setup.run(); } finally { setup.destroy(); }
}

export async function chooseTheme(view, userID, send = request) {
  const setup = new Setup({ views: { theme: view }, send });
  try { return await setup.chooseTheme(userID); } finally { setup.destroy(); }
}
