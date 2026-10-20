package store

import (
	"context"
	"testing"
)

// The harness activity list names the person, not their account id; an actor that is not an account (a system step) stays as written.
func TestHarnessActivityNamesTheActor(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	for _, statement := range []string{
		`INSERT INTO machine(id,name,key_hash,approved,is_host) VALUES ('mch_act','host','hash',1,1)`,
		`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_act','tomas','hash','{}','1')`,
		`INSERT INTO machine_launcher_activity(id,machine_id,launcher_id,actor,action,created_at) VALUES ('a1','mch_act','l1','usr_act','added','2026-10-20T10:00:00.000000000Z')`,
		`INSERT INTO machine_launcher_activity(id,machine_id,launcher_id,actor,action,created_at) VALUES ('a2','mch_act','l1','supervisor','synced','2026-10-20T10:01:00.000000000Z')`,
		`INSERT INTO machine_harness_activity(id,machine_id,tool,variant,actor,old_enabled,new_enabled,old_path,new_path,created_at) VALUES ('h1','mch_act','claude','native','usr_act',0,1,'','','2026-10-20T10:00:00.000000000Z')`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	launcher, err := s.LauncherActivity(ctx, "mch_act")
	if err != nil || len(launcher) != 2 || launcher[0].Actor != "supervisor" || launcher[1].Actor != "tomas" {
		t.Fatalf("launcher activity actors = %+v err=%v", launcher, err)
	}
	harness, err := s.HarnessActivity(ctx, "mch_act")
	if err != nil || len(harness) != 1 || harness[0].Actor != "tomas" {
		t.Fatalf("harness activity actors = %+v err=%v", harness, err)
	}
}
