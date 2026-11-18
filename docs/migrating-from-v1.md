# Migrating from aimebu v1

This guide moves your existing rooms and history into v2 on the same machine.
Keep the v1 executable until you have checked the result. The commands below
use v1's usual data folder (`~/.aimebu`) and port (`9997`); substitute your
own folder and port if you changed them.

For the full backup, cutover, agent restart, rehearsal, and rollback sequence,
follow the [cutover runbook](cutover-runbook.md).

Expect the whole move to take a few minutes of your attention. The import itself
is quick: a rehearsal with a real history of about 21,000 messages, 3,600
reactions and 325 attachments (150 MB) took 0.2 s to review, 1.3 s to check and
4.4 s to import. v2 keeps the untouched v1 files as a backup and writes its
own copy of the attachments, so plan for roughly twice the attachment space on
disk until you delete the backup.

This `.old-v1-bkp/` copy is separate from v2's schema-migration backups. On
later starts, if an existing v2 database needs a schema migration, startup
first writes a verified online snapshot to `server/backups/` and normally keeps
the new copy plus the four newest older ones. A fresh v2 database needs no
schema backup. If the snapshot or its integrity check fails, startup stops
before changing the schema; fix the backup error and retry. If deletion of an
older copy fails, startup logs a
warning and continues with the verified new backup. These database snapshots
do not include attachments,
switcher credentials, or agent runtime files.

The v1 backup must be in place **before the first v2 start**: that start reserves
v1's message numbers so the imported history keeps them, and a backup that
arrives later cannot be imported (the wizard then offers only to skip).

## 0. Check token refresh before stopping v1

If v1 uses **Auto-refresh tokens**, wait for any in-flight refresh to finish.
Run the journal check immediately before stopping v1:

```sh
jq '.journal|length' "$HOME/.aimebu/switcher/refresh-state.json"
command -v harness-docker-ctrl
docker info >/dev/null && echo ok
env | grep '^AIMEBU_TOKEN_LEAD_' || true
```

The journal count must be `0`; it is a snapshot, so stop v1 promptly after
checking it. `harness-docker-ctrl` and a working `docker` CLI with a reachable
daemon (`ok` above) must be available to the process that starts v2. The last command must print nothing in the environment that starts
v2; also check the running v1 server's startup environment if it was launched
by another service. A stopped
refresh in the `started` phase is not spent again; that account may need a new
login. Stop v1's quota watchdog before cutover. Stop any development v2 server
that uses the same Claude or Codex live files. Two layers guard this. The
server holds a lock beside each live credential file it refreshes and refuses a
second refresher on the same file, whatever its config directory; the refused
one reports a `refresher_conflict` alert in `GET /api/credentials/targets`.
`aimebu doctor` additionally fails if `~/.aimebu` and `~/.aimebu2` both mark the
same tool as token-only; it checks only those two folders and is a pre-flight,
not the guard.

## 1. Stop v1

If v1 runs as a daemon, use its `aimebu server stop` command. If it runs in a
terminal, press Ctrl+C there. Make sure no other v1 server is still using the
data folder, including one on a custom port.

What v2 checks for you: a v1 daemon recorded in `server/aimebu.pid` that is
still alive (any port), a listener on 9997 or on v2's own host addresses, and
SQLite's write lock on the v1 database. What it cannot see: a v1 started in a
terminal on a custom port (no pid file) while it is idle, because an idle
server holds no write lock. Stop it yourself; v2 starting on a folder that a
live v1 still uses would split the data between them.

## 2. Start v2 with the old data folder

Use the **v2** `aimebu` executable. Its default data folder and port match
v1, so the usual cutover starts with:

```sh
aimebu server
```

If v1 used a custom data folder or port, set `AIMEBU_CONFIG_DIR` and
`AIMEBU_PORT` to those values before starting v2. These variable names are
shared by v1 and v2; the v2 process reads the values you set for it.

Leave this terminal open. The server prints a one-time setup token and a
shortcut link, `http://127.0.0.1:9997/setup?token=…` ([owner setup](auth.md#first-run-owner-setup)).
Open the address, enter the token (or open the link), choose the new v2 owner login and password, and you land in the
import wizard. (Headless? Set `AIMEBU_OWNER_LOGIN` and
`AIMEBU_OWNER_PASSWORD` before starting and sign in at
<http://127.0.0.1:9997/> instead.) During startup, v2 moves everything at the root of
`~/.aimebu/` (`server/`, `agents/` and any other entries) into
`~/.aimebu/.old-v1-bkp/`, except `switcher/` and `usages/` (which stay in place)
and v2's own markers and lock. Keep that folder: it is your
pre-migration copy. Do not start v1 on the original folder afterwards: it
would see no history and build an empty database next to v2's (`aimebu doctor`
fails on that mix). v2 checks the owner variables (if you set them), the turn-limit environment,
the credential registry and folder write access before it moves anything; when
the switcher is token-only (Auto-refresh tokens was on) it also requires
`harness-docker-ctrl` on the PATH and a docker daemon that answers `docker info`; if one
fails, nothing has moved. A failure after the move, or part way through
it, prints where the backup (or the unfinished `.old-v1-bkp.partial`) is and how
to restore it; the next start finishes an unfinished move. If startup says v1 is still running, stop v1 and run the v2
command again; v2 refuses to touch the folder while anything still listens on
v1's port.
The switcher stays at `~/.aimebu/switcher/` in its v1 format. V2 reads those
profiles directly under the same `switcher/.lock`; it does not copy them from
the backup or merge profiles from a development `~/.aimebu2/switcher/` tree.
Keep the v1 server and its agent wrappers stopped while switching accounts.
V2 copies the backed-up `server/credential-targets.json` into its new server
directory before starting the refresher, preserving external token targets.
The wizard lists that file under **Files kept for v2**. If v1 left Codex
waiting for running sessions to end, v2 may migrate it immediately at first
start once those sessions are gone.

## 3. Review and import

At first owner sign-in, the **Review → Check → Import** wizard opens. Complete
the import during setup. **Skip import forever** closes the import path; it
cannot be reopened from Settings or the normal API or CLI. The same holds
after a completed import: it cannot be run twice, and every import route,
including skip, answers `410 import_closed`. (Repeating a skip is the one no-op
that still answers `204`.)

The wizard walks you through these choices one page at a time (Projects, People, Rooms,
Direct messages, Memory, Macros, Roles, Summary) and keeps your place if you reload.
Before importing, **Start over** on any page clears your choices and returns to Projects, after you confirm "Start over?" (**Discard**; **Keep editing**, or Esc, changes nothing):

- where each v1 room goes. Rooms are suggested into projects (a `x` room and
  its `x-leader` room share one; the leader room is suggested into the project's
  root room and the other into a `general` sub-room; an `x` room with no `x-leader`
  is suggested into the root; each room can instead be a
  separate room, any sub-room title, or skipped; rooms sent to the same place merge,
  and the wizard warns before you check); v1's `_system` room is never imported and is not listed. A room with
  starts open regardless of its last message; change
  any room, or use **Open all** / **Archive all**. Each project needs a
  workspace folder: v2 suggests the working directory most of that project's v1
  agents ran from (even if they used several checkouts, such as git
  worktrees), and **Check** refuses, with a link to the field, until every
  project has an absolute folder;
- which v2 login each v1 person maps to, and who may read each project;
- which memory records to bring across (project memory starts on when v1's was on; they arrive as ordinary records in
  **Memory**);
- what happens to shared macros that clash with a v2 one (v2 text is kept by
  default; the shipped `do-cr` and `example_macro` count as v2's own);
- nothing about v1 fleets: they are not imported (v2 has no saved teams),
  and the Summary lists them under **Not carried over**.

The **Summary** is a dry run. It shows the counts, any access gaps, every v1 setting
that has no v2 equivalent, and a **Not carried over** list of the v1 data v2
does not bring. Resolve the gaps, then choose **Import** and wait for completion.
The finished page lists imported rooms, messages, historical agents, and destination
projects with their workspace folders. Running it again changes nothing. The detailed choices are explained in the
[import guide](import-v1.md).

What comes across: rooms, messages, replies, reactions and attachments; project
facts, user profiles and shared notes; macros; compatible global roles; the owner's custom notification sounds (at most ten
`.mp3` or `.wav` files of 1 MB; any that do not fit are listed with the reason);
each mapped person's hidden rooms and pinned rooms (a pinned room moves its project to the front of their manual project order); and each person's read position
per room. A room opens read up to where v1 had you. A room you had no position
for starts fully read, so thousands of old messages do not appear as unread.

What does not: v1's leaderboard rating cards and on/off setting (v2 has no
leaderboards; they stay in the backup, listed under **Not carried over**), v1's liveness windows and other server settings v2 does not use (your theme, notification volume and sound, system events and debug button are carried to your profile), plans (v1's Plans resource, which v2 replaced with inline
visual plans on messages) and per-agent usage history. v1 agent sessions are kept
for the profile of an imported sender, but agents themselves start fresh sessions. Pick a custom notification sound again in **Settings**,
and expect agents to start fresh sessions.
When enabling v2's quota switcher, set its account order to the order you
want **today**, including any after-reset order v1 had already reached. V2
starts without v1's reset latch, so an old pre-reset order could otherwise
remain in effect for a week.

The wizard preserves v1 message numbers, including `#number` references and
reply links: a v1 message's number is its v2 message number (`no`). New v2
messages continue after the highest v1 message number, in every room.
See the [import guide](import-v1.md) for mapping choices.

## 4. Check the result

Open a few familiar rooms and confirm their messages, replies, reactions, and
attachments. Check who can see each project and private chat. Imported rooms
start open by default. Rooms you chose to archive appear in each project's
**Archived** list, with unread counts when they have unread messages. Reopen
the rooms you want to keep working in. Old AI sessions do
not resume automatically. Check imported memory in **Memory** (it arrives as ordinary
records, and project facts reach agents only once you enable the project's
memory), check **Settings → Agents → Harnesses** for each harness and its models (the v1 import does not carry model lists), and run
`aimebu doctor` to confirm the database and listener are healthy.

## If the wizard reports a changed backup

If the import status answers `409 backup_changed` ("the v1 backup was modified
after it was made"), something wrote to `.old-v1-bkp/` after the move, typically
a v1 started on the original folder or on a copy of the backup. v2 refuses to
import a backup it can no longer verify. Stop every v1 server, then either put
a pristine copy of the backup back in place (a copy made before v1 touched it)
or skip the import in the wizard and keep working from v2 alone. The wizard opens
in this case too, with that message, a **Skip import forever** button and a
**Decide later** button; Decide later closes it without deciding, and it opens
again on the next sign-in until the backup is restored or skipped.

## Roll back to v1

Stop the v2 server first. Copy the complete backup into a **new** folder and
start your saved v1 executable against that copy:

```sh
restore="$HOME/aimebu-v1-restore-$(date +%Y%m%d-%H%M%S)"
mkdir "$restore"
cp -Rp "$HOME/.aimebu/.old-v1-bkp/." "$restore/"
ln -s "$HOME/.aimebu/switcher" "$restore/switcher"
ln -s "$HOME/.aimebu/usages" "$restore/usages"
AIMEBU_CONFIG_DIR="$restore" /path/to/v1/aimebu server serve
```

To retry the move later, do not reuse the restore folder in place: populate a
**new** root from it again, because the rolled-back v1 keeps writing (message
numbers v2 had reserved collide with what it writes), and a v2 started on the
restore folder would take it for a fresh v1 root. Know what the copy brings:
`cp -Rp` also copies `manifest.json` from the backup (v1 ignores it; v2 uses it
to verify the backup, so a restore folder that v1 has since written to reports
`backup_changed`), and the `usages` symlink dangles if `~/.aimebu/usages` does
not exist yet.

Replace `/path/to/v1/aimebu` with your v1 executable. This restores the state
from just before the move; work done in v2 is not in the v1 backup. Your v2
data stays in the original folder. If your original folder was not
`~/.aimebu`, change the backup and symlink paths above. `switcher/` and
`usages/` stay outside `.old-v1-bkp/`; the links keep v1's token refresher
and settings active. Do not start v2 on the original root while this v1
restore runs: the two servers would share token state and live files.
If the complete backup is absent or only `.old-v1-bkp.partial/` exists, leave
the files in place and seek help before trying to restore them.
