package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/answer"
)

func TestMessageNumbersAreOneSequenceAcrossRoomsWithoutGaps(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	project, home, first := fixture(t, s)
	other, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, ParentID: home.ID, Kind: "task", Title: "general", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	rooms := []string{home.ID, other.ID}
	numbers := []int64{first.No}
	for i := 0; i < 8; i++ {
		// Events between messages (room membership, reactions) take seq numbers but never message numbers.
		if err := s.AddMember(ctx, rooms[i%2], "agent", "agent_example"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
		message, err := s.InsertMessage(ctx, Message{RoomID: rooms[i%2], AuthorKind: "system", AuthorID: "system", Body: "m"})
		if err != nil {
			t.Fatal(err)
		}
		numbers = append(numbers, message.No)
	}
	for i, no := range numbers {
		if no != int64(i+1) {
			t.Fatalf("message numbers across two rooms = %v, want 1..%d with no gaps", numbers, len(numbers))
		}
	}
	var seqGaps int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM message WHERE seq<>no`).Scan(&seqGaps); err != nil || seqGaps == 0 {
		t.Fatalf("seq and no should differ once events take seq numbers: differing rows=%d err=%v", seqGaps, err)
	}
	// Reading a message back carries its number.
	list, err := s.ListMessagesAfter(ctx, other.ID, 0)
	if err != nil || len(list) == 0 || list[0].No == 0 {
		t.Fatalf("listed messages lack their number: %+v %v", list, err)
	}
}

func TestMessageNumberIsUniqueAndAssignedToRawInserts(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	_, room, first := fixture(t, s)
	// A row inserted by any path, without a number, is numbered by the trigger, in order.
	if _, err := s.db.Exec(`INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at) VALUES ('raw_1',900,?,'system','system','x','[]',?)`, room.ID, timestamp()); err != nil {
		t.Fatal(err)
	}
	var no int64
	if err := s.db.QueryRow(`SELECT no FROM message WHERE id='raw_1'`).Scan(&no); err != nil || no != first.No+1 {
		t.Fatalf("raw insert numbered %d (err %v), want %d", no, err, first.No+1)
	}
	// A number cannot be taken twice.
	if _, err := s.db.Exec(`INSERT INTO message(id,seq,no,room_id,author_kind,author_id,body,targets_json,created_at) VALUES ('raw_2',901,?,?,'system','system','x','[]',?)`, first.No, room.ID, timestamp()); err == nil {
		t.Fatal("two messages with one number were accepted")
	}
}

func TestMessageByNoAndReferenceResolution(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	project, room, first := fixture(t, s)
	found, err := s.MessageByNo(ctx, first.No)
	if err != nil || found.ID != first.ID || found.RoomID != room.ID || found.Seq != first.Seq {
		t.Fatalf("MessageByNo = %+v %v", found, err)
	}
	if _, err := s.MessageByNo(ctx, 999); err != sql.ErrNoRows {
		t.Fatalf("an unknown number: %v", err)
	}
	for _, ref := range []string{"#1", "1", " #1 "} {
		if id, err := s.ResolveMessageRef(ctx, ref); err != nil || id != first.ID {
			t.Fatalf("ResolveMessageRef(%q) = %q %v, want %s", ref, id, err, first.ID)
		}
	}
	for _, ref := range []string{first.ID, "#999", "0", "#0", "#", "", "#1x"} {
		if id, err := s.ResolveMessageRef(ctx, ref); err != nil || id != ref {
			t.Fatalf("ResolveMessageRef(%q) = %q %v, want it unchanged", ref, id, err)
		}
	}
	// An agent's answer may name the message it replies to by number; the reference becomes the id when the room the message goes to carries
	// that number, and a number another room carries stays as written for the validation to report.
	other, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "task", Title: "other", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	otherMessage, err := s.InsertMessage(ctx, Message{RoomID: other.ID, AuthorKind: "system", AuthorID: "system", Body: "elsewhere"})
	if err != nil {
		t.Fatal(err)
	}
	messages := []answer.Message{
		{Room: room.ID, ReplyTo: "#1"},
		{Room: room.ID, ReplyTo: first.ID},
		{Room: room.ID, ReplyTo: "#42"},
		{Room: room.ID, ReplyTo: fmt.Sprintf("%d", otherMessage.No)},
		{Room: "", ReplyTo: "#1"},
	}
	if err := s.Tx(ctx, func(tx *Tx) error { return tx.resolveMessageRefs(messages) }); err != nil {
		t.Fatal(err)
	}
	if messages[0].ReplyTo != first.ID || messages[1].ReplyTo != first.ID || messages[2].ReplyTo != "#42" || messages[3].ReplyTo != fmt.Sprintf("%d", otherMessage.No) || messages[4].ReplyTo != "#1" {
		t.Fatalf("resolved references: %+v", messages)
	}
}

func TestMigrationRefusesToNumberExistingMessages(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/old.sqlite"
	db := databaseAtVersion(t, path, 70)
	defer db.Close()
	ctx := context.Background()
	if _, err := db.Exec(`INSERT INTO project(id,name,folder,created_at) VALUES ('p','p','/tmp/p',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO room(id,project_id,depth,kind,title,created_by,created_at) VALUES ('r','p',0,'home','h','u',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at) VALUES ('m',1,'r','system','system','x','[]',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	err := migrate(ctx, db)
	if err == nil || !strings.Contains(err.Error(), "already has 1 messages") || !strings.Contains(err.Error(), "reset") {
		t.Fatalf("migration over history: %v", err)
	}
	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 70 {
		t.Fatalf("the refused migration left version %d (%v)", version, err)
	}
}

// The unique index on no is partial (where no>0), so a lookup must repeat the predicate or SQLite cannot use it and reads the whole table
// once per call: the v1 import checks every message and was quadratic. These are the queries that look a message up by number.
func TestMessageNumberQueriesUseTheIndex(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	for _, query := range []string{
		`SELECT id,seq,no,room_id FROM message WHERE no=? AND no>0`,
		`SELECT id FROM message WHERE no=? AND no>0`,
		`SELECT COUNT(*) FROM message WHERE id=? OR seq=? OR (no=? AND no>0)`,
		`SELECT EXISTS(SELECT 1 FROM message WHERE seq<=?) OR EXISTS(SELECT 1 FROM message WHERE no>0 AND no<=?)`,
	} {
		args := make([]any, strings.Count(query, "?"))
		for i := range args {
			args[i] = 1
		}
		rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+query, args...)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		for _, step := range plan {
			if strings.HasPrefix(step, "SCAN message") && !strings.Contains(step, "USING") {
				t.Fatalf("%s\nreads the whole message table: %v", query, plan)
			}
		}
	}
}
