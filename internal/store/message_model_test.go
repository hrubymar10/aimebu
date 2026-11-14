package store

import (
	"context"
	"testing"
)

func TestMigration110BackfillsOnlyKnownMessageGeneration(t *testing.T) {
	s, _, room, agent, leader := wakeFixture(t)
	ctx := context.Background()
	// CreateAgent begins at generation 1. Recreate the pre-turn generation 0
	// case that migration 110 can backfill without a source outbox entry.
	if _, err := s.db.Exec(`UPDATE agent SET generation=0 WHERE id IN (?,?)`, agent.ID, leader.ID); err != nil {
		t.Fatal(err)
	}
	old, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: agent.ID, Body: "old"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET generation=1,tool='claude',model='haiku' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	current, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: agent.ID, Body: "current"})
	if err != nil {
		t.Fatal(err)
	}
	known, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: leader.ID, Body: "known"})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE message SET author_harness='',author_model='' WHERE id IN ('` + old.ID + `','` + current.ID + `','` + known.ID + `')`,
		`ALTER TABLE message DROP COLUMN switch_handoff_json`,
		`DELETE FROM schema_version WHERE version>=110`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateUpTo(ctx, s.db, 0); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ id, tool, model string }{
		{old.ID, "", ""}, {current.ID, "", ""}, {known.ID, leader.Tool, leader.Model},
	} {
		var tool, model string
		if err := s.db.QueryRow(`SELECT author_harness,author_model FROM message WHERE id=?`, item.id).Scan(&tool, &model); err != nil {
			t.Fatal(err)
		}
		if tool != item.tool || model != item.model {
			t.Fatalf("message %s: %q/%q, want %q/%q", item.id, tool, model, item.tool, item.model)
		}
	}
}

func TestMigration110LeavesMessagesBeforeModelSettlementUnknown(t *testing.T) {
	s, _, room, agent, _ := wakeFixture(t)
	ctx := t.Context()
	if _, err := s.db.Exec(`UPDATE agent SET generation=0 WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	before, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: agent.ID, Body: "before"})
	if err != nil {
		t.Fatal(err)
	}
	after, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: agent.ID, Body: "after"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE message SET created_at='2020-01-01T00:00:00.000000Z',author_harness='',author_model='' WHERE id=?`, before.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE message SET created_at='2030-01-01T00:00:00.000000Z',author_harness='',author_model='' WHERE id=?`, after.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET model='new-default' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO machine_launcher_activity(id,machine_id,actor,action,created_at)
		VALUES ('mla_settled',? ,?,'model_default','2025-01-01T00:00:00.000000Z')`, agent.MachineID, agent.ID); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`ALTER TABLE message DROP COLUMN switch_handoff_json`,
		`DELETE FROM schema_version WHERE version>=110`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateUpTo(ctx, s.db, 0); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ id, harness, model string }{
		{before.ID, "", ""}, {after.ID, agent.Tool, "new-default"},
	} {
		var harness, model string
		if err := s.db.QueryRow(`SELECT author_harness,author_model FROM message WHERE id=?`, item.id).Scan(&harness, &model); err != nil {
			t.Fatal(err)
		}
		if harness != item.harness || model != item.model {
			t.Fatalf("message %s: %q/%q, want %q/%q", item.id, harness, model, item.harness, item.model)
		}
	}
}
