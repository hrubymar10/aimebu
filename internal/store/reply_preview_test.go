package store

import (
	"strings"
	"testing"
)

// The preview of a page's parents is looked up by message ID, never by reading the room: a plan that walks the room's messages would make
// every page of a long room cost the room's size.
func TestReplyPreviewQueryUsesTheMessageKey(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT m.id,m.no,m.author_kind,m.author_id,COALESCE(u.login,''),substr(m.body,1,600)
		FROM message m LEFT JOIN user u ON m.author_kind='user' AND u.id=m.author_id
		WHERE m.room_id=? AND m.id IN (?,?,?)`, "r", "a", "b", "c")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	for _, step := range plan {
		if strings.HasPrefix(step, "SCAN m") || strings.Contains(step, "message_room_seq") {
			t.Fatalf("the preview lookup reads the room's messages: %q", plan)
		}
	}
}

func TestMessageSnippetRules(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", 200)
	for name, c := range map[string]struct {
		body        string
		attachments int
		want        string
	}{
		"collapsed":       {"  a \n b\t c ", 0, "a b c"},
		"empty":           {"", 0, ""},
		"only files":      {" \n ", 2, "[attachment]"},
		"text wins":       {"text", 3, "text"},
		"exactly 96":      {strings.Repeat("x", 96), 0, strings.Repeat("x", 96)},
		"one over is cut": {strings.Repeat("x", 97), 0, strings.Repeat("x", 95) + "…"},
		"cut on a rune":   {long, 0, strings.Repeat("é", 95) + "…"},
		"no trailing gap": {strings.Repeat("x", 94) + " " + strings.Repeat("y", 10), 0, strings.Repeat("x", 94) + "…"},
	} {
		if got := MessageSnippet(c.body, c.attachments); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

// The attachments of the parents a page answers are looked up by message id; without an index each lookup reads the whole table.
func TestReplyPreviewAttachmentLookupUsesAnIndex(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	for _, query := range []string{
		`SELECT DISTINCT message_id FROM attachment WHERE message_id IN (?,?,?)`,
		`SELECT id,filename,mime,size,width,height,COALESCE(removed_at,''),removed_after_days FROM attachment WHERE message_id=?`,
	} {
		args := make([]any, strings.Count(query, "?"))
		for i := range args {
			args[i] = "x"
		}
		rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+query, args...)
		if err != nil {
			t.Fatal(err)
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
			if strings.HasPrefix(step, "SCAN attachment") {
				t.Errorf("%s reads the whole attachment table: %q", query, plan)
			}
		}
	}
}
