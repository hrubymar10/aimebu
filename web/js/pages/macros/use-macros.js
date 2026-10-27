import { useEffect, useLayoutEffect, useMemo, useRef, signal, batch } from '../../ui.js';
import { request, rights } from '../../api.js';
import { copyText } from '../../markdown.js';
import { mergeMacros, parseMacroImport, describeMerge, isMacroKey, MACRO_BODY_MAX } from '../../macro-import.js';
import { useAlive } from '../../hooks/use-alive.js';
import { blankForm, saveChange, removeChange, importSummary, EMPTY_IMPORT } from './model.js';

const createState = () => ({
  macros: signal({}),
  // A read succeeded since the last open. Every write sends the whole map, so one built from a map that failed to load would wipe the rest.
  loaded: signal(false),
  canEdit: signal(false),
  busy: signal(false),
  error: signal(''),
  filter: signal(''),
  form: signal(blankForm()),
  editing: signal(null),
  importOpen: signal(false),
  importText: signal(''),
  summary: signal({ text: '', error: false }),
  pending: signal(null),
  copyLabel: signal('Copy all'),
});

// Reads carry a generation, so only the latest one draws. Writes carry the opening they started in; one answered after a reopen reports
// nothing and reads the map again, because the reopen's read may have been answered before the write landed.
export function useMacros({ openToken }) {
  const alive = useAlive();
  const state = useMemo(createState, []);
  const generation = useRef(0);
  const opened = useRef(0);
  const copies = useRef(0);
  const copyTimer = useRef(0);
  useLayoutEffect(() => () => { generation.current++; clearTimeout(copyTimer.current); }, []);

  const dropPreview = () => batch(() => { state.pending.value = null; state.summary.value = { text: '', error: false }; });
  const toggleImport = open => batch(() => {
    state.importOpen.value = open;
    if (!open) state.importText.value = '';
    dropPreview();
  });

  const load = async () => {
    const ticket = ++generation.current;
    batch(() => { state.loaded.value = false; state.error.value = ''; });
    try {
      const macros = await request('/api/macros');
      if (ticket !== generation.current || !alive()) return;
      batch(() => {
        state.macros.value = macros || {};
        state.loaded.value = true;
        dropPreview();
        if (!state.canEdit.peek()) toggleImport(false);
      });
    } catch (failure) {
      if (ticket === generation.current && alive()) state.error.value = failure.message || 'Could not load macros';
    }
  };

  // Drafts go when the read is asked for, not when it answers, so text typed while it is on its way stays.
  const open = () => {
    opened.current++;
    copies.current++; clearTimeout(copyTimer.current);
    batch(() => {
      state.canEdit.value = rights.admin;
      state.editing.value = null; state.form.value = blankForm();
      toggleImport(false);
      state.copyLabel.value = 'Copy all';
    });
    load();
  };
  useEffect(() => { if (openToken) open(); }, [openToken]);

  // One write at a time, and every control that could change what it sends is locked until it settles.
  const write = async (next, done, failed) => {
    const token = opened.current;
    batch(() => { state.error.value = ''; state.busy.value = true; });
    try {
      let saved;
      try { saved = await request('/api/macros', { method: 'PUT', body: next }); } catch (failure) {
        if (!alive()) return;
        if (token === opened.current) failed(failure.message || 'Could not save');
        else await load();
        return;
      }
      if (!alive()) return;
      if (token !== opened.current) { await load(); return; }
      batch(() => { state.macros.value = saved; dropPreview(); done(); });
    } finally { if (alive()) state.busy.value = false; }
  };
  const writable = () => state.loaded.peek() && state.canEdit.peek() && !state.busy.peek();
  const resetForm = () => batch(() => { state.editing.value = null; state.form.value = blankForm(); });

  const save = () => {
    if (!writable()) return;
    const editing = state.editing.peek(), form = state.form.peek(), key = form.key.trim().toLowerCase();
    const change = saveChange(state.macros.peek(), editing, key, form.body, { validKey: isMacroKey(key), withinSize: new TextEncoder().encode(form.body).length <= MACRO_BODY_MAX });
    state.error.value = change.error || '';
    if (change.error) return;
    write(change.next, resetForm, message => { state.error.value = message; });
  };

  const remove = key => {
    if (!writable()) return;
    write(removeChange(state.macros.peek(), key), () => { if (state.editing.peek() === key) resetForm(); }, message => { state.error.value = message; });
  };

  const edit = key => batch(() => { state.editing.value = key; state.form.value = { key, body: state.macros.peek()[key] }; });

  const setImportText = value => batch(() => { state.importText.value = value; dropPreview(); });
  const preview = () => {
    const text = state.importText.peek().trim();
    let result = EMPTY_IMPORT;
    if (text) {
      try { const parsed = parseMacroImport(text); result = importSummary(parsed, describeMerge(state.macros.peek(), parsed.macros)); }
      catch (failure) { result = { text: failure.message, error: true, pending: null }; }
    }
    batch(() => { state.summary.value = { text: result.text, error: result.error }; state.pending.value = result.pending; });
  };
  const applyImport = () => {
    const pending = state.pending.peek();
    if (!pending || !writable()) return;
    write(mergeMacros(state.macros.peek(), pending), () => toggleImport(false), message => { state.summary.value = { text: message, error: true }; });
  };

  const copyAll = async () => {
    const copy = ++copies.current;
    state.error.value = '';
    let text = 'Copied';
    try { await copyText(JSON.stringify(state.macros.peek(), null, 2)); } catch { text = 'Copy failed'; }
    if (!alive() || copy !== copies.current) return;
    state.copyLabel.value = text;
    clearTimeout(copyTimer.current);
    copyTimer.current = setTimeout(() => { state.copyLabel.value = 'Copy all'; }, 2000);
  };

  return { state, save, remove, edit, resetForm, setImportText, preview, applyImport, toggleImport, copyAll };
}
