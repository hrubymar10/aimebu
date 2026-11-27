package doctorcheck

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/sqliteutil"
)

const (
	statusOK              = "ok"
	statusWarn            = "warn"
	statusFail            = "fail"
	foreignKeyReportLimit = 20
	machineStaleAfter     = 10 * time.Minute
)

// Checks reads the database through the connection owned by the caller.
func Checks(db *sql.DB, now time.Time, serverUp bool) []protocol.DoctorCheck {
	checks := []protocol.DoctorCheck{checkForeignKeys(db)}
	checks = append(checks, checkMachines(db, now, serverUp)...)
	checks = append(checks, checkPendingStops(db, now, serverUp)...)
	checks = append(checks, checkLaunchers(db)...)
	checks = append(checks, checkNameCollisions(db)...)
	checks = append(checks, checkProviderLimits(db, now)...)
	return checks
}

// checkForeignKeys runs PRAGMA foreign_key_check, which lists rows that point
// at a row that no longer exists.
func checkForeignKeys(db *sql.DB) protocol.DoctorCheck {
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
		return protocol.DoctorCheck{Name: name, Status: statusWarn, Description: fmt.Sprintf("foreign_key_check: %v", err)}
	}
	if len(violations) == 0 {
		return protocol.DoctorCheck{Name: name, Status: statusOK, Description: "no dangling references"}
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
	return protocol.DoctorCheck{Name: name, Status: statusFail, Description: fmt.Sprintf("%d dangling references: %s", len(violations), strings.Join(parts, ", ")),
		Hint: "Stop the server and restore the newest backup in server/backups, or ask for help before deleting rows by hand."}
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
func checkMachines(db *sql.DB, now time.Time, serverUp bool) []protocol.DoctorCheck {
	const name = "machines"
	if !tableExists(db, "machine") {
		return nil
	}
	machines, err := loadMachines(db)
	if err != nil {
		return []protocol.DoctorCheck{{Name: name, Status: statusWarn, Description: fmt.Sprintf("read machines: %v", err)}}
	}
	if len(machines) == 0 {
		return []protocol.DoctorCheck{{Name: name, Status: statusOK, Description: "none paired yet"}}
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
	var checks []protocol.DoctorCheck
	if len(unseen) > 0 {
		checks = append(checks, protocol.DoctorCheck{Name: name, Status: statusWarn, Description: summary + "; not seen recently: " + strings.Join(unseen, ", "),
			Hint: "Start the supervisor on that machine (aimebu supervisor), or revoke the machine in Settings → Machines if it is gone."})
	} else {
		desc := summary
		if !serverUp && active > 0 {
			desc += " (server not reachable, so liveness is not judged)"
		}
		checks = append(checks, protocol.DoctorCheck{Name: name, Status: statusOK, Description: desc})
	}
	if len(withAgents) > 0 {
		checks = append(checks, protocol.DoctorCheck{Name: "revoked machines", Status: statusWarn, Description: "revoked but still own live agents: " + strings.Join(withAgents, ", "),
			Hint: "Stop or remove those agents; a revoked machine can never receive their controls."})
	}
	return checks
}

// checkPendingStops reports stop controls no machine has applied yet, with exactly the condition project deletion applies
// (store.DeleteProject): an agent that is not revoked, on a machine that is not revoked, wanted stopped, whose stop the machine has not
// confirmed. Such a stop makes deleting its project return 409 until the machine applies it, so one held by a machine the server cannot
// reach is worth a warning; a connected machine with a few in flight is normal. A revoked machine's stops do not block deletion and are
// not listed.
func checkPendingStops(db *sql.DB, now time.Time, serverUp bool) []protocol.DoctorCheck {
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
		return []protocol.DoctorCheck{{Name: name, Status: statusWarn, Description: fmt.Sprintf("read pending stops: %v", err)}}
	}
	if len(held) == 0 {
		return []protocol.DoctorCheck{{Name: name, Status: statusOK, Description: "none waiting"}}
	}
	var checks []protocol.DoctorCheck
	var inFlight []string
	for _, item := range held {
		label := fmt.Sprintf("%d for %s", item.count, machineLabel(item.machine))
		if serverUp && (!item.machine.seen || now.Sub(item.machine.lastSeen) > machineStaleAfter) {
			checks = append(checks, protocol.DoctorCheck{Name: name, Status: statusWarn,
				Description: fmt.Sprintf("%d stop(s) waiting for machine %s, which is not connected: deleting a project with such an agent returns 409", item.count, machineLabel(item.machine)),
				Hint:        "Reconnect the machine (aimebu supervisor), or revoke it in Settings → Machines."})
			continue
		}
		inFlight = append(inFlight, label)
	}
	if len(inFlight) > 0 {
		checks = append(checks, protocol.DoctorCheck{Name: name, Status: statusOK, Description: "in flight: " + strings.Join(inFlight, ", ")})
	}
	return checks
}

// checkLaunchers inspects the harness launchers: an enabled one without models
// cannot spawn anything, and a model list without a default leaves new agents
// with no model to start on.
func checkLaunchers(db *sql.DB) []protocol.DoctorCheck {
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
		return []protocol.DoctorCheck{{Name: name, Status: statusWarn, Description: fmt.Sprintf("read launchers: %v", err)}}
	}
	if total == 0 {
		return []protocol.DoctorCheck{{Name: name, Status: statusOK, Description: "none set up yet"}}
	}
	var checks []protocol.DoctorCheck
	if len(noModels) > 0 {
		checks = append(checks, protocol.DoctorCheck{Name: name, Status: statusWarn, Description: "enabled with no models: " + strings.Join(noModels, ", "),
			Hint: "Add a model in Settings → Harnesses & models, or disable the launcher."})
	}
	if len(noDefault) > 0 {
		checks = append(checks, protocol.DoctorCheck{Name: name, Status: statusWarn, Description: "model list has no default: " + strings.Join(noDefault, ", "),
			Hint: "Mark one model as the default in Settings → Harnesses & models."})
	}
	if len(checks) == 0 {
		checks = append(checks, protocol.DoctorCheck{Name: name, Status: statusOK, Description: fmt.Sprintf("%d launcher(s), %d enabled, each with a model list and a default", total, enabled)})
	}
	return checks
}

// checkProviderLimits lists agents held by a provider usage limit that has not
// reset yet. A hold with no reset time is shown as open-ended.
func checkProviderLimits(db *sql.DB, now time.Time) []protocol.DoctorCheck {
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
		return []protocol.DoctorCheck{{Name: name, Status: statusWarn, Description: fmt.Sprintf("read provider limits: %v", err)}}
	}
	if len(held) == 0 {
		return []protocol.DoctorCheck{{Name: name, Status: statusOK, Description: "no agent is held by a provider limit"}}
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
	return []protocol.DoctorCheck{{Name: name, Status: statusWarn, Description: "held: " + strings.Join(parts, ", "),
		Hint: "Wait for the limit to reset, or switch the account in Settings → Credentials."}}
}

// checkNameCollisions lists each person login that equals (case-insensitive) the slug of a live agent in any project. New ones are refused,
// but ones made before that rule stay: a bare @name then addresses the agent and the people together. Doctor only reports them.
func checkNameCollisions(db *sql.DB) []protocol.DoctorCheck {
	const name = "login and agent names"
	if !tableExists(db, "user") || !tableExists(db, "agent") || !tableExists(db, "project") {
		return nil
	}
	var found []string
	err := sqliteutil.Retry(func() error {
		found = found[:0]
		rows, err := db.Query(`SELECT u.login,a.slug,COALESCE(p.name,'') FROM user u JOIN agent a ON lower(a.slug)=lower(u.login) AND a.revoked_at IS NULL
			LEFT JOIN project p ON p.id=a.project_id ORDER BY lower(u.login),u.login,p.name,a.slug`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var login, slug, project string
			if err := rows.Scan(&login, &slug, &project); err != nil {
				return err
			}
			found = append(found, fmt.Sprintf("the login %q and the agent %s@%s", login, slug, project))
		}
		return rows.Err()
	})
	if err != nil {
		return []protocol.DoctorCheck{{Name: name, Status: statusWarn, Description: fmt.Sprintf("cannot read logins and agents: %v", err)}}
	}
	if len(found) == 0 {
		return []protocol.DoctorCheck{{Name: name, Status: statusOK, Description: "no login equals an agent slug"}}
	}
	return []protocol.DoctorCheck{{Name: name, Status: statusWarn,
		Description: "a name is shared by a person and an agent, so a bare @name addresses both: " + strings.Join(found, "; "),
		Hint:        "Nothing is changed for you. Rename one side by removing the agent and spawning it again under another name, or give the person another login."}}
}
