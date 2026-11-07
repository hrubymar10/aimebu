// Which way the person last used the page: with a pointer (mouse, touch, pen) or with the keyboard. The focus ring on a menu item is for the keyboard: a
// menu opened with a click moves focus to its first item so the arrow keys work, and some browsers draw the ring there as if a key had been pressed.
// CSS reads `data-input` on <html> (see v2-only.css) to leave that ring off while the last input was a pointer.
export function trackInputModality(root = document) {
  const set = value => { if (root.documentElement.dataset.input !== value) root.documentElement.dataset.input = value; };
  root.addEventListener('pointerdown', () => set('pointer'), true);
  root.addEventListener('keydown', event => { if (!event.metaKey && !event.ctrlKey && !event.altKey || event.key === 'Tab') set('keyboard'); }, true);
}
