package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// NotifyLevels are the per-room notification levels, in the order the room settings list them.
var NotifyLevels = []string{"default", "all", "mentions", "none"}

// ErrInvalidRoomPreference is a notification level or mute time the server refuses; the message says which.
var ErrInvalidRoomPreference = errors.New("invalid room preference")

// maxRoomMute bounds how far ahead a room can be muted: a mute is a pause, not a way to silence a room for good (that is the level none).
const maxRoomMute = 366 * 24 * time.Hour

type RoomViewPreference struct {
	Hidden     bool   `json:"hidden"`
	Notify     string `json:"notify"`
	MutedUntil string `json:"muted_until,omitempty"`
}

// RoomViewUpdate is what one request changes; a nil field is left as it is. MutedUntil is an RFC 3339 instant in the future, or "" to
// end a mute.
type RoomViewUpdate struct {
	Hidden     *bool
	Notify     *string
	MutedUntil *string
}

// SetRoomViewPreference changes one user's view of a room: hidden from the lists, how it notifies, and an optional mute.
func (s *Store) SetRoomViewPreference(ctx context.Context, userID, roomID string, update RoomViewUpdate) (RoomViewPreference, error) {
	if err := s.UserRoomAccess(ctx, userID, roomID); err != nil {
		return RoomViewPreference{}, err
	}
	if update.Notify != nil && !validNotifyLevel(*update.Notify) {
		return RoomViewPreference{}, fmt.Errorf("%w: notify must be one of default, all, mentions, none", ErrInvalidRoomPreference)
	}
	var mutedUntil *string
	if update.MutedUntil != nil {
		if *update.MutedUntil == "" {
			empty := ""
			mutedUntil = &empty
		} else {
			until, err := time.Parse(time.RFC3339, *update.MutedUntil)
			if err != nil {
				return RoomViewPreference{}, fmt.Errorf("%w: muted_until must be an RFC 3339 time", ErrInvalidRoomPreference)
			}
			now := time.Now()
			if !until.After(now) || until.After(now.Add(maxRoomMute)) {
				return RoomViewPreference{}, fmt.Errorf("%w: muted_until must be in the future, within a year", ErrInvalidRoomPreference)
			}
			formatted := until.UTC().Format(time.RFC3339)
			mutedUntil = &formatted
		}
	}
	var preference RoomViewPreference
	err := s.Tx(ctx, func(tx *Tx) error {
		var projectID string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id FROM room WHERE id=?`, roomID).Scan(&projectID); err != nil {
			return err
		}
		preference = RoomViewPreference{Notify: "default"}
		var muted sql.NullString
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT hidden,notify,muted_until FROM room_view_preference WHERE user_id=? AND room_id=?`, userID, roomID).Scan(&preference.Hidden, &preference.Notify, &muted)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		preference.MutedUntil = muted.String
		old := preference
		if update.Hidden != nil {
			preference.Hidden = *update.Hidden
		}
		if update.Notify != nil {
			preference.Notify = *update.Notify
		}
		if mutedUntil != nil {
			preference.MutedUntil = *mutedUntil
		}
		if preference == old {
			return nil
		}
		if _, err := tx.Exec(`INSERT INTO room_view_preference(user_id,room_id,hidden,notify,muted_until) VALUES (?,?,?,?,?)
			ON CONFLICT(user_id,room_id) DO UPDATE SET hidden=excluded.hidden,notify=excluded.notify,muted_until=excluded.muted_until`,
			userID, roomID, preference.Hidden, preference.Notify, nullable(preference.MutedUntil)); err != nil {
			return err
		}
		_, err = tx.AppendEvent(Event{Kind: "room_preference_changed", ProjectID: projectID, RoomID: roomID,
			PayloadJSON: payloadJSON(map[string]string{"user_id": userID})})
		return err
	})
	return preference, err
}

func validNotifyLevel(level string) bool {
	for _, known := range NotifyLevels {
		if level == known {
			return true
		}
	}
	return false
}
