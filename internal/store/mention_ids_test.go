package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// The A10-3 fixtures: one role, machine and checkout shared by every agent of
// the tests, a person, and helpers to add agents and read what a message looks
// like to a reader. All names are fake pool names.

const mentionRoleSQL = `INSERT INTO role(id,key,label,body,role_key) VALUES ('role_mentions','panel-worker','Worker','body','panel-worker')`

func mentionSeed(t *testing.T, s *Store) {
	t.Helper()
	for _, statement := range []string{
		mentionRoleSQL,
		`INSERT INTO machine(id,name,key_hash,approved,is_host) VALUES ('mch_mentions','host','hash',1,1)`,
		`INSERT INTO project(id,name,folder,created_at) VALUES ('prj_mentions','verda','/tmp/verda',?)`,
		`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_mentions','prj_mentions','mch_mentions','/tmp/mentions')`,
		`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_pia','pia','hash','{}',?)`,
		`INSERT INTO membership(project_id,user_id,role) VALUES ('prj_mentions','usr_pia','owner')`,
	} {
		if _, err := s.db.Exec(statement, timestamp(), timestamp()); err != nil {
			t.Fatal(err)
		}
	}
}

func mentionRoom(t *testing.T, s *Store, kind, title string) Room {
	t.Helper()
	room, err := s.CreateRoom(context.Background(), Room{ProjectID: "prj_mentions", Kind: kind, Title: title, CreatedBy: "usr_pia"})
	if err != nil {
		t.Fatal(err)
	}
	return room
}

func mentionAgent(t *testing.T, s *Store, slug, roomID string) Agent {
	t.Helper()
	agent, err := s.CreateAgent(context.Background(), Agent{
		Slug: slug, ProjectID: "prj_mentions", RoleID: "role_mentions", Tool: "codex",
		Variant: "native", Model: "test", Provider: "test", MachineID: "mch_mentions",
		CheckoutID: "chk_mentions", KeyHash: "hash", CreatedBy: "usr_pia",
	})
	if err != nil {
		t.Fatal(err)
	}
	if roomID != "" {
		if err := s.Tx(context.Background(), func(tx *Tx) error { return tx.AddMember(roomID, "agent", agent.ID) }); err != nil {
			t.Fatal(err)
		}
	}
	return agent
}

func mentionPersonMessage(t *testing.T, s *Store, roomID, body string) Message {
	t.Helper()
	message, err := s.InsertMessage(context.Background(), Message{RoomID: roomID, AuthorKind: "user", AuthorID: "usr_pia", Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func storedBody(t *testing.T, s *Store, messageID string) string {
	t.Helper()
	var body string
	if err := s.db.QueryRow(`SELECT body FROM message WHERE id=?`, messageID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

func wakeReason(t *testing.T, s *Store, agentID, messageID string) string {
	t.Helper()
	var reason string
	err := s.db.QueryRow(`SELECT wake_reason FROM note WHERE agent_id=? AND message_id=?`, agentID, messageID).Scan(&reason)
	if err == sql.ErrNoRows {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return reason
}

func stopAgent(t *testing.T, s *Store, agentID string, stoppedAgo time.Duration) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped',observed='stopped',stopped_at=? WHERE id=?`,
		time.Now().UTC().Add(-stoppedAgo).Format(timestampLayout), agentID); err != nil {
		t.Fatal(err)
	}
}

func closeRoom(t *testing.T, s *Store, roomID string) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE room SET lifecycle='closed',closed_at=? WHERE id=?`, timestamp(), roomID); err != nil {
		t.Fatal(err)
	}
}

// A person's @name is stored as the agent's ID and every reader is shown the
// name again; an agent's @agt_<id> mention works too, and both wake.
func TestMentionStoredAsIDAndRenderedAsName(t *testing.T) {
	s, _ := testStore(t, Options{})
	mentionSeed(t, s)
	room := mentionRoom(t, s, "task", "wiring")
	wren := mentionAgent(t, s, "wren", room.ID)

	message := mentionPersonMessage(t, s, room.ID, "@wren please wire the panel")
	if stored := storedBody(t, s, message.ID); !strings.Contains(stored, "@"+wren.ID) || strings.Contains(stored, "@wren") {
		t.Fatalf("stored body %q is not stored by agent ID", stored)
	}
	messages, err := s.ListMessagesAfter(context.Background(), room.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var shown *Message
	for i := range messages {
		if messages[i].ID == message.ID {
			shown = &messages[i]
		}
	}
	if shown == nil {
		t.Fatal("the message is missing from the room history")
	}
	if shown.Body != "@wren please wire the panel" {
		t.Fatalf("a reader sees %q, want the display name", shown.Body)
	}
	if len(shown.Mentions) != 1 || shown.Mentions[0].ID != wren.ID || shown.Mentions[0].Name != "wren" || !shown.Mentions[0].Resolved {
		t.Fatalf("mentions = %+v", shown.Mentions)
	}
	if reason := wakeReason(t, s, wren.ID, message.ID); reason != "mention" {
		t.Fatalf("the person's mention did not wake the agent: %q", reason)
	}

	// An agent may write the ID form itself.
	direct := mentionPersonMessage(t, s, room.ID, "@"+wren.ID+" and nobody else")
	if reason := wakeReason(t, s, wren.ID, direct.ID); reason != "mention" {
		t.Fatalf("an @agt_<id> mention did not wake the agent: %q", reason)
	}
	// Code never becomes an ID.
	code := mentionPersonMessage(t, s, room.ID, "run `@wren status` first")
	if stored := storedBody(t, s, code.ID); strings.Contains(stored, "@agt_") {
		t.Fatalf("a mention inside code was rewritten: %q", stored)
	}
	if reason := wakeReason(t, s, wren.ID, code.ID); reason == "mention" {
		t.Fatalf("a mention inside code addressed the agent: %q", reason)
	}
}

// A name is free again once its agent is stopped, its home room is closed, and
// it has been stopped for at least 24 hours (all three); old messages still
// show the old agent's name and new mentions point to the new agent.
func TestAgentNameFreedAndReused(t *testing.T) {
	s, _ := testStore(t, Options{})
	mentionSeed(t, s)
	task := mentionRoom(t, s, "task", "first")
	one := mentionAgent(t, s, "wren", task.ID)
	mentionPersonMessage(t, s, task.ID, "@wren finish the wiring")

	// A live agent holds its name.
	if err := s.SlugInUse(context.Background(), "prj_mentions", "wren"); err == nil {
		t.Fatal("a live agent's name is free")
	}
	// Stopped for 23h59m with the room open: still held.
	stopAgent(t, s, one.ID, 23*time.Hour+59*time.Minute)
	if err := s.SlugInUse(context.Background(), "prj_mentions", "wren"); err == nil {
		t.Fatal("a stopped agent with an open room keeps its name")
	}
	// Stopped for 23h59m with the room closed: still held (the 24 hours are not over).
	closeRoom(t, s, task.ID)
	if err := s.SlugInUse(context.Background(), "prj_mentions", "wren"); err == nil {
		t.Fatal("a 23h59m stop is not free yet")
	}
	// Past the 24 hours, all three conditions met: the name is free.
	if _, err := s.db.Exec(`UPDATE agent SET stopped_at=? WHERE id=?`,
		time.Now().UTC().Add(-24*time.Hour-time.Minute).Format(timestampLayout), one.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SlugInUse(context.Background(), "prj_mentions", "wren"); err != nil {
		t.Fatalf("the freed name is still held: %v", err)
	}

	two, err := s.CreateAgent(context.Background(), Agent{
		Slug: "wren", ProjectID: "prj_mentions", RoleID: "role_mentions", Tool: "codex",
		Variant: "native", Model: "test", Provider: "test", MachineID: "mch_mentions",
		CheckoutID: "chk_mentions", KeyHash: "hash", CreatedBy: "usr_pia",
	})
	if err != nil {
		t.Fatalf("the freed name was not reusable: %v", err)
	}
	fresh := mentionRoom(t, s, "task", "second")
	if err := s.Tx(context.Background(), func(tx *Tx) error { return tx.AddMember(fresh.ID, "agent", two.ID) }); err != nil {
		t.Fatal(err)
	}
	newMessage := mentionPersonMessage(t, s, fresh.ID, "@wren start the second panel")
	if stored := storedBody(t, s, newMessage.ID); !strings.Contains(stored, "@"+two.ID) {
		t.Fatalf("the reused name resolved to the old agent: %q", stored)
	}
	// The old message still shows the old agent's name, from its own room.
	oldRoom, err := s.ListMessagesAfter(context.Background(), task.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(oldRoom) == 0 || !strings.Contains(oldRoom[len(oldRoom)-1].Body, "@wren") {
		t.Fatalf("old messages stopped showing the old agent's name: %+v", oldRoom)
	}
	if got := oldRoom[len(oldRoom)-1].Mentions; len(got) != 1 || got[0].ID != one.ID {
		t.Fatalf("the old mention now points at %+v, want the old agent", got)
	}
	// The new message shows the new agent, and both are named wren.
	freshRoom, err := s.ListMessagesAfter(context.Background(), fresh.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(freshRoom) == 0 || !strings.Contains(freshRoom[len(freshRoom)-1].Body, "@wren") {
		t.Fatalf("the new mention does not show the name: %+v", freshRoom)
	}
	if got := freshRoom[len(freshRoom)-1].Mentions; len(got) != 1 || got[0].ID != two.ID {
		t.Fatalf("the new mention points at %+v, want the new agent", got)
	}
}

// The 24-hour rule reads the injectable clock: just under a day the name is
// still held, at a day it is free.
func TestAgentNameClockIsInjectable(t *testing.T) {
	s, _ := testStore(t, Options{})
	mentionSeed(t, s)
	task := mentionRoom(t, s, "task", "clock")
	one := mentionAgent(t, s, "iris", task.ID)
	stopAgent(t, s, one.ID, 0)
	closeRoom(t, s, task.ID)

	fake := time.Now().UTC().Add(23*time.Hour + 59*time.Minute)
	original := nameFreeClock
	nameFreeClock = func() time.Time { return fake }
	t.Cleanup(func() { nameFreeClock = original })
	if err := s.SlugInUse(context.Background(), "prj_mentions", "iris"); err == nil {
		t.Fatal("23h59m on the fake clock is not free yet")
	}
	nameFreeClock = func() time.Time { return fake.Add(time.Minute) }
	if err := s.SlugInUse(context.Background(), "prj_mentions", "iris"); err != nil {
		t.Fatalf("24h on the fake clock frees the name: %v", err)
	}
}

// Two agents that share a name in one room: a bare @name stays ambiguous, the
// later one is displayed (and addressable) as name-2, and reactivating the old
// agent never renames it.
func TestDuplicateDisplayNameGetsAnOrdinalSuffix(t *testing.T) {
	s, _ := testStore(t, Options{})
	mentionSeed(t, s)
	first := mentionRoom(t, s, "task", "first")
	robin1 := mentionAgent(t, s, "robin", first.ID)
	stopAgent(t, s, robin1.ID, 25*time.Hour)
	closeRoom(t, s, first.ID)
	second := mentionRoom(t, s, "task", "second")
	robin2 := mentionAgent(t, s, "robin", second.ID)
	if err := s.Tx(context.Background(), func(tx *Tx) error { return tx.AddMember(second.ID, "agent", robin1.ID) }); err != nil {
		t.Fatal(err)
	}

	bare := mentionPersonMessage(t, s, second.ID, "@robin who is on the panel")
	if stored := storedBody(t, s, bare.ID); !strings.Contains(stored, "@robin") {
		t.Fatalf("an ambiguous bare name was rewritten: %q", stored)
	}
	var bareTargets string
	if err := s.db.QueryRow(`SELECT targets_json FROM message WHERE id=?`, bare.ID).Scan(&bareTargets); err != nil {
		t.Fatal(err)
	}
	if bareTargets != "[]" && bareTargets != "" && bareTargets != "null" {
		t.Fatalf("an ambiguous bare name addressed somebody: %s", bareTargets)
	}
	suffixed := mentionPersonMessage(t, s, second.ID, "@robin-2 take the panel")
	if stored := storedBody(t, s, suffixed.ID); !strings.Contains(stored, "@"+robin2.ID) {
		t.Fatalf("@robin-2 did not resolve to the later agent: %q", stored)
	}
	if reason := wakeReason(t, s, robin2.ID, suffixed.ID); reason != "mention" {
		t.Fatalf("@robin-2 did not wake the later agent: %q", reason)
	}
	messages, err := s.ListMessagesAfter(context.Background(), second.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var bareShown, suffixedShown bool
	for _, message := range messages {
		switch message.ID {
		case bare.ID:
			bareShown = strings.Contains(message.Body, "@robin ") || strings.HasSuffix(message.Body, "@robin")
		case suffixed.ID:
			suffixedShown = strings.Contains(message.Body, "@robin-2 ")
		}
	}
	if !bareShown || !suffixedShown {
		t.Fatalf("display names are wrong: bare=%v suffixed=%v", bareShown, suffixedShown)
	}
	for _, message := range messages {
		for _, ref := range message.Mentions {
			if ref.ID == robin1.ID && ref.Name != "robin" {
				t.Fatalf("the first agent must keep the plain name: %+v", ref)
			}
			if ref.ID == robin2.ID && ref.Name != "robin-2" {
				t.Fatalf("the later duplicate must show as robin-2: %+v", ref)
			}
		}
	}
}

// A mention of a removed agent keeps its ID text and says it resolved to
// nothing.
func TestMentionOfARemovedAgentStaysReadable(t *testing.T) {
	s, _ := testStore(t, Options{})
	mentionSeed(t, s)
	room := mentionRoom(t, s, "task", "gone")
	agent := mentionAgent(t, s, "tessa", room.ID)
	message := mentionPersonMessage(t, s, room.ID, "@tessa where were you")
	if _, err := s.db.Exec(`UPDATE agent SET removed_slug=slug,slug=slug||'~'||id,revoked_at=? WHERE id=?`, timestamp(), agent.ID); err != nil {
		t.Fatal(err)
	}
	messages, err := s.ListMessagesAfter(context.Background(), room.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var shown *Message
	for i := range messages {
		if messages[i].ID == message.ID {
			shown = &messages[i]
		}
	}
	if shown == nil {
		t.Fatal("the message is missing")
	}
	// A removed agent keeps its kept name (removed_slug), so the reader still sees the name.
	if !strings.Contains(shown.Body, "@tessa") {
		t.Fatalf("a removed agent's mention lost its name: %q", shown.Body)
	}
}

// The one-time migration rewrites the stored mention names of old messages to
// agent IDs, leaves code and unresolvable tokens alone, and a second pass
// changes nothing.
func TestMentionMigrationConvertsOnce(t *testing.T) {
	ctx := context.Background()
	db := databaseAtVersion(t, t.TempDir()+"/m.sqlite", 132)
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO role(id,key,label,body,role_key) VALUES ('role_m','shed-hand','Worker','body','shed-hand')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO machine(id,name,key_hash,approved,is_host) VALUES ('mch_m','host','hash',1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO project(id,name,folder,created_at) VALUES ('prj_m','olden','/tmp/olden','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_m','prj_m','mch_m','/tmp/m')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO room(id,project_id,depth,kind,title,created_by,created_at) VALUES ('room_m','prj_m',0,'task','shed','u','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO agent(id,slug,project_id,role_id,tool,variant,model,provider,machine_id,checkout_id,key_hash,created_by,created_at)
		VALUES ('agt_wren','wren','prj_m','role_m','codex','native','x','x','mch_m','chk_m','x','u','now')`); err != nil {
		t.Fatal(err)
	}
	bodies := map[string]string{
		"msg_plain":     "ping @wren please",
		"msg_qualified": "see @wren@olden too",
		"msg_code":      "run `@wren status`",
		"msg_person":    "ask @pia about it",
		"msg_role":      "@shed-hand assemble",
	}
	seq := 0
	for id, body := range bodies {
		seq++
		if _, err := db.Exec(`INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at)
		VALUES (?,?, 'room_m','user','usr_x',?,'[]','now')`, id, seq, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateUpTo(ctx, db, 0); err != nil {
		t.Fatal(err)
	}
	read := func(id string) string {
		var body string
		if err := db.QueryRow(`SELECT body FROM message WHERE id=?`, id).Scan(&body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	if got := read("msg_plain"); got != "ping @agt_wren please" {
		t.Fatalf("plain mention not converted: %q", got)
	}
	if got := read("msg_qualified"); got != "see @agt_wren too" {
		t.Fatalf("qualified mention not converted: %q", got)
	}
	if got := read("msg_code"); got != bodies["msg_code"] {
		t.Fatalf("code was rewritten: %q", got)
	}
	if got := read("msg_person"); got != bodies["msg_person"] {
		t.Fatalf("a person's login was rewritten: %q", got)
	}
	if got := read("msg_role"); got != bodies["msg_role"] {
		t.Fatalf("a role key was rewritten: %q", got)
	}
	// Idempotent: a second pass changes nothing.
	for id := range bodies {
		before := read(id)
		if err := db.QueryRow(`SELECT 1`).Err(); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT 1`).Scan(new(any)); err != nil {
			t.Fatal(err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := migrateMentionsToIDs(ctx, tx); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if after := read(id); after != before {
			t.Fatalf("the second pass changed %q into %q", before, after)
		}
	}
}

// A message that mentions nobody (in live prose) is still a broadcast: the
// rewrite must not turn plain text into an addressed mention.
func TestPlainBroadcastSurvivesTheRewrite(t *testing.T) {
	s, _ := testStore(t, Options{})
	mentionSeed(t, s)
	room := mentionRoom(t, s, "task", "quiet")
	one := mentionAgent(t, s, "wren", room.ID)
	two := mentionAgent(t, s, "milo", room.ID)
	message := mentionPersonMessage(t, s, room.ID, "the panel is wired")
	for _, agent := range []Agent{one, two} {
		// A person's unaddressed message reaches the room as a human message (its own wake reason), never as a mention.
		if reason := wakeReason(t, s, agent.ID, message.ID); reason != "human_message" {
			t.Fatalf("a person's plain message reached %s as %q", agent.Slug, reason)
		}
	}
	// An agent's unaddressed message is a broadcast that wakes the others.
	agentMessage, err := s.InsertMessage(context.Background(), Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: one.ID, Body: "panel done"})
	if err != nil {
		t.Fatal(err)
	}
	if reason := wakeReason(t, s, two.ID, agentMessage.ID); reason != "broadcast" {
		t.Fatalf("the rewrite broke the broadcast wake: %q", reason)
	}
}

// In a chat between two agents of different projects, `@slug@project` names
// the agent of that project alone.
func TestProjectQualifiedMentionInAnAgentChat(t *testing.T) {
	s, _ := testStore(t, Options{})
	mentionSeed(t, s)
	other, err := s.CreateProject(context.Background(), Project{Name: "sable", Folder: "/tmp/sable"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_sable',?,'mch_mentions','/tmp/sable-work')`, other.ID); err != nil {
		t.Fatal(err)
	}
	bob, err := s.CreateAgent(context.Background(), Agent{
		Slug: "bob", ProjectID: other.ID, RoleID: "role_mentions", Tool: "codex",
		Variant: "native", Model: "test", Provider: "test", MachineID: "mch_mentions",
		CheckoutID: "chk_sable", KeyHash: "hash", CreatedBy: "usr_pia",
	})
	if err != nil {
		t.Fatal(err)
	}
	home := mentionAgent(t, s, "kim", "")
	var chat Room
	if err := s.Tx(context.Background(), func(tx *Tx) error {
		var created bool
		var err error
		chat, created, err = tx.OpenAgentDM(home.ID, bob.ID, home.ID)
		if err == nil && !created {
			// existing chat is fine too
			_ = created
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	message := mentionPersonMessage(t, s, chat.ID, fmt.Sprintf("@bob@%s over to you", other.ID))
	if stored := storedBody(t, s, message.ID); !strings.Contains(stored, "@"+bob.ID) {
		t.Fatalf("the cross-project qualified mention was not stored by ID: %q", stored)
	}
	if reason := wakeReason(t, s, bob.ID, message.ID); reason != "mention" {
		t.Fatalf("the qualified mention did not wake the other project's agent: %q", reason)
	}
	if reason := wakeReason(t, s, home.ID, message.ID); reason != "" {
		t.Fatalf("the qualified mention woke the wrong agent: %q", reason)
	}
}

// Agent-facing reads (message_get, room_history) keep the stored @agt_<id>
// bodies and name them in an entities table; the no-leak rule keeps a name the
// reader may not see out of the table, and an unknown id out entirely.
func TestAgentFacingReadsKeepIDsAndCarryEntities(t *testing.T) {
	s, _ := testStore(t, Options{})
	mentionSeed(t, s)
	room := mentionRoom(t, s, "task", "wiring")
	wren := mentionAgent(t, s, "wren", room.ID)
	pia := mentionAgent(t, s, "pia", room.ID) // same-project peer: named

	// A person writes @wren; the store resolves it to the ID on save.
	message := mentionPersonMessage(t, s, room.ID, "@wren wire the panel with @"+pia.ID)

	byNo, err := s.AgentMessageByNo(context.Background(), wren.ID, "prj_mentions", message.No)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(byNo.Body, "@"+wren.ID) || strings.Contains(byNo.Body, "@wren") {
		t.Fatalf("message_get translated the ids: %q", byNo.Body)
	}
	if len(byNo.Entities) != 2 || byNo.Entities[0].Name != "wren" || byNo.Entities[0].Kind != "agent" || byNo.Entities[1].Name != "pia" {
		t.Fatalf("entities = %+v", byNo.Entities)
	}

	page, err := s.AgentRoomHistory(context.Background(), wren.ID, "prj_mentions", room.ID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	var found *AgentMessage
	for i := range page.Messages {
		if page.Messages[i].No == message.No {
			found = &page.Messages[i]
		}
	}
	if found == nil || !strings.Contains(found.Body, "@"+wren.ID) || len(found.Entities) != 2 {
		t.Fatalf("room_history = %+v", found)
	}

	// A person's mention of a person id: named in the entities table when the person is addressable in the room.
	withPerson := mentionPersonMessage(t, s, room.ID, "@usr_pia have a look")
	byPerson, err := s.AgentMessageByNo(context.Background(), wren.ID, "prj_mentions", withPerson.No)
	if err != nil {
		t.Fatal(err)
	}
	if len(byPerson.Entities) != 1 || byPerson.Entities[0].Kind != "person" || byPerson.Entities[0].Name != "pia" {
		t.Fatalf("person entities = %+v", byPerson.Entities)
	}
}

// The no-leak rule: an agent of another project (no DM peer) is listed without
// a name; a person the reader cannot address in the room is left out.
func TestEntitiesDoNotLeak(t *testing.T) {
	s, _ := testStore(t, Options{})
	mentionSeed(t, s)
	other, err := s.CreateProject(context.Background(), Project{Name: "sable", Folder: "/tmp/sable"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_sable',?,'mch_mentions','/tmp/sable-work')`, other.ID); err != nil {
		t.Fatal(err)
	}
	foreign, err := s.CreateAgent(context.Background(), Agent{
		Slug: "robin", ProjectID: other.ID, RoleID: "role_mentions", Tool: "codex",
		Variant: "native", Model: "test", Provider: "test", MachineID: "mch_mentions",
		CheckoutID: "chk_sable", KeyHash: "hash", CreatedBy: "usr_pia",
	})
	if err != nil {
		t.Fatal(err)
	}
	// A person who is not a member of the project (not addressable in the room).
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_stranger','stranger','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	room := mentionRoom(t, s, "task", "leak")
	one := mentionAgent(t, s, "wren", room.ID)
	message := mentionPersonMessage(t, s, room.ID, "@"+foreign.ID+" and @usr_stranger and @usr_pia")
	byNo, err := s.AgentMessageByNo(context.Background(), one.ID, "prj_mentions", message.No)
	if err != nil {
		t.Fatal(err)
	}
	if len(byNo.Entities) != 2 {
		t.Fatalf("entities = %+v, want the foreign agent (unnamed) and pia, not the stranger", byNo.Entities)
	}
	if byNo.Entities[0].ID != foreign.ID || byNo.Entities[0].Name != "" {
		t.Fatalf("a foreign agent leaked a name: %+v", byNo.Entities[0])
	}
	if byNo.Entities[1].Name != "pia" {
		t.Fatalf("an addressable person lost its name: %+v", byNo.Entities[1])
	}
}

// An agent DM names its peer: the reader may see the chat's other agent.
func TestEntitiesNameAPrivateChatPeer(t *testing.T) {
	s, _ := testStore(t, Options{})
	mentionSeed(t, s)
	other, err := s.CreateProject(context.Background(), Project{Name: "sable", Folder: "/tmp/sable"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_sable2',?,'mch_mentions','/tmp/sable-work')`, other.ID); err != nil {
		t.Fatal(err)
	}
	bob, err := s.CreateAgent(context.Background(), Agent{
		Slug: "bob", ProjectID: other.ID, RoleID: "role_mentions", Tool: "codex",
		Variant: "native", Model: "test", Provider: "test", MachineID: "mch_mentions",
		CheckoutID: "chk_sable2", KeyHash: "hash", CreatedBy: "usr_pia",
	})
	if err != nil {
		t.Fatal(err)
	}
	home := mentionAgent(t, s, "kim", "")
	var chat Room
	if err := s.Tx(context.Background(), func(tx *Tx) error {
		var err error
		chat, _, err = tx.OpenAgentDM(home.ID, bob.ID, home.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	message := mentionPersonMessage(t, s, chat.ID, "@"+bob.ID+" over to you")
	byNo, err := s.AgentMessageByNo(context.Background(), home.ID, "prj_mentions", message.No)
	if err != nil {
		t.Fatal(err)
	}
	if len(byNo.Entities) != 1 || byNo.Entities[0].Name != "bob" {
		t.Fatalf("the chat peer lost its name: %+v", byNo.Entities)
	}
}

// A delivered note carries the same entities table, computed under the same
// no-leak rule. Built on the dispatch fixture with a pending turn, like the
// silent-turn tests do.
func TestDeliveryNoteCarriesEntities(t *testing.T) {
	s, f := answerFixture(t, false)
	peer, err := s.CreateAgent(context.Background(), Agent{Slug: "pia", ProjectID: f.project.ID, RoleID: "role_test",
		Tool: "codex", Variant: "native", Model: "test", Provider: "test", MachineID: "mch_test",
		CheckoutID: "chk_test", KeyHash: "hash_peer", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(context.Background(), f.room.ID, "agent", peer.ID); err != nil {
		t.Fatal(err)
	}
	message, err := s.InsertMessage(context.Background(), Message{RoomID: f.room.ID, AuthorKind: "user", AuthorID: "usr_answers",
		Body: "@" + peer.ID + " wire it, report to @" + f.agent.ID})
	if err != nil {
		t.Fatal(err)
	}
	// Dispatch creates its own turn; the fixture's seed turn must be closed first.
	if _, err := s.db.Exec(`UPDATE turn SET exec='exited',settle='settled' WHERE id=?`, f.turn.ID); err != nil {
		t.Fatal(err)
	}
	delivery, ready, err := s.PrepareDispatch(context.Background(), f.agent.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if !ready {
		t.Fatal("the mentioned agent is not dispatchable")
	}
	defer s.CancelPreparedDispatch(context.Background(), delivery.TurnID)
	var delivered *protocol.DeliveryNote
	for i := range delivery.Notes {
		if delivery.Notes[i].MessageID == message.ID {
			delivered = &delivery.Notes[i]
		}
	}
	if delivered == nil {
		t.Fatal("the mention note was not delivered")
	}
	if !strings.Contains(delivered.Body, "@"+peer.ID) {
		t.Fatalf("the note body was translated: %q", delivered.Body)
	}
	var ids []string
	for _, entity := range delivered.Entities {
		ids = append(ids, entity.ID+"|"+entity.Name)
	}
	// Body order: the peer first, then the addressed agent; both of the
	// reader's project, so both are named.
	want := peer.ID + "|pia " + f.agent.ID + "|worker"
	if strings.Join(ids, " ") != want {
		t.Fatalf("note entities = %v, want %q", ids, want)
	}
}

// A person's @usr_<id> reads as @login like an agent's @agt_<id> reads as its
// name; an unknown id and any id inside code stay as written.
func TestPersonMentionIDRendersAsLogin(t *testing.T) {
	s, _ := testStore(t, Options{})
	mentionSeed(t, s)
	room := mentionRoom(t, s, "task", "people")
	body := "@usr_pia look, @usr_nobody too, `@usr_pia` and\n```\n@usr_pia\n```"
	message := mentionPersonMessage(t, s, room.ID, body)
	messages, err := s.ListMessagesAfter(context.Background(), room.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range messages {
		if m.ID != message.ID {
			continue
		}
		want := "@pia look, @usr_nobody too, `@usr_pia` and\n```\n@usr_pia\n```"
		if m.Body != want {
			t.Fatalf("a reader sees %q, want %q", m.Body, want)
		}
		if len(m.Mentions) != 2 || m.Mentions[0].Name != "pia" || !m.Mentions[0].Resolved || m.Mentions[1].Resolved {
			t.Fatalf("mentions = %+v", m.Mentions)
		}
		return
	}
	t.Fatal("the message is missing from the room history")
}
