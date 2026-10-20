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
		var existing sql.NullString
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT native_session_id FROM session WHERE id=? AND agent_id=? AND generation=?`,
			sessionID, agentID, generation).Scan(&existing); err != nil {
			return err
		}
		if existing.Valid && existing.String != handle {
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
		_, err = tx.AppendEvent(Event{Kind: "native_session_started", ProjectID: projectID, RoomID: roomID,
			AgentID: agentID, TurnID: turnID, PayloadJSON: payload})
		return err
	})
}

func sessionNativeState(tx *Tx, sessionID string) (string, bool, error) {
	var handle, started sql.NullString
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT native_session_id,native_session_started_at FROM session WHERE id=?`, sessionID).
		Scan(&handle, &started)
	return handle.String, started.Valid, err
}
