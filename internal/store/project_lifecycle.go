package store

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"os"
	"time"

	"github.com/goccy/go-json"
)

var (
	// ErrProjectArchived is returned for a change to an archived project that reached a transaction (the HTTP guard refuses these first).
	ErrProjectArchived = errors.New("project_archived")
	// ErrProjectNotArchived is returned for deleting a project that is still live: archive it first, as for a room.
	ErrProjectNotArchived = errors.New("project_not_archived")
	// ErrProjectBusy is returned for deleting a project that still has an agent that may be running or a live worktree.
	ErrProjectBusy = errors.New("project_busy")
	// ErrProjectNameMismatch is returned for a delete whose typed name is not the project's.
	ErrProjectNameMismatch = errors.New("project_name_mismatch")
)

// StopPendingError identifies the machine that must apply an agent's stop
// before its project can be removed. Revoking that machine remains an escape
// hatch when it will never reconnect.
type StopPendingError struct{ MachineID string }

func (e *StopPendingError) Error() string { return "stop pending on machine " + e.MachineID }
func (e *StopPendingError) Unwrap() error { return ErrProjectBusy }

// What a project deletion has to clear besides the rows the foreign keys reach. TestProjectDeleteCoversEveryTableThatNamesAProject reads
// the schema and fails when a table that names a project is in none of these lists, so a table added later cannot be forgotten.
var (
	// projectOptionalTables have a project_id that may be empty, where empty means "everyone's" or "global": an event with no project is
	// the instance owner's, and a role, a prompt override or an invite with none applies everywhere. Walking the foreign keys would set
	// the column to NULL and turn the project's rows into global ones, so they are deleted first.
	projectOptionalTables = []string{"event", "prompt_override", "role", "user_invite"}
	// projectLooseTables name the project in a project_id column with no foreign key to it.
	projectLooseTables = []string{"cleanup_log"}
	// projectScopeTables are scoped by scope_kind and scope_id, which name a project or one of its rooms, with no foreign key.
	projectScopeTables = []string{"scope_control_failure", "scope_control_agent", "usage_limit_day", "usage_limit"}
	// projectNotLinked are columns that look like a project reference but are not one: v1's own agent session names, and a memory
	// scope key that is a user or the word "global".
	projectNotLinked = map[string]string{"legacy_agent_session.project": "the v1 project name, kept for the import", "legacy_agent_session_history.project": "the v1 project name, kept for the import", "global_memory.scope_key": "a user or the word global, never a project"}
)

// refuseArchived is the check a transaction makes before it starts or restarts anything in a project.
func (tx *Tx) refuseArchived(projectID string) error {
	var archived bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT archived_at IS NOT NULL FROM project WHERE id=?`, projectID).Scan(&archived); err != nil {
		return err
	}
	if archived {
		return ErrProjectArchived
	}
	return nil
}

// ArchivedScope reports whether the project that owns the room, agent, inbox item or message (or the project itself) is archived, for
// the guard that keeps an archived project read-only. kind is "project", "room", "agent", "inbox" or "message". It answers true only for
// someone who may write to that project at all (a member, the instance owner, an admin): anyone else is told nothing, so an id does not
// reveal whether its project is archived (the route then answers as it does for any object the caller cannot see). An unknown id is not
// archived.
func (s *Store) ArchivedScope(ctx context.Context, userID, kind, id string) (bool, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return false, err
	}
	if kind == "room" || kind == "message" {
		if archived, ok, err := s.agentChatArchived(ctx, userID, kind, id); err != nil || ok {
			return archived, err
		}
	}
	var query string
	switch kind {
	case "project":
		query = `SELECT p.archived_at IS NOT NULL FROM project p WHERE p.id=?`
	case "room":
		query = `SELECT p.archived_at IS NOT NULL FROM room r JOIN project p ON p.id=r.project_id WHERE r.id=?`
	case "agent":
		query = `SELECT p.archived_at IS NOT NULL FROM agent a JOIN project p ON p.id=a.project_id WHERE a.id=?`
	case "inbox":
		query = `SELECT p.archived_at IS NOT NULL FROM inbox_item i JOIN project p ON p.id=i.project_id WHERE i.id=?`
	case "message":
		query = `SELECT p.archived_at IS NOT NULL FROM message m JOIN room r ON r.id=m.room_id JOIN project p ON p.id=r.project_id WHERE m.id=?`
	default:
		return false, nil
	}
	// Whoever may write to the project at all: its members, the instance owner and the admins. Anyone else is told nothing.
	query += ` AND (EXISTS (SELECT 1 FROM membership mm WHERE mm.project_id=p.id AND mm.user_id=?)
		OR ?=(SELECT id FROM user ORDER BY created_at,id LIMIT 1)
		OR EXISTS (SELECT 1 FROM user ua WHERE ua.id=? AND ua.admin=1 AND ua.disabled_at IS NULL))`
	var archived bool
	err := s.db.QueryRowContext(ctx, query, id, userID, userID, userID).Scan(&archived)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return archived, err
}

// ArchiveFailure is an agent that archiving could not stop, and why.
type ArchiveFailure struct {
	AgentID string `json:"agent_id"`
	Error   string `json:"error"`
}

// ArchiveProject makes the project read-only: its agents are stopped, its dispatch gate closed, its schedules switched off and the
// questions and usage notices waiting in its Inbox withdrawn (recovery and delivery-failure items stay, to be resolved). The rooms and
// their messages stay as they are. The caller has decided that the actor may do this (the project's owner or an admin). Archiving an
// archived project changes nothing. An agent that cannot be stopped (one whose execution is unknown until a recovery decision) does not
// stop the archive: it is reported, stays as it is, and is resolved by that decision. The cards are the agents that were stopped, for
// the caller to tell their machines.
func (s *Store) ArchiveProject(ctx context.Context, id, actor string) ([]AgentCard, []ArchiveFailure, error) {
	var controls []AgentCard
	var failures []ArchiveFailure
	err := s.Tx(ctx, func(tx *Tx) error {
		controls, failures = nil, nil
		var archived sql.NullString
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT archived_at FROM project WHERE id=? AND system=0`, id).Scan(&archived); err != nil {
			return err
		}
		if archived.Valid {
			return nil
		}
		now := timestamp()
		if _, err := tx.Exec(`UPDATE project SET archived_at=?,gate='paused',gate_rev=gate_rev+1 WHERE id=?`, now, id); err != nil {
			return err
		}
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT id,desired_rev FROM agent WHERE project_id=? AND revoked_at IS NULL AND desired<>'stopped' ORDER BY id`, id)
		if err != nil {
			return err
		}
		type target struct {
			id  string
			rev int64
		}
		var targets []target
		for rows.Next() {
			var t target
			if err := rows.Scan(&t.id, &t.rev); err != nil {
				rows.Close()
				return err
			}
			targets = append(targets, t)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, t := range targets {
			// Each agent in a savepoint of its own: one that cannot be stopped is reported and left, never a reason to keep the project live.
			if _, err := tx.Exec(`SAVEPOINT archive_agent`); err != nil {
				return err
			}
			card, err := tx.setAgentDesiredTrusted(t.id, actor, "stopped", t.rev)
			if err != nil {
				if _, rollback := tx.Exec(`ROLLBACK TO archive_agent`); rollback != nil {
					return rollback
				}
				if _, release := tx.Exec(`RELEASE archive_agent`); release != nil {
					return release
				}
				reason := err.Error()
				if errors.Is(err, ErrInvalidTransition) {
					reason = "its execution is unknown until a recovery decision is made in the Inbox"
				}
				failures = append(failures, ArchiveFailure{AgentID: t.id, Error: reason})
				continue
			}
			if _, err := tx.Exec(`RELEASE archive_agent`); err != nil {
				return err
			}
			controls = append(controls, card)
		}
		if _, err := tx.Exec(`UPDATE schedule SET enabled=0,rev=rev+1,updated_at=? WHERE project_id=? AND enabled=1`, now, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE inbox_item SET state='withdrawn',rev=rev+1,resolved_at=? WHERE project_id=? AND state='open' AND kind IN ('question','approval','usage_info','usage_limit')`, now, id); err != nil {
			return err
		}
		_, err = tx.AppendEvent(Event{Kind: "project_archived", ProjectID: id, PayloadJSON: payloadJSON(map[string]string{"project_id": id, "actor_id": actor})})
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return controls, failures, nil
}

// ReopenProject lifts the archive: the project accepts changes again and its dispatch gate goes back to what its own control and its
// daily limits say (open only when it is not paused or stopped and no usage limit has paused it today). Its agents stay stopped and its
// schedules stay off; they are the person's to start again. Reopening a live project changes nothing.
func (s *Store) ReopenProject(ctx context.Context, id, actor string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		var archived sql.NullString
		var control string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT archived_at,control_desired FROM project WHERE id=? AND system=0`, id).Scan(&archived, &control); err != nil {
			return err
		}
		if !archived.Valid {
			return nil
		}
		limited, err := tx.budgetPaused("project", id, time.Now())
		if err != nil {
			return err
		}
		gate := "paused"
		if control == "active" && !limited {
			gate = "open"
		}
		if _, err := tx.Exec(`UPDATE project SET archived_at=NULL,gate=?,gate_rev=gate_rev+1 WHERE id=?`, gate, id); err != nil {
			return err
		}
		_, err = tx.AppendEvent(Event{Kind: "project_reopened", ProjectID: id, PayloadJSON: payloadJSON(map[string]string{"project_id": id, "actor_id": actor})})
		return err
	})
}

// DeleteProject removes an archived project for good, with every room, message, reaction, attachment, agent, usage record, memory,
// schedule, task note, Inbox item, worktree record, member, spawn default and event that belonged to it, and the attachment files. Files in
// the project folder and worktrees on disk are not touched. It is allowed only when every agent is confirmed stopped (desired and observed
// both stopped) or sits on a revoked machine, which can never come back; an agent on an approved machine that is merely offline may still
// be running its turn and would run on, unknown to the server, when the machine returns, so it refuses (bring the machine online or
// revoke it). A worktree that is still live (not removed) refuses it too. name must be the project's name: the caller asks the person to
// type it, and this is the check that it was.
func (s *Store) DeleteProject(ctx context.Context, id, name, actor string) error {
	var files []string
	var members []string
	err := s.Tx(ctx, func(tx *Tx) error {
		files, members = nil, nil
		var projectName string
		var archived sql.NullString
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT name,archived_at FROM project WHERE id=? AND system=0`, id).Scan(&projectName, &archived); err != nil {
			return err
		}
		if !archived.Valid {
			return ErrProjectNotArchived
		}
		if name != projectName {
			return ErrProjectNameMismatch
		}
		var pendingMachine string
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.machine_id FROM agent a JOIN machine m ON m.id=a.machine_id
			WHERE a.project_id=? AND a.revoked_at IS NULL AND m.revoked_at IS NULL AND a.desired='stopped'
			AND NOT EXISTS (SELECT 1 FROM event e WHERE e.agent_id=a.id AND e.kind='agent_control_applied'
				AND CAST(json_extract(e.payload_json,'$.desired_rev') AS INTEGER)=a.desired_rev)
			ORDER BY a.id LIMIT 1`, id).Scan(&pendingMachine)
		if err == nil {
			return &StopPendingError{MachineID: pendingMachine}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var busy int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT (SELECT COUNT(*) FROM agent a LEFT JOIN machine m ON m.id=a.machine_id
				WHERE a.project_id=? AND a.revoked_at IS NULL AND (m.revoked_at IS NULL) AND
				(a.desired<>'stopped' OR a.observed<>'stopped'))
			+ (SELECT COUNT(*) FROM room_worktree w JOIN room r ON r.id=w.room_id WHERE r.project_id=? AND w.state<>'removed')
			+ (SELECT COUNT(*) FROM room_worktree_instance i JOIN room r ON r.id=i.room_id WHERE r.project_id=? AND i.state IN ('ready','preserved'))`, id, id, id).Scan(&busy); err != nil {
			return err
		}
		if busy > 0 {
			return ErrProjectBusy
		}
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT path FROM attachment WHERE project_id=?`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var path string
			if err := rows.Scan(&path); err != nil {
				rows.Close()
				return err
			}
			files = append(files, path)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		memberRows, err := tx.tx.QueryContext(tx.ctx, `SELECT user_id FROM membership WHERE project_id=? ORDER BY user_id`, id)
		if err != nil {
			return err
		}
		for memberRows.Next() {
			var user string
			if err := memberRows.Scan(&user); err != nil {
				memberRows.Close()
				return err
			}
			members = append(members, user)
		}
		err = memberRows.Err()
		memberRows.Close()
		if err != nil {
			return err
		}
		// Chats between agents belong to no project and outlive it: the project's agents leave them before the cascade removes the agents.
		if err := tx.detachAgentsFromChats(id, projectName); err != nil {
			return err
		}
		for _, table := range projectLooseTables {
			if _, err := tx.Exec(`DELETE FROM `+table+` WHERE project_id=?`, id); err != nil {
				return err
			}
		}
		for _, table := range projectScopeTables {
			if _, err := tx.Exec(`DELETE FROM `+table+` WHERE (scope_kind='project' AND scope_id=?) OR (scope_kind='room' AND scope_id IN (SELECT id FROM room WHERE project_id=?))`, id, id); err != nil {
				return err
			}
		}
		graph, err := tx.foreignKeyGraph()
		if err != nil {
			return err
		}
		// The foreign keys nest a few levels deeper from a project than from a room, so the walk starts with more headroom.
		for _, table := range projectOptionalTables {
			if err := tx.deleteCascade(graph, table, "project_id=?", []any{id}, -4); err != nil {
				return err
			}
		}
		if err := tx.deleteCascade(graph, "project", "id=?", []any{id}, -4); err != nil {
			return err
		}
		// The project's own events went with it, so this one belongs to nobody's project; it names the members it concerns.
		payload, _ := json.Marshal(map[string]any{"project_id": id, "actor_id": actor, "user_ids": members})
		_, err = tx.AppendEvent(Event{Kind: "project_removed", PayloadJSON: string(payload)})
		return err
	})
	if err != nil {
		return err
	}
	for _, path := range files {
		var shared int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM attachment WHERE path=?`, path).Scan(&shared); err == nil && shared > 0 {
			continue
		}
		// The project is already gone; a file that cannot be removed is a leftover to log, not a failed removal.
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			log.Printf("remove project %s: attachment file %s left behind: %v", id, path, removeErr)
		}
	}
	return nil
}
