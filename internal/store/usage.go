package store

import (
	"context"
	"database/sql"
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
		cost, total := u.CostEstimate, (*float64)(nil)
		if u.CostCumulative && u.CostEstimate != nil {
			if cost, total, err = tx.costSinceLastTotal(turnID, agentID, *u.CostEstimate); err != nil {
				return err
			}
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
		_, err = tx.Exec(`INSERT INTO usage_record(id,source_event_id,turn_id,agent_id,room_id,project_id,tokens_in,tokens_out,tokens_cache_read,tokens_cache_write,context_used,context_window,cost_est,cost_total,model,partial,source,created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, sourceID, turnID, agentID, roomID, projectID,
			u.TokensIn, u.TokensOut, u.TokensCacheRead, u.TokensCacheWrite, u.ContextUsed, u.ContextWindow, cost, total, model, partial, source, timestamp())
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
		_, err := tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "system", AuthorID: "system",
			Body: fmt.Sprintf("%s's memory was trimmed (%s → %s tokens)", slug, formatMeasuredTokenCount(frame.Compaction.PreTokens), formatMeasuredTokenCount(frame.Compaction.PostTokens))})
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

// costSinceLastTotal turns a session's running total into the cost of this turn alone. A resumed Claude session reports everything the session
// has spent, so each turn's own cost is the difference from the total at the session's previous report. The first report of a session is its
// own cost. A difference that is negative (the harness restarted its count) is unknown, never zero, and a total is kept either way, so the next
// turn subtracts from the latest one. It returns the turn's cost (nil when unknown) and the total to remember.
func (tx *Tx) costSinceLastTotal(turnID, agentID string, total float64) (*float64, *float64, error) {
	var sessionID string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT session_id FROM turn WHERE id=?`, turnID).Scan(&sessionID); err != nil {
		return nil, nil, err
	}
	var previous sql.NullFloat64
	err := tx.tx.QueryRowContext(tx.ctx, previousTotalSQL, agentID, sessionID).Scan(&previous)
	if err != nil && err != sql.ErrNoRows {
		return nil, nil, err
	}
	remembered := total
	if !previous.Valid {
		return &total, &remembered, nil
	}
	difference := total - previous.Float64
	if difference < 0 {
		return nil, &remembered, nil
	}
	return &difference, &remembered, nil
}

// previousTotalSQL finds the running total of the agent's session at its latest report; the agent+time index gives the order, and the walk
// stops at the first match, which is nearly always the newest row.
const previousTotalSQL = `SELECT u.cost_total FROM usage_record u JOIN turn t ON t.id=u.turn_id
	WHERE u.agent_id=? AND t.session_id=? AND u.cost_total IS NOT NULL ORDER BY u.created_at DESC,u.rowid DESC LIMIT 1`
