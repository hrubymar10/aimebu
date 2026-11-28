package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/answer"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

type AnswerInput struct {
	TurnID, AgentID, Digest string
	Generation              int
	AnswerRev               int
	Candidate               []byte
	Parsed                  *answer.Candidate
	ParseError              string
	// AnswerTool, when set, is the supervisor's report on the answer frame of whether this turn was offered the submit_answer tool
	// (the same test its adapter applied). nil is an answer an older supervisor sent: the turn's column stays NULL and the held-reply
	// repair note falls back to the machine's last probed harness version.
	AnswerTool *bool
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
	// handoffOutcomeRequired makes a handoff without an outcome invalid: the supervisor the turn was dispatched to reported
	// supports_handoff_outcome (Admission), so only then did the agent's prompt and response schema offer the field.
	handoffOutcomeRequired bool
	// answerTool is what the turn's supervisor reported about the submit_answer tool on the answer frame; NULL before the first
	// answer, or when the answer came from a supervisor that does not report the fact.
	answerTool sql.NullInt64
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
		// The supervisor reports, on the answer frame, whether this turn was offered the submit_answer tool — the same test its
		// adapter applied before it marked the turn's bus endpoint with answer_tool=1. The turn keeps the fact, so the held-reply
		// repair note follows what the turn actually had; a frame without the report (an older supervisor's) leaves the column NULL
		// and the note falls back to the machine's last probed harness version.
		if input.AnswerTool != nil {
			value := 0
			if *input.AnswerTool {
				value = 1
			}
			if _, err := tx.Exec(`UPDATE turn SET answer_tool=? WHERE id=?`, value, turn.id); err != nil {
				return err
			}
			turn.answerTool = sql.NullInt64{Int64: int64(value), Valid: true}
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
		var dropped []droppedMessage
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
			// The turn that answers decides whether a handoff needs an outcome: a correction runs under the supervisor it was
			// dispatched to, which may not be its parent's.
			reason, detail, err := tx.validateAnswer(parent, input, turn.handoffOutcomeRequired, &receipt.Warnings, &dropped)
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
				sent, err := tx.partialHeldNote(parent, receipt)
				if err != nil {
					return err
				}
				if !sent {
					return tx.deliveryFailedInbox(parent, false)
				}
			}
		}
		if receipt.Result == "committed" && turn.mode == "normal" && len(receipt.MessageIDs) == 0 {
			if err := tx.silentTurnNotice(turn); err != nil {
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
			} else if invalidHeld {
				err = tx.routeInvalidReply(parent, input, receipt.Error)
			} else {
				err = tx.routeUndeliverable(parent, input, receipt, dropped)
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
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT t.id,t.parent_turn_id,t.agent_id,t.session_id,t.room_id,t.mode,t.exec,t.generation,t.attempt,t.terminal_event,a.recovery_block,t.policy_json,t.handoff_outcome_required,t.answer_tool FROM turn t JOIN agent a ON a.id=t.agent_id WHERE t.id=?`, id).
		Scan(&turn.id, &parent, &turn.agentID, &turn.sessionID, &turn.roomID, &turn.mode, &turn.exec, &turn.generation, &turn.attempt, &turn.terminal, &turn.blocked, &policy, &turn.handoffOutcomeRequired, &turn.answerTool)
	turn.parentID, turn.policyJSON = parent.String, policy.String
	return turn, err
}

func (tx *Tx) noActionMode(raw string) bool {
	var policy TurnPolicy
	return json.Unmarshal([]byte(raw), &policy) == nil && policy.Capabilities.NoActionMode
}

func (tx *Tx) validateAnswer(turn answerTurn, input AnswerInput, requireOutcome bool, warnings *[]string, dropped *[]droppedMessage) (reason, detail string, dbErr error) {
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
	var agentProject string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id FROM agent WHERE id=?`, turn.agentID).Scan(&agentProject); err != nil {
		return "", "", err
	}
	for i, item := range input.Parsed.Messages {
		if item.Handoff && item.Outcome == "" && requireOutcome {
			return "invalid_answer", fmt.Sprintf("message %d is a handoff without an outcome; set outcome to %s", i, answer.OutcomeChoices), nil
		}
		if turnRoomKind == "dm" && item.Room != turn.roomID && !agentChat {
			*dropped = append(*dropped, droppedMessage{i, "not_member", "Replies in a private chat with a person must stay in that chat."})
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
		// Agent chats use a system project. A same-project chat may receive this answer from any turn room; a cross-project chat
		// needs its own turn, so a different room cannot bypass dm_agent's reach checks.
		targetAgentChat := itemKind == "dm" && projectID == AgentDMProjectID
		if targetAgentChat && item.Room != turn.roomID {
			var foreignPeer bool
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM room_member rm JOIN agent a ON a.id=rm.member_id
				WHERE rm.room_id=? AND rm.member_kind='agent' AND rm.member_id<>? AND rm.left_at IS NULL AND a.project_id<>?)`,
				item.Room, turn.agentID, agentProject).Scan(&foreignPeer); err != nil {
				return "", "", err
			}
			if foreignPeer {
				*dropped = append(*dropped, droppedMessage{i, "not_member", outsideProjectDetail})
				continue
			}
		}
		if projectID != agentProject && !targetAgentChat {
			*dropped = append(*dropped, droppedMessage{i, "not_member", outsideProjectDetail})
			continue
		}
		if lifecycle != "open" {
			*dropped = append(*dropped, droppedMessage{i, "room_closed", "This room is closing or closed."})
			continue
		}
		var member int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL`, item.Room, turn.agentID).Scan(&member); err != nil {
			return "", "", err
		}
		if member != 1 {
			detail := notMemberDetail
			if item.Force {
				detail += " Force does not allow posting into a room you are not a member of."
			}
			*dropped = append(*dropped, droppedMessage{i, "not_member", detail})
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
		// A reply names one message of the room the message goes to, by id or by number. A reference that names nothing, or a message of
		// another room (whether the agent may post there or not), never holds the answer: the link is dropped, the message is posted
		// without it, and the receipt carries a warning saying why.
		if item.ReplyTo != "" {
			refRoom, found, err := tx.messageRefRoom(item.ReplyTo)
			if err != nil {
				return "", "", err
			}
			switch {
			case !found:
				*warnings = append(*warnings, fmt.Sprintf("message %d: reply_to %s dropped: no message carries that reference; the message was posted without the reply link", i, safeReplyRef(item.ReplyTo)))
			case refRoom == item.Room:
			default:
				var member int
				if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL`, refRoom, turn.agentID).Scan(&member); err != nil {
					return "", "", err
				}
				if member == 1 {
					*warnings = append(*warnings, fmt.Sprintf("message %d: reply_to %s dropped: message is in another room; the message was posted without the reply link", i, safeReplyRef(item.ReplyTo)))
				} else {
					*warnings = append(*warnings, fmt.Sprintf("message %d: reply_to %s dropped: message is in a room you are not a member of; the message was posted without the reply link", i, safeReplyRef(item.ReplyTo)))
				}
			}
			if refRoom != item.Room || !found {
				input.Parsed.Messages[i].ReplyTo = ""
			}
		}
		if item.Ask != nil || item.NeedsAttention || len(item.ProposedAnswers) > 0 || len(item.OpenQuestions) > 0 {
			var recipients int
			query := `SELECT COUNT(*) FROM membership WHERE project_id=? AND role IN ('owner','operator')`
			arg := projectID
			var err error
			switch {
			case targetAgentChat:
				// The Inbox shows an agent's question to the owners and operators of the agent's own project and an approval to its owners, so only
				// someone in the chat who is one of them can answer it.
				roles := `'owner','operator'`
				if item.Ask != nil && item.Ask.Kind == "approval" {
					roles = `'owner'`
				}
				err = tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM room_member rm JOIN membership mb ON mb.project_id=? AND mb.user_id=rm.member_id
					WHERE rm.room_id=? AND rm.member_kind='user' AND rm.left_at IS NULL AND mb.role IN (`+roles+`)`, agentProject, item.Room).Scan(&recipients)
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

// The reasons a message's target is refused, each a sentence of its own; a room of another project also names the missing membership.
const notMemberDetail = "The agent is not a member of this room."
const outsideProjectDetail = "This room is outside the agent's project. " + notMemberDetail

// heldDetail makes a held-answer reason safe to put in a note the room can read: a parser error quotes what the agent wrote (a field name, a
// character), so only fixed texts go in as they are and a quoting one is described instead.
func heldDetail(detail string) string {
	if strings.HasPrefix(detail, "invalid_answer:") {
		return answer.SafeError(errors.New(detail))
	}
	if strings.ContainsAny(detail, `"'`) || strings.Contains(detail, "json:") {
		return "it was not one valid JSON answer object (malformed JSON, an unknown or misplaced field, or text around the object)"
	}
	return strings.TrimPrefix(detail, "invalid_answer: ")
}

// recoveryEvidence reduces a held-answer reason to one safe line for the lead's recovery note: the same fixed texts as a held-reply note,
// with control characters and runs of space collapsed and a length bound, so a crafted reason cannot reshape the note.
func recoveryEvidence(detail string) string {
	detail = heldDetail(detail)
	detail = strings.Join(strings.Fields(detail), " ")
	runes := []rune(detail)
	if len(runes) > 240 {
		return string(runes[:240]) + "…"
	}
	return detail
}

// safeReplyRef reduces a reply reference to a form a note may echo: only the characters a message number or id is made of survive, so a
// crafted reference cannot carry text or control characters into the note.
func safeReplyRef(ref string) string {
	safe := make([]rune, 0, len(ref))
	for _, r := range ref {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			safe = append(safe, r)
		default:
			safe = append(safe, '?')
		}
	}
	if len(safe) > 64 {
		safe = safe[:64]
	}
	return string(safe)
}

// routeInvalidReply decides where a held, malformed final answer goes. A person never sees a worker's malformed answer first:
//  1. two empty answers in a row, with no tool activity, are a broken session: a recovery to the lead (restart with a fresh session);
//  2. otherwise the agent is told why (the error only: the note is readable in the room, and the agent has its own answer in its session) and
//     woken to send it again (no card);
//  3. when that note was already sent for this turn (the repair failed too), the failure goes to the lead.
//
// A lead (or a worker with no lead to hand to) has nobody above it, so it is told and woken the same way, and only its failed repair becomes
// the delivery_failed card and room notice for people — except when the repair note cannot wake the agent into a repair turn at all: a
// stopped or blocked agent, or one whose machine has not been seen for machineSeenWindow, can never hold again, so its failure reaches its
// lead (or people, when it has nobody above it) at once. The note is still written and queued for when the agent can act again.
func (tx *Tx) routeInvalidReply(parent answerTurn, input AnswerInput, detail string) error {
	var isLead bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT r.cap_is_lead FROM agent a JOIN role r ON r.id=a.role_id WHERE a.id=?`, parent.agentID).Scan(&isLead); err != nil {
		return err
	}
	wakeable, err := tx.repairWakeable(parent.agentID)
	if err != nil {
		return err
	}
	if isLead {
		// The lead is told why and woken to resend; its own next answer may settle the hold, so people hear of it (the Inbox card and the
		// room notice) only once that repair has failed too: a turn already woken by the note is held again. A lead the note cannot wake
		// (stopped, blocked, its machine gone) never gets that second hold, so people hear of it now.
		sent, err := tx.invalidReplyNote(parent, input, detail)
		if err != nil {
			return err
		}
		if !sent || !wakeable {
			return tx.deliveryFailedInbox(parent, false)
		}
		return nil
	}
	if strings.TrimSpace(string(input.Candidate)) == "" {
		broken, err := tx.repeatedEmptyAnswer(parent)
		if err != nil {
			return err
		}
		if broken {
			return tx.brokenSessionRecovery(parent)
		}
	}
	sent, err := tx.invalidReplyNote(parent, input, detail)
	if err != nil {
		return err
	}
	if !sent || !wakeable {
		return tx.deliveryFailedInbox(parent, true)
	}
	return nil
}

// invalidReplyNote tells an agent why its previous reply was held, as a system turn note only it can see, and wakes it to resend it. It
// reports false when the turn was itself woken by such a note (the repair failed too), so a repeating invalid shape ends with the lead
// instead of a loop of notes and turns.
func (tx *Tx) invalidReplyNote(turn answerTurn, input AnswerInput, detail string) (bool, error) {
	var woken bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM turn_note tn JOIN note n ON n.id=tn.note_id JOIN message m ON m.id=n.message_id
		WHERE tn.turn_id=? AND m.author_kind='system' AND (m.body LIKE ? OR m.body LIKE ?))`, turn.id, heldReplyNotePrefix+"%", partialHeldNotePrefix+"%").Scan(&woken); err != nil || woken {
		return false, err
	}
	// A parser error quotes what the agent wrote (a field name, a character); the room can read this note, so only the fixed
	// validation texts go in as they are and a quoting one is described instead.
	detail = heldDetail(detail)
	body := heldReplyNotePrefix + detail + ". Your previous answer is in your session; send your final answer again as exactly one JSON object with a messages array."
	var tool, variant, launcherID, machineID, harnesses string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.tool,a.variant,COALESCE(a.launcher_id,''),COALESCE(a.machine_id,''),COALESCE(m.harnesses_json,'[]')
		FROM agent a LEFT JOIN machine m ON m.id=a.machine_id WHERE a.id=?`, turn.agentID).Scan(&tool, &variant, &launcherID, &machineID, &harnesses); err != nil {
		return false, err
	}
	if tool == "opencode" || tool == "pi" {
		// What the turn actually had wins: the supervisor reports on the answer frame whether its adapter offered the tool for this
		// turn (the same test that marked the turn's bus endpoint), a launcher turn included. An answer that carries no report — an
		// older supervisor's, or one sent before the upgrade — falls back to the machine's last probed version, which knows nothing
		// about a launcher's harness.
		offered := turn.answerTool.Valid && turn.answerTool.Int64 == 1
		if !turn.answerTool.Valid {
			offered = tx.answerToolSupported(tool, variant, launcherID, harnesses)
		}
		if offered {
			body += " End the turn by calling submit_answer. If the tool is unavailable, use this exact answer schema: <json-schema>" + string(answer.ResponseSchema) + "</json-schema>."
		} else {
			// The turn had no submit_answer tool (an older or unprobed CLI), so naming it would send the agent after a tool it cannot call.
			body += " Use this exact answer schema: <json-schema>" + string(answer.ResponseSchema) + "</json-schema>."
		}
	}
	_, err := tx.InsertMessage(Message{RoomID: turn.roomID, AuthorKind: "system", AuthorID: "system", Body: body, OnlyTargets: []string{turn.agentID}})
	return err == nil, err
}

// answerToolSupported is the held-reply note's fallback for an answer whose supervisor did not report whether the turn had the
// submit_answer tool (an older build): it reads the version the machine last probed for the agent's harness. The server keeps no probed
// version for a launcher's harness, so a launcher agent counts as unsupported and keeps the plain-text repair note, which is correct for
// every version; a turn whose supervisor does report the fact is judged by the turn instead.
func (tx *Tx) answerToolSupported(tool, variant, launcherID, harnesses string) bool {
	if launcherID != "" {
		return false
	}
	var rows []protocol.HarnessRow
	if json.Unmarshal([]byte(harnesses), &rows) != nil {
		return false
	}
	for _, row := range rows {
		if row.Tool == tool && row.Variant == variant {
			return adapter.SupportsAnswerTool(tool, row.Version)
		}
	}
	return false
}

// machineSeenWindow is how fresh a machine's last_seen must be for the repair note's wake to plausibly reach a running turn: supervisors
// ping every 15 s and the stored timestamp is refreshed at most once a minute, so anything older is a machine that cannot take the turn now.
const machineSeenWindow = 5 * time.Minute

// repairWakeable says whether the held-reply repair note can actually wake the agent into a repair turn: an agent that is stopped or not
// accepting, a blocked one, one already recorded lost, or one whose machine has not been seen for machineSeenWindow cannot take a turn,
// so its failure reaches its lead — or people, when it has nobody above it — at once instead of waiting for a second hold that can never
// happen. An agent in a turn is alive (the note queues behind it), and a machine with no recorded last_seen counts as gone.
func (tx *Tx) repairWakeable(agentID string) (bool, error) {
	var desired, observed, lastSeen string
	var blocked bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.desired,a.observed,a.recovery_block,COALESCE(m.last_seen,'')
		FROM agent a LEFT JOIN machine m ON m.id=a.machine_id WHERE a.id=?`, agentID).Scan(&desired, &observed, &blocked, &lastSeen); err != nil {
		return false, err
	}
	if desired != "active" || blocked || observed == "stopped" || observed == "lost" || lastSeen == "" {
		return false, nil
	}
	seen, err := time.Parse(timestampLayout, lastSeen)
	if err != nil {
		return false, nil
	}
	return time.Since(seen) < machineSeenWindow, nil
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
func (tx *Tx) partialHeldNote(turn answerTurn, receipt AnswerReceipt) (bool, error) {
	var reachable bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM room r JOIN room_member m ON m.room_id=r.id
		WHERE r.id=? AND r.lifecycle='open' AND m.member_kind='agent' AND m.member_id=? AND m.left_at IS NULL)`, turn.roomID, turn.agentID).Scan(&reachable); err != nil || !reachable {
		return false, err
	}
	var woken bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM turn_note tn JOIN note n ON n.id=tn.note_id JOIN message m ON m.id=n.message_id
		WHERE tn.turn_id=? AND m.author_kind='system' AND (m.body LIKE ? OR m.body LIKE ?))`, turn.id, heldReplyNotePrefix+"%", partialHeldNotePrefix+"%").Scan(&woken); err != nil || woken {
		return false, err
	}
	var b strings.Builder
	b.WriteString(partialHeldNotePrefix)
	b.WriteString("the other messages were delivered and must not be sent again. Not delivered:")
	writeHeldMessages(&b, receipt.HeldMessages, false)
	_, err := tx.InsertMessage(Message{RoomID: turn.roomID, AuthorKind: "system", AuthorID: "system", Body: b.String(), OnlyTargets: []string{turn.agentID}})
	return err == nil, err
}

// writeHeldMessages lists refused messages without copying their text into
// room history, which may be readable by people other than the author.
func writeHeldMessages(b *strings.Builder, held []HeldMessage, agentOnly bool) {
	for _, h := range held {
		fmt.Fprintf(b, "\n- message %d: %s", h.Index+1, h.Error)
		switch {
		case agentOnly:
			b.WriteString(" Send it to that agent with the `dm_agent` tool. If that tool refuses the target, ask your lead or dispatcher to relay it; keep private text out of this room.")
		case h.Error == outsideProjectDetail:
			b.WriteString(" This room is in another project; to reach an agent there use the `dm_agent` tool, and to reach a person use your own project's rooms.")
		case h.Reason == "room_closed":
			b.WriteString(" That room is closing or closed; use an open room of your project.")
		default:
			b.WriteString(" Send it to a room of your project that you are a member of.")
		}
		b.WriteString(" The original reply remains in your session.")
	}
}

// routeUndeliverable decides where a held reply goes when its target was refused (room_closed, not_member). A reply whose messages all go to
// other agents — a private chat with an agent, a mention of an agent, a reply to an agent's message, and no person addressed in any of them —
// raises no Inbox item: its author gets a note in the room it was woken in,
// with the reason, and sends it again. Anything else raises the
// delivery_failed card for people: a plain message, a mention of a person,
// a reply to a person,
// an ask, a mixed message that names an agent and a person, an author that no longer exists, a room the note cannot be written in, and a turn
// that the same kind of note already woke (no loop).
func (tx *Tx) routeUndeliverable(parent answerTurn, input AnswerInput, receipt AnswerReceipt, dropped []droppedMessage) error {
	toLead := receipt.HeldReason == "invalid_after_correction"
	if (receipt.HeldReason == "not_member" || receipt.HeldReason == "room_closed") && input.Parsed != nil && len(dropped) == len(input.Parsed.Messages) && len(dropped) > 0 {
		ok, err := tx.agentOnlyReply(parent.agentID, input.Parsed.Messages)
		if err != nil {
			return err
		}
		if ok {
			sent, err := tx.undeliverableAgentNote(parent, input, dropped)
			if err != nil {
				return err
			}
			if sent {
				wakeable, err := tx.repairWakeable(parent.agentID)
				if err != nil || wakeable {
					return err
				}
			}
		}
	}
	return tx.deliveryFailedInbox(parent, toLead)
}

// agentOnlyReply says whether every message of a reply is addressed to agents and to nobody else (see routeUndeliverable).
func (tx *Tx) agentOnlyReply(authorID string, messages []answer.Message) (bool, error) {
	var revoked bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT revoked_at IS NOT NULL FROM agent WHERE id=?`, authorID).Scan(&revoked); err != nil || revoked {
		return false, err
	}
	for _, message := range messages {
		if message.Ask != nil || message.NeedsAttention || len(message.ProposedAnswers) > 0 || len(message.OpenQuestions) > 0 {
			return false, nil
		}
		agentSignal := false
		var kind, project string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT kind,project_id FROM room WHERE id=?`, message.Room).Scan(&kind, &project); err != nil {
			return false, err
		}
		members, err := tx.roomRecipients(message.Room)
		if err != nil {
			return false, err
		}
		if kind == "dm" {
			for _, m := range members {
				if m.kind == "user" && !m.implicit {
					return false, nil
				}
				agentSignal = agentSignal || m.kind == "agent" && m.id != authorID
			}
		}
		var projectName string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT name FROM project WHERE id=?`, project).Scan(&projectName); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
		for id := range resolveMentionTargets(message.Body, authorID, members, project, projectName) {
			for _, m := range members {
				if m.id != id {
					continue
				}
				if m.kind == "user" {
					return false, nil
				}
				agentSignal = agentSignal || m.id != authorID
			}
		}
		if message.ReplyTo != "" {
			var kindOf, repliedAuthor string
			err := tx.tx.QueryRowContext(tx.ctx, `SELECT author_kind,author_id FROM message WHERE id=?`, message.ReplyTo).Scan(&kindOf, &repliedAuthor)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return false, err
			}
			switch kindOf {
			case "user":
				return false, nil
			case "agent":
				agentSignal = agentSignal || repliedAuthor != authorID
			}
		}
		if !agentSignal {
			return false, nil
		}
	}
	return true, nil
}

// undeliverableAgentNote tells the author, as a system note only it can see in the room its turn was woken in, why messages to other agents
// were refused, without copying private text into room history. It reports false (and writes nothing) when the room is not open or the turn was already woken by a held-reply
// note, so the caller raises the card instead.
func (tx *Tx) undeliverableAgentNote(turn answerTurn, input AnswerInput, dropped []droppedMessage) (bool, error) {
	var lifecycle string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT lifecycle FROM room WHERE id=?`, turn.roomID).Scan(&lifecycle); err != nil || lifecycle != "open" {
		return false, err
	}
	var member bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL)`, turn.roomID, turn.agentID).Scan(&member); err != nil || !member {
		return false, err
	}
	var woken bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM turn_note tn JOIN note n ON n.id=tn.note_id JOIN message m ON m.id=n.message_id
		WHERE tn.turn_id=? AND m.author_kind='system' AND (m.body LIKE ? OR m.body LIKE ?))`, turn.id, heldReplyNotePrefix+"%", partialHeldNotePrefix+"%").Scan(&woken); err != nil || woken {
		return false, err
	}
	held := make([]HeldMessage, 0, len(dropped))
	for _, d := range dropped {
		item := input.Parsed.Messages[d.index]
		held = append(held, HeldMessage{Index: d.index, Reason: d.reason, Error: d.detail, Room: item.Room, Body: item.Body})
	}
	var b strings.Builder
	b.WriteString(heldReplyNotePrefix)
	b.WriteString("a message to another agent was not delivered, and no person was asked about it. Send it again to that agent with the `dm_agent` tool. Not delivered:")
	writeHeldMessages(&b, held, true)
	_, err := tx.InsertMessage(Message{RoomID: turn.roomID, AuthorKind: "system", AuthorID: "system", Body: b.String(), OnlyTargets: []string{turn.agentID}})
	return err == nil, err
}
