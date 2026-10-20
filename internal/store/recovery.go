package store

import (
	"database/sql"

	"github.com/goccy/go-json"
)

func (tx *Tx) refreshRecoveryItemsAfterEvidence(evidence Event) error {
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT id,project_id,room_id,agent_id FROM inbox_item WHERE turn_id=? AND kind='recovery' AND state='open'`, evidence.TurnID)
	if err != nil {
		return err
	}
	type item struct{ id, projectID, roomID, agentID string }
	items := []item{}
	for rows.Next() {
		var x item
		if err := rows.Scan(&x.id, &x.projectID, &x.roomID, &x.agentID); err != nil {
			rows.Close()
			return err
		}
		items = append(items, x)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, x := range items {
		if _, err := tx.Exec(`UPDATE inbox_item SET rev=rev+1 WHERE id=? AND state='open'`, x.id); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(Event{Kind: "inbox_item_updated", ProjectID: x.projectID, RoomID: x.roomID,
			AgentID: x.agentID, TurnID: evidence.TurnID,
			PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": x.id, "evidence_kind": evidence.Kind})}); err != nil {
			return err
		}
	}
	return nil
}

// ensureRecoveryPath runs after the process state is durable. A confirmed
// exit may get a verified read-only diagnostic; a lost process cannot safely
// admit another turn and goes straight to owner review.
func (tx *Tx) ensureRecoveryPath(turnID string) error {
	var mode, exec, policyJSON string
	var parent sql.NullString
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT mode,exec,COALESCE(policy_json,''),parent_turn_id FROM turn WHERE id=?`, turnID).Scan(&mode, &exec, &policyJSON, &parent); err != nil {
		return err
	}
	if mode == "diagnostic" {
		if _, err := tx.Exec(`UPDATE turn SET settle='settled' WHERE id=? AND settle='pending'`, turnID); err != nil {
			return err
		}
		return tx.ensureRecoveryItem(parent.String, "diagnostic_failed")
	}
	if mode != "normal" {
		return nil
	}
	var overridden bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM event WHERE turn_id=? AND kind='recovery_override')`, turnID).Scan(&overridden); err != nil {
		return err
	}
	if overridden {
		return nil
	}
	var desired string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.desired FROM turn t JOIN agent a ON a.id=t.agent_id WHERE t.id=?`, turnID).Scan(&desired); err != nil {
		return err
	}
	if exec != "lost" && desired == "active" {
		var policy TurnPolicy
		if json.Unmarshal([]byte(policyJSON), &policy) == nil && policy.Capabilities.ReadonlyVerified {
			return tx.ensureDiagnosticTurn(turnID)
		}
	}
	return tx.ensureRecoveryItem(turnID, exec)
}

func (tx *Tx) ensureDiagnosticTurn(originID string) error {
	var existing int
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM turn WHERE parent_turn_id=? AND mode='diagnostic'`, originID).Scan(&existing); err != nil {
		return err
	}
	if existing > 0 {
		return nil
	}
	var agentID, sessionID, roomID, projectID string
	var generation, attempt int
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT t.agent_id,t.session_id,t.room_id,a.project_id,t.generation,t.attempt FROM turn t JOIN agent a ON a.id=t.agent_id WHERE t.id=?`, originID).
		Scan(&agentID, &sessionID, &roomID, &projectID, &generation, &attempt); err != nil {
		return err
	}
	turn, err := tx.RecordTurn(Turn{AgentID: agentID, SessionID: sessionID, Generation: generation,
		RoomID: roomID, ParentTurnID: originID, Attempt: attempt + 1, Mode: "diagnostic", Exec: "pending"})
	if err != nil {
		return err
	}
	_, err = tx.AppendEvent(Event{Kind: "diagnostic_requested", ProjectID: projectID, RoomID: roomID,
		AgentID: agentID, TurnID: turn.ID, PayloadJSON: payloadJSON(map[string]string{"origin_turn_id": originID})})
	return err
}

func (tx *Tx) ensureRecoveryItem(originID, reason string) error {
	if originID == "" {
		return ErrInvalidTransition
	}
	var existing int
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM inbox_item WHERE turn_id=? AND kind='recovery' AND state='open'`, originID).Scan(&existing); err != nil {
		return err
	}
	if existing > 0 {
		return nil
	}
	var projectID, roomID, agentID string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.project_id,t.room_id,t.agent_id FROM turn t JOIN agent a ON a.id=t.agent_id WHERE t.id=?`, originID).
		Scan(&projectID, &roomID, &agentID); err != nil {
		return err
	}
	id, err := newID("inb")
	if err != nil {
		return err
	}
	question := "An agent turn has an uncertain outcome. Review the evidence before continuing."
	if reason == "lost" {
		question = "The previous process may still be running; no diagnostic was run. Resume or Retry may repeat an action."
	}
	optionsBytes, err := json.Marshal(inboxAsk{Text: question, Options: []string{"Resume", "Retry", "Abandon"}})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO inbox_item(id,project_id,room_id,turn_id,agent_id,requester_kind,requester_id,kind,options_json,created_at) VALUES (?,?,?,?,?,'agent',?,'recovery',?,?)`,
		id, projectID, roomID, originID, agentID, agentID, string(optionsBytes), timestamp()); err != nil {
		return err
	}
	_, err = tx.AppendEvent(Event{Kind: "inbox_item_created", ProjectID: projectID, RoomID: roomID,
		AgentID: agentID, TurnID: originID, PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": id, "kind": "recovery", "reason": reason})})
	return err
}

func (tx *Tx) completeDiagnostic(turnID string) error {
	var parentID, exec, settle, reportJSON, policyJSON string
	var terminal bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COALESCE(parent_turn_id,''),exec,settle,COALESCE(report_json,''),terminal_event,COALESCE(policy_json,'') FROM turn WHERE id=? AND mode='diagnostic'`, turnID).
		Scan(&parentID, &exec, &settle, &reportJSON, &terminal, &policyJSON); err != nil {
		return err
	}
	if exec != "exited" && exec != "killed" || settle != "settled" {
		return nil
	}
	if !terminal {
		return tx.ensureRecoveryItem(parentID, "diagnostic_incomplete")
	}
	var report struct {
		Reconciliation struct {
			Status string `json:"status"`
		} `json:"reconciliation"`
	}
	if err := json.Unmarshal([]byte(reportJSON), &report); err != nil {
		return tx.ensureRecoveryItem(parentID, "diagnostic_invalid")
	}
	var policy TurnPolicy
	if report.Reconciliation.Status == "clean" && json.Unmarshal([]byte(policyJSON), &policy) == nil &&
		policy.Capabilities.ReadonlyVerified && policy.AutoResumeAfterCleanDiagnostic {
		var desired string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.desired FROM turn t JOIN agent a ON a.id=t.agent_id WHERE t.id=?`, turnID).Scan(&desired); err != nil {
			return err
		}
		if desired != "active" {
			return tx.ensureRecoveryItem(parentID, "paused_after_diagnostic")
		}
		policy.AutoResumeActor = "system (auto-resume setting)"
		updated, err := json.Marshal(policy)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE turn SET policy_json=? WHERE id=?`, string(updated), turnID); err != nil {
			return err
		}
		return tx.createRecoveryContinuation(parentID, "resume", "system (auto-resume setting)", turnID)
	}
	return tx.ensureRecoveryItem(parentID, "diagnostic_report")
}

func (tx *Tx) createRecoveryContinuation(originID, choice, actorID, diagnosticID string) error {
	var agentID, roomID, projectID, desired string
	var attempt, generation int
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT t.agent_id,t.room_id,a.project_id,a.desired,t.attempt,a.generation FROM turn t JOIN agent a ON a.id=t.agent_id WHERE t.id=?`, originID).
		Scan(&agentID, &roomID, &projectID, &desired, &attempt, &generation); err != nil {
		return err
	}
	if desired != "active" {
		return ErrInvalidTransition
	}
	var existing int
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM turn WHERE parent_turn_id=? AND mode='normal'`, originID).Scan(&existing); err != nil {
		return err
	}
	if existing > 0 {
		return ErrStaleInboxRev
	}
	var sessionID string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM session WHERE agent_id=? AND generation=? AND ended_at IS NULL ORDER BY started_at DESC LIMIT 1`, agentID, generation).Scan(&sessionID); err != nil {
		return err
	}
	turn, err := tx.RecordTurn(Turn{AgentID: agentID, SessionID: sessionID, Generation: generation,
		RoomID: roomID, ParentTurnID: originID, Attempt: attempt + 1, Mode: "normal", Exec: "pending"})
	if err != nil {
		return err
	}
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT note_id FROM turn_note WHERE turn_id=?`, originID)
	if err != nil {
		return err
	}
	notes := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		notes = append(notes, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range notes {
		if _, err := tx.Exec(`UPDATE note SET state='queued' WHERE id=? AND state='unknown'`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO turn_note(turn_id,note_id) VALUES (?,?)`, turn.ID, id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE agent SET recovery_block=0,observed='idle' WHERE id=?`, agentID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE turn SET settle='settled' WHERE id=? AND settle='pending'`, originID); err != nil {
		return err
	}
	if _, err := tx.AppendEvent(Event{Kind: "recovery_override", ProjectID: projectID, RoomID: roomID,
		AgentID: agentID, TurnID: originID, PayloadJSON: payloadJSON(map[string]string{"choice": choice, "actor": actorID})}); err != nil {
		return err
	}
	_, err = tx.AppendEvent(Event{Kind: "recovery_decision", ProjectID: projectID, RoomID: roomID,
		AgentID: agentID, TurnID: turn.ID, PayloadJSON: payloadJSON(map[string]string{
			"choice": choice, "actor": actorID, "origin_turn_id": originID, "diagnostic_turn_id": diagnosticID})})
	return err
}

func (tx *Tx) createQuiescenceWaitingItem(originID, childID string) error {
	var projectID, roomID, agentID string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.project_id,t.room_id,t.agent_id FROM turn t JOIN agent a ON a.id=t.agent_id WHERE t.id=?`, originID).
		Scan(&projectID, &roomID, &agentID); err != nil {
		return err
	}
	id, err := newID("inb")
	if err != nil {
		return err
	}
	options, err := json.Marshal(inboxAsk{Text: "Waiting for the old process to be confirmed gone before continuing. An owner may explicitly override the overlap risk or abandon the continuation.",
		Options: []string{"Override", "Abandon"}, Phase: "quiescence_wait"})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO inbox_item(id,project_id,room_id,turn_id,agent_id,requester_kind,requester_id,kind,options_json,created_at)
		VALUES (?,?,?,?,?,'agent',?,'recovery',?,?)`, id, projectID, roomID, originID, agentID, agentID, string(options), timestamp()); err != nil {
		return err
	}
	_, err = tx.AppendEvent(Event{Kind: "inbox_item_created", ProjectID: projectID, RoomID: roomID, AgentID: agentID, TurnID: originID,
		PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": id, "kind": "recovery", "phase": "quiescence_wait", "continuation_turn_id": childID})})
	return err
}

func (tx *Tx) replaceUnstartedRecoveryContinuation(oldID string) error {
	var old Turn
	var projectID, decisionJSON string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT t.agent_id,t.session_id,t.generation,t.room_id,t.parent_turn_id,t.attempt,a.project_id,
		(SELECT e.payload_json FROM event e WHERE e.turn_id=t.id AND e.kind='recovery_decision' ORDER BY e.seq DESC LIMIT 1)
		FROM turn t JOIN agent a ON a.id=t.agent_id WHERE t.id=?`, oldID).
		Scan(&old.AgentID, &old.SessionID, &old.Generation, &old.RoomID, &old.ParentTurnID, &old.Attempt, &projectID, &decisionJSON); err != nil {
		return err
	}
	replacement, err := tx.RecordTurn(Turn{AgentID: old.AgentID, SessionID: old.SessionID, Generation: old.Generation,
		RoomID: old.RoomID, ParentTurnID: old.ParentTurnID, Attempt: old.Attempt + 1, Mode: "normal", Exec: "pending"})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO turn_note(turn_id,note_id) SELECT ?,note_id FROM turn_note WHERE turn_id=?`, replacement.ID, oldID); err != nil {
		return err
	}
	if _, err := tx.AppendEvent(Event{Kind: "recovery_decision", ProjectID: projectID, RoomID: old.RoomID,
		AgentID: old.AgentID, TurnID: replacement.ID, PayloadJSON: decisionJSON}); err != nil {
		return err
	}
	_, err = tx.AppendEvent(Event{Kind: "recovery_continuation_requeued", ProjectID: projectID, RoomID: old.RoomID,
		AgentID: old.AgentID, TurnID: replacement.ID, PayloadJSON: payloadJSON(map[string]string{"prior_turn_id": oldID})})
	return err
}

func (tx *Tx) recordRecoveryQuiescence(originID, source string) error {
	var projectID, roomID, agentID string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.project_id,t.room_id,t.agent_id FROM turn t JOIN agent a ON a.id=t.agent_id WHERE t.id=?`, originID).
		Scan(&projectID, &roomID, &agentID); err != nil {
		return err
	}
	var existing bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM event WHERE turn_id=? AND kind='recovery_quiescent')`, originID).Scan(&existing); err != nil {
		return err
	}
	if !existing {
		if _, err := tx.AppendEvent(Event{Kind: "recovery_quiescent", ProjectID: projectID, RoomID: roomID, AgentID: agentID, TurnID: originID,
			PayloadJSON: payloadJSON(map[string]string{"source": source})}); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE agent SET recovery_block=0,observed=CASE WHEN desired='stopped' THEN 'stopped' ELSE 'idle' END
		WHERE id=? AND EXISTS(SELECT 1 FROM turn child WHERE child.parent_turn_id=? AND child.mode='normal' AND child.exec='pending')`, agentID, originID); err != nil {
		return err
	}
	return tx.resolveQuiescenceWaitingItems(originID, "system (quiescence verified)", "quiescence_verified", source)
}

func (tx *Tx) resolveQuiescenceWaitingItems(originID, actor, choice, source string) error {
	var projectID, roomID, agentID string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.project_id,t.room_id,t.agent_id FROM turn t JOIN agent a ON a.id=t.agent_id WHERE t.id=?`, originID).
		Scan(&projectID, &roomID, &agentID); err != nil {
		return err
	}
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT id FROM inbox_item WHERE turn_id=? AND kind='recovery' AND state='open'
		AND json_extract(options_json,'$.phase')='quiescence_wait'`, originID)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.Exec(`UPDATE inbox_item SET state='resolved',rev=rev+1,resolved_by=?,resolved_at=? WHERE id=?`, actor, timestamp(), id); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(Event{Kind: "inbox_item_resolved", ProjectID: projectID, RoomID: roomID, AgentID: agentID, TurnID: originID,
			PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": id, "choice": choice, "source": source, "actor": actor})}); err != nil {
			return err
		}
	}
	return nil
}
