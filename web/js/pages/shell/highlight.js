// Brings a setting into view and marks it for a moment: the row (or card) around the control the registry names.
const HOLD = 1800;

const rowOf = element => element.closest('.settings-row, .prompt-slot, .settings-group, .settings-subsection') || element;

// Finds the element of a registry entry in `root`: its anchor id, or the first id that fits its template (a quota row is one per tool).
export function findAnchor(root, entry) {
  if (entry.selector) return root.querySelector(entry.selector);
  if (entry.anchor) { const found = root.querySelector(`#${CSS.escape(entry.anchor)}`); if (found) return found; }
  if (entry.template) {
    const pattern = new RegExp(`^${entry.template.replace(/[.*+?^()|[\]\\]/g, '\\$&').replace(/\$\{[^}]*\}/g, '.+')}$`);
    for (const element of root.querySelectorAll('[id]')) if (pattern.test(element.id)) return element;
  }
  return null;
}

// Waits (a page draws a moment after it is shown) for the anchor, scrolls to it and flashes it. A page that draws again as its data arrives replaces the row, so the
// mark is put on the row that is there a moment later too. Resolves with the element, or null when it never came.
export function reveal(root, entry, { tries = 20, wait = 50 } = {}) {
  const mark = row => {
    row.classList.add('settings-flash');
    setTimeout(() => row.classList.remove('settings-flash'), HOLD);
  };
  return new Promise(resolve => {
    const attempt = left => {
      const anchor = findAnchor(root, entry);
      if (!anchor) { if (left > 0) setTimeout(() => attempt(left - 1), wait); else resolve(null); return; }
      let row = rowOf(anchor);
      row.scrollIntoView({ block: 'center', behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth' });
      mark(row);
      for (const later of [250, 700]) {
        setTimeout(() => {
          const again = findAnchor(root, entry), next = again && rowOf(again);
          if (next && next !== row && next.isConnected) { row = next; next.scrollIntoView({ block: 'center' }); mark(next); }
        }, later);
      }
      resolve(row);
    };
    attempt(tries);
  });
}
