package store

import (
	"context"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/answer"
)

// InboxEntry is the owner/operator's view of an open question or approval.
type InboxEntry struct {
	ID                string                `json:"id"`
	ProjectID         string                `json:"project_id"`
	RoomID            string                `json:"room_id"`
	RoomLifecycle     string                `json:"room_lifecycle"`
	MessageID         string                `json:"message_id,omitempty"`
	AgentID           string                `json:"agent_id,omitempty"`
	TurnID            string                `json:"turn_id,omitempty"`
	TurnExec          string                `json:"turn_exec,omitempty"`
	TurnOutcome       string                `json:"turn_outcome,omitempty"`
	Evidence          string                `json:"evidence,omitempty"`
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
	Rev               int64                 `json:"rev"`
	CreatedAt         string                `json:"created_at"`
	BoundaryPending   bool                  `json:"boundary_pending,omitempty"`
	BoundaryMachine   string                `json:"boundary_machine,omitempty"`
	BoundaryPath      string                `json:"boundary_path,omitempty"`
	BoundaryRequester string                `json:"boundary_requester,omitempty"`
}

type inboxAsk struct {
	Text            string                `json:"text"`
	Options         []string              `json:"options"`
	FreeText        bool                  `json:"free_text"`
	Phase           string                `json:"phase,omitempty"`
	Preview         string                `json:"candidate_preview,omitempty"`
	ProposedAnswers []string              `json:"proposed_answers,omitempty"`
	OpenQuestions   []answer.OpenQuestion `json:"open_questions,omitempty"`
}

// inboxFromSQL and inboxWhereSQL say which open items a person sees (two ? placeholders: the user, twice). They are shared by the full
// inbox read and by the per-room count, so the count cannot drift from what the inbox lists. The joins the full read adds between them
// (the message and the turn, both by primary key) neither add nor drop rows.
const inboxFromSQL = `FROM inbox_item i JOIN room r ON r.id=i.room_id JOIN membership member ON member.project_id=i.project_id
        AND member.user_id=?`
const inboxWhereSQL = `WHERE i.state='open' AND i.kind IN ('question','approval','recovery','delivery_failed','usage_info','usage_limit')
		AND (member.role IN ('owner','operator') OR (r.kind='dm' AND i.kind NOT IN ('usage_info','usage_limit')))
		AND (i.kind!='approval' OR member.role='owner' OR r.kind='dm')
		AND (member.role='owner' OR NOT EXISTS (SELECT 1 FROM boundary_pending b WHERE b.inbox_id=i.id))
		AND (r.kind!='dm' OR EXISTS (SELECT 1 FROM room_member own WHERE own.room_id=r.id
			AND own.member_kind='user' AND own.member_id=? AND own.left_at IS NULL)
			OR (i.kind='recovery' AND member.role IN ('owner','operator'))
			OR (i.kind='approval' AND member.role='owner' AND EXISTS(SELECT 1 FROM boundary_pending b WHERE b.inbox_id=i.id)))`

func (s *Store) InboxForUser(ctx context.Context, userID string) ([]InboxEntry, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT i.id,i.project_id,i.room_id,r.lifecycle,COALESCE(i.message_id,''),COALESCE(i.agent_id,''),COALESCE(i.turn_id,''),
		COALESCE(t.exec,''),COALESCE(t.outcome,''),COALESCE((SELECT json_extract(e.payload_json,'$.evidence') FROM event e WHERE e.turn_id=i.turn_id AND e.kind='turn_process_exit' ORDER BY e.seq DESC LIMIT 1),''),
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
		member.role,r.kind,
		EXISTS(SELECT 1 FROM boundary_pending b WHERE b.inbox_id=i.id),
		COALESCE((SELECT b.machine_id FROM boundary_pending b WHERE b.inbox_id=i.id),''),
		COALESCE((SELECT b.canonical_path FROM boundary_pending b WHERE b.inbox_id=i.id),''),
		COALESCE((SELECT b.requested_by_id FROM boundary_pending b WHERE b.inbox_id=i.id),'')
        `+inboxFromSQL+`
        LEFT JOIN message msg ON msg.id=i.message_id
		LEFT JOIN turn t ON t.id=i.turn_id
		`+inboxWhereSQL+`
        ORDER BY i.created_at,i.id`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []InboxEntry{}
	for rows.Next() {
		var item InboxEntry
		var options, fallback, report, memberRole, roomKind string
		if err := rows.Scan(&item.ID, &item.ProjectID, &item.RoomID, &item.RoomLifecycle, &item.MessageID, &item.AgentID, &item.TurnID,
			&item.TurnExec, &item.TurnOutcome, &item.Evidence, &report, &item.HeldReason, &item.DeliveryError, &item.Kind, &options, &fallback, &item.State, &item.Rev, &item.CreatedAt, &memberRole, &roomKind,
			&item.BoundaryPending, &item.BoundaryMachine, &item.BoundaryPath, &item.BoundaryRequester); err != nil {
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
