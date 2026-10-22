package store

import (
	"context"
	"database/sql"
	"strings"
)

// MessageUsage is what the turn that wrote a message spent. A turn can write several messages (Index of Count), and every one of them
// carries the turn's figures: they are the turn's, not the message's, so nothing may add them up across messages (the totals sum
// usage_record, which holds each turn once). A figure the harness did not report is nil, never zero.
type MessageUsage struct {
	TurnID     string   `json:"turn_id"`
	Index      int      `json:"index"`
	Count      int      `json:"count"`
	TokensIn   *int64   `json:"tokens_in,omitempty"`
	TokensOut  *int64   `json:"tokens_out,omitempty"`
	CacheRead  *int64   `json:"tokens_cache_read,omitempty"`
	CacheWrite *int64   `json:"tokens_cache_write,omitempty"`
	Cost       *float64 `json:"cost,omitempty"`
	Model      string   `json:"model,omitempty"`
	// Partial is true when part of the turn's spend was not seen (it ended without a report), so the figures are a floor.
	Partial bool `json:"partial,omitempty"`
}

// UsageForMessages reads, for a page, the usage of the turn behind each agent message, keyed by message ID. Three queries serve the whole
// page whatever its size: which turn wrote each message (through the outbox), how many messages each of those turns wrote, and the turns'
// totals. A message that did not come from a turn, or whose turn reported nothing, has no entry.
func (s *Store) UsageForMessages(ctx context.Context, messages []Message) (map[string]MessageUsage, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	out := make(map[string]MessageUsage)
	var outIDs []any
	byOut := map[string][]string{} // outbox id -> message ids (one, but keyed so the lookup is one query)
	for _, message := range messages {
		if message.AuthorKind == "agent" && message.SourceOutID != "" {
			outIDs = append(outIDs, message.SourceOutID)
			byOut[message.SourceOutID] = append(byOut[message.SourceOutID], message.ID)
		}
	}
	if len(outIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,turn_id FROM outbox WHERE id IN (`+marks(len(outIDs))+`)`, outIDs...)
	if err != nil {
		return nil, err
	}
	turnOf := map[string]string{} // message id -> turn id
	var turnIDs []any
	seenTurn := map[string]bool{}
	for rows.Next() {
		var outID, turnID string
		if err := rows.Scan(&outID, &turnID); err != nil {
			rows.Close()
			return nil, err
		}
		for _, id := range byOut[outID] {
			turnOf[id] = turnID
		}
		if !seenTurn[turnID] {
			seenTurn[turnID] = true
			turnIDs = append(turnIDs, turnID)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(turnIDs) == 0 {
		return out, nil
	}
	// The messages each turn wrote, in the order it wrote them: the answer revisions in turn, then the position inside one.
	order := map[string][]string{} // turn id -> outbox ids
	rows, err = s.db.QueryContext(ctx, `SELECT turn_id,id FROM outbox WHERE state='committed' AND turn_id IN (`+marks(len(turnIDs))+`) ORDER BY turn_id,answer_rev,idx`, turnIDs...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var turnID, outID string
		if err := rows.Scan(&turnID, &outID); err != nil {
			rows.Close()
			return nil, err
		}
		order[turnID] = append(order[turnID], outID)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	totals := map[string]MessageUsage{}
	rows, err = s.db.QueryContext(ctx, `SELECT turn_id,SUM(tokens_in),SUM(tokens_out),SUM(tokens_cache_read),SUM(tokens_cache_write),SUM(cost_est),MAX(partial),COALESCE(MAX(model),'')
		FROM usage_record WHERE turn_id IN (`+marks(len(turnIDs))+`) GROUP BY turn_id`, turnIDs...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var turnID, model string
		var in, outTokens, read, write sql.NullInt64
		var cost sql.NullFloat64
		var partial int
		if err := rows.Scan(&turnID, &in, &outTokens, &read, &write, &cost, &partial, &model); err != nil {
			rows.Close()
			return nil, err
		}
		usage := MessageUsage{TurnID: turnID, Model: model, Partial: partial == 1}
		if in.Valid {
			usage.TokensIn = &in.Int64
		}
		if outTokens.Valid {
			usage.TokensOut = &outTokens.Int64
		}
		if read.Valid {
			usage.CacheRead = &read.Int64
		}
		if write.Valid {
			usage.CacheWrite = &write.Int64
		}
		if cost.Valid {
			usage.Cost = &cost.Float64
		}
		totals[turnID] = usage
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	position := map[string]int{} // outbox id -> 1-based place in its turn
	for _, ids := range order {
		for i, id := range ids {
			position[id] = i + 1
		}
	}
	for _, message := range messages {
		turnID, ok := turnOf[message.ID]
		usage, reported := totals[turnID]
		if !ok || !reported {
			continue
		}
		usage.Count = max(len(order[turnID]), 1)
		usage.Index = max(position[message.SourceOutID], 1)
		out[message.ID] = usage
	}
	return out, nil
}

func marks(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }
