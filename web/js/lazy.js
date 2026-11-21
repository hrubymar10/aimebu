import { html, useEffect, useState } from './ui.js';
import { captureError } from './telemetry.js';

// Lazily fetched parts of the app (chunks of the bundle, see internal/server/web_bundle.go). A Lazy holds the loader of a module's object and the calls the
// app makes on it before it has arrived: they are queued and replayed, in order, when it is made (a call under a `key` replaces the earlier one with that
// key, as a "set the room" call only matters in its last form). `prefetch` fetches the chunk when the browser is idle, so that the first use finds it.
export class Lazy {
  constructor(loader, label) {
    this.loader = loader; this.label = label;
    this.instance = null; this.promise = null;
    this.pending = new Map();
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
  run(call, { key = Symbol(), load = true } = {}) {
    if (this.instance) return call(this.instance);
    this.pending.set(key, call);
    if (load) this.load().catch(error => captureError(error, `lazy.${this.label}`));
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
  const bridge = () => { void lazy.load().then(() => { if (button.onclick && button.onclick !== bridge) button.onclick(); }).catch(error => captureError(error, `lazy.${lazy.label}`)); };
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
      promise ||= load().then(loaded => { Component = loaded; }, error => { promise = null; captureError(error, `lazy.${label}`); });
      promise.then(() => { if (live && Component) bump(count => count + 1); });
      return () => { live = false; };
    }, []);
    return Component ? html`<${Component} ...${props} />` : null;
  };
}
