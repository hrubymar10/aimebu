package store

import (
	"context"
	"strings"
	"testing"
)

// A10-21: spans found in the live prose of a multi-line body must point at the
// same bytes in the raw body, whichever line they are on.

func TestProseSpansPointAtTheirTokenInMultiLineBodies(t *testing.T) {
	cases := map[string]string{
		"later lines longer":  "hi @bob\nthe second line is much longer than the first one @carl and @dora\nthird @eve is longest of all the lines here, truly",
		"after fenced block":  "hi @bob\n```\n@no\n```\n@carl again and more text",
		"after indented code": "hi @bob\n\n    @no code\n\n@carl after the code block here",
		"after inline code":   "hi @bob\n`code` then more text here\n@carl again",
		"escaped at":          "hi @bob\n\\@no but @carl is live on this longer line",
		"crlf":                "hi @bob\r\nthe second line is longer @carl\r\nthird line @dora is the longest of them all",
		"repeat token":        "@bob\nxx @bob\nxxxxxxxxxxxxxxxxxxxxxx @bob",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			spans := proseMentionSpans(body)
			if len(spans) < 2 {
				t.Fatalf("spans = %+v, want at least two", spans)
			}
			seen := map[int]bool{}
			for _, span := range spans {
				if seen[span.Start] {
					t.Fatalf("span repeats: %+v", spans)
				}
				seen[span.Start] = true
				if got := strings.ToLower(body[span.Start:span.End]); got != "@"+span.Token {
					t.Fatalf("span %+v slices %q, want @%s", span, got, span.Token)
				}
			}
		})
	}
	// Room refs: the same guarantee with `#`.
	body := "see #alpha\nand a longer second line with #beta in it\n`#no`\n#gamma at the end of the third line"
	for _, spans := range [][]MentionSpan{proseRoomTitleSpans(body)} {
		if len(spans) != 3 {
			t.Fatalf("spans = %+v, want 3", spans)
		}
		for i, span := range spans {
			if got := body[span.Start:span.End]; got != "#"+span.Token {
				t.Fatalf("span %d slices %q, want #%s", i, got, span.Token)
			}
		}
	}
}

func TestMultiLineMentionWakesAndStores(t *testing.T) {
	s, _ := testStore(t, Options{})
	mentionSeed(t, s)
	room := mentionRoom(t, s, "task", "wiring")
	wren := mentionAgent(t, s, "wren", room.ID)
	vera := mentionAgent(t, s, "vera", room.ID)

	first := mentionPersonMessage(t, s, room.ID, "@wren please wire the panel\nthanks in advance, this line is longer")
	want := "@" + wren.ID + " please wire the panel\nthanks in advance, this line is longer"
	if got := storedBody(t, s, first.ID); got != want {
		t.Fatalf("stored %q, want %q", got, want)
	}
	if reason := wakeReason(t, s, wren.ID, first.ID); reason != "mention" {
		t.Fatalf("line-1 mention did not wake wren: %q", reason)
	}

	third := mentionPersonMessage(t, s, room.ID, "hello\n`code`\nand @vera on line three of this body")
	if got := storedBody(t, s, third.ID); !strings.HasSuffix(got, "@"+vera.ID+" on line three of this body") || !strings.HasPrefix(got, "hello\n`code`\nand ") {
		t.Fatalf("stored %q", got)
	}
	if reason := wakeReason(t, s, vera.ID, third.ID); reason != "mention" {
		t.Fatalf("line-3 mention did not wake vera: %q", reason)
	}
	if reason := wakeReason(t, s, wren.ID, third.ID); reason == "mention" {
		t.Fatalf("wren woken by a mention of vera")
	}
}

func TestMultiLineRoomRefStored(t *testing.T) {
	s, _ := testStore(t, Options{})
	roomRefSeed(t, s)
	general := mentionRoom(t, s, "task", "General")
	other := mentionRoom(t, s, "task", "Planning")
	message := mentionPersonMessage(t, s, general.ID, "see #General\nand a longer second line, also #Planning\nend")
	want := "see #" + general.ID + "\nand a longer second line, also #" + other.ID + "\nend"
	if got := storedBody(t, s, message.ID); got != want {
		t.Fatalf("stored %q, want %q", got, want)
	}
}

func TestMentionMigrationMultiLineHistory(t *testing.T) {
	ctx := context.Background()
	db := databaseAtVersion(t, t.TempDir()+"/m.sqlite", 132)
	defer db.Close()
	for _, statement := range []string{
		`INSERT INTO role(id,key,label,body,role_key) VALUES ('role_m','shed-hand','Worker','body','shed-hand')`,
		`INSERT INTO machine(id,name,key_hash,approved,is_host) VALUES ('mch_m','host','hash',1,1)`,
		`INSERT INTO project(id,name,folder,created_at) VALUES ('prj_m','olden','/tmp/olden','now')`,
		`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_m','prj_m','mch_m','/tmp/m')`,
		`INSERT INTO room(id,project_id,depth,kind,title,created_by,created_at) VALUES ('room_m','prj_m',0,'task','shed','u','now')`,
		`INSERT INTO agent(id,slug,project_id,role_id,tool,variant,model,provider,machine_id,checkout_id,key_hash,created_by,created_at)
			VALUES ('agt_wren','wren','prj_m','role_m','codex','native','x','x','mch_m','chk_m','x','u','now')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	bodies := map[string][2]string{
		"msg_a": {"@wren please wire the panel\nthanks in advance, a longer line", "@agt_wren please wire the panel\nthanks in advance, a longer line"},
		"msg_b": {"hi\n`@wren x`\nthen @wren again here", "hi\n`@wren x`\nthen @agt_wren again here"},
		"msg_c": {"hi\r\nlonger second line @wren\r\nend", "hi\r\nlonger second line @agt_wren\r\nend"},
	}
	seq := 0
	for id, b := range bodies {
		seq++
		if _, err := db.Exec(`INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at)
			VALUES (?,?, 'room_m','user','usr_x',?,'[]','now')`, id, seq, b[0]); err != nil {
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
	for id, b := range bodies {
		if got := read(id); got != b[1] {
			t.Fatalf("%s rewritten to %q, want %q", id, got, b[1])
		}
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
	for id, b := range bodies {
		if got := read(id); got != b[1] {
			t.Fatalf("second pass changed %s into %q", id, got)
		}
	}
}
