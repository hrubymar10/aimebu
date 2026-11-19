import { html } from '../../ui.js';

// The team flow wizard's offer to take over the v1 agent that held a role instead of adding a new one. The facts about the v1 agent
// (the start info the server gives for it) are the ones the Spawn dialog works from; the request carries the same `resume_legacy` and
// `confirm_v1_idle` parameters as a Spawn.

export const OFFERED_ROLES = ['leader', 'dispatcher', 'advisor'];

// The machine, tool and model choices that can continue the v1 session: its own tool, and not a container that keeps a home of its own.
export const fitsLegacy = (option, candidate) => !candidate?.resumable || (option.tool === candidate.tool && !(option.containerized && !option.shares_claude_home));
export const legacyOptions = (options, candidate) => options.filter(option => fitsLegacy(option, candidate));

// The choice the offer starts on for a v1 agent: the model v1 ran when a machine lists it (on the current machine and harness if it can), else what is chosen now when it fits, else the first model that does.
export function legacyTarget(options, candidate, current) {
  const fits = legacyOptions(options, candidate);
  const ran = fits.filter(option => candidate.model && option.model === candidate.model);
  const pick = ran.find(option => current && option.launcher_id === current.launcher_id && option.machine_id === current.machine_id) || ran.find(option => option.default_model) || ran[0]
    || fits.find(option => current && option.launcher_id === current.launcher_id && option.machine_id === current.machine_id && option.model === current.model)
    || fits.find(option => option.default_model) || fits[0];
  return pick && { machine_id: pick.machine_id, tool: pick.tool, variant: pick.variant, launcher_id: pick.launcher_id, model: pick.model };
}

// What the role's target carries when it continues a v1 agent: the agent keeps its name, so no name is sent.
export function withLegacy(target, candidate, confirmed) {
  if (!target || !candidate) return target;
  const { name, ...rest } = target;
  return { ...rest, resume_legacy: candidate.legacy_id, confirm_v1_idle: !!(candidate.resumable && candidate.v1_active && confirmed) };
}

// Whether the choice may go on: a v1 agent seen a moment ago needs the person to say v1 no longer runs it, and a session that cannot be
// continued with any tool the machines offer is no choice at all.
export const legacyBlocked = (candidate, options, confirmed) => !!candidate && (!!candidate.resumable && (!legacyOptions(options, candidate).length || candidate.v1_active && !confirmed));

export function legacyText(candidate, role) {
  const label = role.toLowerCase();
  if (!candidate.resumable) return candidate.reason || `${candidate.slug} starts fresh with the same name.`;
  return `${candidate.slug} carries on the v1 session of ${candidate.legacy_id}, working in ${candidate.cwd}.${candidate.outside_folder ? ` That folder is outside the project folder, so the owner must approve it in the Inbox before ${label === 'leader' ? 'the leader' : 'it'} starts.` : ''}`;
}

// The choice between continuing the v1 agent and adding a new one, with the confirmation a recently seen v1 session needs.
export function LegacyOffer({ role, label, candidates, picked, onPick, confirmed, onConfirm, options }) {
  const candidate = candidates.find(item => item.legacy_id === picked);
  return html`<fieldset class="team-setup-legacy"><legend>${label} source</legend>
    ${candidates.map(item => html`<label key=${item.legacy_id}><input type="radio" name=${`team-setup-${role}-source`} value=${item.legacy_id} checked=${picked === item.legacy_id} onChange=${() => onPick(item.legacy_id)} /> Continue v1 ${role} ${item.legacy_id}</label>`)}
    <label><input type="radio" name=${`team-setup-${role}-source`} value="new" checked=${picked === 'new'} onChange=${() => onPick('new')} /> New ${role}</label>
    ${candidate && html`<p class="team-wizard-current">${legacyText(candidate, label)}</p>
      ${candidate.resumable && !legacyOptions(options, candidate).length && html`<p class="error" role="alert">${candidate.tool} is not available natively on a connected machine, so the session cannot be continued here.</p>`}
      ${candidate.resumable && candidate.v1_active && html`<label><input type="checkbox" class="team-setup-legacy-confirm" checked=${!!confirmed} onChange=${event => onConfirm(event.currentTarget.checked)} /> v1 is not running this session any more</label>`}`}
  </fieldset>`;
}
