import { html, useLayoutEffect, useRef } from '../../ui.js';
import { currentUser } from '../../api.js';
import { confirmDialog } from '../../dialogs.js';
import { SettingsGroup, SettingsRow } from '../../components/settings-row.js';
import { IconButton } from '../../components/icon-button.js';
import { ROLES, accountActions, memberRemoval, uniqueAccounts } from './model.js';

const roleOptions = () => ROLES.map(value => html`<option value=${value}>${value[0].toUpperCase() + value.slice(1)}</option>`);

function MemberRow({ store, user, members }) {
  const removal = memberRemoval(user, members, currentUser);
  const change = event => { const select = event.currentTarget; store.setRole(user, select.value, () => { select.value = user.role; }); };
  const remove = async () => {
    if (!await confirmDialog(`Remove ${user.login} from this project?`, { title: 'Remove member', confirm: 'Remove', danger: true })) return;
    store.removeMember(user);
  };
  return html`<div class="user-row user-member-row"><strong>${user.login}</strong><span>${user.disabled ? 'Deactivated' : ''}</span>
    <select class="settings-select" aria-label=${`Role for ${user.login}`} value=${user.role} onChange=${change}>${roleOptions()}</select>
    <${IconButton} name="trash" label=${removal.disabled ? removal.reason : `Remove ${user.login} from this project`} danger disabled=${removal.disabled} onClick=${remove} /></div>`;
}

function AccountRow({ store, user }) {
  const can = accountActions(user, { globalOwner: store.globalOwner, currentUser });
  // Admin is trusted like the owner (docs/auth.md), so granting or taking it away is asked first; cancelling puts the box back.
  const toggle = async event => {
    const box = event.currentTarget, grant = box.checked;
    const confirmed = await confirmDialog(grant
      ? `Make ${user.login} an admin? An admin is trusted like you: they can change the roles, prompts, macros and memory that shape what every agent is told, create projects in any folder on this machine, manage harnesses, and invite or deactivate ordinary accounts. They cannot reach your own account, other admins, machine pairing or provider logins.`
      : `Remove admin from ${user.login}? They go back to an ordinary account: no instance settings (roles, prompts, macros, harnesses), no new projects and no account management. Their project memberships stay.`,
      { title: grant ? 'Make admin' : 'Remove admin', confirm: grant ? 'Make admin' : 'Remove admin', danger: grant });
    if (!confirmed) { box.checked = !grant; return; }
    store.setAdmin(user, grant, () => { box.checked = !grant; });
  };
  const deactivate = async () => {
    if (!await confirmDialog(`Deactivate ${user.login} across all projects?`, { title: 'Deactivate account', confirm: 'Deactivate', danger: true })) return;
    store.deactivate(user);
  };
  return html`<div class="user-row" data-user-id=${user.id}><strong>${user.login}</strong><span>${user.disabled ? 'Deactivated' : ''}</span>
    ${can.badge && !(can.badge === 'Admin' && can.adminToggle) && html`<span class=${`user-badge user-badge-${can.badge.toLowerCase()}`}>${can.badge}</span>`}
    ${can.adminToggle && html`<label class="user-admin-toggle"><input type="checkbox" checked=${!!user.admin} aria-label=${`${user.login} is an admin`} onChange=${toggle} /> Admin</label>`}
    ${can.reset && html`<button type="button" class="btn" onClick=${() => store.resetLink(user)}>Password reset link</button>`}
    ${can.deactivate && html`<button type="button" class="btn btn-danger" onClick=${deactivate}>Deactivate</button>`}</div>`;
}

export function UsersPage({ store }) {
  const link = useRef(null), shown = store.link.value;
  useLayoutEffect(() => { if (shown) link.current?.select(); }, [shown]);
  const members = store.members.value, accounts = store.accounts.value, allowed = store.allowed.value;
  return html`<h3>People and project access</h3>
    <p class="error" role="alert">${store.error.value}</p>
    <${SettingsGroup} label="Project selection"><${SettingsRow} id="users-project" label="Project" desc="The project whose members and invitations are shown below.">
      <select class="settings-select" value=${store.project.value} onChange=${event => store.select(event.currentTarget.value)}>${allowed.map(project => html`<option value=${project.id}>${project.name}</option>`)}</select><//><//>
    <form class="user-invite-form" onSubmit=${event => { event.preventDefault(); store.createInvite(); }}>
      <${SettingsGroup} label="Invitation"><${SettingsRow} id="users-invite-role" label="Invite as" desc="Access the invited person gets: viewer reads, operator can also run agents, owner manages the project and its people. Creates a one-time invitation link.">
        <select class="settings-select" value=${store.role.value} onChange=${event => { store.role.value = event.currentTarget.value; }}>${roleOptions()}</select><//><//>
      <div class="settings-actions"><button type="submit" class="btn btn-primary">Create invite</button></div>
    </form>
    <input class="settings-text-input" readonly hidden=${!shown} aria-label="Invitation link" value=${shown?.url || ''} ref=${link} />
    <p class="settings-intro invite-link-hint" hidden=${!shown?.hint}>${shown?.hint || ''}</p>
    <div class="user-members">${members && html`<h4>Project members</h4>${members.map(user => html`<${MemberRow} key=${user.id} store=${store} user=${user} members=${members} />`)}`}</div>
    <div class="user-accounts">${accounts && store.globalAdmin && html`<h4>Accounts</h4>${uniqueAccounts(accounts).map(user => html`<${AccountRow} key=${user.id} store=${store} user=${user} />`)}`}</div>`;
}
