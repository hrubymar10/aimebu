package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestCheckoutPerProjectMigrationPreservesAgents(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open(SQLDriverName, sqliteDSN(filepath.Join(t.TempDir(), "scope.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := migrateUpTo(ctx, db, 101); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO project(id,name,folder,created_at) VALUES ('p1','one','/tmp/shared','2026-01-01T00:00:00Z'),('p2','two','/tmp/shared','2026-01-01T00:00:00Z')`,
		`INSERT INTO machine(id,name,key_hash,approved) VALUES ('m1','host','hash',1)`,
		`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('c1','p1','m1','/tmp/shared')`,
		`INSERT INTO agent(id,slug,project_id,role_id,tool,variant,model,provider,machine_id,checkout_id,key_hash,created_by,created_at)
		 VALUES ('a1','first','p1','role_worker_default','fake','test','test','fake','m1','c1','hash','owner','2026-01-01T00:00:00Z')`,
		`UPDATE checkout SET writer_agent_id='a1',writer_state='reserved' WHERE id='c1'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateUpTo(ctx, db, 103); err != nil {
		t.Fatal(err)
	}
	var foreignKeys int
	if err := db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("foreign keys after migration: %d, %v", foreignKeys, err)
	}
	var checkoutID, writerID, writerState string
	if err := db.QueryRowContext(ctx, `SELECT a.checkout_id,c.writer_agent_id,c.writer_state FROM agent a JOIN checkout c ON c.id=a.checkout_id WHERE a.id='a1'`).Scan(&checkoutID, &writerID, &writerState); err != nil || checkoutID != "c1" || writerID != "a1" || writerState != "reserved" {
		t.Fatalf("checkout after migration: id=%q writer=%q state=%q err=%v", checkoutID, writerID, writerState, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('c2','p2','m1','/tmp/shared')`); err != nil {
		t.Fatalf("second project checkout: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('c3','p1','m1','/tmp/shared')`); err == nil {
		t.Fatal("duplicate checkout within one project was accepted")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO agent(id,slug,project_id,role_id,tool,variant,model,provider,machine_id,checkout_id,key_hash,created_by,created_at)
	 VALUES ('a2','second','p2','role_worker_default','fake','test','test','fake','m1','c2','hash','owner','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := (&Store{db: db}).Tx(ctx, func(tx *Tx) error {
		if err := tx.moveSwitchCheckout("a2", "p2", "m1", "/tmp/shared"); err != nil {
			return err
		}
		_, err := tx.pointAgentAtCheckout("a2", "m1", "/tmp/shared")
		return err
	}); err != nil {
		t.Fatalf("switch or worktree lookup selected the other project's checkout: %v", err)
	}
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() || rows.Err() != nil {
		t.Fatalf("foreign key validation failed: %v", rows.Err())
	}
}
