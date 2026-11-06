import { html, Fragment, useLayoutEffect, useRef } from '../../ui.js';
import { SettingsGroup, SettingsRow } from '../../components/settings-row.js';
import { SlotCard } from './slot-card.js';
import { usePrompts } from './use-prompts.js';
import { previewText } from './model.js';

export function PromptsPage({ openToken, projectContext, onDirty = () => {} }) {
  const { state, chooseScope, setDraft, save, reset, saveInline } = usePrompts({ openToken, projectContext });
  const intro = useRef(null);
  const scope = state.scope.value, view = state.view.value, drafts = state.drafts.value, busy = state.busy.value;
  // The section is the slot's, but the legacy page marks it busy and test-ui reads that, so the page sets the one attribute.
  useLayoutEffect(() => {
    const section = intro.current?.closest('section');
    section?.toggleAttribute('aria-busy', busy);
    return () => section?.removeAttribute('aria-busy');
  }, [busy]);
  // Rows drawn for another scope than the selected one (its read is pending or failed) cannot write.
  const locked = !view?.can_edit || busy || view.scope !== scope;

  return html`
    <${Fragment}>
      <h3>Prompts</h3>
      <p class="settings-intro" ref=${intro}>The wording an agent gets around its role instructions. A project override wins over a global one, which wins over the shipped text. The response format stays fixed, so a wording change cannot stop answers from being read.</p>
      <${SettingsGroup} label="Prompt scope and inline plans">
        <${SettingsRow} label="Scope" desc="Whose prompts you are looking at. Global applies to every project that has no override of its own." id="prompt-scope">
          <select class="settings-select" value=${scope} disabled=${busy} onChange=${event => chooseScope(event.currentTarget.value)}>
            ${state.options.value.map(([id, label]) => html`<option value=${id} key=${id}>${label}</option>`)}
          </select>
        <//>
        <${SettingsRow} label="Inline plans" desc='Whether the lead is told to always attach the full plan as a collapsed "Full plan" block under a visual plan, or leave it to the lead. Applies to every project.' id="prompt-inline-plan">
          <select class="settings-select" value=${state.inline.value} disabled=${!view?.can_edit_global || busy || state.inlineSaving.value} onChange=${event => saveInline(event.currentTarget.value)}>
            <option value="optional">Optional</option>
            <option value="always">Always</option>
          </select>
        <//>
      <//>
      <p class="settings-intro" role="status">${state.status.value}</p>
      <p class="error" role="alert">${state.error.value}</p>
      <div id="prompt-list" data-scope=${view?.scope}>
        ${view?.slots.map(slot => html`<${SlotCard}
          key=${slot.key} slot=${slot} scope=${view.scope} max=${view.max} draft=${drafts.get(slot.key)} locked=${locked}
          onDraft=${setDraft} onSave=${save} onReset=${reset} onDirty=${onDirty}
        />`)}
      </div>
      <h3>Preview</h3>
      <p class="settings-intro">The prompt as an agent would get it in this scope, with your unsaved edits. Bracketed parts are the locked ones.</p>
      <pre class="prompt-preview" id="prompt-preview">${view ? previewText(view.slots, drafts) : ''}</pre>
    <//>
  `;
}
