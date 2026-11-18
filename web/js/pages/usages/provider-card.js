import { html, Fragment, useRef, useLayoutEffect } from '../../ui.js';
import { IconButton } from '../../components/icon-button.js';
import { promptDialog } from '../../dialogs.js';
import { toolOf, isSwitchable, providerStatus, affectedText, canEnable, providerConfigured, movedOrder, ollamaBody, flowText, profileKey } from './model.js';
import { ProfileCard } from './profile-card.js';

// The verification address comes from GitHub (or an Enterprise host), and only an https address becomes a link.
function httpsHref(address) {
  try { const url = new URL(address); return url.protocol === 'https:' && !url.username && !url.password ? url.href : ''; } catch { return ''; }
}

function CopilotSetup({ store, provider }) {
  const host = useRef(null);
  const flow = store.flow.value;
  return html`
    <input class="settings-text-input" type="url" placeholder="GitHub Enterprise host (optional)" ref=${host} value=${provider.enterprise_host || ''} />
    <button type="button" class="btn btn-primary" onClick=${() => store.startCopilot(host.current.value)}>Sign in with GitHub</button>
    ${provider.token_configured && html`<button type="button" class="btn" onClick=${() => store.mutateUsage(() => store.source.copilotLogout())}>Sign out</button>`}
    <div class="usage-copilot-flow">${flow && html`<${Fragment}><span>${flowText(flow)}</span>${httpsHref(flow.verification_uri) && html`<a href=${httpsHref(flow.verification_uri)} target="_blank" rel="noopener noreferrer">Open GitHub verification</a>`}<//>`}</div>
  `;
}

function MistralSetup({ store, provider }) {
  const cookie = useRef(null);
  return html`
    <textarea class="settings-text-input settings-textarea" placeholder="Paste Mistral Cookie request header" rows="2" aria-label="Mistral Cookie header" ref=${cookie}></textarea>
    <button type="button" class="btn btn-primary" onClick=${() => store.mutateUsage(() => store.source.mistral(cookie.current.value))}>${provider.cookie_configured ? 'Update cookie' : 'Save cookie'}</button>
    ${provider.cookie_configured && html`<button type="button" class="btn btn-danger" onClick=${() => store.mutateUsage(() => store.source.mistral(''))}>Clear cookie</button>`}
  `;
}

function OllamaSetup({ store, provider, configured }) {
  const mode = useRef(null), cookie = useRef(null), key = useRef(null);
  return html`
    <select class="settings-select" aria-label="Ollama Cloud authentication mode" ref=${mode} value=${provider.auth_mode || 'auto'}><option value="auto">Auto</option><option value="cookie">Cookie</option><option value="api_key">API key</option></select>
    <textarea class="settings-text-input settings-textarea" placeholder="Paste Ollama Cookie request header" rows="2" aria-label="Ollama Cloud Cookie header" ref=${cookie}></textarea>
    <input class="settings-text-input" type="password" placeholder="Ollama API key" aria-label="Ollama Cloud API key" ref=${key} />
    <button type="button" class="btn btn-primary" onClick=${() => store.mutateUsage(() => store.source.ollama(ollamaBody(mode.current.value, cookie.current.value, key.current.value)))}>Save credentials</button>
    ${configured && html`<button type="button" class="btn btn-danger" onClick=${() => store.mutateUsage(() => store.source.ollama({ auth_mode: 'auto', cookie: '', api_key: '' }))}>Clear credentials</button>`}
  `;
}

export function ProviderCard({ store, provider, order, providers, state, alerts, usedDisplay, refreshSec, revision }) {
  const key = provider.provider_name, tool = toolOf(key);
  const refocus = useRef(null);
  // A reorder redraws the list and carries the pressed button with its card, and a browser that cannot move a focused node (no
  // Element.moveBefore) drops focus, which resets the section's scroll to the top. Once the list is drawn again, put the button back where
  // it was pressed, as far as the section's scroll allows, so the moved card stays under the cursor and repeated clicks keep working.
  useLayoutEffect(() => {
    const pending = refocus.current;
    if (!pending) return;
    refocus.current = null;
    const button = pending.node;
    if (!button.isConnected) return;
    const section = button.closest('.settings-section');
    if (section) section.scrollTop += button.getBoundingClientRect().top - pending.top;
    button.focus({ preventScroll: true });
  });
  const switchable = isSwitchable(tool);
  const affected = (state.affected_agents || {})[tool] || [];
  const eligible = (state.eligibility || []).find(item => item.tool === tool);
  const hasActive = (state.profiles || []).some(item => item.tool === tool && item.active);
  const configured = providerConfigured(provider);
  const owner = !!state.owner;
  const setup = [['up', -1, 'arrow-up'], ['down', 1, 'arrow-down']];
  const profileAction = async (label, method) => {
    const name = await promptDialog('Profile name', { title: label, label: 'Profile name', required: true, confirm: label === 'Add profile' ? 'Add' : 'Import' });
    if (name?.trim()) store.mutateSwitcher(() => store.source[method](tool, name.trim()));
  };
  const masked = ['codex', 'claude-code', 'github-copilot', 'ollama-cloud', 'mistral'].includes(key);
  return html`<article class="usage-provider usages-provider-tile" data-provider=${key}>
    <header class="usage-provider-header usages-provider-heading"><div class="usages-provider-title">
      <span class="usages-provider-icon" aria-hidden="true">${masked ? html`<span class=${`usages-provider-mask usages-provider-mask-${key}`}></span>` : 'AI'}</span>
      <div><h4 class="usage-provider-name usages-provider-name">${provider.label || key}</h4><div class="usage-provider-status usages-provider-updated">${providerStatus(provider)}</div></div></div></header>
    <p class="usage-affected" hidden=${!switchable || !state.enabled}>${affectedText(affected)}</p>
    <div class="usage-provider-actions">
      ${switchable && state.enabled && owner && [['Add profile', 'add'], ['Import current login', 'import']].map(([label, method]) => html`<button type="button" class="btn" key=${method} onClick=${() => profileAction(label, method)}>${label}</button>`)}
      ${owner && html`
        <button type="button" class="btn" disabled=${!canEnable(provider, configured)} onClick=${() => store.mutateUsage(() => store.source.provider(key, !provider.enabled))}>${provider.enabled ? 'Disable' : 'Enable'}</button>
        ${setup.map(([direction, delta, glyph]) => html`<${IconButton} name=${glyph} label=${`Move ${provider.label || key} ${direction}`} key=${direction} disabled=${movedOrder(providers, order, delta) === null}
          onClick=${event => { refocus.current = { node: event.currentTarget, top: event.currentTarget.getBoundingClientRect().top }; store.mutateUsage(() => store.source.settings({ provider_order: movedOrder(providers, order, delta) })); }} />`)}
      `}
      ${owner && key === 'github-copilot' && html`<${CopilotSetup} store=${store} provider=${provider} key=${`copilot-${revision}`} />`}
      ${owner && key === 'mistral' && html`<${MistralSetup} store=${store} provider=${provider} key=${`mistral-${revision}`} />`}
      ${owner && key === 'ollama-cloud' && html`<${OllamaSetup} store=${store} provider=${provider} configured=${configured} key=${`ollama-${revision}`} />`}
    </div>
    <div class="usage-profiles">${(provider.profiles || []).map(profile => html`<${ProfileCard} key=${profileKey(key, profile)} store=${store} provider=${provider} profile=${profile} state=${state} usedDisplay=${usedDisplay} refreshSec=${refreshSec}
      tool=${tool} switchable=${switchable} affected=${affected} eligible=${eligible} hasActive=${hasActive} reauth=${alerts.some(alert => alert.kind === 'reauth_required' && alert.tool === tool && alert.profile === profile.profile_name && alert.role === 'store')} />`)}</div>
  </article>`;
}
