package store

import (
	"regexp"
	"strings"
)

var codexThreadNotFound = regexp.MustCompile(`thread [0-9A-Za-z-]+ not found`)

// nativeSessionMissingEvidence reports a harness's own wording for a resumed
// session it can no longer find. Codex needs both phrases of v1's
// agentCodexThreadNotFoundRE so an unrelated "not found" cannot trigger a
// reset; OpenCode and Vibe print their one line on stderr.
func nativeSessionMissingEvidence(tool, stderr, errorEvent string) bool {
	evidence := stderr + "\n" + errorEvent
	switch tool {
	case "claude":
		return strings.Contains(evidence, "No conversation found with session ID:")
	case "codex":
		return strings.Contains(evidence, "failed to record rollout items") && codexThreadNotFound.MatchString(evidence)
	case "opencode", "vibe":
		return strings.Contains(stderr, "Error: Session not found")
	}
	return false
}

// recordNativeSessionReset handles a resumed turn whose native session is gone.
// The session forgets its native handle so the next delivery starts a fresh
// native session, and the turn's notes go back to the queue instead of
// becoming a recovery item. Callers must only invoke it for a session that
// had started natively, which keeps a failing fresh start from looping.
func (tx *Tx) recordNativeSessionReset(turnID, agentID, projectID, roomID, tool string) error {
	var sessionID string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT session_id FROM turn WHERE id=?`, turnID).Scan(&sessionID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE session SET native_session_id=NULL,native_session_started_at=NULL,start_mode='fresh_with_history' WHERE id=?`, sessionID); err != nil {
		return err
	}
	// The harness lost its native session, so it lost the context earlier turns delivered too: the session's context-delivery
	// state starts over, and the next wake carries the role body and knowledge in full. The session's remembered usage totals
	// belonged to the lost thread and say nothing about the new one, so they are forgotten as well.
	if _, err := tx.Exec(`DELETE FROM session_context_delivery WHERE session_id=?`, sessionID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE usage_record SET tokens_in_total=NULL,tokens_out_total=NULL,tokens_cache_read_total=NULL,tokens_cache_write_total=NULL,cost_total=NULL
		WHERE id IN (SELECT u.id FROM usage_record u LEFT JOIN turn t ON t.id=u.turn_id WHERE COALESCE(t.session_id,u.session_id)=?)`, sessionID); err != nil {
		return err
	}
	// A cost-reset marker: the first cumulative report after the reset is the fresh thread's own spend (its totals started at zero),
	// not a report over history the store cannot price. Without the marker the first report would be dropped as an unknown baseline,
	// losing the turn's spend; with it the baseline checks read the reset like a new session. The turn it names is the one whose
	// failure caused the reset; the marker holds until the first report after it.
	if _, err := tx.Exec(`INSERT OR IGNORE INTO cost_reset_marker(id,session_id,turn_id,created_at) VALUES (?,?,?,?)`,
		"crs_"+sessionID, sessionID, turnID, timestamp()); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE turn SET outcome='failed',settle='settled' WHERE id=?`, turnID); err != nil {
		return err
	}
	var uncertain bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM event WHERE turn_id=? AND kind='supervisor_activity'
		AND COALESCE(json_extract(payload_json,'$.summary'),'') NOT IN ('turn.started','system','rate_limit_event',''))`, turnID).Scan(&uncertain); err != nil {
		return err
	}
	if uncertain {
		if _, err := tx.Exec(`INSERT INTO provider_limit_retry(note_id,source_turn_id,caution)
			SELECT note_id,?,1 FROM turn_note WHERE turn_id=?
			ON CONFLICT(note_id) DO UPDATE SET source_turn_id=excluded.source_turn_id,caution=1`, turnID, turnID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE note SET state='queued' WHERE id IN (SELECT note_id FROM turn_note WHERE turn_id=?) AND state IN ('dispatched','in_turn')`, turnID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE agent SET observed='idle' WHERE id=? AND observed='in_turn'`, agentID); err != nil {
		return err
	}
	_, err := tx.AppendEvent(Event{Kind: "native_session_reset", ProjectID: projectID, RoomID: roomID, AgentID: agentID, TurnID: turnID,
		PayloadJSON: payloadJSON(map[string]string{"reason": nativeSessionMissingReason(tool), "cost_reset": "1"})})
	return err
}

// nativeSessionMissingReason names why a session was reset. Codex keeps the
// reason string it has always recorded; every later tool uses its own name.
func nativeSessionMissingReason(tool string) string {
	if tool == "codex" {
		return "codex_thread_missing"
	}
	return tool + "_session_missing"
}
