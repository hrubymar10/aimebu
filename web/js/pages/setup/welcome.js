import { effect, html, signal, useLayoutEffect, useRef } from '../../ui.js';
import { mountPreactPage } from '../mount-page.js';

const FINISH_MS = 2900;
const STARTS = [[-46, -9], [-40, 0], [-46, 9], [46, -9], [40, 0], [46, 9]];

// The screen is four bands: the app icon over the first two, "aimebu v2" in the third and Continue in the fourth. CSS animates; the timer
// only reveals the finished state and moves focus to Continue. Reduced motion, a click, Enter or Space go straight to it.
export class WelcomeStore {
  constructor({ onContinue, reduced, onDone, finishMs = FINISH_MS }) {
    this.onContinue = onContinue; this.onDone = onDone;
    this.stage = signal('logo');
    this.autoFocus = signal(false);
    this.focusTick = signal(0);
    this.closed = signal(false);
    this.alive = true; this.timer = 0; this.flush = () => {};
    if (reduced) this.finish(); else this.timer = setTimeout(() => this.finish(), finishMs);
  }

  finish() {
    clearTimeout(this.timer);
    this.stage.value = 'done'; this.autoFocus.value = true; this.focusTick.value++;
    this.flush();
  }

  // The welcome covers the app, so Tab stays on Continue (the only control); focused by a key press, it shows the keyboard ring.
  skip(event) {
    if (event.type === 'keydown' && event.key === 'Tab') {
      event.preventDefault();
      if (this.stage.value === 'done') { this.autoFocus.value = false; this.focusTick.value++; this.flush(); }
      return;
    }
    if (this.stage.value === 'done') return;
    if (event.type === 'keydown' && event.key !== 'Enter' && event.key !== ' ') return;
    event.preventDefault(); this.finish();
  }

  async proceed() {
    if (this.closed.value || !this.alive) return;
    this.closed.value = true; this.flush();
    try { await this.onContinue?.(); } catch { /* the welcome would come back next sign-in; nothing to block on */ }
    if (this.alive) this.onDone();
  }

  destroy() { this.alive = false; clearTimeout(this.timer); }
}

function Welcome({ store }) {
  const proceed = useRef(null), done = store.stage.value === 'done', tick = store.focusTick.value, auto = store.autoFocus.value;
  useLayoutEffect(() => { if (done) { if (!auto) proceed.current.blur(); proceed.current.focus(); } }, [done, tick]);
  return html`<div class="welcome-art"><span class="welcome-line"></span>
    ${STARTS.map(([x, y], index) => html`<span class="welcome-dot" style=${{ '--sx': `${x}vw`, '--sy': `${y}vh`, '--i': String(index) }}></span>`)}
    <span class="welcome-logo"><img class="welcome-icon welcome-icon-light" src="/icons/icon-512.png" alt="" width="512" height="512" /><img class="welcome-icon welcome-icon-dark" src="/icons/icon-512-dark.png" alt="" width="512" height="512" /></span>
    <span class="welcome-wait" role="status">${done ? '' : 'Getting things ready…'}</span></div>
    <h1 class="welcome-title"><span class="welcome-name">aimebu</span><span class="welcome-version"> v2</span></h1>
    <button type="button" id="welcome-continue" class="btn btn-primary welcome-continue" inert=${!done} disabled=${store.closed.value} data-auto-focus=${done && auto ? '' : undefined} ref=${proceed} onClick=${() => store.proceed()}>Continue</button>`;
}

// The first-run welcome, shown once to the owner between their first sign-in and the app. `done` resolves when Continue has been pressed.
export function openWelcome(onContinue, { reduced = matchMedia('(prefers-reduced-motion: reduce)').matches, finishMs } = {}) {
  const root = document.createElement('main'); root.className = 'welcome'; root.id = 'welcome-view'; root.setAttribute('aria-label', 'Welcome to aimebu');
  document.body.append(root);
  let finished, page, stopStage;
  const done = new Promise(resolve => { finished = resolve; });
  const store = new WelcomeStore({ onContinue, reduced, finishMs, onDone: () => { destroy(); finished(); } });
  const skip = event => store.skip(event);
  const destroy = () => { document.removeEventListener('keydown', skip); root.removeEventListener('click', skip); stopStage(); store.destroy(); page.destroy(); root.remove(); };
  stopStage = effect(() => { root.dataset.stage = store.stage.value; });
  page = mountPreactPage(root, () => html`<${Welcome} store=${store} />`, { onError: error => console.error('welcome:', error) });
  store.flush = () => page.update({});
  document.addEventListener('keydown', skip); root.addEventListener('click', skip);
  return { done, destroy, store };
}

export const showWelcome = (onContinue, options) => openWelcome(onContinue, options).done;
