package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// DismissInboxItem closes an ordinary request without answering it, or
// removes an informational delivery failure. Policy approvals remain decisions.
func (s *Store) DismissInboxItem(ctx context.Context, itemID, userID string, rev int) error {
	return s.Tx(ctx, func(tx *Tx) error {
		var projectID, roomID, agentID, kind, state, role, roomKind, roomProject, requesterID string
		var currentRev int
		var dmMember, boundary bool
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT i.project_id,i.room_id,COALESCE(i.agent_id,''),i.kind,i.state,i.rev,i.requester_id,
			COALESCE(m.role,''),r.kind,r.project_id,EXISTS(SELECT 1 FROM room_member rm WHERE rm.room_id=i.room_id
			AND rm.member_kind='user' AND rm.member_id=? AND rm.left_at IS NULL),
			EXISTS(SELECT 1 FROM boundary_pending b WHERE b.inbox_id=i.id)
			FROM inbox_item i JOIN room r ON r.id=i.room_id
			LEFT JOIN membership m ON m.project_id=i.project_id AND m.user_id=? WHERE i.id=?`, userID, userID, itemID).
			Scan(&projectID, &roomID, &agentID, &kind, &state, &currentRev, &requesterID, &role, &roomKind, &roomProject, &dmMember, &boundary)
		if errors.Is(err, sql.ErrNoRows) {
			return sql.ErrNoRows
		}
		if err != nil {
			return err
		}
		// A private chat with a person answers to that person at any role; a chat between agents also asks for the role in the agent's project.
		privateChat := roomKind == "dm" && roomProject != AgentDMProjectID
		allowedKind := kind == "delivery_failed" || kind == "question" || kind == "approval" && !boundary && !strings.HasPrefix(requesterID, "note:")
		if !allowedKind || role == "" || roomKind == "dm" && !dmMember || !privateChat && role != "owner" && role != "operator" || kind == "approval" && !privateChat && role != "owner" {
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
		nextState := "withdrawn"
		if kind == "delivery_failed" {
			nextState = "resolved"
		}
		result, err := tx.Exec(`UPDATE inbox_item SET state=?,rev=rev+1,resolved_by=?,resolved_at=?
			WHERE id=? AND state='open' AND rev=?`, nextState, userID, timestamp(), itemID, rev)
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
