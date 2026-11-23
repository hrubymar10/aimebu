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

// defaultReplyTarget is the one message an answer that names no reply_to is plainly about: the turn was woken by exactly one note written in the
// room, and the answer is a single message back to that room (not an ask). The note must be a person's, unless the answer is a handoff: a handoff
// answers whoever briefed or nudged the worker, an agent as often as a person, and the link carries it back to them. It returns "" in every other
// case, because a guess is worse than no link: a turn that carries several notes, a message to another room (alone or beside one to this room), an
// ask, a plain message after an agent's note, or a second message could each be about anything.
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
	if err != nil || notes != 1 || roomID != turn.roomID || authorKind != "user" && !(authorKind == "agent" && input.Parsed.Messages[0].Handoff) {
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
	var wokeNobody []int64
	var ambiguities []string
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
		defaulted := defaultTo != "" && item.Room == turn.roomID && item.ReplyTo == "" && item.Ask == nil
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
			Body: item.Body, ReplyTo: item.ReplyTo, AskJSON: askJSON, SourceOutID: outID, Handoff: item.Handoff, Outcome: handoffOutcome(item),
			NeedsAttention: item.NeedsAttention, ProposedAnswersJSON: jsonList(item.ProposedAnswers), OpenQuestionsJSON: jsonList(item.OpenQuestions),
			VisualPlanJSON: rawList(item.VisualPlan), AppendixPagesJSON: rawList(item.AppendixPages)})
		if err != nil {
			return err
		}
		receipt.MessageIDs = append(receipt.MessageIDs, message.ID)
		forwarded := 0
		if !item.Force {
			if forwarded, err = tx.forwardMissingMentions(message); err != nil {
				return err
			}
		}
		ambiguous, err := tx.ambiguousMentions(message)
		if err != nil {
			return err
		}
		if len(ambiguous) > 0 {
			ambiguities = append(ambiguities, fmt.Sprintf("#%d: %s", message.No, strings.Join(ambiguous, "; ")))
		}
		if forwarded == 0 && len(ambiguous) == 0 {
			silent, err := tx.messageWokeNobody(turn, message)
			if err != nil {
				return err
			}
			if silent {
				wokeNobody = append(wokeNobody, message.No)
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
	if err := tx.ambiguousMentionNote(turn, ambiguities); err != nil {
		return err
	}
	return tx.noWakeNote(turn, wokeNobody)
}

const ambiguousNotePrefix = "Your mention addressed nobody: "

// ambiguousMentions lists the live @tokens of a committed agent message that two or more agents of its room answer to. Such a token addresses
// none of them, whatever else the message reaches (a reply, a person in the room, another mention), so the sender is told at once which full
// IDs would name each one. An agent that shares its name only with people is not ambiguous: it and the people are all addressed.
func (tx *Tx) ambiguousMentions(message Message) ([]string, error) {
	var projectID, projectName string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT p.id,p.name FROM room r JOIN project p ON p.id=r.project_id WHERE r.id=?`, message.RoomID).Scan(&projectID, &projectName); err != nil {
		return nil, err
	}
	if projectID == AgentDMProjectID { // a chat between agents reads no mentions
		return nil, nil
	}
	members, err := tx.roomRecipients(message.RoomID)
	if err != nil {
		return nil, err
	}
	var trace []MentionTrace
	resolveMentions(message.Body, message.AuthorID, members, projectID, projectName, &trace)
	var out []string
	for _, entry := range trace {
		if entry.Outcome != "ambiguous" {
			continue
		}
		var forms []string
		for _, person := range entry.Reached {
			if person.Kind == "agent" {
				forms = append(forms, "@"+person.ID)
			}
		}
		out = append(out, fmt.Sprintf("@%s is ambiguous (%d agents in the room share the slug); write the full ID of the one you mean: %s", entry.Token, len(forms), strings.Join(forms, " or ")))
	}
	return out, nil
}

// ambiguousMentionNote tells the agent, as a system note only it can see, which of its mentions addressed nobody because several agents share the
// name. It is sent in every kind of room, with a person in it or not, because the agent believes it has addressed someone. A turn woken by such
// a note gets no second one, so an agent that repeats the ambiguous name ends there rather than in a loop of notes.
func (tx *Tx) ambiguousMentionNote(turn answerTurn, lines []string) error {
	if len(lines) == 0 {
		return nil
	}
	var woken bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM turn_note tn JOIN note n ON n.id=tn.note_id JOIN message m ON m.id=n.message_id
		WHERE tn.turn_id=? AND m.author_kind='system' AND m.body LIKE ?)`, turn.id, ambiguousNotePrefix+"%").Scan(&woken); err != nil || woken {
		return err
	}
	body := ambiguousNotePrefix + strings.Join(lines, ". ") + ". Send a short pointer to your message with the right name; do not repeat the whole message."
	_, err := tx.InsertMessage(Message{RoomID: turn.roomID, AuthorKind: "system", AuthorID: "system", Body: body, OnlyTargets: []string{turn.agentID}})
	return err
}

const noWakeNotePrefix = "Your message woke nobody: "

// messageWokeNobody says an agent's message addressed nobody at all and woke nobody, so whoever should act next never hears of it: no target (a
// person counts), no note. Not when it was not meant to reach anyone, or was meant to and was stopped on purpose: a reply (to a person it
// targets them; to an agent with no wake it was stopped by the reply guard), a private chat, a message that asks a person for something, a
// message in a turn a person in that room woke (it is said to them), and an unaddressed message the broadcast limit stopped. Nor a lead's
// message, nor any message in a room with a person in it (an explicit human member, or the home room): people read their rooms without being
// woken, so a lead's status line to them is not lost. The note is for rooms of agents alone, where nobody will ever read it.
func (tx *Tx) messageWokeNobody(turn answerTurn, message Message) (bool, error) {
	if message.ReplyTo != "" || asksAPerson(message) {
		return false, nil
	}
	if t := strings.TrimSpace(message.TargetsJSON); t != "" && t != "[]" && t != "null" {
		return false, nil
	}
	var kind string
	var notes int
	var human, lead bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT r.kind,(SELECT COUNT(*) FROM note WHERE message_id=?),
		EXISTS(SELECT 1 FROM room_member m WHERE m.room_id=r.id AND m.member_kind='user' AND m.left_at IS NULL),
		COALESCE((SELECT ro.cap_is_lead FROM agent a JOIN role ro ON ro.id=a.role_id WHERE a.id=?),0)
		FROM room r WHERE r.id=?`, message.ID, message.AuthorID, message.RoomID).Scan(&kind, &notes, &human, &lead); err != nil {
		return false, err
	}
	if kind == "dm" || kind == "home" || human || lead || notes > 0 {
		return false, nil
	}
	if answersPerson, err := tx.turnHasPersonNote(message.SourceOutID, message.RoomID); err != nil || answersPerson {
		return false, err
	}
	if len(proseMentions(message.Body)) == 0 {
		prior, err := tx.recentReplyWakes(message.RoomID, message.ID)
		if err != nil || prior >= ReplyWakeHops {
			return false, err
		}
	}
	return true, nil
}

// noWakeNote tells an agent, as a system note only it can see, that messages it just wrote woke nobody, so it can say again who should act.
// A turn woken by such a note gets no second one: an agent that again writes to nobody ends there rather than in a loop of notes.
func (tx *Tx) noWakeNote(turn answerTurn, numbers []int64) error {
	if len(numbers) == 0 || turn.mode != "normal" {
		return nil
	}
	var woken bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM turn_note tn JOIN note n ON n.id=tn.note_id JOIN message m ON m.id=n.message_id
		WHERE tn.turn_id=? AND m.author_kind='system' AND m.body LIKE ?)`, turn.id, noWakeNotePrefix+"%").Scan(&woken); err != nil || woken {
		return err
	}
	refs := make([]string, len(numbers))
	for i, no := range numbers {
		refs[i] = fmt.Sprintf("#%d", no)
	}
	body := noWakeNotePrefix + strings.Join(refs, ", ") + " reached no agent and no person. Mention who should act (a live @name in ordinary prose) or reply to their message; do not repeat the whole message, a short pointer to it is enough."
	_, err := tx.InsertMessage(Message{RoomID: turn.roomID, AuthorKind: "system", AuthorID: "system", Body: body, OnlyTargets: []string{turn.agentID}})
	return err
}

// forwardMissingMentions posts a short pointer in a room shared by the
// author and each absent addressee. The grant exposes only this message,
// never the rest of its room, and the targeted note wakes its recipient.
// It reports how many pointers it posted.
func (tx *Tx) forwardMissingMentions(message Message) (int, error) {
	var projectID, title, kind string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,title,kind FROM room WHERE id=?`, message.RoomID).Scan(&projectID, &title, &kind); err != nil {
		return 0, err
	}
	if kind == "dm" {
		return 0, nil
	}
	forwarded := 0
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
			return 0, err
		}
		if seen[recipientID] || recipientID == message.AuthorID {
			continue
		}
		seen[recipientID] = true
		var member bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL)`,
			message.RoomID, recipientID).Scan(&member); err != nil {
			return 0, err
		}
		if member {
			continue
		}
		sharedID, _, err := tx.sharedMentionRoom(message.RoomID, projectID, message.AuthorID, recipientID)
		if err != nil {
			return 0, err
		}
		if sharedID == "" {
			continue
		}
		pointer, err := tx.InsertMessage(Message{RoomID: sharedID, AuthorKind: "system", AuthorID: "system",
			Body:        fmt.Sprintf("%s mentioned you in %s: #%d", authorLabel(tx.ctx, tx.tx, "agent", message.AuthorID), title, message.No),
			OnlyTargets: []string{recipientID}})
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO message_reference_grant(source_message_id,target_no) VALUES (?,?)`, pointer.ID, message.No); err != nil {
			return 0, err
		}
		forwarded++
	}
	return forwarded, nil
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

// handoffOutcome is the outcome a message stores: a handoff's own, nothing for any other message (the parser already dropped one there).
func handoffOutcome(item answer.Message) string {
	if !item.Handoff {
		return ""
	}
	return item.Outcome
}
