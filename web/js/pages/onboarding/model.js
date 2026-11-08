export const PRINCIPLES = [
  ['A project is one repo or folder.', 'Everything you do with agents happens inside a project.'],
  ['Every project has a home room ⌂ and a general room.', 'The leader is spawned in the home room and coordinates from there; the work itself happens in task rooms such as general.'],
  ['Extra sub-rooms are rare.', 'Your leader proposes one and you agree to it.'],
  ['Agents are spawned into a room.', 'Use Spawn; they keep working there until you stop them.'],
  ['Agents take roles.', 'The leader plans and coordinates, workers build, reviewers check.'],
  ['Anything that needs you lands in the Inbox.', 'You decide there: approve, answer or redirect.'],
  ['@mentions and replies wake agents.', 'A reaction acknowledges without waking anyone.'],
];

export const HARNESS_MISSING = 'No harness with a model yet. Tick a detected harness, or add one by hand and give it a model.';
export const NO_MODEL_NOTE = 'The harness was added but has no model yet. Add a model by hand, or retry Auto-detect in its models, then Continue.';
export const PROJECT_REQUIRED = 'Enter a project name and a workspace folder.';

// The project form's check: both fields are needed, and the focus goes to the first one that is empty.
export function checkProject(name, folder) {
  const trimmed = { name: String(name).trim(), folder: String(folder).trim() };
  if (trimmed.name && trimmed.folder) return { ...trimmed, error: '', focus: '' };
  return { ...trimmed, error: PROJECT_REQUIRED, focus: trimmed.name ? 'folder' : 'name' };
}

// What the Harnesses step says: a note about the last add wins until something is ready or ticked; ticked ones say what Continue will do.
export function harnessStatusText({ ready, ticked, note }) {
  if (note && !ready && !ticked) return note;
  if (ticked) return `${ticked === 1 ? 'One harness is' : `${ticked} harnesses are`} ticked: Continue adds ${ticked === 1 ? 'it' : 'them'} and looks up the models.`;
  return ready ? 'A harness with a model is ready.' : HARNESS_MISSING;
}

// The last step offers Spawn only when an agent could run; otherwise it says so and offers a way to set one up (or to ask the owner).
export function howView(ready, manage) {
  const note = manage ? 'No harness with a model is set up yet, so there is nothing for an agent to run on. Set one up, then spawn your first agent.'
    : 'No harness is set up yet, so there is nothing for an agent to run on. Ask the owner to add one, then spawn your first agent.';
  return { spawnHidden: !ready, setupHidden: ready || !manage, noteHidden: ready, noteText: ready ? '' : note, focus: ready ? 'spawn' : manage ? 'setup' : 'done' };
}
