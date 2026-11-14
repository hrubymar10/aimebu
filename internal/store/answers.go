package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/answer"
)

type AnswerInput struct {
	TurnID, AgentID, Digest string
	Generation              int
	AnswerRev               int
	Candidate               []byte
	Parsed                  *answer.Candidate
	ParseError              string
}

type AnswerReceipt struct {
	TurnID     string          `json:"turn_id"`
	Digest     string          `json:"digest"`
	Result     string          `json:"result"`
	HeldReason string          `json:"held_reason,omitempty"`
	Error      string          `json:"error,omitempty"`
	AnswerRev  int             `json:"answer_rev"`
	MessageIDs []string        `json:"message_ids,omitempty"`
	Warnings   []string        `json:"warnings,omitempty"`
	Additional []AnswerReceipt `json:"-"`
}

type answerTurn struct {
	id, parentID, agentID, sessionID, roomID, mode, exec, policyJSON string
	generation, attempt                                              int
	terminal, blocked                                                bool
}

// ApplyAnswer owns the answer's commit and receipt in one server transaction.
// Parsing is pure and happens before this call; mutable authorisation is
// checked again inside the transaction immediately before message insertion.
func (s *Store) ApplyAnswer(ctx context.Context, input AnswerInput) (AnswerReceipt, error) {
	var receipt AnswerReceipt
	err := s.Tx(ctx, func(tx *Tx) error {
		turn, err := tx.answerTurn(input.TurnID)
		if err != nil {
			return err
		}
		if turn.agentID != input.AgentID {
			return ErrForbidden
		}
		if turn.generation != input.Generation {
			return ErrStaleGeneration
		}
		var oldDigest, oldJSON string
		lookup := tx.tx.QueryRowContext(tx.ctx, `SELECT digest,result_json FROM receipt WHERE turn_id=? AND answer_rev=?`, input.TurnID, input.AnswerRev).Scan(&oldDigest, &oldJSON)
		if lookup == nil {
			if oldDigest != input.Digest {
				return ErrConflict
			}
			return json.Unmarshal([]byte(oldJSON), &receipt)
		}
		if !errors.Is(lookup, sql.ErrNoRows) {
			return lookup
		}
		if turn.mode == "correction" && (turn.parentID == "" || input.AnswerRev != 1) || turn.mode != "correction" && input.AnswerRev != 0 {
			return ErrInvalidTransition
		}
		parent := turn
		if turn.mode == "correction" {
			parent, err = tx.answerTurn(turn.parentID)
			if err != nil {
				return err
			}
			if parent.agentID != turn.agentID || parent.generation != turn.generation || parent.mode != "normal" {
				return ErrScopeMismatch
			}
		}
		receipt = AnswerReceipt{TurnID: input.TurnID, AnswerRev: input.AnswerRev, Digest: input.Digest, MessageIDs: []string{}}
		if input.Parsed != nil {
			receipt.Warnings = input.Parsed.Warnings
		}
		if handled, err := tx.applySwitchHandoffAnswer(turn, input, &receipt); err != nil {
			return err
		} else if handled {
			if err := tx.settleAnswerTurn(turn); err != nil {
				return err
			}
			return tx.insertAnswerReceipt(receipt, input.Candidate)
		}
		missedMentionHeld := false
		if turn.mode == "diagnostic" {
			if input.ParseError != "" || input.Parsed == nil {
				detail := input.ParseError
				if detail == "" {
					detail = "missing parsed diagnostic answer"
				}
				receipt.Result, receipt.HeldReason, receipt.Error = "held", "correction_unavailable", detail
			} else {
				report, err := json.Marshal(input.Parsed)
				if err != nil {
					return err
				}
				if _, err := tx.Exec(`UPDATE turn SET report_json=? WHERE id=?`, string(report), turn.id); err != nil {
					return err
				}
				receipt.Result = "committed"
			}
		} else {
			if input.Parsed != nil {
				if err := tx.resolveMessageRefs(input.Parsed.Messages); err != nil {
					return err
				}
			}
			reason, detail, err := tx.validateAnswer(parent, input, &receipt.Warnings)
			if err != nil {
				return err
			}
			if reason == "" {
				receipt.Result = "committed"
				if err := tx.commitAnswerMessages(parent, input, &receipt); err != nil {
					return err
				}
			} else if reason == "room_closed" || reason == "not_member" {
				receipt.Result, receipt.HeldReason, receipt.Error = "held", reason, detail
			} else if reason == "missed_mention" && turn.mode == "normal" && !tx.noActionMode(parent.policyJSON) {
				receipt.Result, receipt.HeldReason, receipt.Error = "held", "correction_unavailable", detail
				missedMentionHeld = true
			} else if turn.mode == "normal" && tx.noActionMode(parent.policyJSON) {
				receipt.Result, receipt.Error = "correction_requested", detail
			} else {
				receipt.Result, receipt.Error = "held", detail
				if turn.mode == "correction" {
					receipt.HeldReason = "invalid_after_correction"
				} else {
					receipt.HeldReason = "correction_unavailable"
				}
			}
			if receipt.Result != "committed" {
				if err := tx.recordNonCommittedOutbox(parent, input, receipt); err != nil {
					return err
				}
			}
		}
		if receipt.Result != "correction_requested" {
			if err := tx.settleAnswerTurn(turn); err != nil {
				return err
			}
			if turn.mode == "correction" {
				if err := tx.settleAnswerTurn(parent); err != nil {
					return err
				}
				if err := tx.supersedeParentReceipt(parent.id, &receipt); err != nil {
					return err
				}
			}
		}
		if err := tx.insertAnswerReceipt(receipt, input.Candidate); err != nil {
			return err
		}
		if receipt.Result == "committed" {
			if err := tx.resolveDeliveryFailures(turn.agentID, parent.id); err != nil {
				return err
			}
		}
		if receipt.Result == "correction_requested" && turn.exec != "pending" && turn.exec != "running" {
			if err := tx.ensureCorrectionTurn(turn); err != nil {
				return err
			}
		}
		if receipt.Result == "held" && turn.mode != "diagnostic" {
			var err error
			if missedMentionHeld {
				err = tx.missedMentionNote(parent, input, receipt.Error)
			} else {
				err = tx.deliveryFailedInbox(parent)
			}
			if err != nil {
				return err
			}
		}
		if turn.mode == "diagnostic" && turn.exec != "pending" && turn.exec != "running" {
			if err := tx.completeDiagnostic(turn.id); err != nil {
				return err
			}
		}
		return nil
	})
	return receipt, err
}

func (tx *Tx) answerTurn(id string) (answerTurn, error) {
	var turn answerTurn
	var parent, policy sql.NullString
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT t.id,t.parent_turn_id,t.agent_id,t.session_id,t.room_id,t.mode,t.exec,t.generation,t.attempt,t.terminal_event,a.recovery_block,t.policy_json FROM turn t JOIN agent a ON a.id=t.agent_id WHERE t.id=?`, id).
		Scan(&turn.id, &parent, &turn.agentID, &turn.sessionID, &turn.roomID, &turn.mode, &turn.exec, &turn.generation, &turn.attempt, &turn.terminal, &turn.blocked, &policy)
	turn.parentID, turn.policyJSON = parent.String, policy.String
	return turn, err
}

func (tx *Tx) noActionMode(raw string) bool {
	var policy TurnPolicy
	return json.Unmarshal([]byte(raw), &policy) == nil && policy.Capabilities.NoActionMode
}

func (tx *Tx) validateAnswer(turn answerTurn, input AnswerInput, warnings *[]string) (reason, detail string, dbErr error) {
	if input.ParseError != "" || input.Parsed == nil {
		return "invalid_answer", input.ParseError, nil
	}
	seenAttachments := make(map[string]bool)
	var turnRoomKind, turnRoomProject string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT kind,project_id FROM room WHERE id=?`, turn.roomID).Scan(&turnRoomKind, &turnRoomProject); err != nil {
		return "", "", err
	}
	// A chat between two agents belongs to no project, so its turn may also report into the agent's own project (a lead woken by the
	// peer's reply has to tell its people). A chat with a person stays closed to everything else.
	agentChat := turnRoomKind == "dm" && turnRoomProject == AgentDMProjectID
	for i, item := range input.Parsed.Messages {
		if turnRoomKind == "dm" && item.Room != turn.roomID && !agentChat {
			return "not_member", "private chat replies must stay in that chat", nil
		}
		var projectID, lifecycle, itemKind string
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,lifecycle,kind FROM room WHERE id=?`, item.Room).Scan(&projectID, &lifecycle, &itemKind)
		if errors.Is(err, sql.ErrNoRows) {
			return "invalid_answer", fmt.Sprintf("message %d targets an unknown room", i), nil
		}
		if err != nil {
			return "", "", err
		}
		var agentProject string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id FROM agent WHERE id=?`, turn.agentID).Scan(&agentProject); err != nil {
			return "", "", err
		}
		if projectID != agentProject && !(agentChat && item.Room == turn.roomID) {
			return "not_member", "room is outside the agent project", nil
		}
		if agentChat && item.Room != turn.roomID && itemKind == "dm" {
			return "not_member", "a chat between agents cannot report into another private chat", nil
		}
		if lifecycle != "open" {
			return "room_closed", "room is closing or closed", nil
		}
		var member int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL`, item.Room, turn.agentID).Scan(&member); err != nil {
			return "", "", err
		}
		if member != 1 {
			return "not_member", "agent is not an active room member", nil
		}
		if !item.Force {
			miss, warning, err := tx.missedAnswerMention(item.Room, projectID, turn.agentID, item.Body)
			if err != nil {
				return "", "", err
			}
			if warning != "" {
				*warnings = append(*warnings, fmt.Sprintf("message %d: %s", i, warning))
			}
			if miss != "" {
				return "missed_mention", fmt.Sprintf("message %d: %s", i, miss), nil
			}
		}
		if item.Ask != nil || item.NeedsAttention || len(item.ProposedAnswers) > 0 || len(item.OpenQuestions) > 0 {
			var recipients int
			query := `SELECT COUNT(*) FROM membership WHERE project_id=? AND role IN ('owner','operator')`
			arg := projectID
			var err error
			switch {
			case agentChat && item.Room == turn.roomID:
				// The Inbox shows an agent's question to the owners and operators of the agent's own project and an approval to its owners, so only
				// someone in the chat who is one of them can answer it.
				roles := `'owner','operator'`
				if item.Ask != nil && item.Ask.Kind == "approval" {
					roles = `'owner'`
				}
				err = tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM room_member rm JOIN membership mb ON mb.project_id=? AND mb.user_id=rm.member_id
					WHERE rm.room_id=? AND rm.member_kind='user' AND rm.left_at IS NULL AND mb.role IN (`+roles+`)`, agentProject, turn.roomID).Scan(&recipients)
			case turnRoomKind == "dm" && item.Room == turn.roomID:
				err = tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_kind='user' AND left_at IS NULL`, turn.roomID).Scan(&recipients)
			default:
				err = tx.tx.QueryRowContext(tx.ctx, query, arg).Scan(&recipients)
			}
			if err != nil {
				return "", "", err
			}
			if recipients == 0 {
				return "invalid_answer", "ask has no authorized recipient", nil
			}
		}
		for _, ref := range []string{item.ReplyTo} {
			if ref == "" {
				continue
			}
			var refRoom string
			err := tx.tx.QueryRowContext(tx.ctx, `SELECT room_id FROM message WHERE id=?`, ref).Scan(&refRoom)
			if errors.Is(err, sql.ErrNoRows) || err == nil && refRoom != item.Room {
				return "invalid_answer", fmt.Sprintf("message %d has an invalid reply or thread reference", i), nil
			}
			if err != nil {
				return "", "", err
			}
		}
		for _, attachment := range item.Attachments {
			if seenAttachments[attachment] {
				return "invalid_answer", fmt.Sprintf("message %d repeats an attachment", i), nil
			}
			seenAttachments[attachment] = true
			var attachmentProject, attachmentRoom string
			var attached sql.NullString
			err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,COALESCE(room_id,''),message_id FROM attachment WHERE id=?`, attachment).Scan(&attachmentProject, &attachmentRoom, &attached)
			if errors.Is(err, sql.ErrNoRows) || err == nil && (attachmentProject != projectID || attachmentRoom != "" && attachmentRoom != item.Room || attached.Valid) {
				return "invalid_answer", fmt.Sprintf("message %d has an invalid attachment", i), nil
			}
			if err != nil {
				return "", "", err
			}
		}
	}
	return "", "", nil
}

// missedAnswerMention only considers live prose and active agents of the
// message's project. Human names, role keys and group tags retain their own
// addressing rules.
func (tx *Tx) missedAnswerMention(roomID, projectID, authorID, body string) (miss, warning string, dbErr error) {
	type absentMention struct{ token, id string }
	var absent []absentMention
	addressed := false
	for _, token := range proseMentions(body) {
		if groupTag(token) != "" {
			continue
		}
		var id, slug string
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.id,a.slug FROM agent a JOIN project p ON p.id=a.project_id
			WHERE a.project_id=? AND a.revoked_at IS NULL AND
			(LOWER(a.id)=? OR LOWER(a.slug)=? OR LOWER(a.slug || '@' || p.name)=?)`,
			projectID, token, token, token).Scan(&id, &slug)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return "", "", err
		}
		var member bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL)`, roomID, id).Scan(&member); err != nil {
			return "", "", err
		}
		if member {
			addressed = true
			continue
		}
		absent = append(absent, absentMention{token, id})
	}
	if len(absent) == 0 {
		return "", "", nil
	}
	if addressed {
		return "", fmt.Sprintf("@%s is outside the target room and was not notified", absent[0].token), nil
	}
	missing := absent[0]
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT r.title,r.id FROM room_member m JOIN room r ON r.id=m.room_id
			WHERE m.member_kind='agent' AND m.member_id=? AND m.left_at IS NULL AND r.project_id=?
			AND r.lifecycle='open' AND r.kind='task' AND (r.restricted=0 OR EXISTS (
				SELECT 1 FROM room_member own WHERE own.room_id=r.id AND own.member_kind='agent'
				AND own.member_id=? AND own.left_at IS NULL))
			ORDER BY r.created_at DESC LIMIT 3`, missing.id, projectID, authorID)
	if err != nil {
		return "", "", err
	}
	var rooms []string
	for rows.Next() {
		var title, room string
		if err := rows.Scan(&title, &room); err != nil {
			rows.Close()
			return "", "", err
		}
		rooms = append(rooms, fmt.Sprintf("%s (%s)", title, room))
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return "", "", err
	}
	location := "no open task room"
	if len(rooms) > 0 {
		location = "task room " + strings.Join(rooms, ", ")
	}
	return fmt.Sprintf("@%s refers to agent %s, who is not in the target room; send the message in %s or use dm_agent, or repeat this message with force:true", missing.token, missing.id, location), "", nil
}

func (tx *Tx) missedMentionNote(turn answerTurn, input AnswerInput, detail string) error {
	var index int
	if _, err := fmt.Sscanf(detail, "message %d:", &index); err != nil {
		return err
	}
	var outboxID string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM outbox WHERE turn_id=? AND answer_rev=? AND idx=?`, turn.id, input.AnswerRev, index).Scan(&outboxID); err != nil {
		return err
	}
	body := "Your final answer was held because message " + outboxID + " mentions an agent outside its room and addresses no room member; send it to a shared room or retry with force:true."
	_, err := tx.InsertMessage(Message{RoomID: turn.roomID, AuthorKind: "system", AuthorID: "system", Body: body, OnlyTargets: []string{turn.agentID}})
	return err
}
