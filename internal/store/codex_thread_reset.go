package store

import (
	"regexp"
	"strings"
)

var codexThreadNotFound = regexp.MustCompile(`thread [0-9A-Za-z-]+ not found`)

// nativeSessionMissingEvidence reports a harness's own wording for a resumed
// session it can no longer find. Codex needs both phrases of v1's
// agentCodexThreadNotFoundRE so an unrelated "not found" cannot trigger a
// reset; OpenCode prints its one line on stderr.
func nativeSessionMissingEvidence(tool, stderr, errorEvent string) bool {
	evidence := stderr + "\n" + errorEvent
	switch tool {
	case "codex":
		return strings.Contains(evidence, "failed to record rollout items") && codexThreadNotFound.MatchString(evidence)
	case "opencode":
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
	if _, err := tx.Exec(`UPDATE session SET native_session_id=NULL,native_session_started_at=NULL WHERE id=?`, sessionID); err != nil {
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
		PayloadJSON: payloadJSON(map[string]string{"reason": nativeSessionMissingReason(tool)})})
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
