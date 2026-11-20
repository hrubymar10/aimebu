package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/answer"
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

// deliveryFailedInbox raises the card for a held answer. toLead is set only for invalid-answer holds, which a worker's lead settles first;
// room_closed and not_member holds are never re-asked by an agent, so they stay with people. The card records the rooms the held reply meant
// to reach (read back out of the stored candidate), so a later committed reply that reaches one of them settles the card even when it was
// raised in the turn's own room.
func (tx *Tx) deliveryFailedInbox(parent answerTurn, toLead bool) error {
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
	ask := inboxAsk{Text: "An agent reply could not be delivered.", Preview: candidatePreview(saved.CandidateRaw), TargetRooms: candidateTargetRooms(saved.CandidateRaw)}
	leadID := ""
	if toLead {
		var err error
		if leadID, _, _, err = tx.workerRecoveryLead(projectID, parent.roomID, parent.agentID); err != nil {
			return err
		}
	}
	if leadID != "" {
		ask.Phase, ask.Options = answerHeldReason, []string{"Retry", "Abandon"}
	}
	options, err := json.Marshal(ask)
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
	if leadID != "" {
		// A worker's undeliverable answer is the lead's to settle first; people see it only once the lead's interval passes (SweepWorkerRecoveries).
		_, err := tx.handRecoveryToLead(id, projectID, parent.roomID, parent.agentID, parent.id, answerHeldReason)
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

// candidateTargetRooms lists, in order and without repeats, the rooms the stored candidate of a held reply named: what a later committed
// reply must reach to settle the reply's delivery-failure card. A candidate that does not parse (the reply was held for its shape before
// any target could be read) names none, and its card then settles only through the turn and the turn's room.
func candidateTargetRooms(raw string) []string {
	parsed, err := answer.Parse([]byte(raw), false)
	if err != nil {
		return nil
	}
	var seen = make(map[string]bool, len(parsed.Messages))
	var rooms []string
	for _, message := range parsed.Messages {
		if message.Room != "" && !seen[message.Room] {
			seen[message.Room] = true
			rooms = append(rooms, message.Room)
		}
	}
	return rooms
}

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
// committed a reply: the failures of the same parent turn, any earlier ones in
// the rooms the committed reply posted to, and any earlier ones whose recorded
// intended target rooms the committed reply reached — the successful resend of
// a held reply settles its card even when the card was raised in the turn's
// room while the reply was meant for another one.
func (tx *Tx) resolveDeliveryFailures(agentID, turnID string, roomIDs ...string) error {
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT id,project_id,room_id,COALESCE(turn_id,''),COALESCE(options_json,'') FROM inbox_item
		WHERE agent_id=? AND kind='delivery_failed' AND state='open'`, agentID)
	if err != nil {
		return err
	}
	type item struct{ id, projectID, roomID, turnID, options string }
	items := []item{}
	for rows.Next() {
		var x item
		if err := rows.Scan(&x.id, &x.projectID, &x.roomID, &x.turnID, &x.options); err != nil {
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
	posted := make(map[string]bool, len(roomIDs))
	for _, id := range roomIDs {
		posted[id] = true
	}
	for _, x := range items {
		if !deliveryFailureSettled(x.turnID, x.roomID, x.options, turnID, posted) {
			continue
		}
		if _, err := tx.Exec(`UPDATE inbox_item SET state='resolved',rev=rev+1,resolved_at=? WHERE id=?`, timestamp(), x.id); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(Event{Kind: "inbox_item_resolved", ProjectID: x.projectID, RoomID: x.roomID, AgentID: agentID, PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": x.id})}); err != nil {
			return err
		}
	}
	return nil
}

// deliveryFailureSettled says whether a committed reply settles one open delivery-failure card: the card's own turn, the card's room, or a
// room the card recorded as the held reply's target (options of items stored before the target was kept name none and match the old ways only).
func deliveryFailureSettled(itemTurnID, itemRoomID, options, turnID string, posted map[string]bool) bool {
	if itemTurnID == turnID || posted[itemRoomID] {
		return true
	}
	var ask inboxAsk
	if options != "" {
		if err := json.Unmarshal([]byte(options), &ask); err != nil {
			return false
		}
	}
	for _, room := range ask.TargetRooms {
		if posted[room] {
			return true
		}
	}
	return false
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

// repeatedEmptyAnswer says whether the agent's two latest answers were both empty (this turn's, already stored, and the one before) with no
// tool activity in either turn: the session answers with nothing, and another correction note would only repeat it.
func (tx *Tx) repeatedEmptyAnswer(parent answerTurn) (bool, error) {
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT r.turn_id,COALESCE(json_extract(r.result_json,'$.candidate_raw'),'') FROM receipt r JOIN turn t ON t.id=r.turn_id
		WHERE t.agent_id=? AND r.answer_rev=0 AND r.turn_id<>? ORDER BY r.created_at DESC,r.id DESC LIMIT 1`, parent.agentID, parent.id)
	if err != nil {
		return false, err
	}
	var prevTurn, prevRaw string
	found := false
	for rows.Next() {
		found = true
		if err := rows.Scan(&prevTurn, &prevRaw); err != nil {
			rows.Close()
			return false, err
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil || !found || strings.TrimSpace(prevRaw) != "" {
		return false, err
	}
	var active bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM event WHERE turn_id IN (?,?) AND kind='supervisor_activity'
		AND COALESCE(json_extract(payload_json,'$.summary'),'') NOT IN ('turn.started','system','rate_limit_event',''))`, parent.id, prevTurn).Scan(&active); err != nil {
		return false, err
	}
	return !active, nil
}

// brokenSessionRecovery raises one recovery for the worker's lead: the session answers with nothing, restart it with a fresh session. A
// worker with no lead to hand to gets a delivery_failed card for people instead.
func (tx *Tx) brokenSessionRecovery(parent answerTurn) error {
	var open bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM inbox_item WHERE agent_id=? AND state='open' AND kind='recovery' AND json_extract(options_json,'$.phase')=?)`, parent.agentID, brokenSessionReason).Scan(&open); err != nil || open {
		return err
	}
	var projectID string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id FROM agent WHERE id=?`, parent.agentID).Scan(&projectID); err != nil {
		return err
	}
	leadID, _, _, err := tx.workerRecoveryLead(projectID, parent.roomID, parent.agentID)
	if err != nil {
		return err
	}
	if leadID == "" {
		return tx.deliveryFailedInbox(parent, false)
	}
	options, err := json.Marshal(inboxAsk{Text: "The agent answered twice in a row with an empty answer and no tool activity: its session looks broken.",
		Options: []string{"Restart", "Abandon"}, Phase: brokenSessionReason})
	if err != nil {
		return err
	}
	id, err := newID("inb")
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO inbox_item(id,project_id,room_id,turn_id,agent_id,requester_kind,requester_id,kind,options_json,created_at)
		VALUES (?,?,?,?,?,'agent',?,'recovery',?,?)`, id, projectID, parent.roomID, parent.id, parent.agentID, parent.agentID, string(options), timestamp()); err != nil {
		return err
	}
	_, err = tx.handRecoveryToLead(id, projectID, parent.roomID, parent.agentID, parent.id, brokenSessionReason)
	return err
}

// TurnExitEvidence returns the harness exit code and the last lines of its stderr that the turn's latest process-exit report carried, or
// "unknown" and an empty tail while nothing has been reported yet (a warm process answers before it exits).
func (s *Store) TurnExitEvidence(ctx context.Context, turnID string) (exitCode, stderrTail string, err error) {
	var code, evidence sql.NullString
	err = s.readDB.QueryRowContext(ctx, `SELECT json_extract(payload_json,'$.exit_code'),json_extract(payload_json,'$.evidence') FROM event
		WHERE turn_id=? AND kind='turn_process_exit' ORDER BY seq DESC LIMIT 1`, turnID).Scan(&code, &evidence)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "unknown", "", err
	}
	if !code.Valid {
		return "unknown", "", nil
	}
	lines := strings.Split(strings.TrimSpace(evidence.String), "\n")
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	return code.String, strings.Join(lines, "\n"), nil
}
