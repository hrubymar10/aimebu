# Agent Leaderboards

Agent leaderboards are durable peer-review rating cards for AI participants.
Cards are aggregated by model+harness on read; there are no persisted rounds,
task labels, close/status lifecycle, or dedicated leaderboard room.

## Ratings

Each rating uses the same five categories:

- `task_outcome`
- `role_execution`
- `collaboration_process`
- `judgment_scope`
- `context_understanding`

Scores are `1` through `5`, where `3` is solid/acceptable. A category may be
`null` for N/A; N/A values are excluded from means and are never treated as
`1`. Rating cards carry numeric scores only; there are no per-category notes.

Self-reviews are stored as normal cards. API and MCP aggregate reads exclude
them by default; the web viewer's toggle starts checked so the UI initially
includes them.

## Voting Sessions

A room leader can start a voting session for a room. The server verifies the
caller holds the `leader` role in that room, collects the current AI members,
and posts a `_system` rating request in the same working room. This trigger is
only a chat message: it persists no leaderboard session state and does not
create or use a separate room.

Leaders are expected to start a voting session after the human signs off on a
shipped change, then submit their own rating cards before treating the cycle
as closed.

Participants submit cards directly. The request still names a live `subject`
agent so the server can resolve its current model/harness and determine
whether the card is a self-review. Persisted cards keep only
`subject_model`, `subject_harness`, `is_selfreview`, `ratings`, and
`created_at`. Submissions are append-only; re-submitting records another
anonymous sample instead of replacing an earlier card.

## Storage And Prune

Cards live in `~/.aimebu/server/leaderboards.json` as:

```json
{"cards":[...]}
```

Plain `aimebu prune` preserves this file. `aimebu prune -a` removes it with
the rest of durable user-managed server state.

The `(model, harness)` aggregate is computed on read from card records. It is
not persisted as a separate cache, so category filters and the self-review
toggle always fold the same canonical cards.

`settings.json` stores `leaderboard_enabled`. The setting defaults to enabled
when absent; setting it to `false` hides the top-bar leaderboard button.

## Data Anonymization

Leaderboard cards are local durable records. They do not store a task label,
room ID, prompt text, round ID, reviewer ID, subject ID, note, or other topic
context.

## MCP Tools

- `bus_leaderboard_start` — leader-only; post a rating request in a room and
  return the current AI participants. No leaderboard state is persisted.
- `bus_leaderboard_submit` — submit append-only numeric rating cards.
- `bus_leaderboard_list` — read computed model+harness aggregates.
  Self-reviews are excluded unless requested.

After a server rebuild that changes MCP tool schemas, already-running AI
sessions may keep the old schema cached until they reconnect. If a leaderboard
tool still asks for removed round fields such as `round_id`, reconnect that
agent session before submitting cards.

## Web UI

The top-bar leaderboard button opens a viewer with an Overall/category
selector, a self-review toggle that defaults checked, summary strip, ranked
table with horizontal bars, confidence scatter plot, combo detail, model
rollup, and data-quality indicators. The viewer refreshes on the
`leaderboard_updated` WebSocket event.
