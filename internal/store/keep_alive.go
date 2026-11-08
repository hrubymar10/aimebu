package store

import (
	"context"
	"database/sql"
	"errors"
)

// KeepAliveUsage is what a cache keep-alive ping spent, as its supervisor reported it. CostIsTotal says Cost is the harness session's running total
// (Claude's way) and not this ping's own cost (Codex's, from the price list).
type KeepAliveUsage struct {
	AgentID     string
	At          string
	TokensIn    int64
	TokensOut   int64
	CacheRead   int64
	CacheWrite  int64
	Cost        float64
	CostIsTotal bool
}

// RecordKeepAlive writes a ping's spend into usage_record (source 'keep_alive', no turn) so that usage totals, limits and the cost views count it. For
// a harness that reports a running total the ping's own cost is the difference from the session's last total, and the new total is kept, so the next
// real turn is not charged for the ping. It does nothing for an agent that is gone or has no session, and for a ping that spent nothing. It returns
// the ping's own cost as recorded (nil when none was), which for a running total is the server's difference from what the session had reported, so a
// card can show it even when the supervisor could not tell (the first ping after it restarted).
func (s *Store) RecordKeepAlive(ctx context.Context, usage KeepAliveUsage) (*float64, error) {
	if usage.TokensIn == 0 && usage.TokensOut == 0 && usage.CacheRead == 0 && usage.CacheWrite == 0 && usage.Cost == 0 {
		return nil, nil
	}
	var recorded *float64
	err := s.Tx(ctx, func(tx *Tx) error {
		var projectID, roomID, model string
		var generation int
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.project_id,COALESCE(NULLIF(a.home_room_id,''),(SELECT t.room_id FROM turn t WHERE t.agent_id=a.id ORDER BY t.id DESC LIMIT 1),''),COALESCE(a.model,''),a.generation FROM agent a WHERE a.id=?`, usage.AgentID).Scan(&projectID, &roomID, &model, &generation)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var sessionID string
		err = tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM session WHERE agent_id=? AND generation=? AND ended_at IS NULL ORDER BY started_at DESC LIMIT 1`, usage.AgentID, generation).Scan(&sessionID)
		if errors.Is(err, sql.ErrNoRows) || roomID == "" {
			return nil
		}
		if err != nil {
			return err
		}
		var cost, total *float64
		switch {
		case usage.Cost > 0 && usage.CostIsTotal:
			if cost, total, err = tx.costSinceLastTotalInSession(sessionID, usage.AgentID, usage.Cost); err != nil {
				return err
			}
		case usage.Cost > 0:
			value := usage.Cost
			cost = &value
		}
		id, err := newID("use")
		if err != nil {
			return err
		}
		sourceID, err := newID("kae")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO usage_record(id,source_event_id,turn_id,session_id,agent_id,room_id,project_id,tokens_in,tokens_out,tokens_cache_read,tokens_cache_write,cost_est,cost_total,model,partial,source,created_at)
			VALUES (?,?,NULL,?,?,?,?,?,?,?,?,?,?,?,0,'keep_alive',?)`, id, sourceID, sessionID, usage.AgentID, roomID, projectID,
			usage.TokensIn, usage.TokensOut, usage.CacheRead, usage.CacheWrite, cost, total, model, timestamp()); err != nil {
			return err
		}
		recorded = cost
		return tx.evaluateUsageFor(roomID, projectID)
	})
	return recorded, err
}
