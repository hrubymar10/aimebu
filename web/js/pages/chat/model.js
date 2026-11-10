// One line of a message for a quote or a reply chip: whitespace collapsed, cut to 96 characters, "[attachment]" for a message that is only files.
// The words of a message without its Markdown syntax (bold, code, links, quotes and headings), for a one-line quote of it.
export const plainText = text => String(text || '')
  .replace(/^\s{0,3}(?:#{1,6}(?=\s|$)|>)\s?/gm, '')
  .replace(/!?\[([^\]]*)\]\([^)]*\)/g, '$1')
  .replace(/(\*\*|__|~~)(?=\S)(.+?)(?<=\S)\1/g, '$2')
  .replace(/(^|[\s(])([*_])(?=\S)([^*_\n]+?)(?<=\S)\2(?=$|[\s).,;:!?])/g, '$1$3')
  .replace(/`+([^`]*)`+/g, '$1');

export function messageSnippet(message) {
  let text = plainText(message?.body).replace(/\s+/g, ' ').trim();
  if (!text && message?.attachments?.length) text = '[attachment]';
  return text.length > 96 ? `${text.slice(0, 95).trimEnd()}…` : text;
}

export const MENTION_LOGINS_TTL_MS = 30000;

export const threadCount = (counts, serverCounts, id) => Math.max(counts.get(id) || 0, serverCounts?.get(id) || 0);
export const threadLabel = count => (count ? `Replies · ${count}` : 'Reply in thread');
export const threadTitle = count => (count ? `Replies · ${count}` : 'Replies');
export const threadChip = count => ({ hidden: !count, text: count === 1 ? '1 reply' : `${count} replies`, label: `Open thread, ${count} ${count === 1 ? 'reply' : 'replies'}` });
export const newRepliesLabel = count => `${count} new ${count === 1 ? 'reply' : 'replies'}`;
export const latestLabel = count => (count ? `Back to latest · ${count} new` : 'Back to latest');

export function groupReactions(entries, me) {
  const grouped = new Map();
  for (const entry of entries) { if (!grouped.has(entry.emoji)) grouped.set(entry.emoji, []); grouped.get(entry.emoji).push(entry); }
  return [...grouped].map(([emoji, members]) => ({ emoji, count: members.length, title: members.map(member => member.member_name || member.member_id).join(', '), pressed: members.some(member => member.member_id === me) }));
}

export const articleClass = (message, me) => (message.author_kind === 'system' ? 'chat-msg chat-msg-system' : message.author_kind === 'user' && message.author_id === me ? 'chat-msg self' : 'chat-msg');

export const liveArrival = (message, { initialLoading, contextMode }) => !initialLoading && !contextMode && message.author_kind !== 'system';

// What a live refresh does with one message of a page: 'skip', 'new-reply' (for the open panel, which is not at its end; the room still gets its row), 'panel'
// (a reply counted and shown in the panel only, while the room shows a history window), 'add' or 'missed' (counted on the Back to latest button of a history window).
export function liveRoute(message, state) {
  const { hasMoreOlder, oldestSeq, rootRendered, openThreadID, threadLatestHidden, threadEntryKnown, contextMode, known, missed } = state;
  // A row the live cap already dropped, returned by an overlapping refresh, would land out of order.
  if (hasMoreOlder && message.seq < oldestSeq) return 'skip';
  if (message.thread_root && message.thread_root === openThreadID && !threadLatestHidden && !threadEntryKnown) return 'new-reply';
  if (!contextMode) return 'add';
  if (message.thread_root && (openThreadID === message.thread_root || rootRendered)) return 'panel';
  if (!known && !missed) return 'missed';
  return 'skip';
}

// How far the read mark may go; `lastRowSeq()` reads the list and is asked only when the view is not the latest one.
export function newestShownSeq({ initialLoading, contextMode, catchingUp, newestSeq, lastRowSeq }) {
  if (initialLoading) return 0;
  if (!contextMode && !catchingUp) return newestSeq || 0;
  return lastRowSeq() || 0;
}
export const trimCap = (atBottom, liveCap, renderCap) => (atBottom ? liveCap : renderCap);
export const overCap = (rows, cap) => Math.max(0, rows - cap);

export const answerEligible = item => !!item && ['question', 'approval'].includes(item.kind) && !item.boundary_pending && item.can_decide !== false;

export function answerArea(item, message, answered) {
  if (answerEligible(item)) return 'controls';
  if (message && (message.proposed_answers?.length || message.open_questions?.length)) return 'history';
  return answered ? 'done' : 'none';
}

// A reply answers `item` when this person decides it and the text mentions at most the requester; an options-only ask takes only the text of an option.
export function answersThroughInbox(item, requester, body) {
  if (!answerEligible(item)) return false;
  const mentioned = [...String(body).matchAll(/(^|[^\w@`\\])@([a-z0-9][a-z0-9_-]*(?:@[a-z0-9_-]+)?)/gi)].map(match => `@${match[2].toLowerCase()}`);
  const who = String(requester || '').toLowerCase();
  if (!mentioned.every(name => name === who)) return false;
  if (item.free_text) return true;
  const text = String(body).trim(), plain = who && text.toLowerCase().startsWith(who) ? text.slice(who.length).trim() : text;
  return [...(item.proposed_answers || []), ...(item.options || [])].includes(plain);
}

export function recallCandidates(messages, me) {
  const own = [...messages].filter(message => message.author_kind === 'user' && message.author_id === me).sort((a, b) => a.seq - b.seq);
  const out = [];
  for (const message of own) if (out.at(-1) !== message.body) out.push(message.body);
  return out.slice(-200);
}

// Up walks back through own messages, Down forward (the draft in progress past the newest), Escape restores it; null when the key is not for this.
export function recallStep({ key, value, at, end }, recall, candidates) {
  const empty = !value;
  if (key === 'Escape' && recall.index !== null) return { index: null, draft: recall.draft, value: recall.draft };
  if (key === 'ArrowUp' && (empty || (at === 0 && end === 0) || (recall.index !== null && !value.slice(0, at).includes('\n')))) {
    if (!candidates.length) return null;
    const first = recall.index === null, index = first ? candidates.length - 1 : Math.max(recall.index - 1, 0);
    return { index, draft: first ? value : recall.draft, value: candidates[index] };
  }
  if (key === 'ArrowDown' && recall.index !== null && (empty || !value.slice(end).includes('\n'))) {
    const index = recall.index + 1;
    return index >= candidates.length ? { index: null, draft: recall.draft, value: recall.draft } : { index, draft: recall.draft, value: candidates[index] };
  }
  return null;
}

// Where each entry of a conversation sits in its tree, for the thread panel (a flat, time-ordered list in which a reply to an older message reads as the next step of the
// chain): its depth below the first message (capped at 3 for display), its lane (the first reply to a message continues the lane of that message, a later reply starts a new
// one: the lanes are the branches), whether it is a fork (its parent is not the row directly above) and who its parent is. A parent that is not among the entries (an older
// page is not loaded) counts as a fork of an unknown message. entries[0] is the first message.
export function forkLayout(entries) {
  const place = new Map(entries.map((message, index) => [message.id, index]));
  const depth = new Map(), lane = new Map(), continued = new Set(), layout = new Map();
  let lanes = 0;
  entries.forEach((message, index) => {
    if (index === 0) { depth.set(message.id, 0); lane.set(message.id, 0); layout.set(message.id, { depth: 0, lane: 0, fork: false, parent: null, parentIsRoot: false, known: true }); return; }
    const at = place.get(message.reply_to), parent = at !== undefined && at < index ? entries[at] : null;
    let own;
    if (parent && !continued.has(parent.id)) { continued.add(parent.id); own = lane.get(parent.id); } else own = ++lanes;
    const level = parent ? depth.get(parent.id) + 1 : 1;
    depth.set(message.id, level); lane.set(message.id, own);
    layout.set(message.id, { depth: Math.min(3, level), lane: own % 6, fork: parent ? at !== index - 1 : true, parent, parentIsRoot: !!parent && at === 0, known: !!parent });
  });
  return layout;
}
