// The first-run welcome: shown once to the owner, between their first sign-in and the app.
// The screen is four bands from top to bottom: the app icon over the first two, "aimebu v2" in the third and a Continue button in the
// fourth. Dots (agents) run in along a line from both sides and are taken into the icon, which pops; then the name and Continue arrive
// together. CSS does the animating; the timer here only reveals the finished state (data-stage) and moves focus to Continue. Reduced motion,
// a click, Enter or Space go straight to the finished screen.
const FINISH_MS = 2900;

function node(tag, className = '', text = '') { const item = document.createElement(tag); if (className) item.className = className; if (text) item.textContent = text; return item; }

// The app icon: the robot "a" on its rounded tile, in the light or the dark artwork to match the theme (two images, one shown at a time,
// as the header logo does). The tile is part of the artwork, so the icon never blends into the page, and it moves as one unit.
function iconLogo() {
  const group = node('span', 'welcome-logo');
  for (const theme of ['light', 'dark']) { const image = node('img', `welcome-icon welcome-icon-${theme}`); image.src = theme === 'light' ? '/icons/icon-512.png' : '/icons/icon-512-dark.png'; image.alt = ''; image.width = 512; image.height = 512; group.append(image); }
  return group;
}

// The agents that arrive: a line, and dots that travel along it from both sides toward the icon.
function busArt(art) {
  art.append(node('span', 'welcome-line'));
  const starts = [[-46, -9], [-40, 0], [-46, 9], [46, -9], [40, 0], [46, 9]];
  starts.forEach(([x, y], index) => { const dot = node('span', 'welcome-dot'); dot.style.setProperty('--sx', `${x}vw`); dot.style.setProperty('--sy', `${y}vh`); dot.style.setProperty('--i', String(index)); art.append(dot); });
}

export function showWelcome(onContinue, { reduced = matchMedia('(prefers-reduced-motion: reduce)').matches } = {}) {
  return new Promise(resolve => {
    const root = node('main', 'welcome'); root.id = 'welcome-view'; root.setAttribute('aria-label', 'Welcome to aimebu'); root.dataset.stage = 'logo';
    const art = node('div', 'welcome-art');
    busArt(art); art.append(iconLogo());
    const title = node('h1', 'welcome-title'); title.append(node('span', 'welcome-name', 'aimebu'), node('span', 'welcome-version', ' v2'));
    const proceed = node('button', 'btn btn-primary welcome-continue', 'Continue'); proceed.type = 'button'; proceed.id = 'welcome-continue'; proceed.inert = true;
    root.append(art, title, proceed);
    let timer = 0;
    const finish = () => {
      clearTimeout(timer);
      root.dataset.stage = 'done'; proceed.inert = false;
      // Focus lands on Continue so Enter works, but a ring on a button nobody navigated to is noise: it shows only once the person uses Tab.
      proceed.dataset.autoFocus = ''; proceed.focus();
    };
    const skip = event => {
      // The welcome covers the app, so Tab stays on Continue (the only control) instead of walking the page behind it; being focused by a key
      // press, it now shows the keyboard ring.
      if (event.type === 'keydown' && event.key === 'Tab') { event.preventDefault(); if (root.dataset.stage === 'done') { delete proceed.dataset.autoFocus; proceed.blur(); proceed.focus(); } return; }
      if (root.dataset.stage === 'done') return;
      if (event.type === 'keydown' && event.key !== 'Enter' && event.key !== ' ') return;
      event.preventDefault(); finish();
    };
    let closed = false;
    proceed.onclick = async () => {
      if (closed) return; closed = true; proceed.disabled = true;
      try { await onContinue?.(); } catch { /* the welcome would come back next sign-in; nothing to block on */ }
      document.removeEventListener('keydown', skip); root.removeEventListener('click', skip); root.remove(); resolve();
    };
    document.addEventListener('keydown', skip); root.addEventListener('click', skip);
    document.body.append(root);
    if (reduced) { finish(); return; }
    timer = setTimeout(finish, FINISH_MS);
  });
}
