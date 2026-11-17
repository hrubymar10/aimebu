# Cutover runbook: v1 data to v2

Use this for a same-machine cutover. Keep the v1 executable and a separate,
verified copy of the data until the v2 rooms and agents work. Replace the
example root and port if your installation uses different values. The
[migration guide](migrating-from-v1.md) has the full first-run instructions;
this page is the operational order and rollback path.

## 1. Back up and stop the old processes

1. Record the v1 executable, config root, bind address, port, and how its
   server and agent wrappers were started. Plan an **independent copy** of the
   whole stopped v1 root, including `server/aimebu.sqlite` with any `-wal` and
   `-shm` files, `server/attachments/`, `server/sounds/`, `agents/`,
   `switcher/`, and `usages/`. Keep this copy outside the root that v2 will
   use. A plain file copy of a live SQLite database can miss committed WAL
   pages. Record any paths in `server/credential-targets.json`: those external
   live files are outside the root copy and need their own recovery plan.
2. If Auto-refresh tokens is enabled, check that the refresh journal has no
   in-flight entries immediately before stopping v1:

   ```sh
   jq '.journal|length' "$HOME/.aimebu/switcher/refresh-state.json"
   ```

   Proceed only when it prints `0`. A refresh stopped in its `started` phase
   is not spent again and may require a new login. Stop the old quota watchdog
   and any other server refreshing the same live credential files. The v2
   refresher refuses a second owner of a live credential file; `aimebu doctor`
   checks the usual two roots. Then stop v1's server and wrappers, make the
   independent copy, and verify its database and attachments. v2's own startup
   later moves the original v1-owned entries into `.old-v1-bkp/`, through a
   resumable `.old-v1-bkp.partial/` move with a manifest. It leaves
   `switcher/` and `usages/` in place. Keep both backups. See
   [token refresh checks](migrating-from-v1.md#0-check-token-refresh-before-stopping-v1),
   [`backupV1Layout`](../cmd/aimebu/migration_v1.go),
   [`runServer`](../cmd/aimebu/main.go),
   [`StartTokenCopies`](../internal/server/credtargets.go), and
   [switcher cutover](switcher.md).
3. Before upgrading an **existing v2** root, keep an independent full-root
   archive if you need attachments, credentials, and agent runtime state for
   recovery. Stop its server before copying those files. An optional manual
   online database snapshot can be made while the server runs:

   ```sh
   sqlite3 "$HOME/.aimebu/server/aimebu.sqlite" ".backup '$HOME/aimebu-before-upgrade.sqlite'"
   ```

   When the existing database has pending schema migrations, startup also
   makes its own standalone SQLite backup under `server/backups/`, verifies
   `integrity_check`, and normally keeps the new copy plus the four newest
   older copies. It stops before migrating if the backup fails. If removal of
   an older copy fails, startup
   logs a warning and continues with the verified new backup. A fresh database
   needs no schema backup. These automatic
   copies contain the database only, and `prune -a` preserves them. See
   [`backupBeforeMigration`](../internal/store/migration_backup.go),
   [`store.Open`](../internal/store/store.go), and
   [`migrateUpTo`](../internal/store/migrations.go).

## 2. Move the live root and import

1. Confirm every v1 server is stopped, including one on a custom port. Start
   the **v2** executable on its default root and port, which match v1. Set
   `AIMEBU2_CONFIG_DIR` and `AIMEBU2_PORT` only if v1 used custom values.
   v2 checks listeners, a recorded v1 daemon,
   and the v1 database write lock before moving files. An idle terminal-run v1
   on an unusual port can evade those checks; stop it yourself. Startup
   reserves the highest v1 message number before owner setup writes events.
   See [`checkV1CutoverPort`, `checkV1Daemon`, and `backupV1Layout`](../cmd/aimebu/migration_v1.go)
   and [`reserveV1History`](../cmd/aimebu/main.go).

   ```sh
   /path/to/v2/aimebu server
   ```

2. Open the printed setup link, create the v2 owner, and complete the
   **Projects → People → Rooms → Direct messages → Memory → Macros → Roles →
   Summary** wizard. Empty pages are omitted. Do not choose **Skip import
   forever** unless you intend to leave all v1 history in the backup; a
   completed or skipped import cannot be reopened. See
   [`ImportV1Wizard`](../web/js/import-v1.js) and
   [`importV1Status`](../internal/server/import_v1.go).
3. On **Projects**, confirm each workspace folder is an absolute path. A
   folder is suggested for a project whose v1 agents recorded a working
   folder, or whose name matches an existing folder beside the known ones.
   The others (4 of 20 on a real history, whose agents had long left v1's
   registry) are empty and must be filled in before the wizard goes on: any
   existing folder will do, since it can be changed in the project's settings
   until its first agent starts, or remove the project (its rooms are then
   not imported). The
   suggestion pairs `x-leader` with `x`: the leader room becomes the project
   root, and `x` becomes its `general` sub-room. A room `x` without a leader
   pair, including a solo room, is suggested into its own project's root.
   Check unrelated rooms and any merge warnings before continuing. On
   **People**, map each historical person to a v2 login, create an account, or
   keep only a historical label. The wizard maps the person with your name, or
   the one person in the data when there is only one (including people present
   only in agent records, read cursors, or room preferences), to the owner login, and starts everyone
   else on a new passwordless viewer account named after them. Check this
   page: **Skip person** says what it would drop for good (the person's memory
   profiles, reactions, read positions, and pinned or hidden rooms), and an
   import cannot be repeated, so what is dropped cannot be added back
   after the import. Review project readers: project members can
   read imported ordinary rooms. See [`PreviewV1Import`](../internal/store/import_v1_preview.go)
   and [`v1Targets.place`](../internal/store/import_v1_targets.go).
4. On **Rooms**, confirm destinations and open/archive choices. The v1
   `_system` room is excluded. On **Direct messages**, chats start **unticked**;
   turn on only the ones to bring across, checking the pre-filled project and
   participants. Imported chats always open, and the owner can read imported
   agent-to-agent history. On **Memory**, select project facts, global user
   profiles, and shared agent notes; on **Macros**, resolve text conflicts
   (v2 wins by default); on **Roles**, review compatible definitions and
   conflicts. Fleets have no v2 importer. See
   [`ImportV1Wizard`](../web/js/import-v1.js),
   [`PreviewV1Import`](../internal/store/import_v1_preview.go), and
   [`ImportV1`](../internal/store/import_v1.go).
5. Read the **Summary**, run **Check**, resolve access gaps and merge warnings,
   then choose **Import now**. Check runs the import in a rolled-back
   transaction and returns a digest that the commit must match. The report
   gives imported, already-present, and skipped counts, readers and access
   gaps, unresolved memory links, opened chats, merge warnings, and a
   **Not carried over** list. v1's own join/leave/registration bookkeeping
   lines are counted as skipped `presence_lines`; a person's identical words
   are still imported. The old files remain in `.old-v1-bkp/`. See
   [`importV1DryRun` and `runImportV1Job`](../internal/server/import_v1.go),
   [`isV1PresenceLine` and `ImportV1`](../internal/store/import_v1.go).

## 3. Bring agents back, leaders first

v1's tools are not carried over, so a fresh v2 has no harness and nothing can
be started. After the import the first load opens the **Harnesses** step when
no harness has a model yet; if it does not open (another browser, say), add one
in **Settings → Harnesses**. Add and auto-detect the harnesses, with their
models, before starting any agent.

Open a room with an imported agent message, then open the historical sender's
profile and choose **Start as v2 agent**. Start the leader in each active
project first; check that it can read the intended room and respond before
starting its workers. Then start workers from their historical profiles, one
room at a time. This is a deliberate new v2 agent with the old name, selected
tool/model, and its most common v1 role when that role exists in v2. It can
only be started from a room where it wrote, and only once. See
[`LegacyStart` and `StartInfo`](../internal/store/legacy_start.go),
[`legacyStartChecks`](../internal/server/legacy_start.go), and
   [`spawnAgentRequest`](../internal/server/agents.go).

Only Claude Code can continue a v1 harness session, and only through a
**native** launcher. It resumes in the v1 working folder, which must still
exist; an outside-project folder requires the ordinary boundary approval.
The host checks for a transcript before starting. Codex, Pi, OpenCode, Vibe, and
unsupported v1 harnesses start fresh under the same name because their v2
session homes or adapters cannot see the old session. A v1 session seen in
the last ten minutes needs explicit confirmation that v1 is stopped (a profile
of an agent that was active when the backup was made shows it as active, which
is normal for a rehearsal taken from a running v1).
Do not run the v1 wrapper and v2 agent on the same Claude session at once:
two writers to one transcript can corrupt it. See
[`ResumableTool`](../internal/store/legacy_start.go),
[`legacyResumeCWD` and `legacyStartChecks`](../internal/server/legacy_start.go),
and [`spawnAgentRequest`](../internal/server/agents.go).

## 4. Verify and retain the old state

Check several familiar rooms, replies, reactions, attachments, direct
messages, readers, memory, and macros. Quiet rooms may be under **Archived**.
Message numbers are v1's: a message that was `#N` in v1 is `#N` in v2, and the
first new message is the highest v1 number plus one. The messages that are not
imported are those of the `_system` room, v1's own join/leave lines, and the
rooms and direct messages you chose to leave out. Run `aimebu doctor` with the
v2 executable (it should report every check ok and the schema matching the
binary) and inspect the import report for skipped records. v2 also keeps its
own log, `server/aimebu.log` (rotated), and a `server/aimebu.pid` while the
server runs, in the v2 root beside the database. Keep v1 stopped after
cutover. Its `.old-v1-bkp/` is an
unchanged **source for inspection and rollback**, not a live read-only v1
service. There is no automatic expiry for this backup; retain it for as long
as your rollback window requires and remove it only after an independent
archive is verified. v2 writes new state beside that backup, and v2 activity
does not flow back into v1. See [`backupV1Layout`](../cmd/aimebu/migration_v1.go),
[`importV1Status`](../internal/server/import_v1.go), and
[migration checks](migrating-from-v1.md#4-check-the-result).

## 5. Roll back if v2 cannot continue

1. Stop the v2 server and agents. Preserve the current v2 root separately for
   diagnosis; do not point v1 at it.
2. Restore the **complete** `.old-v1-bkp/` into a **new** root. If v2 has not
   changed token-copy layout or refreshed credentials, link the switcher and
   usages directories that remained outside the backup as shown below, then
   run the saved v1 executable against the new root. If v2 changed either,
   copy `switcher/` and `usages/` from the independent stopped-v1 backup
   instead of creating the links. Check the live credential files before
   enabling v1 token refresh; a token already rotated by v2 may require a
   fresh login. Never run both refreshers on one tree. See
   [switcher cutover](switcher.md#returning-a-tool-to-the-link-model-un-migrate).

   ```sh
   restore="$HOME/aimebu-v1-restore-$(date +%Y%m%d-%H%M%S)"
   mkdir "$restore"
   cp -Rp "$HOME/.aimebu/.old-v1-bkp/." "$restore/"
   ln -s "$HOME/.aimebu/switcher" "$restore/switcher"
   ln -s "$HOME/.aimebu/usages" "$restore/usages"
   AIMEBU_CONFIG_DIR="$restore" /path/to/v1/aimebu server serve
   ```

   The copy also brings `manifest.json` into the restored root; v1 ignores it.
   Expect v1 to write its own `server started` and `server stopping` lines to
   its `_system` room.

3. Resume v1 wrappers only after that server is healthy. The restored state
   stops at the cutover; v2 messages and agent turns are not replayed into it.
   If you retry later, take a fresh stopped-v1 snapshot and use a **new** v2
   root. Do not reuse a rollback root whose v1 process has written to it: its
   backup manifest no longer matches. If only `.old-v1-bkp.partial/` exists,
   preserve it and inspect the failed move before restoring. See
   [`backupV1Layout`](../cmd/aimebu/migration_v1.go) and
   [rollback details](migrating-from-v1.md#roll-back-to-v1).

## 6. Rehearse on a copy

Start with a **stopped, independent copy** of v1's data, not the live root.
To rehearse while v1 is still running, take a consistent online copy of the
database and leave the credential and transcript folders out, since the
rehearsal needs neither (v2 accepts a copy that holds only `server/` and an
empty `agents/`):

```sh
V1_SNAPSHOT="$(mktemp -d)"
mkdir -p "$V1_SNAPSHOT/server" "$V1_SNAPSHOT/agents"
sqlite3 "$HOME/.aimebu/server/aimebu.sqlite" ".backup '$V1_SNAPSHOT/server/aimebu.sqlite'"
cp -Rp "$HOME/.aimebu/server/attachments" "$V1_SNAPSHOT/server/attachments"
```

That copy is only for the rehearsal; the real cutover still starts from the
stopped v1. Make the copy self-contained first: replace or remove symlinks to external
credential files and external credential targets. The commands below refuse
remaining symlinks and remove the copied external-target registry, so a
rehearsal cannot refresh a live credential through the copied root. Use a
temporary home and separate ports so it cannot touch live bus state. The
blocked proxy limits accidental external network calls. The copied Claude transcript tree is needed only if you want
to exercise native Claude session continuation; never let a rehearsal resume
a live session. See [`runServer`](../cmd/aimebu/main.go),
[`checkHarnessSession`](../internal/server/legacy_start.go), and
the [development probe rule](import-v1.md#mapping-choices).

```sh
V1_SNAPSHOT=/path/to/stopped-v1-copy
rehearsal_root="$(mktemp -d)"
mkdir -p "$rehearsal_root/home" "$rehearsal_root/v1" "$rehearsal_root/v2"
if [ -n "$(find "$V1_SNAPSHOT" -type l -print -quit)" ]; then
  echo 'Prepare a snapshot with no symlinks before rehearsing' >&2
  exit 1
fi
cp -Rp "$V1_SNAPSHOT/." "$rehearsal_root/v1/"
cp -Rp "$V1_SNAPSHOT/." "$rehearsal_root/v2/"
rm -f "$rehearsal_root/v1/server/credential-targets.json" "$rehearsal_root/v2/server/credential-targets.json"
export HOME="$rehearsal_root/home"
export CODEX_HOME="$rehearsal_root/home/.codex"
export CLAUDE_CONFIG_DIR="$rehearsal_root/home/.claude"
export AIMEBU_CONFIG_DIR="$rehearsal_root/v1"
export AIMEBU2_CONFIG_DIR="$rehearsal_root/v2"
export AIMEBU2_BIND=127.0.0.1
export AIMEBU2_PORT=9985 # any free port; this one may be taken
export HTTPS_PROXY=http://127.0.0.1:9
export HTTP_PROXY=http://127.0.0.1:9
export AIMEBU2_DEV_SKIP_V1_PROBE=1
/path/to/v2/aimebu server
```

Complete the wizard (fill any empty folder with a directory under the
rehearsal root, and check that you are mapped to the owner, as in step 2.3)
and verify rooms and reports on the rehearsal port. With a real history the first
sign-in after the import took about four seconds to load. The checks that
confirm a rehearsal: every imported v2 message number is a v1 id with the same
body, the counter starts at the highest v1 id, replies and reactions match the
report, an attachment downloads, and nothing in the process (`/proc/<pid>/fd`,
its environment, the log) names the real home or `~/.aimebu*`. Test
the rollback with the rehearsal `.old-v1-bkp/`, a third temporary root, and
the saved v1 executable. The development probe escape hatch above only skips
the v1 listener probe; the daemon PID check and SQLite write lock still run.
Unset it outside the rehearsal. Delete the temporary tree only after you no
longer need its report or rollback test.
