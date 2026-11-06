import { html, useLayoutEffect } from '../../ui.js';
import { SettingsGroup, SettingsRow } from '../../components/settings-row.js';
import { FieldsRow, NumberField, EntryField, ActionButton, AddRow } from './fields.js';
import { TOOLS, RANGES, when, describeDecision, isSavable, addable, protectNames, watchChoices, orderEntry, moveEntry, protectProfile, afterResetFor, decisionText, decisionProfiles, decisionAction, resetText, trippedText, focusFallbacks } from './quota-model.js';

const INTRO = 'The server picks the first profile in each order that is still under its limits and switches to it, the same way the Switch button does. A switch waits until no agent is running a turn. Nothing is switched while dry run is on.';

function Live({ store, tool, label, info }) {
  const state = info.state || {}, busy = store.busy.value.has(tool);
  return html`<div class="quota-live">
    <div class="quota-status">
      ${!info.saved && html`<p class="settings-desc quota-defaults">These are suggested defaults built from your profile names. Nothing is saved until you press Save.</p>`}
      ${info.config.enabled && info.config.dry_run && html`<p class="settings-desc quota-dry-run">Dry run: decisions are recorded below, nothing is switched.</p>`}
      <p class="settings-desc quota-last">${info.last_decision ? `Last decision (${when(info.last_decision.at)}): ${describeDecision(info.last_decision)}` : 'No decision yet.'}</p>
      ${state.hold && html`<p class="settings-desc quota-hold">${`Current hold: ${state.hold}`}</p>`}
      <p class="settings-desc quota-evaluated">${`Last evaluated: ${when(state.evaluated_at)}`}</p>
    </div>
    ${state.tripped && html`<div class="quota-banner" role="alert"><p class="quota-tripped">${trippedText(state)}</p>
      <${ActionButton} text="Re-enable switching" label=${`Re-enable automatic switching for ${label}`} primary disabled=${busy} focus=${`${tool}-reenable`} onClick=${() => store.act(tool, 'reenable')} /></div>`}
    ${state.reset_latched_at && html`<div class="quota-reset"><p class="settings-desc quota-reset-text">${resetText(state)}</p>
      <${ActionButton} text="Re-arm" label=${`Re-arm the reset detection for ${label}`} disabled=${busy} focus=${`${tool}-rearm`} onClick=${() => store.act(tool, 'rearm')} /></div>`}
  </div>`;
}

// The ordered list: one row per entry (name, limits, up, down, remove) and an add row.
function Order({ store, tool, kind, entries, stored, groupLabel, protectedNames }) {
  const available = addable(stored, entries, protectedNames);
  const pick = `quota-${tool}-${kind}-add`;
  const change = (change, focus = '') => store.change(tool, draft => change(kind === 'order' ? draft.profiles : draft.after_reset.profiles), focus);
  const typing = (index, field, value) => store.edit(tool, draft => { (kind === 'order' ? draft.profiles : draft.after_reset.profiles)[index][field] = value; }, { typing: true });
  return html`<${SettingsGroup} label=${groupLabel}>
    ${entries.map((entry, index) => {
      const key = name => `${kind}:${entry.name}:${name}`, session = `quota-${tool}-${kind}-${index}-session`;
      const missing = !stored.includes(entry.name);
      return html`<${FieldsRow} key=${entry.name} label=${`${index + 1}. ${entry.name}${missing ? ' (not found)' : ''}`} fieldId=${session} className="settings-row-stack"
        desc=${missing ? 'This profile no longer exists, so it is skipped. Remove it or restore the profile.' : `Switch away from ${entry.name} once its session or weekly usage reaches these limits.`}>
        <div class="quota-entry-controls">
          <${EntryField} caption="Session %"><${NumberField} id=${session} describedBy=${`${session}-desc`} value=${entry.session_max} label=${`${entry.name} session limit (%)`} range=${RANGES.session_max} onInput=${value => typing(index, 'session_max', value)} /><//>
          <${EntryField} caption="Weekly %"><${NumberField} id=${`quota-${tool}-${kind}-${index}-weekly`} value=${entry.weekly_max} label=${`${entry.name} weekly limit (%)`} range=${RANGES.weekly_max} onInput=${value => typing(index, 'weekly_max', value)} /><//>
          <${ActionButton} glyph="arrow-up" label=${`Move ${entry.name} up`} disabled=${index === 0} focus=${key('up')} onClick=${() => change(list => moveEntry(list, index, -1), key('up'))} />
          <${ActionButton} glyph="arrow-down" label=${`Move ${entry.name} down`} disabled=${index === entries.length - 1} focus=${key('down')} onClick=${() => change(list => moveEntry(list, index, 1), key('down'))} />
          <${ActionButton} glyph="trash" danger label=${`Remove ${entry.name} from the order`} focus=${key('remove')} onClick=${() => change(list => list.splice(index, 1), `${kind}-add`)} />
        </div>
      <//>`;
    })}
    <${AddRow} id=${pick} focus=${`${kind}-add`} available=${available} label="Add profile" desc="Profiles in the store that are not in this order and not protected."
      addLabel=${`Add the chosen profile to the ${groupLabel.toLowerCase()}`} onAdd=${name => change(list => list.push(orderEntry(name)), `${kind}-add`)} />
  <//>`;
}

function Protect({ store, tool, label, draft, stored }) {
  const names = protectNames(stored, draft);
  if (!names.length) return html`<p class="settings-desc quota-none">${`There are no ${label} profiles to protect yet.`}</p>`;
  return html`<${SettingsGroup} label=${`${label} protected profiles`}>
    ${names.map((name, index) => html`<${SettingsRow} key=${name} label=${`Protect ${name}`} id=${`quota-${tool}-protect-${index}`} className="settings-row-toggle"
      desc=${`Never switch to or away from ${name} while it is active, and keep it out of the orders. For a profile parked on purpose.`}>
      <input type="checkbox" checked=${draft.protected.includes(name)} data-quota-focus=${`protect:${name}`} onChange=${event => store.change(tool, d => protectProfile(d, name, event.currentTarget.checked), `protect:${name}`)} />
    <//>`)}
  <//>`;
}

function AfterReset({ store, tool, label, draft, stored }) {
  const after = draft.after_reset;
  return html`<div class="quota-after-reset">
    <${SettingsGroup} label=${`${label} reset detection`}>
      <${SettingsRow} label="Use a different order after a reset" id=${`quota-${tool}-after-reset`} className="settings-row-toggle"
        desc="Watch one profile’s weekly usage; when it drops by the chosen number of points the server treats the week as reset and switches to the order below. Re-arm it by hand.">
        <input type="checkbox" checked=${!!after} data-quota-focus=${`${tool}-after-reset`} onChange=${event => store.change(tool, d => { d.after_reset = event.currentTarget.checked ? afterResetFor(d) : null; }, `${tool}-after-reset`)} />
      <//>
      ${after && html`<${SettingsRow} label="Watched profile" desc="The profile whose weekly usage is watched for a reset." id=${`quota-${tool}-watched`}>
        <select class="settings-select" data-quota-focus=${`${tool}-watched`} value=${after.profile} onChange=${event => store.edit(tool, d => { d.after_reset.profile = event.currentTarget.value; }, { typing: true })}>
          ${watchChoices(after).map(name => html`<option value=${name} key=${name}>${name}</option>`)}
        </select>
      <//>
      <${SettingsRow} label="Drop (points)" desc="How far the weekly usage must fall below its highest value to count as a reset. 1 to 100." id=${`quota-${tool}-drop`}>
        <${NumberField} value=${after.drop_points} label=${`${label} reset drop in percentage points`} range=${RANGES.drop_points} onInput=${value => store.edit(tool, d => { d.after_reset.drop_points = value; }, { typing: true })} />
      <//>`}
    <//>
    ${after && html`<${Order} store=${store} tool=${tool} kind="after" entries=${after.profiles} stored=${stored} groupLabel=${`${label} order after a reset`} protectedNames=${draft.protected} />`}
  </div>`;
}

function Decisions({ store, tool, info }) {
  const list = Array.isArray(info.decisions) ? info.decisions : [];
  return html`<details class="quota-decisions" open=${store.open.value.has(`${tool}:decisions`)} onToggle=${event => store.setOpen(`${tool}:decisions`, event.currentTarget.open)}>
    <summary>${`Recent decisions (${list.length})`}</summary>
    ${!list.length ? html`<p class="settings-desc">Nothing has been decided yet.</p>` : html`<ol class="quota-decision-list">
      ${list.map((decision, index) => html`<li class="quota-decision" data-action=${decisionAction(decision)} key=${index}>
        <span class="quota-decision-text">${decisionText(decision)}</span>
        ${decisionProfiles(decision).length > 0 && html`<span class="settings-desc quota-decision-profiles">${decisionProfiles(decision).join('; ')}</span>`}
      </li>`)}
    </ol>`}
  </details>`;
}

function Tool({ store, tool, label }) {
  const data = store.data.value, info = data.tools[tool], draft = store.drafts.value[tool];
  if (!draft) return null;
  const stored = data.profiles?.[tool] || [];
  const dirty = store.dirty(tool), busy = store.busy.value.get(tool);
  const typing = (field, value) => store.edit(tool, d => { d[field] = value; }, { typing: true });
  return html`<div class="quota-tool" data-tool=${tool}>
    <h5 class="quota-tool-title">${label}</h5>
    <${Live} store=${store} tool=${tool} label=${label} info=${info} />
    <${SettingsGroup} label=${`${label} switching`}>
      <${SettingsRow} label=${`Switch ${label} automatically`} desc=${`Move ${label} to the first usable profile in the order below when the active one runs out of quota.`} id=${`quota-${tool}-enabled`} className="settings-row-toggle">
        <input type="checkbox" checked=${!!draft.enabled} data-quota-focus=${`${tool}-enabled`} onChange=${event => store.change(tool, d => { d.enabled = event.currentTarget.checked; }, `${tool}-enabled`)} />
      <//>
      <${SettingsRow} label="Dry run" desc="Record what would be switched without switching. Turn this off to switch for real." id=${`quota-${tool}-dry-run`} className="settings-row-toggle">
        <input type="checkbox" checked=${!!draft.dry_run} data-quota-focus=${`${tool}-dry-run`} onChange=${event => store.change(tool, d => { d.dry_run = event.currentTarget.checked; }, `${tool}-dry-run`)} />
      <//>
    <//>
    <${Order} store=${store} tool=${tool} kind="order" entries=${draft.profiles} stored=${stored} groupLabel=${`${label} profile order`} protectedNames=${draft.protected} />
    <${Protect} store=${store} tool=${tool} label=${label} draft=${draft} stored=${stored} />
    <${AfterReset} store=${store} tool=${tool} label=${label} draft=${draft} stored=${stored} />
    <details class="quota-advanced" open=${store.open.value.has(`${tool}:advanced`)} onToggle=${event => store.setOpen(`${tool}:advanced`, event.currentTarget.open)}>
      <summary>Advanced</summary>
      <${SettingsGroup} label=${`${label} advanced`}>
        <${SettingsRow} label="Warm-up (seconds)" desc="How long every usable profile must report fresh usage before the server acts on it. 0 to 600." id=${`quota-${tool}-warmup`}>
          <${NumberField} value=${draft.warmup_seconds} label=${`${label} warm-up in seconds`} range=${RANGES.warmup_seconds} onInput=${value => typing('warmup_seconds', value)} />
        <//>
        <${SettingsRow} label="Minimum time between switches (minutes)" desc="After an automatic switch the server waits this long before another one. This also delays a switch away from an exhausted account right after a switch. 0 to 120." id=${`quota-${tool}-dwell`}>
          <${NumberField} value=${draft.min_dwell_minutes} label=${`${label} minimum minutes between switches`} range=${RANGES.min_dwell_minutes} onInput=${value => typing('min_dwell_minutes', value)} />
        <//>
      <//>
    </details>
    <div class="settings-actions quota-actions">
      <${ActionButton} text="Save" label=${`Save ${label} automatic switching`} primary className="quota-save" disabled=${!isSavable(data, store.drafts.value, tool) || !!busy} focus=${`${tool}-save`} onClick=${() => store.save(tool)} />
      <${ActionButton} text="Discard changes" label=${`Discard ${label} changes`} className="quota-discard" disabled=${!dirty} focus=${`${tool}-discard`} onClick=${() => store.discard(tool)} />
      <span class="settings-desc quota-unsaved" hidden=${!dirty}>Unsaved changes</span>
    </div>
    <p class="error quota-error" role="alert">${store.errors.value[tool] || ''}</p>
    <p class="settings-desc quota-note" role="status" tabindex="-1" data-quota-focus=${`${tool}-note`}>${busy || store.notes.value[tool] || ''}</p>
    <${Decisions} store=${store} tool=${tool} info=${info} />
  </div>`;
}

// Puts focus on the control a change named, or its nearest sibling when that control is now disabled (a moved row's button).
function restoreFocus(node, key) {
  const find = wanted => [...node.querySelectorAll('[data-quota-focus], [id]')].find(item => (item.dataset.quotaFocus || item.id) === wanted && !item.disabled);
  (find(key) || focusFallbacks(key).map(find).find(Boolean))?.focus();
}

export function QuotaBlock({ store }) {
  const visible = store.visible.value, data = store.data.value, focus = store.focus.value;
  useLayoutEffect(() => {
    if (!focus) return;
    store.focus.value = null;
    if (store.node) restoreFocus(store.node, focus.key);
  }, [focus]);
  const attach = node => {
    if (!node || store.node === node) return;
    store.node = node;
    node.addEventListener('focusout', () => setTimeout(() => store.settle(), 0));
  };
  return html`<section class="quota-settings" aria-label="Automatic account switching" hidden=${!visible} ref=${attach}>
    ${visible && data && html`<h4 class="quota-title">Automatic account switching</h4>
      <p class="settings-desc quota-intro">${INTRO}</p>
      ${store.loadError.value && html`<p class="error" role="alert">${store.loadError.value}</p>`}
      ${TOOLS.filter(([tool]) => data.tools?.[tool]).map(([tool, label]) => html`<${Tool} key=${tool} store=${store} tool=${tool} label=${label} />`)}`}
  </section>`;
}
