package store

import (
	"context"
	"database/sql"
	"errors"
)

// KeepAliveUsage is what a cache keep-alive ping spent, as its supervisor reported it. CostIsTotal says Cost is the harness session's running total
// (Claude's way) and not this ping's own cost (Codex's, from the price list). TokensAreTotal says the token figures are the harness session's
// running totals (a cold Codex ping reports the thread's totals), not this ping's own usage.
type KeepAliveUsage struct {
	AgentID        string
	At             string
	TokensIn       int64
	TokensOut      int64
	CacheRead      int64
	CacheWrite     int64
	Cost           float64
	CostIsTotal    bool
	TokensAreTotal bool
}

// RecordKeepAlive writes a ping's spend into usage_record (source 'keep_alive', no turn) so that usage totals, limits and the cost views count it. For
// a harness that reports a running total the ping's own cost is the difference from the session's last total, and the new total is kept, so the next
// real turn is not charged for the ping. A ping is never the first spend of its session, so when the session's chain holds no baseline (no earlier
// total) neither the cost nor totals-shaped tokens are recorded: they are the session's lifetime, and only the reported totals are kept as the new
// baseline. It does nothing for an agent that is gone or has no session, and for a ping that spent nothing. It returns the ping's own cost as
// recorded (nil when none was), which for a running total is the server's difference from what the session had reported, so a card can show it
// even when the supervisor could not tell (the first ping after it restarted).
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
		in, out, read, write := usage.TokensIn, usage.TokensOut, usage.CacheRead, usage.CacheWrite
		tokensIn, tokensOut, cacheRead, cacheWrite := &in, &out, &read, &write
		var totalsIn, totalsOut, totalsRead, totalsWrite *int64
		if usage.TokensAreTotal {
			// A cold Codex ping reports the thread's running totals: the ping's own usage is the difference from the session's
			// previous totals. A ping is never the first spend of a session, so with no token baseline the figures are the
			// thread's lifetime, not the ping's own: none are recorded, and the reported totals are kept as the baseline the next
			// real turn subtracts from.
			var prevIn, prevOut, prevRead, prevWrite sql.NullInt64
			hasPrevious, err := tx.sessionTokenTotals(sessionID, usage.AgentID, &prevIn, &prevOut, &prevRead, &prevWrite)
			if err != nil {
				return err
			}
			if hasPrevious {
				tokensIn = tokenDelta(&in, prevIn)
				tokensOut = tokenDelta(&out, prevOut)
				cacheRead = tokenDelta(&read, prevRead)
				cacheWrite = tokenDelta(&write, prevWrite)
				totalsIn = totalOr(&in, prevIn)
				totalsOut = totalOr(&out, prevOut)
				totalsRead = totalOr(&read, prevRead)
				totalsWrite = totalOr(&write, prevWrite)
			} else {
				tokensIn, tokensOut, cacheRead, cacheWrite = nil, nil, nil, nil
				totalsIn, totalsOut, totalsRead, totalsWrite = &in, &out, &read, &write
			}
		} else {
			// The ping's own delta (a warm ping): the totals it leaves the session at are remembered the same way a warm turn's
			// are, so the next cold report subtracts the ping too.
			totalsIn, totalsOut, totalsRead, totalsWrite, err = tx.totalsAfterOwn(sessionID, usage.AgentID, tokensIn, tokensOut, cacheRead, cacheWrite)
			if err != nil {
				return err
			}
		}
		switch {
		case usage.Cost > 0 && usage.CostIsTotal:
			// The running total's own cost is the difference from the session's previous total. A ping is never the first spend
			// of a session: with no cost baseline the reported total is the session's lifetime, not the ping's own, so no cost is
			// recorded and the total is kept as the baseline the next real turn is priced from.
			var previous sql.NullFloat64
			err = tx.tx.QueryRowContext(tx.ctx, previousTotalSQL, usage.AgentID, sessionID).Scan(&previous)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			remembered := usage.Cost
			total = &remembered
			if previous.Valid {
				difference := usage.Cost - previous.Float64
				if difference >= 0 {
					cost = &difference
				}
			}
		case usage.Cost > 0:
			value := usage.Cost
			cost = &value
			if total, err = tx.costTotalAfterOwn(sessionID, usage.AgentID, cost); err != nil {
				return err
			}
		}
		id, err := newID("use")
		if err != nil {
			return err
		}
		sourceID, err := newID("kae")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO usage_record(id,source_event_id,turn_id,session_id,agent_id,room_id,project_id,tokens_in,tokens_out,tokens_cache_read,tokens_cache_write,cost_est,cost_total,tokens_in_total,tokens_out_total,tokens_cache_read_total,tokens_cache_write_total,model,partial,source,created_at)
			VALUES (?,?,NULL,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,0,'keep_alive',?)`, id, sourceID, sessionID, usage.AgentID, roomID, projectID,
			tokensIn, tokensOut, cacheRead, cacheWrite, cost, total, totalsIn, totalsOut, totalsRead, totalsWrite, model, timestamp()); err != nil {
			return err
		}
		recorded = cost
		return tx.evaluateUsageFor(roomID, projectID)
	})
	return recorded, err
}
