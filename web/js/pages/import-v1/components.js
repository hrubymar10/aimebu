// The v1 import wizard's view: the dialog's children (heading, progress, status, error, content, totals, buttons, skip zone, and the skip
// confirmation), drawn from the store. The markup, classes, ids, roles and texts are the hand-built wizard's, element for element, with no
// wrappers, so the same CSS and the same test-ui checks apply. Pages edit store.mapping in place and call store.changed(); a text field
// writes as it is typed and does not redraw, as before. `actions` is the adapter: Next and the Enter key go through it.
import { html, Fragment, useLayoutEffect, useRef, useState } from '../../ui.js';
import { rights } from '../../api.js';
import { attachFolderField } from '../../folder-field.js';
import { escapeSelector, problemSummary } from '../../import-problems.js';
import { applyChoice, choiceValue, mergeGroups, parseChoice, planTree, roomChoice } from '../../import-tree.js';
import {
  NEW_ACCOUNT, NEW_PROJECT, ONLY_CHANCE, SYSTEM_ROOM, accountingRows, dmTitle, footerTotals, macroBody, macroCards, newLogin, number, ownerHint,
  reportCountRows, skippedPersonWarning, stepStatus, summaryLists,
} from './model.js';

const RULES = { roomChoice, parseChoice, planTree };

// A button whose action may be async; a failure is shown in the dialog's error line. The action starts in the click itself.
function Button({ store, label, onClick, secondary = false, className = '' }) {
  const run = () => { try { Promise.resolve(onClick()).catch(error => store.fail(error)); } catch (error) { store.fail(error); } };
  return html`<button type="button" class=${`${secondary ? 'btn' : 'btn btn-primary'}${className ? ` ${className}` : ''}`} onClick=${run}>${label}</button>`;
}

// The problem Next found for the field `selector` finds, if any: its message sits after the field's label, tied to the field.
function problemAt(store, selector) {
  const problems = store.error?.problems;
  if (!problems || !selector) return null;
  const index = problems.findIndex(problem => problem.selector === selector);
  return index < 0 ? null : { id: `import-problem-${index}`, message: problems[index].message };
}
const FieldError = ({ problem }) => problem && html`<small class="import-field-error" id=${problem.id}>${problem.message}</small>`;

// A labelled text box whose value is kept as it is typed (not only when it loses focus); Enter goes on to the next page unless `onEnter`
// says otherwise. `folder` makes it the Workspace folder field (an island: folder-field.js owns the button and list it adds).
function TextField({ store, actions, label, value, onValue, onEnter, data = {}, selector, placeholder, inputRef, folder = false, onFocus }) {
  const own = useRef(null), ref = inputRef || own;
  useLayoutEffect(() => { if (folder) attachFolderField(ref.current, () => rights.hostHome); }, []);
  const problem = problemAt(store, selector);
  const take = event => { onValue(event.currentTarget.value); store.save(); };
  const key = event => {
    if (event.key !== 'Enter' || event.isComposing) return;
    event.preventDefault(); take(event); (onEnter || (() => actions.next()))();
  };
  return html`<label>${label}<input ref=${ref} type="text" value=${value ?? ''} placeholder=${placeholder} ...${data} aria-invalid=${problem ? 'true' : undefined}
    aria-describedby=${problem?.id} onInput=${take} onChange=${take} onKeyDown=${key} onFocus=${onFocus} /></label><${FieldError} problem=${problem} />`;
}

function SelectField({ store, label, value, options, onChange, data = {}, selector }) {
  const problem = problemAt(store, selector);
  return html`<label>${label}<select ...${data} value=${value ?? ''} aria-invalid=${problem ? 'true' : undefined} aria-describedby=${problem?.id}
    onChange=${event => onChange(event.currentTarget.value)}>${options.map(([key, title]) => html`<option key=${key} value=${key}>${title}</option>`)}</select></label><${FieldError} problem=${problem} />`;
}

// A checkbox before its label. `redraw` says whether the choice changes what the page shows, or only the totals.
function Checkbox({ store, label, checked, onValue, data = {}, redraw = false }) {
  return html`<label><input type="checkbox" ...${data} checked=${!!checked} onChange=${event => { onValue(event.currentTarget.checked); store.changed(redraw); }} />${label}</label>`;
}

// A select of the projects with "+ New project…" at the end. `none` adds a first choice that means no project.
function ProjectSelect({ store, label, value, update, none, data, selector }) {
  const projects = Object.entries(store.mapping.projects).map(([alias, project]) => [alias, project.name || alias]);
  const options = [...(none ? [['', none]] : []), ...projects, [NEW_PROJECT, '+ New project…']];
  const change = choice => {
    if (choice === NEW_PROJECT) { store.askNewProject(alias => update(alias)); return; }
    update(choice); store.changed();
  };
  return html`<${SelectField} store=${store} label=${label} value=${value} options=${options} onChange=${change} data=${data} selector=${selector} />`;
}

// A collapsible block: long lists (the not-carried records, the skipped items) stay folded until the owner opens them.
function Section({ title, note, open = false, className = '', data = {}, children }) {
  return html`<details class=${`import-section${className ? ` ${className}` : ''}`} open=${open} ...${data}><summary><h3>${title}</h3>${!!note && html`<span class="import-section-note">${note}</span>`}</summary>${children}</details>`;
}

// ---- pages ----

// "New project": a name and a folder. The form is drawn from the store's draft; Create reads the fields themselves, so a value put there by
// any means counts. Enter in either field makes the project; it must not skip to the next page.
function NewProject({ store, actions, scroll = false }) {
  const box = useRef(null), name = useRef(null), folder = useRef(null), draft = store.newProject;
  useLayoutEffect(() => { name.current.focus(); if (scroll) box.current.scrollIntoView({ block: 'nearest' }); }, []);
  const create = () => { draft.name = name.current.value; draft.folder = folder.current.value; const field = store.createProject(draft.name, draft.folder); if (field) (field === 'name' ? name : folder).current.focus(); };
  return html`<fieldset class="import-new-project" ref=${box}><legend>New project</legend>
    <${TextField} store=${store} actions=${actions} label="Name" value=${draft.name} onValue=${value => { draft.name = value; }} onEnter=${create} inputRef=${name} data=${{ 'data-new-project-name': '' }} />
    <${TextField} store=${store} actions=${actions} label="Workspace folder (absolute path)" value=${draft.folder} onValue=${value => { draft.folder = value; }} onEnter=${create} inputRef=${folder}
      data=${{ 'data-new-project-folder': '' }} placeholder="/path/to/the/project" folder=${true} />
    <p class="error" role="alert">${draft.message || null}</p>
    <div class="dialog-actions"><${Button} store=${store} label="Create project" onClick=${create} /><${Button} store=${store} label="Cancel" secondary=${true} onClick=${() => store.cancelNewProject()} /></div></fieldset>`;
}

function ProjectBox({ store, actions, alias, project }) {
  const box = useRef(null), name = useRef(null), m = store.mapping;
  // A project just made here is scrolled to, with its name focused.
  useLayoutEffect(() => { if (store.justAdded !== alias) return; store.justAdded = null; box.current.scrollIntoView({ block: 'nearest' }); name.current.focus({ preventScroll: true }); });
  const viewers = (store.preview.destination_users || []).filter(login => login !== m.owner_login);
  return html`<fieldset ref=${box}><legend>${alias}</legend>
    <${TextField} store=${store} actions=${actions} label="Name" value=${project.name} onValue=${value => { project.name = value; }} inputRef=${name}
      data=${{ 'data-project-name': alias }} selector=${`input[data-project-name="${escapeSelector(alias)}"]`} />
    <${TextField} store=${store} actions=${actions} label="Workspace folder (absolute path)" value=${project.folder} onValue=${value => { project.folder = value; }} folder=${true}
      data=${{ 'data-project-folder': alias }} placeholder="/path/to/the/project" selector=${`input[data-project-folder="${escapeSelector(alias)}"]`} />
    ${!project.folder && html`<p class="settings-intro import-folder-hint">${"v1 did not record a folder for this project. Enter the folder its agents will work in; you can change it in the project's settings until its first agent starts. Or remove the project: its rooms are then not imported."}</p>`}
    ${viewers.map(login => html`<${Checkbox} key=${login} store=${store} label=${`Grant ${login} project viewer access`} checked=${(m.project_members[alias] || []).includes(login)} onValue=${checked => {
      const members = new Set(m.project_members[alias] || []); if (checked) members.add(login); else members.delete(login);
      m.project_members[alias] = [...members];
    }} />`)}
    <${Button} store=${store} label="Remove project" secondary=${true} onClick=${() => store.removeProject(alias)} /></fieldset>`;
}

function ProjectsPage({ store, actions }) {
  const projects = Object.entries(store.mapping.projects);
  return html`<p class="import-warning import-only-chance">${ONLY_CHANCE}</p>
    ${store.restored && html`<p class="import-restored">Continuing where you left off.</p>`}
    ${projects.map(([alias, project]) => html`<${ProjectBox} key=${alias} store=${store} actions=${actions} alias=${alias} project=${project} />`)}
    ${!projects.length && html`<p class="settings-intro">No project yet: everything that needs one will be left out. Add a project to bring rooms across.</p>`}
    ${store.newProject && html`<${NewProject} store=${store} actions=${actions} scroll=${true} />`}
    <${Button} store=${store} label="Add destination project" secondary=${true} onClick=${() => store.askNewProject(() => {})} />`;
}

function PeoplePage({ store, actions }) {
  const p = store.preview, m = store.mapping, newUsers = m.new_users || {};
  const people = [['-', 'Skip person (legacy label only)'], ...(p.destination_users || [m.owner_login]).map(login => [login, login === m.owner_login ? `${login} (owner)` : login]), [NEW_ACCOUNT, 'Create a new account…']];
  const hint = ownerHint(p, m);
  const choose = (human, value) => {
    if (value === NEW_ACCOUNT) {
      const login = newLogin(human, p, m);
      m.new_users[human] = { login, role: 'viewer' }; m.humans[human] = login;
    } else { delete m.new_users[human]; m.humans[human] = value; }
    store.changed();
  };
  return html`${!!hint && html`<p class="import-warning people-owner-hint">${hint}</p>`}
    ${(p.humans || []).map(human => {
      const created = newUsers[human];
      return html`<${Fragment} key=${human}>
        <${SelectField} store=${store} label=${human} value=${created ? NEW_ACCOUNT : m.humans[human]} options=${people} data=${{ 'data-person': human }} onChange=${value => choose(human, value)} />
        ${!created && m.humans[human] === '-' && html`<p class="import-warning">${skippedPersonWarning(p.human_stakes?.[human])}</p>`}
        ${created && html`<div class="import-new-account">
          <${TextField} store=${store} actions=${actions} label="Login for the new account" value=${created.login} onValue=${value => { created.login = value.trim(); m.humans[human] = created.login; }}
            data=${{ 'data-new-login': human }} selector=${`input[data-new-login="${escapeSelector(human)}"]`} onFocus=${event => event.currentTarget.select()} />
          <${SelectField} store=${store} label="Role" value=${created.role} data=${{ 'data-new-role': human }} options=${[['viewer', 'Viewer: reads the project\'s rooms'], ['operator', 'Operator: also works with agents']]}
            onChange=${value => { created.role = value; store.save(); }} />
          <p class="settings-intro">${"The role applies to every project this person's rooms are imported into. The account has no password until its owner follows the one-time link shown after the import."}</p></div>`}
      <//>`;
    })}`;
}

// One v1 room: "Import into" (a project's root, its general room, a new sub-room named like the room, or nowhere) and whether it is archived.
function RoomRow({ store, room }) {
  const m = store.mapping, spec = m.rooms[room.id] || { skip: true };
  const keep = () => (m.rooms[room.id] ||= spec);
  const current = roomChoice(spec, room.id), custom = parseChoice(current);
  const projects = Object.entries(m.projects).map(([alias, project]) => [alias, project.name || alias]);
  const change = value => {
    if (value === NEW_PROJECT) { store.askNewProject(alias => { applyChoice(keep(), choiceValue('general', alias), room.id); }); return; }
    applyChoice(keep(), value, room.id); store.changed();
  };
  const placed = !spec.skip && !!spec.project;
  const memoryNote = room.memory_disabled && placed
    && `v1 had memory off for this room, so v2 will have memory off for ${spec.target === 'root' ? `⌂ ${m.projects[spec.project]?.name || spec.project}` : `its room "${spec.target === 'sub' ? (spec.title || 'general') : room.id}"`}${spec.target ? ', and for any other v1 room merged into it' : ''}.`;
  return html`<div class="import-room-row" data-room=${room.id}><strong>${room.id}</strong><span class="import-room-count">${`${room.messages} messages`}</span>
    <select data-import-into=${room.id} aria-label=${`Import ${room.id} into`} value=${current} onChange=${event => change(event.currentTarget.value)}>
      <option value="skip">Don't import</option>
      ${projects.map(([alias, name]) => html`<optgroup key=${alias} label=${name}><option value=${choiceValue('root', alias)}>${`⌂ ${name}`}</option><option value=${choiceValue('general', alias)}>${`${name} › general`}</option><option value=${choiceValue('new', alias)}>${`${name} › new sub-room "${room.id}"`}</option></optgroup>`)}
      ${custom.kind === 'custom' && html`<option value=${current}>${`${m.projects[custom.alias]?.name || custom.alias} › "${spec.title}"`}</option>`}
      <option value=${NEW_PROJECT}>+ New project…</option></select>
    ${placed && spec.target !== 'root' && html`<${Checkbox} store=${store} label="Archived after import" checked=${spec.lifecycle !== 'open'} data=${{ 'data-archived': room.id }} redraw=${true} onValue=${checked => { keep().lifecycle = checked ? 'closed' : 'open'; }} />`}
    ${placed && spec.target === 'root' && html`<p class="import-count-note">${"A project's home room is never archived."}</p>`}
    ${!!memoryNote && html`<p class="settings-intro">${memoryNote}</p>`}</div>`;
}

// The v2 rooms this mapping makes, as a tree with message counts; what is left out is listed under it.
function RoomTree({ tree }) {
  const off = list => list.length ? ` · memory off (v1 room ${list.join(', ')} had it off)` : '';
  const empty = !tree.projects.length && !tree.direct.length;
  return html`<h4>In v2</h4><ul class="import-tree">
    ${tree.projects.map(project => html`<li key=${project.alias}><strong>${`⌂ ${project.name}`}</strong><span class="import-room-count">${project.root.rooms.length ? ` · ${project.root.messages} messages${off(project.root.memoryOff)}` : ''}</span>
      ${project.subs.length > 0 && html`<ul>${project.subs.map(sub => html`<li key=${sub.title}>${`${sub.title} · ${sub.messages} messages${sub.archived ? ' · archived' : ''}${off(sub.memoryOff)}`}</li>`)}</ul>`}</li>`)}
    ${tree.direct.length > 0 && html`<li>${`Direct messages · ${tree.direct.length} chats`}</li>`}
    ${empty && html`<li>Nothing will be imported yet.</li>`}</ul>
    ${tree.dropped.length > 0 && html`<div class="import-dropped"><h4>${`Not imported (${tree.dropped.length})`}</h4><p class="import-room-count">${tree.dropped.map(room => room.id).join(', ')}</p></div>`}`;
}

function RoomsPage({ store }) {
  const p = store.preview, m = store.mapping;
  const shown = (p.rooms || []).filter(room => room.id !== SYSTEM_ROOM), rooms = shown.filter(room => !room.dm);
  const tree = planTree(m, shown);
  const bulk = lifecycle => () => { for (const room of rooms) { const spec = m.rooms[room.id]; if (spec && !spec.skip) spec.lifecycle = lifecycle; } store.changed(); };
  return html`<div class="import-rooms"><div class="import-room-list">
      ${mergeGroups(tree).map(group => html`<p class="import-warning" role="note">${`${group.rooms.length} v1 rooms will be merged into ${group.where}: ${group.rooms.join(', ')}.`}</p>`)}
      <div class="import-bulk"><${Button} store=${store} label="Open all" secondary=${true} onClick=${bulk('open')} /><${Button} store=${store} label="Archive all" secondary=${true} onClick=${bulk('closed')} /></div>
      ${rooms.map(room => html`<${RoomRow} key=${room.id} store=${store} room=${room} />`)}</div>
    <aside class="import-room-tree" aria-label="Rooms in v2"><${RoomTree} tree=${tree} /></aside></div>
    ${(p.rooms || []).some(room => room.id === SYSTEM_ROOM) && html`<p class="settings-intro">v1 system events are not imported.</p>`}`;
}

function DmsPage({ store }) {
  const p = store.preview, m = store.mapping, direct = (p.rooms || []).filter(room => room.dm);
  return direct.map(room => {
    const spec = m.rooms[room.id] || { skip: true }, keep = () => (m.rooms[room.id] ||= spec), title = dmTitle(room, m);
    // Titles can collide (two chats with the same agent): then the v1 id tells them apart; otherwise it stays in the tooltip.
    const same = direct.filter(other => dmTitle(other, m) === title).length > 1;
    return html`<fieldset key=${room.id}><legend data-dm-room=${room.id}>${title}</legend>
      <p class="import-source-id" title=${`v1 chat ${room.id}`}>${`${room.messages} ${room.messages === 1 ? 'message' : 'messages'}${same ? ` · ${room.id}` : ''}`}</p>
      <${Checkbox} store=${store} label="Import this chat" checked=${!spec.skip} redraw=${true} onValue=${checked => { keep().skip = !checked; }} />
      ${!spec.skip && html`<${ProjectSelect} store=${store} label="Project" value=${spec.project || ''} update=${alias => { keep().project = alias; }} none="Choose a project"
        data=${{ 'data-dm-project': room.id }} selector=${`select[data-dm-project="${escapeSelector(room.id)}"]`} />`}</fieldset>`;
  });
}

function MemoryPage({ store }) {
  const p = store.preview, m = store.mapping, counts = p.memory_counts || {};
  const records = n => `${n || 0} ${n === 1 ? 'record' : 'records'}`;
  return html`${(p.memory_projects || []).map(scope => html`<${ProjectSelect} key=${scope} store=${store} label=${`Project facts: ${scope} (${records(counts.projects?.[scope])})`}
      value=${(m.memory_projects[scope] || '-') === '-' ? '' : m.memory_projects[scope]} update=${value => { m.memory_projects[scope] = value || '-'; }} none="Don't import" />`)}
    ${[['skip_profiles', `Import global user profiles (${records(counts.profiles)})`], ['skip_shared_notes', `Import shared agent notes (${records(counts.shared)})`]].map(([key, title]) => html`<${Checkbox} key=${key} store=${store}
      label=${title} checked=${!m[key]} data=${{ 'data-import-choice': key, 'data-inverted': '1' }} onValue=${checked => { m[key] = !checked; }} />`)}
    ${p.v1_memory_enabled
      ? html`<h3>Project memory after import</h3><p class="settings-intro">v1 global memory was on, so each project below starts with v2 memory on. Untick any project that should stay off.</p>
        ${Object.entries(m.projects).map(([alias, project]) => html`<${Checkbox} key=${alias} store=${store} label=${`Enable project memory after import: ${project.name || alias}`} checked=${m.enable_project_memory[alias]} onValue=${checked => { m.enable_project_memory[alias] = checked; }} />`)}`
      : html`<p class="settings-intro">v1 memory was off or unset; destination project memory stays off.</p>`}`;
}

// A macro's text: short ones in full, long ones folded behind their first line so a page of macros stays a page.
function MacroBody({ text }) {
  const body = macroBody(text);
  if (body.kind === 'empty') return html`<pre class="import-macro-body import-macro-empty">${body.text}</pre>`;
  if (body.kind === 'short') return html`<pre class="import-macro-body">${body.text}</pre>`;
  return html`<details class="import-macro-long"><summary>${body.summary}</summary><pre class="import-macro-body">${body.text}</pre></details>`;
}

function MacrosPage({ store, macros }) {
  const m = store.mapping;
  const include = (key, checked) => { const set = new Set(m.skip_macros); if (checked) set.delete(key); else set.add(key); m.skip_macros = [...set]; };
  return html`${!!macros.leftOut && html`<p class="settings-intro import-macros-left-out">${macros.leftOut}</p>`}
    <div class="import-bulk"><${Button} store=${store} label="Import all" secondary=${true} onClick=${() => { m.skip_macros = []; store.changed(); }} /><${Button} store=${store} label="Import none" secondary=${true} onClick=${() => { m.skip_macros = [...macros.keys]; store.changed(); }} /></div>
    ${macros.cards.map(card => html`<fieldset key=${card.key} data-macro=${card.key}><legend>${card.legend}<span class=${`import-badge import-badge-${card.state}`} data-state=${card.state}>${card.label}</span></legend>
      ${card.imported ? html`<p>Already imported from this backup; later edits are preserved.</p>` : html`<${Checkbox} store=${store} label="Import this macro" checked=${card.included} redraw=${true} onValue=${checked => include(card.key, checked)} />`}
      <p>v1 text</p><${MacroBody} text=${card.body} />
      ${card.exists && html`<p>${card.same ? 'Already matches v2.' : 'Existing v2 text:'}</p>`}
      ${card.exists && !card.same && html`<${MacroBody} text=${card.current} />`}
      ${card.exists && !card.same && !card.imported && html`<${SelectField} store=${store} label="On conflict" value=${card.conflict} options=${[['keep_v2', 'Keep v2 text'], ['use_v1', 'Use v1 text']]} onChange=${value => { m.macro_conflicts[card.key] = value; store.save(); }} />`}</fieldset>`)}`;
}

function RolesPage({ store }) {
  const p = store.preview, m = store.mapping;
  return (p.roles || []).map(role => {
    const imported = (p.imported_roles || []).includes(role.key);
    return html`<fieldset key=${role.key}><legend>${`${role.emoji || ''} ${role.key}`.trim()}</legend>
      ${imported && html`<p>Already imported from this backup; later edits are preserved.</p>`}
      ${!!role.extends && html`<p>${`Extends ${role.extends}: it keeps this link, and the body below is only its own text.`}</p>`}
      ${!!role.flattened_from && html`<p>${`Its parent ${role.flattened_from} is not available in v2, so it cannot keep the link.`}</p>`}
      ${role.skipped_reason ? html`<p>${`Skipped: ${role.skipped_reason}`}</p>` : html`<p>${`${role.label} · ${role.cardinality} · v1 body:`}</p><pre class="import-macro-body">${role.body}</pre>
        ${!!role.conflict && html`<p>${role.current_extends ? `Existing v2 body (extends ${role.current_extends}):` : 'Existing v2 body:'}</p><pre class="import-macro-body">${role.current_body || ''}</pre>
          ${!imported && html`<${SelectField} store=${store} label="On conflict" value=${m.role_conflicts[role.key] || 'keep_v2'} options=${[['keep_v2', 'Keep v2 role'], ['use_v1', 'Use v1 role']]} onChange=${value => { m.role_conflicts[role.key] = value; store.save(); }} />`}`}`}</fieldset>`;
  });
}

// ---- the report: the dry run's, and the finished import's ----

// The reconciled totals first: one headline line, then a block per count whose parts add up to the backup's own number, each skipped part
// with its reason and, when there is something to look at, the rooms behind it (never message text).
function Accounting({ report }) {
  const accounting = accountingRows(report);
  if (!accounting) return null;
  return html`<p class="import-headline">${accounting.headline}</p>
    ${!!accounting.leftNote && html`<p class="import-warning">${accounting.leftNote}</p>`}
    <div class="import-accounting">${accounting.groups.map(group => html`<${Section} key=${group.key} title=${group.title} note=${group.note} open=${group.open} className="import-accounting-group" data=${{ 'data-key': group.key }}>
      <table>${group.rows.map(row => html`<tr key=${row.key} class=${row.skipped ? 'import-skipped-row' : ''} data-part=${row.key}><td>${row.label}${row.items.length > 0 && html`<details class="import-items"><summary>${row.itemsLabel}</summary>${row.items.map(line => html`<p>${line}</p>`)}</details>`}</td><td>${row.count}</td></tr>`)}
        <tr class="import-accounting-total"><td>Total in the backup</td><td>${group.total}</td></tr></table><//>`)}</div>`;
}

// The counts, with the columns and rows that are all zero left out: "Skipped" shows only when something is in it, and a non-zero one is marked.
function Counts({ report }) {
  const counts = reportCountRows(report);
  return html`<${Accounting} report=${report} /><table><tr>${['Record', 'New', ...(counts.showSkipped ? ['Skipped'] : [])].map(label => html`<th>${label}</th>`)}</tr>
    ${counts.rows.map(row => html`<tr key=${row.key}><td data-key=${row.key}>${row.title}${!!row.explanation && html`<div class="import-count-note">${row.explanation}</div>`}</td><td>${row.imported}</td>${counts.showSkipped && html`<td class=${row.skippedNonZero ? 'import-skipped' : ''}>${row.skipped}</td>`}</tr>`)}</table>`;
}

function SummaryPage({ store }) {
  const report = store.report;
  if (!report) return null;
  const access = Object.entries(report.project_readers || {});
  return html`<${Counts} report=${report} />
    <div class="import-left-out">${summaryLists(report, store.preview, store.mapping, RULES).map(list => html`<${Section} key=${list.title} title=${list.title} note=${`${list.items.length}`} open=${true}>
      ${!!list.note && html`<p class="settings-intro">${list.note}</p>`}${list.items.map(item => html`<p>${item}</p>`)}<//>`)}</div>
    ${access.length > 0 && html`<h3>Project access</h3>${access.map(([project, readers]) => html`<p>${`${project}: ${readers} readers; ${report.project_access_gaps?.[project] || 0} access gaps; ${report.unreadable_dm_participants?.[project] || 0} DM access gaps.`}</p>`)}`}
    <p class="settings-intro">Import pauses writes and agent dispatch until the database transaction commits. Keep this page open to watch progress.</p>`;
}

function InviteRow({ invite }) {
  const input = useRef(null), [copied, setCopied] = useState('Copy');
  const copy = async () => { try { await navigator.clipboard.writeText(invite.url); setCopied('Copied'); } catch { input.current.select(); setCopied('Press Ctrl+C'); } };
  return html`<div class="import-invite"><strong>${invite.login}</strong><input ref=${input} type="text" readOnly value=${invite.url} aria-label=${`Invite link for ${invite.login}`} data-invite-for=${invite.login} /><button type="button" class="btn" onClick=${copy}>${copied}</button></div>`;
}

// The finished import: the one-time links for the accounts it made come first (each works once, expires in an hour, and is shown only
// here, so a scroll must not hide them), then what came across.
function Finished({ job }) {
  const invites = job.invites || [], report = job.report, files = report?.carried_over_files || [];
  return html`${!!job.invite_error && html`<p class="error">${`${job.invite_error}. Make the links in Settings → Users.`}</p>`}
    ${invites.length > 0 && html`<div class="import-invites"><h3>Invite links for the new accounts</h3><p class="settings-intro">Give each person their link. It works once, sets their password, and expires in an hour; it is shown only here. Lost a link? Make a new one in Settings → Users → Reset link.</p>
      ${invites.map(invite => html`<${InviteRow} key=${invite.login} invite=${invite} />`)}</div>`}
    ${!invites.length && !job.invite_error && html`<p class="settings-intro import-no-accounts">No new accounts were created, so there are no invite links.</p>`}
    ${report ? html`<h3>Imported from v1</h3><p class="import-finished-summary">${`${number(report.imported?.rooms)} rooms · ${number(report.imported?.messages)} messages · Historical agents: ${number(report.historical_agents)}`}</p>
      <h3>Destination projects</h3>${!(report.destinations || []).length && html`<p>No projects were selected.</p>`}
      ${(report.destinations || []).map(destination => html`<p class="import-destination">${`${destination.name} · ${destination.folder}`}</p>`)}
      <${Counts} report=${report} />
      ${(report.skipped_memory || []).length > 0 && html`<h3>Skipped memory</h3>${report.skipped_memory.map(entry => html`<p>${`${entry.scope} · ${entry.scope_key} · ${entry.id}: ${entry.reason}`}</p>`)}`}
      ${files.length > 0 && html`<h3>Files kept for v2</h3>${files.map(path => html`<p>${path}</p>`)}`}`
    : html`<p>Import completed. Refresh to see the imported rooms.</p>`}`;
}

// A second dialog names what skipping costs; only its own button posts the skip, and focus starts on the safe choice.
function SkipConfirm({ store }) {
  const dialog = useRef(null), keep = useRef(null);
  useLayoutEffect(() => { const node = dialog.current; node.showModal(); keep.current.focus(); return () => { if (node.open) node.close(); }; }, []);
  return html`<dialog class="import-skip-confirm" ref=${dialog} onClose=${() => store.closeSkipConfirm()}><h3>Skip the v1 import forever?</h3>
    <p class="import-warning">${ONLY_CHANCE}</p>
    <p>Your v1 rooms, messages, memory, macros, roles and settings will not be brought into v2, and this wizard will not be offered again. Nothing in the app can import the backup later.</p>
    <div class="dialog-actions"><button ref=${keep} type="button" class="btn" onClick=${() => store.closeSkipConfirm()}>Keep reviewing</button><button type="button" class="btn btn-danger" onClick=${() => store.skipForever()}>Skip import forever</button></div></dialog>`;
}

// Skipping is irreversible, so it lives apart from the forward buttons, behind a confirmation, on the first page only.
const SkipOffer = ({ store }) => html`<span class="import-skip-note">Not going to import?</span><${Button} store=${store} label="Skip the import…" secondary=${true} className="import-skip-open" onClick=${() => store.confirmSkip()} />`;

const PAGES = { projects: ProjectsPage, people: PeoplePage, rooms: RoomsPage, dms: DmsPage, memory: MemoryPage, macros: MacrosPage, roles: RolesPage, summary: SummaryPage };
const IMPORT_STATUS = { running: 'Import running', failed: 'Import status unavailable', done: 'v1 import complete', paused: 'Import paused' };

// What each phase shows: { status, content, buttons, skip, progress, totals }.
function view(store, actions) {
  const s = store, p = s.preview;
  if (s.phase === 'changed') {
    // The backup can no longer be verified, so it cannot be imported as it is. Skipping closes the import for good;
    // closing the dialog leaves it open, so the owner can restore a pristine copy and the wizard offers it again.
    return { status: s.changedMessage || 'The v1 backup was modified after it was made, so it can no longer be verified and will not be imported.', skip: true,
      buttons: html`<${Button} store=${s} label="Decide later" secondary=${true} onClick=${() => actions.dialog.close()} />` };
  }
  if (s.phase === 'import') {
    const job = s.job;
    const buttons = job.state === 'failed' ? html`<${Button} store=${s} label="Retry" onClick=${() => s.retryPoll()} /><${Button} store=${s} label="Reload" onClick=${() => location.reload()} />`
      : job.state === 'done' ? html`<${Button} store=${s} label="Close and refresh" onClick=${() => location.reload()} />`
        : job.state === 'paused' ? html`<${Button} store=${s} label="Review choices" onClick=${() => s.reviewChoices()} />` : null;
    return { status: IMPORT_STATUS[job.state], buttons, content: job.state === 'done' ? html`<${Finished} job=${job.result} />` : html`<p class="import-v1-progress">${job.progress}</p>` };
  }
  if (!p) return {};
  // Something already took message numbers inside the v1 range, so the history cannot keep its numbers.
  if (p.reservation_missed) return { status: 'This install can no longer import v1 history: part of the v1 message-number range is already in use here, and imported messages must keep their v1 numbers. Skipping closes the import permanently; the original backup stays on disk.', skip: true };
  const index = s.stepIndex, step = s.steps[index];
  if (!step) return {};
  const macros = step.id === 'macros' ? macroCards(p, s.mapping, s.destinationMacros) : null;
  let status = macros ? macros.status : stepStatus(step.id, p, s.mapping), content;
  const Page = PAGES[step.id];
  if (step.id === 'summary') status = s.report ? 'Dry run complete. This is what the import will do; nothing has been written yet.' : s.checking === 'running' ? 'Checking what would be imported…' : s.checking === 'failed' ? '' : 'Checking…';
  if (step.id === 'summary' && s.checking) content = s.checking === 'running' ? html`<p class="import-checking">Checking…</p>` : null;
  else {
    content = html`${s.staleNote && s.staleNote.step === s.stepID && html`<p class="import-restored import-stale-note">Some earlier choices no longer apply.</p>`}
      ${s.newProject && step.id !== 'projects' && html`<${NewProject} store=${s} actions=${actions} />`}
      <${Page} store=${s} actions=${actions} macros=${macros} />`;
  }
  const buttons = html`${index > 0 && html`<${Button} store=${s} label="Back" secondary=${true} onClick=${() => actions.go(s.steps[index - 1].id)} />`}
    ${step.id === 'summary' && s.report && html`<${Button} store=${s} label="Import now" onClick=${() => actions.commit()} />`}
    ${step.id !== 'summary' && html`<${Button} store=${s} label="Next" onClick=${() => actions.next()} />`}
    <${Button} store=${s} label="Start over" secondary=${true} onClick=${() => actions.startOver()} />`;
  const label = `Step ${index + 1} of ${s.steps.length} · ${step.title}`;
  return { status, content, buttons, skip: index === 0, totals: footerTotals(step.id, p, s.mapping), progress: { label, index: index + 1, of: s.steps.length } };
}

function Problems({ problems, actions }) {
  return html`<p class="import-problems-summary">${problemSummary(problems.length)}</p><ul class="import-problems">${problems.map(problem => html`<li><span>${`${problem.message} `}</span><button type="button" class="btn btn-link" onClick=${() => actions.focusProblem(problem)}>Go to the field</button></li>`)}</ul>`;
}

// The dialog's children. `refs` are the adapter's handles on the fixed parts (heading, progress, status, error, content, totals, actions,
// skip zone), which test-ui and the adapter read.
export function Wizard({ store, actions, refs }) {
  const shown = view(store, actions), progress = shown.progress, error = store.error;
  return html`<h2 ref=${refs.heading}>${store.title}</h2>
    <div class="import-progress" ref=${refs.progress} hidden=${!progress}><p class="import-progress-label" ref=${refs.progressLabel}>${progress?.label || null}</p>
      <div class="import-progress-bar" role="progressbar" ref=${refs.progressBar} aria-valuemin=${progress && '1'} aria-valuemax=${progress && String(progress.of)} aria-valuenow=${progress && String(progress.index)} aria-label=${progress?.label}>
        <span style=${progress ? { width: `${Math.round((progress.index / progress.of) * 100)}%` } : undefined}></span></div></div>
    <p class="settings-intro" ref=${refs.status}>${shown.status || null}</p>
    <div class="error" role="alert" ref=${refs.error}>${error?.problems ? html`<${Problems} problems=${error.problems} actions=${actions} />` : error || null}</div>
    <div class="import-v1-content" ref=${refs.content}>${shown.content || null}</div>
    <p class="import-totals" role="status" ref=${refs.totals} hidden=${!shown.totals}>${shown.totals || null}</p>
    <div class="dialog-actions" ref=${refs.actions}>${shown.buttons || null}</div>
    <div class="import-skip-zone" ref=${refs.skipZone}>${shown.skip && html`<${SkipOffer} store=${store} />`}</div>
    ${store.confirmingSkip && html`<${SkipConfirm} store=${store} />`}`;
}
