package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"

	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// AuthorizeAgentOutputRead decides whether an agent may read another agent's conversation log through the agent_output tool, and says
// where that log is kept. The reader must be allowed to manage the target under the stop_agent rule: an active lead may read any non-lead
// agent of its project, the flow's coordinator (the dispatcher) only an agent its flow lets it manage, one it spawned or one in a room it
// created. sql.ErrNoRows when the target is not an agent of the reader's project (another project's agent is not told apart from no agent
// at all, as the person's log viewer does), ErrForbidden for a reader that may not read it (a worker, a stopped lead, a lead target, the
// reader itself).
func (s *Store) AuthorizeAgentOutputRead(ctx context.Context, readerID, targetID string) (machineID string, err error) {
	err = s.Tx(ctx, func(tx *Tx) error {
		var readerProject, targetProject, roleKey string
		var targetLead bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id FROM agent WHERE id=?`, readerID).Scan(&readerProject); err != nil {
			return err
		}
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.project_id,a.machine_id,r.role_key,r.cap_is_lead FROM agent a JOIN role r ON r.id=a.role_id WHERE a.id=?`, targetID).
			Scan(&targetProject, &machineID, &roleKey, &targetLead); err != nil {
			return err
		}
		if targetProject != readerProject {
			return sql.ErrNoRows
		}
		if err := tx.authorizeSpawnActor(readerProject, "agent", readerID, false); err != nil {
			return err
		}
		if targetLead || targetID == readerID {
			return ErrForbidden
		}
		return tx.flowActorMayTargetAgent(readerProject, readerID, targetID, roleKey)
	})
	if errors.Is(err, ErrTeamFlowRule) {
		err = ErrForbidden
	}
	return machineID, err
}

// TurnShownToAgent says whether what a turn of an agent said may be shown to another agent (a lead reading its worker's output), and which
// rooms the turn served. The rooms are those of turnServedRooms, the rule TurnWithheldFrom uses too: the turn's own room, the room of every
// message it was given, every room it read through a tool or was quoted in its context (turn_room_read), and, for a turn older than that
// record (migration 129) that has none, every room the agent was a member of when the turn started, private chats included. withheld is
// true when the reader may not read one of them under the rule every agent read uses (room_history's): a room of the reader's project, or
// a chat between agents, that is neither a private chat nor restricted, or one the reader is an active member of. A removed room is not
// readable. known is false when the server cannot place the turn: the agent has no such turn, or the turn predates the record and the
// agent's membership then is not recoverable; such a turn is withheld too. rooms is what a caller that shows the turn records against the
// reader's own turn (RecordTurnRoomReads), so the reader's log is withheld in turn from whoever may not read them.
func (s *Store) TurnShownToAgent(ctx context.Context, turnID, agentID, readerID string) (known, withheld bool, rooms []string, err error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return false, false, nil, err
	}
	placed, rooms, err := s.turnServedRooms(ctx, turnID, agentID)
	if err != nil {
		return false, false, nil, err
	}
	if !placed {
		return false, true, nil, nil
	}
	var readerProject string
	if err := s.db.QueryRowContext(ctx, `SELECT project_id FROM agent WHERE id=?`, readerID).Scan(&readerProject); err != nil {
		return false, false, nil, err
	}
	for _, roomID := range rooms {
		var readable bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM room r WHERE r.id=? AND r.project_id IN (?,`+agentDMProjectSQL+`)
			AND (r.kind<>'dm' AND r.restricted=0 OR EXISTS(SELECT 1 FROM room_member m WHERE m.room_id=r.id AND m.member_kind='agent' AND m.member_id=? AND m.left_at IS NULL)))`,
			roomID, readerProject, readerID).Scan(&readable); err != nil {
			return false, false, nil, err
		}
		if !readable {
			return true, true, rooms, nil
		}
	}
	return true, false, rooms, nil
}

// RecordTurnRoomReads notes that one turn was given content of these rooms; it is RecordAgentTurnRoomReads for a caller that knows the
// turn (a delivery that carries a worker's output to its lead records it against the lead's turn being delivered).
func (s *Store) RecordTurnRoomReads(ctx context.Context, turnID string, roomIDs ...string) error {
	if turnID == "" || len(roomIDs) == 0 {
		return nil
	}
	return s.Tx(ctx, func(tx *Tx) error {
		for _, roomID := range slices.Compact(slices.Sorted(slices.Values(roomIDs))) {
			if roomID == "" {
				continue
			}
			if _, err := tx.Exec(`INSERT OR IGNORE INTO turn_room_read(turn_id,room_id) SELECT ?,? WHERE EXISTS (SELECT 1 FROM turn WHERE id=?) AND EXISTS (SELECT 1 FROM room WHERE id=?)`,
				turnID, roomID, turnID, roomID); err != nil {
				return err
			}
		}
		return nil
	})
}

// noticeOutputLookback bounds how far after a notice's message its event is looked for. The event is appended in the same transaction,
// after the message, and messages and events share one sequence, so it is a few rows on.
const noticeOutputLookback = 200

// noteOutputSources marks each delivered note that is a notice about a worker (a silent turn, a worker recovery handed to the lead) with
// the worker and the turn the notice is about, so the server can add the end of that turn's conversation to the note
// (protocol.DeliveryNote.WorkerOutput). reasons[i] is notes[i]'s wake reason. The source is found from the event the notice wrote: a
// silent_turn event names its notice messages, a recovery_handed_to_lead event its one.
func (tx *Tx) noteOutputSources(notes []protocol.DeliveryNote, reasons []string) error {
	for i := range notes {
		var kind, key string
		switch reasons[i] {
		case silentTurnEvent:
			kind, key = silentTurnEvent, "$.notices"
		case "worker_recovery":
			kind, key = "recovery_handed_to_lead", "$.message_id"
		default:
			continue
		}
		var seq int64
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT seq FROM message WHERE id=?`, notes[i].MessageID).Scan(&seq); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return err
		}
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT COALESCE(agent_id,''),COALESCE(turn_id,''),COALESCE(json_extract(payload_json,?),'') FROM event
			WHERE seq>? AND seq<=? AND kind=? ORDER BY seq`, key, seq, seq+noticeOutputLookback, kind)
		if err != nil {
			return err
		}
		for rows.Next() {
			var agentID, turnID, ids string
			if err := rows.Scan(&agentID, &turnID, &ids); err != nil {
				rows.Close()
				return err
			}
			if agentID != "" && turnID != "" && slices.Contains(strings.Split(ids, ","), notes[i].MessageID) {
				notes[i].OutputSource = &protocol.OutputSource{AgentID: agentID, TurnID: turnID}
				break
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// NoticeSource is a notice about a worker's turn and the agent it was written for.
type NoticeSource struct {
	AgentID     string // the worker
	TurnID      string
	RecipientID string
}

// noticeScanLimit is how far back the first scan of NoticeSourcesAfter (after 0) looks.
const noticeScanLimit = 200

// NoticeSourcesAfter lists the notices about a worker's turn written after event seq after: each agent a silent_turn event woke, and the
// lead a recovery_handed_to_lead event names. last is the seq to scan after next time. after 0 (a server that just started) looks only at
// the latest noticeScanLimit events, so a restart does not read the output of every old notice.
func (s *Store) NoticeSourcesAfter(ctx context.Context, after int64) (notices []NoticeSource, last int64, err error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, 0, err
	}
	var top int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0) FROM event`).Scan(&top); err != nil {
		return nil, 0, err
	}
	if after <= 0 {
		after = max(top-noticeScanLimit, 0)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT seq,kind,COALESCE(agent_id,''),COALESCE(turn_id,''),COALESCE(json_extract(payload_json,'$.woken'),''),COALESCE(json_extract(payload_json,'$.lead_id'),'')
		FROM event WHERE seq>? AND seq<=? AND kind IN (?,'recovery_handed_to_lead') ORDER BY seq`, after, top, silentTurnEvent)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int64
		var kind, agentID, turnID, woken, leadID string
		if err := rows.Scan(&seq, &kind, &agentID, &turnID, &woken, &leadID); err != nil {
			return nil, 0, err
		}
		if agentID == "" || turnID == "" {
			continue
		}
		var recipients []string
		switch kind {
		case silentTurnEvent:
			recipients = strings.Split(woken, ",")
		case "recovery_handed_to_lead":
			recipients = []string{leadID}
		}
		for _, recipient := range recipients {
			if recipient != "" {
				notices = append(notices, NoticeSource{AgentID: agentID, TurnID: turnID, RecipientID: recipient})
			}
		}
	}
	return notices, top, rows.Err()
}
