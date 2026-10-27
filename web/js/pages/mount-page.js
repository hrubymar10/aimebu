import { html, render } from '../ui.js';
import { ErrorBoundary } from '../components/error-boundary.js';

// The handle a page's mount() gives the slot. `Page` gets openToken, idPrefix, embedded, the props the slot sends, onDirty and onView
// (an object of view props). Clear empties the section until the next open; callbacks of an earlier or destroyed tree are ignored.
export function mountPreactPage(section, Page, { onError = () => {}, idPrefix = '', embedded = false } = {}) {
  let generation = 0, alive = true, cleared = false, openToken = 0, props = {}, opening = {}, view = {};
  const unsaved = new Set();
  const callbacks = () => {
    const at = generation, live = () => alive && at === generation;
    let failed = false;
    return {
      onDirty: (key, dirty) => { if (!live()) return; if (dirty) unsaved.add(key); else unsaved.delete(key); },
      onView: next => { if (live()) view = next; },
      // Reported after the boundary's own render: falling back unmounts this tree, which must not happen inside Preact's catch.
      onError: error => { if (failed || !live()) return; failed = true; queueMicrotask(() => { if (live()) onError(error); }); },
    };
  };
  let current = callbacks();
  const draw = () => {
    if (!alive || cleared) return;
    render(html`<${ErrorBoundary} key=${generation} onError=${current.onError}><${Page} ...${opening} ...${props} onDirty=${current.onDirty} onView=${current.onView} idPrefix=${idPrefix} embedded=${embedded} openToken=${openToken} /><//>`, section);
  };
  draw();
  return {
    unsaved,
    update(next) { props = { ...props, ...next }; draw(); },
    get: name => props[name],
    open() {
      cleared = false;
      openToken++;
      opening = {}; // view props such as a machine to open on are used by this open only
      for (const name of Object.keys(props)) if (typeof props[name] !== 'function') { opening[name] = props[name]; delete props[name]; }
      draw();
    },
    clear() { generation++; cleared = true; openToken = 0; opening = {}; view = {}; unsaved.clear(); current = callbacks(); render(null, section); },
    destroy() { alive = false; generation++; unsaved.clear(); render(null, section); },
    hasUnsavedChanges: () => unsaved.size > 0,
    viewSnapshot: () => ({ ...view }),
  };
}
