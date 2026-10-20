package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type HealthFlag struct {
	Code    string `json:"code"`
	Since   string `json:"since,omitempty"`
	ResetAt string `json:"reset_at,omitempty"`
}

// AgentHealth derives current flags from durable turn, inbox, and provider
// evidence. Machine connectivity is owned by the server and added there.
func (s *Store) AgentHealth(ctx context.Context, agentID string, now time.Time, stuckAfter time.Duration) ([]HealthFlag, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	flags := []HealthFlag{}
	var started, lastEvent string
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(t.started_at,''),COALESCE((SELECT MAX(e.created_at) FROM event e
		WHERE e.turn_id=t.id AND e.kind IN ('turn_ack_accepted','supervisor_activity','supervisor_usage','supervisor_compaction')),'')
		FROM turn t WHERE t.agent_id=? AND t.exec='running' ORDER BY t.started_at DESC LIMIT 1`, agentID).Scan(&started, &lastEvent)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		last := started
		if lastEvent > last {
			last = lastEvent
		}
		if at, parseErr := time.Parse(time.RFC3339Nano, last); parseErr == nil && now.Sub(at) > stuckAfter {
			flags = append(flags, HealthFlag{Code: "stuck", Since: last})
		}
	}
	var reset sql.NullString
	var detected string
	err = s.db.QueryRowContext(ctx, `SELECT p.reset_at,p.detected_at FROM provider_limit p JOIN agent a ON a.id=p.agent_id
		WHERE p.agent_id=? AND p.tool=a.tool`, agentID).Scan(&reset, &detected)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil && (!reset.Valid || reset.String > now.UTC().Format("2006-01-02T15:04:05.000000000Z")) {
		flags = append(flags, HealthFlag{Code: "out_of_quota", Since: detected, ResetAt: reset.String})
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT kind FROM inbox_item WHERE agent_id=? AND state='open'
		AND kind IN ('recovery','delivery_failed') ORDER BY kind`, agentID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			rows.Close()
			return nil, err
		}
		if kind == "recovery" {
			flags = append(flags, HealthFlag{Code: "recovery_waiting"})
		} else {
			flags = append(flags, HealthFlag{Code: "held_answer"})
		}
	}
	err = rows.Err()
	rows.Close()
	return flags, err
}
