import { useEffect, useLayoutEffect, useMemo, useRef, signal, batch } from '../../ui.js';
import { request } from '../../api.js';
import { useAlive } from '../../hooks/use-alive.js';
import { scopeOptions, pickScope, keepDrafts, noEditText, savePath, saveBody, resetPath, savedText, resetText, CONFLICT_TEXT } from './model.js';

const createState = () => ({
  options: signal([['', 'Global (all projects)']]),
  scope: signal(''),
  // The last answer, with the scope it was read for; the rows drawn from it write only into that scope.
  view: signal(null),
  // Unsaved text by slot key, only where it differs from the text the box was drawn with.
  drafts: signal(new Map()),
  busy: signal(false),
  error: signal(''),
  status: signal(''),
  inline: signal('optional'),
  savedInline: signal('optional'),
  inlineSaving: signal(false),
});

// Reads carry a generation, so only the latest scope's answer draws. Writes carry the opening they started in: after a reopen, Clear or
// unmount their answer reports nothing and reads nothing again.
export function usePrompts({ openToken, projectContext }) {
  const alive = useAlive();
  const state = useMemo(createState, []);
  const generation = useRef(0);
  const opened = useRef(0);
  const inlineSaves = useRef(0);
  const current = token => alive() && token === opened.current;
  useLayoutEffect(() => () => { generation.current++; }, []);

  // Drafts are dropped when a read is asked for, not when it answers, so text typed while it is on its way stays. An inline-plans
  // value read before a save that succeeded since is older than what the page shows, so it is not drawn.
  const load = async (scope, { except = '' } = {}) => {
    const ticket = ++generation.current, saves = inlineSaves.current;
    batch(() => { state.error.value = ''; state.status.value = ''; });
    try {
      const answer = await request(`/api/prompts?project_id=${encodeURIComponent(scope)}`);
      if (ticket !== generation.current || !alive()) return false;
      batch(() => {
        state.drafts.value = keepDrafts(state.drafts.peek(), answer.slots, except);
        state.view.value = { ...answer, scope };
        if (saves === inlineSaves.current) {
          state.savedInline.value = answer.inline_plan_appendix;
          if (!state.inlineSaving.peek()) state.inline.value = answer.inline_plan_appendix;
        }
        state.status.value = answer.can_edit ? '' : noEditText(scope);
      });
      return true;
    } catch (failure) {
      if (ticket === generation.current && alive()) state.error.value = failure.message || 'Could not load prompts';
      return false;
    }
  };

  const open = () => {
    opened.current++;
    const { projects = [] } = projectContext?.() || {};
    const options = scopeOptions(projects);
    const scope = pickScope(options, state.scope.peek());
    batch(() => { state.options.value = options; state.scope.value = scope; state.drafts.value = new Map(); });
    load(scope);
  };
  useEffect(() => { if (openToken) open(); }, [openToken]);

  const chooseScope = scope => {
    batch(() => { state.scope.value = scope; state.drafts.value = new Map(); });
    load(scope);
  };

  const setDraft = (slot, value) => {
    const next = new Map(state.drafts.peek());
    if (value === slot.effective) next.delete(slot.key); else next.set(slot.key, value);
    state.drafts.value = next;
  };

  // One write at a time for the whole page. A failure other than a conflict leaves every draft as it is; otherwise the list is read again
  // with the drafts kept, the written slot's own included after a conflict.
  const change = async (slot, run, done, failedText) => {
    const token = opened.current, scope = state.view.peek().scope;
    batch(() => { state.error.value = ''; state.status.value = ''; state.busy.value = true; });
    try {
      let conflict = false;
      try { await run(scope); } catch (failure) {
        if (!current(token)) return;
        if (failure.status !== 409) { state.error.value = failure.message || failedText; return; }
        conflict = true;
      }
      if (!current(token)) return;
      await load(scope, { except: conflict ? '' : slot.key });
      if (!current(token)) return;
      if (conflict) state.error.value = CONFLICT_TEXT;
      else state.status.value = done(scope);
    } finally { if (alive()) state.busy.value = false; }
  };

  const save = slot => {
    const value = state.drafts.peek().get(slot.key) ?? slot.effective;
    return change(slot, scope => request(savePath(slot), { method: 'PUT', body: saveBody(slot, scope, value) }), () => savedText(slot), 'Could not save');
  };
  const reset = slot => change(slot, scope => request(resetPath(slot, scope), { method: 'DELETE' }), scope => resetText(slot, scope), 'Could not reset');

  const saveInline = async value => {
    const token = opened.current;
    batch(() => { state.error.value = ''; state.status.value = ''; state.inline.value = value; state.inlineSaving.value = true; });
    try {
      await request('/api/inline-plans', { method: 'PUT', body: { value } });
      if (!alive()) return;
      inlineSaves.current++;
      state.savedInline.value = value;
      if (current(token)) state.status.value = `Saved. Inline plans: ${value}.`;
    } catch (failure) {
      if (!alive()) return;
      state.inline.value = state.savedInline.peek();
      if (current(token)) state.error.value = failure.message || 'Could not save';
    } finally { if (alive()) state.inlineSaving.value = false; }
  };

  return { state, chooseScope, setDraft, save, reset, saveInline };
}
