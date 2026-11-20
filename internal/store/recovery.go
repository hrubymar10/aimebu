package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

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
	// A turn that failed before it did anything has nothing for a read-only diagnostic to find, and the diagnostic would only fail the same
	// way: it goes straight to the person, who is offered Retry.
	if definite, _, err := tx.definiteFailure(turnID); err != nil {
		return err
	} else if definite {
		return tx.ensureRecoveryItem(turnID, exec)
	}
	if exec != "lost" && desired == "active" {
		var policy TurnPolicy
		if json.Unmarshal([]byte(policyJSON), &policy) == nil && policy.Capabilities.ReadonlyVerified {
			return tx.ensureDiagnosticTurn(turnID)
		}
	}
	return tx.ensureRecoveryItem(turnID, exec)
}

// turnLimitContinuationNote is what the agent is told with an automatic continuation turn: the previous turn was killed by the
// supervisor's turn-limit watchdog, not by a failure of the work.
const turnLimitContinuationNote = "your previous turn was stopped by the turn limit; check state and continue; keep commands short"

// turnInterruptedNote is the same for a turn whose harness process was cut off (killed, session restarted, container stopped);
// the reason is the supervisor's plain description of how it ended.
func turnInterruptedNote(reason string) string {
	if reason == "" {
		reason = "the harness process ended unexpectedly"
	}
	return "your previous turn was interrupted (" + reason + "); check state and continue"
}

// Kinds of automatic continuation. Both share one allowance per chain of turns: a turn born from either does not continue again.
const (
	eventLimitContinuation       = "turn_limit_continuation"
	eventInterruptedContinuation = "turn_interrupted_continuation"
)

// createLimitContinuation continues once, on its own, a turn the supervisor's turn-limit watchdog killed: the turn's notes are
// re-queued on a child turn with a note that says why, so a long but legitimate turn does not leave the agent behind a recovery
// card waiting for a person to click Resume. It reports whether it continued; a turn whose chain already had its one
// continuation, one that already has a child, or an agent that is not active, falls back to the ordinary recovery path.
func (tx *Tx) createLimitContinuation(originID, reason string) (bool, error) {
	return tx.createAutoContinuation(originID, eventLimitContinuation, "turn_limit", turnLimitContinuationNote, map[string]string{"reason": reason})
}

// createInterruptedContinuation is the same for a turn the supervisor marked interrupted: reason is how the process ended, and the
// event records turn_interrupted with the exit code.
func (tx *Tx) createInterruptedContinuation(originID, reason string, exitCode int) (bool, error) {
	return tx.createAutoContinuation(originID, eventInterruptedContinuation, "turn_interrupted", turnInterruptedNote(reason),
		map[string]string{"reason": "turn_interrupted", "detail": reason, "exit_code": fmt.Sprint(exitCode)})
}

func (tx *Tx) createAutoContinuation(originID, eventKind, wakeReason, noteBody string, payload map[string]string) (bool, error) {
	var agentID, roomID, projectID, sessionID, desired string
	var attempt, generation int
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT t.agent_id,t.room_id,a.project_id,t.session_id,t.attempt,t.generation,a.desired
		FROM turn t JOIN agent a ON a.id=t.agent_id WHERE t.id=?`, originID).
		Scan(&agentID, &roomID, &projectID, &sessionID, &attempt, &generation, &desired); err != nil {
		return false, err
	}
	if desired != "active" {
		return false, nil
	}
	var existing int
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM turn WHERE parent_turn_id=? AND mode='normal'`, originID).Scan(&existing); err != nil {
		return false, err
	}
	if existing > 0 {
		return false, nil
	}
	// One continuation per chain of limit kills: the marker event rides the continuation turn itself, so a turn that was born
	// this way (or descends from one) goes to recovery as before instead of continuing without end.
	var already bool
	if err := tx.tx.QueryRowContext(tx.ctx, `WITH RECURSIVE chain(id,parent) AS (
			SELECT id,parent_turn_id FROM turn WHERE id=?
			UNION ALL
			SELECT t.id,t.parent_turn_id FROM turn t JOIN chain c ON t.id=c.parent
		) SELECT EXISTS(SELECT 1 FROM event e JOIN chain c ON e.turn_id=c.id WHERE e.kind IN ('turn_limit_continuation','turn_interrupted_continuation'))`, originID).Scan(&already); err != nil {
		return false, err
	}
	if already {
		return false, nil
	}
	turn, err := tx.RecordTurn(Turn{AgentID: agentID, SessionID: sessionID, Generation: generation,
		RoomID: roomID, ParentTurnID: originID, Attempt: attempt + 1, Mode: "normal", Exec: "pending"})
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(`UPDATE note SET state='queued' WHERE id IN(SELECT note_id FROM turn_note WHERE turn_id=?) AND state IN ('in_turn','unknown')`, originID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`INSERT INTO turn_note(turn_id,note_id) SELECT ?,note_id FROM turn_note WHERE turn_id=?`, turn.ID, originID); err != nil {
		return false, err
	}
	message, err := tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "system", AuthorID: "system", Body: noteBody})
	if err != nil {
		return false, err
	}
	note, err := tx.InsertNote(Note{AgentID: agentID, MessageID: message.ID, RoomID: roomID, WakeReason: wakeReason})
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(`INSERT INTO turn_note(turn_id,note_id) VALUES (?,?)`, turn.ID, note.ID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`UPDATE agent SET recovery_block=0,observed='idle' WHERE id=?`, agentID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`UPDATE turn SET settle='settled' WHERE id=? AND settle='pending'`, originID); err != nil {
		return false, err
	}
	payload["origin_turn_id"] = originID
	_, err = tx.AppendEvent(Event{Kind: eventKind, ProjectID: projectID, RoomID: roomID,
		AgentID: agentID, TurnID: turn.ID, PayloadJSON: payloadJSON(payload)})
	if err != nil {
		return false, err
	}
	return true, nil
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

// sentenceEnd closes a harness's error text with a full stop when it has none, so the sentence that follows it does not run on from it.
func sentenceEnd(text string) string {
	text = strings.TrimSpace(text)
	if text == "" || strings.ContainsAny(text[len(text)-1:], ".!?") {
		return text
	}
	return text + "."
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
	options := []string{"Resume", "Retry", "Abandon"}
	if reason == "lost" {
		question = "The previous process may still be running; no diagnostic was run. Resume or Retry may repeat an action."
	}
	definite, resultText, err := tx.definiteFailure(originID)
	if err != nil {
		return err
	}
	if definite && reason != "lost" {
		question = "The turn failed before it did anything: " + sentenceEnd(resultText) + " Nothing needs checking, so Retry is safe and Resume cannot help. Fix the cause first (for example sign in again), then Retry."
		options = []string{"Retry", "Abandon"}
		var startFailure bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COALESCE(json_extract(payload_json,'$.start_failure'),'')='1' FROM event
			WHERE turn_id=? AND kind='turn_process_exit' ORDER BY seq DESC LIMIT 1`, originID).Scan(&startFailure); err != nil {
			return err
		}
		if startFailure {
			question = "The agent could not start: " + sentenceEnd(resultText) + " The harness did not run. Retry after fixing the start problem."
			if strings.Contains(resultText, "normal turn requires agent tools access") {
				question = "The agent could not start: " + sentenceEnd(resultText) + " Start the agent again to restore tools access, then Retry."
			}
		}
	}
	if repeated, err := tx.repeatsParentFailure(originID); err != nil {
		return err
	} else if repeated {
		question = "Same failure again — fix the cause first; doing the same thing will fail the same way. " + question
	}
	optionsBytes, err := json.Marshal(inboxAsk{Text: question, Options: options})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO inbox_item(id,project_id,room_id,turn_id,agent_id,requester_kind,requester_id,kind,options_json,created_at) VALUES (?,?,?,?,?,'agent',?,'recovery',?,?)`,
		id, projectID, roomID, originID, agentID, agentID, string(optionsBytes), timestamp()); err != nil {
		return err
	}
	handed, err := tx.handRecoveryToLead(id, projectID, roomID, agentID, originID, reason)
	if err != nil || handed {
		return err
	}
	_, err = tx.AppendEvent(Event{Kind: "inbox_item_created", ProjectID: projectID, RoomID: roomID,
		AgentID: agentID, TurnID: originID, PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": id, "kind": "recovery", "reason": reason})})
	return err
}

// handRecoveryToLead routes a just-inserted worker recovery item to the project lead (see workerRecoveryLead) and wakes it with a note.
// It reports false when there is no lead to hand it to, and the item then stays with the people.
func (tx *Tx) handRecoveryToLead(id, projectID, roomID, agentID, originID, reason string) (bool, error) {
	leadID, leadRoom, due, err := tx.workerRecoveryLead(projectID, roomID, agentID)
	if err != nil || leadID == "" {
		return false, err
	}
	if _, err := tx.Exec(`UPDATE inbox_item SET recovery_lead_id=?,lead_due_at=? WHERE id=?`, leadID, due, id); err != nil {
		return false, err
	}
	var evidence, exitCode sql.NullString
	err = tx.tx.QueryRowContext(tx.ctx, `SELECT json_extract(payload_json,'$.evidence'),json_extract(payload_json,'$.exit_code') FROM event WHERE turn_id=? AND kind='turn_process_exit' ORDER BY seq DESC LIMIT 1`, originID).Scan(&evidence, &exitCode)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	body := fmt.Sprintf("Recovery handed to the lead for agent %s in room %s: %s (exit code %s). Evidence: %s. [Worker output](/api/rooms/%s/agents/%s/output). Item ID: %s.", agentID, roomID, reason, exitCode.String, evidence.String, roomID, agentID, id)
	switch reason {
	case answerHeldReason:
		body = fmt.Sprintf("Agent %s in room %s sent a final answer that could not be delivered, and a correction note did not settle it. Item ID: %s. resolve_recovery with choice \"retry\" asks it once more to resend; \"abandon\" drops the held answer. [Worker output](/api/rooms/%s/agents/%s/output).", agentID, roomID, id, roomID, agentID)
	case brokenSessionReason:
		body = fmt.Sprintf("Agent %s in room %s answered twice in a row with an empty answer and no tool activity: its session looks broken. Item ID: %s. resolve_recovery with choice \"restart\" starts it on a fresh session; \"abandon\" leaves it as it is. [Worker output](/api/rooms/%s/agents/%s/output).", agentID, roomID, id, roomID, agentID)
	}
	if leadRoom != roomID {
		if _, err := tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "system", AuthorID: "system", Body: "Recovery handed to the lead", OnlyTargets: []string{}}); err != nil {
			return false, err
		}
	}
	message, err := tx.InsertMessage(Message{RoomID: leadRoom, AuthorKind: "system", AuthorID: "system", Body: body, OnlyTargets: []string{}})
	if err != nil {
		return false, err
	}
	if _, err := tx.InsertNote(Note{AgentID: leadID, MessageID: message.ID, RoomID: leadRoom, WakeReason: "worker_recovery"}); err != nil {
		return false, err
	}
	_, err = tx.AppendEvent(Event{Kind: "recovery_handed_to_lead", ProjectID: projectID, RoomID: roomID, AgentID: agentID, TurnID: originID, PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": id, "lead_id": leadID, "reason": reason})})
	return err == nil, err
}

func (tx *Tx) workerRecoveryLead(projectID, roomID, workerID string) (string, string, string, error) {
	var targetIsLead bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT r.cap_is_lead FROM agent a JOIN role r ON r.id=a.role_id WHERE a.id=?`, workerID).Scan(&targetIsLead); err != nil {
		return "", "", "", err
	}
	if targetIsLead {
		return "", "", "", nil
	}
	var enabled bool
	var minutes int
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT worker_recovery_to_lead,worker_recovery_escalate_minutes FROM settings_project WHERE project_id=?`, projectID).Scan(&enabled, &minutes); err != nil {
		return "", "", "", err
	}
	if !enabled {
		return "", "", "", nil
	}
	var leadID string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.id FROM agent a JOIN role r ON r.id=a.role_id WHERE a.project_id=? AND a.id<>? AND r.cap_is_lead=1 AND a.desired='active' AND a.recovery_block=0 AND a.revoked_at IS NULL ORDER BY a.created_at,a.id LIMIT 1`, projectID, workerID).Scan(&leadID)
	if err == sql.ErrNoRows {
		return "", "", "", nil
	}
	if err != nil {
		return "", "", "", err
	}
	var joined bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL)`, roomID, leadID).Scan(&joined); err != nil {
		return "", "", "", err
	}
	leadRoom := roomID
	if !joined {
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM room WHERE project_id=? AND kind='home' LIMIT 1`, projectID).Scan(&leadRoom)
		if errors.Is(err, sql.ErrNoRows) {
			// A lead outside the worker's room, in a project with no home room, has nowhere to be told: the person decides, as with no lead.
			return "", "", "", nil
		}
		if err != nil {
			return "", "", "", err
		}
	}
	return leadID, leadRoom, time.Now().UTC().Add(time.Duration(minutes) * time.Minute).Format(timestampLayout), nil
}

// definiteFailure says whether the turn's process exit reported a failure before it did anything (see adapter.ExitInfo.DefiniteFailure),
// with the sentence the harness gave.
func (tx *Tx) definiteFailure(turnID string) (bool, string, error) {
	var definite sql.NullString
	var text sql.NullString
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT json_extract(payload_json,'$.definite_failure'),json_extract(payload_json,'$.result_text') FROM event
		WHERE turn_id=? AND kind='turn_process_exit' ORDER BY seq DESC LIMIT 1`, turnID).Scan(&definite, &text)
	if err == sql.ErrNoRows {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return definite.String == "1" && text.String != "", text.String, nil
}

// repeatsParentFailure says whether this turn is a retry or a resume of a turn that failed the same way: its process exit says the same as its
// parent's, so doing the same again would fail again. The person is told, instead of the cycle going round silently. "The same" is the
// sentence the harness gave when both exits have one; otherwise the evidence of both, but only when it says more than an exit code (two
// unrelated failures that both exited with code 1 are not the same failure). Raw output is not compared when a sentence exists: it carries
// costs, token counts and ids that differ from one run to the next.
func (tx *Tx) repeatsParentFailure(turnID string) (bool, error) {
	var parent sql.NullString
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT parent_turn_id FROM turn WHERE id=?`, turnID).Scan(&parent); err != nil {
		return false, err
	}
	if !parent.Valid || parent.String == "" {
		return false, nil
	}
	exit := func(turn string) (evidence, result string, err error) {
		var e, r sql.NullString
		err = tx.tx.QueryRowContext(tx.ctx, `SELECT json_extract(payload_json,'$.evidence'),json_extract(payload_json,'$.result_text') FROM event
			WHERE turn_id=? AND kind='turn_process_exit' ORDER BY seq DESC LIMIT 1`, turn).Scan(&e, &r)
		if err == sql.ErrNoRows {
			return "", "", nil
		}
		return e.String, r.String, err
	}
	evidence, result, err := exit(turnID)
	if err != nil {
		return false, err
	}
	parentEvidence, parentResult, err := exit(parent.String)
	if err != nil {
		return false, err
	}
	switch {
	case result != "" && parentResult != "":
		return result == parentResult, nil
	case result != "" || parentResult != "":
		return false, nil
	}
	// Evidence with nothing but the exit code ("Process exited with code 1.") is on every failure, so it proves nothing.
	return strings.Contains(evidence, "\n") && evidence == parentEvidence, nil
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

// quiescenceWaitReason is the reason a recovery item carries (and a lead is told) while a continuation waits for the old process to be confirmed gone.
const quiescenceWaitReason = "quiescence_wait"

// answerHeldReason and brokenSessionReason name the recoveries raised for a worker's undeliverable answer and for a session that answers
// with nothing; each has its own phase and choices (see decideAnswerRecovery).
const (
	answerHeldReason    = "answer_held"
	brokenSessionReason = "broken_session"
)

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
	// Why nothing proved the old process gone, so the person or lead deciding sees it: a replaced journal has no record of the process at all,
	// otherwise no supervisor hello or exit report has covered it since the turn was lost.
	var replaced bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM event WHERE turn_id=? AND kind='supervisor_journal_changed')`, originID).Scan(&replaced); err != nil {
		return err
	}
	why := "no supervisor report of the process ending has arrived since the turn was lost"
	if replaced {
		why = "the supervisor's journal was replaced, so nothing records whether the process is still running"
	}
	options, err := json.Marshal(inboxAsk{Text: "Waiting for the old process to be confirmed gone before continuing (" + why + "). Override the overlap risk or abandon the continuation.",
		Options: []string{"Override", "Abandon"}, Phase: "quiescence_wait"})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO inbox_item(id,project_id,room_id,turn_id,agent_id,requester_kind,requester_id,kind,options_json,created_at)
		VALUES (?,?,?,?,?,'agent',?,'recovery',?,?)`, id, projectID, roomID, originID, agentID, agentID, string(options), timestamp()); err != nil {
		return err
	}
	// The overlap risk (the old process may still be running) is a person's call, not the lead's: the item goes to people, never to the lead.
	_, err = tx.AppendEvent(Event{Kind: "inbox_item_created", ProjectID: projectID, RoomID: roomID, AgentID: agentID, TurnID: originID,
		PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": id, "kind": "recovery", "phase": "quiescence_wait", "continuation_turn_id": childID, "why": why})})
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
