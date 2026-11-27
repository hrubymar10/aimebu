# Backup and restore

Run `aimebu backup` before upgrading. It writes a private (`0600`) gzip tar
archive to `<data root>/backups/aimebu-<version>-<UTC timestamp>.tar.gz`.
`aimebu backup --out <file>` chooses another path. The data root is
`~/.aimebu` by default or `AIMEBU_CONFIG_DIR` when set.

`aimebu backup --keep N` (N of 1 or more; `0` and negative values are refused)
deletes old backups after a **successful** backup: every regular
`aimebu-*.tar.gz` file in the default folder (`<data root>/backups`) except
the N newest, counting the archive just written. Nothing is deleted without
the flag, after any failure, or for files with another name, directories and
symlinks. It does not apply when `--out` is outside the default folder (the
CLI says so). "Newest" is the UTC timestamp in the file name, which backup
always writes; a file whose name carries none is ordered by modification
time. The command prints each deleted file.

The archive contains the server database, config, keys and the attachments
named by active rows in the database snapshot. Files in the attachments folder with
no active row in that snapshot are excluded. Attachments already removed by
retention are excluded. If an active row's file has disappeared, the
manifest lists its ID under `missing_attachments`; backup prints a warning and
restore reports the same list. The `manifest.json` also records the source
data root, binary version, time, schema version and a SHA-256 checksum for
each archived file. **Treat it as a secret:** the keys inside can grant
access to this installation. Harness homes such as `~/.claude` and `~/.codex`
are outside the data root and are not included. Neither are backups, logs,
temporary files, or runtime lock and PID files.

The v1 rollback copy `.old-v1-bkp/` that the v1 cutover leaves in the data
root is **excluded by default**: it is a full second copy of the v1 data and
made every backup much larger. `aimebu backup --include-v1-backup` archives it.
While that folder exists, every backup run prints one line saying whether it
was excluded or included and how to change that. Restore works with archives
that contain it and with archives that do not; it restores the folder only when
the archive has it.

Backups of databases created before attachment retention include every
attachment row, because those databases have no removal marker.

When the server is running, it makes a SQLite snapshot through its own
connection. The CLI requests it over the same-host, doctor-key-authenticated
HTTP channel used by `aimebu supervisor restart`. The CLI never opens the live
database. When the server is stopped, the CLI holds the server writer lock
while it makes the snapshot. A running supervisor owns its journals; backup
skips those journals and prints a warning. Stop both processes first if their
journals matter to the backup. A stopped journal is copied with its WAL under
its lock.
The doctor key is a mode-0600 file in the data root, so anyone who can read it
already has file-system access to the database; this route grants no new data
access. Snapshot temporary files stay inside the data root and are removed
after use, including when a client disconnects.

Restore with `aimebu restore <file>`. The command checks every manifest entry
and SHA-256 checksum, and refuses archives from a newer database schema.
An older schema is restored and migrates when the server next starts. Before
moving the current data root, restore rewrites attachment paths in the
extracted database from the manifest's source root to the target data root.
It refuses an attachment path outside the source root. For older archives
without `source_root`, restore uses the target root if every attachment path
already points there or the snapshot has no attachment rows. Otherwise it
infers the source root from the common `server/attachments/` path prefix and
refuses archives whose attachment paths have no common source root.
An attachment counts as already at the target only when its path is inside
the target's own `server/attachments/` folder.

Restore requires the server and supervisor to be stopped; use `aimebu server stop
--with-supervisor` for a manually started installation or `brew services stop
aimebu` for a Homebrew service. Restore asks for confirmation on a terminal;
`--yes` allows unattended use. It moves the existing data root to
`<data root>.pre-restore-<UTC timestamp>` and prints that path. It never
deletes the original.

Only regular archive files with safe relative paths are accepted. Symlinks,
duplicate paths, mismatched checksums and missing manifest entries are
refused before the current data root is moved.
