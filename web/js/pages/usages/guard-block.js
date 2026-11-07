import { html } from '../../ui.js';
import { SettingsGroup, SettingsRow } from '../../components/settings-row.js';
import { FieldsRow, EntryField, ActionButton, AddRow } from './fields.js';
import { GUARD_TOOLS, guardSentence } from '../../limit-guard-format.js';
import { numberOf, boxText } from './quota-model.js';

const INTRO = 'Act before an account runs out. At the soft threshold the server starts no new turns on that tool (a running turn finishes) and the automatic switcher moves to another profile that is still usable. If there is none, new turns stay held until usage falls; at the hard threshold you are told it has reached its limit. The higher of the session and weekly usage counts.';

const Threshold = ({ label, value, onInput, describedBy, ...rest }) => html`<input class="settings-text-input" type="number" aria-label=${label} aria-describedby=${describedBy} min="1" max="100" step="1" value=${boxText(value)} onInput=${event => onInput(numberOf(event.currentTarget.value))} ...${rest} />`;

// Per-profile overrides: one row per profile that has its own thresholds, and a picker for the rest.
function Overrides({ store, tool, label, draft, stored }) {
  const profiles = draft.profiles || {};
  const typing = (name, field, value) => store.edit(tool, d => { d.profiles[name][field] = value; }, { typing: true });
  return html`<${SettingsGroup} label=${`${label} per-profile thresholds`}>
    ${Object.keys(profiles).sort().map((name, index) => {
      const limit = profiles[name], missing = !stored.includes(name), first = `guard-${tool}-o${index}-soft`;
      return html`<${FieldsRow} key=${name} label=${`${name}${missing ? ' (not found)' : ''}`} fieldId=${first} className="settings-row-stack"
        desc=${missing ? 'This profile no longer exists. Remove the override or restore the profile.' : `Thresholds for ${name} only.`}>
        <div class="quota-entry-controls">
          <${EntryField} caption="Soft %"><${Threshold} id=${first} describedBy=${`${first}-desc`} label=${`${name} soft threshold in percent`} value=${limit.soft} onInput=${value => typing(name, 'soft', value)} /><//>
          <${EntryField} caption="Hard %"><${Threshold} id=${`guard-${tool}-o${index}-hard`} label=${`${name} hard threshold in percent`} value=${limit.hard} onInput=${value => typing(name, 'hard', value)} /><//>
          <${ActionButton} glyph="trash" danger label=${`Remove the own thresholds of ${name}`} onClick=${() => store.edit(tool, d => { delete d.profiles[name]; })} />
        </div>
      <//>`;
    })}
    <${AddRow} id=${`guard-${tool}-add`} available=${stored.filter(name => !profiles[name])} label="Own thresholds for a profile" desc="For a profile that should be held earlier or later than the rest, such as a smaller plan."
      addLabel=${`Give the chosen ${label} profile its own thresholds`} onAdd=${name => store.edit(tool, d => { d.profiles ||= {}; d.profiles[name] = { soft: d.soft, hard: d.hard }; })} />
  <//>`;
}

function Tool({ store, tool, label }) {
  const data = store.data.value, info = data.tools[tool], draft = store.drafts.value[tool];
  if (!draft) return null;
  const stored = data.profiles?.[tool] || [], dirty = store.dirty(tool), busy = store.busy.value.has(tool);
  const typing = (field, value) => store.edit(tool, d => { d[field] = value; }, { typing: true });
  return html`<div class="guard-tool" data-tool=${tool}>
    <h5 class="guard-tool-title">${label}</h5>
    <p class="settings-desc guard-live" data-state=${info.result?.state || ''}>${guardSentence(label, info, data.percent_display)}</p>
    <${SettingsGroup} label=${`${label} limit guard`}>
      <${SettingsRow} label=${`Hold ${label} before it runs out`} desc=${`Use the thresholds below for ${label}.`} id=${`guard-${tool}-enabled`} className="settings-row-toggle">
        <input type="checkbox" checked=${!!draft.enabled} onChange=${event => store.edit(tool, d => { d.enabled = event.currentTarget.checked; }, { typing: true })} />
      <//>
      <${SettingsRow} label="Soft threshold (%)" desc="No new turns from here, and the automatic switcher moves to another usable profile. The running turn finishes." id=${`guard-${tool}-soft`}>
        <${Threshold} label=${`${label} soft threshold in percent`} value=${draft.soft} onInput=${value => typing('soft', value)} />
      <//>
      <${SettingsRow} label="Hard threshold (%)" desc="You are told the profile has reached its limit; new turns stay held until usage falls." id=${`guard-${tool}-hard`}>
        <${Threshold} label=${`${label} hard threshold in percent`} value=${draft.hard} onInput=${value => typing('hard', value)} />
      <//>
    <//>
    <${Overrides} store=${store} tool=${tool} label=${label} draft=${draft} stored=${stored} />
    <div class="settings-actions">
      <${ActionButton} text="Save" label=${`Save ${label} limits`} primary className="guard-save" id=${`guard-${tool}-save`} disabled=${!store.savable(tool) || busy} onClick=${() => store.save(tool)} />
      <${ActionButton} text="Discard changes" label=${`Discard ${label} limit changes`} className="guard-discard" disabled=${!dirty} onClick=${() => store.discard(tool)} />
      <span class="settings-desc guard-unsaved" hidden=${!dirty}>Unsaved changes</span>
    </div>
    <p class="error guard-error" role="alert">${store.errors.value[tool] || ''}</p>
    <p class="settings-desc guard-note" role="status">${store.notes.value[tool] || ''}</p>
  </div>`;
}

export function GuardBlock({ store }) {
  const visible = store.visible.value, data = store.data.value;
  const attach = node => {
    if (!node || store.node === node) return;
    store.node = node;
    node.addEventListener('focusout', () => setTimeout(() => store.settle(), 0));
  };
  return html`<section class="limit-guard-settings" aria-label="Limits before 100%" hidden=${!visible} ref=${attach}>
    ${visible && data && html`<h4 class="quota-title">Limits before 100%</h4>
      <p class="settings-desc">${INTRO}</p>
      ${GUARD_TOOLS.filter(([tool]) => data.tools?.[tool]).map(([tool, label]) => html`<${Tool} key=${tool} store=${store} tool=${tool} label=${label} />`)}`}
  </section>`;
}
