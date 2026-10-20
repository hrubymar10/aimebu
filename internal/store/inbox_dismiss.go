package store

import (
	"context"
	"database/sql"
	"errors"
)

// DismissDeliveryFailure removes an informational failure after a permitted
// human has reviewed it. It never changes the held answer or the room.
func (s *Store) DismissDeliveryFailure(ctx context.Context, itemID, userID string, rev int) error {
	return s.Tx(ctx, func(tx *Tx) error {
		var projectID, roomID, agentID, kind, state, role, roomKind string
		var currentRev int
		var dmMember bool
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT i.project_id,i.room_id,COALESCE(i.agent_id,''),i.kind,i.state,i.rev,
			COALESCE(m.role,''),r.kind,EXISTS(SELECT 1 FROM room_member rm WHERE rm.room_id=i.room_id
			AND rm.member_kind='user' AND rm.member_id=? AND rm.left_at IS NULL)
			FROM inbox_item i JOIN room r ON r.id=i.room_id
			LEFT JOIN membership m ON m.project_id=i.project_id AND m.user_id=? WHERE i.id=?`, userID, userID, itemID).
			Scan(&projectID, &roomID, &agentID, &kind, &state, &currentRev, &role, &roomKind, &dmMember)
		if errors.Is(err, sql.ErrNoRows) {
			return sql.ErrNoRows
		}
		if err != nil {
			return err
		}
		if kind != "delivery_failed" || role == "" || roomKind == "dm" && !dmMember || roomKind != "dm" && role != "owner" && role != "operator" {
			return ErrForbidden
		}
		if currentRev != rev {
			return ErrStaleInboxRev
		}
		if state == "resolved" {
			return ErrAlreadyResolved
		}
		if state == "withdrawn" {
			return ErrAlreadyWithdrawn
		}
		result, err := tx.Exec(`UPDATE inbox_item SET state='resolved',rev=rev+1,resolved_by=?,resolved_at=?
			WHERE id=? AND state='open' AND rev=?`, userID, timestamp(), itemID, rev)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return ErrStaleInboxRev
		}
		_, err = tx.AppendEvent(Event{Kind: "inbox_item_resolved", ProjectID: projectID, RoomID: roomID, AgentID: agentID,
			PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": itemID, "choice": "dismiss", "actor": userID})})
		return err
	})
}
