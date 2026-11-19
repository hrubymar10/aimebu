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
	TurnID     string   `json:"turn_id"`
	Digest     string   `json:"digest"`
	Result     string   `json:"result"`
	HeldReason string   `json:"held_reason,omitempty"`
	Error      string   `json:"error,omitempty"`
	AnswerRev  int      `json:"answer_rev"`
	MessageIDs []string `json:"message_ids,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
	// HeldMessages lists the messages of a committed reply that were held while the rest was delivered, because their target was refused (see
	// droppedMessage). Empty for a reply delivered whole and for a reply held whole.
	HeldMessages []HeldMessage   `json:"held_messages,omitempty"`
	Additional   []AnswerReceipt `json:"-"`
}

// HeldMessage is one message of a reply that was held alone: its position in the reply, why, and the room it named.
type HeldMessage struct {
	Index  int    `json:"index"`
	Reason string `json:"reason"`
	Error  string `json:"error"`
	Room   string `json:"room,omitempty"`
	Body   string `json:"body,omitempty"`
}

// droppedMessage is a message refused for its target (outside the project, closed room, not a member, a foreign private chat) rather than for
// its shape. Such a message is held alone: the others of the same reply are delivered, so one bad target never costs the author's whole answer.
type droppedMessage struct {
	index          int
	reason, detail string
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
		missedMentionHeld, invalidHeld := false, false
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
			var dropped []droppedMessage
			reason, detail, err := tx.validateAnswer(parent, input, &receipt.Warnings, &dropped)
			if err != nil {
				return err
			}
			if reason == "" && len(dropped) > 0 && len(dropped) == len(input.Parsed.Messages) {
				// Nothing of the reply can be delivered: hold it whole, as a reply with a single bad target always was.
				reason, detail = dropped[0].reason, dropped[0].detail
			}
			if reason == "" {
				receipt.Result = "committed"
				if err := tx.commitAnswerMessages(parent, input, &receipt, dropped); err != nil {
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
					// Nobody asks this agent to repair the reply, so tell it
					// why the reply was held on its next turn.
					invalidHeld = turn.mode == "normal"
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
			if err := tx.resolveDeliveryFailures(turn.agentID, parent.id, committedRooms(input, receipt.HeldMessages)...); err != nil {
				return err
			}
			if len(receipt.HeldMessages) > 0 {
				if err := tx.partialHeldRow(parent, input, receipt); err != nil {
					return err
				}
				if err := tx.partialHeldNote(parent, receipt); err != nil {
					return err
				}
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
				if err == nil && invalidHeld {
					err = tx.invalidReplyNote(parent, receipt.Error)
				}
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

func (tx *Tx) validateAnswer(turn answerTurn, input AnswerInput, warnings *[]string, dropped *[]droppedMessage) (reason, detail string, dbErr error) {
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
			*dropped = append(*dropped, droppedMessage{i, "not_member", "private chat replies must stay in that chat"})
			continue
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
			*dropped = append(*dropped, droppedMessage{i, "not_member", outsideProjectDetail})
			continue
		}
		if agentChat && item.Room != turn.roomID && itemKind == "dm" {
			*dropped = append(*dropped, droppedMessage{i, "not_member", "a chat between agents cannot report into another private chat"})
			continue
		}
		if lifecycle != "open" {
			*dropped = append(*dropped, droppedMessage{i, "room_closed", "room is closing or closed"})
			continue
		}
		var member int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL`, item.Room, turn.agentID).Scan(&member); err != nil {
			return "", "", err
		}
		if member != 1 {
			*dropped = append(*dropped, droppedMessage{i, "not_member", "agent is not an active room member"})
			continue
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
	// A shared room can carry a pointer to the committed message. Keep the
	// hold only for absent agents who have no room in common with the author.
	unshared := absent[:0]
	for _, missing := range absent {
		sharedID, _, err := tx.sharedMentionRoom(roomID, projectID, authorID, missing.id)
		if err != nil {
			return "", "", err
		}
		if sharedID == "" {
			unshared = append(unshared, missing)
		}
	}
	if len(unshared) == 0 {
		return "", "", nil
	}
	absent = unshared
	if addressed {
		return "", fmt.Sprintf("@%s is outside the target room and was not notified", absent[0].token), nil
	}
	missing := absent[0]
	// A room both agents can use would already have carried a pointer above.
	// Do not suggest rooms the author cannot post in.
	return fmt.Sprintf("@%s refers to agent %s, who is not in the target room; use dm_agent or retry with force:true", missing.token, missing.id), "", nil
}

// committedRooms lists the rooms a committed reply actually posted into: a reply
// with no messages posted nowhere, so it clears no earlier failure by room.
func committedRooms(input AnswerInput, held []HeldMessage) []string {
	var rooms []string
	skip := make(map[int]bool, len(held))
	for _, h := range held {
		skip[h.Index] = true
	}
	if input.Parsed != nil {
		for i, message := range input.Parsed.Messages {
			if message.Room != "" && !skip[i] {
				rooms = append(rooms, message.Room)
			}
		}
	}
	return rooms
}

const heldReplyNotePrefix = "Your previous reply was held: "
const partialHeldNotePrefix = "Part of your previous reply was held: "
const outsideProjectDetail = "room is outside the agent project"

// invalidReplyNote tells an agent that cannot be asked for a correction why its
// previous reply was held, as a system turn note only it can see.
func (tx *Tx) invalidReplyNote(turn answerTurn, detail string) error {
	// The note wakes the agent. A turn that was itself woken by such a note and held again gets none, so a repeating
	// invalid shape ends in the delivery-failed card instead of a loop of notes and turns.
	var woken bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM turn_note tn JOIN note n ON n.id=tn.note_id JOIN message m ON m.id=n.message_id
		WHERE tn.turn_id=? AND m.author_kind='system' AND (m.body LIKE ? OR m.body LIKE ?))`, turn.id, heldReplyNotePrefix+"%", partialHeldNotePrefix+"%").Scan(&woken); err != nil || woken {
		return err
	}
	// A parser error quotes what the agent wrote (a field name, a character); the room can read this note, so only the fixed
	// validation texts go in as they are and a quoting one is described instead.
	if strings.ContainsAny(detail, `"'`) || strings.Contains(detail, "json:") {
		detail = "it was not one valid JSON answer object (malformed JSON, an unknown or misplaced field, or text around the object)"
	}
	body := heldReplyNotePrefix + detail
	_, err := tx.InsertMessage(Message{RoomID: turn.roomID, AuthorKind: "system", AuthorID: "system", Body: body, OnlyTargets: []string{turn.agentID}})
	return err
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
	body := "Your final answer was held in message " + outboxID + "; use dm_agent to contact the absent agent or retry with force:true."
	_, err := tx.InsertMessage(Message{RoomID: turn.roomID, AuthorKind: "system", AuthorID: "system", Body: body, OnlyTargets: []string{turn.agentID}})
	return err
}

// sharedMentionRoom chooses a room that both agents can read. Flow routing
// rooms take priority so a leader receives dispatcher reports there.
func (tx *Tx) sharedMentionRoom(targetRoom, projectID, authorID, recipientID string) (string, string, error) {
	var id, title string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT r.id,r.title FROM room r
		JOIN room_member author ON author.room_id=r.id AND author.member_kind='agent' AND author.member_id=? AND author.left_at IS NULL
		JOIN room_member recipient ON recipient.room_id=r.id AND recipient.member_kind='agent' AND recipient.member_id=? AND recipient.left_at IS NULL
		WHERE r.project_id=? AND r.id<>? AND r.lifecycle='open' AND r.kind<>'dm'
			AND EXISTS(SELECT 1 FROM room target WHERE target.id=? AND target.kind<>'dm')
		ORDER BY CASE r.team_flow_room_key WHEN 'routing' THEN 0 WHEN 'advice' THEN 1 ELSE 2 END,
			(SELECT COUNT(*) FROM room_member member WHERE member.room_id=r.id AND member.left_at IS NULL),r.created_at,r.id LIMIT 1`,
		authorID, recipientID, projectID, targetRoom, targetRoom).Scan(&id, &title)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return id, title, err
}

// partialHeldNote tells an agent, as a system note only it can see, which messages of its reply were not delivered and why, with their text so it
// can resend them correctly. Like invalidReplyNote it wakes the agent, and a turn already woken by such a note gets no second one.
func (tx *Tx) partialHeldNote(turn answerTurn, receipt AnswerReceipt) error {
	var woken bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM turn_note tn JOIN note n ON n.id=tn.note_id JOIN message m ON m.id=n.message_id
		WHERE tn.turn_id=? AND m.author_kind='system' AND (m.body LIKE ? OR m.body LIKE ?))`, turn.id, heldReplyNotePrefix+"%", partialHeldNotePrefix+"%").Scan(&woken); err != nil || woken {
		return err
	}
	var b strings.Builder
	b.WriteString(partialHeldNotePrefix)
	b.WriteString("the other messages were delivered and must not be sent again. Not delivered:")
	for _, h := range receipt.HeldMessages {
		fmt.Fprintf(&b, "\n- message %d: %s.", h.Index+1, h.Error)
		switch {
		case h.Error == outsideProjectDetail:
			b.WriteString(" This room is in another project; to reach an agent there use the `dm_agent` tool, and to reach a person use your own project's rooms.")
		case h.Reason == "room_closed":
			b.WriteString(" That room is closing or closed; use an open room of your project.")
		default:
			b.WriteString(" Send it to a room of your project that you are a member of.")
		}
		body := []rune(h.Body)
		if len(body) > 4000 {
			body = append(body[:4000:4000], []rune("…")...)
		}
		fmt.Fprintf(&b, " Its text was:\n%s", string(body))
	}
	_, err := tx.InsertMessage(Message{RoomID: turn.roomID, AuthorKind: "system", AuthorID: "system", Body: b.String(), OnlyTargets: []string{turn.agentID}})
	return err
}
