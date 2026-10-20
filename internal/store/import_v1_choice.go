package store

import (
	"context"
	"database/sql"
	"errors"
)

// ErrV1InstallUsed is returned by ImportV1 on an install that already has messages and never
// started a v1 import: history is imported once, into a fresh install.
var ErrV1InstallUsed = errors.New("this install already has messages; v1 history can only be imported once, into a fresh install")

// V1ImportChoice is empty until the first-run wizard is completed or skipped.
func (s *Store) V1ImportChoice(ctx context.Context) (string, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return "", err
	}
	var state string
	err := s.db.QueryRowContext(ctx, `SELECT state FROM import_v1_choice WHERE id=1`).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return state, err
}

func (s *Store) SetV1ImportChoice(ctx context.Context, state string) error {
	if state != "skipped" && state != "done" {
		return ErrInvalidTransition
	}
	return s.Tx(ctx, func(tx *Tx) error {
		var prior string
		err := tx.tx.QueryRowContext(ctx, `SELECT state FROM import_v1_choice WHERE id=1`).Scan(&prior)
		if err == nil {
			if prior == state {
				return nil
			}
			return ErrConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.Exec(`INSERT INTO import_v1_choice(id,state,decided_at) VALUES (1,?,?)`, state, timestamp())
		return err
	})
}

// HasMessages says whether anyone has written a message. System notices do not count: they are
// the install's own bookkeeping, not conversation. v1 history is imported once, into a fresh
// install; once the install has real messages the offer is closed for good.
func (s *Store) HasMessages(ctx context.Context) (bool, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return false, err
	}
	var any bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM message WHERE author_kind<>'system')`).Scan(&any)
	return any, err
}

// V1ImportUsed says whether this install is past its only chance to import: it has messages and
// no v1 import has ever started here (a resumable import has ledger rows and stays open).
func (s *Store) V1ImportUsed(ctx context.Context) (bool, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return false, err
	}
	var used bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM message WHERE author_kind<>'system')
		AND NOT EXISTS(SELECT 1 FROM legacy_import_source)`).Scan(&used)
	return used, err
}

// V1ReservationMissed says whether an event already took a sequence number at or below the
// highest v1 message number, which means the first-run reservation did not happen and the
// import cannot keep the v1 numbers.
func (s *Store) V1ReservationMissed(ctx context.Context, highest int64) (bool, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return false, err
	}
	if highest <= 0 {
		return false, nil
	}
	var missed bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM event WHERE seq<=?)`, highest).Scan(&missed)
	return missed, err
}

// V1ImportStarted says whether any v1 import has written rows here, so a skip now ends a
// partly finished import ("done") rather than declining one that never began ("skipped").
func (s *Store) V1ImportStarted(ctx context.Context) (bool, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return false, err
	}
	var started bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM legacy_import_source)`).Scan(&started)
	return started, err
}
