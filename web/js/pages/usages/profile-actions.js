import { confirmDialog, promptDialog } from '../../dialogs.js';
import { impactText } from './model.js';
import { maskName } from '../../usage-privacy.js';

// What a profile's Switch, Install anyway, Rename and Delete do: each asks first, then writes through the store and reads again. Shared by the
// profile cards of variants A and B, so the wording and the order of asking stay the same in both.
// warmAgentsText names the agents a warm_agents refusal lists (slug@project), or says "some agents" when the server could not name them.
export function warmAgentsText(failure) {
  const names = (failure?.body?.agents || []).map(agent => agent.label || agent.id).filter(Boolean);
  return names.length ? names.join(', ') : 'some agents';
}

// switchPastWarm switches; when the host's warm agents block it (409 warm_agents, after the server's own wait) and the server offers force, it asks
// whether to switch anyway, warning what that means, and switches with force_warm if so. Declined, or not offered, the refusal stands, and its
// message (which names the agents) is the red error text.
export async function switchPastWarm(store, tool, name, shown = () => name) {
  try {
    return await store.source.use(tool, name);
  } catch (failure) {
    if (failure?.code !== 'warm_agents' || !failure.body?.can_force) throw failure;
    const agents = warmAgentsText(failure);
    const go = await confirmDialog(`These agents kept warm could not be confirmed ended: ${agents}. Their old process may still hold the current login. Switch to ${shown()} anyway? They are quarantined (no new turn) until their process is confirmed ended, at most 10 minutes.`,
      { title: 'Agents still kept warm', confirm: 'Switch anyway', danger: true });
    if (!go) throw failure;
    return store.source.use(tool, name, false, true);
  }
}

export function profileActions({ store, tool, profile, affected }) {
  const name = profile.profile_name;
  // The dialogs' display text shows the masked name while B's privacy is on (read when asked, so a toggle flip counts); the real name still goes to the store and into Rename's input.
  const shown = () => maskName(name);
  const use = async () => {
    if (await confirmDialog(`${impactText(affected)}${profile.has_credentials ? '' : ' This profile needs a new login.'} Switch to ${shown()}?`, { title: 'Switch account', confirm: 'Switch' })) store.mutateSwitcher(() => switchPastWarm(store, tool, name, shown));
  };
  const installAnyway = async () => {
    if (await confirmDialog(`${impactText(affected)} Install ${shown()}'s stored credentials without refreshing? They may be expired; sign in again after switching.`, { title: 'Install expired account', confirm: 'Install anyway' })) store.mutateSwitcher(() => store.source.use(tool, name, true));
  };
  const rename = async () => {
    const next = (await promptDialog(`Rename ${shown()} to`, { title: 'Rename profile', label: 'New name', value: name, required: true, confirm: 'Rename' }))?.trim();
    if (next && next !== name) store.mutateSwitcher(() => store.source.rename(tool, name, next));
  };
  const remove = async () => {
    if (await confirmDialog(`Delete ${shown()}?${profile.active ? ' This clears the live login.' : ''}`, { title: 'Delete profile', confirm: 'Delete', danger: true })) store.mutateSwitcher(() => store.source.remove(tool, name));
  };
  return { use, installAnyway, rename, remove };
}
