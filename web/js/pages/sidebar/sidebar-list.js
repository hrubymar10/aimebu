import { html, useEffect, useLayoutEffect, useRef, useState } from '../../ui.js';
import { previewText } from '../../preview.js';
import { formDialog } from '../../dialogs.js';
import { roomSilenced, mutedNow } from '../../room-notify.js';
import { countedActive, leafView, roomStatus, roomType, tracksStatus, treeOf } from './model.js';
import { loadPreviewAll } from './store.js';
import { ProjectStatus } from './project-status.js';

const CHEVRONS = { down: '6 9 12 15 18 9', right: '9 18 15 12 9 6' };

function GlyphButton({ name, label, className, ...rest }) {
  return html`<button type="button" class=${className} title=${label} aria-label=${label} ...${rest}><span class=${`icon-mask icon-mask-${name}`} aria-hidden="true"></span></button>`;
}

function Chevron({ direction }) {
  return html`<svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><polyline points=${CHEVRONS[direction]}></polyline></svg>`;
}

// The ≡ handle of a project's home row arms the row for dragging from the press until it ends anywhere (the pointer may be released elsewhere, or the
// touch cancelled); the drop marks live on the row's classes while a drag is on.
const clearMarks = () => document.querySelectorAll('.drop-before, .drop-after').forEach(node => node.classList.remove('drop-before', 'drop-after'));

function dragHandlers(room, store) {
  const same = () => { const from = store.dragging && store.projects.get(store.dragging), to = store.projects.get(room.project_id); return from && to && from.id !== to.id && store.sort.peek() === 'manual'; };
  const after = (event, row) => event.clientY > row.getBoundingClientRect().top + row.getBoundingClientRect().height / 2;
  return {
    onDragStart: event => {
      if (store.sort.peek() !== 'manual') { event.preventDefault(); return; }
      store.dragging = room.project_id; event.dataTransfer?.setData('text/plain', room.project_id);
      if (event.dataTransfer) event.dataTransfer.effectAllowed = 'move';
      event.currentTarget.classList.add('dragging');
    },
    onDragEnd: event => { store.dragging = null; event.currentTarget.draggable = false; event.currentTarget.classList.remove('dragging'); clearMarks(); },
    onDragOver: event => { if (!same()) return; event.preventDefault(); const below = after(event, event.currentTarget); clearMarks(); event.currentTarget.classList.add(below ? 'drop-after' : 'drop-before'); },
    onDragLeave: event => event.currentTarget.classList.remove('drop-before', 'drop-after'),
    onDrop: event => {
      if (!same()) return;
      event.preventDefault();
      const below = after(event, event.currentTarget), id = store.dragging; clearMarks(); store.dragging = null;
      store.dropProject(id, room.project_id, below);
    },
  };
}

function arm(event, store) {
  const row = event.currentTarget.closest('.room-item');
  row.draggable = true;
  const release = () => { for (const done of ['pointerup', 'pointercancel']) document.removeEventListener(done, release, true); if (!store.dragging) row.draggable = false; };
  for (const done of ['pointerup', 'pointercancel']) document.addEventListener(done, release, true);
}

// In manual sort, a project's home row has a ☰ button for its actions (a new room; moving it up or down) and as the handle the row is dragged by.
// The menu is a real menu: Escape or a click elsewhere closes it, the arrows
// move between its items, and it is reachable by keyboard and touch because the button is always on the row's tab order while the row is hovered/focused.
function ProjectMenu({ project, room, controls, manual, store, onAdd, disabled }) {
  const [open, setOpen] = useState(false), box = useRef(null);
  useEffect(() => {
    if (!open) return undefined;
    const outside = event => { if (box.current && !box.current.contains(event.target)) setOpen(false); };
    document.addEventListener('click', outside);
    return () => document.removeEventListener('click', outside);
  }, [open]);
  useLayoutEffect(() => { if (open) box.current?.querySelector('.project-menu-item:not(:disabled)')?.focus(); }, [open]);
  const pick = action => () => { setOpen(false); box.current?.querySelector('.project-menu-btn')?.focus(); action(); };
  const keys = event => {
    if (!open) return; // a closed menu leaves every key (Escape closes the phone overlay and the panels) to the page
    const items = [...box.current.querySelectorAll('.project-menu-item:not(:disabled)')], index = items.indexOf(document.activeElement);
    if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); setOpen(false); box.current.querySelector('.project-menu-btn').focus(); }
    else if (event.key === 'ArrowDown' && items.length) { event.preventDefault(); items[(index + 1) % items.length].focus(); }
    else if (event.key === 'ArrowUp' && items.length) { event.preventDefault(); items[(index - 1 + items.length) % items.length].focus(); }
  };
  return html`<span class=${open ? 'project-menu is-open' : 'project-menu'} ref=${box} onKeyDown=${keys}>
    <button type="button" class=${manual ? 'row-btn project-move project-menu-btn project-grip' : 'row-btn project-move project-menu-btn'} title=${manual ? `${project.name} actions (drag to move)` : `${project.name} actions`} aria-label=${`${project.name} actions`} aria-haspopup="menu" aria-expanded=${String(open)} onPointerDown=${event => { if (manual) arm(event, store); }} onClick=${() => setOpen(!open)}><span class="icon-mask icon-mask-menu" aria-hidden="true"></span></button>
    ${open && html`<div class="project-menu-list" role="menu" aria-label=${`${project.name} actions`}>
      <button type="button" class="row-btn project-menu-item project-menu-add" role="menuitem" disabled=${disabled} onClick=${pick(onAdd)}>New room</button>
      ${manual && html`<button type="button" class="row-btn project-menu-item project-menu-up" role="menuitem" disabled=${controls.first} onClick=${pick(() => store.moveProject(project.id, -1))}>Move up</button>
      <button type="button" class="row-btn project-menu-item project-menu-down" role="menuitem" disabled=${controls.last} onClick=${pick(() => store.moveProject(project.id, 1))}>Move down</button>`}
    </div>`}</span>`;
}

// New task room under a room: the row's + button and its context menu ask the same questions and make the room the same way.
export async function newRoomDialog(store, room) {
  const generation = store.generation; // the answer belongs to the list the dialog was opened in
  const answer = await formDialog({ title: `New task room under ${room.title}`, confirm: 'Create room', fields: [
    { name: 'title', label: 'Room name', required: true, requiredMessage: 'Give the room a name.' },
    { name: 'worktree', type: 'checkbox', label: 'Create a Git worktree for this room', hint: 'It starts from committed HEAD; uncommitted base changes stay in the project folder.' },
    { name: 'base_ref', label: 'Base branch or ref (optional)', placeholder: 'Current HEAD', showIf: values => values.worktree },
  ] });
  if (!answer || !store.owns(generation)) return;
  await store.createRoom(room, answer, generation);
}

function Row({ view, store, ctx, preview, onToggle }) {
  const { room } = view, project = store.projects.get(room.project_id), [reopening, setReopening] = useState(false);
  const archivedProject = !!project?.archived_at, projectName = project?.name || '';
  const controls = room.kind === 'home' && project && view.controls ? view.controls : null, manual = ctx.sort === 'manual';
  const affected = store.health.get(room.id) || [], quiet = roomSilenced(room),
    attention = room.kind === 'home' ? ctx.projectAttention.get(room.project_id) || 0 : room.attention_count || 0,
    members = room.member_count || 0;
  const isActive = ctx.active === room.id, type = roomType(room), status = roomStatus(room), counted = !tracksStatus(room);
  const classes = ['room-item', isActive ? 'active' : '', room.kind === 'dm' ? 'dm' : '', view.unread > 0 ? 'has-unread' : '', attention > 0 ? 'has-attention' : '', room.lifecycle === 'closed' ? 'archived' : '', room.lifecycle === 'closing' ? 'closing' : '', controls && manual ? 'project-draggable' : ''].filter(Boolean).join(' ');
  const select = () => store.onSelect(store.rooms.get(room.id) || room);
  const label = `${view.collapsible && view.folded ? 'Expand' : 'Collapse'} ${room.title}`;
  const add = () => newRoomDialog(store, room);
  const reopen = async () => { setReopening(true); if (!(await store.reopen(room))) setReopening(false); };
  const drag = controls ? dragHandlers(room, store) : {};
  const statusCounts = room.kind === 'home' && !archivedProject ? ctx.statusCounts.get(room.project_id) : null;
  const hasProjectStatus = statusCounts && statusCounts.needs + statusCounts.waiting + statusCounts.working > 0;
  const memberBadge = counted && html`<span class="room-item-count" role="img" aria-label=${`${members} ${members === 1 ? 'member' : 'members'}`} title=${`${members} ${members === 1 ? 'member' : 'members'}`}>${String(members)}</span>`;
  const healthBadge = (affected.length > 0 || store.seenHealth.has(room.id)) && html`<span class="room-health" hidden=${!affected.length} title=${affected.map(agent => `${agent.name}: ${agent.flags.map(flag => flag.code.replaceAll('_', ' ')).join(', ')}`).join('\n')}>${`⚠ ${affected.length}`}</span>`;
  const attentionBadge = attention > 0 && html`<span class="room-item-attention" role="img" title=${`${attention} waiting for you`} aria-label=${`${attention} items need your attention`}>${`🔔 ${attention}`}</span>`;
  const row = html`<div class=${classes} onClick=${event => { if (!event.target.closest('button, input, select, a')) select(); }} ...${drag}>
    ${view.collapsible ? html`<button type="button" class="row-btn branch room-collapse" title=${label} aria-label=${label} aria-expanded=${String(!view.folded)} onClick=${onToggle || (() => store.toggleCollapsed(room.id))}><${Chevron} direction=${view.folded ? 'right' : 'down'} /></button>` : view.nest && html`<span class="branch-spacer" aria-hidden="true"></span>`}
    <span class="room-item-icon-group"><span class=${`room-item-icon room-type-${type.key}`} data-room-type=${type.key} role="img" aria-label=${type.label} title=${type.label}><span class=${`icon-mask icon-mask-${type.icon}`} aria-hidden="true"></span></span></span>
    <div class="room-item-info"><div class="room-item-top"><span class="room-name-line">
    <button type="button" class="row-btn room-item-name" aria-current=${isActive ? 'true' : undefined} title=${`${room.title} · ${members} ${members === 1 ? 'member' : 'members'}`} onClick=${select}>${room.title}</button>
    <span class="room-item-actions-slot"><span class="room-item-actions">${controls && manual && html`<${ProjectMenu} project=${project} room=${room} controls=${controls} manual=${manual} store=${store} onAdd=${add} disabled=${archivedProject || (!!room.lifecycle && room.lifecycle !== 'open')} />`}
    <${GlyphButton} name="plus" label=${`New task room under ${room.title}`} className="row-btn add-room" disabled=${(!!room.lifecycle && room.lifecycle !== 'open') || archivedProject} hidden=${room.kind === 'dm' || room.lifecycle === 'closed' || archivedProject} onClick=${add} /></span></span>
    ${view.chip && projectName && !(room.kind === 'home' && room.title.trim().toLowerCase() === projectName.trim().toLowerCase()) && html`<span class=${ctx.filtering ? 'room-project room-project-needed' : 'room-project'} title=${`Project ${projectName}`}>${projectName}</span>`}
    ${!hasProjectStatus && memberBadge}
    </span>
    ${room.restricted && html`<span class="room-restricted" role="img" aria-label="Restricted room" title="Restricted room"><svg width="13" height="13" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true"><rect x="3" y="7" width="10" height="7" rx="1.5" /><path d="M5 7V5a3 3 0 0 1 6 0v2" /></svg></span>`}
    ${!hasProjectStatus && healthBadge}
    ${quiet && html`<span class="room-muted" role="img" title=${mutedNow(room) ? `Muted until ${new Date(room.muted_until).toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' })}` : 'Notifications are off for this room'} aria-label=${mutedNow(room) ? `Muted until ${new Date(room.muted_until).toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' })}` : 'Notifications are off for this room'}><span class="icon-mask icon-mask-bell-disabled" aria-hidden="true"></span></span>`}
    ${status ? html`<span class=${`room-state room-status room-status-${status.key}`} role="img" title=${status.title} aria-label=${`${status.label}: ${status.title}`}>${status.label}</span>` : room.lifecycle === 'closing' && html`<span class="room-state">Closing…</span>`}
    ${!hasProjectStatus && attentionBadge}
    ${view.mention && html`<span class="room-item-mention" role="img" title="You were mentioned" aria-label="You were mentioned"></span>`}
    ${view.unread > 0 && html`<span class="room-item-unread" role="img" title=${`${view.unread} unread ${view.unread === 1 ? 'message' : 'messages'}`} aria-label=${`${view.unread} unread messages`}>${String(view.unread)}</span>`}
    ${hasProjectStatus && html`<span class="project-badges"><span class="project-badge-group">${healthBadge}${memberBadge}${attentionBadge}</span><${ProjectStatus} counts=${statusCounts} id=${`project-status-${room.id}`} projectName=${projectName} folded=${view.folded} onExpand=${() => { if (view.folded) store.toggleCollapsed(room.id); }} /></span>`}
    </div>${preview && html`<div class="room-item-preview">${previewText(room.last_message_body) || (room.user_message_count > 0 ? '📎 Attachment' : room.message_count > 0 ? 'Only system messages' : 'No messages yet')}</div>`}</div>
    ${room.lifecycle === 'closed' && !archivedProject && html`<${GlyphButton} name="reopen" label=${`Reopen ${room.title}`} className="row-btn reopen-room" disabled=${reopening} onClick=${reopen} />`}</div>`;
  return row;
}

function Group({ view, store, ctx, className = '', onToggle }) {
  const { room } = view;
  const preview = room.id === ctx.active || ctx.previewAll;
  const rooms = view.children.map(child => html`<${Group} key=${child.room.id} view=${child} store=${store} ctx=${ctx} />`);
  if (view.divider > 0) rooms.splice(view.divider, 0, html`<div key="system-divider" class="room-divider" role="separator"></div>`);
  const kids = [...rooms, ...view.tail.map(node => html`<${Tail} key=${`${node.kind}:${node.project.id}`} node=${node} store=${store} ctx=${ctx} />`)];
  return html`<div class=${`room-group${view.folded ? ' collapsed' : ''}${className ? ` ${className}` : ''}`} data-room-id=${room.id}><${Row} view=${view} store=${store} ctx=${ctx} preview=${preview} onToggle=${onToggle} />${kids.length > 0 && html`<div class="room-children">${kids}</div>`}</div>`;
}

// A project whose rooms have not arrived: its name where its row will be, the same height as the folded row that replaces it.
function Placeholder({ project, preview }) {
  // With every row's last message on, a project row carries a preview line too: an empty one keeps the height.
  return html`<div class="room-group project-placeholder" data-project-id=${project.id} aria-busy="true"><div class="room-item"><span class="branch-spacer" aria-hidden="true"></span><span class="room-item-icon-group"><span class="room-item-icon room-type-home" data-room-type="home" aria-hidden="true"><span class="icon-mask icon-mask-house" aria-hidden="true"></span></span></span><div class="room-item-info"><div class="room-item-top"><span class="row-btn room-item-name">${project.name || ''}</span></div>${preview && html`<div class="room-item-preview" aria-hidden="true">\u00a0</div>`}</div></div></div>`;
}

function Fold({ id, className, open, store, children, summary, heading = false, ...rest }) {
  return html`<details class=${className} open=${open} onToggle=${event => { if (event.currentTarget.open !== open) store.setFold(id, event.currentTarget.open); }} ...${rest}><summary class=${heading ? 'sidebar-section-heading' : 'sidebar-fold-heading'}><${Chevron} direction=${open ? 'down' : 'right'} /><span>${summary}</span></summary>${children}</details>`;
}

function ArchivedProject({ project, rooms, store, ctx, filtering }) {
  const home = rooms.find(room => room.kind === 'home') || rooms[0];
  if (!home) return null;
  const id = `archived-project:${project.id}`, open = filtering || !!store.folds.get(id);
  const view = { ...leafView(home, ctx.counted), nest: true, collapsible: true, folded: !open,
    children: rooms.filter(room => room.id !== home.id).map(room => leafView(room, ctx.counted)) };
  return html`<${Group} view=${view} store=${store} ctx=${ctx} className="archived-project" onToggle=${() => store.setFold(id, !open)} />`;
}

function ArchiveSearch({ project, store }) {
  const value = store.archiveFilter.get(project.id) || '';
  return html`<input type="search" class="archive-search" placeholder="Search archived rooms and messages" aria-label="Search archived rooms and messages" value=${value} onInput=${event => store.searchArchive(project.id, event.currentTarget.value)} />`;
}

function ArchiveHits({ project, store }) {
  const state = store.archiveHits.get(project.id);
  return html`<div class="archive-hits" hidden=${!state || (!state.error && !state.hits?.length)}>${state?.error ? html`<p class="archive-search-error">Could not search archived messages.</p>` : (state?.hits || []).map(hit => html`<button type="button" class="row-btn archive-hit" onClick=${() => store.openHit(hit)}><strong>${hit.room_name || ''}</strong><span>${hit.snippet || ''}</span></button>`)}</div>`;
}

function Tail({ node, store, ctx }) {
  const { project } = node;
  if (node.kind === 'archived') {
    const id = `archived:${project.id}`;
    return html`<${Fold} id=${id} className="archived-rooms" open=${!!store.folds.get(id)} store=${store} summary=${`Archived (${node.count})`}><${ArchiveSearch} project=${project} store=${store} /><${ArchiveHits} project=${project} store=${store} />${node.rooms.map(room => html`<${Group} key=${room.id} view=${leafView(room, ctx.counted)} store=${store} ctx=${ctx} />`)}<//>`;
  }
  const id = `hidden:${project.id}`;
  return html`<${Fold} id=${id} className="hidden-rooms" open=${!!store.folds.get(id)} store=${store} summary=${`Hidden (${node.rooms.length})`}>${node.rooms.map(room => html`<button type="button" key=${room.id} class="row-btn hidden-room-show" data-hidden-room-id=${room.id} title=${`Show ${room.title}`} aria-label=${`Show ${room.title}`} onClick=${async () => { if (await store.setPreference(room, 'hidden', false)) store.onSelect(store.rooms.get(room.id) || room); }}><span class="icon-mask icon-mask-eye" aria-hidden="true"></span>${room.title}</button>`)}<//>`;
}

function Offer({ offer, store }) {
  const [joining, setJoining] = useState(false);
  return html`<div class="room-item agent-chat-offer" data-offer-id=${offer.id}><span class="room-item-icon-group"><span class="room-item-icon room-type-agent-chat" data-room-type="agent-chat" role="img" aria-label="Chat between agents" title="Chat between agents"><span class="icon-mask icon-mask-switch" aria-hidden="true"></span></span></span><div class="room-item-info"><div class="room-item-top"><span class="room-item-name" title=${`${offer.title} · ${offer.message_count} ${offer.message_count === 1 ? 'message' : 'messages'}`}>${offer.title}</span></div></div>
    <${GlyphButton} name="person-plus" label=${`Join ${offer.title}`} className="row-btn agent-chat-join" disabled=${joining} onClick=${async () => { setJoining(true); await store.join(offer); setJoining(false); }} /></div>`;
}

// The box on top is a plain filter: typing narrows the list to matching rooms and projects. The New project button of the header sits beside it, and
// returns to the header when the box goes.
function FilterBox({ store, button }) {
  const form = useRef(null);
  useLayoutEffect(() => {
    const header = document.querySelector('.sidebar > header');
    if (button && form.current) form.current.append(button);
    return () => { if (button && header && button.parentElement !== header) header.insertBefore(button, header.querySelector('#edge-rooms')); };
  }, [button]);
  return html`<form class="room-filter" ref=${form} onSubmit=${event => event.preventDefault()}><input type="text" class="room-filter-input" placeholder="Filter rooms…" autocomplete="off" aria-label="Filter rooms" value=${store.filter.value} onInput=${event => store.setFilter(event.currentTarget.value)} /></form>`;
}

export function SidebarList({ store, projectButton }) {
  store.version.value;
  const shown = store.shown.value, sort = store.sort.value, active = store.active.value, filter = store.filter.value, manual = store.manual.value, held = store.held.value;
  const counted = countedActive(active, held);
  const build = () => treeOf({ projects: store.projects, rooms: store.rooms, filter, sort, manual, collapsed: store.collapsedRooms, active, held, archiveFilter: store.archiveFilter, offers: store.joinable,
    pending: store.pending, provisional: store.holdFolds, seen: store.seenRooms, expected: store.focusRoom() });
  if (shown) store.revealActive(active);
  let tree = shown ? build() : null;
  // A project seen for the first time starts collapsed: fold it, then draw the tree again with it folded.
  if (tree && !store.holdFolds && store.foldNew(tree.collapsible, tree.nested, tree.holdsActive)) tree = build();
  const exposed = new Set();
  const addTail = node => { if (node.kind === 'archived') for (const room of node.rooms) exposed.add(room.id); };
  const addView = view => {
    exposed.add(view.room.id);
    for (const child of view.children) addView(child);
    for (const node of view.tail) addTail(node);
  };
  for (const section of tree?.sections || []) {
    if (section.tailOnly) addTail(section.node);
    else if (!section.placeholder) addView(section);
  }
  for (const { rooms } of tree?.archived || []) for (const room of rooms) exposed.add(room.id);
  const projectAttention = new Map();
  for (const id of exposed) {
    const room = store.rooms.get(id);
    if (!room || room.kind === 'dm' || !room.attention_count) continue;
    projectAttention.set(room.project_id, (projectAttention.get(room.project_id) || 0) + room.attention_count);
  }
  const ctx = { sort, active, counted, previewAll: loadPreviewAll(), filtering: !!tree?.filtering, projectAttention, statusCounts: store.projectStatus };
  useLayoutEffect(() => {
    if (!tree) return;
    store.collapsible.value = tree.collapsible;
    for (const id of tree.closedProjects) store.dropArchive(id);
    store.scheduleMuteEnd();
  });
  if (!tree) return null;
  const text = filter.trim().toLocaleLowerCase();
  const offersOpen = text ? true : !!store.folds.get('offers');
  return html`<${FilterBox} store=${store} button=${projectButton} />
    ${tree.sections.map(section => section.placeholder ? html`<${Placeholder} key=${`placeholder:${section.project.id}`} project=${section.project} preview=${ctx.previewAll} />` : section.tailOnly ? html`<${Tail} key=${`${section.node.kind}:${section.node.project.id}`} node=${section.node} store=${store} ctx=${ctx} />` : html`<${Group} key=${section.room.id} view=${section} store=${store} ctx=${ctx} />`)}
    ${tree.noMatch && html`<p class="rooms-empty">${tree.noMatch}</p>`}
    ${tree.archived.length > 0 && html`<${Fold} id="archived-projects" className="archived-projects" open=${tree.filtering ? true : !!store.folds.get('archived-projects')} store=${store} summary=${`Archived projects (${tree.archived.length})`} heading>${tree.archived.map(({ project, rooms }) => html`<${ArchivedProject} key=${project.id} project=${project} rooms=${rooms} store=${store} ctx=${ctx} filtering=${tree.filtering} />`)}<//>`}
    <div class="direct-rooms"><h3>Direct messages</h3>${tree.direct.map(room => html`<${Group} key=${room.id} view=${leafView(room, counted, tree.many)} store=${store} ctx=${ctx} />`)}
      ${tree.offers.length > 0 && html`<${Fold} id="offers" className="agent-chat-offers" open=${offersOpen} store=${store} summary=${`Agent chats you can join (${tree.offers.length})`}>${tree.offers.map(offer => html`<${Offer} key=${offer.id} offer=${offer} store=${store} />`)}<//>`}</div>`;
}
