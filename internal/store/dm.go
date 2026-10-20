package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
)

// DMPeer is an active account in the caller's project that can share a
// private chat with them. No account credentials or unrelated users leave
// the store through this view.
type DMPeer struct {
	ID    string `json:"id"`
	Login string `json:"login"`
}

func (s *Store) DMPeers(ctx context.Context, projectID, userID string) ([]DMPeer, error) {
	if err := s.ProjectAccess(ctx, userID, projectID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT u.id,u.login FROM membership m
		JOIN user u ON u.id=m.user_id WHERE m.project_id=? AND u.id!=?
		AND u.disabled_at IS NULL ORDER BY u.login,u.id`, projectID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	peers := []DMPeer{}
	for rows.Next() {
		var peer DMPeer
		if err := rows.Scan(&peer.ID, &peer.Login); err != nil {
			return nil, err
		}
		peers = append(peers, peer)
	}
	return peers, rows.Err()
}

// OpenUserDM returns the single private room for two users in a project.
// The key is stable regardless of which participant opens it first.
func (s *Store) OpenUserDM(ctx context.Context, projectID, userID, peerID string) (Room, error) {
	if userID == peerID || peerID == "" {
		return Room{}, ErrInvalidTransition
	}
	ids := []string{userID, peerID}
	sort.Strings(ids)
	key := "users:" + projectID + ":" + ids[0] + ":" + ids[1]
	var roomID string
	err := s.Tx(ctx, func(tx *Tx) error {
		var logins [2]string
		for i, id := range ids {
			err := tx.tx.QueryRowContext(ctx, `SELECT u.login FROM membership m JOIN user u ON u.id=m.user_id
				WHERE m.project_id=? AND m.user_id=? AND u.disabled_at IS NULL`, projectID, id).Scan(&logins[i])
			if errors.Is(err, sql.ErrNoRows) {
				return ErrForbidden
			}
			if err != nil {
				return err
			}
		}
		err := tx.tx.QueryRowContext(ctx, `SELECT id FROM room WHERE dm_key=?`, key).Scan(&roomID)
		if err == nil {
			result, err := tx.Exec(`UPDATE room_view_preference SET hidden=0 WHERE room_id=? AND user_id=? AND hidden=1`, roomID, userID)
			if err != nil {
				return err
			}
			changed, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if changed > 0 {
				_, err = tx.AppendEvent(Event{Kind: "room_preference_changed", ProjectID: projectID, RoomID: roomID,
					PayloadJSON: payloadJSON(map[string]string{"user_id": userID})})
			}
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		room, err := tx.CreateRoom(Room{ProjectID: projectID, Kind: "dm", DMKey: key,
			Title: logins[0] + " · " + logins[1], CreatedBy: userID})
		if err != nil {
			return err
		}
		roomID = room.ID
		for _, id := range ids {
			if err := tx.AddMember(roomID, "user", id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Room{}, err
	}
	return s.RoomByID(ctx, roomID)
}

// PeopleDMWriteAllowed admits the viewer write exception only while both
// human participants remain active project members and no agent has joined.
func (s *Store) PeopleDMWriteAllowed(ctx context.Context, projectID, roomID string) (bool, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return false, err
	}
	var members, eligible, agents int
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM room_member m WHERE m.room_id=? AND m.member_kind='user' AND m.left_at IS NULL),
		(SELECT COUNT(*) FROM room_member m JOIN user u ON u.id=m.member_id
		 JOIN membership p ON p.project_id=? AND p.user_id=m.member_id
		 WHERE m.room_id=? AND m.member_kind='user' AND m.left_at IS NULL AND u.disabled_at IS NULL),
		(SELECT COUNT(*) FROM room_member m WHERE m.room_id=? AND m.member_kind='agent' AND m.left_at IS NULL)`,
		roomID, projectID, roomID, roomID).Scan(&members, &eligible, &agents)
	if err != nil {
		return false, err
	}
	if members != 2 || eligible != 2 {
		return false, ErrForbidden
	}
	return agents == 0, nil
}

// OpenDM returns the one private room for a user and an agent. The unique key
// also serializes concurrent requests for the same pair in SQLite.
func (s *Store) OpenDM(ctx context.Context, userID, agentID string) (Room, error) {
	var roomID string
	err := s.Tx(ctx, func(tx *Tx) error {
		var projectID, slug string
		if err := tx.tx.QueryRowContext(ctx, `SELECT project_id,slug FROM agent WHERE id=? AND revoked_at IS NULL`, agentID).Scan(&projectID, &slug); err != nil {
			return err
		}
		var member int
		if err := tx.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM membership WHERE project_id=? AND user_id=?`, projectID, userID).Scan(&member); err != nil {
			return err
		}
		if member == 0 {
			return ErrForbidden
		}
		key := "user:" + userID + ":agent:" + agentID
		err := tx.tx.QueryRowContext(ctx, `SELECT id FROM room WHERE dm_key=?`, key).Scan(&roomID)
		if err == nil {
			result, err := tx.Exec(`UPDATE room_view_preference SET hidden=0 WHERE room_id=? AND user_id=? AND hidden=1`, roomID, userID)
			if err != nil {
				return err
			}
			changed, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if changed > 0 {
				_, err = tx.AppendEvent(Event{Kind: "room_preference_changed", ProjectID: projectID, RoomID: roomID,
					PayloadJSON: payloadJSON(map[string]string{"user_id": userID})})
			}
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		room, err := tx.CreateRoom(Room{ProjectID: projectID, Kind: "dm", DMKey: key, Title: slug, CreatedBy: userID})
		if err != nil {
			return err
		}
		roomID = room.ID
		if err := tx.AddMember(roomID, "user", userID); err != nil {
			return err
		}
		return tx.AddMember(roomID, "agent", agentID)
	})
	if err != nil {
		return Room{}, err
	}
	return s.RoomByID(ctx, roomID)
}

func (s *Store) UserRoomAccess(ctx context.Context, userID, roomID string) error {
	if err := s.checkTxReentry(ctx); err != nil {
		return err
	}
	var allowed int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM room r
		JOIN membership p ON p.project_id=r.project_id AND p.user_id=?
		WHERE r.id=? AND (r.kind!='dm' OR EXISTS (
			SELECT 1 FROM room_member m WHERE m.room_id=r.id AND m.member_kind='user'
			AND m.member_id=? AND m.left_at IS NULL))`, userID, roomID, userID).Scan(&allowed)
	if err != nil {
		return err
	}
	if allowed == 0 {
		return ErrForbidden
	}
	return nil
}
