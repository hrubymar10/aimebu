import { html } from '../../ui.js';
import { IconButton } from '../../components/icon-button.js';
import { PICK_MODES, canAddDefault, roleMark } from '../../role-defaults-logic.js';

// The ordered defaults of every role and each role's pick order, edited in place: every change is one save. `labelOf(item)` names an entry;
// `run(action)` reports a failed save and redraws.
export function RolePanel({ roles, labelOf, store, run }) {
  return html`
    <section class="harness-roles" aria-label="Role defaults">
      <h4>Role defaults</h4>
      <p class="settings-desc">The models a role is spawned with, in order. Tick a role on a model above to add it; order and pick mode are set here. A role with no defaults leaves the harness and model to the person or lead.</p>
      <div class="harness-role-list">
        ${roles.map(role => {
          const items = role.default_agents || [];
          return html`
            <article class="harness-role-card" data-role-key=${role.role_key} key=${role.id}>
              <div class="harness-role-head-line">
                <span class="harness-role-mark">${roleMark(role)}</span>
                <strong>${role.label}</strong>
                <code class="harness-role-key">${role.role_key}</code>
                <select
                  aria-label=${`Pick order for ${role.label}`}
                  title="How the next default is picked"
                  value=${role.pick_mode || 'in_turn'}
                  onChange=${event => run(() => store.setMode(role, event.currentTarget.value))}
                >${PICK_MODES.map(([value, label]) => html`<option value=${value} key=${value}>${label}</option>`)}</select>
              </div>
              <ol class="harness-role-order">
                ${!items.length && html`<li class="settings-desc harness-role-empty">No defaults</li>`}
                ${items.map((item, index) => html`
                  <li class="harness-role-entry" data-model=${item.model} key=${`${item.launcher_id}:${item.model}`}>
                    <span class="harness-role-entry-label">${labelOf(item)}</span>
                    <${IconButton} name="arrow-up" label=${`Move ${labelOf(item)} up for ${role.label}`} disabled=${index === 0} onClick=${() => run(() => store.moveEntry(role, item.launcher_id, item.model, -1))} />
                    <${IconButton} name="arrow-down" label=${`Move ${labelOf(item)} down for ${role.label}`} disabled=${index === items.length - 1} onClick=${() => run(() => store.moveEntry(role, item.launcher_id, item.model, 1))} />
                    <${IconButton} name="trash" label=${`Remove ${labelOf(item)} from ${role.label}`} danger onClick=${() => run(() => store.removeEntry(role, item.launcher_id, item.model))} />
                  </li>
                `)}
                ${!canAddDefault(role) && html`<li class="settings-desc">The list is full.</li>`}
              </ol>
            </article>
          `;
        })}
        ${!roles.length && html`<p class="settings-intro">No roles yet.</p>`}
      </div>
    </section>
  `;
}
