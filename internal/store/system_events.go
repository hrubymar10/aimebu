package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/goccy/go-json"
)

// systemEventKinds are the operational events a person watching the system
// wants to see: membership, lifecycle, recovery, quota, machine, and access
// changes. Chatty bookkeeping (message_created, note_queued, reactions,
// dispatch records, inbox_item_*) is left out; the chat and the Inbox already
// show those.
var systemEventKinds = []string{
	"agent_created", "agent_stopped", "agent_revoked", "agent_key_rotated",
	"agent_spawn_requested", "agent_spawn_confirmed", "agent_spawn_failed",
	"agent_control_requested", "agent_control_applied",
	"agent_switch_requested", "agent_switch_starting", "agent_switch_completed", "agent_switch_failed",
	"agent_turn_budget_exceeded",
	"room_created", "room_renamed", "room_closing", "room_closed", "room_reopened", "room_close_blocked",
	"room_member_joined", "room_member_left",
	"room_worktree_requested", "room_worktree_updated", "room_worktree_cancelled",
	"project_created", "project_updated", "project_member_joined", "project_member_changed", "project_settings_updated",
	"provider_limit_detected", "provider_limit_cleared", "native_session_reset",
	"turn_lost", "turn_never_started", "turn_process_exit", "special_turn_abandoned",
	"diagnostic_requested", "answer_held",
	"recovery_decision", "recovery_override", "recovery_overlap_override", "recovery_quiescent",
	"recovery_continuation_cancelled", "recovery_continuation_requeued",
	"usage_limit_threshold", "usage_limit_reset", "usage_limit_decided",
	"scope_control_requested", "notes_dropped",
	"machine_created", "machine_approved", "machine_revoked", "machine_key_rotated", "machine_pairing_created",
	"user_created", "user_deactivated", "user_admin_changed", "user_invite_created", "user_invite_redeemed",
	"credential_alert",
	"quota_switch", "quota_switch_failed", "quota_reset_seen", "quota_switcher_tripped",
	"limit_guard_soft", "limit_guard_hard", "limit_guard_clear",
}

// SystemEventKinds lists the event kinds the system-events feed shows, so the
// web client can filter the live stream with the same set.
func SystemEventKinds() []string { return append([]string(nil), systemEventKinds...) }

// RecentSystemEvents returns the newest operational events the user may see,
// newest first, under the same eventVisible rule as the live event stream.
func (s *Store) RecentSystemEvents(ctx context.Context, userID string, limit int) ([]Event, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 500 {
		return nil, errors.New("invalid system event limit")
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(systemEventKinds)), ",")
	args := []any{userID}
	for _, kind := range systemEventKinds {
		args = append(args, kind)
	}
	args = append(args, userID, userID, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT e.id,e.seq,e.kind,COALESCE(e.project_id,''),COALESCE(e.room_id,''),COALESCE(e.agent_id,''),COALESCE(e.turn_id,''),e.payload_json,e.created_at
		FROM event e LEFT JOIN membership m ON m.project_id=e.project_id AND m.user_id=? LEFT JOIN room r ON r.id=e.room_id
		WHERE e.kind IN (`+marks+`) AND `+eventVisible+`
		ORDER BY e.seq DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.Seq, &e.Kind, &e.ProjectID, &e.RoomID, &e.AgentID, &e.TurnID, &e.PayloadJSON, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

const debugTextMax = 8 << 10

type MessageDebugNote struct {
	ID         string             `json:"id"`
	AgentID    string             `json:"agent_id"`
	AgentName  string             `json:"agent_name,omitempty"`
	State      string             `json:"state"`
	WakeReason string             `json:"wake_reason"`
	CreatedAt  string             `json:"created_at"`
	Turns      []MessageDebugTurn `json:"turns"`
}

type MessageDebugTurn struct {
	ID       string            `json:"id"`
	Mode     string            `json:"mode"`
	Attempt  int               `json:"attempt"`
	Exec     string            `json:"exec"`
	Outcome  string            `json:"outcome"`
	Settle   string            `json:"settle"`
	Receipts []json.RawMessage `json:"receipts"`
}

type MessageDebug struct {
	Message    map[string]any     `json:"message"`
	Addressed  []string           `json:"addressed_to"`
	Addressing AddressingTrace    `json:"addressing"`
	Notes      []MessageDebugNote `json:"notes"`
	Events     []Event            `json:"events"`
}

// MessageDebug assembles what the server knows about one message: the stored
// row, who it addressed, the wake note each agent got (or the reason it did
// not), the turns that carried it and their receipts, and the related events.
// The caller must already have checked room access.
func (s *Store) MessageDebug(ctx context.Context, messageID string) (MessageDebug, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return MessageDebug{}, err
	}
	var id, roomID, authorKind, authorID, body, targets, createdAt string
	var seq int64
	var needs bool
	var replyTo, threadRoot, askJSON, proposed, questions, plan, appendix, sourceOut sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id,seq,room_id,author_kind,author_id,body,reply_to,thread_root,targets_json,ask_json,needs_attention,proposed_answers_json,open_questions_json,visual_plan_json,appendix_pages_json,source_out_id,created_at FROM message WHERE id=?`, messageID).
		Scan(&id, &seq, &roomID, &authorKind, &authorID, &body, &replyTo, &threadRoot, &targets, &askJSON, &needs, &proposed, &questions, &plan, &appendix, &sourceOut, &createdAt)
	if err != nil {
		return MessageDebug{}, err
	}
	message := map[string]any{"id": id, "seq": seq, "room_id": roomID, "author_kind": authorKind, "author_id": authorID, "body": truncateRunes(body),
		"reply_to": nullString(replyTo), "thread_root": nullString(threadRoot), "needs_attention": needs, "source_out_id": nullString(sourceOut), "created_at": createdAt,
		"targets": rawOrString(targets), "ask": rawOrNil(askJSON), "proposed_answers": rawOrNil(proposed), "open_questions": rawOrNil(questions),
		"visual_plan": rawOrNil(plan), "appendix_pages": rawOrNil(appendix)}
	debug := MessageDebug{Message: message, Addressed: []string{}, Notes: []MessageDebugNote{}, Events: []Event{}}
	var addressed []string
	if json.Unmarshal([]byte(targets), &addressed) == nil && addressed != nil {
		debug.Addressed = addressed
	}
	debug.Addressing, err = s.messageAddressingTrace(ctx, messageID, roomID, authorID, authorKind, body, replyTo.String, debug.Addressed)
	if err != nil {
		return MessageDebug{}, err
	}

	rows, err := s.db.QueryContext(ctx, `SELECT n.id,n.agent_id,COALESCE(a.slug,''),n.state,n.wake_reason,n.created_at FROM note n LEFT JOIN agent a ON a.id=n.agent_id WHERE n.message_id=? ORDER BY n.created_at,n.id`, messageID)
	if err != nil {
		return MessageDebug{}, err
	}
	for rows.Next() {
		var note MessageDebugNote
		if err := rows.Scan(&note.ID, &note.AgentID, &note.AgentName, &note.State, &note.WakeReason, &note.CreatedAt); err != nil {
			rows.Close()
			return MessageDebug{}, err
		}
		note.Turns = []MessageDebugTurn{}
		debug.Notes = append(debug.Notes, note)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return MessageDebug{}, err
	}
	rows.Close()
	for i := range debug.Notes {
		turns, err := s.db.QueryContext(ctx, `SELECT t.id,t.mode,t.attempt,t.exec,t.outcome,t.settle FROM turn_note tn JOIN turn t ON t.id=tn.turn_id WHERE tn.note_id=? ORDER BY t.attempt,t.id`, debug.Notes[i].ID)
		if err != nil {
			return MessageDebug{}, err
		}
		for turns.Next() {
			turn := MessageDebugTurn{Receipts: []json.RawMessage{}}
			if err := turns.Scan(&turn.ID, &turn.Mode, &turn.Attempt, &turn.Exec, &turn.Outcome, &turn.Settle); err != nil {
				turns.Close()
				return MessageDebug{}, err
			}
			debug.Notes[i].Turns = append(debug.Notes[i].Turns, turn)
		}
		if err := turns.Err(); err != nil {
			turns.Close()
			return MessageDebug{}, err
		}
		turns.Close()
		for j := range debug.Notes[i].Turns {
			receipts, err := s.db.QueryContext(ctx, `SELECT result_json FROM receipt WHERE turn_id=? ORDER BY answer_rev`, debug.Notes[i].Turns[j].ID)
			if err != nil {
				return MessageDebug{}, err
			}
			for receipts.Next() {
				var result string
				if err := receipts.Scan(&result); err != nil {
					receipts.Close()
					return MessageDebug{}, err
				}
				debug.Notes[i].Turns[j].Receipts = append(debug.Notes[i].Turns[j].Receipts, rawOrStringJSON(result))
			}
			if err := receipts.Err(); err != nil {
				receipts.Close()
				return MessageDebug{}, err
			}
			receipts.Close()
		}
	}

	events, err := s.db.QueryContext(ctx, `SELECT id,seq,kind,COALESCE(project_id,''),COALESCE(room_id,''),COALESCE(agent_id,''),COALESCE(turn_id,''),payload_json,created_at FROM event
		WHERE room_id=? AND (json_extract(payload_json,'$.message_id')=? OR (kind='note_queued' AND json_extract(payload_json,'$.note_id') IN (SELECT id FROM note WHERE message_id=?)))
		ORDER BY seq LIMIT 200`, roomID, messageID, messageID)
	if err != nil {
		return MessageDebug{}, err
	}
	defer events.Close()
	for events.Next() {
		var e Event
		if err := events.Scan(&e.ID, &e.Seq, &e.Kind, &e.ProjectID, &e.RoomID, &e.AgentID, &e.TurnID, &e.PayloadJSON, &e.CreatedAt); err != nil {
			return MessageDebug{}, err
		}
		debug.Events = append(debug.Events, e)
	}
	return debug, events.Err()
}

func nullString(value sql.NullString) any {
	if !value.Valid {
		return nil
	}
	return value.String
}

func rawOrNil(value sql.NullString) any {
	if !value.Valid || value.String == "" {
		return nil
	}
	return rawOrString(value.String)
}

// rawOrString embeds stored JSON text as structured data, or the text itself
// when it is not valid JSON, truncated so one oversized row cannot swamp the
// response.
func rawOrString(text string) any {
	if len(text) > debugTextMax {
		return truncateRunes(text)
	}
	var value any
	if json.Unmarshal([]byte(text), &value) == nil {
		return value
	}
	return text
}

// truncateRunes cuts text at the debug size cap without splitting a rune.
func truncateRunes(text string) string {
	if len(text) <= debugTextMax {
		return text
	}
	cut := debugTextMax
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "… (truncated)"
}

func rawOrStringJSON(text string) json.RawMessage {
	encoded, _ := json.Marshal(rawOrString(text))
	return encoded
}
