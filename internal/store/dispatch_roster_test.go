package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/goccy/go-json"
)

func TestDispatchContextTracksRoomRoster(t *testing.T) {
	t.Parallel()
	s, _, room, one, lead := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "@one handle this"}); err != nil {
		t.Fatal(err)
	}
	readContext := func() struct {
		YouAre         map[string]string   `json:"you_are"`
		TeamRoster     []map[string]string `json:"team_roster"`
		TeamRosterMore string              `json:"team_roster_more"`
	} {
		t.Helper()
		delivery, ready, err := s.PrepareDispatch(ctx, one.ID, "")
		if err != nil || !ready {
			t.Fatalf("prepare ready=%t err=%v", ready, err)
		}
		var data struct {
			YouAre         map[string]string   `json:"you_are"`
			TeamRoster     []map[string]string `json:"team_roster"`
			TeamRosterMore string              `json:"team_roster_more"`
		}
		if err := json.Unmarshal(delivery.Context, &data); err != nil {
			t.Fatal(err)
		}
		if err := s.CancelPreparedDispatch(ctx, delivery.TurnID); err != nil {
			t.Fatal(err)
		}
		return data
	}
	first := readContext()
	if first.YouAre["id"] != one.ID || first.YouAre["name"] != one.Slug || first.YouAre["role"] != "worker" || first.YouAre["harness"] != "fake" || first.YouAre["model"] != "test" {
		t.Fatalf("self identity=%v", first.YouAre)
	}
	if len(first.TeamRoster) != 2 || first.TeamRosterMore != "" || first.TeamRoster[0]["id"] != one.ID || first.TeamRoster[1]["id"] != lead.ID || first.TeamRoster[1]["role"] != "lead" {
		t.Fatalf("initial roster=%v", first.TeamRoster)
	}
	if err := s.RemoveMember(ctx, room.ID, "agent", lead.ID); err != nil {
		t.Fatal(err)
	}
	second := readContext()
	if len(second.TeamRoster) != 1 || second.TeamRoster[0]["id"] != one.ID || second.YouAre["id"] != one.ID {
		t.Fatalf("updated roster=%v self=%v", second.TeamRoster, second.YouAre)
	}
}

func TestDispatchContextListsRestrictedMembershipAndRoutingInstructions(t *testing.T) {
	s, project, room, _, leader := wakeFixture(t)
	ctx := context.Background()
	routing, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "task", Title: "routing", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, routing.ID, "agent", leader.ID); err != nil {
		t.Fatal(err)
	}
	closed, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "task", Title: "closed task", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, closed.ID, "agent", leader.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE room SET lifecycle='closed' WHERE id=?`, closed.ID); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE agent SET role_id='role_leader_default' WHERE id='` + leader.ID + `'`,
		`UPDATE project SET team_flow_key='leader-dispatcher' WHERE id='` + project.ID + `'`,
		`UPDATE room SET team_flow_room_key='routing',restricted=1 WHERE id='` + routing.ID + `'`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "@lead please plan"}); err != nil {
		t.Fatal(err)
	}
	delivery, ready, err := s.PrepareDispatch(ctx, leader.ID, "")
	if err != nil || !ready {
		t.Fatalf("dispatch: %+v ready=%t err=%v", delivery, ready, err)
	}
	var contextData struct {
		RoleBody string              `json:"role_body"`
		Rooms    []map[string]string `json:"rooms"`
	}
	if err := json.Unmarshal(delivery.Context, &contextData); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range contextData.Rooms {
		if item["room_id"] == closed.ID {
			t.Fatalf("closed room in dispatch context: %+v", contextData.Rooms)
		}
		if item["room_id"] == routing.ID && item["title"] == "routing" && item["kind"] == "task" {
			found = true
		}
	}
	if !found || !strings.Contains(contextData.RoleBody, "routing ("+routing.ID+")") || !strings.Contains(contextData.RoleBody, "whether a reviewer is needed") {
		t.Fatalf("routing absent from context: %+v", contextData)
	}
}

func TestDispatcherFlowRoleGuidance(t *testing.T) {
	s, project, room, worker, dispatcher := wakeFixture(t)
	ctx := context.Background()
	for _, statement := range []string{
		`UPDATE project SET team_flow_key='leader-dispatcher' WHERE id='` + project.ID + `'`,
		`UPDATE agent SET role_id='role_dispatcher_default' WHERE id='` + dispatcher.ID + `'`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "@one and @lead, review the task"}); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		agent Agent
		want  string
	}{{worker, "report to the agent who briefed you"}, {dispatcher, "Add a reviewer only when the specification or leader asks"}} {
		delivery, ready, err := s.PrepareDispatch(ctx, item.agent.ID, "")
		if err != nil || !ready {
			t.Fatalf("dispatch %s: ready=%t err=%v", item.agent.ID, ready, err)
		}
		var contextData struct {
			RoleBody string `json:"role_body"`
		}
		if err := json.Unmarshal(delivery.Context, &contextData); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(contextData.RoleBody, item.want) {
			t.Fatalf("role body for %s: %q", item.agent.ID, contextData.RoleBody)
		}
	}
}

func TestDispatchContextCapsLargeRoomRoster(t *testing.T) {
	t.Parallel()
	s, _, room, one, lead := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO role(id,key,label,body,role_key) VALUES ('role_helper','helper','Helper','help','helper')`); err != nil {
		t.Fatal(err)
	}
	var assigningAgent, mentionedAgent, namedWorker Agent
	for i := 0; i < 23; i++ {
		slug := fmt.Sprintf("peer%02d", i)
		roleID := "role_helper"
		if i == 20 {
			slug = "worker"
			roleID = one.RoleID
		}
		agent, err := s.CreateAgent(ctx, Agent{Slug: slug, ProjectID: one.ProjectID, RoleID: roleID,
			Tool: "fake", Variant: "test", Model: "test", Provider: "test", MachineID: one.MachineID,
			CheckoutID: one.CheckoutID, KeyHash: "key", CreatedBy: "owner"})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AddMember(ctx, room.ID, "agent", agent.ID); err != nil {
			t.Fatal(err)
		}
		if i == 21 {
			assigningAgent = agent
		}
		if i == 22 {
			mentionedAgent = agent
		}
		if i == 20 {
			namedWorker = agent
		}
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: assigningAgent.ID,
		Body:              "@one coordinate with @peer22@wake, @worker, and @helper",
		AppendixPagesJSON: `[{"title":"Notes","body":"@peer19 appears only in the appendix"}]`}); err != nil {
		t.Fatal(err)
	}
	delivery, ready, err := s.PrepareDispatch(ctx, one.ID, "")
	if err != nil || !ready {
		t.Fatalf("prepare ready=%t err=%v", ready, err)
	}
	var data struct {
		YouAre         map[string]string   `json:"you_are"`
		TeamRoster     []map[string]string `json:"team_roster"`
		TeamRosterMore string              `json:"team_roster_more"`
	}
	if err := json.Unmarshal(delivery.Context, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.TeamRoster) != maxDispatchRoster || data.TeamRosterMore != "5 more" || data.YouAre["id"] != one.ID {
		t.Fatalf("roster length=%d more=%q self=%v", len(data.TeamRoster), data.TeamRosterMore, data.YouAre)
	}
	want := []string{one.ID, lead.ID, assigningAgent.ID, mentionedAgent.ID, namedWorker.ID}
	for i, id := range want {
		if data.TeamRoster[i]["id"] != id {
			t.Fatalf("priority roster=%v", data.TeamRoster[:len(want)])
		}
	}
	for i := len(want); i < len(data.TeamRoster); i++ {
		if got := data.TeamRoster[i]["name"]; got != fmt.Sprintf("peer%02d", i-len(want)) {
			t.Fatalf("remaining roster order at %d: %q", i, got)
		}
	}
}
