package store

import (
	"context"
	"fmt"
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
