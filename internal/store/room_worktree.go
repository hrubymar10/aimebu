package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type RoomWorktree struct {
	RoomID         string `json:"room_id"`
	BaseRef        string `json:"base_ref"`
	BaseCommit     string `json:"base_commit"`
	Branch         string `json:"branch"`
	RelativePath   string `json:"relative_path"`
	State          string `json:"state"`
	Reason         string `json:"reason,omitempty"`
	DirtyBaseCount int    `json:"dirty_base_count,omitempty"`
	Path           string `json:"path,omitempty"`
	MachineID      string `json:"machine_id,omitempty"`
}

func (s *Store) AgentHomeRoomID(ctx context.Context, agentID string) (string, error) {
	var roomID string
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(home_room_id,'') FROM agent WHERE id=?`, agentID).Scan(&roomID)
	return roomID, err
}

// RoomWorktreeInUse says whether a live agent works in the checkout, which blocks removing it.
func (s *Store) RoomWorktreeInUse(ctx context.Context, machineID, path string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent a JOIN checkout c ON c.id=a.checkout_id
		WHERE a.machine_id=? AND c.canonical_path=? AND a.revoked_at IS NULL
		AND (a.desired!='stopped' OR a.observed!='stopped')`, machineID, path).Scan(&count)
	return count > 0, err
}

// CancelRoomWorktree clears a pending or failed opt-in while the room is open.
func (s *Store) CancelRoomWorktree(ctx context.Context, roomID string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		var projectID, lifecycle string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,lifecycle FROM room WHERE id=?`, roomID).Scan(&projectID, &lifecycle); err != nil {
			return err
		}
		if lifecycle != "open" {
			return ErrInvalidTransition
		}
		var count int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM room_worktree_instance WHERE room_id=? AND canonical_path!=''`, roomID).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return ErrConflict
		}
		if _, err := tx.Exec(`DELETE FROM room_worktree_instance WHERE room_id=?`, roomID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM room_worktree WHERE room_id=?`, roomID); err != nil {
			return err
		}
		_, err := tx.AppendEvent(Event{Kind: "room_worktree_cancelled", ProjectID: projectID, RoomID: roomID,
			PayloadJSON: payloadJSON(map[string]string{"room_id": roomID})})
		return err
	})
}

func (s *Store) RoomWorktree(ctx context.Context, roomID, machineID string) (RoomWorktree, bool, error) {
	var item RoomWorktree
	err := s.db.QueryRowContext(ctx, `SELECT room_id,base_ref,base_commit,branch_name,relative_path,state,cleanup_reason,dirty_base_count
		FROM room_worktree WHERE room_id=?`, roomID).Scan(&item.RoomID, &item.BaseRef, &item.BaseCommit, &item.Branch,
		&item.RelativePath, &item.State, &item.Reason, &item.DirtyBaseCount)
	if errors.Is(err, sql.ErrNoRows) {
		return item, false, nil
	}
	if err != nil {
		return item, false, err
	}
	if machineID != "" {
		item.MachineID = machineID
		var state, reason string
		err = s.db.QueryRowContext(ctx, `SELECT canonical_path,base_commit,state,reason,dirty_base_count FROM room_worktree_instance WHERE room_id=? AND machine_id=?`, roomID, machineID).
			Scan(&item.Path, &item.BaseCommit, &state, &reason, &item.DirtyBaseCount)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return item, true, err
		}
		if err == nil {
			item.State, item.Reason = state, reason
		} else {
			item.Path, item.BaseCommit, item.DirtyBaseCount = "", "", 0
			item.State, item.Reason = "pending", ""
		}
	}
	return item, true, nil
}

func (s *Store) RequestRoomWorktree(ctx context.Context, roomID, baseRef string) (RoomWorktree, error) {
	var item RoomWorktree
	err := s.Tx(ctx, func(tx *Tx) error {
		var projectID, kind, lifecycle string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,kind,lifecycle FROM room WHERE id=?`, roomID).Scan(&projectID, &kind, &lifecycle); err != nil {
			return err
		}
		if kind != "task" || lifecycle != "open" {
			return ErrInvalidTransition
		}
		var prior string
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT base_ref FROM room_worktree WHERE room_id=?`, roomID).Scan(&prior)
		if err == nil {
			if baseRef != "" && prior != "" && baseRef != prior {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.Exec(`INSERT INTO room_worktree(room_id,base_ref,branch_name,relative_path,state,created_at,updated_at)
			VALUES (?,?,?,?, 'pending',?,?)`, roomID, baseRef, "aimebu/"+roomID, ".aimebu/worktrees/"+roomID, timestamp(), timestamp())
		if err != nil {
			return err
		}
		_, err = tx.AppendEvent(Event{Kind: "room_worktree_requested", ProjectID: projectID, RoomID: roomID,
			PayloadJSON: payloadJSON(map[string]string{"room_id": roomID})})
		return err
	})
	if err != nil {
		return item, err
	}
	item, _, err = s.RoomWorktree(ctx, roomID, "")
	return item, err
}

func (s *Store) RecordRoomWorktree(ctx context.Context, roomID, machineID, path, baseRef, baseCommit, state, reason string, dirty int) error {
	return s.Tx(ctx, func(tx *Tx) error {
		var projectID string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id FROM room WHERE id=?`, roomID).Scan(&projectID); err != nil {
			return err
		}
		if state != "ready" && state != "failed" && state != "preserved" && state != "removed" {
			return ErrInvalidTransition
		}
		var oldState string
		oldErr := tx.tx.QueryRowContext(tx.ctx, `SELECT state FROM room_worktree_instance WHERE room_id=? AND machine_id=?`, roomID, machineID).Scan(&oldState)
		if oldErr != nil && !errors.Is(oldErr, sql.ErrNoRows) {
			return oldErr
		}
		if _, err := tx.Exec(`UPDATE room_worktree SET base_ref=?,base_commit=?,state=?,cleanup_reason=?,dirty_base_count=?,updated_at=? WHERE room_id=?`,
			baseRef, baseCommit, state, reason, dirty, timestamp(), roomID); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO room_worktree_instance(room_id,machine_id,canonical_path,base_commit,dirty_base_count,state,reason,updated_at) VALUES (?,?,?,?,?,?,?,?)
			ON CONFLICT(room_id,machine_id) DO UPDATE SET
			canonical_path=CASE WHEN excluded.canonical_path='' THEN room_worktree_instance.canonical_path ELSE excluded.canonical_path END,
			base_commit=CASE WHEN excluded.base_commit='' THEN room_worktree_instance.base_commit ELSE excluded.base_commit END,
			dirty_base_count=CASE WHEN excluded.canonical_path='' THEN room_worktree_instance.dirty_base_count ELSE excluded.dirty_base_count END,
			state=excluded.state,reason=excluded.reason,updated_at=excluded.updated_at`, roomID, machineID, path, baseCommit, dirty, state, reason, timestamp()); err != nil {
			return err
		}
		if dirty > 0 && state == "ready" && oldState != "ready" {
			if _, err := tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "system", AuthorID: "system",
				Body: fmt.Sprintf("The base has %d uncommitted changes, which are not in the worktree.", dirty)}); err != nil {
				return err
			}
		}
		if state == "preserved" && oldState != "preserved" {
			if _, err := tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "system", AuthorID: "system",
				Body: "Room worktree was preserved: " + reason}); err != nil {
				return err
			}
		}
		_, err := tx.AppendEvent(Event{Kind: "room_worktree_updated", ProjectID: projectID, RoomID: roomID,
			PayloadJSON: payloadJSON(map[string]string{"room_id": roomID, "state": state})})
		return err
	})
}
