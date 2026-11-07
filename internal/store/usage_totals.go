package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// UsageGroup is one line of a breakdown (an agent, a model): what its turns spent. A figure nobody reported is nil, never 0.
type UsageGroup struct {
	Key        string   `json:"key"`
	Label      string   `json:"label"`
	Turns      int      `json:"turns"`
	TokensIn   *int64   `json:"tokens_in,omitempty"`
	TokensOut  *int64   `json:"tokens_out,omitempty"`
	CacheRead  *int64   `json:"tokens_cache_read,omitempty"`
	CacheWrite *int64   `json:"tokens_cache_write,omitempty"`
	Cost       *float64 `json:"cost,omitempty"`
	// CostEstimated says at least one priced turn in this group used the price list.
	CostEstimated bool `json:"cost_estimated,omitempty"`
}

// UsageTotals is what a room's or a project's turns spent over a range. It sums usage_record, which holds each turn once, so a turn that wrote
// several messages is counted once; it never adds up the per-message figures. CostCoverage is "known" only if every turn's cost came
// from a harness, "estimated" if every turn has a cost and some use the price list, "partial" if some lack a cost, else "unknown".
type UsageTotals struct {
	Scope                  string       `json:"scope"`
	ScopeID                string       `json:"scope_id"`
	Range                  string       `json:"range"`
	Since                  string       `json:"since,omitempty"`
	Turns                  int          `json:"turns"`
	TurnsWithCost          int          `json:"turns_with_cost"`
	TurnsWithEstimatedCost int          `json:"turns_with_estimated_cost"`
	CostCoverage           string       `json:"cost_coverage"`
	TokensIn               *int64       `json:"tokens_in,omitempty"`
	TokensOut              *int64       `json:"tokens_out,omitempty"`
	CacheRead              *int64       `json:"tokens_cache_read,omitempty"`
	CacheWrite             *int64       `json:"tokens_cache_write,omitempty"`
	Cost                   *float64     `json:"cost,omitempty"`
	Partial                bool         `json:"partial,omitempty"`
	ByAgent                []UsageGroup `json:"by_agent"`
	ByModel                []UsageGroup `json:"by_model"`
}

// UsageRanges are the ranges a client may ask for.
var UsageRanges = map[string]bool{"today": true, "7d": true, "all": true}

func usageSince(rng string, now time.Time) string {
	switch rng {
	case "today":
		start, _, _ := localUsageDay(now)
		return start.UTC().Format(timestampLayout)
	case "7d":
		return now.UTC().Add(-7 * 24 * time.Hour).Format(timestampLayout)
	}
	return ""
}

// usageScopeColumn is the usage_record column a scope is read by; both are indexed with the time.
func usageScopeColumn(scope string) (string, error) {
	switch scope {
	case "room":
		return "room_id", nil
	case "project":
		return "project_id", nil
	}
	return "", fmt.Errorf("unknown usage scope %q", scope)
}

type usageSums struct {
	in, out, read, write sql.NullInt64
	cost                 sql.NullFloat64
}

func (u usageSums) groupFields(g *UsageGroup) {
	if u.in.Valid {
		g.TokensIn = &u.in.Int64
	}
	if u.out.Valid {
		g.TokensOut = &u.out.Int64
	}
	if u.read.Valid {
		g.CacheRead = &u.read.Int64
	}
	if u.write.Valid {
		g.CacheWrite = &u.write.Int64
	}
	if u.cost.Valid {
		g.Cost = &u.cost.Float64
	}
}

// UsageTotalsFor sums a room's or a project's usage over a range ("today", "7d" or "all").
func (s *Store) UsageTotalsFor(ctx context.Context, scope, scopeID, rng string) (UsageTotals, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return UsageTotals{}, err
	}
	column, err := usageScopeColumn(scope)
	if err != nil {
		return UsageTotals{}, err
	}
	if !UsageRanges[rng] {
		return UsageTotals{}, fmt.Errorf("unknown usage range %q", rng)
	}
	out := UsageTotals{Scope: scope, ScopeID: scopeID, Range: rng, Since: usageSince(rng, time.Now()), ByAgent: []UsageGroup{}, ByModel: []UsageGroup{}}
	where, args := `u.`+column+`=?`, []any{scopeID}
	if out.Since != "" {
		where += ` AND u.created_at>=?`
		args = append(args, out.Since)
	}
	var total usageSums
	var partial int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT u.turn_id),SUM(u.tokens_in),SUM(u.tokens_out),SUM(u.tokens_cache_read),SUM(u.tokens_cache_write),SUM(u.cost_est),COALESCE(MAX(u.partial),0)
		FROM usage_record u WHERE `+where, args...).Scan(&out.Turns, &total.in, &total.out, &total.read, &total.write, &total.cost, &partial); err != nil {
		return UsageTotals{}, err
	}
	var group UsageGroup
	total.groupFields(&group)
	out.TokensIn, out.TokensOut, out.CacheRead, out.CacheWrite, out.Cost, out.Partial = group.TokensIn, group.TokensOut, group.CacheRead, group.CacheWrite, group.Cost, partial == 1
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT u.turn_id) FROM usage_record u WHERE `+where+` AND u.cost_est IS NOT NULL`, args...).Scan(&out.TurnsWithCost); err != nil {
		return UsageTotals{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT u.turn_id) FROM usage_record u WHERE `+where+` AND u.cost_est IS NOT NULL AND u.source='estimated'`, args...).Scan(&out.TurnsWithEstimatedCost); err != nil {
		return UsageTotals{}, err
	}
	switch {
	case out.Turns == 0:
		out.CostCoverage = "unknown"
	case out.TurnsWithCost == out.Turns:
		out.CostCoverage = "known"
		if out.TurnsWithEstimatedCost > 0 {
			out.CostCoverage = "estimated"
		}
	case out.TurnsWithCost == 0:
		out.CostCoverage = "unknown"
	default:
		out.CostCoverage = "partial"
	}
	groups := func(sqlText string) ([]UsageGroup, error) {
		rows, err := s.db.QueryContext(ctx, sqlText, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		list := []UsageGroup{}
		for rows.Next() {
			var g UsageGroup
			var sums usageSums
			var estimated int
			if err := rows.Scan(&g.Key, &g.Label, &g.Turns, &sums.in, &sums.out, &sums.read, &sums.write, &sums.cost, &estimated); err != nil {
				return nil, err
			}
			sums.groupFields(&g)
			g.CostEstimated = estimated == 1
			list = append(list, g)
		}
		return list, rows.Err()
	}
	// The most expensive first; a group with no cost sorts by its tokens, then by name, so the order is stable.
	const order = ` ORDER BY COALESCE(SUM(u.cost_est),0) DESC, COALESCE(SUM(u.tokens_in),0)+COALESCE(SUM(u.tokens_out),0) DESC, 2`
	if out.ByAgent, err = groups(`SELECT u.agent_id,COALESCE(NULLIF(a.removed_slug,''),a.slug,u.agent_id),COUNT(DISTINCT u.turn_id),SUM(u.tokens_in),SUM(u.tokens_out),SUM(u.tokens_cache_read),SUM(u.tokens_cache_write),SUM(u.cost_est),MAX(CASE WHEN u.cost_est IS NOT NULL AND u.source='estimated' THEN 1 ELSE 0 END)
		FROM usage_record u LEFT JOIN agent a ON a.id=u.agent_id WHERE ` + where + ` GROUP BY u.agent_id` + order); err != nil {
		return UsageTotals{}, err
	}
	if out.ByModel, err = groups(`SELECT COALESCE(u.model,''),COALESCE(NULLIF(u.model,''),'unknown'),COUNT(DISTINCT u.turn_id),SUM(u.tokens_in),SUM(u.tokens_out),SUM(u.tokens_cache_read),SUM(u.tokens_cache_write),SUM(u.cost_est),MAX(CASE WHEN u.cost_est IS NOT NULL AND u.source='estimated' THEN 1 ELSE 0 END)
		FROM usage_record u WHERE ` + where + ` GROUP BY COALESCE(u.model,'')` + order); err != nil {
		return UsageTotals{}, err
	}
	return out, nil
}
