package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// ownerFixture is an execution fixture with a project owner person in it, the way recoveryFixture seeds one.
func noticeOwnerFixture(t *testing.T) (*Store, executionFixture, string) {
	t.Helper()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	if err := s.AddMember(ctx, f.room.ID, "agent", f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_notices','owner','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_notices','owner')`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	return s, f, "usr_notices"
}

// Server-written texts follow the ids-are-truth rule: the Inbox boundary question and the room notices name the agents
// they are about as @agt_<id> tokens and the rooms they refer to as #room_<id> tokens, so the entities table covers them
// and only the human view translates. A person's login in a notice stays a login.
func TestBoundaryQuestionStoresTheAgentTokenAndTheInboxTranslatesIt(t *testing.T) {
	t.Parallel()
	s, f, owner := noticeOwnerFixture(t)
	ctx := context.Background()
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.createBoundaryPending(f.project.ID, f.room.ID, f.agent.ID, f.agent.MachineID, "/tmp/asked", "spawn", "user", owner)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var options string
	if err := s.db.QueryRow(`SELECT options_json FROM inbox_item WHERE agent_id=? AND kind='approval'`, f.agent.ID).Scan(&options); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(options, "agent @"+f.agent.ID+" on machine "+f.agent.MachineID) {
		t.Fatalf("the stored question keeps bare ids: %q", options)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	var card *InboxEntry
	for i := range items {
		if items[i].AgentID == f.agent.ID && items[i].BoundaryPending {
			card = &items[i]
		}
	}
	if card == nil {
		t.Fatalf("no boundary card for the owner: %+v", items)
	}
	if !strings.Contains(card.Question, "agent @"+f.agent.Slug+" on machine") || strings.Contains(card.Question, f.agent.ID) {
		t.Fatalf("the card does not translate the token: %q", card.Question)
	}
}

func TestSystemNoticesStoreAgentAndRoomTokensAndNameThemForTheReader(t *testing.T) {
	t.Parallel()
	s, f, _ := noticeOwnerFixture(t)
	ctx := context.Background()
	// The actor needs lead capability to stop another agent, and a flow-less project makes the lead cap enough. A role
	// of its own, so the target's worker role stays lead-less.
	if _, err := s.db.Exec(`INSERT OR REPLACE INTO role(id,key,label,body,role_key,cap_is_lead) VALUES ('role_notice_lead','leader','Leader','body','leader',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET role_id='role_notice_lead' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE project SET team_flow_key='' WHERE id=?`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	peer, err := s.CreateAgent(ctx, Agent{Slug: "peer", ProjectID: f.project.ID, RoleID: "role_test", Tool: "codex", Variant: "native", Model: "test", Provider: "test", MachineID: f.agent.MachineID, CheckoutID: "chk_test", KeyHash: "hash_peer", CreatedBy: f.agent.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, f.room.ID, "agent", peer.ID); err != nil {
		t.Fatal(err)
	}
	var created Room
	if err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		created, err = tx.CreateTaskRoomAsTool(f.agent.ID, f.project.ID, f.room.ID, "Sub task", "")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.StopAgentAsTool(peer.ID, f.agent.ID, 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.CreateSchedule(Schedule{RoomID: f.room.ID, ProjectID: f.project.ID, CreatorKind: "agent", CreatorID: f.agent.ID,
			TargetAgentID: peer.ID, Body: "check the queue", Kind: "interval", Timezone: "UTC", IntervalSeconds: 300}, time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceSchedules(ctx, time.Now().Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var createdNotice, stoppedNotice, scheduledNotice, runNotice string
	rows, err := s.db.Query(`SELECT body FROM message WHERE room_id=? AND author_kind='system' ORDER BY seq`, f.room.ID)
	if err != nil {
		t.Fatal(err)
	}
	var bodies []string
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, body)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, body := range bodies {
		switch {
		case strings.HasSuffix(body, "created task room #"+created.ID):
			createdNotice = body
		case strings.Contains(body, ") stopped @"+peer.ID+" ("):
			stoppedNotice = body
		case strings.Contains(body, "scheduled a note for"):
			scheduledNotice = body
		case strings.Contains(body, "Scheduled by"):
			runNotice = body
		}
	}
	if createdNotice != "@"+f.agent.ID+" created task room #"+created.ID {
		t.Fatalf("created notice: %q", createdNotice)
	}
	if stoppedNotice != "@"+f.agent.ID+" (leader) stopped @"+peer.ID+" (worker)" {
		t.Fatalf("stopped notice: %q", stoppedNotice)
	}
	if !strings.Contains(scheduledNotice, "@"+f.agent.ID+" scheduled a note for @"+peer.ID+" (") {
		t.Fatalf("scheduled notice: %q", scheduledNotice)
	}
	if !strings.Contains(runNotice, "Scheduled by @"+f.agent.ID+" for @"+peer.ID+"; due ") {
		t.Fatalf("run notice: %q", runNotice)
	}
	entities, err := mentionEntities(ctx, s.db, f.agent.ID, f.project.ID, f.room.ID, createdNotice)
	if err != nil {
		t.Fatal(err)
	}
	var actor, room *MentionEntity
	for i := range entities {
		switch {
		case entities[i].Kind == "agent" && entities[i].ID == f.agent.ID:
			actor = &entities[i]
		case entities[i].Kind == "room" && entities[i].ID == created.ID:
			room = &entities[i]
		}
	}
	if actor == nil || actor.Name != f.agent.Slug {
		t.Fatalf("actor entity = %+v, want %s named %s", actor, f.agent.ID, f.agent.Slug)
	}
	var title string
	if err := s.db.QueryRow(`SELECT title FROM room WHERE id=?`, created.ID).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if room == nil || room.Name != title {
		t.Fatalf("room entity = %+v, want %s named %s", room, created.ID, title)
	}
}

// A schedule a person created keeps the login in its notices: person mentions are stored as written logins, and a login
// is unique, so no token is needed. The agent target still becomes a token.
func TestPersonScheduledNoteKeepsTheLoginAndTokensTheTarget(t *testing.T) {
	t.Parallel()
	s, f, owner := noticeOwnerFixture(t)
	ctx := context.Background()
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.CreateSchedule(Schedule{RoomID: f.room.ID, ProjectID: f.project.ID, CreatorKind: "user", CreatorID: owner,
			TargetAgentID: f.agent.ID, Body: "summarize the day", Kind: "interval", Timezone: "UTC", IntervalSeconds: 300}, time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var scheduled string
	if err := s.db.QueryRow(`SELECT body FROM message WHERE room_id=? AND author_kind='system' AND body LIKE '%scheduled a note for%' ORDER BY seq DESC LIMIT 1`, f.room.ID).Scan(&scheduled); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(scheduled, "owner scheduled a note for @"+f.agent.ID+" (") {
		t.Fatalf("scheduled notice: %q", scheduled)
	}
}
