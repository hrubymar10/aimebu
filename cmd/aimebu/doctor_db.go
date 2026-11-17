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

	"github.com/hrubymar10/aimebu/internal/sqliteutil"
)

// The offline server checks read the config root and the server database
// without a running server. Every query goes through doctor's read-only DSN, so
// they are safe beside a live server, and every check treats an empty
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
func doctorServerChecks(root string, now time.Time, serverUp, databaseUsable bool) []doctorCheck {
	var checks []doctorCheck
	checks = append(checks, checkDiskSpace(root))
	dbPath := filepath.Join(root, "server", "aimebu.sqlite")
	checks = append(checks, checkBackups(filepath.Join(root, "server", "backups"), now))
	if !databaseUsable {
		return scopeAll(checks, scopeServer)
	}
	db, err := sql.Open("sqlite", readonlyDSN(dbPath))
	if err != nil {
		return scopeAll(append(checks, doctorCheck{Name: "database queries", Status: statusWarn, Description: fmt.Sprintf("open %s: %v", dbPath, err)}), scopeServer)
	}
	defer db.Close()
	checks = append(checks, checkForeignKeys(db))
	checks = append(checks, checkMachines(db, now, serverUp)...)
	checks = append(checks, checkPendingStops(db, now, serverUp)...)
	checks = append(checks, checkLaunchers(db)...)
	checks = append(checks, checkProviderLimits(db, now)...)
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

// checkForeignKeys runs PRAGMA foreign_key_check, which lists rows that point
// at a row that no longer exists.
func checkForeignKeys(db *sql.DB) doctorCheck {
	const name = "foreign keys"
	var violations []string
	err := sqliteutil.Retry(func() error {
		violations = violations[:0]
		rows, err := db.Query("PRAGMA foreign_key_check")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var table, parent string
			var rowid sql.NullInt64
			var fkid int
			if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
				return err
			}
			violations = append(violations, table+"→"+parent)
		}
		return rows.Err()
	})
	if err != nil {
		return doctorCheck{Name: name, Status: statusWarn, Description: fmt.Sprintf("foreign_key_check: %v", err)}
	}
	if len(violations) == 0 {
		return doctorCheck{Name: name, Status: statusOK, Description: "no dangling references"}
	}
	counts := map[string]int{}
	for _, violation := range violations {
		counts[violation]++
	}
	var parts []string
	for relation, count := range counts {
		parts = append(parts, fmt.Sprintf("%s ×%d", relation, count))
	}
	sort.Strings(parts)
	if len(parts) > foreignKeyReportLimit {
		parts = append(parts[:foreignKeyReportLimit], "…")
	}
	return doctorCheck{Name: name, Status: statusFail, Description: fmt.Sprintf("%d dangling references: %s", len(violations), strings.Join(parts, ", ")),
		Hint: "Stop the server and restore the newest backup in server/backups, or ask for help before deleting rows by hand."}
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

// tableExists reports whether the database has the table; an older schema
// than this binary simply lacks the newer ones.
func tableExists(db *sql.DB, name string) bool {
	var count int
	err := sqliteutil.Retry(func() error {
		return db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&count)
	})
	return err == nil && count > 0
}

// parseStoredTime reads a store timestamp (RFC 3339 with nanoseconds).
func parseStoredTime(value sql.NullString) (time.Time, bool) {
	if !value.Valid || value.String == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value.String)
	return parsed, err == nil
}

type doctorMachine struct {
	id, name string
	approved bool
	revoked  bool
	isHost   bool
	lastSeen time.Time
	seen     bool
}

func loadMachines(db *sql.DB) ([]doctorMachine, error) {
	var machines []doctorMachine
	err := sqliteutil.Retry(func() error {
		machines = machines[:0]
		rows, err := db.Query(`SELECT id,name,approved,revoked_at IS NOT NULL,is_host,last_seen FROM machine ORDER BY name,id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var machine doctorMachine
			var seen sql.NullString
			if err := rows.Scan(&machine.id, &machine.name, &machine.approved, &machine.revoked, &machine.isHost, &seen); err != nil {
				return err
			}
			machine.lastSeen, machine.seen = parseStoredTime(seen)
			machines = append(machines, machine)
		}
		return rows.Err()
	})
	return machines, err
}

func machineLabel(machine doctorMachine) string {
	if machine.name == "" {
		return machine.id
	}
	return fmt.Sprintf("%s (%s)", machine.name, machine.id)
}

// checkMachines reports paired machines: approved ones the server has not heard
// from while it is up, and revoked ones that still own live agents. A new
// install has no machines, which is ok.
func checkMachines(db *sql.DB, now time.Time, serverUp bool) []doctorCheck {
	const name = "machines"
	if !tableExists(db, "machine") {
		return nil
	}
	machines, err := loadMachines(db)
	if err != nil {
		return []doctorCheck{{Name: name, Status: statusWarn, Description: fmt.Sprintf("read machines: %v", err)}}
	}
	if len(machines) == 0 {
		return []doctorCheck{{Name: name, Status: statusOK, Description: "none paired yet"}}
	}
	var active, pending, revoked int
	var unseen, withAgents []string
	liveAgents := map[string]int{}
	if tableExists(db, "agent") {
		_ = sqliteutil.Retry(func() error {
			rows, err := db.Query(`SELECT machine_id,COUNT(*) FROM agent WHERE revoked_at IS NULL AND desired<>'stopped' GROUP BY machine_id`)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var id string
				var count int
				if err := rows.Scan(&id, &count); err != nil {
					return err
				}
				liveAgents[id] = count
			}
			return rows.Err()
		})
	}
	for _, machine := range machines {
		switch {
		case machine.revoked:
			revoked++
			if count := liveAgents[machine.id]; count > 0 {
				withAgents = append(withAgents, fmt.Sprintf("%s (%d agent(s))", machineLabel(machine), count))
			}
		case !machine.approved:
			pending++
		default:
			active++
			if serverUp && (!machine.seen || now.Sub(machine.lastSeen) > machineStaleAfter) {
				if !machine.seen {
					unseen = append(unseen, machineLabel(machine)+" never connected")
				} else {
					unseen = append(unseen, fmt.Sprintf("%s unseen for %s", machineLabel(machine), now.Sub(machine.lastSeen).Round(time.Minute)))
				}
			}
		}
	}
	summary := fmt.Sprintf("%d approved, %d awaiting approval, %d revoked", active, pending, revoked)
	var checks []doctorCheck
	if len(unseen) > 0 {
		checks = append(checks, doctorCheck{Name: name, Status: statusWarn, Description: summary + "; not seen recently: " + strings.Join(unseen, ", "),
			Hint: "Start the supervisor on that machine (aimebu supervisor), or revoke the machine in Settings → Machines if it is gone."})
	} else {
		desc := summary
		if !serverUp && active > 0 {
			desc += " (server not reachable, so liveness is not judged)"
		}
		checks = append(checks, doctorCheck{Name: name, Status: statusOK, Description: desc})
	}
	if len(withAgents) > 0 {
		checks = append(checks, doctorCheck{Name: "revoked machines", Status: statusWarn, Description: "revoked but still own live agents: " + strings.Join(withAgents, ", "),
			Hint: "Stop or remove those agents; a revoked machine can never receive their controls."})
	}
	return checks
}

// checkPendingStops reports stop controls no machine has applied yet, with exactly the condition project deletion applies
// (store.DeleteProject): an agent that is not revoked, on a machine that is not revoked, wanted stopped, whose stop the machine has not
// confirmed. Such a stop makes deleting its project return 409 until the machine applies it, so one held by a machine the server cannot
// reach is worth a warning; a connected machine with a few in flight is normal. A revoked machine's stops do not block deletion and are
// not listed.
func checkPendingStops(db *sql.DB, now time.Time, serverUp bool) []doctorCheck {
	const name = "pending stops"
	if !tableExists(db, "agent") || !tableExists(db, "machine") || !tableExists(db, "event") {
		return nil
	}
	type pending struct {
		machine doctorMachine
		count   int
	}
	var held []pending
	err := sqliteutil.Retry(func() error {
		held = held[:0]
		rows, err := db.Query(`SELECT m.id,m.name,m.last_seen,COUNT(*)
			FROM agent a JOIN machine m ON m.id=a.machine_id
			WHERE a.revoked_at IS NULL AND m.revoked_at IS NULL AND a.desired='stopped'
			AND NOT EXISTS (SELECT 1 FROM event e WHERE e.agent_id=a.id AND e.kind='agent_control_applied'
				AND CAST(json_extract(e.payload_json,'$.desired_rev') AS INTEGER)=a.desired_rev)
			GROUP BY m.id ORDER BY m.name,m.id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item pending
			var seen sql.NullString
			if err := rows.Scan(&item.machine.id, &item.machine.name, &seen, &item.count); err != nil {
				return err
			}
			item.machine.lastSeen, item.machine.seen = parseStoredTime(seen)
			held = append(held, item)
		}
		return rows.Err()
	})
	if err != nil {
		return []doctorCheck{{Name: name, Status: statusWarn, Description: fmt.Sprintf("read pending stops: %v", err)}}
	}
	if len(held) == 0 {
		return []doctorCheck{{Name: name, Status: statusOK, Description: "none waiting"}}
	}
	var checks []doctorCheck
	var inFlight []string
	for _, item := range held {
		label := fmt.Sprintf("%d for %s", item.count, machineLabel(item.machine))
		if serverUp && (!item.machine.seen || now.Sub(item.machine.lastSeen) > machineStaleAfter) {
			checks = append(checks, doctorCheck{Name: name, Status: statusWarn,
				Description: fmt.Sprintf("%d stop(s) waiting for machine %s, which is not connected: deleting a project with such an agent returns 409", item.count, machineLabel(item.machine)),
				Hint:        "Reconnect the machine (aimebu supervisor), or revoke it in Settings → Machines."})
			continue
		}
		inFlight = append(inFlight, label)
	}
	if len(inFlight) > 0 {
		checks = append(checks, doctorCheck{Name: name, Status: statusOK, Description: "in flight: " + strings.Join(inFlight, ", ")})
	}
	return checks
}

// checkLaunchers inspects the harness launchers: an enabled one without models
// cannot spawn anything, and a model list without a default leaves new agents
// with no model to start on.
func checkLaunchers(db *sql.DB) []doctorCheck {
	const name = "launchers"
	if !tableExists(db, "machine_launcher") || !tableExists(db, "harness_model") {
		return nil
	}
	var total, enabled int
	var noModels, noDefault []string
	err := sqliteutil.Retry(func() error {
		total, enabled = 0, 0
		noModels, noDefault = noModels[:0], noDefault[:0]
		rows, err := db.Query(`SELECT l.id,l.tool,l.name,l.label,l.enabled,
			(SELECT COUNT(*) FROM harness_model WHERE launcher_id=l.id),
			(SELECT COUNT(*) FROM harness_model WHERE launcher_id=l.id AND is_default=1)
			FROM machine_launcher l ORDER BY l.machine_id,l.tool,l.name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, tool, launcherName, label string
			var isEnabled bool
			var models, defaults int
			if err := rows.Scan(&id, &tool, &launcherName, &label, &isEnabled, &models, &defaults); err != nil {
				return err
			}
			total++
			if !isEnabled {
				continue
			}
			enabled++
			display := label
			if display == "" {
				display = tool + "/" + launcherName
			}
			if models == 0 {
				noModels = append(noModels, display)
			} else if defaults == 0 {
				noDefault = append(noDefault, display)
			}
		}
		return rows.Err()
	})
	if err != nil {
		return []doctorCheck{{Name: name, Status: statusWarn, Description: fmt.Sprintf("read launchers: %v", err)}}
	}
	if total == 0 {
		return []doctorCheck{{Name: name, Status: statusOK, Description: "none set up yet"}}
	}
	var checks []doctorCheck
	if len(noModels) > 0 {
		checks = append(checks, doctorCheck{Name: name, Status: statusWarn, Description: "enabled with no models: " + strings.Join(noModels, ", "),
			Hint: "Add a model in Settings → Harnesses, or disable the launcher."})
	}
	if len(noDefault) > 0 {
		checks = append(checks, doctorCheck{Name: name, Status: statusWarn, Description: "model list has no default: " + strings.Join(noDefault, ", "),
			Hint: "Mark one model as the default in Settings → Harnesses."})
	}
	if len(checks) == 0 {
		checks = append(checks, doctorCheck{Name: name, Status: statusOK, Description: fmt.Sprintf("%d launcher(s), %d enabled, each with a model list and a default", total, enabled)})
	}
	return checks
}

// checkProviderLimits lists agents held by a provider usage limit that has not
// reset yet. A hold with no reset time is shown as open-ended.
func checkProviderLimits(db *sql.DB, now time.Time) []doctorCheck {
	const name = "provider limits"
	if !tableExists(db, "provider_limit") {
		return nil
	}
	held := map[string]int{}
	var latestReset = map[string]time.Time{}
	open := map[string]bool{}
	err := sqliteutil.Retry(func() error {
		clear(held)
		clear(latestReset)
		clear(open)
		rows, err := db.Query(`SELECT tool,reset_at FROM provider_limit`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var tool string
			var reset sql.NullString
			if err := rows.Scan(&tool, &reset); err != nil {
				return err
			}
			resetAt, ok := parseStoredTime(reset)
			if ok && !resetAt.After(now) {
				continue // the limit already reset
			}
			held[tool]++
			if !ok {
				open[tool] = true
			} else if resetAt.After(latestReset[tool]) {
				latestReset[tool] = resetAt
			}
		}
		return rows.Err()
	})
	if err != nil {
		return []doctorCheck{{Name: name, Status: statusWarn, Description: fmt.Sprintf("read provider limits: %v", err)}}
	}
	if len(held) == 0 {
		return []doctorCheck{{Name: name, Status: statusOK, Description: "no agent is held by a provider limit"}}
	}
	var tools []string
	for tool := range held {
		tools = append(tools, tool)
	}
	sort.Strings(tools)
	var parts []string
	for _, tool := range tools {
		part := fmt.Sprintf("%d %s agent(s)", held[tool], tool)
		if open[tool] {
			part += " (no reset time known)"
		} else {
			part += " until " + latestReset[tool].UTC().Format("2006-01-02 15:04 UTC")
		}
		parts = append(parts, part)
	}
	return []doctorCheck{{Name: name, Status: statusWarn, Description: "held: " + strings.Join(parts, ", "),
		Hint: "Wait for the limit to reset, or switch the account in Settings → Credentials."}}
}
