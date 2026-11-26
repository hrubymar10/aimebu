import { html } from '../../ui.js';
import { SettingsGroup, SettingsRow } from '../../components/settings-row.js';
import { IconButton } from '../../components/icon-button.js';
import { SpawnDefaults } from '../../components/spawn-defaults.js';
import { extendsOptions, editHeading, CAPS } from './model.js';

const Toggle = ({ label, desc, id, name, form, set }) => html`
  <${SettingsRow} label=${label} desc=${desc} id=${id} className="settings-row-toggle">
    <input name=${name} type="checkbox" checked=${form[name]} onChange=${event => set(name, event.currentTarget.checked)} />
  <//>`;

function promptDiff(before, after) {
  const oldLines = before.split('\n'), newLines = after.split('\n');
  let start = 0, end = 0;
  while (start < oldLines.length && start < newLines.length && oldLines[start] === newLines[start]) start++;
  while (end < oldLines.length - start && end < newLines.length - start && oldLines[oldLines.length - 1 - end] === newLines[newLines.length - 1 - end]) end++;
  return [...oldLines.slice(0, start).map(line => `  ${line}`), ...oldLines.slice(start, oldLines.length - end).map(line => `- ${line}`),
    ...newLines.slice(start, newLines.length - end).map(line => `+ ${line}`), ...oldLines.slice(oldLines.length - end).map(line => `  ${line}`)].join('\n');
}

export function RoleForm({ hidden, roles, role, form, options, busy, dirty, resolved, owner, onField, onDefaults, onSave, onCancel, onReset, onDelete, onUseDefault, onKeepDefault }) {
  const set = onField;
  const text = name => ({ name, value: form[name], onInput: event => set(name, event.currentTarget.value) });
  return html`<form id="role-editor" hidden=${hidden} aria-labelledby="role-editor-title" onSubmit=${event => { event.preventDefault(); onSave(); }}
    onKeyDown=${event => {
      // Esc would otherwise close the whole Settings dialog; inside the editor it only cancels the edit.
      if (event.key !== 'Escape' || event.isComposing) return;
      event.preventDefault(); event.stopPropagation();
      onCancel();
    }}>
    <div class="role-editor-head">
      <h4 id="role-editor-title">${editHeading(role)}</h4>
      <${IconButton} name="close" label="Close without saving" className="role-editor-close" onClick=${onCancel} />
    </div>
    <${SettingsGroup} label="Role definition">
      <${SettingsRow} label="Key" desc=${html`Stable identifier used in <code>@role</code> mentions and team definitions: lowercase letters, digits, <code>_</code> or <code>-</code>, 2–32 characters.`} id="role-key">
        <input class="settings-text-input" ...${text('role_key')} required pattern="[a-z][a-z0-9_-]{1,31}" readOnly=${!!role} />
      <//>
      <${SettingsRow} label="Name" desc="Display name shown on agent cards and in the spawn dialog." id="role-label">
        <input class="settings-text-input" ...${text('label')} required maxLength="80" />
      <//>
      <${SettingsRow} label="Emoji" desc="Small icon shown next to the role on agent cards and messages." id="role-emoji">
        <input class="settings-text-input" ...${text('emoji')} maxLength="16" />
      <//>
      <${SettingsRow} label="Instructions" desc="Prompt text given to every agent that holds this role, on top of the shared room protocol (and after the instructions of the role it extends, if any)." id="role-body" className="settings-row-stack">
        <textarea class="settings-text-input settings-textarea" ...${text('body')} rows="8"></textarea>
      <//>
      ${role?.default_body_changed && html`<div class="settings-row settings-row-stack" id="role-default-change">
        <div class="settings-row-info"><span class="settings-label">Default prompt changed</span><span class="settings-desc">${role.default_body_base ? 'Compare the default you edited against with the new default.' : 'We do not know which default this was based on. Compare your saved instructions with the new default.'}</span></div>
        <div class="settings-control"><details><summary>Show prompt diff</summary><pre class="role-default-diff" aria-label=${role.default_body_base ? 'Previous default to new default diff' : 'Your instructions to new default diff'}>${promptDiff(role.default_body_base || role.body, role.default_body)}</pre></details>${dirty && html`<p class="settings-desc">Save or discard your draft before choosing a default action.</p>`}
          <div class="dialog-actions"><button type="button" class="btn-icon" disabled=${busy || dirty} title="Use the new default" aria-label="Use the new default" onClick=${onUseDefault}><svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><path d="M12 3v12m-4-4 4 4 4-4M4 17v4h16v-4"/></svg></button><button type="button" class="btn-icon" disabled=${busy || dirty} title="Keep my version" aria-label="Keep my version" onClick=${onKeepDefault}><svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><path d="M5 12l5 5L20 7"/></svg></button></div>
        </div>
      </div>`}
      <${SettingsRow} label="Extension" desc=${owner ? 'Text added after the instructions without replacing them, so updates to the default instructions keep flowing. Projects can add more of their own.' : 'Text added after the instructions without replacing them. Only the owner can change it.'} id="role-extension" className="settings-row-stack">
        <textarea class="settings-text-input settings-textarea" ...${text('body_extension')} rows="4" readOnly=${!owner} placeholder=${owner ? '' : 'Only the owner edits the extension.'}></textarea>
      <//>
      <div class="settings-row settings-row-stack" id="role-effective-row" hidden=${!role}>
        <div class="settings-row-info"><label class="settings-label" for="role-effective">Final prompt</label><span class="settings-desc" id="role-effective-desc">What an agent with this role is told, the extension included, as last saved.</span></div>
        <div class="settings-control"><details><summary aria-describedby="role-effective-desc">Show final prompt</summary><textarea class="settings-text-input settings-textarea" id="role-effective" aria-describedby="role-effective-desc" rows="8" readOnly value=${role?.effective_body ?? ''}></textarea></details></div>
      </div>
      <${SettingsRow} label="Extends" desc="Adds to another role: agents get that role's instructions first, then these. Leave on None for a complete role. A project role may extend the global role with the same key." id="role-extends">
        <select class="settings-select" name="extends" value=${form.extends} onChange=${event => set('extends', event.currentTarget.value)}>
          ${extendsOptions(roles, role).map(([value, label]) => html`<option value=${value} key=${value}>${label}</option>`)}
        </select>
      <//>
      <div class="settings-row settings-row-stack" id="role-resolved-row" hidden=${!role || !role.extends}>
        <div class="settings-row-info"><label class="settings-label" for="role-resolved">Effective instructions</label><span class="settings-desc" id="role-resolved-desc">What an agent with this role receives, the parent's text included, as last saved.</span></div>
        <div class="settings-control"><textarea class="settings-text-input settings-textarea" id="role-resolved" aria-describedby="role-resolved-desc" rows="8" readOnly value=${resolved}></textarea></div>
      </div>
      <${SettingsRow} label="Cardinality" desc="Whether several agents can hold this role at once, or only one. Cannot change while live agents use the role." id="role-cardinality">
        <select class="settings-select" name="cardinality" value=${form.cardinality} onChange=${event => set('cardinality', event.currentTarget.value)}>
          <option value="multi">Many holders</option><option value="singleton">One holder</option>
        </select>
      <//>
      <${Toggle} label="Leader" desc="Holders are the target of handoffs and of scheduled tasks that name no agent." id="role-lead" name=${CAPS[0]} form=${form} set=${set} />
      <${Toggle} label="Can spawn" desc="Holders may start more agents, within the project's spawn limits." id="role-spawn" name=${CAPS[1]} form=${form} set=${set} />
    <//>
    <${SpawnDefaults} items=${form.default_agents} mode=${form.pick_mode} options=${options} onChange=${onDefaults} />
    <div class="dialog-actions">
      <button type="submit" class="btn btn-primary" disabled=${busy}>Save role</button>
      <button id="role-cancel" type="button" class="btn" title="Close the editor without saving" onClick=${onCancel}>Cancel</button>
      <button id="role-reset" type="button" class="btn-icon" disabled=${busy || !role || !role.builtin} title="Reset to default — restore this built-in role’s original name, instructions and settings" aria-label="Reset to default" onClick=${onReset}><span class="icon-mask icon-mask-refresh" aria-hidden="true"></span></button>
      <button id="role-delete" type="button" class="btn btn-danger" disabled=${busy || !role || role.builtin} onClick=${onDelete}>Delete</button>
    </div>
  </form>`;
}
