package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
)

var ErrStaleGeneration = errors.New("stale_generation")
var ErrStaleRev = errors.New("stale_rev")
var ErrAgentNotActive = errors.New("agent_not_active")
var ErrNotMember = errors.New("not_member")

type Admission struct {
	TurnID, AgentID, MachineID string
	Generation                 int
	DesiredRev                 int64
	NoteIDs                    []string
}

func (s *Store) AdmitTurn(ctx context.Context, input Admission) error {
	return s.Tx(ctx, func(tx *Tx) error {
		var machineID, projectID, desired, observed, tool, variant string
		var generation int
		var desiredRev int64
		var blocked, revoked bool
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT machine_id,project_id,desired,observed,tool,variant,generation,desired_rev,recovery_block,revoked_at IS NOT NULL FROM agent WHERE id=?`, input.AgentID).Scan(&machineID, &projectID, &desired, &observed, &tool, &variant, &generation, &desiredRev, &blocked, &revoked)
		if err != nil {
			return err
		}
		if machineID != input.MachineID || revoked {
			return ErrForbidden
		}
		var approved, machineRevoked bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT approved,revoked_at IS NOT NULL FROM machine WHERE id=?`, machineID).Scan(&approved, &machineRevoked); err != nil {
			return err
		}
		if !approved || machineRevoked {
			return ErrForbidden
		}
		var switchGate string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COALESCE((SELECT gate FROM dispatch_switch_gate WHERE machine_id=? AND tool=?),'open')`, machineID, tool).Scan(&switchGate); err != nil {
			return err
		}
		if switchGate != "open" {
			return ErrAgentNotActive
		}
		if generation != input.Generation {
			return ErrStaleGeneration
		}
		if desiredRev != input.DesiredRev {
			return ErrStaleRev
		}
		var switchPending bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM agent_switch WHERE agent_id=? AND state IN ('starting','failed'))`, input.AgentID).Scan(&switchPending); err != nil {
			return err
		}
		if switchPending {
			return ErrAgentNotActive
		}
		var turnAgent, roomID, exec, mode string
		var parentID sql.NullString
		var turnGeneration int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT agent_id,generation,room_id,exec,mode,parent_turn_id FROM turn WHERE id=?`, input.TurnID).Scan(&turnAgent, &turnGeneration, &roomID, &exec, &mode, &parentID); err != nil {
			return err
		}
		if desired != "active" || (blocked && mode == "normal") || (observed != "idle" && (mode != "correction" || observed != "in_turn")) {
			return ErrAgentNotActive
		}
		if mode == "correction" {
			if !parentID.Valid {
				return ErrInvalidTransition
			}
			var parentExec, parentSettle, resultJSON string
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT t.exec,t.settle,r.result_json FROM turn t JOIN receipt r ON r.turn_id=t.id AND r.answer_rev=0 WHERE t.id=?`, parentID.String).Scan(&parentExec, &parentSettle, &resultJSON); err != nil {
				return err
			}
			var parentReceipt AnswerReceipt
			if err := json.Unmarshal([]byte(resultJSON), &parentReceipt); err != nil {
				return err
			}
			if (parentExec != "exited" && parentExec != "killed") || parentSettle != "pending" || parentReceipt.Result != "correction_requested" {
				return ErrInvalidTransition
			}
		}
		if turnAgent != input.AgentID || turnGeneration != generation {
			return ErrScopeMismatch
		}
		if exec != "pending" {
			return ErrInvalidTransition
		}
		var other int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM turn WHERE agent_id=? AND id<>? AND exec IN ('pending','running')`, input.AgentID, input.TurnID).Scan(&other); err != nil {
			return err
		}
		if other != 0 {
			return ErrAgentNotActive
		}
		if mode == "normal" {
			var unsettled int
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM turn WHERE agent_id=? AND id<>? AND settle='pending'`, input.AgentID, input.TurnID).Scan(&unsettled); err != nil {
				return err
			}
			if unsettled != 0 {
				return ErrAgentNotActive
			}
		}
		var roomProject, lifecycle, roomGate, projectGate string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT r.project_id,r.lifecycle,r.gate,p.gate FROM room r JOIN project p ON p.id=r.project_id WHERE r.id=?`, roomID).Scan(&roomProject, &lifecycle, &roomGate, &projectGate); err != nil {
			return err
		}
		if roomProject != projectID {
			return ErrScopeMismatch
		}
		if lifecycle != "open" || roomGate != "open" || projectGate != "open" {
			return ErrAgentNotActive
		}
		var membership int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL`, roomID, input.AgentID).Scan(&membership); err != nil {
			return err
		}
		if membership != 1 {
			return ErrNotMember
		}
		var writerRole bool
		var writerAgent, writerState sql.NullString
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT role.cap_singleton_writer,checkout.writer_agent_id,checkout.writer_state FROM agent JOIN role ON role.id=agent.role_id JOIN checkout ON checkout.id=agent.checkout_id WHERE agent.id=?`, input.AgentID).Scan(&writerRole, &writerAgent, &writerState); err != nil {
			return err
		}
		if mode == "normal" && writerRole && (!writerAgent.Valid || writerAgent.String != input.AgentID || writerState.String != "reserved") {
			return ErrAgentNotActive
		}
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT n.id,n.state FROM turn_note tn JOIN note n ON n.id=tn.note_id WHERE tn.turn_id=?`, input.TurnID)
		if err != nil {
			return err
		}
		state := map[string]string{}
		for rows.Next() {
			var id, value string
			if err := rows.Scan(&id, &value); err != nil {
				rows.Close()
				return err
			}
			state[id] = value
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(state) != len(input.NoteIDs) {
			return ErrScopeMismatch
		}
		seen := map[string]bool{}
		for _, id := range input.NoteIDs {
			if seen[id] || state[id] != "queued" {
				return ErrInvalidTransition
			}
			seen[id] = true
		}
		policy, err := tx.snapshotTurnPolicy(projectID, machineID, tool, variant)
		if err != nil {
			return err
		}
		if mode == "diagnostic" {
			var checked TurnPolicy
			if err := json.Unmarshal([]byte(policy), &checked); err != nil || !checked.Capabilities.ReadonlyVerified {
				return ErrAgentNotActive
			}
		}
		if _, err := tx.Exec(`UPDATE turn SET policy_json=? WHERE id=? AND policy_json IS NULL`, policy, input.TurnID); err != nil {
			return err
		}
		_, err = tx.AppendEvent(Event{Kind: "turn_dispatch_requested", ProjectID: projectID, RoomID: roomID, AgentID: input.AgentID, TurnID: input.TurnID, PayloadJSON: payloadJSON(map[string]string{"turn_id": input.TurnID})})
		return err
	})
}

func (s *Store) AckTurn(ctx context.Context, turnID, agentID string, generation int, level string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		var projectID, roomID, turnAgent, exec, observed string
		var turnGeneration int
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.project_id,t.room_id,t.agent_id,t.generation,t.exec,a.observed FROM turn t JOIN agent a ON a.id=t.agent_id WHERE t.id=?`, turnID).Scan(&projectID, &roomID, &turnAgent, &turnGeneration, &exec, &observed)
		if err != nil {
			return err
		}
		if turnAgent != agentID || turnGeneration != generation {
			return ErrStaleGeneration
		}
		if level != "persisted" && level != "accepted" {
			return fmt.Errorf("unknown ack level %q", level)
		}
		if exec != "pending" && exec != "running" {
			return nil
		}
		if level == "accepted" && exec == "pending" {
			if err := TurnExecTransitions.Transition(exec, "running"); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE turn SET exec='running',started_at=? WHERE id=?`, timestamp(), turnID); err != nil {
				return err
			}
			if observed == "idle" {
				if err := AgentObservedTransitions.Transition("idle", "in_turn"); err != nil {
					return err
				}
				if _, err := tx.Exec(`UPDATE agent SET observed='in_turn' WHERE id=?`, agentID); err != nil {
					return err
				}
			}
			if err := tx.evaluateUsageFor(roomID, projectID); err != nil {
				return err
			}
		}
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT n.id,n.state FROM turn_note tn JOIN note n ON n.id=tn.note_id WHERE tn.turn_id=?`, turnID)
		if err != nil {
			return err
		}
		type noteState struct{ id, state string }
		notes := []noteState{}
		for rows.Next() {
			var note noteState
			if err := rows.Scan(&note.id, &note.state); err != nil {
				rows.Close()
				return err
			}
			notes = append(notes, note)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, note := range notes {
			target := "dispatched"
			if level == "accepted" {
				target = "in_turn"
			}
			if level == "persisted" && note.state == "in_turn" {
				continue
			}
			if note.state == target {
				continue
			}
			if note.state == "queued" && target == "in_turn" {
				if err := NoteTransitions.Transition("queued", "dispatched"); err != nil {
					return err
				}
				note.state = "dispatched"
			}
			if err := NoteTransitions.Transition(note.state, target); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE note SET state=? WHERE id=?`, target, note.id); err != nil {
				return err
			}
		}
		_, err = tx.AppendEvent(Event{Kind: "turn_ack_" + level, ProjectID: projectID, RoomID: roomID, AgentID: agentID, TurnID: turnID, PayloadJSON: payloadJSON(map[string]string{"turn_id": turnID})})
		return err
	})
}

type HeldAnswer struct {
	TurnID     string
	AgentID    string
	Generation int
	AnswerRev  int
	Digest     string
	Candidate  []byte
	Reason     string
}

type DeliveryState struct {
	Exec, Outcome, Settle, NoteState string
	RecoveryBlock                    bool
}

func (s *Store) DeliveryState(ctx context.Context, turnID string) (DeliveryState, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return DeliveryState{}, err
	}
	var state DeliveryState
	err := s.db.QueryRowContext(ctx, `SELECT t.exec,t.outcome,t.settle,COALESCE((SELECT n.state FROM turn_note tn JOIN note n ON n.id=tn.note_id WHERE tn.turn_id=t.id ORDER BY n.id LIMIT 1),''),a.recovery_block FROM turn t JOIN agent a ON a.id=t.agent_id WHERE t.id=?`, turnID).Scan(&state.Exec, &state.Outcome, &state.Settle, &state.NoteState, &state.RecoveryBlock)
	return state, err
}

func (s *Store) ReceiptByTurn(ctx context.Context, turnID string, answerRev int) (digest, resultJSON string, err error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return "", "", err
	}
	err = s.db.QueryRowContext(ctx, `SELECT digest,result_json FROM receipt WHERE turn_id=? AND answer_rev=?`, turnID, answerRev).Scan(&digest, &resultJSON)
	return
}

func (s *Store) HoldAnswer(ctx context.Context, answer HeldAnswer) (resultJSON string, reused bool, err error) {
	err = s.Tx(ctx, func(tx *Tx) error {
		var owner string
		var generation int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT agent_id,generation FROM turn WHERE id=?`, answer.TurnID).Scan(&owner, &generation); err != nil {
			return err
		}
		if owner != answer.AgentID {
			return ErrForbidden
		}
		if generation != answer.Generation {
			return ErrStaleGeneration
		}
		var oldDigest string
		lookup := tx.tx.QueryRowContext(tx.ctx, `SELECT digest,result_json FROM receipt WHERE turn_id=? AND answer_rev=?`, answer.TurnID, answer.AnswerRev).Scan(&oldDigest, &resultJSON)
		if lookup == nil {
			if oldDigest != answer.Digest {
				return ErrConflict
			}
			reused = true
			return nil
		}
		if !errors.Is(lookup, sql.ErrNoRows) {
			return lookup
		}
		var projectID, roomID, agentID, exec string
		var terminal bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.project_id,t.room_id,t.agent_id,t.exec,t.terminal_event FROM turn t JOIN agent a ON a.id=t.agent_id WHERE t.id=?`, answer.TurnID).Scan(&projectID, &roomID, &agentID, &exec, &terminal); err != nil {
			return err
		}
		data, _ := json.Marshal(map[string]any{"result": "held", "held_reason": answer.Reason, "candidate_raw": string(answer.Candidate)})
		resultJSON = string(data)
		receiptID, err := newID("rcv")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO receipt(id,turn_id,answer_rev,digest,result_json,created_at) VALUES (?,?,?,?,?,?)`, receiptID, answer.TurnID, answer.AnswerRev, answer.Digest, resultJSON, timestamp()); err != nil {
			return err
		}
		var settle string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT settle FROM turn WHERE id=?`, answer.TurnID).Scan(&settle); err != nil {
			return err
		}
		if err := TurnSettleTransitions.Transition(settle, "settled"); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE turn SET settle='settled' WHERE id=? AND settle='pending'`, answer.TurnID); err != nil {
			return err
		}
		if terminal && (exec == "exited" || exec == "killed") {
			if err := tx.finishCompletedTurn(answer.TurnID, agentID); err != nil {
				return err
			}
		}
		_, err = tx.AppendEvent(Event{Kind: "answer_held", ProjectID: projectID, RoomID: roomID, AgentID: agentID, TurnID: answer.TurnID, PayloadJSON: payloadJSON(map[string]string{"held_reason": answer.Reason})})
		return err
	})
	return
}

func (tx *Tx) finishCompletedTurn(turnID, agentID string) error {
	var outcome string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT outcome FROM turn WHERE id=?`, turnID).Scan(&outcome); err != nil {
		return err
	}
	if outcome == "none" {
		if err := TurnOutcomeTransitions.Transition("none", "completed"); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE turn SET outcome='completed' WHERE id=?`, turnID); err != nil {
			return err
		}
	}
	_, err := tx.Exec(`UPDATE note SET state='done' WHERE id IN(SELECT note_id FROM turn_note WHERE turn_id=?) AND state='in_turn'`, turnID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM provider_limit_retry WHERE note_id IN(SELECT note_id FROM turn_note WHERE turn_id=?)
		AND note_id IN(SELECT id FROM note WHERE state='done')`, turnID); err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE agent SET observed='idle' WHERE id=? AND observed='in_turn'
		AND NOT EXISTS(SELECT 1 FROM turn WHERE agent_id=? AND id<>? AND exec IN ('pending','running'))`, agentID, agentID, turnID)
	return err
}

func (s *Store) RecordProcess(ctx context.Context, turnID, agentID string, generation int, exitCode int, terminal, cancel bool) error {
	return s.RecordProcessInfo(ctx, turnID, agentID, generation, adapter.ExitInfo{ExitCode: exitCode, TerminalEvent: terminal, CancelIntent: cancel})
}

func (s *Store) RecordProcessInfo(ctx context.Context, turnID, agentID string, generation int, info adapter.ExitInfo) error {
	exitCode, terminal, cancel := info.ExitCode, info.TerminalEvent, info.CancelIntent
	stderr := adapter.SanitizeStderr(info.StderrTail)
	errorEvent := adapter.SanitizeStderr(info.ErrorEvent)
	return s.Tx(ctx, func(tx *Tx) error {
		var projectID, roomID, turnAgent, exec, settle, tool, mode string
		var turnGeneration int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.project_id,t.room_id,t.agent_id,t.generation,t.exec,t.settle,a.tool,t.mode FROM turn t JOIN agent a ON a.id=t.agent_id WHERE t.id=?`, turnID).Scan(&projectID, &roomID, &turnAgent, &turnGeneration, &exec, &settle, &tool, &mode); err != nil {
			return err
		}
		if turnAgent != agentID || turnGeneration != generation {
			return ErrStaleGeneration
		}
		target := "exited"
		if cancel {
			target = "killed"
		}
		if exec == target {
			return nil
		}
		if exec == "pending" {
			if err := TurnExecTransitions.Transition("pending", "running"); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE turn SET exec='running',started_at=? WHERE id=?`, timestamp(), turnID); err != nil {
				return err
			}
			if err := tx.advanceNotesToInTurn(turnID); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE agent SET observed='in_turn' WHERE id=? AND observed='idle'`, agentID); err != nil {
				return err
			}
			if err := tx.evaluateUsageFor(roomID, projectID); err != nil {
				return err
			}
			exec = "running"
		}
		if err := TurnExecTransitions.Transition(exec, target); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE turn SET exec=?,terminal_event=?,cancel_intent=?,ended_at=? WHERE id=?`, target, terminal, cancel, timestamp(), turnID); err != nil {
			return err
		}
		resetAt, providerLimited := providerLimitEvidence(tool, stderr, errorEvent, time.Now())
		providerLimited = providerLimited && !terminal && !cancel && mode == "normal"
		if providerLimited {
			var hasReceipt bool
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM receipt WHERE turn_id=?)`, turnID).Scan(&hasReceipt); err != nil {
				return err
			}
			providerLimited = !hasReceipt
		}
		threadMissing := !providerLimited && !terminal && !cancel && mode == "normal" && nativeSessionMissingEvidence(tool, stderr, errorEvent)
		if threadMissing {
			var started sql.NullString
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT s.native_session_started_at FROM turn t JOIN session s ON s.id=t.session_id WHERE t.id=?`, turnID).Scan(&started); err != nil {
				return err
			}
			var hasReceipt bool
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM receipt WHERE turn_id=?)`, turnID).Scan(&hasReceipt); err != nil {
				return err
			}
			// A turn that opened its own thread and then lost it is not a stale
			// handle; resetting again would loop, so it takes ordinary recovery.
			var ownThread bool
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM event WHERE turn_id=? AND kind='native_session_started')`, turnID).Scan(&ownThread); err != nil {
				return err
			}
			threadMissing = started.Valid && !hasReceipt && !ownThread
		}
		if terminal && settle == "settled" {
			if err := tx.finishCompletedTurn(turnID, agentID); err != nil {
				return err
			}
		} else if threadMissing {
			if err := tx.recordNativeSessionReset(turnID, agentID, projectID, roomID, tool); err != nil {
				return err
			}
		} else if providerLimited {
			if err := tx.recordProviderLimit(turnID, agentID, projectID, roomID, tool, resetAt); err != nil {
				return err
			}
		} else if !terminal {
			if err := TurnOutcomeTransitions.Transition("none", "unknown"); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE turn SET outcome='unknown' WHERE id=?`, turnID); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE note SET state='unknown' WHERE id IN(SELECT note_id FROM turn_note WHERE turn_id=?) AND state IN ('dispatched','in_turn')`, turnID); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE agent SET recovery_block=1,observed='idle' WHERE id=?`, agentID); err != nil {
				return err
			}
		}
		payload := map[string]string{"exit_code": fmt.Sprint(exitCode)}
		if !terminal && !providerLimited && !threadMissing {
			payload["evidence"] = fmt.Sprintf("Process exited with code %d.", exitCode)
			if stderr != "" {
				payload["evidence"] += "\n" + stderr
			}
			if errorEvent != "" {
				payload["evidence"] += "\nCodex error event: " + errorEvent
			}
		}
		_, err := tx.AppendEvent(Event{Kind: "turn_process_exit", ProjectID: projectID, RoomID: roomID, AgentID: agentID, TurnID: turnID, PayloadJSON: payloadJSON(payload)})
		if err != nil {
			return err
		}
		if exec == "lost" {
			if err := tx.recordRecoveryQuiescence(turnID, "supervisor_process_exit"); err != nil {
				return err
			}
		}
		if !terminal && !providerLimited && !threadMissing {
			if err := tx.ensureRecoveryPath(turnID); err != nil {
				return err
			}
		} else {
			var mode string
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT mode FROM turn WHERE id=?`, turnID).Scan(&mode); err != nil {
				return err
			}
			if mode == "diagnostic" {
				if err := tx.completeDiagnostic(turnID); err != nil {
					return err
				}
			}
		}
		if terminal {
			var resultJSON string
			err := tx.tx.QueryRowContext(tx.ctx, `SELECT result_json FROM receipt WHERE turn_id=? AND answer_rev=0`, turnID).Scan(&resultJSON)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil {
				var receipt AnswerReceipt
				if err := json.Unmarshal([]byte(resultJSON), &receipt); err != nil {
					return err
				}
				if receipt.Result == "correction_requested" {
					parent, err := tx.answerTurn(turnID)
					if err != nil {
						return err
					}
					return tx.ensureCorrectionTurn(parent)
				}
			}
		}
		return nil
	})
}

func (tx *Tx) advanceNotesToInTurn(turnID string) error {
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT n.id,n.state FROM turn_note tn JOIN note n ON n.id=tn.note_id WHERE tn.turn_id=?`, turnID)
	if err != nil {
		return err
	}
	type note struct{ id, state string }
	notes := []note{}
	for rows.Next() {
		var item note
		if err := rows.Scan(&item.id, &item.state); err != nil {
			rows.Close()
			return err
		}
		notes = append(notes, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, item := range notes {
		if item.state == "in_turn" {
			continue
		}
		if item.state == "queued" {
			if err := NoteTransitions.Transition("queued", "dispatched"); err != nil {
				return err
			}
			item.state = "dispatched"
		}
		if err := NoteTransitions.Transition(item.state, "in_turn"); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE note SET state='in_turn' WHERE id=?`, item.id); err != nil {
			return err
		}
	}
	return nil
}

// RecordTurnEvent dedupes supervisor event IDs inside the server transaction.
func (s *Store) RecordTurnEvent(ctx context.Context, turnID, agentID, sourceID, kind, summary string, payload []byte) error {
	if sourceID == "" || !json.Valid(payload) {
		return fmt.Errorf("invalid supervisor event")
	}
	return s.Tx(ctx, func(tx *Tx) error {
		var projectID, roomID, turnAgent string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.project_id,t.room_id,t.agent_id FROM turn t JOIN agent a ON a.id=t.agent_id WHERE t.id=?`, turnID).Scan(&projectID, &roomID, &turnAgent); err != nil {
			return err
		}
		if turnAgent != agentID {
			return ErrForbidden
		}
		var oldKind, oldPayload string
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT kind,payload_json FROM event WHERE turn_id=? AND json_extract(payload_json,'$.source_event_id')=? AND kind LIKE 'supervisor_%' LIMIT 1`, turnID, sourceID).Scan(&oldKind, &oldPayload)
		if err == nil {
			var old struct {
				EventRaw string `json:"event_raw"`
			}
			if json.Unmarshal([]byte(oldPayload), &old) != nil || oldKind != "supervisor_"+kind || old.EventRaw != string(payload) {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		data, _ := json.Marshal(map[string]any{"source_event_id": sourceID, "summary": summary, "event_raw": string(payload)})
		if _, err := tx.AppendEvent(Event{Kind: "supervisor_" + kind, ProjectID: projectID, RoomID: roomID, AgentID: agentID, TurnID: turnID, PayloadJSON: string(data)}); err != nil {
			return err
		}
		return tx.recordUsageOrCompaction(turnID, agentID, projectID, roomID, sourceID, kind, payload)
	})
}
