import { useEffect, useLayoutEffect, useMemo, useRef, signal, batch } from '../../ui.js';
import { request } from '../../api.js';
import { useAlive } from '../../hooks/use-alive.js';
import { launcherReady } from '../../harness-offer.js';
import { harnessFromLauncher, createBody, initialModels, detectNotes, detectionFailure, machineSummary } from './model.js';

const launchersURL = machineID => `/api/machines/${encodeURIComponent(machineID)}/launchers`;
const toggled = (set, id, open) => {
  const next = new Set(set);
  if (open) next.add(id);
  else next.delete(id);
  return next;
};

const createState = () => ({
  machines: signal([]),
  machineID: signal(''),
  summary: signal(''),
  error: signal(''),
  errorDetail: signal(''),
  toolbarHidden: signal(false),
  rows: signal(null),
  activity: signal(null),
  detecting: signal(false),
  openForms: signal(new Set()),
  openModels: signal(new Set()),
  modelFailures: signal(new Map()),
  // Unsaved edits by harness id ({ form, models }), dropped with the list they belong to.
  drafts: signal(new Map()),
});

// Answers for another machine, an earlier opening or an unmounted page are dropped (generation and view tokens).
export function useHarnesses({ openToken, wanted, closePanel, onReady = () => {} }) {
  const alive = useAlive();
  const state = useMemo(createState, []);
  const generation = useRef(0);
  const view = useRef(0);
  const current = token => alive() && token === view.current;
  const shownRef = useRef('');
  const setError = (text, detail = '') => batch(() => {
    state.error.value = text;
    state.errorDetail.value = detail;
  });
  useLayoutEffect(() => () => { generation.current++; }, []);

  const clearList = () => batch(() => {
    state.rows.value = null;
    state.activity.value = null;
    state.drafts.value = new Map();
    shownRef.current = '';
  });

  // A read for a machine that is no longer selected does nothing: it must not cancel or clear the selected one's list.
  const load = async (id = state.machineID.peek(), { keepError = false } = {}) => {
    if (id !== state.machineID.peek() || !alive()) return false;
    const ticket = ++generation.current;
    if (!keepError) setError('');
    if (shownRef.current !== id) clearList();
    if (!id) return false;
    state.summary.value = machineSummary(state.machines.peek().find(item => item.id === id));
    try {
      const response = await request(launchersURL(id));
      if (ticket !== generation.current || state.machineID.peek() !== id || !alive()) return false;
      shownRef.current = id;
      const list = (response?.launchers || []).map(harnessFromLauncher);
      batch(() => {
        state.rows.value = list;
        state.activity.value = response?.activity || [];
      });
      if (list.some(row => launcherReady(row.enabled, row.models))) onReady();
      return true;
    } catch (problem) {
      if (ticket === generation.current && alive()) setError(problem.message || 'Could not load harnesses');
      return false;
    }
  };

  const open = async preferred => {
    view.current++;
    const ticket = ++generation.current;
    batch(() => {
      setError('');
      state.summary.value = 'Loading machines…';
      clearList();
    });
    closePanel();
    try {
      const answer = await request('/api/machines');
      if (ticket !== generation.current || !alive()) return;
      const list = Array.isArray(answer) ? answer : answer?.machines || [];
      const previous = state.machineID.peek();
      let selected = list.some(machine => machine.id === previous) ? previous : list[0]?.id || '';
      if (preferred && list.some(machine => machine.id === preferred)) selected = preferred;
      batch(() => {
        state.machines.value = list;
        state.machineID.value = selected;
        state.toolbarHidden.value = !list.length;
      });
      if (!list.length) {
        state.summary.value = 'No machines are registered.';
        return;
      }
      await load(selected);
    } catch (problem) {
      if (ticket === generation.current && alive()) {
        batch(() => {
          state.summary.value = '';
          setError(problem.message || 'Could not load machines');
        });
      }
    }
  };
  useEffect(() => { if (openToken) open(wanted); }, [openToken]);

  const chooseMachine = id => {
    view.current++;
    state.machineID.value = id;
    closePanel();
    load(id);
  };

  const createHarness = (id, fields) => request(launchersURL(id), { method: 'POST', body: createBody(fields) });
  const fillModels = async (id, harnessID, token, containerized) => {
    if (!harnessID) return { saved: false, cut: 0 };
    const base = `${launchersURL(id)}/${encodeURIComponent(harnessID)}/models`;
    try {
      let found;
      try {
        found = await request(`${base}/detect`, { method: 'POST', body: {} });
      } catch (problem) {
        const failure = detectionFailure(problem, containerized);
        if (current(token)) setModelFailure(harnessID, failure);
        return { saved: false, cut: 0, reason: failure.summary, detail: failure.detail };
      }
      const { models, cut } = initialModels(found.candidates);
      if (!models.length) return { saved: false, cut: 0 };
      await request(base, { method: 'PUT', body: { models } });
      return { saved: true, cut, kept: models.length };
    } catch (problem) {
      return { saved: false, cut: 0, reason: problem.message || '' };
    }
  };

  const detect = async () => {
    const id = state.machineID.peek(), token = view.current;
    batch(() => {
      setError('');
      state.detecting.value = true;
    });
    try {
      const found = await request(`${launchersURL(id)}/detect`, { method: 'POST', body: {} });
      return current(token) ? { machineID: id, candidates: found.candidates || [] } : null;
    } catch (problem) {
      if (current(token)) setError(problem.message || "Couldn't detect harnesses");
      return null;
    } finally {
      if (alive()) state.detecting.value = false;
    }
  };
  const addDetected = async (id, chosen) => {
    const token = view.current, created = [];
    state.errorDetail.value = '';
    for (const candidate of chosen) {
      created.push(await createHarness(id, { tool: candidate.family, label: candidate.label, command: candidate.command, containerized: candidate.containerized }));
    }
    const filled = await Promise.all(created.map(item => fillModels(id, item?.config?.id || item?.id, token, !!item?.config?.containerized)));
    if (!current(token)) return;
    closePanel();
    if (await load(id) && current(token)) setError(detectNotes(filled), filled.find(done => !done.saved && done.detail)?.detail || '');
  };
  const addByHand = async (id, fields) => {
    const token = view.current;
    await createHarness(id, fields);
    if (!current(token)) return;
    closePanel();
    await load(id);
  };

  const setModelFailure = (id, reason) => {
    const next = new Map(state.modelFailures.peek());
    if (reason) next.set(id, reason);
    else next.delete(id);
    state.modelFailures.value = next;
  };
  const patchRow = (id, changes) => {
    state.rows.value = state.rows.peek()?.map(item => item.id === id ? { ...item, ...changes } : item);
  };
  const setFormOpen = (id, open) => { state.openForms.value = toggled(state.openForms.peek(), id, open); };
  const setModelsOpen = (id, open) => { state.openModels.value = toggled(state.openModels.peek(), id, open); };
  // `update` is a value or a function of the current one, so two edits before the next render both count.
  const setDraft = (id, kind, update) => {
    const next = new Map(state.drafts.peek());
    const entry = { ...next.get(id) };
    entry[kind] = typeof update === 'function' ? update(entry[kind] ?? null) : update;
    next.set(id, entry);
    state.drafts.value = next;
  };

  return {
    state, alive, load, chooseMachine, detect, addDetected, addByHand,
    setModelFailure, patchRow, setFormOpen, setModelsOpen, setDraft,
  };
}
