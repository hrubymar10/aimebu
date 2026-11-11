# Importing v1 rooms, history, and memory

## Guided import in the web UI

The development build still defaults to `~/.aimebu2` and port 9897. For a
same-root cutover, point `AIMEBU2_CONFIG_DIR` at the v1 root and use the v1
port. Stop v1 before starting v2. The v2 server binds its HTTP and HTTPS
listeners before touching the root, so a running v1 server produces a clear
stop-and-retry error without modifying its files. If the development build
listens on a separate port while pointed at a v1 root, it probes v1's default
port 9997 and the configured v1 bind address (`AIMEBU_BIND`) before moving any
files. The
backup acquires a SQLite write lock before hashing the source and writing its
manifest, then holds that lock while renaming the source entries. Stop any
v1 server using a custom port.

On first startup over a v1 schema-1 root, v2 locks the root and moves the
v1-owned entries into `.old-v1-bkp.partial/` with same-filesystem renames. A
durable manifest lists every entry before the first move. An interrupted batch
resumes from that manifest (the resume runs the same checks as a first start, for example the owner login and password, before it moves another file, and a failure after it prints the same notice naming the backup); a complete batch becomes `.old-v1-bkp/` and is
never overwritten or merged. `switcher/` and `usages/` remain in place for
their v1-format readers. The backup retains the SQLite database, WAL, SHM,
attachments, and agent session metadata together. Do not edit or delete it.
Startup also copies a backed-up `server/credential-targets.json` into the new
server directory when no v2 registry exists, preserving external token
targets. The wizard lists the present file under **Files kept for v2**.
Before owner setup emits v2 events, startup reserves the highest v1 message
number in the shared sequence. Import refuses an overlap if that reservation
was missed, so displayed `#number` references never change.

Sign in as the v2 owner. The import wizard opens automatically when a backup is
ready on the first run. It is a stepper, one page per category with a progress
bar ("Step 3 of 8 · Rooms") and Back / Next: Projects, People, Rooms, Direct
messages, Memory, Macros, Roles, then a Summary. A page with nothing to show for
this backup is left out. Next refuses while anything on the page cannot be imported
as it stands (a project with no name or no absolute folder; a new account with a
bad, taken or repeated login; a chat turned on with no project) and lists all of
it at once: a link to each field, the same message beside each field, each field
marked invalid and tied to its message, and the first one focused. Back keeps
every choice. A Workspace folder field is empty until you ask: a house button at the left edge of the field
fills the home folder of the machine the server
runs on, and typed text is never replaced. As the owner types an absolute path,
the field offers matching directories on that machine (files are omitted;
hidden directories appear after typing `.`). Arrow keys choose a directory,
Enter or Tab completes it, `/` enters it, and Escape closes the list. The draft (the
choices and the page) is kept in the browser for each backup, so a reload
continues where you left off; a choice in it for something the backup no longer offers (for example a macro that is now left out) is dropped, with a note on the page that opens; **Start over** on the first page drops the draft. The
Summary is the dry run in words. It opens with one line of totals ("Bringing across
23,889 of 28,002 messages, …") and, per count of the backup (rooms, messages,
attachments, reactions), a block that adds up to the backup's own number: what is
imported, and every part left out with its reason (v1's presence and memory
bookkeeping lines, v1's own event room, direct messages not ticked, rooms with no
project, rooms chosen out, and for reactions `person_skipped`: a reaction by a person
mapped to nobody), with the rooms behind each part one click away (ids and message
counts, never text). An attachment is counted once, by the messages that refer to it:
imported if any imported message refers to it, otherwise left out with the reason of
the message that does (a room left out, a join or leave line), and "unused files"
only when no message refers to it (the table's `orphan_attachments` follows the same
rule). The report carries the same data as `accounting`
(`key`, `total`, `parts[]` of `key`, `count`, `items`); a count no named reason
covers shows as `other`, so the parts always sum to the total. Below that comes the
table of counts, ordered projects, rooms, messages, attachments first and settings
last (the columns that are all zero are left out, so "Skipped" appears only when
something is in it, and a non-zero cell is marked; what the backup already put in v2
is not listed), then what is not imported and why, merged rooms, and what v2 does not carry.
Choose **Import now** there, or **Skip the
import…** (a separate link on the first page only, never next to **Import
now**) and confirm **Skip import forever** in the dialog that names the
consequence. A completed or skipped import (skip after a completed import answers `410 import_closed`) cannot be reopened from
Settings, the normal API, or the CLI. The choice is stored in the v2 database.
The workspace offers Retry if startup reads time out; a late read still checks
the first-run import choice. An empty backup or no backup has no import wizard,
and reading the status never closes the offer by itself (a backup may still be
on its way): only **Skip import forever**, a completed import, or a used install
does. The review
suggests projects from what v1 itself names: an exact `x` + `x-leader` room pair, a
room that has messages and no leader pair (suggested as project `x` with the room in its root,
even when its agent has since left), and a memory scope (a memory scope with no matching project is suggested as a new project on the
first page). There is no catch-all project: a room with no project to go to starts as **Don't
import**, every direct message starts as **Don't import** (it is imported only when the owner
ticks it), and a room or memory scope that
reaches an import with no project is not imported and is listed in the report as
`skipped_no_project`. An import with no projects at all is allowed (the rest of the backup,
macros and the like, still imports). `_system` is never imported and is not listed: the preview skips it (with a `skip_reason`) and the importer ignores any mapping that puts it in a project, so a saved draft or a hand-written map cannot bring it across. A room with a message in the last 30 days starts open and an older one
archived; **Open all** and **Archive all** set every kept room at once. Each room has one
**Import into** choice: the project's root room, its `general` room, a new sub-room named like
the v1 room, or **Don't import**; a live tree of the v2 rooms (with message counts) sits beside
the list, **Archived after import** is a toggle on the room, and every project select ends
with **+ New project…**, which makes the project in place. Each room is
**placed**: in a separate room under the project's home room (the default), in the
**project root** (the home room itself), or in a **sub-room** of the root with a title
you choose (default `general`). For an exact `x` + `x-leader` pair the review suggests
`x-leader` into the root and `x` into a sub-room called `general`; a project's room `x` with
no `x-leader` goes into the root. Like a project made in v2, a project the import creates
starts with an open `general` task room under its home room, and the `general` sub-room
target is that room; importing into an existing project fills the project's own
**open** `general` task room rather than making a second one. A `general` the owner
has closed takes no new conversation, so v1 rooms that are imported open get a new
open `general` beside it and the closed one stays as it was; when every v1 room going
into the sub-room is imported archived, the owner's closed `general` takes them and no
second closed room of that name is made. Rooms placed in the
same root or the same-titled sub-room are merged: their messages keep their v1 numbers
and times and so interleave in time order. The review and the Check step list every merge
as a warning (`target_warnings` in the report). A merged sub-room is open when any of its
rooms is set to open, a root has no lifecycle, and a v1 read position is judged among
its own v1 room's messages (the larger position wins in a merged room). If any v1 room merged into
a target (the project root or a shared sub-room) had memory off, the target has memory off, whichever
room comes first; the wizard says so next to the target and the report lists it (`memory_off_targets`,
counted under room memory switches). The ledger records each v1 room against the room it landed in, and a shared
sub-room once (its key quotes the project and the title, so a project or title holding ':' cannot collide with another target), so a rerun adds nothing twice. Each room can be
mapped or skipped; add destination projects when unrelated rooms need
separate workspaces. Map each historical human to an existing v2 user, skip that person, or give
them a **new account** (`new_users` in the mapping, keyed by the v1 name, with a
login of 3 to 32 characters and a role of viewer or operator). The import makes
the account with a password nobody knows, attributes that person's messages to
it, and makes it a member, at that role, of every project its rooms are imported
into. Once the import has committed the server creates a one-time reset link for
each new account (valid an hour, working once, shown only on the finished page and
in the job); a failure to make a link is reported there and never undoes the import,
and the owner can make one in Settings → Users. A taken login, a duplicate login or a
malformed one is refused before anything is written. A person's messages show under the account they are mapped to, not under their v1 name (people mapped to one account are merged, and their old messages carry that account's name); only a skipped person, who has no account, keeps their v1 name, as a label. The person with
the owner's own name (any case; the exact spelling first, then sorted order) is suggested into the owner's account, and so is the one person in the data when there is only one (messages, members, reactions, memory profiles, human agent records with read cursors, and room preferences count: a lone writer next to people who never wrote is not guessed at, and the wizard says so)
(a single-person install is the owner, and mapping them elsewhere would leave their read positions, memory profile,
pins and reactions behind). Everyone else is not merged into the owner's account unasked: the server's suggestion gives
each of them a new viewer account named after them (`suggested.new_users`, with the matching `suggested.humans`), which
is also what the wizard shows; a saved skip stays a skip, and Start over suggests everyone again. A
skipped person's row says what skipping means, counted from the preview's `human_stakes` (memory profiles, reactions,
read positions and pinned or hidden rooms that belong to that person and would not be imported, which an import cannot
repeat). In the wizard a typed value counts at once, and Enter in a text
field goes to the next page (in the New project form it creates the project). Skipped people receive no
live room membership, and their imported rooms must be in owner-only
projects. A v1 direct message (`dm:a:b`) imports as a v2 direct message: it
is listed under Direct messages and titled by the other participant (an
agent's slug, `nina · wren` for two agents or an agent and
another person), and the wizard shows these in their own **Direct messages**
section with an import box and a project choice. No direct message is imported
unless the owner ticks it; the project is already filled in (the agent's own
project when the wizard proposes one), so ticking a chat needs no further
choice. Direct messages always import open, because v2 has no
archive for them; when a mapping asked for the archive default the Check
report lists those DMs under **Direct messages opened** (`opened_dms`). Each
entry shows its source id and message count, since two chats can share a title,
and the title follows the current people mapping. The owner is added to every imported DM, so a chat between two
agents is readable history for the owner; its other participants remain
historical names. A new account made for a person whose only link to a
project is a chat that goes into it gets access to that project; the Summary
lists each as "X gets viewer access to project Y through a direct message"
(`dm_only_access` in the report). Only a ticked chat counts, so a chat left
unticked leaves them out. The wizard offers separate
choices for project facts (each project, or **Don't import**), global profiles, and shared notes, each labelled with the number of
records it covers (the preview's `memory_counts`). Global profiles
and shared notes are included by default and arrive as ordinary records;
the Check report names every skipped memory record and its reason. If profiles
merged onto one v2 user exceed the 2048-byte profile cap, Check points to
**Skip global user profiles** or mapping people to separate users. It shows each
backed-up macro body and any matching v2 body, on a card with a badge for what will happen (New, Conflict: keeping or replacing the v2 text, Already in v2, Already imported); conflicts come first and long bodies are folded behind their first line. A conflicting key keeps the v2
text by default; choose the v1 text explicitly to replace it. A fresh v2
install already carries the shipped `do-cr` and `example_macro` defaults, so a
backup whose `do-cr` body differs shows that conflict too. v1 keeps a row for every built-in it ever seeded, so two kinds of row carry nothing the user wrote and are not offered: a built-in the user deleted (a row with no text) and a built-in still exactly as shipped (v2 seeds its own). The Macros page names them in a "Left out" line, and the Summary accounts for each under `v1_builtin_example` and `deleted_in_v1` in the `macros` block of `accounting`, so the totals still add up to the backup's rows. A built-in the user edited is theirs and is offered as any macro; an empty body, should one ever be offered, shows as "(empty)". Only macros
present in the backup import. The wizard marks keys already recorded in
that ledger and hides their conflict selector on later runs. Project memory
starts on for each destination project when v1's global memory was on (and off
otherwise); the owner can untick any project, and imported records are
ordinary and active.
The wizard shows v1 rooms whose memory override was off and preserves that
restriction in v2. It previews v1 role definitions; a v1 role that extends
another (including an overridden built-in specialist) keeps its parent as a
v2 `extends` link and imports only its own text, and conflicting v2 roles win
by default unless the owner selects the v1 definition. A link is kept only
to a parent that v2 has now or that the same import brings; a parent the owner
deleted after an earlier import, or one that is skipped, counts as missing and
its child is flattened from the v1 text (the preview marks it), as is a child
whose chain would exceed five ancestors. A role with no v1 body of its own takes
v2's text and v2's parent. Cyclic parents, and a flattened text over v2's
limit, are skipped with reasons. An import never stops on a link: if a conflict
choice changes a parent so a link would loop or run too deep, that role is
flattened and listed in the report's `flattened_roles`. Already imported roles are marked and their conflict selectors
are hidden after a saved choice. v1 fleets are not imported: v2 has no saved teams, so a fleet's rows are listed under **Not
carried over** and stay in the v1 backup on disk. The command `aimebu import-v1 --from <dir> --map <file> [--to <v2-config-dir>] [--dry-run]`
(`--to` defaults to `AIMEBU2_CONFIG_DIR`, else `~/.aimebu2`; it needs the v2 server
stopped and an existing v2 database) prints
its report as JSON and exits 3 with a warning when the map left rooms with no project, which are not imported.
The preview lists command options
that v2 does not carry over, including fixed room, terminal wrapping, and
working-directory behavior; v2 uses the room and checkout chosen at spawn
time. A skipped-settings list explains v1 preferences and
prompt overrides, which are not imported: v2's overridable sentences (Settings → Prompts) are
different and shorter, so the owner re-enters any still needed. Malformed settings or role JSON is
skipped with a reason while the rest of the backup remains importable.
v1's `messages_considered_expired_after_seconds` is carried into the v2 message-expiry
window ([message expiry](retention.md)) when the destination has no value of its own;
an existing v2 choice is never overwritten, and an out-of-range v1 value is listed
under skipped settings. v1's explicit `inline_plan_appendix` choice (`always` or `optional`) is carried the
same way into v2's inline-plans setting; a v1 install that never chose one carries nothing, and v2's default is `optional`.

Check runs the importer in a rolled-back transaction and reports imported,
already-present, skipped, reader, and access-gap counts. Import accepts only
the matching dry-run digest. While it runs, HTTP writes return a retryable
maintenance response and agent dispatch pauses; reads and progress polling
remain available. A machine sending a frame during import is disconnected with
a retryable close, then reconnects and replays journaled work. Attachment files
are staged before the database commit and
removed if it fails. The completion dialog shows the final imported,
already-present, and skipped counts. The `.v2-migrated` marker is synced only after a
successful import. Completed status records the one-time choice. If the
process stops after database commit but before the marker is written, the
first-run wizard can resume its saved mapping. Once the import completes or
the owner skips it, the API closes the import path.
Older manifests without a source key use the backed-up database to identify
the source. A marker for another source, or one without matching ledger rows,
blocks the wizard before any import writes.

### Rooms page

The wizard opens like the other first-run screens: one centred modal dialog over a full-page backdrop in the page colour, with the app behind it (top bar, sidebar, room header) hidden and unreachable until the import is done or skipped. The review pages share one dialog height, and the long ones (Projects, Rooms, Memory) keep a one-line total of what is selected and what is left out beside the buttons. The Direct messages page leads with the message count and keeps the raw v1 chat id in a tooltip unless two chats share a title. The finished page's heading reads "Import finished". Macro keys are shown as stored (lowercase). The first page carries the "only chance" warning as its own callout (the skip confirmation repeats it in the same words). A room that goes to a project's home room has no "Archived after import" box: the page says a home room is never archived. The Rooms page has a select per v1 room ("Import into": a project's root, its
general room, a new sub-room named like the room, or don't import), an
"Archived after import" checkbox and Open all / Archive all, and a tree of the
v2 rooms the choices make. Focus stays on the select, checkbox or bulk button
you used after each redraw. v1's own system room (`_system`) is never imported,
so it is left out of the list, the tree and the Summary, with a footnote saying
so. The lines v1's server wrote into rooms when an agent came or went (`x joined`,
`x left`, `x was kicked`, `x reconnected`, `x pruned after stale timeout`,
`x registered (...)`, `x deregistered`) are v1 bookkeeping and are not imported
either: they are not counted as the room's messages or activity, the report
lists them as skipped `presence_lines` (and the preview as not carried), and a
reply to one keeps no link. Only the system author's own wording is matched, so
a person typing the same words is imported. The Summary lists each room's destination ("Where each room goes", with
"(archived)" taken from the merged target: a sub-room shared by several v1
rooms is open when any of them is open, and a direct message row ends with its
v1 room id) so a wrong placement is visible before the import.

## Mapping choices

The first-run wizard builds a mapping for destination projects, room lifecycle,
human identities, project membership, memory scopes, macro and role conflicts,
and the memory switches. Fleets are not imported. The Summary folds its long lists until opened. A project's
workspace folder is prefilled with the working directory most of its v1 agents
ran from (sessions and each agent's registered cwd; ties go to the shorter, then
alphabetical path). Check refuses a blank or relative folder and links to the
field. When v1 memory was on, each project starts with v2 memory on. v1's personal
settings (theme, notification volume and sound, system events, the debug button)
are written to the owner's preferences, where v2 keeps them (`user_preference`), and
an existing v2 choice always wins; a theme v2 lacks, a custom sound or a malformed
value is listed under skipped settings. The Summary
table gives each count a plain name and one-line explanation. Human cursors map
to the owner, so unread counts carry over for rooms where v1 recorded them. Review project membership before importing: every member of a
v2 project can read its non-DM imported rooms. A mapped human DM member also
needs project membership to open that DM. The dry-run report counts readers,
access gaps, skipped records, and source fields that v2 does not carry.

The wizard imports from the read-only `.old-v1-bkp/` copy made during startup.
Keep its `server/aimebu.sqlite`, WAL and SHM files when present, and
`server/attachments/` together until import finishes. The development test
escape hatch `AIMEBU2_DEV_IMPORT_V1=1` enables the old CLI command against a
copied source; it is outside the normal first-run flow.

A second development-only escape hatch, `AIMEBU2_DEV_SKIP_V1_PROBE=1`, skips
the v1 listener probe on port 9997 (loopback, `AIMEBU_BIND`, and every
interface address) so a dev instance can rehearse first setup and import over
a copy of v1 data while the real v1 server keeps serving on 9997. Only the
exact value `1` skips the probe, and the server logs a warning line to stderr
when it does. The v1 daemon pid-file check and the backup's SQLite write lock
still apply. Never set it in production or against a v1 data folder that a
live v1 server uses.

The import preserves room and message history, replies, reactions, attached
files, project facts, user profiles, shared agent notes, global macros,
compatible global role definitions, each mapped human's read position per room,
and the owner-uploaded custom notification sounds present in the copied v1
SQLite database and its `sounds/` directory. A v1 read cursor (a message ID)
becomes the v2 read cursor of the mapped login at the newest imported message
at or below it, so a cursor past the last imported message clamps to that
message and the room imports as read up to there. A cursor only ever moves
forward; two v1 humans mapped to one login keep the larger one.
Cursors of skipped humans or rooms, and every agent's cursor, are not
imported; a v1 cursor that maps to no imported message leaves that room unread
for that person, as v1 had it. Imported history is not new to anyone, so in a
room the run creates, every other reader without a v1 cursor starts at the
newest imported message, the same baseline migration 43 gives existing rooms:
the project's members in an ordinary room, the chat's own members in a DM. The
preview counts the human cursors (`read_cursors`); the report lists imported
and skipped cursors and `read_cursor_baselines`.
The import runs once, into a fresh install. An install that already has
messages from people or agents (system notices do not count) and never started
an import is never offered it: status, preview, dry run and commit all close the
offer (the status goes to `skipped`, the others return 410), and the store
itself and the offline CLI refuse as well. The first start with a backup raises
the sequence counter and the message-number counter to the highest v1 message
number before anything else is written, so setup events land above the history
and native messages are numbered after it; each imported message keeps its v1
number as its `no`. If an event still sits inside
that range the status and preview report `reservation_missed` and the wizard
offers only **Skip import forever**; a number that is already taken makes the
import stop with a clear error instead of renumbering anything, so a second copy
of the same history cannot be imported on top of the first. Skipping a partly
finished import records it as done, not skipped.

Message IDs and displayed `#number` values retain their v1 numbers. Reply
links, attachment ownership, and reactions use those message IDs. Native
messages continue after the highest v1 number. Sender kind, harness, model,
attention, proposed answers, open questions, visual plans, and appendix pages
use native message fields. Unknown source fields appear under **Not carried
over** and remain only in the backup. Imported messages are historical: they
do not wake agents or create v2 inbox items. Project memory follows v1's global
memory setting (on for each destination project when v1 had it on) and the
owner can untick any project before importing. Imported project facts,
user profiles, and shared notes are ordinary Memory records: active at once,
editable and deletable like any other, with no review step and no stored v1
payload. Project facts only reach agents when the project's memory is on.
Memory fields v2 has no column for appear under **Not carried over** like
message fields do. Source references to
messages deleted in v1, or excluded by a wizard room skip, are not stored: v2
keeps no provenance for them (the memory record simply has no source message).
They are counted as unresolved links in the report, and the v1 backup still
holds the original references.

Leaderboards: v2 has none, so v1's rating cards (the `leaderboards` table) and its
`leaderboard_enabled` setting are left in the backup. They appear under **Not carried
over** and **Skipped settings**, and nothing in them can stop an import.

Custom sounds are checked one by one under the same limits as an upload: `.mp3`
or `.wav` (an entry without an extension is a legacy mp3), 1 byte to 1 MB, a
header that matches the extension, and at most 10 sounds in total counting
the sounds v2 already stores. The importer calls the upload path's own checks.
Sounds are taken oldest upload first (by their v1 upload time, then id; one
with no readable time comes last) until the limit is reached, so the pool keeps
the sounds that were there longest. A sound that is
missing from the backup, not a regular file, malformed (an unreadable
database row is skipped, not guessed at), or over a limit is
skipped and listed with its reason under "Skipped sounds"; it never fails the
import. Each imported sound gets a new v2 ID and a file written under
`sounds/`, staged so that nothing is left behind if the import does not commit
and a dry run writes nothing. v1's choice of sound is
one global server setting (`notification_sound`). A built-in sound that v2 also
has (`beep`, `chime`, `ding`, `knock`) is carried into the owner's preferences
with the volume; a custom one (`user:…`) appears under skipped settings, and the
owner picks it again in Settings → Notifications.

The importer leaves v1's leaderboard rating cards and its `leaderboard_enabled`
setting, agents' read cursors,
prompt overrides, usages, switcher credentials,
and runtime settings without a v2 equivalent behind. Original room JSON is
retained for inspection; only compatible room memory restrictions are applied.
Imported attachment bytes are sniffed for MIME and names are sanitized; an
image's width and height are carried, so its thumbnail holds its place in the
chat, and other attachment fields without a v2 column are listed under **Not
carried over** and stay only in the backup.

v1 agent sessions (`agent_sessions`: working directory, harness session id,
first and last seen) are kept for the read-only profile that an
imported sender's name opens in the chat. They are stored under the v1 agent id,
which is also the author id of that agent's imported messages, and are shown only
for an agent that wrote in the room being viewed. The preview counts them
(`agent_sessions`) and a row that is not valid JSON is skipped. v1's resume
command is not imported: it ran against v1 and would mislead in v2. The roles v1 gave
each agent in its rooms (the `roles` map of a v1 room) are kept as well
(`legacy_agent_role`), to suggest a role when the agent is started in v2. v1 vacates an agent's
roles when it deregisters, and a backup is taken after the agents left, so the roles are also rebuilt from the system
messages v1 wrote in each room, in message order: the last "`<agent>` was assigned as `<role>`" holds, a person's
"`<agent>` role cleared" removes it, and the deregistration's `role "<role>" vacated` notice is ignored. A role in the room's
own record wins.

### Start as v2 agent

An imported sender's profile (the dialog its name opens in the chat) offers
**Start as v2 agent** to an owner or operator of the project. It is never
automatic. The button opens the Spawn dialog for the old name (the
v1 slug, if it is free in the project and fits the agent-name rule; otherwise
the start is refused; the dialog has no Name field because the agent keeps it), the tool and model v1 ran, and the role v1 gave the
agent most often, if v2 has it (otherwise worker). The dialog has a **Start in**
picker listing the project's open task rooms, each named by where it sits (`project › room`, with the rooms it is nested under in between): the room that holds most of the
agent's imported messages (`history_room` in the profile's start info) when that is an open task
room, otherwise `general`. The dialog says where the history is in every case. When it is an
archived room the picker also offers "Reopen … and start there", which reopens it (the room
menu's reopen) and then starts the agent in it; when it is the project's home room, which
only a lead role can start in, it only says so. The agent
is started in the room picked there, not necessarily the room whose profile was
opened. The lookup is **per project**: an agent that wrote in any room of the
project can be started, so a worker whose v1 history is only in a home room
with **Home room: lead only** is started in `general`. An agent that wrote only
in another project is never found (404 `not_found`). Every check of the
chosen room still applies (permissions, limits, `v1_may_be_active`, and the
lead-only home room, which the picker does not offer; the server refuses a
non-lead role posted to it with 409 `home_is_lead_only`). The role is the
agent's own: the dialog lists every role and keeps the one v1 gave. A project
with no open task room cannot start it (Start stays off and the dialog says
so). After the start the web shows the room the agent was started in. The
dialog states what will happen; submitting sends `resume_legacy` with the v1
agent id on `POST /api/rooms/{room}/agents` for the room picked in Start in.
The server reads the session itself: the client never supplies a session id.

**Which session is offered.** v1 keeps one session per agent name, so a name that
another harness or model, or a later instance, used still holds an earlier one.
The session is chosen, in this order:

1. A v1 build that records them stamps each message with the session that sent
   it, and keeps every session of a name (`agent_session_history`). The import
   carries both (`message.author_session`, `legacy_agent_session_history`), and
   the session offered is the one that sent the sender's latest message, with
   the folder and model of that session.
2. Older messages carry no session. The host's supervisor then reads the project
   folder's Claude Code transcripts for the results of `bus_register` (the agent
   id) and `bus_say`/`bus_dm` (the message numbers) and offers the session that
   sent the sender's latest message, or else the one that sent most of its
   latest messages. Only ids come back, never text; a supervisor without
   `supports_session_attribution` skips this. The answer of a scan that ran is kept for
   ten minutes, so the profile and the start read the transcripts once (a look made
   while one is running waits for it); "no host connection yet" and "the supervisor
   is old" are not remembered, so the first look after an update reads them.
3. Otherwise the session v1 kept under the name, unless the sender's messages
   contradict it: another harness or model than the messages name, or a session
   last seen long before the agent's first message. Such a session is not
   offered and not shown as the sender's; the dialog says why, and the agent
   starts fresh with the same name.

- **Claude Code** sessions are **continued**, or, when the dialog's **How** choice
  says so, worked on as **a copy** (`resume_mode: fork`: the first turn forks the
  v1 session, the original stays as it is, and v1 still having it open is no
  obstacle; see [Working on a copy](claude-sessions.md#working-on-a-copy-fork)).
  Continuing is the same mechanism that continues any Claude Code session a machine has, see [Continuing a Claude Code session](claude-sessions.md): the new agent's session is
  created already started with v1's session id, so its first turn runs
  `claude --resume <id>`. Claude looks a session up under the working folder it
  was made in, so the agent works in **v1's working folder**, even outside the
  project folder: then it waits for the owner's boundary approval in the Inbox
  like any agent outside its folder, and its notice says so. A folder that no
  longer exists on the chosen machine refuses the start. On the host machine
  with the native launcher the server also checks that the transcript exists
  (`session_not_found`). Claude names a working folder's transcript
  directory by turning every character that is not a letter or digit into a
  dash; a transcript found under some other name is not refused, because the
  naming has cases worth leaving to claude (long paths, other versions), and a
  failing first turn surfaces like any failed turn. The dialog leaves the
  machine's project folder out of it, since the agent runs in v1's folder.
  A containerized launcher keeps its own sessions and cannot
  continue one (`needs_native`) unless it reports that it shares the host's Claude home
  (`claude_home_shared`, [sessions](claude-sessions.md)); another machine is not checked, and a failing
  first turn surfaces like any failed turn.
- **codex, pi, opencode and vibe** sessions cannot be continued: v2 runs each with a
  private home or data directory per agent, so a session v1 made is not visible
  to it. The dialog says so, and the agent starts **fresh with the same name**
  (still linked to the v1 agent). Harnesses v2 has no adapter for (cursor,
  aider) are the same.
- If v1 saw the agent in the last ten minutes it may still be using the session
  (`v1_may_be_active`); two processes on one transcript corrupt it, so the
  dialog asks to confirm that v1 is stopped.
- The name is the v2 agent's for good (names are unique in a project), so an
  agent is started once (`already_started`); afterwards the historical profile
  says "Running as v2 agent X" and opens it, the v2 agent's profile says it
  continues the v1 session of the old agent (`resumed_from`), and the agent is
  restarted with the ordinary controls.

Each mapped human's hidden and pinned rooms (v1's `room_prefs`) become that
login's v2 view preferences: a hidden room becomes the sidebar's Hide. v2 has no pins: a pinned room puts the project it landed in at
the front of that person's manual project order (the `sidebar_project_order` preference), and only when they have no order of their
own yet (a pinned direct message has no project in the tree and is skipped). Rows for agents,
rows with neither flag, and rows for skipped humans or rooms are not imported;
two v1 people mapped to one login merge their flags. An existing v2 preference
always wins. The
preview counts them (`room_prefs`) and the report lists imported,
already-present and skipped preferences.

Nothing is dropped silently. The preview, the dry run and the final report carry
`not_carried`, a map of every v1 SQLite table or message field the importer
does not carry to its record count. Table names come from the source schema;
unknown message keys are counted from source JSON. The wizard shows both under
**Not carried over**. v1 agent
sessions are also read to suggest workspace folders.
