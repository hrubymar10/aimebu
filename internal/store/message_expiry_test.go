package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/goccy/go-json"
)

// Message expiry at delivery: a note that names the agent is always delivered; a
// room-wide note older than the window is dropped (never deleted) and the turn says so.

func ageMessage(t *testing.T, s *Store, messageID string, age time.Duration) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE message SET created_at=? WHERE id=?`, time.Now().UTC().Add(-age).Format(timestampLayout), messageID); err != nil {
		t.Fatal(err)
	}
}

type expiryDelivery struct {
	Ready   bool
	Bodies  []string
	Expired map[string]any
}

// deliveryFor prepares the agent's next turn and reads what it carries, then cancels
// it so the same notes can be looked at again.
func deliveryFor(t *testing.T, s *Store, agentID string) expiryDelivery {
	t.Helper()
	delivery, ready, err := s.PrepareDispatch(context.Background(), agentID, "")
	if err != nil {
		t.Fatal(err)
	}
	out := expiryDelivery{Ready: ready}
	if !ready {
		return out
	}
	for _, note := range delivery.Notes {
		out.Bodies = append(out.Bodies, note.Body)
	}
	var data struct {
		Expired map[string]any `json:"expired"`
	}
	if err := json.Unmarshal(delivery.Context, &data); err != nil {
		t.Fatal(err)
	}
	out.Expired = data.Expired
	if err := s.CancelPreparedDispatch(context.Background(), delivery.TurnID); err != nil {
		t.Fatal(err)
	}
	return out
}

func noteState(t *testing.T, s *Store, agentID, messageID string) string {
	t.Helper()
	var state string
	if err := s.db.QueryRow(`SELECT state FROM note WHERE agent_id=? AND message_id=?`, agentID, messageID).Scan(&state); err != nil {
		t.Fatalf("note for %s: %v", messageID, err)
	}
	return state
}

func TestOldAIGroupNotesAreDroppedAndEverythingElseIsAlwaysDelivered(t *testing.T) {
	t.Parallel()
	s, _, room, one, two := wakeFixture(t)
	ctx := context.Background()
	human := func(body string) Message {
		m, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: body})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	ai := func(body string) Message {
		m, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: two.ID, Body: body})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	humanOld := human("old untargeted human message")      // a human ask to the room: human_message, never expired
	humanGroupOld := human("@channel old human broadcast") // a human's own group tag is never expired either
	ambientOld := ai("@channel first old AI chatter")      // AI group chatter
	groupOld := ai("@channel second old AI chatter")
	mentionOld := ai("@one please do the old thing") // names the agent
	ambientNew := ai("@channel fresh AI chatter")
	for _, m := range []Message{humanOld, humanGroupOld, ambientOld, groupOld, mentionOld} {
		ageMessage(t, s, m.ID, 7*time.Hour)
	}

	got := deliveryFor(t, s, one.ID)
	if !got.Ready {
		t.Fatal("the agent has live and addressed notes and must be woken")
	}
	want := []string{"old untargeted human message", "@channel old human broadcast", "@one please do the old thing", "@channel fresh AI chatter"}
	if fmt.Sprint(got.Bodies) != fmt.Sprint(want) {
		t.Fatalf("delivered %q, want %q: human messages and addressed notes are delivered however old, old AI group chatter is not", got.Bodies, want)
	}
	if noteState(t, s, one.ID, ambientOld.ID) != "dropped" || noteState(t, s, one.ID, groupOld.ID) != "dropped" {
		t.Fatal("old AI group notes must be dropped")
	}
	for _, m := range []Message{humanOld, humanGroupOld, mentionOld, ambientNew} {
		if noteState(t, s, one.ID, m.ID) != "queued" {
			t.Fatalf("delivered note %q was touched", m.Body)
		}
	}
	// Nothing is deleted: the messages are all still there.
	var kept int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM message WHERE id IN (?,?,?,?,?,?)`, humanOld.ID, humanGroupOld.ID, ambientOld.ID, groupOld.ID, mentionOld.ID, ambientNew.ID).Scan(&kept); err != nil || kept != 6 {
		t.Fatalf("expiry must not delete messages: %d %v", kept, err)
	}
	// The marker says how many were left out, which range, before when, and where to look.
	if got.Expired == nil || got.Expired["hidden_count"] != float64(2) || got.Expired["oldest_id"] != ambientOld.ID || got.Expired["newest_expired_id"] != groupOld.ID {
		t.Fatalf("marker = %+v", got.Expired)
	}
	if hint, _ := got.Expired["hint"].(string); hint == "" || got.Expired["expired_before"] == "" {
		t.Fatalf("marker lacks its hint or cutoff: %+v", got.Expired)
	}
	var dropEvents int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE kind='notes_dropped' AND json_extract(payload_json,'$.reason')='expired' AND json_extract(payload_json,'$.count')='2'`).Scan(&dropEvents); err != nil || dropEvents != 1 {
		t.Fatalf("notes_dropped events = %d %v", dropEvents, err)
	}
}

func TestExpiryBoundaryAndWindowZero(t *testing.T) {
	t.Parallel()
	s, _, room, one, two := wakeFixture(t)
	ctx := context.Background()
	post := func(body string, age time.Duration) Message {
		m, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: two.ID, Body: body})
		if err != nil {
			t.Fatal(err)
		}
		ageMessage(t, s, m.ID, age)
		return m
	}
	if err := s.SetMessageExpirySeconds(ctx, 3600); err != nil {
		t.Fatal(err)
	}
	atWindow := post("@channel exactly at the window", time.Hour+time.Second) // a moment older by the time dispatch runs: now - created >= window
	justInside := post("@channel just inside the window", time.Hour-30*time.Second)
	got := deliveryFor(t, s, one.ID)
	if fmt.Sprint(got.Bodies) != fmt.Sprint([]string{"@channel just inside the window"}) {
		t.Fatalf("delivered %q: a note at or past the window goes, a younger one stays", got.Bodies)
	}
	if noteState(t, s, one.ID, atWindow.ID) != "dropped" || noteState(t, s, one.ID, justInside.ID) != "queued" {
		t.Fatal("boundary states wrong")
	}

	// Window 0 means never expire: a month-old ambient note is delivered, with no marker.
	if err := s.SetMessageExpirySeconds(ctx, 0); err != nil {
		t.Fatal(err)
	}
	month := post("@channel a month old", 30*24*time.Hour)
	// A human message in the batch lifts the two-second pause that follows an agent-only turn.
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "go on"}); err != nil {
		t.Fatal(err)
	}
	got = deliveryFor(t, s, one.ID)
	foundMonth := false
	for _, body := range got.Bodies {
		foundMonth = foundMonth || body == "@channel a month old"
	}
	if !foundMonth || got.Expired != nil || noteState(t, s, one.ID, month.ID) != "queued" {
		t.Fatalf("window 0 must never expire: %q marker=%v", got.Bodies, got.Expired)
	}
}

func TestAnAgentWithOnlyExpiredAmbientNotesIsNotWoken(t *testing.T) {
	t.Parallel()
	s, _, room, one, two := wakeFixture(t)
	ctx := context.Background()
	m, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: two.ID, Body: "@channel stale broadcast"})
	if err != nil {
		t.Fatal(err)
	}
	ageMessage(t, s, m.ID, 8*time.Hour)
	if got := deliveryFor(t, s, one.ID); got.Ready {
		t.Fatalf("an agent with nothing left to read must not get a turn: %+v", got)
	}
	if noteState(t, s, one.ID, m.ID) != "dropped" {
		t.Fatal("the stale note should be dropped, not left queued to wake the agent again")
	}
	if got := deliveryFor(t, s, one.ID); got.Ready {
		t.Fatal("a second pass must not wake it either")
	}
}

// A reply to the agent's own message names it even when it is old.
func TestAnOldReplyToTheAgentIsStillDelivered(t *testing.T) {
	t.Parallel()
	s, _, room, one, _ := wakeFixture(t)
	ctx := context.Background()
	mine, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: one.ID, Body: "my question"})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "the answer", ReplyTo: mine.ID})
	if err != nil {
		t.Fatal(err)
	}
	ageMessage(t, s, reply.ID, 9*time.Hour)
	got := deliveryFor(t, s, one.ID)
	if !got.Ready || fmt.Sprint(got.Bodies) != fmt.Sprint([]string{"the answer"}) || got.Expired != nil {
		t.Fatalf("an old reply to the agent must be delivered unmarked: %+v", got)
	}
}

// Only group chatter is ambient; every reason that names the agent, and an untargeted human message, is not.
func TestOnlyGroupWakeReasonIsAmbient(t *testing.T) {
	t.Parallel()
	for reason, ambient := range map[string]bool{"group": true, "human_message": false, "mention": false, "role": false, "reply": false, "dm": false, "handoff": false, "inbox_answer": false, "": false} {
		if ambientWakeReason(reason) != ambient {
			t.Errorf("ambientWakeReason(%q) = %v, want %v", reason, !ambient, ambient)
		}
	}
}

func v1ExpiryFixture(t *testing.T, settingsJSON string) (string, V1ImportMap) {
	t.Helper()
	source, mapping := v1Fixture(t)
	db, err := sql.Open("sqlite", fixtureDSN(filepath.Join(source, "server", "aimebu.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO settings(id,data) VALUES (1,?)`, settingsJSON); err != nil {
		t.Fatal(err)
	}
	return source, mapping
}

func TestV1ImportCarriesTheMessageExpiryWindow(t *testing.T) {
	t.Parallel()
	source, mapping := v1ExpiryFixture(t, `{"messages_considered_expired_after_seconds":7200,"theme":"dark"}`)
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)", timestamp()); err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewV1Import(ctx, source, "owner")
	if err != nil {
		t.Fatal(err)
	}
	for _, skipped := range preview.SkippedSettings {
		if skipped.Key == v1ExpiryKey {
			t.Fatalf("the expiry window is carried, not skipped: %+v", preview.SkippedSettings)
		}
	}
	report, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil || report.Imported["message_expiry"] != 1 {
		t.Fatalf("import: %+v %v", report, err)
	}
	if got, err := s.MessageExpirySeconds(ctx); err != nil || got != 7200 {
		t.Fatalf("window = %d, want the v1 value 7200", got)
	}
	// A value someone chose in v2 is never overwritten by a re-import.
	if err := s.SetMessageExpirySeconds(ctx, 600); err != nil {
		t.Fatal(err)
	}
	again, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil || again.AlreadyPresent["message_expiry"] != 1 || again.Imported["message_expiry"] != 0 {
		t.Fatalf("re-import: %+v %v", again, err)
	}
	if got, err := s.MessageExpirySeconds(ctx); err != nil || got != 600 {
		t.Fatalf("window = %d after re-import, want the v2 choice 600", got)
	}
}

func TestV1ImportSkipsAnInvalidMessageExpiryWindow(t *testing.T) {
	t.Parallel()
	source, mapping := v1ExpiryFixture(t, `{"messages_considered_expired_after_seconds":5}`)
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)", timestamp()); err != nil {
		t.Fatal(err)
	}
	report, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil || report.Imported["message_expiry"] != 0 {
		t.Fatalf("import: %+v %v", report, err)
	}
	found := false
	for _, skipped := range report.SkippedSettings {
		found = found || skipped.Key == v1ExpiryKey
	}
	if !found {
		t.Fatalf("an out-of-range value must be reported as skipped: %+v", report.SkippedSettings)
	}
	if got, err := s.MessageExpirySeconds(ctx); err != nil || got != DefaultMessageExpirySeconds {
		t.Fatalf("window = %d, want the default", got)
	}
}

func TestExpirableNoteIsOnlyAIGroupChatterThatIsNeitherAReplyNorInADM(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name                              string
		reason, author, replyTo, roomKind string
		want                              bool
	}{
		{"AI group chatter", "group", "agent", "", "task", true},
		{"a human's group tag", "group", "user", "", "task", false},
		{"an AI reply that also carries a group tag", "group", "agent", "msg_parent", "task", false},
		{"an AI message with a group tag in a private chat", "group", "agent", "", "dm", false},
		{"a mention", "mention", "agent", "", "task", false},
		{"a system note", "group", "system", "", "task", false},
	} {
		if got := expirableNote(c.reason, c.author, c.replyTo, c.roomKind); got != c.want {
			t.Errorf("%s: expirableNote = %v, want %v", c.name, got, c.want)
		}
	}
}

// An agent's reply to another agent that also carries @ais reaches the parent's author with the group
// wake reason, but a reply names its recipient and must not expire.
func TestAnOldAIReplyWithAGroupTagIsStillDelivered(t *testing.T) {
	t.Parallel()
	s, _, room, one, two := wakeFixture(t)
	ctx := context.Background()
	mine, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: one.ID, Body: "my question"})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: two.ID, Body: "@ais here is the answer", ReplyTo: mine.ID})
	if err != nil {
		t.Fatal(err)
	}
	var reason string
	if err := s.db.QueryRow(`SELECT wake_reason FROM note WHERE agent_id=? AND message_id=?`, one.ID, reply.ID).Scan(&reason); err != nil {
		t.Fatalf("the parent's author has a note for the reply: %v", err)
	}
	ageMessage(t, s, reply.ID, 9*time.Hour)
	// A human message in the batch lifts the pause that follows an agent-only turn.
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "go on"}); err != nil {
		t.Fatal(err)
	}
	got := deliveryFor(t, s, one.ID)
	delivered := false
	for _, body := range got.Bodies {
		delivered = delivered || body == "@ais here is the answer"
	}
	if !delivered || got.Expired != nil || noteState(t, s, one.ID, reply.ID) != "queued" {
		t.Fatalf("an old reply that carries a group tag (wake reason %q) must be delivered unmarked: %q %+v", reason, got.Bodies, got.Expired)
	}
}

// An AI's message in a private chat that carries @channel reaches the other AI with the group wake reason,
// but a DM is addressed to its members and must not expire.
func TestAnOldAIGroupTagInAPrivateChatIsStillDelivered(t *testing.T) {
	t.Parallel()
	s, _, room, one, two := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE room SET kind='dm' WHERE id=?`, room.ID); err != nil {
		t.Fatalf("the fixture room cannot stand in for a private chat: %v", err)
	}
	old, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: two.ID, Body: "@channel old note in a DM"})
	if err != nil {
		t.Fatal(err)
	}
	var reason string
	if err := s.db.QueryRow(`SELECT wake_reason FROM note WHERE agent_id=? AND message_id=?`, one.ID, old.ID).Scan(&reason); err != nil || reason != "group" {
		t.Fatalf("the DM note should carry the group wake reason: %q %v", reason, err)
	}
	ageMessage(t, s, old.ID, 9*time.Hour)
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "go on"}); err != nil {
		t.Fatal(err)
	}
	got := deliveryFor(t, s, one.ID)
	delivered := false
	for _, body := range got.Bodies {
		delivered = delivered || body == "@channel old note in a DM"
	}
	if !delivered || got.Expired != nil {
		t.Fatalf("an old AI group-tag note in a DM must be delivered: %q %+v", got.Bodies, got.Expired)
	}
}

// When the two-second pause after an agent-only turn ends a dispatch without building a turn, the expired notes
// must not have been dropped yet, or the next turn would carry no marker and the agent would never learn that
// older group messages exist.
func TestExpiredNotesAreDroppedOnlyWhenATurnIsBuilt(t *testing.T) {
	t.Parallel()
	s, _, room, one, two := wakeFixture(t)
	ctx := context.Background()
	ai := func(body string) Message {
		m, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: two.ID, Body: body})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	ai("@channel first, fresh")
	if got := deliveryFor(t, s, one.ID); !got.Ready {
		t.Fatal("the first turn should be built (and cancelled, which ends it just now)")
	}
	old := ai("@channel old chatter")
	ageMessage(t, s, old.ID, 8*time.Hour)
	ai("@channel second, fresh")
	if got := deliveryFor(t, s, one.ID); got.Ready {
		t.Fatalf("an agent-only batch right after a turn must wait out the pause: %+v", got)
	}
	if noteState(t, s, one.ID, old.ID) != "queued" || droppedEvents(t, s) != 0 {
		t.Fatal("no turn was built, so the expired note must still be queued and nothing recorded as dropped")
	}
	// Once the pause is over the next turn is built, and it carries the marker.
	if _, err := s.db.Exec(`UPDATE turn SET ended_at=? WHERE agent_id=?`, time.Now().UTC().Add(-time.Hour).Format(timestampLayout), one.ID); err != nil {
		t.Fatal(err)
	}
	got := deliveryFor(t, s, one.ID)
	if !got.Ready || got.Expired == nil || got.Expired["hidden_count"] != float64(1) || noteState(t, s, one.ID, old.ID) != "dropped" {
		t.Fatalf("the next turn carries the marker for the note it left out: %+v %+v", got.Bodies, got.Expired)
	}
}

func droppedEvents(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE kind='notes_dropped'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// What the message list flags is derived from the notes dispatch wrote, so a mention, a role mention, a handoff and
// an answer sent to named agents take a message out however its text reads, and there is no second rule to drift.
func TestGroupOnlyMessagesAreDerivedFromTheNotesDispatchWrote(t *testing.T) {
	t.Parallel()
	s, _, room, one, lead := wakeFixture(t)
	ctx := context.Background()
	post := func(m Message) Message {
		m.RoomID = room.ID
		m, err := s.InsertMessage(ctx, m)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	// Posted by `one`; `lead` is the other agent, so group tags and mentions reach it.
	chatter := post(Message{AuthorKind: "agent", AuthorID: one.ID, Body: "@channel status"})
	ais := post(Message{AuthorKind: "agent", AuthorID: one.ID, Body: "@ais status"})
	handoff := post(Message{AuthorKind: "agent", AuthorID: one.ID, Body: "@channel finished", Handoff: true})
	answered := post(Message{AuthorKind: "agent", AuthorID: one.ID, Body: "@channel answer", OnlyTargets: []string{lead.ID}})
	mention := post(Message{AuthorKind: "agent", AuthorID: one.ID, Body: "@channel @" + lead.Slug + " look"})
	role := post(Message{AuthorKind: "agent", AuthorID: one.ID, Body: "@channel and @lead look"})
	quiet := post(Message{AuthorKind: "agent", AuthorID: one.ID, Body: "just talking, nobody addressed"}) // a broadcast: every note is room-wide
	// A group tag that reaches no agent: the other agent has left the room, so nothing was queued and nothing can expire.
	if _, err := s.db.Exec(`UPDATE room_member SET left_at='2026-01-01T00:00:00.000000000Z' WHERE room_id=? AND member_kind='agent' AND member_id=?`, room.ID, lead.ID); err != nil {
		t.Fatal(err)
	}
	nobody := post(Message{AuthorKind: "agent", AuthorID: one.ID, Body: "@channel is anyone there"})
	ids := []string{chatter.ID, ais.ID, handoff.ID, answered.ID, mention.ID, role.ID, quiet.ID, nobody.ID, "msg_missing"}
	groupOnly, err := s.GroupOnlyMessages(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{chatter.ID: true, ais.ID: true, handoff.ID: false, answered.ID: false, mention.ID: false, role.ID: false, quiet.ID: true, nobody.ID: false, "msg_missing": false} {
		if groupOnly[id] != want {
			t.Errorf("message %s: group-only = %v, want %v", id, groupOnly[id], want)
		}
	}
}

func TestExpirableMessageKeepsTheDispatchRulesForAuthorReplyAndPrivateChats(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name                string
		author, reply, kind string
		groupOnly, want     bool
	}{
		{"AI group chatter", "agent", "", "task", true, true},
		{"a human's group tag", "user", "", "task", true, false},
		{"an AI reply that reached its parent with a group tag", "agent", "msg_1", "task", true, false},
		{"an AI group tag in a private chat", "agent", "", "dm", true, false},
		{"a message with a direct note", "agent", "", "task", false, false},
		{"a system message", "system", "", "task", true, false},
	} {
		if got := ExpirableMessage(c.author, c.reply, c.kind, c.groupOnly); got != c.want {
			t.Errorf("%s: ExpirableMessage = %v, want %v", c.name, got, c.want)
		}
	}
}
