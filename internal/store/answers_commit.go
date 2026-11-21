package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/answer"
)

func (tx *Tx) insertOutbox(turn answerTurn, input AnswerInput, index int, item answer.Message, state, heldReason string) (string, error) {
	id, err := newID("out")
	if err != nil {
		return "", err
	}
	roomID := item.Room
	var found int
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT 1 FROM room WHERE id=?`, roomID).Scan(&found); errors.Is(err, sql.ErrNoRows) {
		roomID = turn.roomID // Retain malformed targets privately in the receipt.
	} else if err != nil {
		return "", err
	}
	var ask, attachments any
	if item.Ask != nil {
		data, err := json.Marshal(item.Ask)
		if err != nil {
			return "", err
		}
		ask = string(data)
	}
	if item.Attachments != nil {
		data, err := json.Marshal(item.Attachments)
		if err != nil {
			return "", err
		}
		attachments = string(data)
	}
	visualPlan, err := json.Marshal(item.VisualPlan)
	if err != nil {
		return "", err
	}
	appendixPages, err := json.Marshal(item.AppendixPages)
	if err != nil {
		return "", err
	}
	proposed, err := json.Marshal(item.ProposedAnswers)
	if err != nil {
		return "", err
	}
	questions, err := json.Marshal(item.OpenQuestions)
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(`INSERT INTO outbox(id,turn_id,answer_rev,idx,agent_id,room_id,body,reply_to,thread_root,ask_json,attachments_json,visual_plan_json,appendix_pages_json,digest,state,held_reason,dedupe_key,needs_attention,proposed_answers_json,open_questions_json)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, turn.id, input.AnswerRev, index, turn.agentID, roomID, item.Body,
		nullable(item.ReplyTo), nil, ask, attachments, nullablePlan(item.VisualPlan, visualPlan), nullablePlan(item.AppendixPages, appendixPages), input.Digest, state,
		nullable(heldReason), fmt.Sprintf("%s:%d:%d", turn.id, input.AnswerRev, index), item.NeedsAttention,
		nullableList(item.ProposedAnswers, proposed), nullableList(item.OpenQuestions, questions))
	return id, err
}

// defaultReplyTarget is the one message an answer that names no reply_to is plainly about: the turn was woken by exactly one note, from a person
// writing in the room, and the answer is a single plain message back to that room. It returns "" in every other case, because a guess is worse than
// no link: a turn that carries several notes, a message to another room (alone or beside one to this room), an ask or a handoff, or a second message could each be about anything.
func (tx *Tx) defaultReplyTarget(turn answerTurn, input AnswerInput) (string, error) {
	if turn.mode != "normal" || input.Parsed == nil {
		return "", nil
	}
	if len(input.Parsed.Messages) != 1 || input.Parsed.Messages[0].Room != turn.roomID {
		return "", nil
	}
	var notes int
	var messageID, authorKind, roomID string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*),COALESCE(MIN(m.id),''),COALESCE(MIN(m.author_kind),''),COALESCE(MIN(m.room_id),'') FROM turn_note tn JOIN note n ON n.id=tn.note_id JOIN message m ON m.id=n.message_id WHERE tn.turn_id=?`, turn.id).
		Scan(&notes, &messageID, &authorKind, &roomID)
	if err != nil || notes != 1 || authorKind != "user" || roomID != turn.roomID {
		return "", err
	}
	return messageID, nil
}

func (tx *Tx) commitAnswerMessages(turn answerTurn, input AnswerInput, receipt *AnswerReceipt, dropped []droppedMessage) error {
	defaultTo, err := tx.defaultReplyTarget(turn, input)
	if err != nil {
		return err
	}
	heldAt := make(map[int]droppedMessage, len(dropped))
	for _, d := range dropped {
		heldAt[d.index] = d
	}
	for index, item := range input.Parsed.Messages {
		if d, held := heldAt[index]; held {
			// Held alone: the row keeps what the agent wrote (its target may not exist or be readable), and the rest of the reply goes on.
			held := item
			held.ReplyTo = ""
			outID, err := tx.insertOutbox(turn, input, index, held, "proposed", "")
			if err != nil {
				return err
			}
			if err := OutboxTransitions.Transition("proposed", "held"); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE outbox SET state='held',held_reason=? WHERE id=?`, d.reason, outID); err != nil {
				return err
			}
			if receipt.HeldReason == "" {
				receipt.HeldReason, receipt.Error = d.reason, d.detail // what the delivery-failed card shows
			}
			receipt.HeldMessages = append(receipt.HeldMessages, HeldMessage{Index: index, Reason: d.reason, Error: d.detail, Room: item.Room, Body: item.Body})
			continue
		}
		defaulted := defaultTo != "" && item.Room == turn.roomID && item.ReplyTo == "" && item.Ask == nil && !item.Handoff
		if defaulted {
			item.ReplyTo = defaultTo
		}
		outID, err := tx.insertOutbox(turn, input, index, item, "proposed", "")
		if err != nil {
			return err
		}
		if err := OutboxTransitions.Transition("proposed", "validated"); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE outbox SET state='validated' WHERE id=?`, outID); err != nil {
			return err
		}
		var askJSON string
		if item.Ask != nil {
			data, err := json.Marshal(item.Ask)
			if err != nil {
				return err
			}
			askJSON = string(data)
		}
		message, err := tx.InsertMessage(Message{RoomID: item.Room, AuthorKind: "agent", AuthorID: turn.agentID,
			Body: item.Body, ReplyTo: item.ReplyTo, AskJSON: askJSON, SourceOutID: outID, Handoff: item.Handoff,
			NeedsAttention: item.NeedsAttention, ProposedAnswersJSON: jsonList(item.ProposedAnswers), OpenQuestionsJSON: jsonList(item.OpenQuestions),
			VisualPlanJSON: rawList(item.VisualPlan), AppendixPagesJSON: rawList(item.AppendixPages)})
		if err != nil {
			return err
		}
		receipt.MessageIDs = append(receipt.MessageIDs, message.ID)
		if !item.Force {
			if err := tx.forwardMissingMentions(message); err != nil {
				return err
			}
		}
		if defaulted {
			if err := tx.recordReplyDefault(turn, message); err != nil {
				return err
			}
		}
		members, err := tx.roomRecipients(item.Room)
		if err != nil {
			return err
		}
		known := make(map[string]bool, len(members))
		for _, member := range members {
			known[strings.ToLower(member.slug)] = true
		}
		if warning := legacyPrefixWarning(item.Body, known); warning != "" {
			receipt.Warnings = append(receipt.Warnings, fmt.Sprintf("message %d: %s", index, warning))
		}
		for _, attachment := range item.Attachments {
			if _, err := tx.Exec(`UPDATE attachment SET message_id=?,room_id=? WHERE id=? AND message_id IS NULL`, message.ID, item.Room, attachment); err != nil {
				return err
			}
		}
		if item.Ask != nil {
			if err := tx.askInbox(turn, message, item.Ask); err != nil {
				return err
			}
		} else if item.NeedsAttention || len(item.ProposedAnswers) > 0 || len(item.OpenQuestions) > 0 {
			if err := tx.attentionInbox(turn, message, item); err != nil {
				return err
			}
		}
		if err := OutboxTransitions.Transition("validated", "committed"); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE outbox SET state='committed' WHERE id=?`, outID); err != nil {
			return err
		}
	}
	return nil
}

// forwardMissingMentions posts a short pointer in a room shared by the
// author and each absent addressee. The grant exposes only this message,
// never the rest of its room, and the targeted note wakes its recipient.
func (tx *Tx) forwardMissingMentions(message Message) error {
	var projectID, title, kind string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,title,kind FROM room WHERE id=?`, message.RoomID).Scan(&projectID, &title, &kind); err != nil {
		return err
	}
	if kind == "dm" {
		return nil
	}
	seen := map[string]bool{}
	for _, token := range proseMentions(message.Body) {
		if groupTag(token) != "" {
			continue
		}
		var recipientID string
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.id FROM agent a JOIN project p ON p.id=a.project_id
			WHERE a.project_id=? AND a.revoked_at IS NULL AND
			(LOWER(a.id)=? OR LOWER(a.slug)=? OR LOWER(a.slug || '@' || p.name)=?)`,
			projectID, token, token, token).Scan(&recipientID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if seen[recipientID] || recipientID == message.AuthorID {
			continue
		}
		seen[recipientID] = true
		var member bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL)`,
			message.RoomID, recipientID).Scan(&member); err != nil {
			return err
		}
		if member {
			continue
		}
		sharedID, _, err := tx.sharedMentionRoom(message.RoomID, projectID, message.AuthorID, recipientID)
		if err != nil {
			return err
		}
		if sharedID == "" {
			continue
		}
		pointer, err := tx.InsertMessage(Message{RoomID: sharedID, AuthorKind: "system", AuthorID: "system",
			Body:        fmt.Sprintf("%s mentioned you in %s: #%d", authorLabel(tx.ctx, tx.tx, "agent", message.AuthorID), title, message.No),
			OnlyTargets: []string{recipientID}})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO message_reference_grant(source_message_id,target_no) VALUES (?,?)`, pointer.ID, message.No); err != nil {
			return err
		}
	}
	return nil
}

func jsonList[T any](items []T) string {
	if len(items) == 0 {
		return ""
	}
	data, _ := json.Marshal(items)
	return string(data)
}

func nullableList[T any](items []T, encoded []byte) any {
	if len(items) == 0 {
		return nil
	}
	return string(encoded)
}

func (tx *Tx) attentionInbox(turn answerTurn, message Message, item answer.Message) error {
	options := struct {
		Text            string                `json:"text"`
		Options         []string              `json:"options"`
		FreeText        bool                  `json:"free_text"`
		ProposedAnswers []string              `json:"proposed_answers,omitempty"`
		OpenQuestions   []answer.OpenQuestion `json:"open_questions,omitempty"`
		Attention       bool                  `json:"attention"`
	}{Text: message.Body, Options: item.ProposedAnswers, FreeText: true,
		ProposedAnswers: item.ProposedAnswers, OpenQuestions: item.OpenQuestions, Attention: true}
	return tx.insertAnswerInbox(turn, message, "question", options)
}

func rawList(items []json.RawMessage) string {
	if items == nil {
		return ""
	}
	data, _ := json.Marshal(items)
	return string(data)
}

func nullablePlan(items []json.RawMessage, encoded []byte) any {
	if items == nil {
		return nil
	}
	return string(encoded)
}

func (tx *Tx) recordNonCommittedOutbox(turn answerTurn, input AnswerInput, receipt AnswerReceipt) error {
	if input.Parsed == nil {
		return nil
	}
	state := "held"
	if receipt.Result == "correction_requested" {
		state = "superseded"
	}
	for index, item := range input.Parsed.Messages {
		// Invalid references remain in the raw candidate on the receipt;
		// nullable outbox foreign keys cannot point at missing messages.
		item.ReplyTo = ""
		if _, err := tx.insertOutbox(turn, input, index, item, "proposed", ""); err != nil {
			return err
		}
		if state == "superseded" {
			// The proposed row is invalid until the repair turn replaces the
			// entire revision; the old revision cannot be partly committed.
			if err := OutboxTransitions.Transition("proposed", "invalid"); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE outbox SET state='invalid' WHERE turn_id=? AND answer_rev=? AND idx=?`, turn.id, input.AnswerRev, index); err != nil {
				return err
			}
			if err := OutboxTransitions.Transition("invalid", "superseded"); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE outbox SET state='superseded' WHERE turn_id=? AND answer_rev=? AND idx=?`, turn.id, input.AnswerRev, index); err != nil {
				return err
			}
		} else {
			if err := OutboxTransitions.Transition("proposed", "held"); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE outbox SET state='held',held_reason=? WHERE turn_id=? AND answer_rev=? AND idx=?`, receipt.HeldReason, turn.id, input.AnswerRev, index); err != nil {
				return err
			}
		}
	}
	return nil
}

func (tx *Tx) askInbox(turn answerTurn, message Message, ask *answer.Ask) error {
	return tx.insertAnswerInbox(turn, message, ask.Kind, ask)
}

func (tx *Tx) insertAnswerInbox(turn answerTurn, message Message, kind string, payload any) error {
	// The item belongs to the asking agent's project, whatever room it asked in: a chat between agents has no members of its own to see it.
	var projectID string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id FROM agent WHERE id=?`, turn.agentID).Scan(&projectID); err != nil {
		return err
	}
	id, err := newID("inb")
	if err != nil {
		return err
	}
	options, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO inbox_item(id,project_id,room_id,message_id,turn_id,agent_id,requester_kind,requester_id,kind,options_json,created_at)
		VALUES (?,?,?,?,?,?,'agent',?,?,?,?)`, id, projectID, message.RoomID, message.ID, turn.id, turn.agentID, turn.agentID, kind, string(options), timestamp()); err != nil {
		return err
	}
	_, err = tx.AppendEvent(Event{Kind: "inbox_item_created", ProjectID: projectID, RoomID: message.RoomID, AgentID: turn.agentID, TurnID: turn.id,
		PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": id, "message_id": message.ID})})
	return err
}

// recordReplyDefault notes in the turn's events that the server, not the agent, chose this message's reply_to.
func (tx *Tx) recordReplyDefault(turn answerTurn, message Message) error {
	var projectID string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id FROM agent WHERE id=?`, turn.agentID).Scan(&projectID); err != nil {
		return err
	}
	_, err := tx.AppendEvent(Event{Kind: "reply_defaulted", ProjectID: projectID, RoomID: message.RoomID, AgentID: turn.agentID, TurnID: turn.id,
		PayloadJSON: payloadJSON(map[string]string{"message_id": message.ID, "reply_to": message.ReplyTo})})
	return err
}
