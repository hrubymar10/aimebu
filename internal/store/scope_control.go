package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"
)

// ScopeControl is the persisted bulk control and its dispatch gate revision.
type ScopeControl struct {
	Desired        string             `json:"desired"`
	EffectiveState string             `json:"effective_state"`
	Rev            int64              `json:"rev"`
	Results        []ScopeAgentResult `json:"results,omitempty"`
	Failures       []ScopeFailure     `json:"failures,omitempty"`
}

var ErrInvalidScope = errors.New("invalid_scope")

type scopeLedger struct {
	kind, id, restore, hold, bulkDesired string
	bulkRev                              int64
}

func (tx *Tx) scopeLedgers(agentID string) ([]scopeLedger, error) {
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT scope_kind,scope_id,restore_desired,hold_desired,bulk_desired,bulk_rev FROM scope_control_agent WHERE agent_id=? ORDER BY bulk_rev,scope_kind,scope_id`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []scopeLedger{}
	for rows.Next() {
		var item scopeLedger
		if err := rows.Scan(&item.kind, &item.id, &item.restore, &item.hold, &item.bulkDesired, &item.bulkRev); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func scopeEffectiveDesired(base string, ledgers []scopeLedger) string {
	if base == "stopped" {
		return "stopped"
	}
	paused := base == "not_accepting"
	for _, item := range ledgers {
		if item.hold == "stopped" {
			return "stopped"
		}
		if item.hold == "not_accepting" {
			paused = true
		}
	}
	if paused {
		return "not_accepting"
	}
	return "active"
}

func (tx *Tx) syncScopeLedgers(agentID, base, desired string, rev int64) error {
	_, err := tx.Exec(`UPDATE scope_control_agent SET restore_desired=?,bulk_desired=?,bulk_rev=? WHERE agent_id=?`, base, desired, rev, agentID)
	return err
}

func (tx *Tx) recordScopeFailure(kind, id, agentID, action, message string) error {
	_, err := tx.Exec(`INSERT INTO scope_control_failure(scope_kind,scope_id,agent_id,action,error,attempted_at) VALUES (?,?,?,?,?,?)
		ON CONFLICT(scope_kind,scope_id,agent_id) DO UPDATE SET action=excluded.action,error=excluded.error,attempted_at=excluded.attempted_at`, kind, id, agentID, action, message, timestamp())
	return err
}

func (tx *Tx) clearScopeFailure(kind, id, agentID string) error {
	_, err := tx.Exec(`DELETE FROM scope_control_failure WHERE scope_kind=? AND scope_id=? AND agent_id=?`, kind, id, agentID)
	return err
}

func (tx *Tx) scopeFailures(kind, id string) ([]ScopeFailure, error) {
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT agent_id,action,error FROM scope_control_failure WHERE scope_kind=? AND scope_id=? ORDER BY agent_id`, kind, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ScopeFailure{}
	for rows.Next() {
		var item ScopeFailure
		if err := rows.Scan(&item.AgentID, &item.Action, &item.Error); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

type ScopeFailure struct {
	AgentID string `json:"agent_id"`
	Action  string `json:"action"`
	Error   string `json:"error"`
}

type ScopeAgentResult struct {
	Agent *AgentCard `json:"agent,omitempty"`
	Error string     `json:"error,omitempty"`
}

func (tx *Tx) scopeAgentTransition(id, actor, desired string, rev int64, restorePaused bool) (AgentCard, error) {
	if _, err := tx.Exec(`SAVEPOINT scope_agent_transition`); err != nil {
		return AgentCard{}, err
	}
	var updated AgentCard
	var err error
	if restorePaused {
		updated, err = tx.SetAgentDesired(id, actor, "active", rev)
		if err == nil {
			updated, err = tx.SetAgentDesired(id, actor, "not_accepting", updated.DesiredRev)
		}
	} else {
		updated, err = tx.SetAgentDesired(id, actor, desired, rev)
	}
	if err != nil {
		_, _ = tx.Exec(`ROLLBACK TO scope_agent_transition`)
		_, _ = tx.Exec(`RELEASE scope_agent_transition`)
		return AgentCard{}, err
	}
	_, err = tx.Exec(`RELEASE scope_agent_transition`)
	return updated, err
}

func scopeTable(kind string) (string, error) {
	switch kind {
	case "room", "project":
		return kind, nil
	default:
		return "", ErrInvalidTransition
	}
}

func (s *Store) ScopeControl(ctx context.Context, kind, id string) (ScopeControl, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return ScopeControl{}, err
	}
	table, err := scopeTable(kind)
	if err != nil {
		return ScopeControl{}, err
	}
	var out ScopeControl
	var gate string
	err = s.db.QueryRowContext(ctx, "SELECT control_desired,gate,gate_rev FROM "+table+" WHERE id=?", id).
		Scan(&out.Desired, &gate, &out.Rev)
	if err != nil {
		return ScopeControl{}, err
	}
	out.EffectiveState = out.Desired
	if gate != "open" && out.Desired == "active" {
		out.EffectiveState = "paused"
	}
	rows, err := s.db.QueryContext(ctx, `SELECT agent_id,action,error FROM scope_control_failure WHERE scope_kind=? AND scope_id=? ORDER BY agent_id`, kind, id)
	if err != nil {
		return ScopeControl{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var item ScopeFailure
		if err := rows.Scan(&item.AgentID, &item.Action, &item.Error); err != nil {
			return ScopeControl{}, err
		}
		out.Failures = append(out.Failures, item)
	}
	if err := rows.Err(); err != nil {
		return ScopeControl{}, err
	}
	return out, nil
}

// ScopeResumeLauncherStatus checks the saved launcher configuration without
// depending on a live supervisor connection. Only agents held by this scope
// and still at the revision it applied are candidates for resume.
func (s *Store) ScopeResumeLauncherStatus(ctx context.Context, kind, id string) (disabled, needsBind bool, err error) {
	if err = s.checkTxReentry(ctx); err != nil {
		return
	}
	if _, err = scopeTable(kind); err != nil {
		return
	}
	rows, err := s.db.QueryContext(ctx, `SELECT a.tool,a.variant,a.launcher_id,l.enabled,l.containerized,l.agent_server_url
		FROM scope_control_agent sc JOIN agent a ON a.id=sc.agent_id
		LEFT JOIN machine_launcher l ON l.id=a.launcher_id
		WHERE sc.scope_kind=? AND sc.scope_id=? AND a.desired=sc.bulk_desired AND a.desired_rev=sc.bulk_rev`, kind, id)
	if err != nil {
		return false, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var tool, variant string
		var launcherID, serverURL sql.NullString
		var enabled, containerized sql.NullBool
		if err = rows.Scan(&tool, &variant, &launcherID, &enabled, &containerized, &serverURL); err != nil {
			return false, false, err
		}
		if launcherID.Valid {
			disabled = disabled || !enabled.Valid || !enabled.Bool
			needsBind = needsBind || containerized.Bool && serverURL.String == ""
		} else {
			needsBind = needsBind || tool != "fake" && variant == "docker"
		}
	}
	err = rows.Err()
	return
}

// ControlScope applies ordinary agent transitions in one transaction. Each
// ledger row holds one scope's intent; overlapping holds share the original
// desired state and the last bulk-applied revision. A later individual
// decision invalidates the holds before another bulk operation runs.
func (tx *Tx) ControlScope(kind, id, action, actor string, rev int64) (ScopeControl, error) {
	table, err := scopeTable(kind)
	if err != nil {
		return ScopeControl{}, err
	}
	if action != "pause" && action != "stop" && action != "resume" {
		return ScopeControl{}, ErrInvalidTransition
	}
	var projectID, current, gate, roomID, lifecycle, roomKind string
	var currentRev int64
	if kind == "project" {
		err = tx.tx.QueryRowContext(tx.ctx, `SELECT id,control_desired,gate,gate_rev FROM project WHERE id=?`, id).
			Scan(&projectID, &current, &gate, &currentRev)
		if err == nil {
			err = tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM room WHERE project_id=? AND kind='home' LIMIT 1`, id).Scan(&roomID)
		}
	} else {
		roomID = id
		err = tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,control_desired,gate,gate_rev,lifecycle,kind FROM room WHERE id=?`, id).
			Scan(&projectID, &current, &gate, &currentRev, &lifecycle, &roomKind)
	}
	if err != nil {
		return ScopeControl{}, err
	}
	if kind == "room" && roomKind == "dm" {
		return ScopeControl{}, ErrInvalidScope
	}
	if err := tx.roomLifecyclePermission(projectID, actor); err != nil {
		return ScopeControl{}, err
	}
	if rev != currentRev {
		return ScopeControl{}, ErrStaleRev
	}
	if kind == "room" && lifecycle != "open" && action == "resume" {
		return ScopeControl{}, ErrInvalidTransition
	}
	if action == "resume" {
		paused, err := tx.budgetPaused(kind, id, time.Now())
		if err != nil {
			return ScopeControl{}, err
		}
		if paused {
			return ScopeControl{}, ErrInvalidTransition
		}
	}
	target := map[string]string{"pause": "paused", "stop": "stopped", "resume": "active"}[action]
	out := ScopeControl{Desired: current, EffectiveState: current, Rev: currentRev, Results: []ScopeAgentResult{}}
	if current == "stopped" && action == "pause" {
		return ScopeControl{}, ErrInvalidTransition
	}
	changed := current != target
	var rows *sql.Rows
	if kind == "room" {
		rows, err = tx.tx.QueryContext(tx.ctx, `SELECT a.id,a.desired,a.desired_rev,a.observed FROM agent a
			WHERE a.revoked_at IS NULL AND (a.home_room_id=? AND EXISTS(SELECT 1 FROM room_member m WHERE m.room_id=? AND m.member_kind='agent' AND m.member_id=a.id AND m.left_at IS NULL)
			OR EXISTS(SELECT 1 FROM scope_control_agent sc WHERE sc.scope_kind='room' AND sc.scope_id=? AND sc.agent_id=a.id)
			OR EXISTS(SELECT 1 FROM scope_control_failure f WHERE f.scope_kind='room' AND f.scope_id=? AND f.agent_id=a.id)) ORDER BY a.id`, id, id, id, id)
	} else {
		rows, err = tx.tx.QueryContext(tx.ctx, `SELECT id,desired,desired_rev,observed FROM agent
			WHERE project_id=? AND revoked_at IS NULL ORDER BY id`, id)
	}
	if err != nil {
		return ScopeControl{}, err
	}
	type candidate struct {
		id, desired, observed string
		rev                   int64
	}
	var agents []candidate
	for rows.Next() {
		var a candidate
		if err = rows.Scan(&a.id, &a.desired, &a.rev, &a.observed); err != nil {
			rows.Close()
			return ScopeControl{}, err
		}
		agents = append(agents, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return ScopeControl{}, err
	}
	for _, a := range agents {
		if current == target && action != "resume" {
			var failed bool
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM scope_control_failure WHERE scope_kind=? AND scope_id=? AND agent_id=?)`, kind, id, a.id).Scan(&failed); err != nil {
				return ScopeControl{}, err
			}
			if !failed {
				continue
			}
		}
		ledgers, err := tx.scopeLedgers(a.id)
		if err != nil {
			return ScopeControl{}, err
		}
		if len(ledgers) > 0 && (a.rev != ledgers[len(ledgers)-1].bulkRev || a.desired != ledgers[len(ledgers)-1].bulkDesired) {
			// An individual control supersedes every scope's restore intent.
			if _, err := tx.Exec(`DELETE FROM scope_control_agent WHERE agent_id=?`, a.id); err != nil {
				return ScopeControl{}, err
			}
			ledgers = nil
		}
		own := -1
		for i, item := range ledgers {
			if item.kind == kind && item.id == id {
				own = i
				break
			}
		}
		base := a.desired
		if len(ledgers) > 0 {
			base = ledgers[0].restore
		}
		if action == "resume" {
			if err := tx.clearScopeFailure(kind, id, a.id); err != nil {
				return ScopeControl{}, err
			}
			if own < 0 {
				continue
			}
			remaining := append([]scopeLedger{}, ledgers[:own]...)
			remaining = append(remaining, ledgers[own+1:]...)
			newDesired := scopeEffectiveDesired(base, remaining)
			newRev := a.rev
			if newDesired != a.desired {
				updated, controlErr := tx.scopeAgentTransition(a.id, actor, newDesired, a.rev, a.desired == "stopped" && newDesired == "not_accepting")
				if controlErr != nil {
					// Keep the hold, gate, and failure records together. A failed
					// Resume must leave the scope stopped for a later retry.
					return ScopeControl{}, controlErr
				}
				out.Results = append(out.Results, ScopeAgentResult{Agent: &updated})
				newRev = updated.DesiredRev
			}
			if _, err := tx.Exec(`DELETE FROM scope_control_agent WHERE scope_kind=? AND scope_id=? AND agent_id=?`, kind, id, a.id); err != nil {
				return ScopeControl{}, err
			}
			if err := tx.syncScopeLedgers(a.id, base, newDesired, newRev); err != nil {
				return ScopeControl{}, err
			}
			changed = true
		} else {
			hold := "not_accepting"
			if action == "stop" {
				hold = "stopped"
			}
			if len(ledgers) == 0 && (a.desired == hold || a.desired == "stopped" && hold == "not_accepting") {
				if err := tx.clearScopeFailure(kind, id, a.id); err != nil {
					return ScopeControl{}, err
				}
				continue
			}
			prospective := append([]scopeLedger{}, ledgers...)
			if own >= 0 {
				prospective[own].hold = hold
			} else {
				prospective = append(prospective, scopeLedger{kind: kind, id: id, hold: hold})
			}
			newDesired := scopeEffectiveDesired(base, prospective)
			newRev := a.rev
			if newDesired != a.desired {
				updated, controlErr := tx.scopeAgentTransition(a.id, actor, newDesired, a.rev, a.desired == "stopped" && newDesired == "not_accepting")
				if controlErr != nil {
					message := controlErr.Error()
					out.Results = append(out.Results, ScopeAgentResult{Error: fmt.Sprintf("%s: %s", a.id, message)})
					if err := tx.recordScopeFailure(kind, id, a.id, action, message); err != nil {
						return ScopeControl{}, err
					}
					continue
				}
				out.Results = append(out.Results, ScopeAgentResult{Agent: &updated})
				newRev = updated.DesiredRev
			}
			if _, err := tx.Exec(`INSERT INTO scope_control_agent(scope_kind,scope_id,agent_id,restore_desired,bulk_desired,bulk_rev,hold_desired)
				VALUES (?,?,?,?,?,?,?) ON CONFLICT(scope_kind,scope_id,agent_id) DO UPDATE SET hold_desired=excluded.hold_desired`, kind, id, a.id, base, newDesired, newRev, hold); err != nil {
				return ScopeControl{}, err
			}
			if err := tx.syncScopeLedgers(a.id, base, newDesired, newRev); err != nil {
				return ScopeControl{}, err
			}
			if err := tx.clearScopeFailure(kind, id, a.id); err != nil {
				return ScopeControl{}, err
			}
			if own < 0 || ledgers[own].hold != hold || newDesired != a.desired {
				changed = true
			}
		}
	}
	if action == "resume" {
		if _, err := tx.Exec(`DELETE FROM scope_control_failure WHERE scope_kind=? AND scope_id=?`, kind, id); err != nil {
			return ScopeControl{}, err
		}
	}
	out.Failures, err = tx.scopeFailures(kind, id)
	if err != nil {
		return ScopeControl{}, err
	}
	if !changed {
		return out, nil
	}
	newGate := "paused"
	if action == "resume" {
		newGate = "open"
	}
	if _, err = tx.Exec("UPDATE "+table+" SET control_desired=?,gate=?,gate_rev=gate_rev+1 WHERE id=?", target, newGate, id); err != nil {
		return ScopeControl{}, err
	}
	if _, err = tx.AppendEvent(Event{Kind: "scope_control_requested", ProjectID: projectID, RoomID: roomID,
		PayloadJSON: payloadJSON(map[string]string{"scope_kind": kind, "scope_id": id, "action": action, "actor_id": actor})}); err != nil {
		return ScopeControl{}, err
	}
	verb := map[string]string{"pause": "paused", "stop": "stopped", "resume": "resumed"}[action]
	// The notice names the scope the ids-are-truth way when it is an entity (an agent as its @agt_<id> token, a room as
	// its #room_<id>), so the human view shows names; a project keeps its kind and id.
	scope := kind + " " + id
	switch kind {
	case "agent":
		scope = "@" + id
	case "room":
		scope = "#" + id
	}
	if _, err = tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "system", AuthorID: "system",
		Body: fmt.Sprintf("%s %s by owner/operator", scope, verb)}); err != nil {
		return ScopeControl{}, err
	}
	out.Desired, out.EffectiveState, out.Rev = target, target, currentRev+1
	return out, nil
}

// ScopeBudgetPaused says whether the scope's daily budget has paused it today, which a resume is refused for.
func (s *Store) ScopeBudgetPaused(ctx context.Context, kind, id string) (paused bool, err error) {
	err = s.Tx(ctx, func(tx *Tx) (err error) {
		paused, err = tx.budgetPaused(kind, id, time.Now())
		return err
	})
	return paused, err
}

// ScopeOwnWorktreeNames lists the worktrees of the agents a bulk control of the scope holds that are in use as their folder (ready), by name.
// A resume locks them, as a start of one agent does, so a removal cannot take a folder from an agent it starts.
func (s *Store) ScopeOwnWorktreeNames(ctx context.Context, kind, id string) ([]string, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	if _, err := scopeTable(kind); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT w.name FROM scope_control_agent sc JOIN agent_worktree w ON w.agent_id=sc.agent_id
		WHERE sc.scope_kind=? AND sc.scope_id=? AND w.state='ready' ORDER BY w.name`, kind, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// ProjectFolderContains says whether path is the project's folder on the machine or inside it, as the machine's own boundary check resolves it.
func (s *Store) ProjectFolderContains(ctx context.Context, projectID, machineID, path string) (bool, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return false, err
	}
	var root string
	var host bool
	err := s.db.QueryRowContext(ctx, `SELECT CASE WHEN m.is_host=1 THEN p.folder ELSE COALESCE(f.folder,'') END,m.is_host
		FROM project p JOIN machine m ON m.id=? LEFT JOIN project_machine_folder f ON f.project_id=p.id AND f.machine_id=m.id
		WHERE p.id=?`, machineID, projectID).Scan(&root, &host)
	if err != nil {
		return false, err
	}
	if host {
		if canonical, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil {
			root = filepath.Clean(canonical)
		}
	}
	return WithinFolder(root, path), nil
}
