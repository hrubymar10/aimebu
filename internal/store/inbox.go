package store

import (
	"context"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/answer"
)

// InboxEntry is the owner/operator's view of an open question or approval.
type InboxEntry struct {
	ID            string `json:"id"`
	ProjectID     string `json:"project_id"`
	RoomID        string `json:"room_id"`
	RoomLifecycle string `json:"room_lifecycle"`
	MessageID     string `json:"message_id,omitempty"`
	AgentID       string `json:"agent_id,omitempty"`
	TurnID        string `json:"turn_id,omitempty"`
	TurnExec      string `json:"turn_exec,omitempty"`
	TurnOutcome   string `json:"turn_outcome,omitempty"`
	Evidence      string `json:"evidence,omitempty"`
	// ResultText is what the harness said was wrong, as a sentence, when it said (Claude Code's error result); Evidence holds it inside the
	// raw output. DefiniteFailure says the turn failed before it did anything, so Retry is safe and Resume cannot help.
	ResultText        string                `json:"result_text,omitempty"`
	DefiniteFailure   bool                  `json:"definite_failure,omitempty"`
	StartFailure      bool                  `json:"start_failure,omitempty"`
	DiagnosticReport  json.RawMessage       `json:"diagnostic_report,omitempty"`
	RecoveryPhase     string                `json:"recovery_phase,omitempty"`
	HeldReason        string                `json:"held_reason,omitempty"`
	DeliveryError     string                `json:"delivery_error,omitempty"`
	CandidatePreview  string                `json:"candidate_preview,omitempty"`
	Kind              string                `json:"kind"`
	CanDecide         bool                  `json:"can_decide"`
	Question          string                `json:"question"`
	Options           []string              `json:"options"`
	FreeText          bool                  `json:"free_text"`
	ProposedAnswers   []string              `json:"proposed_answers,omitempty"`
	OpenQuestions     []answer.OpenQuestion `json:"open_questions,omitempty"`
	State             string                `json:"state"`
	Seen              bool                  `json:"seen"`
	Rev               int64                 `json:"rev"`
	CreatedAt         string                `json:"created_at"`
	BoundaryPending   bool                  `json:"boundary_pending,omitempty"`
	BoundaryMachine   string                `json:"boundary_machine,omitempty"`
	BoundaryPath      string                `json:"boundary_path,omitempty"`
	BoundaryRequester string                `json:"boundary_requester,omitempty"`
	// NoteID is the shared agent note an approval item asks about (see note_inbox.go); empty for every other item.
	NoteID string `json:"note_id,omitempty"`
}

type inboxAsk struct {
	Text            string                `json:"text"`
	Options         []string              `json:"options"`
	FreeText        bool                  `json:"free_text"`
	Phase           string                `json:"phase,omitempty"`
	Preview         string                `json:"candidate_preview,omitempty"`
	ProposedAnswers []string              `json:"proposed_answers,omitempty"`
	OpenQuestions   []answer.OpenQuestion `json:"open_questions,omitempty"`
	// TargetRooms lists the rooms the held reply of a delivery_failed item meant to reach (see deliveryFailedInbox). A later committed reply
	// that reaches one of them settles the item, even when the card itself was raised in the turn's room.
	TargetRooms []string `json:"target_rooms,omitempty"`
}

// inboxFromSQL and inboxVisibleSQL say which items a person can access (two ? placeholders: the user, twice). They are shared by the full
// inbox read, by the read of the superseded asks that keep their answer controls, and by the per-room unseen count, so the badge uses the
// same visibility rules. The joins the full read adds between them (the message and the turn, both by primary key) neither add nor drop rows.
// The two item sets each add their own state filter: inboxWhereSQL is the Inbox itself (open items), inboxSupersededWhereSQL the superseded
// agent asks that leave the Inbox but stay answerable in the chat.
const inboxFromSQL = `FROM inbox_item i JOIN room r ON r.id=i.room_id JOIN membership member ON member.project_id=i.project_id
        AND member.user_id=?`

// A closed room's items never show: nothing in it can be acted on, and closing settles them (the usage kinds belong to the project, not the room).
// A recovery still open there is one the settle pass left on purpose (a lost turn with no proof its process is gone): only a person can accept that
// risk, so it stays visible.
// An attention message (needs_attention, proposed answers, open questions; no structured ask) keeps its item in every room, so the chat shows the
// card's live controls, but only the project's home room lists it in the Inbox and counts it: a person who is in another room reads it there.
// attentionInbox marks such an item (options_json "attention"); a real ask, a recovery and the rest are listed wherever they come from, and an
// attention item stored before the mark was added stays listed until it settles.
const inboxHomeOnlySQL = `
        AND NOT (i.kind IN ('question','approval') AND r.kind<>'home' AND COALESCE(json_extract(i.options_json,'$.attention'),0)=1)`
const inboxWhereSQL = `WHERE i.state='open' AND i.lead_due_at IS NULL AND i.kind IN ('question','approval','recovery','delivery_failed','usage_info','usage_limit')
        AND (r.lifecycle<>'closed' OR i.kind IN ('usage_info','usage_limit','recovery'))` + inboxHomeOnlySQL + inboxVisibleSQL
const inboxSupersededWhereSQL = `WHERE i.state='superseded' AND i.lead_due_at IS NULL AND i.kind IN ('question','approval') AND i.requester_kind='agent'` + inboxVisibleSQL
const inboxVisibleSQL = `
		AND (member.role IN ('owner','operator') OR (r.kind='dm' AND r.project_id!=` + agentDMProjectSQL + ` AND i.kind NOT IN ('usage_info','usage_limit')))
		AND (i.kind!='approval' OR member.role='owner' OR r.kind='dm' AND r.project_id!=` + agentDMProjectSQL + `)
		AND (i.requester_id NOT LIKE 'note:%' OR EXISTS (SELECT 1 FROM user u WHERE u.id=member.user_id AND u.disabled_at IS NULL
			AND (u.admin=1 OR u.id=(SELECT id FROM user ORDER BY created_at,id LIMIT 1))))
		AND (member.role='owner' OR NOT EXISTS (SELECT 1 FROM boundary_pending b WHERE b.inbox_id=i.id))
		AND (r.kind!='dm' OR EXISTS (SELECT 1 FROM room_member own WHERE own.room_id=r.id
			AND own.member_kind='user' AND own.member_id=? AND own.left_at IS NULL)
			OR (i.kind='recovery' AND member.role IN ('owner','operator'))
			OR (i.kind='approval' AND member.role='owner' AND EXISTS(SELECT 1 FROM boundary_pending b WHERE b.inbox_id=i.id)))`

// InboxForUser is the Inbox: the open items a person can see. A superseded ask is not among them (its list and its badge count drop it).
func (s *Store) InboxForUser(ctx context.Context, userID string) ([]InboxEntry, error) {
	return s.inboxEntriesForUser(ctx, userID, inboxWhereSQL)
}

// SupersededAsksForUser is the set a superseded ask leaves the Inbox for: still answerable in the chat, so the same rows (state included)
// reach the page's answer controls while the Inbox skips them.
func (s *Store) SupersededAsksForUser(ctx context.Context, userID string) ([]InboxEntry, error) {
	return s.inboxEntriesForUser(ctx, userID, inboxSupersededWhereSQL)
}

func (s *Store) inboxEntriesForUser(ctx context.Context, userID, where string) ([]InboxEntry, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	rows, err := s.readDB.QueryContext(ctx, `SELECT i.id,i.project_id,i.room_id,r.lifecycle,COALESCE(i.message_id,''),COALESCE(i.agent_id,''),COALESCE(i.turn_id,''),
		COALESCE(t.exec,''),COALESCE(t.outcome,''),COALESCE((SELECT json_extract(e.payload_json,'$.evidence') FROM event e WHERE e.turn_id=i.turn_id AND e.kind='turn_process_exit' ORDER BY e.seq DESC LIMIT 1),''),
		COALESCE((SELECT json_extract(e.payload_json,'$.result_text') FROM event e WHERE e.turn_id=i.turn_id AND e.kind='turn_process_exit' ORDER BY e.seq DESC LIMIT 1),''),
		COALESCE((SELECT json_extract(e.payload_json,'$.definite_failure') FROM event e WHERE e.turn_id=i.turn_id AND e.kind='turn_process_exit' ORDER BY e.seq DESC LIMIT 1),'')='1',
		COALESCE((SELECT json_extract(e.payload_json,'$.start_failure') FROM event e WHERE e.turn_id=i.turn_id AND e.kind='turn_process_exit' ORDER BY e.seq DESC LIMIT 1),'')='1',
		COALESCE((SELECT d.report_json FROM turn d WHERE d.parent_turn_id=i.turn_id AND d.mode='diagnostic' AND d.report_json IS NOT NULL ORDER BY d.id DESC LIMIT 1),''),
		CASE WHEN i.kind='delivery_failed' THEN COALESCE(
			NULLIF((SELECT json_extract(r.result_json,'$.held_reason') FROM receipt r WHERE r.turn_id=i.turn_id AND r.answer_rev=0),''),
			NULLIF((SELECT json_extract(r.result_json,'$.held_reason') FROM turn child JOIN receipt r ON r.turn_id=child.id
				WHERE child.parent_turn_id=i.turn_id AND child.mode='correction' ORDER BY r.answer_rev DESC LIMIT 1),''),
			'') ELSE '' END,
		CASE WHEN i.kind='delivery_failed' THEN COALESCE(
			NULLIF((SELECT json_extract(r.result_json,'$.error') FROM receipt r WHERE r.turn_id=i.turn_id AND r.answer_rev=0),''),
			NULLIF((SELECT json_extract(r.result_json,'$.error') FROM turn child JOIN receipt r ON r.turn_id=child.id
				WHERE child.parent_turn_id=i.turn_id AND child.mode='correction' ORDER BY r.answer_rev DESC LIMIT 1),''),
			'') ELSE '' END,i.kind,
		COALESCE(i.options_json,''),COALESCE(msg.body,''),i.state,i.rev,i.created_at,
		member.role,CASE WHEN r.project_id=`+agentDMProjectSQL+` THEN 'agent_chat' ELSE r.kind END,
		EXISTS(SELECT 1 FROM boundary_pending b WHERE b.inbox_id=i.id),
		COALESCE((SELECT b.machine_id FROM boundary_pending b WHERE b.inbox_id=i.id),''),
		COALESCE((SELECT b.canonical_path FROM boundary_pending b WHERE b.inbox_id=i.id),''),
		COALESCE((SELECT b.requested_by_id FROM boundary_pending b WHERE b.inbox_id=i.id),''),
		CASE WHEN i.kind='approval' AND i.requester_kind='system' AND i.requester_id LIKE 'note:%' THEN substr(i.requester_id,6) ELSE '' END,
		EXISTS(SELECT 1 FROM inbox_item_seen seen WHERE seen.item_id=i.id AND seen.user_id=?)
        `+inboxFromSQL+`
        LEFT JOIN message msg ON msg.id=i.message_id
		LEFT JOIN turn t ON t.id=i.turn_id
		`+where+`
        ORDER BY i.created_at,i.id`, userID, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []InboxEntry{}
	for rows.Next() {
		var item InboxEntry
		var options, fallback, report, memberRole, roomKind string
		if err := rows.Scan(&item.ID, &item.ProjectID, &item.RoomID, &item.RoomLifecycle, &item.MessageID, &item.AgentID, &item.TurnID,
			&item.TurnExec, &item.TurnOutcome, &item.Evidence, &item.ResultText, &item.DefiniteFailure, &item.StartFailure, &report, &item.HeldReason, &item.DeliveryError, &item.Kind, &options, &fallback, &item.State, &item.Rev, &item.CreatedAt, &memberRole, &roomKind,
			&item.BoundaryPending, &item.BoundaryMachine, &item.BoundaryPath, &item.BoundaryRequester, &item.NoteID, &item.Seen); err != nil {
			return nil, err
		}
		item.CanDecide = memberRole == "owner" || memberRole == "operator" && !item.BoundaryPending || roomKind == "dm" && (item.Kind == "question" || item.Kind == "delivery_failed" || item.Kind == "approval" && !item.BoundaryPending)
		if json.Valid([]byte(report)) {
			item.DiagnosticReport = json.RawMessage(report)
		}
		var ask inboxAsk
		if options != "" {
			if err := json.Unmarshal([]byte(options), &ask); err != nil {
				return nil, err
			}
		}
		item.Question = ask.Text
		if item.Question == "" {
			item.Question = fallback
		}
		if item.Question == "" && item.Kind == "delivery_failed" {
			item.Question = "An agent reply could not be delivered. Review the room."
		}
		if item.Question == "" && item.Kind == "recovery" {
			item.Question = "An agent needs recovery review."
		}
		item.Options = ask.Options
		item.ProposedAnswers = ask.ProposedAnswers
		item.OpenQuestions = ask.OpenQuestions
		item.CandidatePreview = ask.Preview
		item.DeliveryError = candidatePreview(item.DeliveryError)
		item.RecoveryPhase = ask.Phase
		if item.Options == nil {
			item.Options = []string{}
		}
		item.FreeText = ask.FreeText
		entries = append(entries, item)
	}
	return entries, rows.Err()
}
