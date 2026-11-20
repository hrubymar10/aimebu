package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"

	"github.com/goccy/go-json"
)

// The role body and the knowledge snapshot an agent's wake context carries are
// the same from turn to turn, and a harness session keeps what an earlier turn
// delivered in its own context. Resending them on every wake spends tokens for
// nothing, so a wake whose session already holds the current text is handed a
// short marker instead. The state lives in session_context_delivery, one row
// per harness session: the revision last delivered in full and the turn that
// carried it. A turn whose delivery the harness never confirmed (no supervisor
// event arrived) does not count as delivered, so a turn that was dispatched and
// then lost makes the next wake send the full text again. A compaction inside
// the session after the full delivery, a new session, or a changed revision
// does the same. When in doubt the full text goes out.

// contextRevision identifies the exact delivered content of a context block,
// so a changed block is delivered again even when its shape is unchanged.
func contextRevision(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:8])
}

// contextUnchangedMarker is what a wake carries instead of a block the session
// already holds. rev names the revision the session holds.
func contextUnchangedMarker(rev string) map[string]any {
	return map[string]any{"unchanged": true, "rev": rev}
}

// toolReportsCompaction says whether the harness's adapter reports compaction
// events and keeps a session whose loss the store can see. Only such sessions
// can be trusted to still hold a block an earlier turn delivered in full: a
// harness that compacts without reporting it can summarise the text away, and
// one that silently starts a new session when its files are gone never says
// so. Only Claude qualifies (its compaction events are reported and its
// session loss is detected), so every other harness always receives the full
// text. Keep this in step with the adapters' Capabilities.
func toolReportsCompaction(tool string) bool {
	return tool == "claude"
}

// deliverContextBlocks decides, for one normal turn of one harness session,
// whether the role body and the knowledge snapshot go out in full or as the
// unchanged marker, and records what this turn delivers. It returns the values
// to put into the turn context under role_body and knowledge. A harness whose
// adapter reports no compaction events always receives the full text.
func (tx *Tx) deliverContextBlocks(sessionID, turnID, roleBody string, knowledge map[string]any, markerAllowed bool) (roleAny any, knowledgeAny any, err error) {
	if !markerAllowed {
		return roleBody, knowledge, nil
	}
	roleRev := contextRevision(roleBody)
	knowledgeRev := contextRevision(knowledge)
	var row struct {
		roleRevision       sql.NullString
		roleTurn           sql.NullString
		roleConfirmed      sql.NullBool
		knowledgeRevision  sql.NullString
		knowledgeTurn      sql.NullString
		knowledgeConfirmed sql.NullBool
	}
	err = tx.tx.QueryRowContext(tx.ctx, `SELECT role_revision,role_turn_id,role_confirmed,knowledge_revision,knowledge_turn_id,knowledge_confirmed FROM session_context_delivery WHERE session_id=?`, sessionID).
		Scan(&row.roleRevision, &row.roleTurn, &row.roleConfirmed, &row.knowledgeRevision, &row.knowledgeTurn, &row.knowledgeConfirmed)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, err
	}
	compacted := ""
	if exists {
		// The newest compaction inside this session: a compaction at or after the turn that carried a block in full
		// distilled that turn's context, so the block goes out in full again.
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COALESCE(MAX(t.id),'') FROM event e JOIN turn t ON t.id=e.turn_id WHERE e.kind='agent_context_compacted' AND t.session_id=?`, sessionID).Scan(&compacted); err != nil {
			return nil, nil, err
		}
	}
	compactedSince := func(turn sql.NullString) bool {
		return compacted != "" && turn.Valid && turn.String != "" && compacted >= turn.String
	}
	roleFull := !exists || row.roleRevision.String != roleRev || !row.roleConfirmed.Bool || compactedSince(row.roleTurn)
	knowledgeFull := !exists || row.knowledgeRevision.String != knowledgeRev || !row.knowledgeConfirmed.Bool || compactedSince(row.knowledgeTurn)
	if roleFull {
		roleAny = roleBody
	} else {
		roleAny = contextUnchangedMarker(row.roleRevision.String)
	}
	if knowledgeFull {
		knowledgeAny = knowledge
	} else {
		knowledgeAny = contextUnchangedMarker(row.knowledgeRevision.String)
	}
	// Record what this turn delivers: a full delivery replaces the remembered revision and turn, unconfirmed until the
	// harness shows life; a marker leaves what the session holds as it is.
	newRoleRevision, newRoleTurn, newRoleConfirmed := row.roleRevision.String, row.roleTurn.String, row.roleConfirmed.Bool
	newKnowledgeRevision, newKnowledgeTurn, newKnowledgeConfirmed := row.knowledgeRevision.String, row.knowledgeTurn.String, row.knowledgeConfirmed.Bool
	if roleFull {
		newRoleRevision, newRoleTurn, newRoleConfirmed = roleRev, turnID, false
	}
	if knowledgeFull {
		newKnowledgeRevision, newKnowledgeTurn, newKnowledgeConfirmed = knowledgeRev, turnID, false
	}
	confirmed := 0
	if newRoleConfirmed {
		confirmed = 1
	}
	knowledgeConfirmedValue := 0
	if newKnowledgeConfirmed {
		knowledgeConfirmedValue = 1
	}
	if _, err := tx.Exec(`INSERT INTO session_context_delivery(session_id,role_revision,role_turn_id,role_confirmed,knowledge_revision,knowledge_turn_id,knowledge_confirmed)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(session_id) DO UPDATE SET role_revision=excluded.role_revision,role_turn_id=excluded.role_turn_id,role_confirmed=excluded.role_confirmed,
		knowledge_revision=excluded.knowledge_revision,knowledge_turn_id=excluded.knowledge_turn_id,knowledge_confirmed=excluded.knowledge_confirmed`,
		sessionID, newRoleRevision, newRoleTurn, confirmed, newKnowledgeRevision, newKnowledgeTurn, knowledgeConfirmedValue); err != nil {
		return nil, nil, err
	}
	return roleAny, knowledgeAny, nil
}

// confirmContextDelivery marks the full deliveries a turn carried as seen by
// the harness: the turn's usage event proves the harness got far enough to
// take the context in. A delivery that is never confirmed (the turn was lost,
// or never reported usage) makes the next wake send the full text again.
func (tx *Tx) confirmContextDelivery(turnID string) error {
	if _, err := tx.Exec(`UPDATE session_context_delivery SET role_confirmed=1 WHERE role_turn_id=? AND role_confirmed=0`, turnID); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE session_context_delivery SET knowledge_confirmed=1 WHERE knowledge_turn_id=? AND knowledge_confirmed=0`, turnID)
	return err
}

// InvalidateContextDelivery drops the context-delivery state of the live
// session of one agent, so the next wake carries the role body and knowledge
// in full. A keep-alive ping's compaction (which no turn row records) calls
// this: the ping resumed the same native session, so what earlier turns
// delivered in full may have been summarised away. An agent that is gone or
// has no live session has nothing to invalidate.
func (s *Store) InvalidateContextDelivery(ctx context.Context, agentID string) error {
	if err := s.checkTxReentry(ctx); err != nil {
		return err
	}
	return s.Tx(ctx, func(tx *Tx) error {
		var sessionID string
		var generation int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT generation FROM agent WHERE id=?`, agentID).Scan(&generation); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM session WHERE agent_id=? AND generation=? AND ended_at IS NULL ORDER BY started_at DESC LIMIT 1`, agentID, generation).Scan(&sessionID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		_, err := tx.Exec(`DELETE FROM session_context_delivery WHERE session_id=?`, sessionID)
		return err
	})
}
