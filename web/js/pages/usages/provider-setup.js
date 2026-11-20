import { html, Fragment, useRef, useState } from '../../ui.js';
import { ollamaBody, flowText } from './model.js';

// The verification address comes from GitHub (or an Enterprise host), and only an https address becomes a link.
export function httpsHref(address) {
  try { const url = new URL(address); return url.protocol === 'https:' && !url.username && !url.password ? url.href : ''; } catch { return ''; }
}

// The forms rebuild on every read of the panel (they are keyed by the store's revision), and a read can come between choosing and saving
// (a live push, another provider's save). What a person chooses here is kept as the store's draft, which a rebuilt form starts from, so the
// stored value cannot be put back over it; the stored value only seeds a form nobody has touched.
function useDraft(store, name, stored) {
  const [value, setValue] = useState(() => store.drafts[name] ?? stored);
  return [value, next => { store.drafts[name] = next; setValue(next); }];
}

export function CopilotSetup({ store, provider }) {
  const [host, setHost] = useDraft(store, 'copilotHost', provider.enterprise_host || '');
  const flow = store.flow.value;
  const signIn = () => { delete store.drafts.copilotHost; return store.startCopilot(host); };
  return html`
    <input class="settings-text-input" type="url" placeholder="GitHub Enterprise host (optional)" value=${host} onInput=${event => setHost(event.currentTarget.value)} />
    <button type="button" class="btn btn-primary" onClick=${signIn}>Sign in with GitHub</button>
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
  const [mode, setMode] = useDraft(store, 'ollamaMode', provider.auth_mode || 'auto');
  const cookie = useRef(null), key = useRef(null);
  const save = body => store.mutateUsage(async () => { await store.source.ollama(body); delete store.drafts.ollamaMode; });
  return html`
    <select class="settings-select" aria-label="Ollama Cloud authentication mode" value=${mode} onChange=${event => setMode(event.currentTarget.value)}><option value="auto">Auto</option><option value="cookie">Cookie</option><option value="api_key">API key</option></select>
    <textarea class="settings-text-input settings-textarea" placeholder="Paste Ollama Cookie request header" rows="2" aria-label="Ollama Cloud Cookie header" ref=${cookie}></textarea>
    <input class="settings-text-input" type="password" placeholder="Ollama API key" aria-label="Ollama Cloud API key" ref=${key} />
    <button type="button" class="btn btn-primary" onClick=${() => save(ollamaBody(mode, cookie.current.value, key.current.value))}>Save credentials</button>
    ${configured && html`<button type="button" class="btn btn-danger" onClick=${() => save({ auth_mode: 'auto', cookie: '', api_key: '' })}>Clear credentials</button>`}
  `;
}
