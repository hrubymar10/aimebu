package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"

	"github.com/goccy/go-json"
)

var ErrAlreadyResolved = errors.New("already_resolved")
var ErrAlreadyWithdrawn = errors.New("already_withdrawn")
var ErrStaleInboxRev = errors.New("stale_rev")

// AnswerInbox writes the reply, requester wake note, inbox transition and
// event in one transaction. The request key protects retries after a lost
// HTTP response; a replay returns the original message.
func (s *Store) AnswerInbox(ctx context.Context, itemID, userID, key, body string, rev int) (Message, bool, error) {
	digest := sha256.Sum256([]byte(itemID + "\x00" + body + "\x00" + strconv.Itoa(rev)))
	result, reused, err := s.WithRequestKey(ctx, RequestKey{ActorKind: "user", ActorID: userID, Kind: "inbox_answer", Key: key, Digest: hex.EncodeToString(digest[:])}, func(tx *Tx) (string, error) {
		var projectID, roomID, requesterKind, requesterID, kind, state string
		var askMessage sql.NullString
		var currentRev int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,room_id,message_id,requester_kind,requester_id,kind,state,rev FROM inbox_item WHERE id=?`, itemID).
			Scan(&projectID, &roomID, &askMessage, &requesterKind, &requesterID, &kind, &state, &currentRev); err != nil {
			return "", err
		}
		// Who is asking comes first: a person outside the item's project learns nothing of it, not that it exists, was resolved or
		// withdrawn, or what its revision is.
		var role string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT role FROM membership WHERE project_id=? AND user_id=?`, projectID, userID).Scan(&role); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return "", ErrForbidden
			}
			return "", err
		}
		if state == "resolved" {
			return "", ErrAlreadyResolved
		}
		if state == "withdrawn" {
			// Structured choices (proposed answers, Open Questions) wait for their answer however the card left the Inbox (dismissed, withdrawn by
			// a restart); an answer is only a reply, so it resolves the card like one on a superseded ask, while its room is open (checked below)
			// and its agent exists. Any other withdrawn item stays final.
			var again bool
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM inbox_item i JOIN message ask ON ask.id=i.message_id
				WHERE i.id=? AND `+reanswerableSQL+`)`, itemID).Scan(&again); err != nil {
				return "", err
			}
			if !again {
				return "", ErrAlreadyWithdrawn
			}
		}
		if currentRev != rev {
			return "", ErrStaleInboxRev
		}
		if kind != "question" && kind != "approval" {
			return "", ErrForbidden
		}
		var boundary bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM boundary_pending WHERE inbox_id=?)`, itemID).Scan(&boundary); err != nil {
			return "", err
		}
		if boundary {
			return "", ErrForbidden
		}
		var roomKind, roomProject string
		var privateAllowed int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT r.kind,r.project_id,EXISTS
			(SELECT 1 FROM room_member own WHERE own.room_id=r.id AND own.member_kind='user'
			AND own.member_id=? AND own.left_at IS NULL) FROM room r WHERE r.id=?`, userID, roomID).Scan(&roomKind, &roomProject, &privateAllowed); err != nil {
			return "", err
		}
		// A private chat with a person answers to that person at any role. A chat between agents is a member-only room whose questions
		// follow the roles of the asking agent's project, as in any room: owners and operators answer, owners approve.
		privateChat := roomKind == "dm" && roomProject != AgentDMProjectID
		if kind == "approval" && !privateChat && role != "owner" || roomKind == "dm" && privateAllowed == 0 || !privateChat && role != "owner" && role != "operator" {
			return "", ErrForbidden
		}
		var lifecycle string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT lifecycle FROM room WHERE id=?`, roomID).Scan(&lifecycle); err != nil {
			return "", err
		}
		if lifecycle != "open" {
			return "", ErrInvalidTransition
		}
		// Close the item before writing the answer so that the answer itself cannot supersede its ask. A superseded ask is not over: it
		// left the Inbox only, and the answer resolves it here like an open one (so does a withdrawn card with structured choices).
		if _, err := tx.Exec(`UPDATE inbox_item SET state='resolved',rev=rev+1,resolved_by=?,resolved_at=? WHERE id=? AND state IN ('open','superseded','withdrawn') AND rev=?`, userID, timestamp(), itemID, rev); err != nil {
			return "", err
		}
		// The answer replies to the ask, so it joins the ask's conversation (its thread_root is derived from that).
		message, err := tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "user", AuthorID: userID, Body: body, ReplyTo: askMessage.String, OnlyTargets: []string{requesterID}})
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(`UPDATE inbox_item SET answer_message_id=? WHERE id=?`, message.ID, itemID); err != nil {
			return "", err
		}
		if _, err := tx.AppendEvent(Event{Kind: "inbox_item_resolved", ProjectID: projectID, RoomID: roomID, PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": itemID, "message_id": message.ID})}); err != nil {
			return "", err
		}
		data, err := json.Marshal(message)
		return string(data), err
	})
	if err != nil {
		return Message{}, false, err
	}
	var message Message
	if err := json.Unmarshal([]byte(result), &message); err != nil {
		return Message{}, false, err
	}
	return message, reused, nil
}
