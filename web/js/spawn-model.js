// Picking a model in the Spawn dialog. The model list is rebuilt whenever the catalog arrives or the tool changes, and the person's own
// choice must survive that; a model that only a name is known for (a v1 agent's "opus5.5", a Claude session's "claude-opus-4-5-20251101")
// is matched to the list by its family when it is not listed verbatim. No DOM here, so the rules can be tested.

const FAMILIES = ['opus', 'sonnet', 'haiku'];

// The Claude family a model name belongs to ("opus", "sonnet", "haiku"), or ''.
export const modelFamily = name => FAMILIES.find(family => String(name || '').toLowerCase().includes(family)) || '';

// The version a model name carries ("4.5" for claude-opus-4-5-20251101, opus5.5 and claude-3-5-sonnet-20241022), or '' for a name with none (the
// aliases "opus", "sonnet", "haiku", which mean whatever is current). The date at the end of an id is not a version.
export function modelVersion(name) {
  const text = String(name || '').toLowerCase();
  const family = modelFamily(text);
  if (!family) return '';
  const after = text.slice(text.indexOf(family) + family.length).match(/^[-_ ]?(\d{1,2})(?!\d)(?:[-._](\d{1,2})(?!\d))?/);
  if (after) return after[2] ? `${after[1]}.${after[2]}` : after[1];
  const before = text.slice(0, text.indexOf(family)).match(/(\d{1,2})[-._](\d{1,2})[-_ ]?$/);
  return before ? `${before[1]}.${before[2]}` : '';
}

// matchModel picks the entry of `models` ([{ value, label }]) that `wanted` names: the same id, the same id ignoring case, or else an entry of
// the same Claude family and the same version (an entry with no version, an alias, stands for the current one and is taken when none has the
// version). An entry of another version is never taken: a session recorded on one model is not silently moved to another. '' when nothing
// fits, so the caller keeps the list's own default.
export function matchModel(models, wanted) {
  if (!wanted) return '';
  const exact = models.find(model => model.value === wanted);
  if (exact) return exact.value;
  const lower = String(wanted).toLowerCase();
  const loose = models.find(model => model.value.toLowerCase() === lower);
  if (loose) return loose.value;
  const family = modelFamily(wanted);
  if (!family) return '';
  const same = models.filter(model => modelFamily(model.value) === family || modelFamily(model.label) === family);
  const version = modelVersion(wanted);
  if (!version) return same[0]?.value || '';
  const versioned = same.find(model => modelVersion(model.value) === version || modelVersion(model.label) === version);
  if (versioned) return versioned.value;
  const alias = same.find(model => !modelVersion(model.value) && !modelVersion(model.label));
  return alias ? alias.value : '';
}

// The model to select after the list was rebuilt: what the person picked for this tool when it is still offered, else the one named by the
// start (the v1 agent's or the session's), else the list's own default, else the first.
export function chooseModel(models, { picked = null, toolKey = '', named = '', preferred = '' } = {}) {
  if (picked && picked.toolKey === toolKey && models.some(model => model.value === picked.model)) return picked.model;
  return matchModel(models, named) || (models.some(model => model.value === preferred) ? preferred : '') || models[0]?.value || '';
}

// The key of a tool entry of the catalog, which says whether the tool is still the same after a reload.
export const toolKey = tool => (tool ? tool.launcher_id || `${tool.tool || ''}/${tool.variant || ''}` : '');

// The session entry of the Spawn dialog. A Claude Code session can only be continued through a launcher that sees the Claude home it sits in:
// the native one, or a container launcher that says it shares the host's (harness-docker's claude_home_shared). With another one selected the
// dialog says so up front, and offers the launcher that can, instead of listing sessions that fail once picked.
// Returns { enabled, note, switchTo } (switchTo is the catalog index of the launcher to use, -1 when there is none).
export function sessionEntryState(selected, catalog) {
  const canContinue = tool => tool.tool === 'claude' && tool.available && (!tool.containerized || tool.shares_claude_home);
  if (!selected || selected.tool !== 'claude' || !selected.containerized || selected.shares_claude_home) return { enabled: true, note: '', switchTo: -1 };
  const label = selected.label || 'This launcher';
  const switchTo = catalog.findIndex(canContinue);
  if (switchTo >= 0) return { enabled: false, note: `${label} keeps its own Claude home, so a session cannot be continued through it.`, switchTo };
  return { enabled: false, note: `${label} keeps its own Claude home, so a session cannot be continued through it. Sessions need the native Claude Code harness, or a Docker launcher that shares Claude's home (harness-docker's claude_home_shared); add one under Settings → Harnesses.`, switchTo: -1 };
}

// A warning whenever the model chosen for the agent is not the one the session ran on, versions included, so a session is not continued on
// another model unnoticed. `sessionModel` is what Claude recorded ("claude-opus-4-5-20251101") or a v1 slug ("opus5.5"); `chosen` is
// { value, label }. The same id, or the same family and version, is no change; an alias of the same family may be another version.
export function modelChangeWarning(sessionModel, chosen) {
  const was = modelFamily(sessionModel);
  if (!was || !chosen) return '';
  if (chosen.value === sessionModel || String(chosen.value).toLowerCase() === String(sessionModel).toLowerCase()) return '';
  const now = modelFamily(chosen.value) || modelFamily(chosen.label);
  const named = chosen.label || chosen.value;
  if (now === was) {
    const version = modelVersion(sessionModel), chosenVersion = modelVersion(chosen.value) || modelVersion(chosen.label);
    if (version && chosenVersion && version === chosenVersion) return '';
    if (!chosenVersion) return `This session ran on ${sessionModel}; the agent will continue it on ${named}, an alias that may be another version.`;
  }
  return `This session ran on ${sessionModel}; the agent will continue it on ${named}.`;
}

// The "Start in" value that asks for a room of its own, for a continued Claude Code session.
export const NEW_ROOM = '__new__';

const HARNESSES = 'Settings → Harnesses';

// One row of the tool list: the entry's index as its value, and for an unavailable one why, with where to look.
export function toolRows(catalog) {
  return (catalog || []).map((tool, index) => {
    const detail = tool.unavailable_reason || 'unavailable';
    const help = detail.includes(HARNESSES) ? detail : `${detail}; check ${HARNESSES}`;
    return { value: String(index), label: `${tool.label || tool.tool}${tool.available ? '' : ` — ${help}`}`, disabled: !tool.available };
  });
}

// The v1 tool is picked when this machine offers it. A session that can be continued needs that tool natively, or in a Docker launcher that
// shares the host's Claude home (the catalog says so: shares_claude_home), since any other container has a home of its own and cannot see
// it. Only those entries stay selectable and the first of them is picked, whatever order the catalog lists them in. A fresh start only
// preselects the v1 tool; the others stay available. `index` is null when the choice is left as it was and -1 when nothing may be picked.
export function resumeToolChoice(catalog, resume) {
  if (!resume?.tool) return { index: null, disabled: null, note: '' };
  const fits = tool => tool.tool === resume.tool && tool.available && !(resume.resumable && tool.containerized && !tool.shares_claude_home);
  // A continued session was listed from one launcher's Claude home: that launcher is picked when it is still offered, else the first fit.
  const preferred = resume.launcher_id ? catalog.findIndex(tool => fits(tool) && tool.launcher_id === resume.launcher_id) : -1;
  const found = preferred >= 0 ? preferred : catalog.findIndex(fits);
  const none = found < 0 && !!resume.resumable;
  return { index: found >= 0 ? found : none ? -1 : null, disabled: resume.resumable ? catalog.map(tool => !fits(tool)) : null,
    note: none ? `${resume.tool} is not available natively on the chosen machine, so the session cannot be continued here.` : '' };
}

// The worktree choice exists only in a task room, and is offered only where the server would make one: the machine's row carries the
// server's own check for the host (with git's reason when it refuses), and is silent for other machines, where the box stays available.
export function worktreeOption({ roomKind, resumable, check }) {
  const refused = check?.ok === false;
  return { hidden: roomKind !== 'task' || !!resumable, disabled: refused, refused, note: refused ? `No worktree here: ${check.reason || 'the project folder cannot have one.'}` : '' };
}

// A hint that names Settings → Harnesses makes that name a button that opens it, so an empty machine says where to go. Only an owner or admin
// can add or change a harness, so anyone else gets plain words (the link would only end in a refusal).
export function hintParts(text, canManage) {
  const at = text.indexOf(HARNESSES);
  if (at < 0) return { text };
  if (!canManage) {
    return { text: text.replace(/Add a harness in Settings → Harnesses/, 'Ask an admin to add a harness').replace(/Add one (?:in|under) Settings → Harnesses/, 'Ask an admin to add one').replace(/add one (?:in|under) Settings → Harnesses/, 'ask an admin to add one')
      .replace(/; [Cc]heck Settings → Harnesses/, '; ask an admin to check the harnesses').replace(/[Cc]heck Settings → Harnesses/, 'Ask an admin to check the harnesses').replace(/enable it in Settings → Harnesses/, 'ask an admin to enable it') };
  }
  return { before: text.slice(0, at), link: HARNESSES, after: text.slice(at + HARNESSES.length) };
}

// What the line under the tool says: why there is no tool or model to pick, or what the chosen launcher lacks.
export function spawnReason({ needsUpdate, tool, catalogCount, modelCount }) {
  return needsUpdate && !tool?.available ? 'This machine\'s supervisor predates harnesses. Update the supervisor to add harnesses.'
    : !tool ? (catalogCount ? `No tool is available on a connected machine. Check ${HARNESSES}.` : `No harness yet. Add a harness in ${HARNESSES}.`)
    : !tool.available ? (tool.unavailable_reason?.includes(HARNESSES) ? tool.unavailable_reason : `${tool.unavailable_reason || 'This tool is unavailable'}; check ${HARNESSES}`)
    : modelCount ? (tool.toolbox === false ? `No agent toolbox: ${tool.toolbox_reason || 'unavailable for this launcher'}.` : '') : `This harness has no models yet. Add one under ${HARNESSES}.`;
}

export const NAME_RULE = 'A name is 3 to 21 characters: lowercase letters, digits, - and _, starting with a letter and ending with a letter or digit.';
export const validName = name => !name || /^[a-z][a-z0-9_-]{1,19}[a-z0-9]$/.test(name);

// The rooms a v1 agent can be started in: the project's open task rooms. Its history may be only in the home room (which keeps its own
// lead-only rule) or in a room since archived, so the start is not tied to the room the profile was opened in. A continued Claude Code
// session gets a room of its own unless the person picks an existing one.
export function startRoomChoices(rooms, resume, currentRoomID) {
  if (!resume) return { options: [], value: '' };
  const open = rooms.filter(room => room.kind === 'task' && room.lifecycle === 'open');
  const options = open.map(room => ({ value: room.id, label: room.title }));
  const continuing = resume.kind === 'claude_session';
  if (continuing) options.unshift({ value: NEW_ROOM, label: 'A new room for this session' });
  // general is the project's shared task room; otherwise the open room if it is one, else the first.
  const preferred = open.filter(room => room.title === 'general').sort((a, b) => a.depth - b.depth)[0] || open.find(room => room.id === currentRoomID) || open[0];
  return { options, value: continuing ? NEW_ROOM : preferred?.id || options[0]?.value || '' };
}

// The banner of the start-from-v1 dialog: what is being started, where it will work, and what to confirm. `mode` is the continue/copy choice,
// `hasRoom` whether the start has a task room to go to, `warning` the note about another model, `relative` writes a time.
export function resumeBanner({ resume, mode, hasRoom, toolNote, warning, relative }) {
  const empty = { hidden: true, title: 'Spawn', text: '', card: null, notes: '', choosing: false, held: false, forking: false, confirmHidden: true, confirmText: ' v1 is not running this session any more' };
  if (!resume) return empty;
  const continuing = resume.kind === 'claude_session';
  // A v1 agent whose Claude Code session can be continued is offered the same choice: continue the original, or work on a copy.
  const choosing = continuing || !!resume.resumable;
  const held = continuing && !!resume.held_by;
  const forking = choosing && mode === 'fork';
  const title = continuing ? (forking ? 'Work on a copy of a Claude Code session' : 'Continue a Claude Code session') : (forking ? `Start ${resume.slug} on a copy of its v1 session` : `Start ${resume.slug} as a v2 agent`);
  // Closing the session matters only when it is continued; a copy leaves the original as it is.
  const confirmHidden = continuing ? !resume.active || forking : !resume.v1_active || forking;
  const confirmText = continuing ? ' I closed this session in my terminal' : ' v1 is not running this session any more';
  const lines = [];
  if (continuing) {
    const card = [['Session', resume.session_id.slice(0, 8), true], ['Folder', resume.cwd, true], ['Last active', resume.modified_at ? relative(resume.modified_at) : 'unknown', false], ...(resume.model ? [['Ran on', resume.model, true]] : [])];
    if (!hasRoom) lines.push('This project has no open task room to start it in.');
    if (resume.outside_folder) lines.push(`It works in ${resume.cwd}, outside the project folder, so the owner must approve it in the Inbox before it starts.`);
    if (held) lines.push(`${resume.held_by} already continues this session, so it can only be copied.`);
    if (resume.active && !forking) lines.push('A Claude process has this session open. Close it first: two processes on one session corrupt it.');
    lines.push(toolNote, warning);
    return { hidden: false, title, choosing, held, forking, confirmHidden, confirmText, card,
      text: forking ? 'This works on a copy of the session. The agent starts from everything said so far, and what it does goes to the copy: the original stays exactly as it is.' : 'This continues the session. It carries on from where it stopped, in the same transcript.',
      notes: lines.filter(Boolean).join(' ') };
  }
  const text = resume.resumable
    ? (forking ? `This works on a copy of the v1 session of ${resume.legacy_id}, in ${resume.cwd}. The agent starts from everything said so far, and what it does goes to the copy: the original stays exactly as it is.` : `This continues the v1 session of ${resume.legacy_id}, working in ${resume.cwd}.`)
    : `${resume.reason || 'This agent starts fresh with the same name.'}`;
  if (!hasRoom) lines.push('This project has no open task room to start it in.');
  if (resume.resumable && resume.outside_folder) lines.push(`It works in ${resume.cwd}, outside the project folder, so the owner must approve it in the Inbox before it starts.`);
  if (resume.v1_active && !forking) lines.push('v1 saw this agent a moment ago and may still be using the session. Stop it in v1 first: two processes on one session corrupt it.');
  if (resume.resumable) lines.push(toolNote, warning);
  return { hidden: false, title, choosing, held: false, forking, confirmHidden, confirmText, card: null, text, notes: lines.filter(Boolean).join(' ') };
}

// The request a Spawn sends, or the reason it must not be sent. Nothing is sent without a role, a tool that is available and a model.
export function spawnRequest({ role, tool, model, typed, machineID, resume, mode, confirmed, worktree, baseRef, target, newRoom }) {
  const continuing = resume?.kind === 'claude_session', fork = mode === 'fork';
  if (!role || !tool?.available || !model) return { skip: true };
  // A v1 agent keeps its own name, so the field does not count then.
  const name = resume && !continuing ? '' : typed.trim();
  if (!validName(name)) return { nameError: NAME_RULE };
  if (resume?.resumable && tool.tool !== resume.tool) return { error: `To continue this session the tool must be ${resume.tool}.` };
  if (continuing && !fork && resume.active && !confirmed) return { confirm: 'Confirm that you closed this session.' };
  const legacyFork = !continuing && !!resume?.resumable && fork;
  if (!continuing && !legacyFork && resume?.v1_active && !confirmed) return { confirm: 'Confirm that v1 is not running this session any more.' };
  const worktreeChoice = target?.kind !== 'task' || !worktree ? { worktree: false, base_ref: '' } : { worktree: true, base_ref: baseRef };
  return { params: { role, tool: tool.tool, variant: tool.variant, launcher_id: tool.launcher_id, model, provider: tool.provider, machine_id: machineID, ...(name ? { name } : {}), ...(resume?.resumable ? {} : worktreeChoice),
    ...(resume && !continuing ? { resume_legacy: resume.legacy_id, ...(resume.resumable ? { resume_mode: legacyFork ? 'fork' : 'continue' } : {}), confirm_v1_idle: !legacyFork && !!confirmed } : {}),
    ...(continuing ? { resume_session: resume.session_id, resume_mode: fork ? 'fork' : 'continue', confirm_session_closed: !fork && !!confirmed, new_room: !!newRoom } : {}) } };
}

// The Switch dialog's tool rows: the entry's index as its value, and for an unavailable one why.
export function switchRows(catalog) {
  return (catalog || []).map((tool, index) => ({ value: String(index), label: `${tool.label || tool.tool}${tool.available ? '' : ` — ${tool.unavailable_reason || 'unavailable'}`}`, disabled: !tool.available }));
}

// The tool the dialog starts on: the agent's own when the machine offers it, else the first that is available (-1 when none is).
export function switchStart(catalog, agent) {
  const current = catalog.findIndex(tool => tool.available && tool.tool === agent?.tool && tool.variant === agent?.variant);
  return current >= 0 ? current : catalog.findIndex(tool => tool.available);
}

// The model a tool opens on: the harness's default, or the agent's own when it stays on that tool and the tool lists it.
export function switchModel(tool, agent) {
  const models = (tool?.models || []).map(model => model.id);
  if (tool?.tool === agent?.tool && tool?.variant === agent?.variant && models.includes(agent.model)) return agent.model;
  return (tool?.models || []).find(model => model.default)?.id || models[0] || '';
}

// Whether the pick is the agent as it already is, which is not a switch.
export const switchUnchanged = ({ tool, model, agent, machineID }) => tool?.tool === agent?.tool && tool?.variant === agent?.variant && model === agent?.model && (!agent?.machine_id || machineID === agent.machine_id);

export function switchReason({ needsUpdate, tool, modelCount, unchanged }) {
  return needsUpdate && !tool?.available ? 'This machine\'s supervisor predates harnesses. Update the supervisor to add harnesses.'
    : !tool ? `No harness is available on a connected machine. Add one in ${HARNESSES}.`
    : !modelCount ? `This harness has no models yet. Add one under ${HARNESSES}.`
      : unchanged ? 'Choose a different model or tool.'
        : tool.toolbox === false ? `No agent toolbox: ${tool.toolbox_reason || 'unavailable for this launcher'}.` : '';
}
