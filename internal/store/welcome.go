package store

import (
	"context"
	"database/sql"
	"errors"
)

// WelcomeSeenKey names the setting that says the owner has been through the first-run welcome.
const WelcomeSeenKey = "welcome_seen"

// WelcomeSeen reports whether the first-run welcome has been dismissed (or never applied: see the migration that seeds it).
func (s *Store) WelcomeSeen(ctx context.Context) (bool, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return false, err
	}
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM global_setting WHERE key=?`, WelcomeSeenKey).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return value == "1", err
}

// MarkWelcomeSeen records that the welcome has been dismissed; it is idempotent.
func (s *Store) MarkWelcomeSeen(ctx context.Context) error {
	if err := s.checkTxReentry(ctx); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO global_setting(key,value,updated_at) VALUES (?,?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, WelcomeSeenKey, "1", timestamp())
	return err
}
