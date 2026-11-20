import { Picker } from './picker.js';
import { agentLabel } from './sender-identity.js';
// @mention candidates, composer autocomplete and in-message highlighting. Resolution follows the
// server's addressing order (internal/store/wake.go resolveMentionTargets): group tags win, then an
// exact unique slug (agent slugs and human logins share one namespace), then an assigned role key;
// `slug@<project>` selects an agent explicitly, the project being named by its id or by its name (a name only when it is a valid
// mention qualifier, [a-z0-9_.-]+: any other name is offered and read by id). It is deliberately narrower than the server:
// member-ID tokens are not recognised, so those mentions go unhighlighted.
const GROUP_TAGS = [
  ['everyone', 'all members of this room'], ['all', 'alias for @everyone'], ['channel', 'all members of this room'],
  ['here', 'active room members'], ['humans', 'human members of this room'], ['ais', 'AI members of this room'],
];
const GROUPS = new Set(GROUP_TAGS.map(([tag]) => tag));
// The project name agentLabel(card) puts after the slug ('' when the project is unknown); a name may itself contain an @.
const projectOf = card => { const label = agentLabel(card), slug = card?.slug || card?.name || ''; return label.length > slug.length ? label.slice(slug.length + 1) : ''; };
const QUALIFIER = /^[a-z0-9_.-]+$/i;
const TOKEN = /@([a-z][a-z0-9_-]*(?:@[a-z0-9_.-]+)?)/gi;

export class MentionIndex {
  constructor(cards = [], projectID = '', logins = [], historical = [], me = '') {
    // The viewer's own login: a mention of it is highlighted as theirs.
    this.me = String(me || '').toLowerCase();
    // Authors seen in the room's history whose agent record is gone (departed, or imported from v1): their @name still reads as a mention.
    this.historical = new Set(historical.map(name => String(name).toLowerCase()));
    this.projectID = String(projectID || '');
    // The same agent can arrive twice (the room's member list and its roster both carry it): one card per agent id.
    const seen = new Set();
    cards = cards.filter(card => { const id = card.id; if (!id) return true; if (seen.has(id)) return false; seen.add(id); return true; });
    this.cards = [...cards].sort((a, b) => String(a.slug).localeCompare(String(b.slug)));
    this.humans = [...new Set(logins.map(String))].sort((a, b) => a.localeCompare(b));
    this.agentSlugs = new Set(this.cards.map(card => String(card.slug).toLowerCase()));
    this.slugs = new Map();
    for (const slug of [...this.cards.map(card => String(card.slug)), ...this.humans]) this.slugs.set(slug.toLowerCase(), (this.slugs.get(slug.toLowerCase()) || 0) + 1);
    this.roles = new Map();
    for (const card of this.cards) if (card.role_key) { const key = card.role_key.toLowerCase(); if (!this.roles.has(key)) this.roles.set(key, { emoji: card.role_emoji || '', slugs: [] }); this.roles.get(key).slugs.push(card.slug); }
  }
  get signature() { return JSON.stringify([this.me, this.projectID, this.humans, this.cards.map(card => [card.slug, card.role_key, this.qualifier(card)]), [...this.historical].sort()]); }
  // The qualifier that tells same-named agents apart, per agent: its project's name when that is a valid qualifier, else its project's id
  // (the room's project for a card that carries none).
  qualifier(card) {
    const name = projectOf(card);
    return QUALIFIER.test(name) ? name : String(card.project_id || this.projectID || '');
  }
  // Whether `slug@qualifier` names one of the room's agents: by its project's id or its name (whatever qualifier() offers is read back).
  qualified(slug, qualifier) {
    if (!qualifier) return false;
    return this.cards.some(card => String(card.slug).toLowerCase() === slug && [this.qualifier(card), card.project_id || this.projectID, projectOf(card)].some(name => name && String(name).toLowerCase() === qualifier));
  }
  isSelf(token) { return !!this.me && token === this.me; }
  // A mention that opens a profile: a live agent of the room, or an author of its history whose agent record is gone.
  // The qualified form (`slug@project`) names an agent even when a person's login shares the slug, so it does not need the slug to be unique.
  opensProfile(token) {
    const at = token.indexOf('@'), slug = at < 0 ? token : token.slice(0, at);
    if (this.historical.has(token)) return true;
    if (at >= 0) return this.qualified(slug, token.slice(at + 1));
    return this.agentSlugs.has(slug) && this.slugs.get(slug) === 1;
  }
  resolve(token) {
    if (GROUPS.has(token)) return 'group';
    const at = token.indexOf('@');
    if (at >= 0) return this.qualified(token.slice(0, at), token.slice(at + 1)) || this.historical.has(token) ? 'mention' : null;
    const matches = this.slugs.get(token) || 0;
    if (matches === 1) return 'mention';
    if (matches === 0 && this.roles.has(token)) return 'role';
    return matches === 0 && this.historical.has(token) ? 'mention' : null;
  }
  candidates(prefix) {
    const match = key => !prefix || key.startsWith(prefix);
    const items = GROUP_TAGS.filter(([tag]) => match(tag)).map(([tag, preview]) => ({ insertText: `@${tag}`, preview }));
    for (const [key, role] of [...this.roles].sort((a, b) => a[0].localeCompare(b[0]))) {
      if (GROUPS.has(key) || this.slugs.has(key) || !match(key)) continue;
      items.push({ insertText: `@${key}`, preview: `${role.emoji} role · ${role.slugs.join(', ')}`.trim() });
    }
    const members = this.cards.map(card => ({ slug: String(card.slug), insertText: this.slugs.get(String(card.slug).toLowerCase()) > 1 && this.qualifier(card) ? `@${card.slug}@${this.qualifier(card)}` : `@${card.slug}`, preview: `${card.role_emoji || ''} ${card.model || 'unknown'} · ${card.tool || 'unknown'}`.trim(), project: agentLabel(card).split('@')[1] || '' }));
    // A human login that collides with another member has no unambiguous form, so it is not offered.
    for (const login of this.humans) if (this.slugs.get(login.toLowerCase()) === 1) members.push({ slug: login, insertText: `@${login}`, preview: 'human' });
    members.sort((a, b) => a.slug.localeCompare(b.slug));
    for (const member of members) if (match(member.slug.toLowerCase())) items.push({ insertText: member.insertText, preview: member.preview, project: member.project });
    // Authors of the room's history that are no longer in it (departed, or imported) come last: the name is text, it addresses no one.
    for (const name of [...this.historical].sort()) {
      if (name.includes('@') || this.slugs.has(name) || GROUPS.has(name) || this.roles.has(name) || !match(name)) continue;
      items.push({ insertText: `@${name}`, preview: 'not in room' });
    }
    return items;
  }
}

const SKIP = 'code, pre, a, .mention, .md-escaped, .raw-codeblock';
function unwrap(root) {
  for (const span of root.querySelectorAll('.mention')) span.replaceWith(document.createTextNode(span.textContent));
  root.normalize();
}
// Wraps live mentions in <span class="mention">; idempotent, and never touches code or links.
export function highlightMentions(root, index) {
  unwrap(root);
  if (!index) return;
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT, { acceptNode: node => node.parentElement?.closest(SKIP) ? NodeFilter.FILTER_REJECT : NodeFilter.FILTER_ACCEPT });
  const nodes = [];
  for (let node; (node = walker.nextNode());) if (node.nodeValue.includes('@')) nodes.push(node);
  for (const node of nodes) {
    const value = node.nodeValue, fragment = document.createDocumentFragment();
    let last = 0;
    TOKEN.lastIndex = 0;
    for (let match; (match = TOKEN.exec(value)) !== null;) {
      const raw = match[1].replace(/[-_]+$/, ''), kind = index.resolve(raw.toLowerCase());
      if (!kind) continue;
      if (match.index > last) fragment.append(document.createTextNode(value.slice(last, match.index)));
      const span = document.createElement('span');
      const token = raw.toLowerCase();
      span.className = kind === 'group' ? 'mention mention-group' : index.isSelf(token) ? 'mention mention-self' : 'mention'; span.textContent = `@${raw}`;
      // A mention of an agent opens its profile; it is a button for the keyboard too.
      if (kind === 'mention' && index.opensProfile(token)) { span.tabIndex = 0; span.setAttribute('role', 'button'); span.title = 'Open profile'; }
      else if (index.isSelf(token)) span.title = 'You';
      fragment.append(span); last = match.index + 1 + raw.length;
    }
    if (!last) continue;
    if (last < value.length) fragment.append(document.createTextNode(value.slice(last)));
    node.replaceWith(fragment);
  }
}

export class MentionAutocomplete {
  constructor(draft) {
    this.draft = draft; this.index = null; this.picking = false; this.items = [];
    this.list = new Picker(draft, 'mention-commands', index => this.pick(index));
    this.popup = this.list.popup;
    draft.addEventListener('input', () => this.refresh());
    draft.addEventListener('click', () => this.refresh());
    draft.addEventListener('keyup', event => { if (['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) this.refresh(); });
    draft.addEventListener('focus', () => this.refresh());
    draft.addEventListener('keydown', event => this.keydown(event));
    draft.addEventListener('blur', () => setTimeout(() => { if (!draft.hasFocus?.()) this.close(); }, 120));
  }
  setIndex(index) { this.index = index; if (this.list.open) this.refresh(); }
  reset() { this.index = null; this.close(); }
  match() {
    const before = this.draft.value.slice(0, this.draft.selectionStart);
    const match = /(^|\s)@([a-z0-9_-]*)$/i.exec(before);
    return match ? { start: before.length - match[2].length - 1, prefix: match[2].toLowerCase() } : null;
  }
  refresh() {
    if (this.picking) return;
    const match = this.index && this.match();
    this.items = match ? this.index.candidates(match.prefix) : [];
    if (!this.items.length) { this.close(); return; }
    // A member is listed by its slug alone; only when another member shares it is the full form (`@slug@project`) shown, as that is what gets inserted.
    this.list.show(this.items.map(item => ({ key: item.insertText, preview: item.preview })));
  }
  keydown(event) { this.list.keydown(event); }
  pick(index) {
    const match = this.match(), item = this.items[index];
    if (!match || !item) return;
    const position = this.draft.selectionStart, after = this.draft.value.slice(position);
    const space = !after || (!/\s/.test(after[0]) && !/[,.!?;:)\]}>]/.test(after[0])) ? ' ' : '';
    const inserted = item.insertText + space;
    this.draft.value = this.draft.value.slice(0, match.start) + inserted + after;
    this.draft.setSelectionRange(match.start + inserted.length, match.start + inserted.length);
    this.close();
    // The synthetic input (for autosize and draft listeners) must not reopen the popup on the token just picked.
    this.picking = true;
    try { this.draft.dispatchEvent(new Event('input', { bubbles: true })); } finally { this.picking = false; }
    this.draft.focus();
  }
  close() { this.list.close(); this.items = []; }
}
