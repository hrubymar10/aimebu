import { confirmDialog, promptDialog } from '../../dialogs.js';
import { impactText } from './model.js';

// What a profile's Switch, Install anyway, Rename and Delete do: each asks first, then writes through the store and reads again. Shared by the
// profile cards of variants A and B, so the wording and the order of asking stay the same in both.
export function profileActions({ store, tool, profile, affected }) {
  const name = profile.profile_name;
  const use = async () => {
    if (await confirmDialog(`${impactText(affected)}${profile.has_credentials ? '' : ' This profile needs a new login.'} Switch to ${name}?`, { title: 'Switch account', confirm: 'Switch' })) store.mutateSwitcher(() => store.source.use(tool, name));
  };
  const installAnyway = async () => {
    if (await confirmDialog(`${impactText(affected)} Install ${name}'s stored credentials without refreshing? They may be expired; sign in again after switching.`, { title: 'Install expired account', confirm: 'Install anyway' })) store.mutateSwitcher(() => store.source.use(tool, name, true));
  };
  const rename = async () => {
    const next = (await promptDialog(`Rename ${name} to`, { title: 'Rename profile', label: 'New name', value: name, required: true, confirm: 'Rename' }))?.trim();
    if (next && next !== name) store.mutateSwitcher(() => store.source.rename(tool, name, next));
  };
  const remove = async () => {
    if (await confirmDialog(`Delete ${name}?${profile.active ? ' This clears the live login.' : ''}`, { title: 'Delete profile', confirm: 'Delete', danger: true })) store.mutateSwitcher(() => store.source.remove(tool, name));
  };
  return { use, installAnyway, rename, remove };
}
