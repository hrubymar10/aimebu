package store

import (
	"context"
	"testing"
)

func noteReason(t *testing.T, s *Store, agentID, messageID string) string {
	t.Helper()
	var reason string
	err := s.db.QueryRow(`SELECT wake_reason FROM note WHERE agent_id=? AND message_id=?`, agentID, messageID).Scan(&reason)
	if err != nil {
		return ""
	}
	return reason
}

// A person's reply deep in a conversation an agent started wakes the author replied to, and also the agent that started it; an agent's reply
// in the same place wakes only the author replied to.
func TestReplyInAConversationWakesItsStarter(t *testing.T) {
	t.Parallel()
	s, _, room, one, lead := wakeFixture(t)
	ctx := context.Background()
	root, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: lead.ID, Body: "the plan"})
	if err != nil {
		t.Fatal(err)
	}
	answer, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: one.ID, Body: "a question on it", ReplyTo: root.ID})
	if err != nil {
		t.Fatal(err)
	}
	if answer.ThreadRoot != root.ID {
		t.Fatalf("a reply's thread_root = %q, want the root %q", answer.ThreadRoot, root.ID)
	}
	person, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "go ahead", ReplyTo: answer.ID})
	if err != nil {
		t.Fatal(err)
	}
	if person.ThreadRoot != root.ID {
		t.Fatalf("a reply to a reply stays in the root's conversation: %q", person.ThreadRoot)
	}
	if got := noteReason(t, s, one.ID, person.ID); got != "reply" {
		t.Fatalf("the author replied to: %q, want reply", got)
	}
	if got := noteReason(t, s, lead.ID, person.ID); got != "conversation" {
		t.Fatalf("the agent that started the conversation: %q, want conversation", got)
	}
	agentReply, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: one.ID, Body: "done", ReplyTo: person.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got := noteReason(t, s, lead.ID, agentReply.ID); got != "" {
		t.Fatalf("an agent's reply woke the conversation's starter (%q)", got)
	}
}

// The conversation wake only adds the starter when that is an agent other than the author replied to: replying straight to the starter is the
// ordinary reply (one note, not two), and a conversation a person started has no agent to add.
func TestConversationWakeNeverDoublesOrReachesAPerson(t *testing.T) {
	t.Parallel()
	s, _, room, one, lead := wakeFixture(t)
	ctx := context.Background()
	root, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: lead.ID, Body: "the plan"})
	if err != nil {
		t.Fatal(err)
	}
	direct, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "thanks", ReplyTo: root.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got := noteReason(t, s, lead.ID, direct.ID); got != "reply" {
		t.Fatalf("a reply straight to the starter: %q, want reply", got)
	}
	var notes int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM note WHERE agent_id=? AND message_id=?`, lead.ID, direct.ID).Scan(&notes); err != nil || notes != 1 {
		t.Fatalf("notes for the starter = %d, %v; want exactly one", notes, err)
	}
	started, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "my own question"})
	if err != nil {
		t.Fatal(err)
	}
	answer, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: one.ID, Body: "an answer", ReplyTo: started.ID})
	if err != nil {
		t.Fatal(err)
	}
	followUp, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "and one more thing", ReplyTo: answer.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got := noteReason(t, s, lead.ID, followUp.ID); got != "" {
		t.Fatalf("a conversation a person started woke an agent that is not in it: %q", got)
	}
}

// A starter that a person also @mentions in a reply deeper in its conversation keeps the stronger reason: one note, why "mention".
func TestConversationStarterThatIsMentionedKeepsMention(t *testing.T) {
	t.Parallel()
	s, _, room, one, lead := wakeFixture(t)
	ctx := context.Background()
	root, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: lead.ID, Body: "the plan"})
	if err != nil {
		t.Fatal(err)
	}
	answer, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: one.ID, Body: "on it", ReplyTo: root.ID})
	if err != nil {
		t.Fatal(err)
	}
	deep, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "@lead please look at this", ReplyTo: answer.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got := noteReason(t, s, lead.ID, deep.ID); got != "mention" {
		t.Fatalf("the starter was mentioned in a deeper reply: reason %q, want mention", got)
	}
	var notes int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM note WHERE agent_id=? AND message_id=?`, lead.ID, deep.ID).Scan(&notes); err != nil || notes != 1 {
		t.Fatalf("notes for the starter = %d, %v; want exactly one", notes, err)
	}
}

// A thread_root a caller passes in is never believed: a message with none and no reply_to starts its own conversation, and a reply takes its parent's.
func TestInsertIgnoresACallersThreadRoot(t *testing.T) {
	t.Parallel()
	s, _, room, _, lead := wakeFixture(t)
	ctx := context.Background()
	root, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: lead.ID, Body: "root"})
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "stale", ThreadRoot: root.ID})
	if err != nil {
		t.Fatal(err)
	}
	if m.ReplyTo != "" || m.ThreadRoot != "" {
		t.Fatalf("reply_to=%q thread_root=%q, want both empty: the conversation follows reply_to", m.ReplyTo, m.ThreadRoot)
	}
}

// The reply migration (98) turns thread-only messages into replies to their root and gives every reply its chain's first message as thread_root.
func TestReplyMigrationDerivesConversations(t *testing.T) {
	t.Parallel()
	s, project, room, _, lead := wakeFixture(t)
	ctx := context.Background()
	insert := func(body string) Message {
		t.Helper()
		m, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: lead.ID, Body: body})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	root, threadOnly, reply, deep := insert("root"), insert("thread only"), insert("reply"), insert("deep")
	// A thread root that was itself a reply (starter -> grand), with two old thread-only children: the conversation follows the chain, so they end up
	// in the grand message's conversation, not the starter's.
	grand, starter, child, childTwo := insert("grand"), insert("starter"), insert("thread child"), insert("thread child two")
	// A parent in another room: the chain does not cross rooms, so the row keeps what it had, and its own replies start from it.
	other, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "task", Title: "elsewhere", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := s.InsertMessage(ctx, Message{RoomID: other.ID, AuthorKind: "agent", AuthorID: lead.ID, Body: "foreign"})
	if err != nil {
		t.Fatal(err)
	}
	crossReply, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: lead.ID, Body: "answers another room"})
	if err != nil {
		t.Fatal(err)
	}
	afterCross := insert("after the cross-room reply")
	// The rows as they were before the change: a thread message without reply_to, and plain replies without thread_root (reply_to always
	// names an existing message: it is a foreign key).
	rewindWorkerRecoveryMigration(t, s)
	for _, statement := range [][]any{
		{`DROP TABLE message_reference_grant`},
		{`DROP INDEX room_team_flow_key`},
		{`ALTER TABLE room DROP COLUMN restricted`},
		{`ALTER TABLE room DROP COLUMN team_flow_room_key`},
		{`ALTER TABLE project DROP COLUMN team_flow_review_pending`},
		{`ALTER TABLE project DROP COLUMN team_flow_rev`},
		{`ALTER TABLE project DROP COLUMN team_flow_key`},
		{`UPDATE message SET reply_to=NULL, thread_root=? WHERE id=?`, root.ID, threadOnly.ID},
		{`UPDATE message SET reply_to=?, thread_root=NULL WHERE id=?`, threadOnly.ID, reply.ID},
		{`UPDATE message SET reply_to=?, thread_root=NULL WHERE id=?`, reply.ID, deep.ID},
		{`UPDATE message SET reply_to=?, thread_root=NULL WHERE id=?`, grand.ID, starter.ID},
		{`UPDATE message SET reply_to=NULL, thread_root=? WHERE id IN (?,?)`, starter.ID, child.ID, childTwo.ID},
		{`UPDATE message SET reply_to=?, thread_root=NULL WHERE id=?`, foreign.ID, crossReply.ID},
		{`UPDATE message SET reply_to=?, thread_root=NULL WHERE id=?`, crossReply.ID, afterCross.ID},
		{`ALTER TABLE message DROP COLUMN switch_handoff_json`},
		{`ALTER TABLE team_flow_migration DROP COLUMN drain_errors`},
		{`ALTER TABLE settings_project DROP COLUMN reviewer_agents`},
		{`DROP INDEX message_reply_to`},
		{`ALTER TABLE boundary_pending DROP COLUMN start_fresh`},
		{`ALTER TABLE harness_model DROP COLUMN available`},
		{`ALTER TABLE harness_model DROP COLUMN source`},
		{`ALTER TABLE agent DROP COLUMN approved_by`},
		{`DELETE FROM schema_version WHERE version>=98`},
	} {
		if _, err := s.db.Exec(statement[0].(string), statement[1:]...); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateUpTo(ctx, s.db, 0); err != nil {
		t.Fatal(err)
	}
	links := func(id string) (string, string) {
		var replyTo, threadRoot string
		if err := s.db.QueryRow(`SELECT COALESCE(reply_to,''),COALESCE(thread_root,'') FROM message WHERE id=?`, id).Scan(&replyTo, &threadRoot); err != nil {
			t.Fatal(err)
		}
		return replyTo, threadRoot
	}
	for _, c := range []struct{ id, replyTo, threadRoot string }{
		{root.ID, "", ""},
		{threadOnly.ID, root.ID, root.ID},
		{reply.ID, threadOnly.ID, root.ID},
		{deep.ID, reply.ID, root.ID},
		{grand.ID, "", ""},
		{starter.ID, grand.ID, grand.ID},
		{child.ID, starter.ID, grand.ID},
		{childTwo.ID, starter.ID, grand.ID},
		{foreign.ID, "", ""},
		{crossReply.ID, foreign.ID, ""},
		{afterCross.ID, crossReply.ID, crossReply.ID},
	} {
		if replyTo, threadRoot := links(c.id); replyTo != c.replyTo || threadRoot != c.threadRoot {
			t.Fatalf("%s: reply_to=%q thread_root=%q, want %q %q", c.id, replyTo, threadRoot, c.replyTo, c.threadRoot)
		}
	}
}
