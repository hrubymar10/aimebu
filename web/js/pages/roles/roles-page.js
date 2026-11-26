import { html, Fragment, useLayoutEffect } from '../../ui.js';
import { rights } from '../../api.js';
import { useDirty } from '../../hooks/use-dirty.js';
import { formState, roleOptionLabel } from './model.js';
import { useRoles } from './use-roles.js';
import { RoleCard } from './role-card.js';
import { RoleForm } from './role-form.js';

export function RolesPage({ openToken, onDirty = () => {} }) {
  const roles = useRoles();
  const { state } = roles;
  const list = state.roles.value, selected = state.selected.value, editorOpen = state.editorOpen.value, form = state.form.value, focus = state.focus.value;
  const role = roles.selectedRole();
  useLayoutEffect(() => { if (openToken) roles.open(); }, [openToken]);
  useDirty(onDirty, 'role-form', editorOpen && formState(form) !== state.baseline.value);
  // Focus waits for the render that puts the element there.
  useLayoutEffect(() => {
    if (!focus) return;
    state.focus.value = null;
    if (focus.field) {
      const editor = document.querySelector('#role-editor');
      if (focus.scroll) editor.scrollIntoView?.({ block: 'nearest' });
      editor.elements[focus.field].focus({ preventScroll: true });
    } else if (focus.card) document.querySelector(`#roles-cards [data-role-id="${focus.card}"] .role-card-edit`)?.focus();
    else document.querySelector('#role-new')?.focus();
  }, [focus]);

  return html`
    <${Fragment}>
      <div class="settings-group" role="group" aria-label="Role selection" hidden>
        <div class="settings-row" hidden>
          <div class="settings-row-info"><label class="settings-label" for="roles-list">Role</label><span class="settings-desc" id="roles-list-desc">The role to edit. Choose <em>New role</em> to define another.</span></div>
          <div class="settings-control">
            <select class="settings-select" id="roles-list" aria-describedby="roles-list-desc" data-loaded=${state.loaded.value ? '1' : ''} value=${selected} onChange=${event => roles.pick(event.currentTarget.value)}>
              <option value="">New role</option>
              ${list.map(item => html`<option value=${item.id} key=${item.id}>${roleOptionLabel(item)}</option>`)}
            </select>
          </div>
        </div>
      </div>
      <div class="role-cards-toolbar"><button type="button" id="role-new" class="btn" onClick=${() => roles.edit('')}>New role</button></div>
      <div id="roles-cards" class="role-cards">
        ${state.loaded.value && !list.length && html`<p class="settings-intro">No roles yet.</p>`}
        ${list.map(item => html`<${RoleCard} key=${item.id} role=${item} roles=${list} selected=${selected === item.id}
          open=${editorOpen && selected === item.id} onEdit=${() => roles.edit(item.id)} onClose=${roles.cancel} />`)}
      </div>
      <${RoleForm} hidden=${!editorOpen} roles=${list} role=${role} form=${form} options=${state.options.value} busy=${state.busy.value} dirty=${formState(form) !== state.baseline.value} resolved=${role?.resolved_body ?? ''} owner=${rights.owner}
        onField=${roles.setField} onDefaults=${roles.setDefaults} onSave=${roles.save} onCancel=${roles.cancel} onReset=${roles.reset} onDelete=${roles.remove} onUseDefault=${roles.useDefault} onKeepDefault=${roles.keepDefault} />
      <p id="roles-error" class="error" role="alert">${state.error.value}</p>
    <//>
  `;
}
