package store

import (
	"context"
	"testing"

	"github.com/goccy/go-json"
)

func TestV1DMTitleNamesTheOtherParticipant(t *testing.T) {
	t.Parallel()
	agents := map[string]v1ImportAgent{
		"nina@reviewbot": {Kind: "ai", Name: "nina"}, "wren@aimebu": {Kind: "ai"}, "alice": {Kind: "human"}, "bob": {Kind: "human"},
	}
	humans := map[string]string{"alice": "owner", "bob": "bob-login"}
	for _, c := range []struct {
		name  string
		room  v1ImportRoom
		title string
	}{
		{"owner and an agent", v1ImportRoom{ID: "dm:alice:nina@reviewbot", Members: []string{"alice", "nina@reviewbot"}}, "nina"},
		{"two agents", v1ImportRoom{ID: "dm:nina@reviewbot:wren@aimebu", Members: []string{"nina@reviewbot", "wren@aimebu"}}, "nina · wren"},
		{"another person and an agent", v1ImportRoom{ID: "dm:bob:wren@aimebu", Members: []string{"bob", "wren@aimebu"}}, "bob · wren"},
		{"members come from the id when the list is empty", v1ImportRoom{ID: "dm:alice:wren@aimebu"}, "wren"},
		{"only the owner's accounts", v1ImportRoom{ID: "dm:alice:alice", Members: []string{"alice"}}, "alice"},
	} {
		if got := v1DMTitle(c.room, agents, humans, "owner"); got != c.title {
			t.Errorf("%s: title %q, want %q", c.name, got, c.title)
		}
	}
	// A skipped person has no live account, so the agent is the other participant.
	if got := v1DMTitle(v1ImportRoom{ID: "dm:alice:wren@aimebu", Members: []string{"alice", "wren@aimebu"}}, agents, map[string]string{"alice": "-"}, "owner"); got != "wren" {
		t.Errorf("skipped person: %q", got)
	}
}

// v1 DMs become v2 DMs: kind dm, titled by the other participant, always open (v2 has no archive for a DM), visible to the owner
// even when no human took part, and listed as their own preview entries.
func TestV1ImportDirectMessages(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	db := openV1Fixture(t, source)
	at := "2026-10-01T12:00:00Z"
	put := func(table, key, room string, value any) {
		raw, _ := json.Marshal(value)
		query, args := "INSERT INTO "+table+"(id,data) VALUES (?,?)", []any{key, string(raw)}
		if table == "messages" {
			query, args = "INSERT INTO messages(id,room_id,data) VALUES (?,?,?)", []any{key, room, string(raw)}
		}
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	const agentDM = "dm:nina@repo:wren@aimebu"
	put("agents", "nina@repo", "", map[string]any{"id": "nina@repo", "kind": "ai", "name": "nina", "harness": "codex", "model": "gpt-6-sol"})
	put("agents", "wren@aimebu", "", map[string]any{"id": "wren@aimebu", "kind": "ai", "name": "wren"})
	put("rooms", agentDM, "", map[string]any{"id": agentDM, "members": []string{"nina@repo", "wren@aimebu"}, "created_at": at, "created_by": "nina@repo"})
	put("messages", "4", agentDM, map[string]any{"id": 4, "room_id": agentDM, "from": "nina@repo", "from_kind": "ai", "body": "ping", "created_at": at})

	preview, err := PreviewV1Import(context.Background(), source, "owner")
	if err != nil {
		t.Fatal(err)
	}
	titles := map[string]string{}
	for _, room := range preview.Rooms {
		if room.DM {
			titles[room.ID] = room.Title
		}
		if !room.DM && room.Title != "" {
			t.Errorf("an ordinary room carries no DM title: %+v", room)
		}
	}
	if titles["dm:alice:bot@repo"] != "bot" || titles[agentDM] != "nina · wren" || len(titles) != 2 {
		t.Errorf("preview DM titles: %v", titles)
	}
	for _, room := range preview.Rooms {
		if room.ID == agentDM && (len(room.Participants) != 2 || room.Participants[0].Kind != "agent" || room.Participants[0].Name != "nina") {
			t.Errorf("the wizard retitles from participants: %+v", room.Participants)
		}
	}
	if got := preview.Suggested.Rooms[agentDM]; got.Lifecycle != "open" || got.Project != "repo" || got.Skip {
		t.Errorf("a DM starts imported and open, in its agent's project when there is one: %+v", got)
	}

	s, _ := testStore(t, Options{})
	if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)", timestamp()); err != nil {
		t.Fatal(err)
	}
	mapping.Wizard = true
	mapping.MemoryProjects["repo"] = "-"
	mapping.SkipProfiles, mapping.SkipShared = true, true
	// The mapping asks for both DMs archived; a DM cannot be, so they still land open.
	mapping.Rooms["dm:alice:bot@repo"] = V1ImportRoom{Project: "repo", Lifecycle: "closed"}
	mapping.Rooms[agentDM] = V1ImportRoom{Project: "repo", Lifecycle: "closed"}
	report, err := s.ImportV1(context.Background(), source, mapping, false)
	if err != nil {
		t.Fatal(err)
	}
	// Both asked for the archive default, so both are listed as opened; the report says so.
	if len(report.OpenedDMs) != 2 {
		t.Errorf("the report should name the DMs opened despite an archive request: %v", report.OpenedDMs)
	}
	rows, err := s.db.Query(`SELECT r.title,r.lifecycle,r.kind,EXISTS(SELECT 1 FROM room_member m WHERE m.room_id=r.id AND m.member_id='usr_owner' AND m.left_at IS NULL)
		FROM room r WHERE r.kind='dm' ORDER BY r.title`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type dm struct {
		lifecycle string
		owner     bool
	}
	got := map[string]dm{}
	for rows.Next() {
		var title, lifecycle, kind string
		var owner bool
		if err := rows.Scan(&title, &lifecycle, &kind, &owner); err != nil {
			t.Fatal(err)
		}
		got[title] = dm{lifecycle, owner}
	}
	want := map[string]dm{"bot": {"open", true}, "nina · wren": {"open", true}}
	if len(got) != len(want) || got["bot"] != want["bot"] || got["nina · wren"] != want["nina · wren"] {
		t.Fatalf("imported DMs: %v, want %v", got, want)
	}
}
