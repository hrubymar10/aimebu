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

// The conversation as `tree` would print it, for the thread panel: the entries depth first (a message, then the subtrees of its replies, the replies in the
// order they were written), each with what its guide lines need. `guides` has one entry per level between the first message and the row's own: true where that level's
// ancestor has replies still to come (a vertical line goes on through the row), false where it was the last (blank). `last` says the row is its parent's last reply (an
// elbow, "\u2514\u2500\u2500") and not one with more after it (a tee, "\u251c\u2500\u2500"); `branch` is false for a root, which has no connector. Past `cap` levels no further step is
// added: the row keeps the cap's guides and says which message it answers instead (`deep`). A parent that is not among the entries (an older page is not loaded) makes its
// reply a root of its own (`orphan`). entries[0] is the first message. Returns { order, info }: the messages in tree order and the facts about each by id.
export const TREE_DEPTH_CAP = 6;
export function treeLayout(entries, cap = TREE_DEPTH_CAP) {
  const present = new Set(entries.map(message => message.id)), children = new Map(), roots = [], order = [], info = new Map();
  entries.forEach((message, index) => {
    if (index === 0 || !present.has(message.reply_to)) { roots.push(message); return; }
    if (!children.has(message.reply_to)) children.set(message.reply_to, []);
    children.get(message.reply_to).push(message);
  });
  const visit = (message, depth, guides, last, parent) => {
    const shown = Math.min(depth, cap);
    info.set(message.id, { depth, shown, guides: guides.slice(0, Math.max(0, shown - 1)), last, branch: depth > 0, deep: depth > cap, parent, orphan: depth === 0 && message !== entries[0] });
    order.push(message);
    const own = children.get(message.id) || [];
    own.forEach((child, index) => visit(child, depth + 1, depth > 0 ? [...guides, !last] : guides, index === own.length - 1, message));
  };
  roots.forEach(root => visit(root, 0, [], true, null));
  return { order, info };
}
