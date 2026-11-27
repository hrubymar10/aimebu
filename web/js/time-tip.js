// The tooltip for a time: any element carrying `data-time-tip` (the absolute date a relative time stands for, from time.js's absoluteTime) shows
// it in the same bubble the project status pill uses, on hover and keyboard focus, and on a touch long press. One listener set on the document, so
// vanilla-DOM and Preact rows alike keep the tooltip through every live text update (the attribute is never rewritten by a refresh).
const HOLD_MS = 500, SHOW_MS = 2500;
let bubble = null, touchTimer = 0, hideTimer = 0;

const targetOf = event => event.target instanceof Element ? event.target.closest('[data-time-tip]') : null;

function hide() {
  clearTimeout(touchTimer); clearTimeout(hideTimer);
  bubble?.remove(); bubble = null;
}

function show(anchor) {
  hide();
  bubble = document.createElement('span');
  bubble.className = 'project-status-tooltip time-tip';
  bubble.setAttribute('role', 'tooltip');
  bubble.textContent = anchor.dataset.timeTip;
  document.body.append(bubble);
  const rect = anchor.getBoundingClientRect(), tip = bubble.getBoundingClientRect();
  bubble.style.left = `${Math.max(6, Math.min(rect.left, window.innerWidth - tip.width - 6))}px`;
  bubble.style.top = `${rect.bottom + tip.height + 6 <= window.innerHeight ? rect.bottom + 4 : Math.max(6, rect.top - tip.height - 4)}px`;
}

export function installTimeTips(doc = document) {
  if (doc.__timeTips) return;
  doc.__timeTips = true;
  doc.addEventListener('mouseover', event => { const anchor = targetOf(event); if (anchor) show(anchor); });
  doc.addEventListener('mouseout', event => { if (targetOf(event)) hide(); });
  doc.addEventListener('focusin', event => { const anchor = targetOf(event); if (anchor) show(anchor); });
  doc.addEventListener('focusout', event => { if (targetOf(event)) hide(); });
  doc.addEventListener('touchstart', event => {
    const anchor = targetOf(event);
    if (!anchor) { hide(); return; }
    clearTimeout(touchTimer);
    touchTimer = setTimeout(() => { show(anchor); hideTimer = setTimeout(hide, SHOW_MS); }, HOLD_MS);
  }, { passive: true });
  for (const name of ['touchend', 'touchcancel', 'touchmove']) doc.addEventListener(name, () => clearTimeout(touchTimer), { passive: true });
  doc.addEventListener('scroll', hide, true);
}

installTimeTips();
