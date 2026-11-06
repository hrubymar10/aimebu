import { html } from '../../ui.js';
import { IconButton } from '../../components/icon-button.js';
import { confirmDialog, promptDialog } from '../../dialogs.js';
import { creditsText } from '../../usage-format.js';
import { UsageWindow } from './usage-window.js';
import { impactText, profileStatusLine, switchView } from './model.js';

const time = date => date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });

export function ProfileCard({ store, provider, profile, state, usedDisplay, tool, switchable, affected, eligible, hasActive }) {
  const name = profile.profile_name;
  const configured = (state.profiles || []).some(item => item.tool === tool && item.name === name);
  const view = switchView({ switchable, enabled: state.enabled, owner: state.owner, configured, profile, hasActive, eligible });
  const manage = !switchable || !state.enabled || !state.owner || !configured;
  const status = profileStatusLine(profile, time);
  const credits = profile.credits ? creditsText(profile.credits) : '';
  const use = async () => {
    if (await confirmDialog(`${impactText(affected)}${profile.has_credentials ? '' : ' This profile needs a new login.'} Switch to ${name}?`, { title: 'Switch account', confirm: 'Switch' })) store.mutateSwitcher(() => store.source.use(tool, name));
  };
  const rename = async () => {
    const next = (await promptDialog(`Rename ${name} to`, { title: 'Rename profile', label: 'New name', value: name, required: true, confirm: 'Rename' }))?.trim();
    if (next && next !== name) store.mutateSwitcher(() => store.source.rename(tool, name, next));
  };
  const remove = async () => {
    if (await confirmDialog(`Delete ${name}?${profile.active ? ' This clears the live login.' : ''}`, { title: 'Delete profile', confirm: 'Delete', danger: true })) store.mutateSwitcher(() => store.source.remove(tool, name));
  };
  return html`<section class=${`usage-profile usages-popover-profile switcher-tile-profile${profile.active ? ' is-active' : ''}`}>
    <div class="usage-profile-header switcher-tile-head">
      <strong class="usage-profile-name switcher-tile-name">${name || 'local'}</strong>
      <button type="button" class="btn usage-switch switcher-switch-pill" hidden=${view.hidden} disabled=${view.disabled} title=${view.title} onClick=${use}>Switch</button>
      <span class="usage-profile-active switcher-tile-active" hidden=${!profile.active}>${profile.active ? 'Active' : ''}</span>
      <span class="usage-profile-plan switcher-tile-plan" title=${profile.plan || ''} hidden=${!profile.plan}>${profile.plan || ''}</span>
    </div>
    <p class="usage-profile-email" hidden=${!profile.email}>${profile.email || ''}</p>
    <div class="usage-windows">${(profile.windows || []).map((window, index) => html`<${UsageWindow} window=${window} usedDisplay=${usedDisplay} fetchedAt=${profile.last_refresh_at} key=${`${window.key}:${index}`} />`)}</div>
    <p class="usage-profile-credits usages-credits-row" hidden=${!credits}>${credits}</p>
    <p class="usage-profile-status" hidden=${!status}>${status}</p>
    <div class="usage-profile-actions" hidden=${manage}>
      <${IconButton} name="edit" label=${`Rename ${name}`} className="usage-rename" hidden=${manage} onClick=${rename} />
      <${IconButton} name="trash" label=${`Delete ${name}`} danger className="usage-remove" hidden=${manage} onClick=${remove} />
    </div>
  </section>`;
}
