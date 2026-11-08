import { html, useLayoutEffect, useRef } from '../../ui.js';
import { THEMES } from '../general/model.js';
import { MIN_PASSWORD } from './model.js';

const submitter = send => event => { event.preventDefault(); send(event.currentTarget.elements); };

export function TokenStep({ store }) {
  const input = useRef(null), shown = store.step.value === 'token', focus = store.tokenFocus.value, linked = store.tokenValue.value, path = store.hintPath.value;
  useLayoutEffect(() => { if (linked) input.current.value = linked; }, [linked]);
  useLayoutEffect(() => { if (shown) { input.current.focus(); if (focus.select) input.current.select(); } }, [shown, focus.n]);
  return html`<form id="setup-token-form" noValidate onSubmit=${submitter(fields => store.submitToken(fields.token.value))}>
    <h1>Enter the setup token to continue</h1>
    <p>This aimebu has no owner yet. The server printed a one-time setup token when it started.</p>
    <p id="setup-token-hint" class="settings-desc">Find it in the output of the server${path ? html`, or in the file <code>${path}</code>` : null}.</p>
    <label>Setup token<input name="token" autocomplete="off" spellcheck="false" required ref=${input} /></label>
    <button type="submit" class="btn btn-primary" disabled=${store.busy.token.value}>Continue</button>
    <p id="setup-token-error" class="error" role="alert">${store.tokenError.value}</p>
  </form>`;
}

export function OwnerStep({ store }) {
  const login = useRef(null), shown = store.step.value === 'owner';
  useLayoutEffect(() => { if (shown) login.current.focus(); }, [shown]);
  return html`<form id="setup-form" onSubmit=${submitter(fields => store.submitOwner(fields.login.value, fields.password.value, fields.confirm.value))}>
    <h1>Create owner</h1>
    <p>Choose the account that owns this aimebu. You will sign in with it.</p>
    <label>Login<input name="login" autocomplete="username" required minlength="3" maxlength="32" ref=${login} aria-describedby="setup-login-hint" /></label>
    <p class="setup-hint" id="setup-login-hint">3 to 32 lowercase letters, digits, - or _, starting with a letter.</p>
    <label>Password<input name="password" type="password" autocomplete="new-password" required minlength="12" aria-describedby="setup-password-hint" /></label>
    <p class="setup-hint" id="setup-password-hint">At least ${MIN_PASSWORD} characters.</p>
    <label>Repeat password<input name="confirm" type="password" autocomplete="new-password" required minlength="12" /></label>
    <button type="submit" class="btn btn-primary" disabled=${store.busy.owner.value}>Create owner</button>
    <p id="setup-error" class="error" role="alert">${store.ownerError.value}</p>
  </form>`;
}

function Tile({ store, value, label }) {
  const selected = store.theme.value === value;
  const press = event => {
    const next = store.moveTheme(event.key);
    if (!next) return;
    event.preventDefault(); event.currentTarget.parentNode.querySelector(`[data-choice="${next}"]`)?.focus();
  };
  return html`<button type="button" class="theme-tile" data-theme=${value} data-choice=${value} role="radio" aria-checked=${String(selected)} tabindex=${selected ? 0 : -1} onClick=${() => store.select(value)} onKeyDown=${press}>
    <span class="theme-preview" aria-hidden="true"><span class="theme-preview-bar"></span><span class="theme-preview-line"></span><span class="theme-preview-line short"></span><span class="theme-preview-accent"></span></span>
    <span class="theme-label">${label}</span></button>`;
}

export function ThemeStep({ store }) {
  const grid = useRef(null), shown = store.step.value === 'theme';
  useLayoutEffect(() => { if (shown) grid.current.querySelector('[aria-checked=true]')?.focus(); }, [shown]);
  return html`<section><h1>Choose a theme</h1><p>Pick how aimebu looks. You can change it later in Settings.</p>
    <div class="theme-grid" role="radiogroup" aria-label="Theme" ref=${grid}>${THEMES.map(([value, label]) => html`<${Tile} key=${value} store=${store} value=${value} label=${label} />`)}</div>
    <button type="button" class="btn btn-primary theme-continue" onClick=${() => store.continueTheme()}>Continue</button></section>`;
}
