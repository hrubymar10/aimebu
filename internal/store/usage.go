package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/goccy/go-json"
)

// AgentUsage holds the latest measured context for the current process
// generation and the sum of reported costs over the agent's lifetime.
type AgentUsage struct {
	TokensIn      *int64   `json:"tokens_in,omitempty"`
	TokensOut     *int64   `json:"tokens_out,omitempty"`
	ContextUsed   *int64   `json:"context_used,omitempty"`
	ContextWindow *int64   `json:"context_window,omitempty"`
	CostSoFar     *float64 `json:"cost_so_far,omitempty"`
	CostEstimated bool     `json:"cost_estimated,omitempty"`
}

// latestUsageSQL reads the newest record (by timestamp, the later insert on a tie) of the agent's current process generation. A partial record
// (a turn that ended with no report) carries no context, so it is skipped: the card shows the last complete report. The generation comes from a subquery, not a join, so the
// agent+time index also gives the order and no sort is needed (the agent's history can be long).
const latestUsageSQL = `SELECT u.tokens_in,u.tokens_out,u.context_used,u.context_window
	FROM usage_record u JOIN turn t ON t.id=u.turn_id
	WHERE u.agent_id=? AND u.partial=0 AND t.generation=(SELECT generation FROM agent WHERE id=?) ORDER BY u.created_at DESC,u.rowid DESC LIMIT 1`

// agentCostSQL is the agent's cost over its lifetime.
const agentCostSQL = `SELECT SUM(cost_est) FROM usage_record WHERE agent_id=?`

func (s *Store) AgentUsage(ctx context.Context, agentID string) (*AgentUsage, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	var input, output, used, window sql.NullInt64
	err := s.db.QueryRowContext(ctx, latestUsageSQL, agentID, agentID).
		Scan(&input, &output, &used, &window)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	usage := &AgentUsage{}
	if input.Valid {
		usage.TokensIn = &input.Int64
	}
	if output.Valid {
		usage.TokensOut = &output.Int64
	}
	if used.Valid {
		usage.ContextUsed = &used.Int64
	}
	if window.Valid {
		usage.ContextWindow = &window.Int64
	}
	var cost sql.NullFloat64
	if err := s.db.QueryRowContext(ctx, agentCostSQL, agentID).Scan(&cost); err != nil {
		return nil, err
	}
	if cost.Valid {
		usage.CostSoFar = &cost.Float64
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM usage_record WHERE agent_id=? AND cost_est IS NOT NULL AND source='estimated')`, agentID).Scan(&usage.CostEstimated); err != nil {
			return nil, err
		}
	}
	if err == sql.ErrNoRows && !cost.Valid {
		return nil, nil
	}
	return usage, nil
}

func (tx *Tx) recordUsageOrCompaction(turnID, agentID, projectID, roomID, sourceID, kind string, payload []byte) error {
	var frame struct {
		Usage *struct {
			TokensIn         *int64   `json:"tokens_in"`
			TokensOut        *int64   `json:"tokens_out"`
			TokensCacheRead  *int64   `json:"tokens_cache_read"`
			TokensCacheWrite *int64   `json:"tokens_cache_write"`
			ContextUsed      *int64   `json:"context_used"`
			ContextWindow    *int64   `json:"context_window"`
			CostEstimate     *float64 `json:"cost_estimate"`
			CostSource       string   `json:"cost_source"`
			CostCumulative   bool     `json:"cost_cumulative"`
			TokensCumulative bool     `json:"tokens_cumulative"`
			Model            string   `json:"model"`
			Partial          bool     `json:"partial"`
		} `json:"usage"`
		Compaction *struct {
			PreTokens  *int64 `json:"pre_tokens"`
			PostTokens *int64 `json:"post_tokens"`
		} `json:"compaction"`
	}
	if kind != "usage" && kind != "compaction" {
		return nil
	}
	if err := json.Unmarshal(payload, &frame); err != nil {
		return err
	}
	if kind == "usage" && frame.Usage != nil {
		id, err := newID("use")
		if err != nil {
			return err
		}
		u := frame.Usage
		var sessionID string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT session_id FROM turn WHERE id=?`, turnID).Scan(&sessionID); err != nil {
			return err
		}
		cost, total := u.CostEstimate, (*float64)(nil)
		costUnknownBaseline := false
		if u.CostCumulative && u.CostEstimate != nil {
			cost, total, costUnknownBaseline, err = tx.costSinceLastTotalInSession(sessionID, agentID, *u.CostEstimate)
			if err != nil {
				return err
			}
		}
		tokensIn, tokensOut, cacheRead, cacheWrite := u.TokensIn, u.TokensOut, u.TokensCacheRead, u.TokensCacheWrite
		var totalsIn, totalsOut, totalsRead, totalsWrite *int64
		tokensUnknownBaseline := false
		if u.TokensCumulative {
			// The harness reports the session's running totals (a cold Codex thread reports the thread's totals, earlier turns
			// included). Turn them into this turn's own usage by subtracting the session's previous totals, and remember the
			// new totals either way, so the next turn subtracts from the latest one. A session whose earlier totals the store
			// never measured records no tokens, rather than the whole lifetime.
			tokensIn, tokensOut, cacheRead, cacheWrite, totalsIn, totalsOut, totalsRead, totalsWrite, tokensUnknownBaseline, err = tx.tokensSinceLastTotalInSession(sessionID, agentID, u.TokensIn, u.TokensOut, u.TokensCacheRead, u.TokensCacheWrite)
			if err != nil {
				return err
			}
			if tokensUnknownBaseline && cost != nil {
				// The cost was priced over the reported totals, so without a token baseline it is no more knowable than they are.
				cost = nil
			}
			costUnknownBaseline = costUnknownBaseline || tokensUnknownBaseline
		} else {
			// Own per-turn deltas (a warm Codex turn, OpenCode, Vibe): remember the totals the session is left at, so the next
			// cumulative report subtracts the right baseline instead of counting these turns again. A cold Codex turn after warm
			// ones reports the thread's totals since its beginning, warm turns included.
			totalsIn, totalsOut, totalsRead, totalsWrite, err = tx.totalsAfterOwn(sessionID, agentID, tokensIn, tokensOut, cacheRead, cacheWrite)
			if err != nil {
				return err
			}
			if !u.CostCumulative {
				if total, err = tx.costTotalAfterOwn(sessionID, agentID, cost); err != nil {
					return err
				}
			}
		}
		// The report is recorded, so a session reset marker has served its purpose: the next report is priced against what this
		// report left behind. A keep-alive ping never reaches here, so a ping after a reset leaves the marker standing for the
		// session's next real turn.
		if err := tx.clearCostResetMarker(sessionID); err != nil {
			return err
		}
		model := u.Model
		if model == "" {
			// The agent's configured model, when the harness did not name one.
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COALESCE(model,'') FROM agent WHERE id=?`, agentID).Scan(&model); err != nil {
				return err
			}
		}
		partial := 0
		if u.Partial {
			partial = 1
		}
		source := "measured"
		if cost != nil && u.CostSource == "estimated" {
			source = "estimated"
		}
		if costUnknownBaseline {
			// A report the store cannot turn into this turn's own spend is marked estimated rather than measured, and never
			// recorded as the session's whole lifetime.
			source = "estimated"
		}
		_, err = tx.Exec(`INSERT INTO usage_record(id,source_event_id,turn_id,agent_id,room_id,project_id,tokens_in,tokens_out,tokens_cache_read,tokens_cache_write,context_used,context_window,cost_est,cost_total,tokens_in_total,tokens_out_total,tokens_cache_read_total,tokens_cache_write_total,model,partial,source,created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, sourceID, turnID, agentID, roomID, projectID,
			tokensIn, tokensOut, cacheRead, cacheWrite, u.ContextUsed, u.ContextWindow, cost, total, totalsIn, totalsOut, totalsRead, totalsWrite, model, partial, source, timestamp())
		if err != nil {
			return err
		}
		return tx.evaluateUsageFor(roomID, projectID)
	}
	if kind == "compaction" && frame.Compaction != nil {
		var slug string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT slug FROM agent WHERE id=?`, agentID).Scan(&slug); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(Event{Kind: "agent_context_compacted", ProjectID: projectID, RoomID: roomID, AgentID: agentID, TurnID: turnID,
			PayloadJSON: payloadJSON(map[string]string{"pre_tokens": formatMeasuredTokenCount(frame.Compaction.PreTokens), "post_tokens": formatMeasuredTokenCount(frame.Compaction.PostTokens)})}); err != nil {
			return err
		}
		_, err := tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "system", AuthorID: "system",
			Body: fmt.Sprintf("%s's context was compacted (%s → %s tokens)", slug, formatMeasuredTokenCount(frame.Compaction.PreTokens), formatMeasuredTokenCount(frame.Compaction.PostTokens))})
		return err
	}
	return nil
}

func formatMeasuredTokenCount(measured *int64) string {
	if measured == nil || *measured <= 0 {
		return "—"
	}
	value := *measured
	if value < 1000 {
		return fmt.Sprint(value)
	}
	if value%1000 == 0 {
		return fmt.Sprintf("%dk", value/1000)
	}
	return fmt.Sprintf("%.1fk", float64(value)/1000)
}

// ensureUsageTotalColumns adds the four session-total token columns of usage_record unless the database already has them, so a
// migration replayed over a rewound database does not collide with columns it added the first time.
func ensureUsageTotalColumns(ctx context.Context, tx *sql.Tx) error {
	for _, column := range []string{"tokens_in_total", "tokens_out_total", "tokens_cache_read_total", "tokens_cache_write_total"} {
		var present int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('usage_record') WHERE name=?`, column).Scan(&present); err != nil {
			return err
		}
		if present > 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `ALTER TABLE usage_record ADD COLUMN `+column+` INTEGER`); err != nil {
			return err
		}
	}
	return nil
}

// costSinceLastTotalInSession turns a session's running total into one report's own cost: a turn or a keep-alive ping known by its session.
// The first report of a fresh session is its own cost. The first report of a session that continues earlier history the store never measured
// (a native resume, a fork) has no baseline: its whole lifetime is not its own cost, so none is recorded and the row is marked estimated. A
// difference that is negative (the harness restarted its count) is unknown, never zero, and a total is kept either way, so the next turn
// subtracts from the latest one. It returns the report's cost (nil when unknown), the total to remember, and whether the cost is unknown
// because the session's earlier total was never measured.
func (tx *Tx) costSinceLastTotalInSession(sessionID, agentID string, total float64) (*float64, *float64, bool, error) {
	var previous sql.NullFloat64
	err := tx.tx.QueryRowContext(tx.ctx, previousTotalSQL, agentID, sessionID).Scan(&previous)
	if err != nil && err != sql.ErrNoRows {
		return nil, nil, false, err
	}
	remembered := total
	if !previous.Valid {
		marker, err := tx.sessionCostWasReset(sessionID)
		if err != nil {
			return nil, nil, false, err
		}
		resumed := false
		if !marker {
			resumed, err = tx.sessionContinuesUnmeasuredHistory(sessionID)
			if err != nil {
				return nil, nil, false, err
			}
			if !resumed {
				// A chain whose earlier rows carry no cost total has no cost baseline when one of them ever recorded a cost (a
				// report the harness could not price, a pre-fix row): that spend is not this report's own, and its total was never
				// known, so the whole lifetime cannot be priced as this turn's own. A chain where no row ever recorded a cost has
				// nothing earlier to charge this report with: the uncharged cumulative total is real spend, and recording it seeds
				// the baseline for the turns after.
				var priced int
				if err := tx.tx.QueryRowContext(tx.ctx, sessionChainCostSQL, agentID, sessionID).Scan(&priced); err != nil {
					return nil, nil, false, err
				}
				resumed = priced > 0
			}
		}
		if resumed {
			// The report covers history the store has no cost total for: the whole lifetime is never a turn's own cost.
			return nil, &remembered, true, nil
		}
		// The first report of a session that started empty (a reset marks this case): the total is its own cost. The reset
		// marker is consumed once the report is recorded, not here: the token path reads the same marker, and a keep-alive ping
		// (which never records a fresh baseline) must leave it standing.
		return &total, &remembered, false, nil
	}
	difference := total - previous.Float64
	if difference < 0 {
		return nil, &remembered, false, nil
	}
	return &difference, &remembered, false, nil
}

// sessionContinuesUnmeasuredHistory says whether the harness session carries earlier history whose spend the store never recorded: a session
// resumed from a native session aimebu gave no earlier session row to (a v1 legacy session, or a report older than the totals), or a fork of
// another session. A session whose native thread was reset (recordNativeSessionReset) is treated as fresh even though its earlier rows
// remain: the new thread's totals started at zero, so its first report is its own spend. A fresh session's first report is its own spend.
func (tx *Tx) sessionContinuesUnmeasuredHistory(sessionID string) (bool, error) {
	reset, err := tx.sessionCostWasReset(sessionID)
	if err != nil || reset {
		return false, err
	}
	var mode, forkOf string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT start_mode,COALESCE(fork_of,'') FROM session WHERE id=?`, sessionID).Scan(&mode, &forkOf); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return mode == "native_resume" || forkOf != "", nil
}

// sessionCostWasReset says whether the session's native thread was reset and no report has followed yet: the reset marker
// (cost_reset_marker) is written when the reset forgets the chain's totals, and consumed when the first report after the reset is
// baselined as a fresh session's own spend.
func (tx *Tx) sessionCostWasReset(sessionID string) (bool, error) {
	var n int
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM cost_reset_marker WHERE session_id=?`, sessionID).Scan(&n); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return n > 0, nil
}

// clearCostResetMarker consumes the reset marker once a report has been baselined against it.
func (tx *Tx) clearCostResetMarker(sessionID string) error {
	_, err := tx.Exec(`DELETE FROM cost_reset_marker WHERE session_id=?`, sessionID)
	return err
}

// tokensSinceLastTotalInSession turns a harness session's running token totals into one report's own usage, the way costSinceLastTotalInSession
// turns a running cost total into the report's own cost. It returns the report's own token figures (nil where unknown), the session's totals to
// remember, and whether the tokens are unknown because the session's earlier totals were never measured.
func (tx *Tx) tokensSinceLastTotalInSession(sessionID, agentID string, in, out, read, write *int64) (tokensIn, tokensOut, cacheRead, cacheWrite, totalsIn, totalsOut, totalsRead, totalsWrite *int64, unknownBaseline bool, err error) {
	var prevIn, prevOut, prevRead, prevWrite sql.NullInt64
	hasPrevious, err := tx.sessionTokenTotals(sessionID, agentID, &prevIn, &prevOut, &prevRead, &prevWrite)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, nil, false, err
	}
	if hasPrevious {
		tokensIn = tokenDelta(in, prevIn)
		tokensOut = tokenDelta(out, prevOut)
		cacheRead = tokenDelta(read, prevRead)
		cacheWrite = tokenDelta(write, prevWrite)
		// A category the harness did not report leaves the remembered total as it was.
		totalsIn, totalsOut, totalsRead, totalsWrite = totalOr(in, prevIn), totalOr(out, prevOut), totalOr(read, prevRead), totalOr(write, prevWrite)
		return tokensIn, tokensOut, cacheRead, cacheWrite, totalsIn, totalsOut, totalsRead, totalsWrite, false, nil
	}
	marker, err := tx.sessionCostWasReset(sessionID)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, nil, false, err
	}
	resumed := false
	if !marker {
		resumed, err = tx.sessionContinuesUnmeasuredHistory(sessionID)
		if err != nil {
			return nil, nil, nil, nil, nil, nil, nil, nil, false, err
		}
		var history int
		if err := tx.tx.QueryRowContext(tx.ctx, sessionChainUsageSQL, agentID, sessionID).Scan(&history); err != nil {
			return nil, nil, nil, nil, nil, nil, nil, nil, false, err
		}
		resumed = resumed || history > 0
	}
	if resumed {
		// The report covers history the store has no totals for: the whole lifetime is never a turn's own usage. The reported
		// totals are still remembered, so the next turn subtracts from them.
		return nil, nil, nil, nil, in, out, read, write, true, nil
	}
	// The first report of a session that started empty (a reset marks this case): the totals are this turn's own usage. The reset
	// marker is consumed once the report is recorded, not here: the cost path reads the same marker, and a keep-alive ping (which
	// never records a fresh baseline) must leave it standing.
	return in, out, read, write, in, out, read, write, false, nil
}

// totalsAfterOwn returns the session's token totals a row of own per-turn deltas leaves the session at, so the next cumulative report
// subtracts the right baseline instead of counting these turns again: the previous totals plus the deltas when the chain has a baseline, the
// deltas themselves when the row is the first report of a session that started empty (a cost reset marks that case), and nothing when the
// session continues history the store never measured or holds earlier rows without totals.
func (tx *Tx) totalsAfterOwn(sessionID, agentID string, in, out, read, write *int64) (totalsIn, totalsOut, totalsRead, totalsWrite *int64, err error) {
	var prevIn, prevOut, prevRead, prevWrite sql.NullInt64
	hasPrevious, err := tx.sessionTokenTotals(sessionID, agentID, &prevIn, &prevOut, &prevRead, &prevWrite)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if hasPrevious {
		return intTotalAfter(prevIn, in), intTotalAfter(prevOut, out), intTotalAfter(prevRead, read), intTotalAfter(prevWrite, write), nil
	}
	reset, err := tx.sessionCostWasReset(sessionID)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if reset {
		// The session's thread was reset and no report has followed yet: this warm turn is the fresh thread's own spend, and its
		// deltas are the totals the next cold report is priced against. The marker is consumed once the report is recorded.
		return in, out, read, write, nil
	}
	resumed, err := tx.sessionContinuesUnmeasuredHistory(sessionID)
	if err != nil || resumed {
		return nil, nil, nil, nil, err
	}
	var history int
	if err := tx.tx.QueryRowContext(tx.ctx, sessionChainUsageSQL, agentID, sessionID).Scan(&history); err != nil {
		return nil, nil, nil, nil, err
	}
	if history > 0 {
		// Earlier rows the store cannot build a baseline from: their totals were never recorded.
		return nil, nil, nil, nil, nil
	}
	// The first report of a session that started empty: the deltas are the totals.
	return in, out, read, write, nil
}

// intTotalAfter is one category's session total after a row of own deltas: the previous total plus the delta, the previous total kept when
// this row did not report the category, and nothing when neither side is known.
func intTotalAfter(previous sql.NullInt64, own *int64) *int64 {
	if own == nil {
		if previous.Valid {
			value := previous.Int64
			return &value
		}
		return nil
	}
	if !previous.Valid {
		return nil
	}
	total := previous.Int64 + *own
	return &total
}

// costTotalAfterOwn returns the session's running cost total a row of own per-turn cost leaves the session at, the way totalsAfterOwn works
// for tokens: the previous total plus this row's own cost when the chain has a baseline, the own cost when the row is the first report of a
// session that started empty (a cost reset marks that case), and nothing when the session continues history the store never measured. The
// prices are linear in the tokens, so the derived total is the running total the next cumulative report is priced against.
func (tx *Tx) costTotalAfterOwn(sessionID, agentID string, cost *float64) (*float64, error) {
	if cost == nil {
		return nil, nil
	}
	var previous sql.NullFloat64
	err := tx.tx.QueryRowContext(tx.ctx, previousTotalSQL, agentID, sessionID).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if previous.Valid {
		total := previous.Float64 + *cost
		return &total, nil
	}
	reset, err := tx.sessionCostWasReset(sessionID)
	if err != nil {
		return nil, err
	}
	if reset {
		// The session's thread was reset and no report has followed yet: this warm turn's own cost starts the fresh thread's total.
		return cost, nil
	}
	resumed, err := tx.sessionContinuesUnmeasuredHistory(sessionID)
	if err != nil || resumed {
		return nil, err
	}
	var history int
	if err := tx.tx.QueryRowContext(tx.ctx, sessionChainUsageSQL, agentID, sessionID).Scan(&history); err != nil {
		return nil, err
	}
	if history > 0 {
		return nil, nil
	}
	return cost, nil
}

// tokenDelta is what one category spent on its own: the difference from the session's previous total, nil when either side is unknown or the
// difference is negative (the harness restarted its count).
func tokenDelta(reported *int64, previous sql.NullInt64) *int64 {
	if reported == nil || !previous.Valid {
		return nil
	}
	difference := *reported - previous.Int64
	if difference < 0 {
		return nil
	}
	return &difference
}

// totalOr keeps the reported total, or the remembered one when the harness did not report the category this time.
func totalOr(reported *int64, previous sql.NullInt64) *int64 {
	if reported != nil {
		return reported
	}
	if previous.Valid {
		value := previous.Int64
		return &value
	}
	return nil
}

// sessionTokenTotals reads the session's most recent remembered token totals, following a native session across stopped/start generations the
// way previousTotalSQL follows the cost total. It says whether any were found.
func (tx *Tx) sessionTokenTotals(sessionID, agentID string, in, out, read, write *sql.NullInt64) (bool, error) {
	err := tx.tx.QueryRowContext(tx.ctx, previousTokenTotalsSQL, agentID, sessionID).Scan(in, out, read, write)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// previousTokenTotalsSQL follows a native session across stopped/start generations, as previousTotalSQL does for the cost total. It takes the
// latest row with any non-null token total: a row whose totals were never recorded is skipped, but a row that reported some categories (and
// not others) still baselines those, and the per-category delta handles the missing ones.
const previousTokenTotalsSQL = `SELECT u.tokens_in_total,u.tokens_out_total,u.tokens_cache_read_total,u.tokens_cache_write_total FROM usage_record u LEFT JOIN turn t ON t.id=u.turn_id
	WHERE u.agent_id=? AND (u.tokens_in_total IS NOT NULL OR u.tokens_out_total IS NOT NULL OR u.tokens_cache_read_total IS NOT NULL OR u.tokens_cache_write_total IS NOT NULL) AND EXISTS (
		SELECT 1 FROM session current JOIN session prior ON prior.id=COALESCE(t.session_id,u.session_id)
		WHERE current.id=? AND (current.id=prior.id OR
			(current.native_session_id IS NOT NULL AND current.native_session_id=prior.native_session_id)))
	ORDER BY u.created_at DESC,u.rowid DESC LIMIT 1`

// sessionChainUsageSQL counts the usage rows already recorded for a native session's chain, whatever their shape: a session with earlier
// measured history has no baseline even when none of it remembered totals.
const sessionChainUsageSQL = `SELECT COUNT(*) FROM usage_record u LEFT JOIN turn t ON t.id=u.turn_id
	WHERE u.agent_id=? AND EXISTS (
		SELECT 1 FROM session current JOIN session prior ON prior.id=COALESCE(t.session_id,u.session_id)
		WHERE current.id=? AND (current.id=prior.id OR
			(current.native_session_id IS NOT NULL AND current.native_session_id=prior.native_session_id)))`

// sessionChainCostSQL counts the chain's rows that ever recorded a cost: an unknown cost baseline exists only when an earlier row charged
// spend the store no longer has a running total for. A chain where no row ever recorded a cost leaves the next cumulative total uncharged
// spend, which is recorded as the fresh baseline.
const sessionChainCostSQL = `SELECT COUNT(*) FROM usage_record u LEFT JOIN turn t ON t.id=u.turn_id
	WHERE u.agent_id=? AND u.cost_est IS NOT NULL AND EXISTS (
		SELECT 1 FROM session current JOIN session prior ON prior.id=COALESCE(t.session_id,u.session_id)
		WHERE current.id=? AND (current.id=prior.id OR
			(current.native_session_id IS NOT NULL AND current.native_session_id=prior.native_session_id)))`

// previousTotalSQL follows a native session across stopped/start generations.
// A resumed Claude turn reports the whole native session's cost, even when
// aimebu gave that continuation a new session row.
const previousTotalSQL = `SELECT u.cost_total FROM usage_record u LEFT JOIN turn t ON t.id=u.turn_id
	WHERE u.agent_id=? AND u.cost_total IS NOT NULL AND EXISTS (
		SELECT 1 FROM session current JOIN session prior ON prior.id=COALESCE(t.session_id,u.session_id)
		WHERE current.id=? AND (current.id=prior.id OR
			(current.native_session_id IS NOT NULL AND current.native_session_id=prior.native_session_id)))
	ORDER BY u.created_at DESC,u.rowid DESC LIMIT 1`
