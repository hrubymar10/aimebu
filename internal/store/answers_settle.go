package store

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/goccy/go-json"
)

func (tx *Tx) insertAnswerReceipt(receipt AnswerReceipt, raw []byte) error {
	id, err := newID("rcv")
	if err != nil {
		return err
	}
	stored := receipt
	stored.Additional = nil
	data, err := json.Marshal(struct {
		AnswerReceipt
		CandidateRaw string `json:"candidate_raw"`
	}{stored, string(raw)})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO receipt(id,turn_id,answer_rev,digest,result_json,created_at) VALUES (?,?,?,?,?,?)`, id,
		receipt.TurnID, receipt.AnswerRev, receipt.Digest, string(data), timestamp()); err != nil {
		return err
	}
	var projectID, roomID, agentID string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.project_id,t.room_id,t.agent_id FROM turn t JOIN agent a ON a.id=t.agent_id WHERE t.id=?`, receipt.TurnID).Scan(&projectID, &roomID, &agentID); err != nil {
		return err
	}
	_, err = tx.AppendEvent(Event{Kind: "answer_" + receipt.Result, ProjectID: projectID, RoomID: roomID, AgentID: agentID, TurnID: receipt.TurnID,
		PayloadJSON: payloadJSON(map[string]string{"result": receipt.Result, "held_reason": receipt.HeldReason})})
	return err
}

func (tx *Tx) settleAnswerTurn(turn answerTurn) error {
	var settle string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT settle FROM turn WHERE id=?`, turn.id).Scan(&settle); err != nil {
		return err
	}
	if settle == "settled" {
		return nil
	}
	if err := TurnSettleTransitions.Transition(settle, "settled"); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE turn SET settle='settled' WHERE id=?`, turn.id); err != nil {
		return err
	}
	if turn.terminal && (turn.exec == "exited" || turn.exec == "killed") {
		return tx.finishCompletedTurn(turn.id, turn.agentID)
	}
	return nil
}

func (tx *Tx) supersedeParentReceipt(parentID string, child *AnswerReceipt) error {
	var digest, data string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT digest,result_json FROM receipt WHERE turn_id=? AND answer_rev=0`, parentID).Scan(&digest, &data); err != nil {
		return err
	}
	var stored struct {
		AnswerReceipt
		CandidateRaw string `json:"candidate_raw"`
	}
	if err := json.Unmarshal([]byte(data), &stored); err != nil {
		return err
	}
	if stored.Result != "correction_requested" {
		return ErrInvalidTransition
	}
	stored.Result = "superseded"
	stored.Error = ""
	updated, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE receipt SET result_json=? WHERE turn_id=? AND answer_rev=0`, string(updated), parentID); err != nil {
		return err
	}
	child.Additional = append(child.Additional, AnswerReceipt{TurnID: parentID, AnswerRev: 0, Digest: digest, Result: "superseded"})
	return nil
}

func (tx *Tx) ensureCorrectionTurn(parent answerTurn) error {
	if !parent.terminal || parent.exec == "pending" || parent.exec == "running" {
		return nil
	}
	var existing string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM turn WHERE parent_turn_id=? AND mode='correction'`, parent.id).Scan(&existing)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.RecordTurn(Turn{AgentID: parent.agentID, SessionID: parent.sessionID, Generation: parent.generation,
		RoomID: parent.roomID, ParentTurnID: parent.id, Attempt: parent.attempt + 1, Mode: "correction", Exec: "pending", PolicyJSON: parent.policyJSON})
	return err
}

func (tx *Tx) deliveryFailedInbox(parent answerTurn) error {
	var projectID string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id FROM agent WHERE id=?`, parent.agentID).Scan(&projectID); err != nil {
		return err
	}
	var already bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM inbox_item WHERE turn_id=? AND kind='delivery_failed')`, parent.id).Scan(&already); err != nil {
		return err
	}
	if already {
		return nil
	}
	var raw string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT result_json FROM receipt WHERE turn_id=? OR turn_id IN
		(SELECT id FROM turn WHERE parent_turn_id=? AND mode='correction') ORDER BY created_at DESC,id DESC LIMIT 1`, parent.id, parent.id).Scan(&raw); err != nil {
		return err
	}
	var saved struct {
		AnswerReceipt
		CandidateRaw string `json:"candidate_raw"`
	}
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		return err
	}
	options, err := json.Marshal(inboxAsk{Text: "An agent reply could not be delivered.", Preview: candidatePreview(saved.CandidateRaw)})
	if err != nil {
		return err
	}
	id, err := newID("inb")
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO inbox_item(id,project_id,room_id,turn_id,agent_id,requester_kind,requester_id,kind,options_json,created_at)
		VALUES (?,?,?,?,?,'agent',?,'delivery_failed',?,?)`, id, projectID, parent.roomID, parent.id, parent.agentID, parent.agentID, string(options), timestamp()); err != nil {
		return err
	}
	if _, err := tx.AppendEvent(Event{Kind: "inbox_item_created", ProjectID: projectID, RoomID: parent.roomID, AgentID: parent.agentID, TurnID: parent.id,
		PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": id, "kind": "delivery_failed"})}); err != nil {
		return err
	}
	var lifecycle string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT lifecycle FROM room WHERE id=?`, parent.roomID).Scan(&lifecycle); err != nil {
		return err
	}
	if lifecycle == "open" {
		actor, err := tx.agentDisplay(parent.agentID)
		if err != nil {
			return err
		}
		_, err = tx.InsertMessage(Message{RoomID: parent.roomID, AuthorKind: "system", AuthorID: "system",
			Body: fmt.Sprintf("%s reply was held (%s). Review the delivery failure in Inbox.", actor, saved.HeldReason)})
		if err != nil {
			return err
		}
	}
	return nil
}

var previewAgentKey = regexp.MustCompile(`aak_[A-Za-z0-9_-]+`)
var previewBearer = regexp.MustCompile(`(?i)Bearer[[:space:]]+[^[:space:]"']+`)
var previewAPIKey = regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}`)
var previewSecretField = regexp.MustCompile(`(?i)("(?:api_key|access_token|refresh_token|password|secret|token)"[[:space:]]*:[[:space:]]*")[^"]*(")`)

func candidatePreview(raw string) string {
	redacted := strings.ToValidUTF8(raw, "�")
	redacted = previewAgentKey.ReplaceAllString(redacted, "[redacted]")
	redacted = previewBearer.ReplaceAllString(redacted, "Bearer [redacted]")
	redacted = previewAPIKey.ReplaceAllString(redacted, "[redacted]")
	redacted = previewSecretField.ReplaceAllString(redacted, "${1}[redacted]${2}")
	collapsed := strings.Join(strings.Fields(redacted), " ")
	runes := []rune(collapsed)
	if len(runes) > 240 {
		return string(runes[:240]) + "…"
	}
	return collapsed
}

// resolveDeliveryFailures closes the agent's open delivery failures once it has
// committed a reply: the failures of the same parent turn, and any earlier ones
// in the rooms the committed reply posted to.
func (tx *Tx) resolveDeliveryFailures(agentID, turnID string, roomIDs ...string) error {
	query := `SELECT id,project_id,room_id FROM inbox_item WHERE agent_id=? AND kind='delivery_failed' AND state='open' AND (turn_id=?`
	args := []any{agentID, turnID}
	for _, id := range roomIDs {
		query += ` OR room_id=?`
		args = append(args, id)
	}
	rows, err := tx.tx.QueryContext(tx.ctx, query+`)`, args...)
	if err != nil {
		return err
	}
	type item struct{ id, projectID, roomID string }
	items := []item{}
	for rows.Next() {
		var x item
		if err := rows.Scan(&x.id, &x.projectID, &x.roomID); err != nil {
			rows.Close()
			return err
		}
		items = append(items, x)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, x := range items {
		if _, err := tx.Exec(`UPDATE inbox_item SET state='resolved',rev=rev+1,resolved_at=? WHERE id=?`, timestamp(), x.id); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(Event{Kind: "inbox_item_resolved", ProjectID: x.projectID, RoomID: x.roomID, AgentID: agentID, PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": x.id})}); err != nil {
			return err
		}
	}
	return nil
}

// partialHeldText says which messages of a reply were held alone, for the Inbox card and the room's system row.
func partialHeldText(held []HeldMessage, total int) string {
	parts := make([]string, 0, len(held))
	for _, h := range held {
		parts = append(parts, fmt.Sprintf("message %d of %d (%s: %s)", h.Index+1, total, h.Reason, h.Error))
	}
	delivered := total - len(held)
	return fmt.Sprintf("%s held; the other %d delivered", strings.Join(parts, ", "), delivered)
}

// partialHeldRow says in the originating room, when it is open, that only part of a reply was held and which messages. It raises no Inbox item:
// the author gets a note with the held text and is the one to resend it.
func (tx *Tx) partialHeldRow(parent answerTurn, input AnswerInput, receipt AnswerReceipt) error {
	var lifecycle string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT lifecycle FROM room WHERE id=?`, parent.roomID).Scan(&lifecycle); err != nil || lifecycle != "open" {
		return err
	}
	actor, err := tx.agentDisplay(parent.agentID)
	if err != nil {
		return err
	}
	_, err = tx.InsertMessage(Message{RoomID: parent.roomID, AuthorKind: "system", AuthorID: "system",
		Body: fmt.Sprintf("%s reply was only partly delivered: %s. The author was asked to resend it.", actor, partialHeldText(receipt.HeldMessages, len(input.Parsed.Messages)))})
	return err
}
