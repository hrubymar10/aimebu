package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// Open seeds the team-flow busy cache with one read, so a crash that left a
// change or an interrupted agent waiting is served by the first passes, and an
// idle server (nothing unsettled) skips the step from the start.
func TestFlowMigrationBusySeededOnOpen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "aimebu.sqlite")
	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if s.TeamFlowMigrationBusy() {
		t.Fatal("a fresh store says a team-flow change is under way")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// A project row the migration row needs.
	if _, err := db.Exec(`INSERT INTO project(id,name,folder,created_at,team_flow_key,team_flow_review_pending) VALUES ('prj_busy','p','/tmp/busy','2026-11-24T00:00:00.000000000Z','',0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO team_flow_migration(id,project_id,actor_id,from_key,to_key,flow_rev,state,input_json,created_at)
		VALUES ('tfm_busy','prj_busy','usr_x','leader-workers','leader-dispatcher',1,'draining','{}','2026-11-24T00:00:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.TeamFlowMigrationBusy() {
		t.Fatal("a store with a draining change on record says none is under way")
	}
	// The change is over (nothing left draining or applying, no interrupted
	// agent), so the next pass settles the cache: the advance re-reads the
	// same query it serves and finds nothing left.
	if _, err := s.db.Exec(`UPDATE team_flow_migration SET state='done',finished_at='2026-11-24T00:00:00.000000000Z' WHERE id='tfm_busy'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceTeamFlowMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	if s.TeamFlowMigrationBusy() {
		t.Fatal("the advance found nothing left, but the store still says a change is under way")
	}
	if runs := s.FlowMigrationAdvanceRuns(); runs != 1 {
		t.Fatalf("advance ran %d times, want 1", runs)
	}
}

// Starting a change marks the step busy, so the passes a change needs are not
// skipped by the cache that lets an idle server skip the step.
func TestFlowMigrationBusyTracksAChange(t *testing.T) {
	f, setup := flowMigrationFixture(t, "leader-dispatcher")
	ctx := t.Context()
	if f.s.TeamFlowMigrationBusy() {
		t.Fatal("no change has started, but the store says one is under way")
	}
	toA := TeamFlowMigrationInput{FlowKey: "leader-workers", FlowRev: setup.Flow.Rev}
	if err := f.s.Tx(ctx, func(tx *Tx) error { _, err := tx.BeginTeamFlowMigration(f.project.ID, f.alice, toA); return err }); err != nil {
		t.Fatal(err)
	}
	if !f.s.TeamFlowMigrationBusy() {
		t.Fatal("a change started, but the store says none is under way")
	}
	if _, err := f.s.AdvanceTeamFlowMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	// The drain is under way: agents must stop and start again, so the step
	// stays busy and the passes keep serving it.
	if !f.s.TeamFlowMigrationBusy() {
		t.Fatal("the drain is still under way, but the store says it is done")
	}
}
