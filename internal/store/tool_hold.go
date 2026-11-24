package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
)

// A tool hold is the limit guard's verdict on the host machine's account: while one is set for a tool, no new
// turn is dispatched to an agent of that tool on the host machine (PrepareDispatch); agents on other machines,
// which have their own logins, are not held, and a running turn is never touched. It lives in
// memory only. It is derived from the usage snapshot and re-derived within seconds of a
// restart, so a stale hold can never outlive the process that set it.

// SetHostLogin gives the store the way to read which login of a tool is live on the host machine now (the switcher's active profile). The store
// records it on each turn admitted on the host and on the provider limit such a turn meets, and counts a limit only against its own login (see
// provider_limit.go). Without it, or when it answers "", every limit counts against whatever login is live, as before logins were recorded.
func (s *Store) SetHostLogin(fn func(tool string) string) {
	s.holdMu.Lock()
	defer s.holdMu.Unlock()
	s.hostLogin = fn
}

// HostLogin is the login of tool live on the host machine now, or "" when it is not known.
func (s *Store) HostLogin(tool string) string {
	s.holdMu.RLock()
	fn := s.hostLogin
	s.holdMu.RUnlock()
	if fn == nil || (tool != "claude" && tool != "codex") {
		return ""
	}
	return fn(tool)
}

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

// HoldDispatch holds new turns of tool on the host machine for an account switch, until the returned release is called. It is a hold of its own, not
// the limit guard's (the guard sets and clears its verdict every few seconds, and a switch must not be undone by it). When it returns, every
// transaction that began before the hold has finished, so a turn that was being dispatched is already recorded as pending or running and a
// quiescence check made after this sees it: from then on nothing can start a turn of the tool, finish it and park an idle warm process on the login that
// is about to be swapped. It must not be called from inside a transaction.
func (s *Store) HoldDispatch(tool string) (release func()) {
	s.holdMu.Lock()
	if s.switchHolds == nil {
		s.switchHolds = map[string]int{}
	}
	s.switchHolds[tool]++
	s.holdMu.Unlock()
	s.txBarrier.Lock() // a barrier: waits for the transactions that are open, and lets the next ones see the hold
	s.txBarrier.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.holdMu.Lock()
			if s.switchHolds[tool]--; s.switchHolds[tool] <= 0 {
				delete(s.switchHolds, tool)
			}
			s.holdMu.Unlock()
		})
	}
}

// DispatchHeldForSwitch says an account switch of the tool holds its new turns (see HoldDispatch).
func (s *Store) DispatchHeldForSwitch(tool string) bool { return s.dispatchHeldForSwitch(tool) }

func (s *Store) dispatchHeldForSwitch(tool string) bool {
	s.holdMu.RLock()
	defer s.holdMu.RUnlock()
	return s.switchHolds[tool] > 0
}

// ToolHold returns the reason tool is held, or "".
func (s *Store) ToolHold(tool string) string {
	s.holdMu.RLock()
	defer s.holdMu.RUnlock()
	return s.holds[tool]
}

// DeleteGlobalSettingValue removes a free-form global setting (none is not an error).
func (s *Store) DeleteGlobalSettingValue(ctx context.Context, key string) error {
	if err := s.checkTxReentry(ctx); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM global_setting WHERE key=?`, key)
	return err
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
