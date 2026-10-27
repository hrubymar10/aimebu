// Import-free: scripts/test-project-model.cjs runs this module in a vm.

export const money = value => `$${Number(value).toFixed(2)}`;

const SCOPE_LABELS = { active: 'Active', paused: 'Paused', stopped: 'Stopped' };

export function scopeStatus(state, id) {
  return state ? `State: ${SCOPE_LABELS[state.effective_state] || state.effective_state}` : id ? 'Loading control state…' : '';
}

export function scopeButtons(state, { busy, retryResume }) {
  const off = action => busy || !state || (action === 'pause' && state.desired !== 'active') || (action === 'stop' && state.desired === 'stopped')
    || (action === 'resume' && state.desired === 'active' && !retryResume);
  return { pause: { disabled: off('pause') }, stop: { disabled: off('stop') }, resume: { disabled: off('resume'), label: state?.desired === 'active' ? 'Retry resume' : 'Resume' } };
}

export function scopeFailures(next, action) {
  const failures = (next.results || []).filter(result => result.error).map(result => result.error);
  return { retryResume: action === 'resume' && failures.length > 0, text: failures.length ? `Agent controls needing attention: ${failures.join('; ')}.${action === 'resume' ? ' Retry Resume after the agent exits.' : ''}` : '' };
}

export function limitDraft(state) {
  return { costMode: state.cost_mode, cost: state.cost_mode === 'custom' ? String(state.daily_cost_cap ?? '') : '', turnMode: state.turn_mode, turn: state.turn_mode === 'custom' ? String(state.daily_turn_cap ?? '') : '' };
}

export const inheritText = (cap, unit) => `Inherit (default: ${cap == null ? 'no limit' : unit === 'cost' ? money(cap) : `${cap} turns`})`;

export function limitSummary(state, id) {
  if (!state) return id ? 'Loading today’s usage…' : '';
  const day = new Date(state.day_start).toLocaleDateString();
  const costLimit = state.daily_cost_cap == null ? 'no cap' : `${money(state.daily_cost_cap)} cap`;
  const turnLimit = state.daily_turn_cap == null ? 'no cap' : `${state.daily_turn_cap} cap`;
  const coverage = state.cost_coverage === 'known' ? 'reported' : state.cost_coverage === 'estimated' ? 'estimated' : state.cost_coverage === 'partial' ? (state.turns_with_estimated_cost ? 'partly estimated' : 'partly reported') : 'unreported';
  return `${day} (${state.tz}), all rooms: ${state.daily_turns} turns / ${turnLimit}; ${money(state.daily_cost)} ${coverage} cost / ${costLimit}. Dispatch: ${state.effective_state}.`;
}

export function limitRequest(draft, rev) {
  const costCap = draft.costMode === 'custom' ? Number(draft.cost) : null;
  const turnCap = draft.turnMode === 'custom' ? Number(draft.turn) : null;
  if (draft.costMode === 'custom' && (draft.cost.trim() === '' || !Number.isFinite(costCap) || costCap <= 0) ||
      draft.turnMode === 'custom' && (draft.turn.trim() === '' || !Number.isSafeInteger(turnCap) || turnCap <= 0)) {
    return { problem: 'Enter a positive value for each custom limit, or choose Inherit or No limit.' };
  }
  return { body: { cost_mode: draft.costMode, daily_cost_cap: costCap, turn_mode: draft.turnMode, daily_turn_cap: turnCap, rev } };
}

export function detailsView(project, admin) {
  const canName = !!project && (project.role === 'owner' || admin), canFolder = !!project && admin;
  return { canName, canFolder, archived: !!project?.archived_at,
    status: !project || canFolder ? '' : canName ? 'Only an admin can change the folder.' : 'Only the project owner can change these.' };
}

// Only what changed goes to the server: a project from before folders were checked can still be renamed.
// A project from before folders were checked can still be renamed, so only what changed is sent.
export function detailsChanges(drafts, loaded, view) {
  const body = {};
  if (view.canName && drafts.name !== loaded.name) body.name = drafts.name;
  if (view.canFolder && drafts.folder !== loaded.folder) body.folder = drafts.folder;
  return body;
}

export function lifecycleView(project, allowed) {
  const archived = !!project?.archived_at;
  return { show: !!project, archive: !!project && !archived, reopen: !!project && archived, remove: !!project && archived, importer: !!project && !archived,
    note: !project ? '' : !allowed ? 'Only the project owner or an admin can archive, reopen or delete a project.' : archived ? `Archived ${new Date(project.archived_at).toLocaleString()}. It is read-only.` : '' };
}

export function stopFailedNote(failed) {
  return `Archived. ${failed.length} agent${failed.length === 1 ? '' : 's'} could not be stopped: ${failed.map(item => item.error).join('; ')}. Choose Abandon on its recovery item in the Inbox: that stops it. The project cannot be deleted before every agent has stopped.`;
}
