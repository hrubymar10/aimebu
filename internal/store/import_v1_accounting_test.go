package store

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/goccy/go-json"
)

func accountingPart(group V1AccountingGroup, key string) (V1AccountingPart, bool) {
	for _, part := range group.Parts {
		if part.Key == key {
			return part, true
		}
	}
	return V1AccountingPart{}, false
}

// Every count of the backup is reconciled to what the import does with it, a reason per skipped part, and the parts add up to the total.
func TestV1ImportAccountingReconcilesAndSaysWhy(t *testing.T) {
	t.Parallel()
	source, base := v1Fixture(t) // rooms work + dm:alice:bot@repo; messages 1, 2 (work), 3 (dm); one reaction on message 2
	addV1Rooms(t, source, []string{V1SystemRoom, "gone"}, map[int64]string{10: V1SystemRoom, 11: "work", 12: "gone", 13: "gone"})
	db := openV1Fixture(t, source)
	// A join line carrying an attachment, an attachment in the room that is left out, a file no message refers to, and a reaction by a person
	// who is mapped to nobody.
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal(map[string]any{"id": 14, "room_id": "work", "from": "_system", "from_kind": "system", "body": "bot joined", "created_at": "2026-10-01T12:00:00Z", "attachments": []map[string]string{{"id": "att_join"}}})
	exec(`INSERT INTO messages(id,room_id,data) VALUES (14,'work',?)`, string(raw))
	raw, _ = json.Marshal(map[string]any{"id": 12, "room_id": "gone", "from": "alice", "from_kind": "human", "body": "m", "created_at": "2026-10-01T12:00:00Z", "attachments": []map[string]string{{"id": "att_gone"}}})
	exec(`UPDATE messages SET data=? WHERE id=12`, string(raw))
	// att_both is referred to by an imported message (11) and by one in the room that is left out (13): it counts as imported.
	message := func(id int, room string, attachment string) {
		raw, _ := json.Marshal(map[string]any{"id": id, "room_id": room, "from": "alice", "from_kind": "human", "body": "m", "created_at": "2026-10-01T12:00:00Z", "attachments": []map[string]string{{"id": attachment}}})
		exec(`UPDATE messages SET data=? WHERE id=?`, string(raw), id)
	}
	message(11, "work", "att_both")
	message(13, "gone", "att_both")
	for _, id := range []string{"att_join", "att_both"} { // the importer checks the file of every attachment an imported message or a join line names
		if err := os.WriteFile(filepath.Join(source, "server", "attachments", id+".png"), []byte("PNG"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"att_join", "att_both", "att_gone", "att_unused"} {
		raw, _ = json.Marshal(map[string]any{"id": id, "name": id + ".png", "mime": "image/png", "size": 3, "uploaded_at": "2026-10-01T12:00:00Z", "ext": "png"})
		exec(`INSERT INTO attachments(id,data) VALUES (?,?)`, id, string(raw))
	}
	raw, _ = json.Marshal(map[string]any{"id": "bob", "kind": "human", "name": "bob"})
	exec(`INSERT INTO agents(id,data) VALUES ('bob',?)`, string(raw))
	exec(`INSERT INTO reactions(message_id,data) VALUES (11,?)`, `[{"agent_id":"bob","emoji":"👍","created_at":"2026-10-01T12:00:00Z"}]`)
	s := newV1RoleStore(t)
	ctx := context.Background()
	if _, err := s.ReserveV1MessageIDs(ctx, source); err != nil {
		t.Fatal(err)
	}
	mapping := base
	mapping.Humans = map[string]string{"alice": "owner", "bob": "-"}
	mapping.Rooms = map[string]V1ImportRoom{"work": {Project: "repo", Lifecycle: "open"}, "gone": {Project: "repo", Skip: true}, "dm:alice:bot@repo": {Project: "repo", Skip: true}}
	report, err := s.ImportV1(ctx, source, mapping, true)
	if err != nil {
		t.Fatal(err)
	}
	groups := map[string]V1AccountingGroup{}
	for _, group := range report.Accounting {
		groups[group.Key] = group
		sum := 0
		for _, part := range group.Parts {
			sum += part.Count
		}
		if sum != group.Total {
			t.Errorf("%s: parts add up to %d, total %d (%+v)", group.Key, sum, group.Total, group.Parts)
		}
		if _, ok := accountingPart(group, "other"); ok {
			t.Errorf("%s: a count no reason accounts for: %+v", group.Key, group.Parts)
		}
	}
	if got := groups["rooms"]; got.Total != 4 {
		t.Fatalf("rooms: %+v", got)
	}
	want := map[string][]string{"imported": nil, "system_room": {V1SystemRoom}, "dm_not_imported": {"dm:alice:bot@repo"}, "room_skipped": {"gone"}}
	for key, items := range want {
		part, ok := accountingPart(groups["rooms"], key)
		if !ok || (items != nil && !reflect.DeepEqual(part.Items, items)) {
			t.Errorf("rooms part %s: %+v %v", key, part, ok)
		}
	}
	messages := groups["messages"]
	for key, count := range map[string]int{"imported": 3, "presence_lines": 1, "system_room": 1, "dm_not_imported": 1, "room_skipped": 2} {
		if part, ok := accountingPart(messages, key); !ok || part.Count != count {
			t.Errorf("messages part %s: %+v %v, want %d", key, part, ok, count)
		}
	}
	if part, _ := accountingPart(messages, "room_skipped"); !reflect.DeepEqual(part.Items, []string{"gone · 2"}) {
		t.Errorf("a skipped room is named with its message count, never its text: %+v", part)
	}
	for key, count := range map[string]int{"imported": 1, "person_skipped": 1} {
		if part, ok := accountingPart(groups["reactions"], key); !ok || part.Count != count {
			t.Errorf("reactions part %s: %+v %v, want %d", key, part, ok, count)
		}
	}
	// An attachment is counted once: file1 and att_both (also named in the room left out) are imported, att_join sits on a bookkeeping line,
	// att_gone in the room left out, att_unused is referred to by nothing.
	for key, count := range map[string]int{"imported": 2, "presence_lines": 1, "in_skipped_rooms": 1, "unused_files": 1} {
		if part, ok := accountingPart(groups["attachments"], key); !ok || part.Count != count {
			t.Errorf("attachments part %s: %+v %v, want %d", key, part, ok, count)
		}
	}
	if unused, _ := accountingPart(groups["attachments"], "unused_files"); report.Skipped["orphan_attachments"] != unused.Count {
		t.Errorf("the table counts %d unused files, the accounting %d", report.Skipped["orphan_attachments"], unused.Count)
	}
	for _, group := range report.Accounting {
		for _, part := range group.Parts {
			if part.Count <= 0 {
				t.Errorf("%s: a part that is not a positive count: %+v", group.Key, part)
			}
		}
	}
}
