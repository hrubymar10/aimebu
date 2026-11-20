import { html, Fragment, useRef } from '../../ui.js';
import { ollamaBody, flowText } from './model.js';

// The verification address comes from GitHub (or an Enterprise host), and only an https address becomes a link.
export function httpsHref(address) {
  try { const url = new URL(address); return url.protocol === 'https:' && !url.username && !url.password ? url.href : ''; } catch { return ''; }
}

export function CopilotSetup({ store, provider }) {
  const host = useRef(null);
  const flow = store.flow.value;
  return html`
    <input class="settings-text-input" type="url" placeholder="GitHub Enterprise host (optional)" ref=${host} value=${provider.enterprise_host || ''} />
    <button type="button" class="btn btn-primary" onClick=${() => store.startCopilot(host.current.value)}>Sign in with GitHub</button>
    ${provider.token_configured && html`<button type="button" class="btn" onClick=${() => store.mutateUsage(() => store.source.copilotLogout())}>Sign out</button>`}
    <div class="usage-copilot-flow">${flow && html`<${Fragment}><span>${flowText(flow)}</span>${httpsHref(flow.verification_uri) && html`<a href=${httpsHref(flow.verification_uri)} target="_blank" rel="noopener noreferrer">Open GitHub verification</a>`}<//>`}</div>
  `;
}

export function MistralSetup({ store, provider }) {
  const cookie = useRef(null);
  return html`
    <textarea class="settings-text-input settings-textarea" placeholder="Paste Mistral Cookie request header" rows="2" aria-label="Mistral Cookie header" ref=${cookie}></textarea>
    <button type="button" class="btn btn-primary" onClick=${() => store.mutateUsage(() => store.source.mistral(cookie.current.value))}>${provider.cookie_configured ? 'Update cookie' : 'Save cookie'}</button>
    ${provider.cookie_configured && html`<button type="button" class="btn btn-danger" onClick=${() => store.mutateUsage(() => store.source.mistral(''))}>Clear cookie</button>`}
  `;
}

export function OllamaSetup({ store, provider, configured }) {
  const mode = useRef(null), cookie = useRef(null), key = useRef(null);
  return html`
    <select class="settings-select" aria-label="Ollama Cloud authentication mode" ref=${mode} value=${provider.auth_mode || 'auto'}><option value="auto">Auto</option><option value="cookie">Cookie</option><option value="api_key">API key</option></select>
    <textarea class="settings-text-input settings-textarea" placeholder="Paste Ollama Cookie request header" rows="2" aria-label="Ollama Cloud Cookie header" ref=${cookie}></textarea>
    <input class="settings-text-input" type="password" placeholder="Ollama API key" aria-label="Ollama Cloud API key" ref=${key} />
    <button type="button" class="btn btn-primary" onClick=${() => store.mutateUsage(() => store.source.ollama(ollamaBody(mode.current.value, cookie.current.value, key.current.value)))}>Save credentials</button>
    ${configured && html`<button type="button" class="btn btn-danger" onClick=${() => store.mutateUsage(() => store.source.ollama({ auth_mode: 'auto', cookie: '', api_key: '' }))}>Clear credentials</button>`}
  `;
}
