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
}

// latestUsageSQL reads the newest record (by timestamp, the later insert on a tie) of the agent's current process generation. The generation comes from a subquery, not a join, so the
// agent+time index also gives the order and no sort is needed (the agent's history can be long).
const latestUsageSQL = `SELECT u.tokens_in,u.tokens_out,u.context_used,u.context_window
	FROM usage_record u JOIN turn t ON t.id=u.turn_id
	WHERE u.agent_id=? AND t.generation=(SELECT generation FROM agent WHERE id=?) ORDER BY u.created_at DESC,u.rowid DESC LIMIT 1`

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
	}
	if err == sql.ErrNoRows && !cost.Valid {
		return nil, nil
	}
	return usage, nil
}

func (tx *Tx) recordUsageOrCompaction(turnID, agentID, projectID, roomID, sourceID, kind string, payload []byte) error {
	var frame struct {
		Usage *struct {
			TokensIn      *int64   `json:"tokens_in"`
			TokensOut     *int64   `json:"tokens_out"`
			ContextUsed   *int64   `json:"context_used"`
			ContextWindow *int64   `json:"context_window"`
			CostEstimate  *float64 `json:"cost_estimate"`
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
		_, err = tx.Exec(`INSERT INTO usage_record(id,source_event_id,turn_id,agent_id,room_id,project_id,tokens_in,tokens_out,context_used,context_window,cost_est,source,created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,'measured',?)`, id, sourceID, turnID, agentID, roomID, projectID,
			frame.Usage.TokensIn, frame.Usage.TokensOut, frame.Usage.ContextUsed, frame.Usage.ContextWindow, frame.Usage.CostEstimate, timestamp())
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
