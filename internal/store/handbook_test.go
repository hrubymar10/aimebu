package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
)

func TestHandbookRevisionHistoryAndAccess(t *testing.T) {
	t.Parallel()
	s, project, _, worker, lead := wakeFixture(t)
	ctx := context.Background()
	empty, err := s.GetHandbook(ctx, project.ID)
	if err != nil || empty.Rev != 0 || len(empty.Sections) != 5 {
		t.Fatalf("empty: %+v %v", empty, err)
	}
	state := "Working on handbook"
	first, err := s.UpdateHandbook(ctx, project.ID, lead.ID, "current_state", &state, nil, 0)
	if err != nil || first.Rev != 1 || first.UpdatedBy != lead.ID {
		t.Fatalf("first: %+v %v", first, err)
	}
	if _, err := s.UpdateHandbook(ctx, project.ID, worker.ID, "timeline", &state, nil, 1); !errors.Is(err, ErrForbidden) {
		t.Fatalf("worker update: %v", err)
	}
	if _, err := s.UpdateHandbook(ctx, project.ID, lead.ID, "open_backlog", &state, nil, 0); !errors.Is(err, ErrHandbookRevision) {
		t.Fatalf("stale: %v", err)
	}
	patch := "\nNext step"
	second, err := s.UpdateHandbook(ctx, project.ID, lead.ID, "current_state", nil, &patch, 1)
	if err != nil || second.Sections["current_state"] != state+patch {
		t.Fatalf("append: %+v %v", second, err)
	}
	history, err := s.HandbookRevisions(ctx, project.ID, 1, 0)
	if err != nil || len(history.Revisions) != 1 || history.Revisions[0].Rev != 2 || history.NextBeforeRev == nil || *history.NextBeforeRev != 2 {
		t.Fatalf("history: %+v %v", history, err)
	}
	older, err := s.HandbookRevisions(ctx, project.ID, 1, *history.NextBeforeRev)
	if err != nil || len(older.Revisions) != 1 || older.Revisions[0].Rev != 1 || older.NextBeforeRev != nil || !strings.Contains(older.Revisions[0].Diff, "+Working on handbook") {
		t.Fatalf("older: %+v %v", older, err)
	}
	snapshot, err := s.HandbookSnapshot(ctx, project.ID, 1)
	if err != nil || snapshot.Sections["current_state"] != state {
		t.Fatalf("snapshot: %+v %v", snapshot, err)
	}
	if _, err := s.HandbookSnapshot(ctx, project.ID, 3); !errors.Is(err, sql.ErrNoRows) && err == nil {
		t.Fatalf("missing snapshot: %v", err)
	}
	if access, err := s.HandbookAgentAccess(ctx, project.ID, lead.ID); err != nil || access != "full" {
		t.Fatalf("lead access: %q %v", access, err)
	}
	if _, err := s.HandbookAgentAccess(ctx, project.ID, worker.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("worker access: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE project SET team_flow_key='leader-dispatcher' WHERE id=?`, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET role_id='role_dispatcher_default' WHERE id=?`, worker.ID); err != nil {
		t.Fatal(err)
	}
	if access, err := s.HandbookAgentAccess(ctx, project.ID, worker.ID); err != nil || access != "limited" {
		t.Fatalf("dispatcher access: %q %v", access, err)
	}
	if err := s.PruneRuntime(ctx); err != nil {
		t.Fatal(err)
	}
	preserved, err := s.GetHandbook(ctx, project.ID)
	if err != nil || preserved.Rev != 2 {
		t.Fatalf("runtime prune lost handbook: %+v %v", preserved, err)
	}
}

func TestLeadFirstDeliveryIncludesHandbook(t *testing.T) {
	t.Parallel()
	s, project, room, _, lead := wakeFixture(t)
	ctx := context.Background()
	state := "Prior leader's state"
	if _, err := s.UpdateHandbook(ctx, project.ID, lead.ID, "current_state", &state, nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET observed='idle' WHERE id=?`, lead.ID); err != nil {
		t.Fatal(err)
	}
	// A resumed native session can already have a start timestamp copied from
	// its prior generation. Its first turn still needs the handbook.
	if _, err := s.db.Exec(`UPDATE session SET start_mode='native_resume',native_session_started_at=? WHERE agent_id=?`, timestamp(), lead.ID); err != nil {
		t.Fatal(err)
	}
	note, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "resume"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.NoteForAgentMessage(ctx, lead.ID, note.ID); err != nil {
		t.Fatal(err)
	}
	delivery, ready, err := s.PrepareDispatch(ctx, lead.ID, "")
	if err != nil || !ready {
		t.Fatalf("delivery: %t %v", ready, err)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(delivery.Context, &data); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data["project_handbook"]), state) || !strings.Contains(string(data["project_handbook_note"]), "previous leader") {
		t.Fatalf("context: %s", delivery.Context)
	}
}

func TestHandbookRoleMigrationPreservesEditedLeader(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	if _, err := s.db.Exec(`UPDATE role SET body=?,rev=1 WHERE id='role_leader_default'`, priorHandbookLeadRoleBody); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(roleHandbookMigration()); err != nil {
		t.Fatal(err)
	}
	var body string
	if err := s.db.QueryRow(`SELECT body FROM role WHERE id='role_leader_default'`).Scan(&body); err != nil || body != leadRoleBody {
		t.Fatalf("unedited role: %q %v", body, err)
	}
	if _, err := s.db.Exec(`UPDATE role SET body='owner edit',rev=2 WHERE id='role_leader_default'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(roleHandbookMigration()); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT body FROM role WHERE id='role_leader_default'`).Scan(&body); err != nil || body != "owner edit" {
		t.Fatalf("edited role: %q %v", body, err)
	}
}

func TestHandbookSoftLineBudget(t *testing.T) {
	t.Parallel()
	s, project, _, _, lead := wakeFixture(t)
	timeline := strings.Repeat("older entry\n", 1002)
	doc, err := s.UpdateHandbook(context.Background(), project.ID, lead.ID, "timeline", &timeline, nil, 0)
	if err != nil || !doc.OverBudget || doc.LineCount <= 1000 || !strings.Contains(doc.Nudge, "compress") {
		t.Fatalf("soft budget: %+v %v", doc, err)
	}
}

func TestHandbookWarningThresholds(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	makeDoc := func(stamp string, age time.Duration, lines int) Handbook {
		backlog := ""
		if lines > 0 {
			backlog = strings.Repeat("item\n", lines)
		}
		return Handbook{Sections: map[string]string{"current_state": stamp, "open_backlog": backlog}, UpdatedAt: now.Add(-age).Format(time.RFC3339Nano)}
	}
	for _, tc := range []struct {
		name                   string
		doc                    Handbook
		wantState, wantBacklog bool
	}{
		{"below both", makeDoc("Updated 2026-11-27", 47*time.Hour, 60), false, false},
		{"at age threshold", makeDoc("no stamp", 48*time.Hour, 60), false, false},
		{"stale stamp", makeDoc("Updated 2026-11-25", time.Hour, 60), true, false},
		{"invalid stamp uses timestamp", makeDoc("Updated 2026-13-40", 49*time.Hour, 60), true, false},
		{"fallback timestamp", makeDoc("no stamp", 49*time.Hour, 60), true, false},
		{"over backlog", makeDoc("Updated 2026-11-27", time.Hour, 61), false, true},
		{"both", makeDoc("Updated 2026-11-25", time.Hour, 61), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warning := handbookWarning(tc.doc, now)
			if strings.Contains(warning, "Current state was last updated") != tc.wantState || strings.Contains(warning, "Open backlog has") != tc.wantBacklog {
				t.Fatalf("warning %q", warning)
			}
		})
	}
}

func TestLeadTurnGetsOneHandbookWarning(t *testing.T) {
	t.Parallel()
	s, project, room, _, lead := wakeFixture(t)
	ctx := context.Background()
	state := "Updated 2020-01-01\nNeeds review"
	if _, err := s.UpdateHandbook(ctx, project.ID, lead.ID, "current_state", &state, nil, 0); err != nil {
		t.Fatal(err)
	}
	backlog := strings.Repeat("open\n", 61)
	if _, err := s.UpdateHandbook(ctx, project.ID, lead.ID, "open_backlog", &backlog, nil, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET observed='idle' WHERE id=?`, lead.ID); err != nil {
		t.Fatal(err)
	}
	note, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "continue"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.NoteForAgentMessage(ctx, lead.ID, note.ID); err != nil {
		t.Fatal(err)
	}
	delivery, ready, err := s.PrepareDispatch(ctx, lead.ID, "")
	if err != nil || !ready {
		t.Fatalf("delivery %v %v", ready, err)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(delivery.Context, &data); err != nil {
		t.Fatal(err)
	}
	var warning string
	if err := json.Unmarshal(data["project_handbook_warning"], &warning); err != nil {
		t.Fatal(err)
	}
	if strings.Count(warning, "Current state was last updated") != 1 || strings.Count(warning, "Open backlog has 61 lines") != 1 {
		t.Fatalf("warning %q", warning)
	}
}
