package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"sort"

	"github.com/goccy/go-json"
)

//go:embed defaults_macros.json
var defaultMacrosJSON []byte

const (
	macroCountMax = 256
	macroBodyMax  = 16 * 1024
)

var macroKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// ValidateMacros preserves the v1 key and byte limits. Values may be empty.
func ValidateMacros(macros map[string]string) error {
	if len(macros) > macroCountMax {
		return fmt.Errorf("at most %d macros are allowed", macroCountMax)
	}
	for key, body := range macros {
		if !macroKeyPattern.MatchString(key) {
			return fmt.Errorf("invalid macro key %q", key)
		}
		if len(body) > macroBodyMax {
			return fmt.Errorf("macro %q exceeds %d bytes", key, macroBodyMax)
		}
	}
	return nil
}

func listMacrosTx(ctx context.Context, tx *sql.Tx) (map[string]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT key,body FROM macro ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	macros := map[string]string{}
	for rows.Next() {
		var key, body string
		if err := rows.Scan(&key, &body); err != nil {
			return nil, err
		}
		macros[key] = body
	}
	return macros, rows.Err()
}

func (s *Store) ListMacros(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key,body FROM macro ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	macros := map[string]string{}
	for rows.Next() {
		var key, body string
		if err := rows.Scan(&key, &body); err != nil {
			return nil, err
		}
		macros[key] = body
	}
	return macros, rows.Err()
}

// ReplaceMacros applies the complete shared map atomically.
func (s *Store) ReplaceMacros(ctx context.Context, macros map[string]string) error {
	if err := ValidateMacros(macros); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM macro`); err != nil {
		return err
	}
	keys := make([]string, 0, len(macros))
	for key := range macros {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, err := tx.ExecContext(ctx, `INSERT INTO macro(key,body) VALUES (?,?)`, key, macros[key]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

var ErrMacroConflictChoice = errors.New("invalid macro conflict choice")

func validMacroChoice(choice string) bool {
	return choice == "keep_v2" || choice == "use_v1"
}

// seedDefaultMacros adds each shipped default macro once per key. It runs
// inside schema migration 42, never on a plain store open, so auxiliary tools
// that open the database next to a running server do not write (and contend
// for the lock) at startup. The seen table is write-once, as in v1: a default
// the user later edits or deletes is never re-seeded or overwritten, and a key
// that already exists (created or imported by the user) keeps its body. A
// default is only skipped for lack of room when the map is at the macro limit;
// it is still marked seen. A default whose key the v1 import ledger already
// records is marked seen without being inserted, so a macro imported and later
// deleted is not resurrected on an install that predates the seen table.
func seedDefaultMacros(ctx context.Context, tx *sql.Tx) error {
	var defaults map[string]string
	if err := json.Unmarshal(defaultMacrosJSON, &defaults); err != nil {
		return err
	}
	keys := make([]string, 0, len(defaults))
	for key := range defaults {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM macro`).Scan(&count); err != nil {
		return err
	}
	for _, key := range keys {
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO macro_default_seen(key) VALUES (?)`, key)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 || count >= macroCountMax {
			continue
		}
		// A key the v1 import ledger knows was already part of this install:
		// if it is gone now the owner deleted it, so it stays deleted.
		var imported int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM legacy_import_map WHERE entity_kind='macro' AND old_id=?`, key).Scan(&imported); err != nil {
			return err
		}
		if imported > 0 {
			continue
		}
		res, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO macro(key,body) VALUES (?,?)`, key, defaults[key])
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			count++
		}
	}
	return nil
}
