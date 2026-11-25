package store

import (
	"database/sql"
	"errors"
	"testing"
)

func TestRemoveAgentNeedsAStoppedAgentAndTakesItOutForGood(t *testing.T) {
	t.Parallel()
	s, project, room, agent, lead := wakeFixture(t)
	ctx := t.Context()
	message, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: agent.ID, Body: "I was here"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveAgent(ctx, agent.ID, "owner"); !errors.Is(err, ErrAgentNotStopped) {
		t.Fatalf("an active agent must be refused: %v", err)
	}
	// One that is only asked to stop is not stopped yet: a process may still run.
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped',observed='interrupting' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveAgent(ctx, agent.ID, "owner"); !errors.Is(err, ErrAgentNotStopped) {
		t.Fatalf("an agent still stopping must be refused: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET observed='stopped' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	// A recovery decision waits on it, and a note is queued for it.
	if _, err := s.db.Exec(`INSERT INTO inbox_item(id,project_id,room_id,agent_id,requester_kind,requester_id,kind,options_json,created_at) VALUES ('inb_gone',?,?,?,'agent',?,'recovery','{}',?)`,
		project.ID, room.ID, agent.ID, agent.ID, timestamp()); err != nil {
		t.Fatal(err)
	}
	removed, err := s.RemoveAgent(ctx, agent.ID, "usr_owner")
	if err != nil || removed.Slug != "one" || removed.ProjectID != project.ID {
		t.Fatalf("removal = %+v err=%v", removed, err)
	}
	var left, revoked int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM room_member WHERE member_id=? AND left_at IS NULL`, agent.ID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("the agent should have left its rooms: %d err=%v", left, err)
	}
	if err := s.db.QueryRow(`SELECT revoked_at IS NOT NULL FROM agent WHERE id=?`, agent.ID).Scan(&revoked); err != nil || revoked != 1 {
		t.Fatalf("revoked=%d err=%v", revoked, err)
	}
	var state, by string
	if err := s.db.QueryRow(`SELECT state,COALESCE(resolved_by,'') FROM inbox_item WHERE id='inb_gone'`).Scan(&state, &by); err != nil || state != "withdrawn" || by != "agent removed by usr_owner" {
		t.Fatalf("the inbox item should be withdrawn: %q %q err=%v", state, by, err)
	}
	cards, err := s.AgentCardsForRoom(ctx, room.ID)
	if err != nil || len(cards) != 1 || cards[0].ID != lead.ID {
		t.Fatalf("only the other agent should be listed: %+v err=%v", cards, err)
	}
	// It is gone for any further removal, and its name and role's slot are free again.
	if _, err := s.RemoveAgent(ctx, agent.ID, "usr_owner"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a second removal should find nothing: %v", err)
	}
	if _, err := s.AgentRemovalInfo(ctx, agent.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a removed agent is not found: %v", err)
	}
	if taken, err := s.AgentSlugTaken(ctx, project.ID, "one"); err != nil || taken {
		t.Fatalf("the name should be free: taken=%v err=%v", taken, err)
	}
	// Its message stays, and names it as removed (the slug and its project, for the client to write slug@project).
	messages, err := s.ListMessagesAfter(ctx, room.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, m := range messages {
		if m.ID == message.ID {
			found = true
			if m.AuthorName != "one" || m.AuthorProjectID != project.ID {
				t.Fatalf("the message should name its removed author: %q %q", m.AuthorName, m.AuthorProjectID)
			}
		}
	}
	if !found {
		t.Fatal("the message should stay")
	}
}

func TestRemoveAgentClosesAChatBetweenAgentsThatIsLeftWithOne(t *testing.T) {
	t.Parallel()
	s, _, _, agent, lead := wakeFixture(t)
	ctx := t.Context()
	chat, err := s.CreateRoom(ctx, Room{ProjectID: AgentDMProjectID, Kind: "dm", Title: "one & lead", CreatedBy: "system"})
	if err != nil {
		t.Skipf("the fixture cannot make a chat between agents: %v", err)
	}
	for _, id := range []string{agent.ID, lead.ID} {
		if err := s.AddMember(ctx, chat.ID, "agent", id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped',observed='stopped' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveAgent(ctx, agent.ID, "usr_owner"); err != nil {
		t.Fatal(err)
	}
	var lifecycle string
	if err := s.db.QueryRow(`SELECT lifecycle FROM room WHERE id=?`, chat.ID).Scan(&lifecycle); err != nil || lifecycle != "closed" {
		t.Fatalf("the chat should be closed: %q err=%v", lifecycle, err)
	}
}

// What aimed at an agent that is removed is stopped, and what shows its name shows the name it had, not the tombstone its row now carries.
func TestRemoveAgentPausesItsSchedulesAndShowsItsOldName(t *testing.T) {
	t.Parallel()
	s, project, room, agent, lead := wakeFixture(t)
	ctx := t.Context()
	message, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: agent.ID, Body: "I was here"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error { return tx.SetReaction(message.ID, "agent", agent.ID, "👍", true) }); err != nil {
		t.Fatal(err)
	}
	insert := func(id string, target any, enabled int) {
		t.Helper()
		if _, err := s.db.Exec(`INSERT INTO schedule(id,room_id,project_id,creator_kind,creator_id,target_agent_id,target_lead,body,kind,timezone,interval_seconds,enabled,next_run_at,created_at,updated_at)
			VALUES (?,?,?,'agent',?,?,?,'ping','interval','UTC',3600,?,?,?,?)`, id, room.ID, project.ID, agent.ID, target, map[bool]int{true: 1}[target == nil], enabled, timestamp(), timestamp(), timestamp()); err != nil {
			t.Fatal(err)
		}
	}
	insert("sch_aimed", agent.ID, 1)
	insert("sch_aimed_off", agent.ID, 0)
	insert("sch_lead", nil, 1)
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped',observed='stopped' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveAgent(ctx, agent.ID, "usr_owner"); err != nil {
		t.Fatal(err)
	}
	enabled := func(id string) (on, rev int) {
		t.Helper()
		if err := s.db.QueryRow(`SELECT enabled,rev FROM schedule WHERE id=?`, id).Scan(&on, &rev); err != nil {
			t.Fatal(err)
		}
		return on, rev
	}
	if on, rev := enabled("sch_aimed"); on != 0 || rev != 1 {
		t.Fatalf("a schedule aimed at the removed agent should be paused: enabled=%d rev=%d", on, rev)
	}
	if on, rev := enabled("sch_aimed_off"); on != 0 || rev != 0 {
		t.Fatalf("one that was paused already is left alone: enabled=%d rev=%d", on, rev)
	}
	if on, rev := enabled("sch_lead"); on != 1 || rev != 0 {
		t.Fatalf("a schedule aimed at the lead is not the removed agent's: enabled=%d rev=%d", on, rev)
	}
	// The stored notice names the removed agent by its @agt_<id> token; the mention display and scheduleCreator are what show its old name.
	var notice int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM message WHERE room_id=? AND author_kind='system' AND body=?`, room.ID, "Schedule sch_aimed paused: @"+agent.ID+" was removed").Scan(&notice); err != nil || notice != 1 {
		t.Fatalf("the room should be told: %d err=%v", notice, err)
	}
	entities, err := mentionEntities(ctx, s.db, lead.ID, project.ID, room.ID, "Schedule sch_aimed paused: @"+agent.ID+" was removed")
	if err != nil || len(entities) != 1 || entities[0].ID != agent.ID || entities[0].Name != "one" {
		t.Fatalf("the removed agent is named by its kept old name: %+v %v", entities, err)
	}
	// Names: the reaction's hover name and a schedule's creator show the name it had.
	reactions, err := s.ReactionsForMessage(ctx, message.ID)
	if err != nil || len(reactions) != 1 || reactions[0]["member_name"] != "one" {
		t.Fatalf("reactions = %+v err=%v", reactions, err)
	}
	var creator string
	if err := s.Tx(ctx, func(tx *Tx) (err error) { creator, err = tx.scheduleCreator(Schedule{CreatorID: agent.ID}); return err }); err != nil || creator != "one" {
		t.Fatalf("a schedule's creator shows %q err=%v", creator, err)
	}
}
