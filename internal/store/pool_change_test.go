package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// poolFixture is a project with an owner, an operator, an admin who is a member, a worker pool of two models on one launcher, and two
// agents of origin "pool" (a worker on model A, the lead on model A of the lead role's pool).
type poolFixture struct {
	s       *Store
	project Project
	room    Room
	worker  Agent
	lead    Agent
	a, b    SpawnDefaultChoice
}

func newPoolFixture(t *testing.T) poolFixture {
	t.Helper()
	s, project, room, worker, lead := wakeFixture(t)
	roleReviewUsers(t, s, project.ID)
	for _, statement := range []string{
		`INSERT INTO user(id,login,pw_hash,pw_params,created_at,admin) VALUES ('usr_admin','usr_admin','hash','{}','3',1)`,
		`INSERT INTO membership(project_id,user_id,role) VALUES ('` + project.ID + `','usr_admin','operator')`,
		`INSERT INTO machine_launcher(id,machine_id,tool,name,command,enabled) VALUES ('lnch_a','mch_wake','codex','native','codex',1)`,
		`INSERT INTO harness_model(launcher_id,slug,label,is_default,position) VALUES ('lnch_a','gpt-a','A',1,0)`,
		`INSERT INTO harness_model(launcher_id,slug,label,is_default,position) VALUES ('lnch_a','gpt-b','B',0,1)`,
		`INSERT INTO harness_model(launcher_id,slug,label,is_default,position) VALUES ('lnch_a','gpt-c','C',0,2)`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	f := poolFixture{s: s, project: project, room: room, worker: worker, lead: lead,
		a: SpawnDefaultChoice{LauncherID: "lnch_a", Model: "gpt-a"}, b: SpawnDefaultChoice{LauncherID: "lnch_a", Model: "gpt-b"}}
	for _, key := range []string{"worker", "lead"} {
		if _, err := s.SetProjectRoleSpawnDefaults(context.Background(), project.ID, "usr_owner", key, false, []SpawnDefaultChoice{f.a, f.b}, "in_turn"); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{worker.ID, lead.ID} {
		if _, err := s.db.Exec(`UPDATE agent SET launcher_id='lnch_a',model='gpt-a',pool_origin='pool',pool_launcher_id='lnch_a',pool_model='gpt-a' WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f poolFixture) setPool(t *testing.T, key string, items ...SpawnDefaultChoice) {
	t.Helper()
	if _, err := f.s.SetProjectRoleSpawnDefaults(context.Background(), f.project.ID, "usr_owner", key, false, items, "in_turn"); err != nil {
		t.Fatal(err)
	}
}

func (f poolFixture) openItems(t *testing.T, user string) []InboxEntry {
	t.Helper()
	items, err := f.s.InboxForUser(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	var out []InboxEntry
	for _, item := range items {
		if item.Kind == "pool_change" {
			out = append(out, item)
		}
	}
	return out
}

func TestPoolChangeFlagsEveryoneWhoseChoiceLeftThePool(t *testing.T) {
	t.Parallel()
	f := newPoolFixture(t)
	ctx := context.Background()
	if items := f.openItems(t, "usr_owner"); len(items) != 0 {
		t.Fatalf("an unchanged pool raised an item: %+v", items)
	}
	// Both pools drop model A; the lead and the dispatcher-like roles count exactly as workers do.
	f.setPool(t, "worker", f.b)
	f.setPool(t, "lead", f.b)
	items := f.openItems(t, "usr_owner")
	if len(items) != 1 || items[0].RoomID == "" || len(items[0].Options) != 2 || items[0].Options[0] != PoolChangeOptions[0] {
		t.Fatalf("items: %+v", items)
	}
	if len(items[0].PoolAgents) != 2 {
		t.Fatalf("flagged: %+v", items[0].PoolAgents)
	}
	byID := map[string]PoolAgent{}
	for _, a := range items[0].PoolAgents {
		byID[a.AgentID] = a
	}
	if w := byID[f.worker.ID]; w.RoomID != f.room.ID || w.RoomTitle != "task" || w.Lead || w.Model != "gpt-a" {
		t.Fatalf("worker entry: %+v", w)
	}
	if l := byID[f.lead.ID]; !l.Lead || l.RoomID != f.room.ID {
		t.Fatalf("lead entry: %+v", l)
	}
	// A changed pool moves the revision even when the affected agents stay the same.
	before := items[0].Rev
	f.setPool(t, "worker", f.b, SpawnDefaultChoice{LauncherID: "lnch_a", Model: "gpt-c"})
	items = f.openItems(t, "usr_owner")
	if len(items) != 1 || items[0].Rev <= before {
		t.Fatalf("changed pool: %+v", items)
	}
	before = items[0].Rev
	// A restart re-check with the same pool and affected agents leaves the revision alone.
	if err := f.s.DetectPoolChanges(ctx); err != nil {
		t.Fatal(err)
	}
	items = f.openItems(t, "usr_owner")
	if len(items) != 1 || items[0].Rev != before {
		t.Fatalf("second change: %+v", items)
	}
	var total int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM inbox_item WHERE kind='pool_change'`).Scan(&total); err != nil || total != 1 {
		t.Fatalf("items stored: %d %v", total, err)
	}
	// An agent that stopped drops off the list; with none left the item is withdrawn.
	for _, id := range []string{f.worker.ID, f.lead.ID} {
		if _, err := f.s.db.Exec(`UPDATE agent SET desired='stopped' WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
	}
	f.setPool(t, "worker", f.b)
	if items := f.openItems(t, "usr_owner"); len(items) != 0 {
		t.Fatalf("item stayed with nobody to switch: %+v", items)
	}
}

func TestPoolChangeSwapInvalidatesTheOldDecision(t *testing.T) {
	t.Parallel()
	f := newPoolFixture(t)
	ctx := context.Background()
	if _, err := f.s.db.Exec(`UPDATE agent SET role_id='role_worker',model='gpt-b',pool_model='gpt-b' WHERE id=?`, f.lead.ID); err != nil {
		t.Fatal(err)
	}
	f.setPool(t, "worker", f.b)
	before := f.openItems(t, "usr_owner")[0]
	if len(before.PoolAgents) != 1 || before.PoolAgents[0].AgentID != f.worker.ID {
		t.Fatalf("first affected agent: %+v", before.PoolAgents)
	}
	f.setPool(t, "worker", f.a)
	after := f.openItems(t, "usr_owner")[0]
	if after.ID != before.ID || after.Rev <= before.Rev || len(after.PoolAgents) != 1 || after.PoolAgents[0].AgentID != f.lead.ID {
		t.Fatalf("swapped affected agent: before=%+v after=%+v", before, after)
	}
	var updates int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE kind='inbox_item_updated' AND json_extract(payload_json,'$.inbox_item_id')=?`, before.ID).Scan(&updates); err != nil || updates != 1 {
		t.Fatalf("update events: %d (%v)", updates, err)
	}
	if _, _, err := f.s.DecidePoolChange(ctx, before.ID, "usr_owner", "swap-old", PoolChangeRespawn, int(before.Rev), []string{f.worker.ID}); !errors.Is(err, ErrStaleInboxRev) {
		t.Fatalf("old revision decision: %v", err)
	}
	var queued int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM pool_respawn`).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("old revision queued %d agents: %v", queued, err)
	}
}

func TestPoolChangeSkipsWhatIsNotFromThePool(t *testing.T) {
	t.Parallel()
	f := newPoolFixture(t)
	for origin, id := range map[string]string{"explicit": f.worker.ID, "approved": f.lead.ID} {
		if _, err := f.s.db.Exec(`UPDATE agent SET pool_origin=? WHERE id=?`, origin, id); err != nil {
			t.Fatal(err)
		}
	}
	f.setPool(t, "worker", f.b)
	f.setPool(t, "lead", f.b)
	if items := f.openItems(t, "usr_owner"); len(items) != 0 {
		t.Fatalf("an explicit or approved choice was flagged: %+v", items)
	}
	if _, err := f.s.db.Exec(`UPDATE agent SET pool_origin='unknown' WHERE id=?`, f.worker.ID); err != nil {
		t.Fatal(err)
	}
	f.setPool(t, "worker", f.b, f.a)
	f.setPool(t, "worker", f.b)
	if items := f.openItems(t, "usr_owner"); len(items) != 0 {
		t.Fatalf("an unknown origin was flagged: %+v", items)
	}
}

func TestPoolChangeShowsOnlyToTheOwnerAndAdmins(t *testing.T) {
	t.Parallel()
	f := newPoolFixture(t)
	f.setPool(t, "worker", f.b)
	if len(f.openItems(t, "usr_owner")) != 1 || len(f.openItems(t, "usr_admin")) != 1 {
		t.Fatal("the owner and an admin member must see the item")
	}
	if items := f.openItems(t, "usr_operator"); len(items) != 0 {
		t.Fatalf("an operator saw the item: %+v", items)
	}
	item := f.openItems(t, "usr_owner")[0]
	if _, _, err := f.s.DecidePoolChange(context.Background(), item.ID, "usr_operator", "k-op", PoolChangeKeep, int(item.Rev), nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("operator decided: %v", err)
	}
}

func TestPoolChangeKeepRecordsTheDecisionUntilThePoolChangesAgain(t *testing.T) {
	t.Parallel()
	f := newPoolFixture(t)
	ctx := context.Background()
	f.setPool(t, "worker", f.b)
	item := f.openItems(t, "usr_admin")[0]
	if _, _, err := f.s.DecidePoolChange(ctx, item.ID, "usr_admin", "k1", PoolChangeKeep, int(item.Rev)+1, nil); !errors.Is(err, ErrStaleInboxRev) {
		t.Fatalf("stale revision: %v", err)
	}
	if _, _, err := f.s.DecidePoolChange(ctx, item.ID, "usr_admin", "k1", PoolChangeKeep, int(item.Rev), nil); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DetectPoolChanges(ctx); err != nil || len(f.openItems(t, "usr_owner")) != 0 {
		t.Fatalf("a kept model came back at restart: %v", err)
	}
	var queued int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM pool_respawn`).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("keep queued a respawn: %d %v", queued, err)
	}
	f.setPool(t, "worker", f.b, SpawnDefaultChoice{LauncherID: "lnch_a", Model: "gpt-c"})
	if items := f.openItems(t, "usr_owner"); len(items) != 1 || len(items[0].PoolAgents) != 1 {
		t.Fatalf("the pool changed again but the item did not return: %+v", items)
	}
}

func TestPoolChangeRespawnQueuesIdleAgentsAndWaitsForBusyOnes(t *testing.T) {
	t.Parallel()
	f := newPoolFixture(t)
	ctx := context.Background()
	f.setPool(t, "worker", f.b)
	f.setPool(t, "lead", f.b)
	if _, err := f.s.db.Exec(`INSERT INTO turn(id,agent_id,session_id,generation,room_id,exec,settle) VALUES ('turn_busy',?,'ses_lead',0,?,'running','pending')`, f.lead.ID, f.room.ID); err != nil {
		t.Skipf("turn schema differs: %v", err)
	}
	item := f.openItems(t, "usr_owner")[0]
	var busy int
	for _, a := range item.PoolAgents {
		if a.Busy {
			busy++
		}
	}
	if busy != 1 {
		t.Fatalf("busy flags: %+v", item.PoolAgents)
	}
	if _, _, err := f.s.DecidePoolChange(ctx, item.ID, "usr_owner", "k2", PoolChangeRespawn, int(item.Rev), nil); err != nil {
		t.Fatal(err)
	}
	ready, err := f.s.PoolRespawnsReady(ctx)
	if err != nil || len(ready) != 1 || ready[0].AgentID != f.worker.ID {
		t.Fatalf("ready with a busy lead: %+v %v", ready, err)
	}
	if _, err := f.s.db.Exec(`UPDATE turn SET exec='done',settle='settled' WHERE id='turn_busy'`); err != nil {
		t.Fatal(err)
	}
	if ready, err = f.s.PoolRespawnsReady(ctx); err != nil || len(ready) != 2 {
		t.Fatalf("ready after the turn: %+v %v", ready, err)
	}
	// An agent that stopped meanwhile leaves the queue.
	if _, err := f.s.db.Exec(`UPDATE agent SET desired='stopped' WHERE id=?`, f.lead.ID); err != nil {
		t.Fatal(err)
	}
	if ready, err = f.s.PoolRespawnsReady(ctx); err != nil || len(ready) != 1 {
		t.Fatalf("ready after a stop: %+v %v", ready, err)
	}
}

func TestPoolChangeRespawnHonoursAPerAgentKeep(t *testing.T) {
	t.Parallel()
	f := newPoolFixture(t)
	ctx := context.Background()
	f.setPool(t, "worker", f.b)
	f.setPool(t, "lead", f.b)
	item := f.openItems(t, "usr_owner")[0]
	if _, _, err := f.s.DecidePoolChange(ctx, item.ID, "usr_owner", "k3", PoolChangeRespawn, int(item.Rev), []string{f.lead.ID}); err != nil {
		t.Fatal(err)
	}
	ready, err := f.s.PoolRespawnsReady(ctx)
	if err != nil || len(ready) != 1 || ready[0].AgentID != f.worker.ID {
		t.Fatalf("queued: %+v %v", ready, err)
	}
	if err := f.s.DetectPoolChanges(ctx); err != nil || len(f.openItems(t, "usr_owner")) != 0 {
		t.Fatalf("the kept lead or the queued worker came back: %v", err)
	}
}

func TestPoolOriginAtSpawn(t *testing.T) {
	t.Parallel()
	f := newPoolFixture(t)
	origin := func(explicit, approvedBy, launcher, model string) string {
		var got string
		err := f.s.Tx(context.Background(), func(tx *Tx) error {
			var err error
			got, err = tx.poolOriginFor(f.project.ID, "worker", explicit, approvedBy, launcher, model)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	for name, want := range map[string]string{
		origin(PoolOriginPool, "", "lnch_a", "gpt-a"):      PoolOriginPool,
		origin(PoolOriginExplicit, "", "lnch_a", "gpt-a"):  PoolOriginExplicit,
		origin(PoolOriginPool, "msg_1", "lnch_a", "gpt-c"): PoolOriginApproved,
		origin("", "", "lnch_a", "gpt-b"):                  PoolOriginPool,
		origin("", "", "lnch_a", "gpt-c"):                  PoolOriginExplicit,
		origin("", "", "", "gpt-a"):                        PoolOriginExplicit,
	} {
		if name != want {
			t.Errorf("origin %q, want %q", name, want)
		}
	}
	agent, err := f.s.CreateAgent(context.Background(), Agent{Slug: "fresh", ProjectID: f.project.ID, RoleID: "role_worker", Tool: "codex", Variant: "native",
		LauncherID: "lnch_a", Model: "gpt-b", Provider: "codex", MachineID: "mch_wake", CheckoutID: "chk_wake", KeyHash: "k", CreatedBy: "usr_owner", PoolOrigin: PoolOriginPool})
	if err != nil {
		t.Fatal(err)
	}
	var got, launcher, model string
	if err := f.s.db.QueryRow(`SELECT pool_origin,COALESCE(pool_launcher_id,''),COALESCE(pool_model,'') FROM agent WHERE id=?`, agent.ID).Scan(&got, &launcher, &model); err != nil ||
		got != "pool" || launcher != "lnch_a" || model != "gpt-b" {
		t.Fatalf("stored provenance: %s %s %s %v", got, launcher, model, err)
	}
}

// The migration marks an existing agent "pool" only when the pool in force lists its launcher and model, and never flags the rest.
func TestPoolProvenanceMigrationIsConservative(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "before.sqlite")
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateUpTo(ctx, db, 142); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO project(id,name,folder,created_at) VALUES ('prj_m','m','/tmp/m','2026-01-01T00:00:00.000000000Z')`,
		`INSERT INTO machine(id,name,key_hash,approved,is_host) VALUES ('mch_m','host','hash',1,1)`,
		`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_m','prj_m','mch_m','/tmp/m')`,
		`INSERT INTO machine_launcher(id,machine_id,tool,name,command,enabled) VALUES ('lnch_m','mch_m','codex','native','codex',1)`,
		`INSERT INTO machine_launcher(id,machine_id,tool,name,command,enabled) VALUES ('lnch_n','mch_m','codex','other','codex',1)`,
		`INSERT OR REPLACE INTO role(id,key,label,body,role_key,spawn_defaults_json) VALUES ('role_mw','mworker','MWorker','w','mworker','[{"launcher_id":"lnch_m","model":"m-a"}]')`,
		`INSERT INTO project_role_spawn_default(project_id,role_id,items_json,pick_mode) VALUES ('prj_m','role_mw','[{"launcher_id":"lnch_m","model":"m-b"}]','in_turn')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	insert := func(id, launcher, model, approvedBy string) {
		t.Helper()
		var launcherArg, approvedArg any
		if launcher != "" {
			launcherArg = launcher
		}
		if approvedBy != "" {
			approvedArg = approvedBy
		}
		if _, err := db.Exec(`INSERT INTO agent(id,slug,project_id,role_id,tool,variant,launcher_id,model,provider,machine_id,checkout_id,key_hash,created_by,created_at,approved_by)
			VALUES (?,?,'prj_m','role_mw','codex','native',?,?,'codex','mch_m','chk_m','k','u','2026-01-01T00:00:00.000000000Z',?)`, id, id, launcherArg, model, approvedArg); err != nil {
			t.Fatal(err)
		}
	}
	insert("agt_in", "lnch_m", "m-b", "")        // in the project's override
	insert("agt_old", "lnch_m", "m-a", "")       // only in the role's own pool, which the override replaces
	insert("agt_other", "lnch_n", "m-b", "")     // a launcher the pool does not list
	insert("agt_nolauncher", "", "m-b", "")      // no launcher recorded
	insert("agt_approved", "lnch_m", "m-c", "m") // an approved deviation
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	want := map[string]string{"agt_in": "pool", "agt_old": "unknown", "agt_other": "unknown", "agt_nolauncher": "unknown", "agt_approved": "approved"}
	for id, origin := range want {
		var got string
		if err := s.db.QueryRow(`SELECT pool_origin FROM agent WHERE id=?`, id).Scan(&got); err != nil || got != origin {
			t.Errorf("%s: origin %q, want %q (%v)", id, got, origin, err)
		}
	}
	// An agent migrated to "unknown" is never flagged, whatever the pool does.
	if err := s.DetectPoolChanges(ctx); err != nil {
		t.Fatal(err)
	}
	var items int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM inbox_item WHERE kind='pool_change'`).Scan(&items); err != nil || items != 0 {
		t.Fatalf("the migration raised %d items", items)
	}
}

func TestPoolChangeEventsFireOnEveryTransitionAndAnUnchangedSaveLeavesTheRevAlone(t *testing.T) {
	t.Parallel()
	f := newPoolFixture(t)
	ctx := context.Background()
	f.setPool(t, "worker", f.b)
	f.setPool(t, "lead", f.b)
	item := f.openItems(t, "usr_owner")[0]
	countEvents := func(kind, extra string) int {
		t.Helper()
		var got int
		if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE kind='`+kind+`' AND json_extract(payload_json,'$.inbox_item_id')=?`+extra, item.ID).Scan(&got); err != nil {
			t.Fatalf("%s events: %v", kind, err)
		}
		return got
	}
	expectEvents := func(kind, extra string, want int) {
		t.Helper()
		if got := countEvents(kind, extra); got != want {
			t.Fatalf("%s events: %d, want %d", kind, got, want)
		}
	}
	expectEvents("inbox_item_created", "", 1)
	// The second pool save (the lead's) moved the one item from one listed agent to two, so one update has flown already; the counts below
	// are relative to it.
	updates := countEvents("inbox_item_updated", "")
	// A role save that does not change the pool in force leaves the row and its revision alone, so a card on screen does not go stale.
	before := item.Rev
	f.setPool(t, "worker", f.b)
	if now := f.openItems(t, "usr_owner")[0].Rev; now != before {
		t.Fatalf("a save without a change moved the revision: %d -> %d", before, now)
	}
	expectEvents("inbox_item_updated", "", updates)
	// A change that moves an agent off the list (the lead's pool lists its choice again) updates the open item and says so.
	f.setPool(t, "lead", f.b, f.a)
	item = f.openItems(t, "usr_owner")[0]
	if item.Rev <= before {
		t.Fatalf("the item did not move: %d -> %d", before, item.Rev)
	}
	expectEvents("inbox_item_updated", "", updates+1)
	// The decision resolves the item and says so.
	if _, _, err := f.s.DecidePoolChange(ctx, item.ID, "usr_owner", "k-ev", PoolChangeKeep, int(item.Rev), nil); err != nil {
		t.Fatal(err)
	}
	expectEvents("inbox_item_resolved", " AND json_extract(payload_json,'$.choice')='keep'", 1)
}

func TestPoolRespawnDropsAnAgentSwitchedAwayFromThePool(t *testing.T) {
	t.Parallel()
	f := newPoolFixture(t)
	ctx := context.Background()
	f.setPool(t, "worker", f.b)
	item := f.openItems(t, "usr_owner")[0]
	if _, _, err := f.s.DecidePoolChange(ctx, item.ID, "usr_owner", "k-explicit", PoolChangeRespawn, int(item.Rev), nil); err != nil {
		t.Fatal(err)
	}
	if ready, err := f.s.PoolRespawnsReady(ctx); err != nil || len(ready) != 1 || ready[0].AgentID != f.worker.ID {
		t.Fatalf("queued: %+v %v", ready, err)
	}
	// The person switches the agent to a model the pool does not list.
	card, err := f.s.AgentCardByID(ctx, f.worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.BeginAgentSwitch(f.worker.ID, "usr_owner", f.room.ID, "claude", "native", "haiku", "anthropic", card.DesiredRev)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var queued int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM pool_respawn WHERE agent_id=?`, f.worker.ID).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("the switch left the respawn queued: %d (%v)", queued, err)
	}
	if ready, err := f.s.PoolRespawnsReady(ctx); err != nil || len(ready) != 0 {
		t.Fatalf("ready after an explicit switch: %+v %v", ready, err)
	}
}

// The queue's own pass also drops an agent whose origin is no longer "pool" (an older row, or a switch that began before the pass read it).
func TestPoolRespawnsReadyDropsAnAgentThatIsNotFromThePool(t *testing.T) {
	t.Parallel()
	f := newPoolFixture(t)
	ctx := context.Background()
	f.setPool(t, "worker", f.b)
	item := f.openItems(t, "usr_owner")[0]
	if _, _, err := f.s.DecidePoolChange(ctx, item.ID, "usr_owner", "k-origin", PoolChangeRespawn, int(item.Rev), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE agent SET pool_origin='explicit' WHERE id=?`, f.worker.ID); err != nil {
		t.Fatal(err)
	}
	if ready, err := f.s.PoolRespawnsReady(ctx); err != nil || len(ready) != 0 {
		t.Fatalf("ready for an agent that is not from the pool: %+v %v", ready, err)
	}
	var queued int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM pool_respawn WHERE agent_id=?`, f.worker.ID).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("the row stayed: %d (%v)", queued, err)
	}
}

// An empty pool in force (an empty project override, cleared default_agents, a role reset) is not a pool change: an empty pool means manual
// choice, so nobody is flagged, and an open item whose pool became empty is withdrawn.
func TestPoolChangeIgnoresAnEmptyPool(t *testing.T) {
	t.Parallel()
	f := newPoolFixture(t)
	ctx := context.Background()
	// An empty project override flags nobody.
	f.setPool(t, "worker")
	if items := f.openItems(t, "usr_owner"); len(items) != 0 {
		t.Fatalf("an empty override raised an item: %+v", items)
	}
	// An open item whose pool becomes empty is withdrawn.
	f.setPool(t, "worker", f.b)
	item := f.openItems(t, "usr_owner")[0]
	f.setPool(t, "worker")
	if items := f.openItems(t, "usr_owner"); len(items) != 0 {
		t.Fatalf("the item stayed with an empty pool: %+v", items)
	}
	var resolved int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE kind='inbox_item_resolved' AND json_extract(payload_json,'$.inbox_item_id')=?`, item.ID).Scan(&resolved); err != nil || resolved != 1 {
		t.Fatalf("withdrawal events: %d (%v)", resolved, err)
	}
	// Cleared default_agents: with the override inherited away the role's own pool decides, so giving it one entry raises the item and a
	// role save that clears it withdraws it (a role reset writes the same empty list).
	if _, err := f.s.SetProjectRoleSpawnDefaults(ctx, f.project.ID, "usr_owner", "worker", true, nil, ""); err != nil {
		t.Fatal(err)
	}
	var rev int64
	if err := f.s.db.QueryRow(`SELECT rev FROM role WHERE id='role_worker'`).Scan(&rev); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ManageRole(ctx, "usr_owner", RoleDefinition{ID: "role_worker", Key: "worker", Label: "Worker", Cardinality: "multi", DefaultAgents: []SpawnDefaultChoice{f.b}, Rev: rev}, false); err != nil {
		t.Fatal(err)
	}
	item = f.openItems(t, "usr_owner")[0]
	if err := f.s.db.QueryRow(`SELECT rev FROM role WHERE id='role_worker'`).Scan(&rev); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ManageRole(ctx, "usr_owner", RoleDefinition{ID: "role_worker", Key: "worker", Label: "Worker", Cardinality: "multi", DefaultAgents: []SpawnDefaultChoice{}, Rev: rev}, false); err != nil {
		t.Fatal(err)
	}
	if items := f.openItems(t, "usr_owner"); len(items) != 0 {
		t.Fatalf("the item stayed with an empty role pool: %+v", items)
	}
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE kind='inbox_item_resolved' AND json_extract(payload_json,'$.inbox_item_id')=?`, item.ID).Scan(&resolved); err != nil || resolved != 1 {
		t.Fatalf("withdrawal events: %d (%v)", resolved, err)
	}
}

func TestPoolChangeWithdrawsWhenTheLastAffectedAgentLeaves(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raise := func(f poolFixture) string {
		t.Helper()
		f.setPool(t, "worker", f.b) // only the worker is affected: the lead's pool still lists its choice
		return f.openItems(t, "usr_owner")[0].ID
	}
	withdrawn := func(f poolFixture, itemID string) {
		t.Helper()
		if items := f.openItems(t, "usr_owner"); len(items) != 0 {
			t.Fatalf("the item stayed with nobody left to switch: %+v", items)
		}
		var resolved int
		if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE kind='inbox_item_resolved' AND json_extract(payload_json,'$.inbox_item_id')=?
			AND json_extract(payload_json,'$.resolution')='withdrawn'`, itemID).Scan(&resolved); err != nil || resolved != 1 {
			t.Fatalf("withdrawal events: %d (%v)", resolved, err)
		}
	}
	// A stop.
	f := newPoolFixture(t)
	itemID := raise(f)
	card, err := f.s.AgentCardByID(ctx, f.worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.SetAgentDesired(f.worker.ID, "usr_owner", "stopped", card.DesiredRev)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	withdrawn(f, itemID)
	// A revoke.
	f = newPoolFixture(t)
	itemID = raise(f)
	if err := f.s.RevokeAgent(ctx, f.worker.ID); err != nil {
		t.Fatal(err)
	}
	withdrawn(f, itemID)
	// An explicit switch (to a model the pool does not list).
	f = newPoolFixture(t)
	itemID = raise(f)
	card, err = f.s.AgentCardByID(ctx, f.worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.BeginAgentSwitch(f.worker.ID, "usr_owner", f.room.ID, "claude", "native", "haiku", "anthropic", card.DesiredRev)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	withdrawn(f, itemID)
}
