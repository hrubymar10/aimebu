package store

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHotLookupPlans(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	if _, err := s.readDB.ExecContext(t.Context(), `UPDATE seq_counter SET value=value+1 WHERE id=1`); err == nil {
		t.Fatal("read pool accepted a write")
	}
	for _, check := range []struct{ query, index string }{
		{`SELECT MAX(created_at) FROM event WHERE agent_id=?`, "event_agent_created"},
		{`SELECT payload_json FROM event WHERE turn_id=? AND kind='turn_process_exit'`, "event_turn_kind"},
		{`SELECT id FROM turn WHERE agent_id=? ORDER BY id DESC LIMIT 1`, "turn_agent"},
	} {
		rows, err := s.readDB.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+check.query, "agt_example")
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(detail, "SCAN event") || strings.Contains(detail, "SCAN turn") {
				t.Errorf("full scan: %s", detail)
			}
			if strings.Contains(detail, check.index) {
				found = true
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		if !found {
			t.Errorf("query did not use %s: %s", check.index, check.query)
		}
	}
}

func TestHotReadWorkload50k(t *testing.T) {
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	if _, err := s.db.Exec(`UPDATE turn SET exec='running',started_at='2026-10-01T00:00:00.000000000Z' WHERE id=?`, f.turn.ID); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_perf','perf','hash','{}','now')`,
		`INSERT INTO membership(project_id,user_id,role) VALUES ('` + f.project.ID + `','usr_perf','owner')`,
		`INSERT INTO room_member(room_id,member_kind,member_id,joined_at) VALUES ('` + f.room.ID + `','agent','` + f.agent.ID + `','now')`,
	} {
		if _, err := s.db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	var base int64
	if err := s.db.QueryRow(`SELECT MAX(seq) FROM event`).Scan(&base); err != nil {
		t.Fatal(err)
	}
	_, err := s.db.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<50000)
		INSERT INTO event(id,seq,kind,project_id,room_id,agent_id,turn_id,payload_json,created_at)
		SELECT 'evt_perf_'||i,?+i,'supervisor_activity',?,?,?,?,'{}','2026-10-01T00:00:00.000000000Z' FROM n`,
		base, f.project.ID, f.room.ID, f.agent.ID, f.turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const runs = 10
	measure := func(name string, run func() error) {
		t.Helper()
		start := time.Now()
		for i := 0; i < runs; i++ {
			if err := run(); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("%s: %s/op over %d runs", name, time.Since(start)/runs, runs)
	}
	measure("health", func() error { _, err := s.HealthForUser(ctx, "usr_perf", time.Now(), 10*time.Minute); return err })
	measure("room cards", func() error { _, err := s.AgentCardsForRoom(ctx, f.room.ID); return err })
	all, err := s.HealthForUser(ctx, "usr_perf", time.Now(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	one, err := s.AgentHealth(ctx, f.agent.ID, time.Now(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || len(all[0].RoomIDs) != 1 || all[0].RoomIDs[0] != f.room.ID || !reflect.DeepEqual(all[0].Flags, one) {
		t.Fatalf("batched health=%+v; agent health=%+v", all, one)
	}
	other, err := s.HealthForUser(ctx, "usr_elsewhere", time.Now(), 10*time.Minute)
	if err != nil || len(other) != 0 {
		t.Fatalf("invisible agents=%+v, error=%v", other, err)
	}
	reader, err := s.readDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := reader.ExecContext(ctx, `BEGIN`); err != nil {
		t.Fatal(err)
	}
	defer reader.ExecContext(ctx, `ROLLBACK`)
	var count int
	if err := reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM event`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count < 50000 {
		t.Fatalf("event count=%d", count)
	}
	writeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	start := time.Now()
	if err := s.Tx(writeCtx, func(tx *Tx) error { _, err := tx.Exec(`UPDATE seq_counter SET value=value+1 WHERE id=1`); return err }); err != nil {
		t.Fatal(err)
	}
	t.Logf("write with reader snapshot open: %s", time.Since(start))
}

func TestOpenAcceptsARelativePath(t *testing.T) {
	// The read-only pool's DSN is a file: URI, which takes the first segment of a relative path for its authority.
	t.Chdir(t.TempDir())
	if err := os.Mkdir("srv", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("srv", "a.sqlite"), blankTestDB, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.Context(), filepath.Join("srv", "a.sqlite"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !filepath.IsAbs(s.dbPath) {
		t.Fatalf("dbPath=%q", s.dbPath)
	}
	var one int
	if err := s.readDB.QueryRowContext(t.Context(), `SELECT 1`).Scan(&one); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(t.Context(), func(tx *Tx) error { _, err := tx.Exec(`UPDATE seq_counter SET value=value+1 WHERE id=1`); return err }); err != nil {
		t.Fatal(err)
	}
}

// healthBefore is what the health endpoint did before HealthForUser: every project, then every room the user can see, then that room's
// agent cards, with each agent's health computed once.
func healthBefore(t *testing.T, s *Store, userID string, now time.Time, stuckAfter time.Duration) []HealthAgent {
	t.Helper()
	ctx := t.Context()
	byID := map[string]*HealthAgent{}
	ordered := []*HealthAgent{}
	projects, err := s.ProjectsForUser(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	for _, project := range projects {
		rooms, err := s.RoomsForUserProject(ctx, project.ID, userID)
		if err != nil {
			t.Fatal(err)
		}
		for _, room := range rooms {
			cards, err := s.AgentCardsForRoom(ctx, room.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, card := range cards {
				item := byID[card.ID]
				if item == nil {
					flags, err := s.AgentHealth(ctx, card.ID, now, stuckAfter)
					if err != nil {
						t.Fatal(err)
					}
					item = &HealthAgent{ID: card.ID, Name: card.Slug, RoomIDs: []string{}, Flags: flags}
					byID[card.ID] = item
					ordered = append(ordered, item)
				}
				item.RoomIDs = append(item.RoomIDs, room.ID)
			}
		}
	}
	out := []HealthAgent{}
	for _, item := range ordered {
		out = append(out, *item)
	}
	return out
}

type healthWorld struct {
	s       *Store
	f       executionFixture
	now     time.Time
	execute func(query string, args ...any)
}

func (w healthWorld) room(id string, depth int, kind, dmKey, createdAt string) {
	var key any
	if dmKey != "" {
		key = dmKey
	}
	w.execute(`INSERT INTO room(id,project_id,parent_id,depth,kind,dm_key,title,created_by,created_at) VALUES (?,?,NULL,?,?,?,?,'usr_test',?)`,
		id, w.f.project.ID, depth, kind, key, id, createdAt)
}

func (w healthWorld) agent(t *testing.T, slug, tool string) Agent {
	t.Helper()
	agent, err := w.s.CreateAgent(t.Context(), Agent{Slug: slug, ProjectID: w.f.project.ID, RoleID: "role_test", Tool: tool, Variant: "native",
		Model: "test", Provider: "test", MachineID: "mch_test", CheckoutID: "chk_test", KeyHash: "hash", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

func (w healthWorld) join(room, kind, member string, left bool) {
	leftAt := any(nil)
	if left {
		leftAt = "2026-10-02T00:00:00.000000000Z"
	}
	w.execute(`INSERT INTO room_member(room_id,member_kind,member_id,joined_at,left_at) VALUES (?,?,?,'2026-10-01T00:00:00.000000000Z',?)`, room, kind, member, leftAt)
}

func (w healthWorld) stamp(ago time.Duration) string {
	return w.now.Add(-ago).UTC().Format("2006-01-02T15:04:05.000000000Z")
}

// TestHealthForUserMatchesPerRoomComposition holds the one-query health read to what the per-room composition it replaced returned:
// the same agents for the same viewer, the same rooms in the same order, the same flags, and the same agent order inside each room.
func TestHealthForUserMatchesPerRoomComposition(t *testing.T) {
	const stuckAfter = 10 * time.Minute
	cases := []struct {
		name  string
		user  string
		setup func(t *testing.T, w healthWorld)
		// rooms lists, by agent slug, the rooms the agent must be reported in; HOME is the fixture's room. flags lists the codes.
		rooms map[string][]string
		flags map[string][]string
		// order, when set, is the agents' names in the order the list must come back in.
		order []string
	}{
		{name: "an agent in several rooms appears once with its rooms in depth, creation and id order", user: "usr_a",
			setup: func(t *testing.T, w healthWorld) {
				// Joined in the reverse of the order they have to come out in.
				w.room("room_deep", 1, "task", "", "2000-01-01T00:00:00.000000000Z")
				w.room("room_zz", 0, "task", "", "2030-01-01T00:00:00.000000000Z")
				w.room("room_aa", 0, "task", "", "2030-01-01T00:00:00.000000000Z")
				for _, room := range []string{"room_deep", "room_zz", "room_aa"} {
					w.join(room, "agent", w.f.agent.ID, false)
				}
				w.join(w.f.room.ID, "agent", w.f.agent.ID, false)
			},
			rooms: map[string][]string{"worker": {"HOME", "room_aa", "room_zz", "room_deep"}}},
		{name: "a room the agent left is not reported", user: "usr_a",
			setup: func(t *testing.T, w healthWorld) {
				w.room("room_gone", 0, "task", "", "2030-01-01T00:00:00.000000000Z")
				w.join("room_gone", "agent", w.f.agent.ID, true)
				w.join(w.f.room.ID, "agent", w.f.agent.ID, false)
			},
			rooms: map[string][]string{"worker": {"HOME"}}},
		{name: "an agent only in a conversation the user left is hidden", user: "usr_a",
			setup: func(t *testing.T, w healthWorld) {
				w.join(w.f.room.ID, "agent", w.f.agent.ID, false)
				other := w.agent(t, "other", "codex")
				w.room("room_dm", 0, "dm", "dm:usr_a:usr_b", "2030-01-01T00:00:00.000000000Z")
				w.join("room_dm", "user", "usr_a", true)
				w.join("room_dm", "user", "usr_b", false)
				w.join("room_dm", "agent", other.ID, false)
			},
			rooms: map[string][]string{"worker": {"HOME"}}},
		{name: "the same conversation is visible to the user who is still in it", user: "usr_b",
			setup: func(t *testing.T, w healthWorld) {
				w.join(w.f.room.ID, "agent", w.f.agent.ID, false)
				other := w.agent(t, "other", "codex")
				w.room("room_dm", 0, "dm", "dm:usr_a:usr_b", "2030-01-01T00:00:00.000000000Z")
				w.join("room_dm", "user", "usr_a", true)
				w.join("room_dm", "user", "usr_b", false)
				w.join("room_dm", "agent", other.ID, false)
			},
			rooms: map[string][]string{"worker": {"HOME"}, "other": {"room_dm"}}},
		{name: "a user outside the project sees no agents", user: "usr_elsewhere",
			setup: func(t *testing.T, w healthWorld) { w.join(w.f.room.ID, "agent", w.f.agent.ID, false) },
			rooms: map[string][]string{}},
		{name: "agents come back in the order the per-room composition met them", user: "usr_a",
			setup: func(t *testing.T, w healthWorld) {
				// The second project is older, so its agent comes first. In the first project the older-dated room comes before home, so
				// the agents first met there precede "worker", whose only room is home, although "worker" was created before all of them.
				w.execute(`INSERT INTO project(id,name,folder,created_at) VALUES ('prj_old','old','/tmp/old','2000-01-01T00:00:00.000000000Z')`)
				w.execute(`INSERT INTO membership(project_id,user_id,role) VALUES ('prj_old','usr_a','owner')`)
				w.execute(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_old','prj_old','mch_test','/tmp/old/checkout')`)
				w.execute(`INSERT INTO room(id,project_id,parent_id,depth,kind,title,created_by,created_at) VALUES ('room_old','prj_old',NULL,0,'task','old','usr_test','2000-01-01T00:00:00.000000000Z')`)
				w.room("room_early", 0, "task", "", "2000-01-01T00:00:00.000000000Z")
				w.join(w.f.room.ID, "agent", w.f.agent.ID, false)
				late := w.agent(t, "late", "codex")
				both := w.agent(t, "both", "codex")
				w.join("room_early", "agent", late.ID, false)
				w.join(w.f.room.ID, "agent", both.ID, false)
				w.join("room_early", "agent", both.ID, false)
				oldAgent, err := w.s.CreateAgent(t.Context(), Agent{Slug: "oldest", ProjectID: "prj_old", RoleID: "role_test", Tool: "codex", Variant: "native",
					Model: "test", Provider: "test", MachineID: "mch_test", CheckoutID: "chk_old", KeyHash: "hash", CreatedBy: "usr_test"})
				if err != nil {
					t.Fatal(err)
				}
				w.join("room_old", "agent", oldAgent.ID, false)
			},
			rooms: map[string][]string{"worker": {"HOME"}, "late": {"room_early"}, "both": {"room_early", "HOME"}, "oldest": {"room_old"}},
			order: []string{"oldest", "late", "both", "worker"}},
		{name: "a soft-deleted role keeps its agents listed", user: "usr_a",
			setup: func(t *testing.T, w healthWorld) {
				w.join(w.f.room.ID, "agent", w.f.agent.ID, false)
				w.execute(`UPDATE role SET deleted_at='2026-10-02T00:00:00.000000000Z' WHERE id='role_test'`)
			},
			rooms: map[string][]string{"worker": {"HOME"}}},
		{name: "provider limits: an unexpired one, an open-ended one, an expired one and one for another tool", user: "usr_a",
			setup: func(t *testing.T, w healthWorld) {
				w.join(w.f.room.ID, "agent", w.f.agent.ID, false)
				for _, item := range []struct{ slug, tool, reset string }{
					{"future", "codex", w.stamp(-time.Hour)}, {"open", "codex", ""}, {"past", "codex", w.stamp(time.Hour)}, {"mismatch", "claude", w.stamp(-time.Hour)},
				} {
					agent := w.agent(t, item.slug, "codex")
					w.join(w.f.room.ID, "agent", agent.ID, false)
					var reset any
					if item.reset != "" {
						reset = item.reset
					}
					w.execute(`INSERT INTO provider_limit(agent_id,turn_id,detected_at,reset_at,tool) VALUES (?,?,?,?,?)`, agent.ID, w.f.turn.ID, w.stamp(2*time.Hour), reset, item.tool)
				}
			},
			rooms: map[string][]string{"worker": {"HOME"}, "future": {"HOME"}, "open": {"HOME"}, "past": {"HOME"}, "mismatch": {"HOME"}},
			flags: map[string][]string{"future": {"out_of_quota"}, "open": {"out_of_quota"}}},
		{name: "open recovery and delivery items combine, resolved and other kinds do not", user: "usr_a",
			setup: func(t *testing.T, w healthWorld) {
				w.join(w.f.room.ID, "agent", w.f.agent.ID, false)
				add := func(agentID, kind, state string) {
					w.execute(`INSERT INTO inbox_item(id,project_id,room_id,agent_id,requester_kind,requester_id,kind,state,created_at) VALUES (?,?,?,?,'agent',?,?,?,'2026-10-01T00:00:00.000000000Z')`,
						"inb_"+agentID+kind+state, w.f.project.ID, w.f.room.ID, agentID, agentID, kind, state)
				}
				add(w.f.agent.ID, "recovery", "open")
				add(w.f.agent.ID, "delivery_failed", "open")
				add(w.f.agent.ID, "question", "open")
				quiet := w.agent(t, "quiet", "codex")
				w.join(w.f.room.ID, "agent", quiet.ID, false)
				add(quiet.ID, "recovery", "resolved")
				add(quiet.ID, "delivery_failed", "withdrawn")
			},
			rooms: map[string][]string{"worker": {"HOME"}, "quiet": {"HOME"}},
			flags: map[string][]string{"worker": {"held_answer", "recovery_waiting"}}},
		{name: "a running turn is stuck only once its last sign of life is old", user: "usr_a",
			setup: func(t *testing.T, w healthWorld) {
				w.join(w.f.room.ID, "agent", w.f.agent.ID, false)
				w.execute(`UPDATE turn SET exec='running',started_at=? WHERE id=?`, w.stamp(time.Hour), w.f.turn.ID)
				live := w.agent(t, "live", "codex")
				w.join(w.f.room.ID, "agent", live.ID, false)
				session := "ses_live"
				w.execute(`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES (?,?,0,'fresh_with_history','now')`, session, live.ID)
				w.execute(`INSERT INTO turn(id,agent_id,session_id,generation,room_id,attempt,mode,exec,started_at) VALUES ('turn_live',?,?,0,?,1,'normal','running',?)`,
					live.ID, session, w.f.room.ID, w.stamp(time.Hour))
				w.execute(`INSERT INTO event(id,seq,kind,agent_id,turn_id,payload_json,created_at) VALUES ('evt_live',(SELECT MAX(seq)+1 FROM event),'supervisor_activity',?,'turn_live','{}',?)`, live.ID, w.stamp(time.Minute))
			},
			rooms: map[string][]string{"worker": {"HOME"}, "live": {"HOME"}},
			flags: map[string][]string{"worker": {"stuck"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := testStore(t, Options{})
			f := seedExecution(t, s)
			w := healthWorld{s: s, f: f, now: time.Now(), execute: func(query string, args ...any) {
				t.Helper()
				if _, err := s.db.Exec(query, args...); err != nil {
					t.Fatalf("%s: %v", query, err)
				}
			}}
			for _, query := range []string{
				`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_a','a','hash','{}','now')`,
				`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_b','b','hash','{}','now')`,
				`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_elsewhere','e','hash','{}','now')`,
			} {
				w.execute(query)
			}
			w.execute(`INSERT INTO membership(project_id,user_id,role) VALUES (?, 'usr_a','owner')`, f.project.ID)
			w.execute(`INSERT INTO membership(project_id,user_id,role) VALUES (?, 'usr_b','owner')`, f.project.ID)
			tc.setup(t, w)
			// The old composition lists rooms through RoomsForUserProject, which needs the user's own membership of any conversation.
			got, err := s.HealthForUser(t.Context(), tc.user, w.now, stuckAfter)
			if err != nil {
				t.Fatal(err)
			}
			want := healthBefore(t, s, tc.user, w.now, stuckAfter)
			byID := func(agents []HealthAgent) map[string]HealthAgent {
				out := map[string]HealthAgent{}
				for _, agent := range agents {
					agent.MachineID, agent.Desired = "", ""
					out[agent.ID] = agent
				}
				return out
			}
			if !reflect.DeepEqual(byID(got), byID(want)) || len(got) != len(want) {
				t.Fatalf("one query=%+v\nper room=%+v", got, want)
			}
			names := func(agents []HealthAgent) []string {
				out := []string{}
				for _, agent := range agents {
					out = append(out, agent.ID)
				}
				return out
			}
			if !reflect.DeepEqual(names(got), names(want)) {
				t.Fatalf("agent order: one query=%v, per room=%v", names(got), names(want))
			}
			if tc.order != nil {
				listed := []string{}
				for _, agent := range got {
					listed = append(listed, agent.Name)
				}
				if !reflect.DeepEqual(listed, tc.order) {
					t.Fatalf("agent order=%v, want %v", listed, tc.order)
				}
			}
			// Each room's agents keep the order its cards had: the Health and sidebar pages list them as they arrive.
			perRoom := func(agents []HealthAgent) map[string][]string {
				out := map[string][]string{}
				for _, agent := range agents {
					for _, room := range agent.RoomIDs {
						out[room] = append(out[room], agent.ID)
					}
				}
				return out
			}
			if !reflect.DeepEqual(perRoom(got), perRoom(want)) {
				t.Fatalf("agent order per room: one query=%v, per room=%v", perRoom(got), perRoom(want))
			}
			// The comparison could pass with both sides empty or both wrong, so the expectations are spelled out too.
			if len(got) != len(tc.rooms) {
				t.Fatalf("agents=%+v, want %d", got, len(tc.rooms))
			}
			for _, agent := range got {
				wantRooms := []string{}
				for _, room := range tc.rooms[agent.Name] {
					if room == "HOME" {
						room = f.room.ID
					}
					wantRooms = append(wantRooms, room)
				}
				if !reflect.DeepEqual(agent.RoomIDs, wantRooms) {
					t.Errorf("%s rooms=%v, want %v", agent.Name, agent.RoomIDs, wantRooms)
				}
				codes := []string{}
				for _, flag := range agent.Flags {
					codes = append(codes, flag.Code)
				}
				wantCodes := tc.flags[agent.Name]
				if wantCodes == nil {
					wantCodes = []string{}
				}
				if !reflect.DeepEqual(codes, wantCodes) {
					t.Errorf("%s flags=%v, want %v", agent.Name, codes, wantCodes)
				}
			}
		})
	}
}

// The old path joined role, and the new one does not: an agent row cannot lose its role, because the reference is enforced and a role is
// only ever soft-deleted.
func TestAgentRoleCannotBeRemovedUnderItsAgent(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	seedExecution(t, s)
	if _, err := s.db.Exec(`DELETE FROM role WHERE id='role_test'`); err == nil {
		t.Fatal("a role with an agent was deleted")
	}
}

func TestOpenRejectsPathsTheReadPoolWouldOpenDifferently(t *testing.T) {
	t.Parallel()
	for _, path := range []string{":memory:", "file:" + filepath.Join(t.TempDir(), "a.sqlite") + "?mode=memory"} {
		if s, err := Open(t.Context(), path, Options{}); err == nil {
			s.Close()
			t.Errorf("Open(%q) succeeded", path)
		}
	}
}
