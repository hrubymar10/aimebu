package store

import (
	"context"
	"database/sql"
	"errors"
)

// A tool hold is the limit guard's verdict on the host machine's account: while one is set for a tool, no new
// turn is dispatched to an agent of that tool on the host machine (PrepareDispatch); agents on other machines,
// which have their own logins, are not held, and a running turn is never touched. It lives in
// memory only. It is derived from the usage snapshot and re-derived within seconds of a
// restart, so a stale hold can never outlive the process that set it.

// SetToolHold holds new turns of tool with reason; an empty reason releases the hold.
func (s *Store) SetToolHold(tool, reason string) {
	s.holdMu.Lock()
	defer s.holdMu.Unlock()
	if reason == "" {
		delete(s.holds, tool)
		return
	}
	if s.holds == nil {
		s.holds = map[string]string{}
	}
	s.holds[tool] = reason
}

// ToolHold returns the reason tool is held, or "".
func (s *Store) ToolHold(tool string) string {
	s.holdMu.RLock()
	defer s.holdMu.RUnlock()
	return s.holds[tool]
}

// GlobalSettingValue reads a free-form global setting.
func (s *Store) GlobalSettingValue(ctx context.Context, key string) (string, bool, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return "", false, err
	}
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM global_setting WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return value, err == nil, err
}

// SetGlobalSettingValue writes a free-form global setting.
func (s *Store) SetGlobalSettingValue(ctx context.Context, key, value string) error {
	if err := s.checkTxReentry(ctx); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO global_setting(key,value,updated_at) VALUES (?,?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, key, value, timestamp())
	return err
}
