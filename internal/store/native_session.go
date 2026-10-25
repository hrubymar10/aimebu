package store

import (
	"context"
	"database/sql"
	"errors"
)

// RecordNativeSessionStarted binds a CLI-reported session handle to the
// admitted turn's session. Journal replay is idempotent, including after a
// generation changes, but a different handle cannot replace the first one.
func (s *Store) RecordNativeSessionStarted(ctx context.Context, turnID, agentID string, generation int, sourceID, handle string) error {
	if sourceID == "" || handle == "" || len(handle) > 512 {
		return ErrConflict
	}
	return s.Tx(ctx, func(tx *Tx) error {
		var sessionID, turnAgent, projectID, roomID string
		var turnGeneration int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT t.session_id,t.agent_id,t.generation,a.project_id,t.room_id
			FROM turn t JOIN agent a ON a.id=t.agent_id WHERE t.id=?`, turnID).
			Scan(&sessionID, &turnAgent, &turnGeneration, &projectID, &roomID); err != nil {
			return err
		}
		if turnAgent != agentID {
			return ErrForbidden
		}
		if turnGeneration != generation {
			return ErrStaleGeneration
		}
		var existing, forkOf sql.NullString
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT native_session_id,fork_of FROM session WHERE id=? AND agent_id=? AND generation=?`,
			sessionID, agentID, generation).Scan(&existing, &forkOf); err != nil {
			return err
		}
		// A session forked from another names the original until its first turn reports the copy's id: that one report changes the handle.
		forking := forkOf.Valid && existing.Valid && existing.String == forkOf.String && handle != forkOf.String
		if existing.Valid && existing.String != handle && !forking {
			return ErrConflict
		}
		payload := payloadJSON(map[string]string{"source_event_id": sourceID, "native_session_handle": handle})
		var oldPayload string
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT payload_json FROM event WHERE turn_id=? AND kind='native_session_started'
			AND json_extract(payload_json,'$.source_event_id')=?`, turnID, sourceID).Scan(&oldPayload)
		if err == nil {
			if oldPayload != payload {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.Exec(`UPDATE session SET native_session_id=?,native_session_started_at=COALESCE(native_session_started_at,?) WHERE id=?`,
			handle, timestamp(), sessionID); err != nil {
			return err
		}
		if forking {
			if _, err := tx.Exec(`UPDATE imported_session SET forked_session_id=? WHERE agent_id=?`, handle, agentID); err != nil {
				return err
			}
		}
		_, err = tx.AppendEvent(Event{Kind: "native_session_started", ProjectID: projectID, RoomID: roomID,
			AgentID: agentID, TurnID: turnID, PayloadJSON: payload})
		return err
	})
}

func sessionNativeState(tx *Tx, sessionID string) (string, bool, error) {
	handle, started, _, err := sessionNativeFork(tx, sessionID)
	return handle, started, err
}

// sessionNativeFork is sessionNativeState and whether the session is a fork whose copy has not been named yet: the next turn forks the
// session it names (the original) and reports the copy's id.
func sessionNativeFork(tx *Tx, sessionID string) (handle string, started, forking bool, err error) {
	var h, s, f sql.NullString
	err = tx.tx.QueryRowContext(tx.ctx, `SELECT native_session_id,native_session_started_at,fork_of FROM session WHERE id=?`, sessionID).Scan(&h, &s, &f)
	return h.String, s.Valid, f.Valid && h.Valid && h.String == f.String, err
}
