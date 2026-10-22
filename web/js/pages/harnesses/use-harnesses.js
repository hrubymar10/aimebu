import { useState, useEffect, useLayoutEffect, useRef } from '../../ui.js';
import { request } from '../../api.js';
import { useAlive } from '../../hooks/use-alive.js';
import { launcherReady } from '../../harness-offer.js';
import { harnessFromLauncher, createBody, initialModels, detectNotes, detectionFailure, machineSummary } from './model.js';

const launchersURL = machineID => `/api/machines/${encodeURIComponent(machineID)}/launchers`;

// Every read carries a generation and every page action the view (machine and opening) it started in, so a late answer
// for another machine, an earlier opening or an unmounted page is dropped.
export function useHarnesses({ openToken, wanted, closePanel, onReady = () => {} }) {
  const alive = useAlive();
  const generation = useRef(0);
  const view = useRef(0);
  const current = token => alive() && token === view.current;
  const machinesRef = useRef([]), machineRef = useRef(''), shownRef = useRef('');
  const [machines, setMachines] = useState([]);
  const [machineID, setMachineID] = useState('');
  const [summary, setSummary] = useState('');
  const [error, setErrorText] = useState('');
  const [errorDetail, setErrorDetail] = useState('');
  const setError = (text, detail = '') => {
    setErrorText(text);
    setErrorDetail(detail);
  };
  const [toolbarHidden, setToolbarHidden] = useState(false);
  const [rows, setRows] = useState(null);
  const [activity, setActivity] = useState(null);
  const [detecting, setDetecting] = useState(false);
  const [modelFailures, setModelFailures] = useState(() => new Map());
  useLayoutEffect(() => () => { generation.current++; }, []);

  // A read for a machine that is no longer selected does nothing: it must not cancel or clear the selected one's list.
  const load = async (id = machineRef.current, { keepError = false } = {}) => {
    if (id !== machineRef.current || !alive()) return false;
    const ticket = ++generation.current;
    if (!keepError) setError('');
    if (shownRef.current !== id) {
      setRows(null);
      setActivity(null);
      shownRef.current = '';
    }
    if (!id) return false;
    setSummary(machineSummary(machinesRef.current.find(item => item.id === id)));
    try {
      const response = await request(launchersURL(id));
      if (ticket !== generation.current || machineRef.current !== id || !alive()) return false;
      shownRef.current = id;
      const list = (response?.launchers || []).map(harnessFromLauncher);
      setRows(list);
      if (list.some(row => launcherReady(row.enabled, row.models))) onReady();
      setActivity(response?.activity || []);
      return true;
    } catch (problem) {
      if (ticket === generation.current && alive()) setError(problem.message || 'Could not load harnesses');
      return false;
    }
  };

  const open = async preferred => {
    view.current++;
    const ticket = ++generation.current;
    setError('');
    setSummary('Loading machines…');
    setRows(null);
    setActivity(null);
    closePanel();
    shownRef.current = '';
    try {
      const answer = await request('/api/machines');
      if (ticket !== generation.current || !alive()) return;
      const list = Array.isArray(answer) ? answer : answer?.machines || [];
      let selected = list.some(machine => machine.id === machineRef.current) ? machineRef.current : list[0]?.id || '';
      if (preferred && list.some(machine => machine.id === preferred)) selected = preferred;
      machinesRef.current = list;
      machineRef.current = selected;
      setMachines(list);
      setMachineID(selected);
      setToolbarHidden(!list.length);
      if (!list.length) {
        setSummary('No machines are registered.');
        return;
      }
      await load(selected);
    } catch (problem) {
      if (ticket === generation.current && alive()) {
        setSummary('');
        setError(problem.message || 'Could not load machines');
      }
    }
  };
  useEffect(() => { if (openToken) open(wanted); }, [openToken]);

  const chooseMachine = id => {
    view.current++;
    machineRef.current = id;
    setMachineID(id);
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
        if (current(token)) setModelFailures(failures => new Map(failures).set(harnessID, failure));
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

  // Resolves to what auto-detect found, or null when the page moved on or the request failed.
  const detect = async () => {
    const id = machineRef.current, token = view.current;
    setError('');
    setDetecting(true);
    try {
      const found = await request(`${launchersURL(id)}/detect`, { method: 'POST', body: {} });
      return current(token) ? { machineID: id, candidates: found.candidates || [] } : null;
    } catch (problem) {
      if (current(token)) setError(problem.message || "Couldn't detect harnesses");
      return null;
    } finally {
      if (alive()) setDetecting(false);
    }
  };
  const addDetected = async (id, chosen) => {
    const token = view.current, created = [];
    setErrorDetail('');
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

  const setModelFailure = (id, reason) => setModelFailures(failures => {
    const next = new Map(failures);
    if (reason) next.set(id, reason);
    else next.delete(id);
    return next;
  });
  const patchRow = (id, changes) => setRows(list => list?.map(item => item.id === id ? { ...item, ...changes } : item));

  return {
    alive, machines, machineID, summary, error, errorDetail, setError, toolbarHidden, rows, activity, detecting, modelFailures,
    load, chooseMachine, detect, addDetected, addByHand, setModelFailure, patchRow,
  };
}
