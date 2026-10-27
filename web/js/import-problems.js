// What stops the v1 import wizard from going on, found page by page. A page reports every problem it has at once, so the owner fixes them
// in one go instead of meeting them one Next at a time. Each problem is { message, selector }: what is wrong, and how to find the field.
// Pure: the wizard shows them (links, inline messages) and tests run this module without a page.

// A selector-safe form of a value (a project alias, a room id): CSS.escape where the page has it.
export const escapeSelector = value => (typeof CSS !== 'undefined' && CSS.escape) ? CSS.escape(String(value)) : String(value).replace(/[^a-zA-Z0-9_-]/g, char => `\\${char}`);

// A project this import creates needs a name and an absolute workspace folder; an existing one is left as it is.
export function projectProblems(projects, escape = escapeSelector) {
  const problems = [];
  for (const [alias, project] of Object.entries(projects || {})) {
    if (project.existing_id) continue;
    if (!(project.name || '').trim()) problems.push({ message: `Project "${alias}" needs a name.`, selector: `input[data-project-name="${escape(alias)}"]` });
    if (!(project.folder || '').startsWith('/')) problems.push({ message: `Project "${(project.name || '').trim() || alias}" needs its workspace folder: an absolute path such as /Users/you/code/${alias}.`, selector: `input[data-project-folder="${escape(alias)}"]` });
  }
  return problems;
}

// A new account needs a valid login that is not an account here already (compared case-insensitively, as the server does) and that no
// other new account takes: the second one to ask for a login is the one flagged.
export function peopleProblems(newUsers, destinationUsers, escape = escapeSelector) {
  const taken = new Set((destinationUsers || []).map(login => login.toLowerCase()));
  const seen = new Set();
  const problems = [];
  for (const [human, user] of Object.entries(newUsers || {})) {
    const selector = `input[data-new-login="${escape(human)}"]`, login = user.login || '';
    if (!/^[a-z][a-z0-9_-]{2,31}$/.test(login)) { problems.push({ message: `The login for ${human} must be 3 to 32 characters: a lowercase letter, then lowercase letters, digits, - or _.`, selector }); continue; }
    if (taken.has(login.toLowerCase())) { problems.push({ message: `The login "${login}" is already an account here; map ${human} to it instead, or choose another login.`, selector }); continue; }
    if (seen.has(login.toLowerCase())) { problems.push({ message: `Two people would both be "${login}"; give each their own login.`, selector }); continue; }
    seen.add(login.toLowerCase());
  }
  return problems;
}

// A direct message that is imported needs a project to live in.
export function dmProblems(rooms, specs, titleOf, escape = escapeSelector) {
  const problems = [];
  for (const room of (rooms || []).filter(item => item.dm)) {
    const spec = (specs || {})[room.id];
    if (spec && !spec.skip && !spec.project) problems.push({ message: `Choose a project for the chat "${titleOf(room)}", or turn it off.`, selector: `[data-dm-room="${escape(room.id)}"]` });
  }
  return problems;
}

// The line above the list of problems.
export const problemSummary = count => count === 1 ? 'One thing needs fixing before you go on:' : `${count} things need fixing before you go on:`;
