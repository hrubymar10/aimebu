package store

import (
	"context"
	"database/sql"
	"errors"
)

type RoomViewPreference struct {
	Hidden bool `json:"hidden"`
}

// SetRoomViewPreference changes one user's view of a room. Hiding is the only per-room view setting.
func (s *Store) SetRoomViewPreference(ctx context.Context, userID, roomID string, hidden *bool) (RoomViewPreference, error) {
	if err := s.UserRoomAccess(ctx, userID, roomID); err != nil {
		return RoomViewPreference{}, err
	}
	var preference RoomViewPreference
	err := s.Tx(ctx, func(tx *Tx) error {
		var projectID string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id FROM room WHERE id=?`, roomID).Scan(&projectID); err != nil {
			return err
		}
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT hidden FROM room_view_preference WHERE user_id=? AND room_id=?`, userID, roomID).Scan(&preference.Hidden)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		old := preference
		if hidden != nil {
			preference.Hidden = *hidden
		}
		if preference == old {
			return nil
		}
		if _, err := tx.Exec(`INSERT INTO room_view_preference(user_id,room_id,hidden) VALUES (?,?,?)
			ON CONFLICT(user_id,room_id) DO UPDATE SET hidden=excluded.hidden`, userID, roomID, preference.Hidden); err != nil {
			return err
		}
		_, err = tx.AppendEvent(Event{Kind: "room_preference_changed", ProjectID: projectID, RoomID: roomID,
			PayloadJSON: payloadJSON(map[string]string{"user_id": userID})})
		return err
	})
	return preference, err
}
