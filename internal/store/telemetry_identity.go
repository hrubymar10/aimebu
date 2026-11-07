package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
)

// TelemetryIdentity is how a person's error reports name them: their random fingerprint, or their login when they chose to send it.
type TelemetryIdentity struct {
	Fingerprint string
	SendName    bool
	Login       string
}

// ReportID is the one id a report carries: the login when the person chose to send it, otherwise the fingerprint. Never both.
func (i TelemetryIdentity) ReportID() string {
	if i.SendName {
		return i.Login
	}
	return i.Fingerprint
}

func newFingerprint() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil // 16 hex characters, from crypto/rand, unrelated to the user id
}

// UserTelemetry reads a person's telemetry identity, generating the fingerprint the first time it is needed (existing accounts have none).
func (s *Store) UserTelemetry(ctx context.Context, userID string) (TelemetryIdentity, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return TelemetryIdentity{}, err
	}
	read := func() (TelemetryIdentity, error) {
		var id TelemetryIdentity
		var send int
		err := s.db.QueryRowContext(ctx, `SELECT login, telemetry_fingerprint, telemetry_send_name FROM user WHERE id=?`, userID).Scan(&id.Login, &id.Fingerprint, &send)
		id.SendName = send == 1
		return id, err
	}
	id, err := read()
	if err != nil || id.Fingerprint != "" {
		return id, err
	}
	fresh, err := newFingerprint()
	if err != nil {
		return id, err
	}
	// Only while still empty, so two first reads cannot hand out two fingerprints.
	if _, err := s.db.ExecContext(ctx, `UPDATE user SET telemetry_fingerprint=? WHERE id=? AND telemetry_fingerprint=''`, fresh, userID); err != nil {
		return id, err
	}
	return read()
}

// SetTelemetrySendName chooses whether this person's reports carry their login instead of the fingerprint.
func (s *Store) SetTelemetrySendName(ctx context.Context, userID string, send bool) error {
	if err := s.checkTxReentry(ctx); err != nil {
		return err
	}
	value := 0
	if send {
		value = 1
	}
	res, err := s.db.ExecContext(ctx, `UPDATE user SET telemetry_send_name=? WHERE id=?`, value, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// RollTelemetryFingerprint replaces the fingerprint with a fresh random one and turns sending the login off: the new id is an anonymous one.
func (s *Store) RollTelemetryFingerprint(ctx context.Context, userID string) (TelemetryIdentity, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return TelemetryIdentity{}, err
	}
	fresh, err := newFingerprint()
	if err != nil {
		return TelemetryIdentity{}, err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE user SET telemetry_fingerprint=?, telemetry_send_name=0 WHERE id=?`, fresh, userID)
	if err != nil {
		return TelemetryIdentity{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return TelemetryIdentity{}, errors.Join(sql.ErrNoRows)
	}
	return s.UserTelemetry(ctx, userID)
}
