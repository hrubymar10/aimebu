package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestSharedMacrosLimitsAndAtomicReplacement(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	if err := s.ReplaceMacros(ctx, map[string]string{"greet": "Hello", "empty": ""}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []map[string]string{{"Upper": "x"}, {"x": strings.Repeat("界", 5462)}} {
		if err := s.ReplaceMacros(ctx, bad); err == nil {
			t.Fatalf("accepted invalid map: %+v", bad)
		}
	}
	tooMany := map[string]string{}
	for i := 0; i <= macroCountMax; i++ {
		tooMany[fmt.Sprintf("key%d", i)] = "x"
	}
	if err := s.ReplaceMacros(ctx, tooMany); err == nil {
		t.Fatal("accepted 257 macros")
	}
	macros, err := s.ListMacros(ctx)
	if err != nil || macros["greet"] != "Hello" || len(macros) != 2 {
		t.Fatalf("replacement was not atomic: %+v %v", macros, err)
	}
	if err := s.ReplaceMacros(ctx, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	macros, err = s.ListMacros(ctx)
	if err != nil || macros == nil || len(macros) != 0 {
		t.Fatalf("empty map: %+v %v", macros, err)
	}
}

func TestDefaultMacrosSeededOnceAndNeverOverwritten(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, path := testStore(t, Options{})
	macros, err := s.ListMacros(ctx)
	if err != nil || macros["do-cr"] == "" || macros["example_macro"] == "" {
		t.Fatalf("fresh install defaults missing: %v %v", len(macros), err)
	}
	// Edit one default and delete the other: neither may come back on reopen.
	if err := s.ReplaceMacros(ctx, map[string]string{"do-cr": "mine"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	macros, _ = s.ListMacros(ctx)
	if len(macros) != 1 || macros["do-cr"] != "mine" {
		t.Fatalf("defaults re-seeded or overwritten: %+v", macros)
	}
}

// reseedMacros re-runs the migration's seeding step in its own transaction.
func reseedMacros(t *testing.T, s *Store) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := seedDefaultMacros(context.Background(), tx); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultMacrosKeepExistingKeyBeforeFirstSeed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := testStore(t, Options{})
	if _, err := s.db.ExecContext(ctx, `DELETE FROM macro; DELETE FROM macro_default_seen; INSERT INTO macro(key,body) VALUES ('do-cr','imported')`); err != nil {
		t.Fatal(err)
	}
	reseedMacros(t, s)
	macros, _ := s.ListMacros(ctx)
	if macros["do-cr"] != "imported" || macros["example_macro"] == "" {
		t.Fatalf("existing key overwritten or sibling default missing: %v", len(macros))
	}
}

func TestDefaultMacrosSkipKeysFromImportLedger(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := testStore(t, Options{})
	// Simulate an upgrade: the install imported v1's do-cr, deleted it, and
	// only now gets the seen table.
	for _, statement := range []string{
		`DELETE FROM macro`,
		`DELETE FROM macro_default_seen`,
		`INSERT INTO legacy_import_source(source_key,map_digest,created_at,updated_at) VALUES ('src','d','now','now')`,
		`INSERT INTO legacy_import_map(source_key,entity_kind,old_id,new_id) VALUES ('src','macro','do-cr','do-cr')`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	reseedMacros(t, s)
	macros, err := s.ListMacros(ctx)
	if err != nil || macros["do-cr"] != "" || macros["example_macro"] == "" {
		t.Fatalf("ledger key resurrected or other default missing: %v %v", macros, err)
	}
	var seen int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM macro_default_seen`).Scan(&seen); err != nil || seen != 2 {
		t.Fatalf("seen=%d err=%v", seen, err)
	}
}

func TestOpeningMigratedStoreDoesNotSeedOrWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, path := testStore(t, Options{})
	if _, err := s.db.Exec(`DELETE FROM macro; DELETE FROM macro_default_seen`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if macros, err := s.ListMacros(ctx); err != nil || len(macros) != 0 {
		t.Fatalf("plain open seeded macros: %v %v", macros, err)
	}
}

func TestReplacedMacrosSurviveReopen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, path := testStore(t, Options{})
	want := map[string]string{"greet": "Hello", "bye": "Goodbye"}
	if err := s.ReplaceMacros(ctx, want); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	got, err := s.ListMacros(ctx)
	if err != nil || len(got) != len(want) || got["greet"] != "Hello" || got["bye"] != "Goodbye" {
		t.Fatalf("saved macros lost across reopen: %v %v", got, err)
	}
}
