package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/doctorcheck"
	"github.com/hrubymar10/aimebu/v2/internal/sqliteutil"
)

// The offline server checks read the config root and the server database
// without a running server, under the server's startup lock. Every check treats an empty
// database, no machines and no backups as normal: those answer ok or warn,
// never fail.

const (
	scopeServer  = "server"
	scopeLive    = "live"
	scopeMachine = "machine"

	diskWarnBytes = 2 << 30   // below this the config root's disk warns
	diskFailBytes = 200 << 20 // below this it fails

	// machineStaleAfter is how long an approved machine may stay unseen while
	// the server is up before doctor says so. A connected supervisor pings every
	// 15 seconds, so this is generous.
	machineStaleAfter = 10 * time.Minute

	// backupQuickCheckTimeout and backupQuickCheckMaxBytes bound the quick_check
	// of the newest backup: a very large file is reported unverified instead of
	// stalling doctor.
	backupQuickCheckTimeout = 10 * time.Second

	// foreignKeyReportLimit caps how many dangling relations the description lists.
	foreignKeyReportLimit = 20
)

// backupQuickCheckMaxBytes is a variable so tests can lower it.
var backupQuickCheckMaxBytes uint64 = 512 << 20

// diskFreeBytes returns the bytes available to an unprivileged user on the
// filesystem holding path. A variable so tests can pin low-disk answers.
var diskFreeBytes = func(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}

// doctorServerChecks runs the offline checks for the config root. serverUp says
// whether the live probe reached the server (machine liveness is only judged
// then); databaseUsable says whether the database opened and passed its
// integrity check, so the queries below can be trusted to run.
func doctorServerChecks(root string, now time.Time, serverUp bool, db *sql.DB) []doctorCheck {
	var checks []doctorCheck
	checks = append(checks, checkDiskSpace(root))
	checks = append(checks, checkBackups(filepath.Join(root, "server", "backups"), now))
	if db == nil {
		return scopeAll(checks, scopeServer)
	}
	for _, check := range doctorcheck.Checks(db, now, serverUp) {
		checks = append(checks, doctorCheck(check))
	}
	return scopeAll(checks, scopeServer)
}

// scopeAll fills Scope on checks that did not set one.
func scopeAll(checks []doctorCheck, scope string) []doctorCheck {
	for i := range checks {
		if checks[i].Scope == "" {
			checks[i].Scope = scope
		}
	}
	return checks
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for value := n / unit; value >= unit; value /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// fileSize returns a file's size, or 0 when it does not exist.
func fileSize(path string) uint64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return uint64(info.Size())
}

// checkDiskSpace reports the free space on the filesystem holding the config
// root, next to the database and WAL sizes. SQLite fails writes on a full disk,
// so low space is a failure well before the server reports it.
func checkDiskSpace(root string) doctorCheck {
	const name = "disk space"
	free, err := diskFreeBytes(root)
	if err != nil {
		return doctorCheck{Name: name, Status: statusWarn, Description: fmt.Sprintf("cannot read free space of %s: %v", root, err)}
	}
	dbPath := filepath.Join(root, "server", "aimebu.sqlite")
	detail := fmt.Sprintf("%s free; database %s, wal %s", humanBytes(free), humanBytes(fileSize(dbPath)), humanBytes(fileSize(dbPath+"-wal")))
	hint := "Free disk space on the volume holding the config root, or move AIMEBU_CONFIG_DIR to a larger one."
	switch {
	case free < diskFailBytes:
		return doctorCheck{Name: name, Status: statusFail, Description: detail + " (under 200 MiB: SQLite writes will start failing)", Hint: hint}
	case free < diskWarnBytes:
		return doctorCheck{Name: name, Status: statusWarn, Description: detail + " (under 2 GiB)", Hint: hint}
	}
	return doctorCheck{Name: name, Status: statusOK, Description: detail}
}

var doctorBackupName = regexp.MustCompile(`^aimebu-pre-migrate-([0-9]+)-to-([0-9]+)-([0-9]{8}T[0-9]{6}\.[0-9]{9}Z)\.sqlite$`)

// checkBackups looks at server/backups, where the server writes a verified copy
// before every schema migration. No directory is normal (nothing has migrated
// yet). The newest backup gets a read-only quick_check, bounded by size and
// time; leftover .partial files from an interrupted backup warn.
func checkBackups(dir string, now time.Time) doctorCheck {
	const name = "backups"
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return doctorCheck{Name: name, Status: statusOK, Description: "none yet (the server writes one before each schema migration)"}
	}
	if err != nil {
		return doctorCheck{Name: name, Status: statusWarn, Description: fmt.Sprintf("read %s: %v", dir, err)}
	}
	type backup struct {
		name  string
		stamp time.Time
	}
	var backups []backup
	var partials []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".partial") {
			partials = append(partials, entry.Name())
			continue
		}
		match := doctorBackupName.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		stamp, err := time.Parse("20060102T150405.000000000Z", match[3])
		if err != nil {
			continue
		}
		backups = append(backups, backup{entry.Name(), stamp})
	}
	var notes []string
	status := statusOK
	hint := ""
	if info, err := os.Stat(dir); err == nil && info.Mode().Perm()&0o077 != 0 {
		status = statusWarn
		notes = append(notes, fmt.Sprintf("directory mode %o is readable by others", info.Mode().Perm()))
		hint = "chmod 700 " + dir
	}
	if len(partials) > 0 {
		status = statusWarn
		notes = append(notes, fmt.Sprintf("%d interrupted .partial file(s) left behind", len(partials)))
		if hint == "" {
			hint = "Remove the .partial files from " + dir + " once the server is stopped; they are incomplete copies."
		}
	}
	if len(backups) == 0 {
		desc := "none yet (the server writes one before each schema migration)"
		if len(notes) > 0 {
			desc = "no complete backup; " + strings.Join(notes, "; ")
		}
		return doctorCheck{Name: name, Status: status, Description: desc, Hint: hint}
	}
	sort.Slice(backups, func(i, j int) bool { return backups[i].stamp.After(backups[j].stamp) })
	newest := backups[0]
	desc := fmt.Sprintf("%d backup(s); newest %s (%s ago)", len(backups), newest.name, now.Sub(newest.stamp).Round(time.Minute))
	quick, quickStatus := quickCheckBackup(filepath.Join(dir, newest.name))
	if quickStatus != statusOK {
		status = quickStatus
		if hint == "" {
			hint = "The newest backup did not verify; do not rely on it. The next migration writes a fresh one."
		}
	}
	notes = append([]string{quick}, notes...)
	return doctorCheck{Name: name, Status: status, Description: desc + "; " + strings.Join(notes, "; "), Hint: hint}
}

// quickCheckBackup opens one backup read-only and runs PRAGMA quick_check. A
// file over the size cap, or a check that outlives its timeout, is reported as
// unverified (warn) rather than failed.
func quickCheckBackup(path string) (string, string) {
	size := fileSize(path)
	if size > backupQuickCheckMaxBytes {
		return fmt.Sprintf("not verified (%s is over the %s limit)", humanBytes(size), humanBytes(backupQuickCheckMaxBytes)), statusWarn
	}
	db, err := sql.Open("sqlite", sqliteutil.DSN(path, true))
	if err != nil {
		return fmt.Sprintf("open failed: %v", err), statusFail
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), backupQuickCheckTimeout)
	defer cancel()
	var result string
	err = sqliteutil.Retry(func() error { return db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result) })
	switch {
	case errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil:
		return fmt.Sprintf("not verified (quick_check took longer than %s)", backupQuickCheckTimeout), statusWarn
	case err != nil:
		return fmt.Sprintf("quick_check failed: %v", err), statusFail
	case result != "ok":
		return "quick_check: " + result, statusFail
	}
	return "quick_check ok", statusOK
}
