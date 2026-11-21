import { html, useEffect, useState } from './ui.js';
import { captureError } from './telemetry.js';

// Lazily fetched parts of the app (chunks of the bundle, see internal/server/web_bundle.go). A Lazy holds the loader of a module's object and the calls the
// app makes on it before it has arrived: they are queued and replayed, in order, when it is made (a call under a `key` replaces the earlier one with that
// key, as a "set the room" call only matters in its last form). `prefetch` fetches the chunk when the browser is idle, so that the first use finds it.
// A chunk that cannot be fetched is most often one a newer build replaced (the server that answers now does not have the old build's chunks): the page's own
// handler (app.js) asks which build the server is, and reloads, offers the reload, or says the part could not be loaded. It is never silent.
let chunkFailureHandler = null;
export function onChunkFailure(handler) { chunkFailureHandler = handler; }
export function reportChunkFailure(error, label) {
  console.error(`could not load ${label}:`, error);
  captureError(error, `lazy.${label}`);
  try { if (chunkFailureHandler) void Promise.resolve(chunkFailureHandler(error, label)).catch(() => {}); } catch { /* the handler failing is not a second failure to report */ }
}

// What a person's session may have queued must not outlive it: the queues are dropped, and a call made for an earlier session does not run, when the
// session ends (app.js calls endLazySession), so a new sign-in never finds the last one's "open" waiting.
let generation = 0;
const registry = new Set();
export const lazySession = () => generation;
export function endLazySession() { generation++; for (const lazy of registry) lazy.pending.clear(); }

export class Lazy {
  constructor(loader, label) {
    this.loader = loader; this.label = label;
    this.instance = null; this.promise = null;
    this.pending = new Map();
    this.quietUntil = 0;
    registry.add(this);
  }
  // The object, made once; a failed fetch is reported and tried again by the next call.
  load() {
    this.promise ||= this.loader().then(instance => {
      this.instance = instance;
      const queued = [...this.pending.values()]; this.pending.clear();
      for (const call of queued) { try { call(instance); } catch (error) { captureError(error, `lazy.${this.label}`); } }
      return instance;
    }, error => { this.promise = null; throw error; });
    return this.promise;
  }
  // Runs call(instance) now, or once the object is made; `load: false` queues it without fetching the chunk (it runs if the chunk is fetched for another reason).
  // `background` is for a load nobody asked for (a timer's refresh, a sign-in's catch-up): a failure of it is not reported or shown, and the chunk is not asked
  // for again for 30 s (the next call after that retries); a person's own use of the screen reports.
  run(call, { key = Symbol(), load = true, background = false } = {}) {
    if (this.instance) return call(this.instance);
    if (background && Date.now() < this.quietUntil) return undefined;
    const session = generation;
    this.pending.set(key, instance => { if (session === generation) call(instance); });
    if (load) this.load().catch(error => { if (background) this.quietUntil = Date.now() + 30000; else reportChunkFailure(error, this.label); });
    return undefined;
  }
  // Fetches the chunk, without making the object, when the browser is idle.
  prefetch(importer) { whenIdle(() => { void importer().catch(() => {}); }); }
}

export function whenIdle(task) {
  if (typeof requestIdleCallback === 'function') requestIdleCallback(task, { timeout: 4000 });
  else setTimeout(task, 1500);
}

// Makes a button's click start the lazily made object and then press the button as the object set it up: the object's constructor replaces onclick
// (the bridge is only there until it does).
export function bridgeClick(button, lazy) {
  const bridge = () => { void lazy.load().then(() => { if (button.onclick && button.onclick !== bridge) button.onclick(); }).catch(error => reportChunkFailure(error, lazy.label)); };
  button.onclick = bridge;
}

// A component whose module is fetched when it is first drawn (nothing until then): `load` returns the component.
export function lazyComponent(load, label = 'component') {
  let Component = null, promise = null;
  return function LazyComponent(props) {
    const [, bump] = useState(0);
    useEffect(() => {
      if (Component) return undefined;
      let live = true;
      promise ||= load().then(loaded => { Component = loaded; }, error => { promise = null; reportChunkFailure(error, label); });
      promise.then(() => { if (live && Component) bump(count => count + 1); });
      return () => { live = false; };
    }, []);
    return Component ? html`<${Component} ...${props} />` : null;
  };
}
