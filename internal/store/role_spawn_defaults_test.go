package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRoleSpawnDefaultsInheritOverrideAndCursor(t *testing.T) {
	t.Parallel()
	s, project, _, _, _ := wakeFixture(t)
	roleReviewUsers(t, s, project.ID)
	ctx := context.Background()
	for _, statement := range []string{
		`INSERT INTO machine_launcher(id,machine_id,tool,name,command,enabled) VALUES ('lnch_a','mch_wake','codex','native','codex',1)`,
		`INSERT INTO harness_model(launcher_id,slug,label,is_default,position) VALUES ('lnch_a','gpt-a','A',1,0)`,
		`INSERT INTO harness_model(launcher_id,slug,label,is_default,position) VALUES ('lnch_a','gpt-b','B',0,1)`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	a := SpawnDefaultChoice{LauncherID: "lnch_a", Model: "gpt-a"}
	b := SpawnDefaultChoice{LauncherID: "lnch_a", Model: "gpt-b"}
	role, err := s.ManageRole(ctx, "usr_owner", RoleDefinition{Key: "planner", Label: "Planner", Body: "plan", Cardinality: "multi", DefaultAgents: []SpawnDefaultChoice{a, b}, PickMode: "in_turn"}, false)
	if err != nil {
		t.Fatal(err)
	}
	global, err := s.RoleSpawnDefaults(ctx, project.ID, role.Key)
	if err != nil || !global.Inherited || len(global.DefaultAgents) != 2 || global.DefaultAgents[0] != a {
		t.Fatalf("global: %+v %v", global, err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error { return tx.AdvanceRoleSpawnCursor(project.ID, role.ID, 0, 1) }); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error { return tx.AdvanceRoleSpawnCursor(project.ID, role.ID, 0, 1) }); !errors.Is(err, ErrRoleSpawnCursorChanged) {
		t.Fatalf("stale cursor: %v", err)
	}
	custom, err := s.SetProjectRoleSpawnDefaults(ctx, project.ID, "usr_operator", role.Key, false, []SpawnDefaultChoice{b}, "randomly")
	if err != nil || custom.Inherited || custom.PickMode != "randomly" || custom.DefaultAgents[0] != b || custom.NextPosition != 1 {
		t.Fatalf("custom: %+v %v", custom, err)
	}
	if _, err := s.SetProjectRoleSpawnDefaults(ctx, project.ID, "usr_operator", role.Key, false, []SpawnDefaultChoice{{LauncherID: "lnch_a", Model: "missing"}}, "in_turn"); !errors.Is(err, ErrInvalidSpawnDefaults) {
		t.Fatalf("unlisted model: %v", err)
	}
	back, err := s.SetProjectRoleSpawnDefaults(ctx, project.ID, "usr_operator", role.Key, true, nil, "")
	if err != nil || !back.Inherited || len(back.DefaultAgents) != 2 {
		t.Fatalf("inherit: %+v %v", back, err)
	}
}

func TestRoleSpawnUnavailableWarningExpires(t *testing.T) {
	t.Parallel()
	s, _, room, _, _ := wakeFixture(t)
	ctx := t.Context()
	for i := 0; i < 2; i++ {
		if err := s.WarnRoleSpawnUnavailable(ctx, room.ID, "worker"); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM message WHERE room_id=? AND body LIKE 'No configured default agent%'`, room.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("warning within one hour: count=%d err=%v", count, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE role_spawn_warning SET last_warned_at=? WHERE room_id=?`,
		time.Now().UTC().Add(-2*time.Hour).Format(timestampLayout), room.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.WarnRoleSpawnUnavailable(ctx, room.ID, "worker"); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM message WHERE room_id=? AND body LIKE 'No configured default agent%'`, room.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("warning after one hour: count=%d err=%v", count, err)
	}
}
