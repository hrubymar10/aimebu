import { useLayoutEffect, useMemo, useRef, signal, batch } from '../../ui.js';
import { request } from '../../api.js';
import { confirmDialog } from '../../dialogs.js';
import { useAlive } from '../../hooks/use-alive.js';
import { ROLE_DEFAULTS_CHANGED } from '../../role-defaults.js';
import { loadSpawnOptions } from '../../spawn-options.js';
import { formFromRole, formState, roleData, resetQuestion } from './model.js';

const createState = () => ({
  roles: signal([]),
  loaded: signal(false),
  // The role in the editor ('' for a new one) and whether the editor is showing.
  selected: signal(''),
  editorOpen: signal(false),
  form: signal(formFromRole(null)),
  // What the form held when it was drawn from the stored role, to tell an edit from a fresh form.
  baseline: signal(formState(formFromRole(null))),
  options: signal([]),
  error: signal(''),
  busy: signal(false),
  // Where focus goes once the next render has put the element there: { field }, { card } or { newRole }.
  focus: signal(null),
});

// Reads carry a generation, so only the latest answer draws. The form is a draft kept apart from the stored roles: a read for another
// reason leaves an open editor alone, and only the action that saved it hands the form back to the server's values.
export function useRoles() {
  const alive = useAlive();
  const state = useMemo(createState, []);
  const generation = useRef(0);
  const source = useRef({});
  useLayoutEffect(() => () => { generation.current++; }, []);

  const selectedRole = () => state.roles.peek().find(role => role.id === state.selected.peek());
  const show = id => {
    const role = state.roles.peek().find(item => item.id === id);
    const form = formFromRole(role);
    batch(() => { state.selected.value = role ? role.id : ''; state.form.value = form; state.baseline.value = formState(form); });
  };
  const changed = () => document.dispatchEvent(new CustomEvent(ROLE_DEFAULTS_CHANGED, { detail: { source: source.current } }));

  const load = async (selected = '') => {
    const ticket = ++generation.current;
    batch(() => { state.error.value = ''; state.loaded.value = false; });
    try {
      const roles = await request('/api/roles');
      if (ticket !== generation.current || !alive()) return;
      batch(() => {
        state.roles.value = roles;
        show(selected);
        state.editorOpen.value = !!selected;
        state.loaded.value = true;
      });
    } catch (failure) {
      if (ticket === generation.current && alive()) state.error.value = failure.message;
    }
  };

  const open = async () => {
    try { await Promise.all([load(), loadSpawnOptions().then(options => { if (alive()) state.options.value = options; })]); }
    catch (failure) { if (alive()) state.error.value = failure.message; }
  };

  // Default agents are edited here and on the Harnesses page, two views of the same role: a save made there is read again here, unless
  // an editor is open (the list is read again whenever Settings opens this page). A layout effect, so a destroyed page stops listening at once.
  useLayoutEffect(() => {
    const listener = event => { if (event.detail?.source !== source.current && !state.editorOpen.peek()) load(); };
    document.addEventListener(ROLE_DEFAULTS_CHANGED, listener);
    return () => document.removeEventListener(ROLE_DEFAULTS_CHANGED, listener);
  }, []);

  const edit = id => {
    batch(() => { show(id); state.editorOpen.value = true; state.focus.value = { field: id ? 'label' : 'role_key', scroll: true }; });
  };
  // The select behind the cards: picking a role opens it.
  const pick = id => batch(() => { show(id); state.editorOpen.value = true; });
  const setField = (name, value) => { state.form.value = { ...state.form.peek(), [name]: value }; };
  const setDefaults = ({ items, mode }) => { state.form.value = { ...state.form.peek(), default_agents: items, pick_mode: mode }; };

  // Closes the editor without saving (after a confirmation when something was changed) and gives focus back to what opened it.
  const cancel = async () => {
    if (!state.editorOpen.peek()) return;
    if (formState(state.form.peek()) !== state.baseline.peek() && !await confirmDialog('Discard your changes to this role?', { title: 'Discard changes', confirm: 'Discard', danger: true })) return;
    if (!alive() || !state.editorOpen.peek()) return;
    const id = state.selected.peek();
    batch(() => { show(''); state.editorOpen.value = false; state.focus.value = id ? { card: id } : { newRole: true }; });
  };

  // One write at a time: a second would carry a revision the first has already moved on.
  const write = async (run, done) => {
    if (state.busy.peek()) return;
    batch(() => { state.error.value = ''; state.busy.value = true; });
    try {
      const result = await run();
      if (!alive()) return;
      await done(result);
      if (alive()) changed();
    } catch (failure) {
      if (alive()) state.error.value = failure.message;
    } finally { if (alive()) state.busy.value = false; }
  };

  const save = () => {
    const data = roleData(state.form.peek(), selectedRole());
    return write(() => request(data.id ? `/api/roles/${data.id}` : '/api/roles', { method: data.id ? 'PUT' : 'POST', body: data }), async saved => {
      // A saved role closes its form; focus goes back to the role's card, where its Edit button is.
      await load();
      if (alive()) state.focus.value = { card: saved.id };
    });
  };
  const reset = async () => {
    const role = selectedRole();
    if (!role || !role.builtin || !await confirmDialog(resetQuestion(role), { title: 'Reset role', confirm: 'Reset', danger: true })) return;
    await write(() => request(`/api/roles/${role.id}/reset`, { method: 'POST', body: { rev: role.rev } }), () => load(role.id));
  };
  const remove = async () => {
    const role = selectedRole();
    if (!role || !await confirmDialog(`Delete role ${role.label}?`, { title: 'Delete role', confirm: 'Delete', danger: true })) return;
    await write(() => request(`/api/roles/${role.id}?rev=${role.rev}`, { method: 'DELETE' }), () => load());
  };

  return { state, open, edit, pick, cancel, setField, setDefaults, save, reset, remove, selectedRole };
}
