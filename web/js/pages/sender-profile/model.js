// What the read-only profile of a sender that is no longer a live member says: the facts from the loaded messages, and what the profile read
// adds. A historical sender is known only by what it wrote (imported from v1); a departed one is a v2 agent that left.

// What the loaded messages say about one sender.
export function senderFacts(messages, authorID) {
  const mine = [...messages].filter(message => message.author_id === authorID && message.author_kind === 'agent').sort((a, b) => a.seq - b.seq);
  const withModel = [...mine].reverse().find(message => message.author_model || message.author_harness);
  return { count: mine.length, first: mine[0]?.created_at || '', last: mine.at(-1)?.created_at || '', model: withModel?.author_model || '', harness: withModel?.author_harness || '' };
}

// The rows under the name, from the messages alone: [label, value]. `date` writes a time.
export function messageRows(facts, title, date) {
  return [['Runtime', title.replace(/ \((departed|historical)\)$/, '')], ['Model', facts.model], ['Harness', facts.harness], ['First heard', date(facts.first)], ['Last heard', date(facts.last)],
    ['Messages loaded', facts.count ? String(facts.count) : '']].filter(row => row[1]);
}

// The rows the profile read adds: [label, value, mono].
export function profileRows(profile, date, relative) {
  return [['Session', profile.native_session_id, true], ['Resume', profile.resume_command, true], ['CWD', profile.cwd, true], ['Branch', profile.branch, true], ['Machine', profile.machine_name],
    ['Project', profile.project_name], ...(profile.imported ? [['Last active in v1', date(profile.last_seen)]] : []),
    ['Spawned', profile.spawned_by ? `${profile.spawned_by}${profile.spawned_at ? ` · ${relative(profile.spawned_at)}` : ''}` : '']].filter(row => row[1]);
}

// The note at the foot of the dialog. A departed agent's is plain; a historical one depends on whether the import kept a session for it.
export function profileNote(kind, phase, profile) {
  if (kind !== 'historical') return 'This agent is no longer in the room.';
  if (phase === 'loading') return '';
  if (phase === 'done' && (profile.native_session_id || profile.resume_command || profile.cwd)) return 'This sender comes from imported v1 history.';
  return 'This sender comes from imported v1 history, and no session was kept for it, so only what it wrote is shown.';
}

// The words beside the button that starts a historical agent as a v2 agent.
export function startNotes(start) {
  const notes = [start.resumable ? `Continues the v1 session in ${start.cwd}.` : start.reason];
  if (start.resumable && start.outside_folder) notes.push('That folder is outside the project folder; the owner approves it first.');
  if (start.v1_active) notes.push('v1 saw it a moment ago and may still be using the session.');
  return notes.filter(Boolean).join(' ');
}
