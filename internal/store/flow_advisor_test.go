package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdvisorFlowTemplateAndSetup(t *testing.T) {
	templates := TeamFlowTemplates()
	if len(templates) != 4 || templates[2].Key != "custom" || templates[3].Key != "leader-advisor" {
		t.Fatalf("template order: %+v", templates)
	}
	flow := templates[3]
	if len(flow.CoreRoles) != 2 || !flowAllows(flow.CoreRoles, "advisor") || flowAllows(flow.SpawnRules[0].AllowedRoles, "advisor") || flow.GrantRoles[0] != "leader" {
		t.Fatalf("flow rules: %+v", flow)
	}
	if body := flow.RoleTextOverrides["advisor"]; !strings.Contains(body, roleSafety) || !strings.Contains(body, roleCommunication) {
		t.Fatal("advisor override omitted shared safety or communication instructions")
	}
	if flowRoomKind("task", "advice", "advice") != "advice" {
		t.Fatal("advice room classification")
	}
	f := newMembersFixture(t)
	ctx := context.Background()
	target := TeamSetupTarget{MachineID: "mch_members", Path: f.project.Folder, Tool: "fake", Variant: "test", Model: "test", Provider: "fake"}
	input := TeamSetupInput{FlowKey: "leader-advisor", Leader: target, Advisor: &target}
	var result TeamSetupResult
	if err := f.s.Tx(ctx, func(tx *Tx) error {
		var err error
		result, err = tx.CreateTeamSetup(f.project.ID, f.alice, input)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(result.Agents) != 2 || result.Agents[1].Role != "advisor" || len(result.CreatedRooms) != 2 {
		t.Fatalf("setup: %+v", result)
	}
	rooms := map[string]string{"home": f.home.ID, "task": f.other.ID}
	for _, room := range result.CreatedRooms {
		rooms[room.TeamFlowRoomKey] = room.ID
		if room.TeamFlowRoomKey == "advice" && !room.Restricted {
			t.Fatal("advice must be restricted")
		}
	}
	joined := func(room, agent string) bool {
		t.Helper()
		var n int
		if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_id=? AND left_at IS NULL`, room, agent).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n != 0
	}
	leader, advisor := result.Agents[0].AgentID, result.Agents[1].AgentID
	for _, room := range []string{"home", "advice", "general"} {
		if !joined(rooms[room], leader) {
			t.Fatalf("leader missing from %s", room)
		}
	}
	for _, room := range []string{"advice", "general"} {
		if !joined(rooms[room], advisor) {
			t.Fatalf("advisor missing from %s", room)
		}
	}
	if joined(rooms["home"], advisor) || joined(rooms["task"], advisor) || joined(rooms["task"], leader) {
		t.Fatal("unexpected auto-join")
	}
	if err := f.s.AddMember(ctx, rooms["home"], "agent", advisor); err != nil {
		t.Fatalf("advisor may explicitly join home: %v", err)
	}
	if err := f.s.AddMember(ctx, rooms["task"], "agent", advisor); !errors.Is(err, ErrTeamFlowRule) {
		t.Fatalf("advisor task join: %v", err)
	}
	if err := f.s.AddMember(ctx, rooms["task"], "agent", leader); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE room SET restricted=1 WHERE id=?`, rooms["task"]); err != nil {
		t.Fatal(err)
	}
	secret, err := f.s.InsertMessage(ctx, Message{RoomID: rooms["task"], AuthorKind: "agent", AuthorID: leader, Body: "task detail"})
	if err != nil {
		t.Fatal(err)
	}
	unrelated, err := f.s.InsertMessage(ctx, Message{RoomID: rooms["task"], AuthorKind: "agent", AuthorID: leader, Body: "other detail"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.AgentMessageByNo(ctx, advisor, f.project.ID, secret.No); err != nil || got.Available {
		t.Fatalf("uncited task detail: %+v %v", got, err)
	}
	if _, err := f.s.InsertMessage(ctx, Message{RoomID: rooms["advice"], AuthorKind: "agent", AuthorID: leader, Body: fmt.Sprintf("Please assess #%d", secret.No)}); err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.AgentMessageByNo(ctx, advisor, f.project.ID, secret.No); err != nil || !got.Available {
		t.Fatalf("cited task detail: %+v %v", got, err)
	}
	if got, err := f.s.AgentMessageByNo(ctx, advisor, f.project.ID, unrelated.No); err != nil || got.Available {
		t.Fatalf("unrelated task detail: %+v %v", got, err)
	}
	var count int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM agent a JOIN role r ON r.id=a.role_id WHERE a.project_id=? AND r.role_key='advisor' AND r.cardinality='singleton' AND r.cap_can_spawn=0`, f.project.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("advisor role: %d %v", count, err)
	}
	if err := f.s.Tx(ctx, func(tx *Tx) error {
		metered, err := meteredLiveAgents(ctx, tx.tx, f.project.ID, "", "leader-advisor")
		if err != nil || metered != 0 {
			t.Fatalf("core agents count toward cap: %d %v", metered, err)
		}
		if err := tx.flowActorMayTarget(f.project.ID, leader, "advisor"); !errors.Is(err, ErrTeamFlowRule) {
			t.Fatalf("leader spawned another advisor: %v", err)
		}
		if err := tx.flowActorMayTarget(f.project.ID, advisor, "worker"); !errors.Is(err, ErrTeamFlowRule) {
			t.Fatalf("advisor spawned worker: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAdvisorWakesOnlyWhenAddressed(t *testing.T) {
	s, project, room, worker, _ := wakeFixture(t)
	ctx := context.Background()
	advisor, err := s.CreateAgent(ctx, Agent{Slug: "advisoragent", ProjectID: project.ID, RoleID: "role_advisor_default", Tool: "fake", Variant: "test", Model: "test", Provider: "test", MachineID: "mch_wake", CheckoutID: "chk_wake", KeyHash: "key", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, room.ID, "agent", advisor.ID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind, author, body string
		wake               bool
	}{
		{"user", "owner", "status", false},
		{"agent", worker.ID, "status", false},
		{"user", "owner", "@advisor please advise", true},
		{"agent", worker.ID, "@advisor please advise", true},
	} {
		message, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: tc.kind, AuthorID: tc.author, Body: tc.body})
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.NoteForAgentMessage(ctx, advisor.ID, message.ID)
		if (err == nil) != tc.wake {
			t.Fatalf("%q wake=%t want=%t: %v", tc.body, err == nil, tc.wake, err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_advisor_dm','advisor-user','hash','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_advisor_dm','owner')`, project.ID); err != nil {
		t.Fatal(err)
	}
	dm, err := s.OpenDM(ctx, "usr_advisor_dm", advisor.ID)
	if err != nil {
		t.Fatal(err)
	}
	message, err := s.InsertMessage(ctx, Message{RoomID: dm.ID, AuthorKind: "user", AuthorID: "usr_advisor_dm", Body: "Can you advise privately?"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.NoteForAgentMessage(ctx, advisor.ID, message.ID); err != nil {
		t.Fatalf("human DM did not wake advisor: %v", err)
	}
}

func TestAdvisorMigrationPreservesCustomRole(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(t.TempDir(), "advisor.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE schema_version(version INTEGER PRIMARY KEY,applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := migrateUpTo(ctx, db, 111); err != nil {
		t.Fatal(err)
	}
	// Deleted global roles keep their role_key. The live advisor, not the
	// older tombstone, must move aside for the built-in role.
	if _, err := db.Exec(`INSERT INTO role(id,key,role_key,label,body,emoji,cardinality,deleted_at) VALUES ('role_old_advisor','deleted:role_old_advisor','advisor','Old advisor','old guidance','⭐','multi','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO role(id,key,role_key,label,body,emoji,cardinality) VALUES ('role_existing_advisor','advisor','advisor','Advisor','custom guidance','⭐','multi')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO role(id,key,role_key,label,body,emoji,cardinality,extends) VALUES ('role_child_advisor','advisor-child','advisor-child','Child','child','⭐','multi','advisor')`); err != nil {
		t.Fatal(err)
	}
	if err := migrateUpTo(ctx, db, 0); err != nil {
		t.Fatal(err)
	}
	var key, body, parent string
	if err := db.QueryRow(`SELECT role_key,body FROM role WHERE id='role_existing_advisor'`).Scan(&key, &body); err != nil {
		t.Fatal(err)
	}
	if key != "advisor-v1" || body != "custom guidance" {
		t.Fatalf("custom role: %s %s", key, body)
	}
	customKey := key
	if err := db.QueryRow(`SELECT role_key,body FROM role WHERE id='role_old_advisor'`).Scan(&key, &body); err != nil || key != "advisor" || body != "old guidance" {
		t.Fatalf("deleted advisor was changed: %s %s %v", key, body, err)
	}
	if err := db.QueryRow(`SELECT extends FROM role WHERE id='role_child_advisor'`).Scan(&parent); err != nil || parent != customKey {
		t.Fatalf("child parent: %s %v", parent, err)
	}
	if err := db.QueryRow(`SELECT body FROM role WHERE id='role_advisor_default'`).Scan(&body); err != nil || body != advisorRoleBody {
		t.Fatalf("built-in advisor: %s %v", body, err)
	}
}
