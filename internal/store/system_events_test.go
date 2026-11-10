package store

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRecentSystemEventsFiltersKindsAndAccess(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	if err := s.Tx(ctx, func(tx *Tx) error {
		for _, kind := range []string{"provider_limit_detected", "reaction_changed", "room_member_joined", "note_queued"} {
			if _, err := tx.AppendEvent(Event{Kind: kind, ProjectID: f.project.ID, RoomID: f.room.ID, AgentID: f.agent.ID, PayloadJSON: "{}"}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	events, err := s.RecentSystemEvents(ctx, owner, 50)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, event := range events {
		kinds = append(kinds, event.Kind)
	}
	if len(events) < 2 || events[0].Kind != "room_member_joined" || events[1].Kind != "provider_limit_detected" {
		t.Fatalf("newest first and operational kinds only: %v", kinds)
	}
	for _, kind := range kinds {
		if kind == "reaction_changed" || kind == "note_queued" {
			t.Fatalf("chatty kind leaked: %v", kinds)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_outsider','outsider','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if outsider, err := s.RecentSystemEvents(ctx, "usr_outsider", 50); err != nil || len(outsider) != 0 {
		t.Fatalf("a user outside the project must see nothing: %+v %v", outsider, err)
	}
	if limited, err := s.RecentSystemEvents(ctx, owner, 1); err != nil || len(limited) != 1 {
		t.Fatalf("limit: %+v %v", limited, err)
	}
	if _, err := s.RecentSystemEvents(ctx, owner, 0); err == nil {
		t.Fatal("accepted a zero limit")
	}
}

// The loop guard is an event a person wants to see in the feed (the panel then decides how much of it to show).
func TestLoopGuardIsASystemEventKind(t *testing.T) {
	t.Parallel()
	if !IsSystemEventKind("loop_guard") {
		t.Fatal("loop_guard is not in the system events feed")
	}
}

func TestMessageDebugShowsAddressingNotesTurnsAndReceipts(t *testing.T) {
	t.Parallel()
	s, f, _ := recoveryFixture(t, false)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO receipt(id,turn_id,answer_rev,digest,result_json,created_at) VALUES ('rcv_debug',?,0,'d','{"result":"held","held_reason":"invalid"}',?)`, f.turn.ID, timestamp()); err != nil {
		t.Fatal(err)
	}
	debug, err := s.MessageDebug(ctx, f.message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if debug.Message["id"] != f.message.ID || debug.Message["room_id"] != f.room.ID {
		t.Fatalf("message row: %+v", debug.Message)
	}
	if len(debug.Notes) != 1 || debug.Notes[0].AgentID != f.agent.ID || debug.Notes[0].AgentName != "worker" ||
		debug.Notes[0].WakeReason != "human_message" || len(debug.Notes[0].Turns) != 1 || debug.Notes[0].Turns[0].ID != f.turn.ID {
		t.Fatalf("notes: %+v", debug.Notes)
	}
	if receipts := debug.Notes[0].Turns[0].Receipts; len(receipts) != 1 || string(receipts[0]) == "" {
		t.Fatalf("receipts: %+v", receipts)
	}
	if _, err := s.MessageDebug(ctx, "msg_missing"); err == nil {
		t.Fatal("missing message must error")
	}
}

func TestProjectlessEventsAreOwnerOnlyInHistoryAndStream(t *testing.T) {
	t.Parallel()
	s, f, _ := recoveryFixture(t, false)
	ctx := context.Background()
	if err := s.Tx(ctx, func(tx *Tx) error {
		for _, kind := range []string{"user_created", "machine_created", "agent_revoked", "auth_session_revoked"} {
			if _, err := tx.AppendEvent(Event{Kind: kind, AgentID: f.agent.ID, PayloadJSON: "{}"}); err != nil {
				return err
			}
		}
		_, err := tx.AppendEvent(Event{Kind: "room_member_joined", ProjectID: f.project.ID, RoomID: f.room.ID, PayloadJSON: "{}"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var owner, other string
	for _, id := range []string{"usr_test", "usr_recovery"} {
		isOwner, err := s.IsGlobalOwner(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if isOwner {
			owner = id
		} else {
			other = id
		}
	}
	if owner == "" || other == "" {
		t.Fatalf("fixture needs one owner and one other member: owner=%q other=%q", owner, other)
	}
	count := func(events []Event, kind string) int {
		n := 0
		for _, event := range events {
			if event.Kind == kind {
				n++
			}
		}
		return n
	}
	for _, who := range []struct {
		user    string
		allowed bool
	}{{owner, true}, {other, false}} {
		history, err := s.RecentSystemEvents(ctx, who.user, 100)
		if err != nil {
			t.Fatal(err)
		}
		stream, err := s.EventsAfter(ctx, who.user, 0, 1<<40)
		if err != nil {
			t.Fatal(err)
		}
		batch, err := s.EventsBatchAfter(ctx, who.user, 0, 1<<40, 5000)
		if err != nil {
			t.Fatal(err)
		}
		for name, events := range map[string][]Event{"history": history, "stream": stream, "batch": batch} {
			for _, kind := range []string{"user_created", "machine_created", "agent_revoked"} {
				if got := count(events, kind); who.allowed && got != 1 || !who.allowed && got != 0 {
					t.Errorf("%s: %s sees %d %s events, allowed=%t", name, who.user, got, kind, who.allowed)
				}
			}
			if count(events, "auth_session_revoked") != 0 {
				t.Errorf("%s: session bookkeeping must never be streamed or listed", name)
			}
			if member := who.user == "usr_recovery"; member && count(events, "room_member_joined") < 1 {
				t.Errorf("%s: the project member must still see the project event", name)
			}
		}
	}
}

func TestMessageDebugCapsBodyOnARuneBoundaryAndScopesEvents(t *testing.T) {
	t.Parallel()
	s, f, _ := recoveryFixture(t, false)
	ctx := context.Background()
	body := strings.Repeat("界", debugTextMax) // 3 bytes each, far over the cap
	if _, err := s.db.Exec(`UPDATE message SET body=? WHERE id=?`, body, f.message.ID); err != nil {
		t.Fatal(err)
	}
	debug, err := s.MessageDebug(ctx, f.message.ID)
	if err != nil {
		t.Fatal(err)
	}
	capped, _ := debug.Message["body"].(string)
	if len(capped) > debugTextMax+len("… (truncated)") || !utf8.ValidString(capped) || !strings.HasSuffix(capped, "… (truncated)") {
		t.Fatalf("body must be capped on a rune boundary: %d bytes valid=%t", len(capped), utf8.ValidString(capped))
	}
	elsewhere, err := s.CreateRoom(ctx, Room{ProjectID: f.project.ID, Kind: "task", Title: "elsewhere", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO event(id,seq,kind,project_id,room_id,payload_json,created_at) VALUES ('evt_other',(SELECT MAX(seq)+1 FROM event),'message_created',?,?,?,?)`, f.project.ID, elsewhere.ID, `{"message_id":"`+f.message.ID+`"}`, timestamp()); err != nil {
		t.Fatal(err)
	}
	debug, err = s.MessageDebug(ctx, f.message.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range debug.Events {
		if event.ID == "evt_other" {
			t.Fatal("an event from another room must not be attributed to this message")
		}
	}
}

func TestNameEventsResolvesAgentsAndUsers(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	events := []Event{
		{Kind: "provider_limit_detected", AgentID: f.agent.ID, PayloadJSON: "{}"},
		{Kind: "user_invite_redeemed", PayloadJSON: `{"user_id":"` + owner + `","by":"usr_unknown"}`},
		{Kind: "room_member_joined", AgentID: "agt_unknown", PayloadJSON: "not json but has _id\""},
		{Kind: "agent_control_requested", AgentID: f.agent.ID, PayloadJSON: `{"desired":"stopped","actor_kind":"user","actor_id":"` + owner + `"}`},
		{Kind: "agent_control_requested", AgentID: f.agent.ID, PayloadJSON: `{"desired":"active","actor":"` + owner + `"}`}, // what a recovery decision writes
	}
	if err := s.NameEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	agentName := events[0].Names[f.agent.ID]
	if agentName == "" || !strings.Contains(agentName, "@") {
		t.Fatalf("an agent is named slug@project: %q", agentName)
	}
	if events[1].Names[owner] != "owner" {
		t.Fatalf("a user is named by login: %+v", events[1].Names)
	}
	if _, ok := events[1].Names["usr_unknown"]; ok {
		t.Fatalf("an unknown id gets no name: %+v", events[1].Names)
	}
	if events[3].Names[owner] != "owner" {
		t.Fatalf("who started, stopped or paused an agent is named: %+v", events[3].Names)
	}
	if events[4].Names[owner] != "owner" {
		t.Fatalf("so is the actor of a recovery decision: %+v", events[4].Names)
	}
	if len(events[2].Names) != 0 {
		t.Fatalf("unknown agent and bad payload: %+v", events[2].Names)
	}
}
