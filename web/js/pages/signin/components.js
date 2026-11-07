import { html, useLayoutEffect, useRef } from '../../ui.js';
import { cardView, dialogAsksPassword } from './model.js';

const submitter = send => event => { event.preventDefault(); const fields = event.currentTarget.elements; send(fields); };

export function LoginForm({ store }) {
  return html`<form id="login-form" hidden=${store.mode.value !== 'login'} onSubmit=${submitter(fields => store.submitLogin(fields.login.value, fields.password.value))}>
    <h1>aimebu</h1><p>Sign in to your workspace</p>
    <label>Login<input name="login" autocomplete="username" required /></label>
    <label>Password<input name="password" type="password" autocomplete="current-password" required /></label>
    <button class="btn btn-primary">Sign in</button>
    <p id="login-error" class="error" role="alert">${store.loginError.value}</p>
    <button type="button" class="btn invite-back" hidden=${!store.back.value} onClick=${() => store.toInvite()}>Back to the invitation</button>
  </form>`;
}

function LoginField({ view }) {
  const input = useRef(null);
  useLayoutEffect(() => { if (view.loginValue !== undefined) input.current.value = view.loginValue; }, [view.loginValue]);
  return html`<label class="invite-login" hidden=${!view.fields}>${view.loginLabel}<input name="login" autocomplete="username" ref=${input} readOnly=${view.readOnly} required=${view.loginRequired} /></label>`;
}

export function InviteCard({ store }) {
  const view = cardView(store.info.value, store.redeemed.value);
  return html`<form class="invite-entry" hidden=${store.mode.value !== 'card'} onSubmit=${submitter(fields => store.submitCard(fields.login.value, fields.password.value))}>
    <h1>aimebu</h1><p class="invite-lead">${view.lead}</p>
    <${LoginField} view=${view} />
    <label hidden=${!view.fields}>Password<input name="password" type="password" autocomplete="new-password" required minlength="12" /></label>
    <button type="submit" class="btn btn-primary" hidden=${!view.fields}>${view.submitText}</button>
    <p class="error" role="alert">${store.cardError.value}</p>
    <button type="button" class="btn invite-switch" hidden=${view.switchHidden} onClick=${() => store.toLogin()}>${view.switchText}</button>
  </form>`;
}

export function SigninPage({ store }) {
  return html`<${LoginForm} store=${store} /><${InviteCard} store=${store} />`;
}

export function InviteDialogBody({ store }) {
  return html`<h2>Use invitation</h2><p>Join this project with your signed-in account. ${dialogAsksPassword(store.info.value) ? ' If this is a password reset link, enter a new password.' : ''}</p>
    <form onSubmit=${submitter(fields => store.submitDialog(fields.password.value))}><label hidden=${!dialogAsksPassword(store.info.value)}>New password<input name="password" type="password" autocomplete="new-password" /></label><div class="dialog-actions"><button type="button" class="btn invite-cancel" onClick=${() => store.closeDialog()}>Cancel</button><button type="submit" class="btn btn-primary">Use invitation</button></div></form>
    <p class="error" role="alert">${store.dialogError.value}</p>`;
}
