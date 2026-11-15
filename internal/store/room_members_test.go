package store

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

// membersFixture is a project with people (one disabled), roles (project and global, singleton and
// multi) and a helper that puts agents in rooms.
type membersFixture struct {
	s       *Store
	project Project
	home    Room
	other   Room
	alice   string // owner
	bob     string // viewer
	carol   string // operator, disabled
	n       int
}

func newMembersFixture(t *testing.T) *membersFixture {
	t.Helper()
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	project, home, _ := fixture(t, s)
	other, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "task", Title: "Other", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	f := &membersFixture{s: s, project: project, home: home, other: other}
	for _, spec := range []struct {
		login, role string
		out         *string
	}{{"alice", "owner", &f.alice}, {"bob", "viewer", &f.bob}, {"carol", "operator", &f.carol}} {
		if err := s.Tx(ctx, func(tx *Tx) error {
			user, err := tx.CreateUser(User{Login: spec.login, PWHash: "h", PWParams: "p"})
			*spec.out = user.ID
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,?)`, project.ID, *spec.out, spec.role); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`UPDATE user SET disabled_at='now' WHERE id=?`, f.carol); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO machine(id,name,key_hash,approved,is_host) VALUES ('mch_members','host','hash',1,1)`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_members',?,'mch_members',?)`, project.ID, filepath.Join(project.Folder, "co")); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *membersFixture) role(t *testing.T, id, projectID, key, label, emoji, cardinality string) {
	t.Helper()
	var project any
	if projectID != "" {
		project = projectID
	}
	if _, err := f.s.db.Exec(`INSERT INTO role(id,key,project_id,role_key,label,emoji,body,cardinality) VALUES (?,?,?,?,?,?,?,?)`,
		id, id, project, key, label, emoji, "body", cardinality); err != nil {
		t.Fatal(err)
	}
}

// agent creates an agent under roleID and makes it a member of room (when given).
func (f *membersFixture) agent(t *testing.T, slug, roleID string, room *Room) Agent {
	t.Helper()
	f.n++
	agent, err := f.s.CreateAgent(context.Background(), Agent{Slug: slug, ProjectID: f.project.ID, RoleID: roleID, Tool: "fake", Variant: "test", Model: "m",
		Provider: "p", MachineID: "mch_members", CheckoutID: "chk_members", KeyHash: fmt.Sprintf("h%d", f.n), CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	if room != nil {
		if err := f.s.AddMember(context.Background(), room.ID, "agent", agent.ID); err != nil {
			t.Fatal(err)
		}
	}
	return agent
}

func (f *membersFixture) members(t *testing.T, room Room) RoomMembers {
	t.Helper()
	got, err := f.s.RoomMembers(context.Background(), room.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func logins(humans []RoomMemberHuman) []string {
	out := []string{}
	for _, h := range humans {
		out = append(out, h.Login+":"+h.Role)
	}
	return out
}

func TestRoomMembersListsPeopleAndAgentsWithRoles(t *testing.T) {
	t.Parallel()
	f := newMembersFixture(t)
	// The default global roles: lead (singleton) and worker (multi).
	var leadEmoji, workerEmoji string
	if err := f.s.db.QueryRow(`SELECT emoji FROM role WHERE id='role_leader_default'`).Scan(&leadEmoji); err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.QueryRow(`SELECT emoji FROM role WHERE id='role_worker_default'`).Scan(&workerEmoji); err != nil {
		t.Fatal(err)
	}
	lead := f.agent(t, "tessa", "role_leader_default", &f.home)
	worker := f.agent(t, "drew", "role_worker_default", &f.home)
	f.agent(t, "elsewhere", "role_worker_default", &f.other)

	got := f.members(t, f.home)
	// Everyone active in the project (carol is disabled) with the project role, sorted by login.
	if want := []string{"alice:owner", "bob:viewer"}; !reflect.DeepEqual(logins(got.Humans), want) {
		t.Fatalf("humans = %v, want %v", logins(got.Humans), want)
	}
	if len(got.Agents) != 2 {
		t.Fatalf("agents = %+v", got.Agents)
	}
	a0, a1 := got.Agents[0], got.Agents[1]
	if a0.ID != lead.ID || a0.Slug != "tessa" || a0.RoleKey != "leader" || a0.RoleLabel != "Leader" || a0.RoleEmoji != leadEmoji || !a0.Singleton {
		t.Fatalf("lead member = %+v", a0)
	}
	if a1.ID != worker.ID || a1.RoleLabel != "Worker" || a1.RoleEmoji != workerEmoji || a1.Singleton {
		t.Fatalf("worker member = %+v", a1)
	}
	if a0.Desired == "" || a0.Observed == "" {
		t.Fatalf("member lacks its state: %+v", a0)
	}
}

func TestRoomMembersSingletonHolders(t *testing.T) {
	t.Parallel()
	f := newMembersFixture(t)
	f.role(t, "role_review", "", "gatekeeper", "Gatekeeper", "🔍", "singleton")
	f.role(t, "role_plan", "", "planner", "Planner", "🗺", "singleton")
	f.role(t, "role_scribe", "", "scribe", "Scribe", "✍", "singleton")
	f.role(t, "role_courier", "", "courier", "Courier", "📦", "singleton")
	f.role(t, "role_multi", "", "crew", "Crew", "👥", "multi") // a role with many holders is not a singleton
	f.role(t, "role_gone", "", "gone", "Gone", "💨", "singleton")
	if _, err := f.s.db.Exec(`UPDATE role SET deleted_at='now' WHERE id='role_gone'`); err != nil {
		t.Fatal(err)
	}

	f.agent(t, "tessa", "role_leader_default", &f.home)  // holds the built-in lead, in this room
	f.agent(t, "ravi", "role_plan", &f.other)            // holds planner, in another room
	stopped := f.agent(t, "sam", "role_review", &f.home) // a stopped agent does not hold the role
	revoked := f.agent(t, "rex", "role_review", &f.home) // neither does a revoked one
	if _, err := f.s.db.Exec(`UPDATE agent SET desired='stopped',observed='stopped' WHERE id=?`, stopped.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE agent SET revoked_at='now' WHERE id=?`, revoked.ID); err != nil {
		t.Fatal(err)
	}

	got := f.members(t, f.home)
	byKey := map[string]RoomSingleton{}
	var order []string
	for _, singleton := range got.Singletons {
		byKey[singleton.RoleKey] = singleton
		order = append(order, singleton.Label)
	}
	if want := []string{"Advisor", "Courier", "Dispatcher", "Gatekeeper", "Leader", "Planner", "Scribe"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("singleton roles = %v, want %v (%+v)", order, want, got.Singletons)
	}
	if h := byKey["leader"].HeldBy; h == nil || h.Slug != "tessa" || !h.InThisRoom || byKey["leader"].Emoji != "👑" {
		t.Fatalf("lead = %+v", byKey["leader"])
	}
	if h := byKey["planner"].HeldBy; h == nil || h.Slug != "ravi" || h.InThisRoom {
		t.Fatalf("a holder in another room must be reported without naming the room: %+v", byKey["planner"])
	}
	if byKey["gatekeeper"].HeldBy != nil {
		t.Fatalf("a stopped or revoked agent holds nothing: %+v", byKey["gatekeeper"])
	}
	if byKey["scribe"].HeldBy != nil {
		t.Fatalf("scribe should be vacant: %+v", byKey["scribe"])
	}
	// The same holder, seen from the room it works in.
	for _, singleton := range f.members(t, f.other).Singletons {
		if singleton.RoleKey == "planner" && (singleton.HeldBy == nil || !singleton.HeldBy.InThisRoom) {
			t.Fatalf("planner from its own room: %+v", singleton)
		}
	}
}

func TestRoomMembersPrivateChatShowsOnlyItsPeople(t *testing.T) {
	t.Parallel()
	f := newMembersFixture(t)
	f.agent(t, "tessa", "role_leader_default", &f.home)
	dm, err := f.s.OpenUserDM(context.Background(), f.project.ID, f.alice, f.bob)
	if err != nil {
		t.Fatal(err)
	}
	got := f.members(t, dm)
	if want := []string{"alice:owner", "bob:viewer"}; !reflect.DeepEqual(logins(got.Humans), want) {
		t.Fatalf("private chat people = %v, want %v", logins(got.Humans), want)
	}
	if len(got.Agents) != 0 || len(got.Singletons) != 0 {
		t.Fatalf("a private chat shows no project roles: %+v", got)
	}
	if _, err := f.s.RoomMembers(context.Background(), "room_missing"); err == nil {
		t.Fatal("an unknown room was listed")
	}
}
