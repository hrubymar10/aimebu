package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// Global settings are small server-wide values the owner can change. Each one has
// a typed accessor below; the table itself is a key/value store.

const (
	// MessageExpiryKey names the message-expiry window setting.
	MessageExpiryKey = "messages_considered_expired_after_seconds"
	// DefaultMessageExpirySeconds is six hours, the v1 default: a fresh install has
	// expiry on.
	DefaultMessageExpirySeconds = 21600
	// MinMessageExpirySeconds and MaxMessageExpirySeconds bound a non-zero window;
	// zero means messages never expire.
	MinMessageExpirySeconds = 60
	MaxMessageExpirySeconds = 2592000
)

// ErrInvalidMessageExpiry is returned for a window outside 0 or [60, 2592000].
var ErrInvalidMessageExpiry = fmt.Errorf("%s must be 0 (never) or between %d and %d seconds", MessageExpiryKey, MinMessageExpirySeconds, MaxMessageExpirySeconds)

// ValidMessageExpirySeconds reports whether n is an allowed window.
func ValidMessageExpirySeconds(n int) bool {
	return n == 0 || (n >= MinMessageExpirySeconds && n <= MaxMessageExpirySeconds)
}

// MessageExpirySeconds is how old a message must be before it counts as expired
// for AI context: now - created_at >= the window. Zero means never. Expiry is
// computed when a message is looked at and nothing is stored on the message, so
// changing the window re-scopes every room at once, and it never deletes anything.
func (s *Store) MessageExpirySeconds(ctx context.Context) (int, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return 0, err
	}
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM global_setting WHERE key=?`, MessageExpiryKey).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultMessageExpirySeconds, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(value)
	if err != nil || !ValidMessageExpirySeconds(n) {
		// A hand-edited or corrupt value must not turn expiry off or hide history:
		// fall back to the default and let the next save replace it.
		return DefaultMessageExpirySeconds, nil
	}
	return n, nil
}

// SetMessageExpirySeconds saves the window after validating it.
func (s *Store) SetMessageExpirySeconds(ctx context.Context, n int) error {
	if !ValidMessageExpirySeconds(n) {
		return ErrInvalidMessageExpiry
	}
	if err := s.checkTxReentry(ctx); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO global_setting(key,value,updated_at) VALUES (?,?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, MessageExpiryKey, strconv.Itoa(n), timestamp())
	return err
}
