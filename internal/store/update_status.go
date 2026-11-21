package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// UpdateStatusAck is what one person answered about the last update-status
// notice they saw: skip (hide this version and state for good) or remind
// (hide it until the named time, 24 hours after the ask).
type UpdateStatusAck struct {
	UserID  string
	Version string
	State   string
	Kind    string
	Until   string
}

// UpdateStatusAck reads one person's ack, or false when they have not answered.
func (s *Store) UpdateStatusAck(ctx context.Context, userID string) (UpdateStatusAck, bool, error) {
	err := s.checkTxReentry(ctx)
	if err != nil {
		return UpdateStatusAck{}, false, err
	}
	var ack UpdateStatusAck
	err = s.db.QueryRowContext(ctx, `SELECT user_id, version, state, kind, until FROM update_status_ack WHERE user_id=?`, userID).
		Scan(&ack.UserID, &ack.Version, &ack.State, &ack.Kind, &ack.Until)
	if errors.Is(err, sql.ErrNoRows) {
		return UpdateStatusAck{}, false, nil
	}
	return ack, err == nil, err
}

// SetUpdateStatusAck records (or replaces) one person's answer.
func (s *Store) SetUpdateStatusAck(ctx context.Context, ack UpdateStatusAck) error {
	return s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.Exec(`INSERT INTO update_status_ack (user_id, version, state, kind, until) VALUES (?,?,?,?,?)
			ON CONFLICT(user_id) DO UPDATE SET version=excluded.version, state=excluded.state, kind=excluded.kind, until=excluded.until`,
			ack.UserID, ack.Version, ack.State, ack.Kind, ack.Until)
		return err
	})
}

// UntilIn24h is the remind deadline: 24 hours from now, in the store's timestamp layout.
func UntilIn24h() string { return time.Now().UTC().Add(24 * time.Hour).Format(timestampLayout) }

// TimestampString formats a time the way the store writes timestamps.
func TimestampString(t time.Time) string { return t.UTC().Format(timestampLayout) }

// RecordUpdateStatusEvent stores an update_status system event, which only the
// owner and admins see in the system feed. The payload carries the state and
// the version, never anything else about the install.
func (s *Store) RecordUpdateStatusEvent(ctx context.Context, state, version string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.AppendEvent(Event{Kind: "update_status", PayloadJSON: payloadJSON(map[string]string{"state": state, "version": version})})
		return err
	})
}
