import { useEffect, useLayoutEffect, useMemo, useRef, signal, batch } from '../../ui.js';
import { request } from '../../api.js';
import { useAlive } from '../../hooks/use-alive.js';
import { launcherReady } from '../../harness-offer.js';
import { RoleDefaults, ROLE_DEFAULTS_CHANGED } from '../../role-defaults.js';
import { harnessFromLauncher, createBody, initialModels, detectNotes, detectionFailure, machineSummary, chosenHarness, staleModelsMessage, staleProvidersMessage, defaultLabel } from './model.js';

export const anyReady = rows => rows.some(row => launcherReady(row.enabled, row.models));

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
  // The harness whose detail is shown beside the list.
  selected: signal(''),
  // The global roles with their default agents, and why they could not be read or saved.
  roles: signal([]),
  roleError: signal(''),
  // Harnesses of the other machines that a role default names: launcher id -> { machine, label, models }.
  others: signal(new Map()),
  modelFailures: signal(new Map()),
  // Unsaved edits by harness id ({ form, models }), dropped with the list they belong to.
  drafts: signal(new Map()),
});

// Answers for another machine, an earlier opening or an unmounted page are dropped (generation and view tokens).
export function useHarnesses({ openToken, wanted, closePanel, onReady = () => {}, onOpened = () => {}, isHidden = () => false }) {
  const alive = useAlive();
  const state = useMemo(createState, []);
  const defaults = useMemo(() => new RoleDefaults(), []);
  // The unknown launcher ids the other machines were last read for, so the same ones are not asked for twice.
  const otherTried = useRef('');
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
        state.selected.value = chosenHarness(list, state.selected.peek());
        state.activity.value = response?.activity || [];
      });
      if (anyReady(list)) onReady();
      loadOtherHarnesses();
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
      state.roleError.value = '';
      state.summary.value = 'Loading machines…';
      state.others.value = new Map();
      otherTried.current = '';
      clearList();
    });
    closePanel();
    try {
      const [answer] = await Promise.all([request('/api/machines'), loadRoles()]);
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
  // A layout effect, so the reads start in the render that opens the page, not a frame later: the onboarding step's layout moves when they answer.
  useLayoutEffect(() => { if (openToken) open(wanted).then(onOpened); }, [openToken]);

  const chooseMachine = id => {
    view.current++;
    state.machineID.value = id;
    otherTried.current = '';
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
  const selectHarness = id => { state.selected.value = id; };
  // `update` is a value or a function of the current one, so two edits before the next render both count.
  const setDraft = (id, kind, update) => {
    const next = new Map(state.drafts.peek());
    const entry = { ...next.get(id) };
    entry[kind] = typeof update === 'function' ? update(entry[kind] ?? null) : update;
    next.set(id, entry);
    state.drafts.value = next;
  };

  // The list changed since a draft was started (another tab, another person): drop that draft, not the harness's Edit form, show what is
  // stored now, and say so.
  const staleModels = async harness => {
    const id = state.machineID.peek();
    setDraft(harness.id, 'models', null);
    await load(id);
    if (alive() && state.machineID.peek() === id) setError(staleModelsMessage(harness));
  };

  // Same for the providers: a list saved elsewhere meanwhile is reloaded, the draft of it dropped, and the person told.
  const staleProviders = async harness => {
    const id = state.machineID.peek();
    setDraft(harness.id, 'providers', null);
    await load(id);
    if (alive() && state.machineID.peek() === id) setError(staleProvidersMessage(harness));
  };

  // The roles are the store's list, read and saved through the same /api/roles as Settings -> Roles; the signal holds a copy to draw from.
  const syncRoles = () => { state.roles.value = [...defaults.roles]; };
  async function loadRoles() {
    try {
      await defaults.load();
    } catch (problem) {
      defaults.roles = [];
      if (alive()) state.roleError.value = problem.message || 'Could not load the roles';
    }
    if (alive()) syncRoles();
  }
  // A role default names its harness and model by ids. On the machine shown they are read from the harnesses already loaded; defaults on
  // other machines need those machines' harnesses once, and only when some role has such a default. They are not asked for again for the
  // same unknown ids: a redraw repeats nothing, a harness that appears later (or a machine change) is looked for once more.
  async function loadOtherHarnesses() {
    const known = id => state.rows.peek()?.some(row => row.id === id) || state.others.peek().has(id);
    const missing = [...new Set(state.roles.peek().flatMap(role => (role.default_agents || []).map(item => item.launcher_id)))].filter(id => !known(id)).sort();
    const key = missing.join(',');
    if (!missing.length || key === otherTried.current) return;
    otherTried.current = key;
    const machineID = state.machineID.peek();
    const found = new Map();
    await Promise.all(state.machines.peek().filter(machine => machine.id !== machineID).map(async machine => {
      try {
        const response = await request(launchersURL(machine.id));
        for (const row of response?.launchers || []) found.set(row.config.id, { machine: machine.name || machine.id, label: row.config.label || row.config.name, models: row.models || [] });
      } catch { /* those entries are then named by their ids */ }
    }));
    if (alive() && state.machineID.peek() === machineID) state.others.value = new Map([...state.others.peek(), ...found]);
  }
  const roleChanged = () => {
    syncRoles();
    loadOtherHarnesses();
  };
  const labelOfDefault = item => {
    const row = state.rows.value?.find(entry => entry.id === item.launcher_id);
    const machine = state.machines.value.find(entry => entry.id === state.machineID.value);
    return defaultLabel(item, row && { harness: row, machine: machine?.name || machine?.id }, state.others.value.get(item.launcher_id));
  };
  // Role defaults saved from Settings -> Roles show here without reopening the page; ours, and any while the page is hidden, are skipped.
  // A layout effect, so a destroyed page stops listening at once: a plain effect's cleanup may wait for the next paint.
  useLayoutEffect(() => {
    const listener = event => {
      if (event.detail?.source === defaults || isHidden()) return;
      loadRoles().then(() => alive() && loadOtherHarnesses());
    };
    document.addEventListener(ROLE_DEFAULTS_CHANGED, listener);
    return () => document.removeEventListener(ROLE_DEFAULTS_CHANGED, listener);
  }, []);

  return {
    state, alive, load, chooseMachine, detect, addDetected, addByHand, defaults, roleChanged, labelOfDefault,
    setModelFailure, patchRow, setFormOpen, selectHarness, setDraft, staleModels, staleProviders,
  };
}
