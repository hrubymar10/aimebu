package store

import (
	"context"
	"database/sql"
	"errors"
)

// The quota switcher keeps its settings, its small persisted state and a bounded
// decision history here. The documents are opaque JSON owned by internal/quota; the
// store only keeps them, per tool, atomically. Plain prune keeps all of it (it is
// settings); prune -a removes the database and so removes it too.

// QuotaDecisionKeep is how many decisions of a tool the history keeps.
const QuotaDecisionKeep = 50

// QuotaConfig returns the saved configuration document of tool, if any.
func (s *Store) QuotaConfig(ctx context.Context, tool string) (string, bool, error) {
	return s.quotaDocument(ctx, `SELECT config_json FROM quota_switcher_config WHERE tool=?`, tool)
}

// SaveQuotaConfig replaces tool's configuration document.
func (s *Store) SaveQuotaConfig(ctx context.Context, tool, document string) error {
	return s.quotaPut(ctx, `INSERT INTO quota_switcher_config(tool,config_json,updated_at) VALUES (?,?,?)
		ON CONFLICT(tool) DO UPDATE SET config_json=excluded.config_json, updated_at=excluded.updated_at`, tool, document)
}

// QuotaState returns the persisted state document of tool, if any.
func (s *Store) QuotaState(ctx context.Context, tool string) (string, bool, error) {
	return s.quotaDocument(ctx, `SELECT state_json FROM quota_switcher_state WHERE tool=?`, tool)
}

// SaveQuotaState replaces tool's state document.
func (s *Store) SaveQuotaState(ctx context.Context, tool, document string) error {
	return s.quotaPut(ctx, `INSERT INTO quota_switcher_state(tool,state_json,updated_at) VALUES (?,?,?)
		ON CONFLICT(tool) DO UPDATE SET state_json=excluded.state_json, updated_at=excluded.updated_at`, tool, document)
}

// AppendQuotaDecision records one decision document and drops the oldest ones
// beyond QuotaDecisionKeep for the tool.
func (s *Store) AppendQuotaDecision(ctx context.Context, tool, at, document string) error {
	if err := s.checkTxReentry(ctx); err != nil {
		return err
	}
	return s.Tx(ctx, func(tx *Tx) error {
		if _, err := tx.Exec(`INSERT INTO quota_switcher_decision(tool,at,decision_json) VALUES (?,?,?)`, tool, at, document); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM quota_switcher_decision WHERE tool=? AND id NOT IN
			(SELECT id FROM quota_switcher_decision WHERE tool=? ORDER BY id DESC LIMIT ?)`, tool, tool, QuotaDecisionKeep)
		return err
	})
}

// QuotaDecisions returns tool's recorded decision documents, newest first.
func (s *Store) QuotaDecisions(ctx context.Context, tool string, limit int) ([]string, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT decision_json FROM quota_switcher_decision WHERE tool=? ORDER BY id DESC LIMIT ?`, tool, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var document string
		if err := rows.Scan(&document); err != nil {
			return nil, err
		}
		out = append(out, document)
	}
	return out, rows.Err()
}

func (s *Store) quotaDocument(ctx context.Context, query, tool string) (string, bool, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return "", false, err
	}
	var document string
	err := s.db.QueryRowContext(ctx, query, tool).Scan(&document)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return document, err == nil, err
}

func (s *Store) quotaPut(ctx context.Context, query, tool, document string) error {
	if err := s.checkTxReentry(ctx); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, query, tool, document, timestamp())
	return err
}

// RecordQuotaEvent records an operator-facing quota event (an automatic switch, a
// reset seen, the breaker tripping) as a project-less system event, which only the
// owner sees. The payload carries tool names and fixed reason sentences, never
// credential material.
func (s *Store) RecordQuotaEvent(ctx context.Context, kind string, payload map[string]string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.AppendEvent(Event{Kind: kind, PayloadJSON: payloadJSON(payload)})
		return err
	})
}
