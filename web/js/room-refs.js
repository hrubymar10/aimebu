import { MentionAutocomplete } from './mentions.js';
// Room references: a body stores `#room_<id>` (the room's full id, as agents write it); a person may type
// `#<title>` and the composer resolves it to the id on send — an exact, case-insensitive title match against
// the rooms the person can see, a bare `#<number>` (a message reference) and an id typed directly left alone.
// What a reader may see comes from the message's entities table: a named room renders as `#<title>` and is a
// link to the room (`#<title>-2` when two visible rooms share a title in the view), a room without a name
// renders as plain `#private-room`, and an id with no entry as plain `#unknown-room`.
const TOKEN = /#(room_[a-z0-9]+)/gi;
const TYPED = /(^|\s)#([a-z0-9][a-z0-9_.+\-/]*)/gi;
// The room-ref autocomplete is the mention autocomplete over a RoomRefIndex: the base class's refresh reads
// this.index.candidates(prefix), which the index provides; only the token differs.
export class RoomRefAutocomplete extends MentionAutocomplete {
  match() {
    const before = this.draft.value.slice(0, this.draft.selectionStart);
    const match = /(^|\s)#([a-z0-9]*)$/i.exec(before);
    return match ? { start: before.length - match[2].length - 1, prefix: match[2].toLowerCase() } : null;
  }
}

export class RoomRefIndex {
  constructor(rooms = []) {
    this.rooms = rooms.filter(room => room?.id && room?.title);
    // The rooms the person can see, keyed by their lower-cased title.
    this.byTitle = new Map();
    for (const room of this.rooms) {
      const key = String(room.title).toLowerCase();
      const held = this.byTitle.get(key) || [];
      held.push(room);
      this.byTitle.set(key, held);
    }
  }
  // The room id a typed `#<token>` names: one visible room whose title equals the token; nothing for a bare
  // number (a message reference), an id typed directly, or a title two or more visible rooms share.
  resolve(token) {
    const lower = String(token || '').toLowerCase();
    if (!lower || /^room_[a-z0-9]+$/.test(lower) || /^[0-9]+$/.test(lower)) return null;
    const held = this.byTitle.get(lower);
    return held && held.length === 1 ? held[0].id : null;
  }
  // The draft with every typed title that names exactly one visible room replaced by the room's id.
  rewrite(text) {
    return String(text || '').replace(TYPED, (whole, lead, token) => {
      const id = this.resolve(token);
      return id ? `${lead}#${id}` : whole;
    });
  }
  // The picker's candidates: the visible rooms whose title starts with what was typed.
  candidates(prefix) {
    const match = room => !prefix || String(room.title).toLowerCase().startsWith(prefix);
    return this.rooms.filter(match).map(room => ({ insertText: `#${room.id}`, preview: `room · ${room.title}` }));
  }
}

const SKIP = 'code, pre, a, .msg-ref, .room-ref, .md-escaped, .raw-codeblock';
// How a referenced room is shown in one body: the entities entry says what the reader may see, and the
// ordinals (per view, first appearance keeps the plain title) disambiguate a shared title.
export function renderRoomRefs(root, entities, ordinals) {
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT, { acceptNode: node => node.parentElement?.closest(SKIP) ? NodeFilter.FILTER_REJECT : NodeFilter.FILTER_ACCEPT });
  const nodes = [];
  for (let node; (node = walker.nextNode());) if (node.nodeValue.includes('#')) nodes.push(node);
  for (const node of nodes) {
    const value = node.nodeValue, fragment = document.createDocumentFragment();
    let last = 0;
    TOKEN.lastIndex = 0;
    for (let match; (match = TOKEN.exec(value)) !== null;) {
      const entity = (entities || []).find(item => item?.kind === 'room' && String(item.id || '').toLowerCase() === match[1].toLowerCase());
      const span = document.createElement(entity?.name ? 'a' : 'span');
      if (entity?.name) {
        const n = ordinals?.get(entity.id) || 0;
        span.className = 'room-ref';
        span.href = `#${entity.id}`;
        span.textContent = `#${entity.name}${n > 1 ? `-${n}` : ''}`;
        span.title = `Open ${entity.name}`;
      } else {
        span.className = 'room-ref room-ref-plain';
        span.textContent = entity ? '#private-room' : '#unknown-room';
      }
      if (match.index > last) fragment.append(document.createTextNode(value.slice(last, match.index)));
      fragment.append(span);
      last = match.index + match[0].length;
    }
    if (!last) continue;
    if (last < value.length) fragment.append(document.createTextNode(value.slice(last)));
    node.replaceWith(fragment);
  }
}

// The ordinals of one view: the rooms its messages name, in order of first appearance; a title two rooms
// share shows the later ones as `#<title>-2`, `#<title>-3`, ... Returns the map (room id → ordinal, absent
// or 1 when plain) and a signature that changes whenever the naming does.
export function roomOrdinals(messages) {
  const seen = new Map(); // lower title → [room ids in appearance order]
  for (const message of messages) {
    for (const entity of message?.entities || []) {
      if (entity?.kind !== 'room' || !entity.id || !entity.name) continue;
      const key = String(entity.name).toLowerCase();
      const held = seen.get(key) || [];
      if (!held.includes(entity.id)) { held.push(entity.id); seen.set(key, held); }
    }
  }
  const ordinals = new Map();
  let signature = '';
  for (const held of seen.values()) {
    for (let index = 1; index < held.length; index++) { ordinals.set(held[index], index + 1); signature += `${held[index]}:${index + 1},`; }
  }
  return { ordinals, signature };
}
