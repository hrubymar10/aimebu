import { html } from '../../ui.js';
import { roleMark } from '../../role-defaults-logic.js';

// One role's "default for this model": a labelled switch row, drawn from `state` ({ on, position, total, disabled, reason }).
export function RoleSwitch({ role, modelLabel, state, onToggle }) {
  return html`
    <label class=${`harness-role-switch${state.on ? ' is-on' : ''}`} title=${state.reason || ''}>
      <input
        type="checkbox"
        checked=${state.on}
        disabled=${state.disabled}
        aria-label=${`${role.label} default: ${modelLabel}`}
        onChange=${event => onToggle(event.currentTarget.checked)}
      />
      <span class="harness-role-switch-text"><span class="harness-role-switch-mark">${roleMark(role)}</span>${` ${role.label}`}</span>
      <span class="harness-role-switch-position">${state.on ? `#${state.position} of ${state.total}` : ''}</span>
    </label>
  `;
}
