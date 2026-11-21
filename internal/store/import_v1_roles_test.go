package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/sqliteutil"
)

func TestResolveV1RolesKeepsParentLinks(t *testing.T) {
	t.Parallel()
	destination := map[string]RoleDefinition{
		"reviewer":     {Key: "reviewer", Body: "reviewer text", Cardinality: "multi"},
		"sec-reviewer": {Key: "sec-reviewer", Body: "Additional focus: sec", Extends: "reviewer", Cardinality: "multi"},
	}
	source := map[string]v1ImportRole{
		"deep-reviewer": {Key: "deep-reviewer", Body: " Extra checks ", Extends: "reviewer", Custom: true},
		"sec-reviewer":  {Key: "sec-reviewer", Body: "v1 focus"},
		"pair-a":        {Key: "pair-a", Body: "A", Extends: "pair-b", Custom: true},
		"pair-b":        {Key: "pair-b", Body: "B", Custom: true},
		"orphan":        {Key: "orphan", Body: "O", Extends: "ghost", Custom: true},
		"selfish":       {Key: "selfish", Body: "S", Extends: "selfish", Custom: true},
		"loop-a":        {Key: "loop-a", Body: "a", Extends: "loop-b", Custom: true},
		"loop-b":        {Key: "loop-b", Body: "b", Extends: "loop-a", Custom: true},
		"plain":         {Key: "plain", Body: "plain body", Custom: true},
	}
	got := map[string]V1PreviewRole{}
	for _, role := range resolveV1Roles(source, destination, nil) {
		got[role.Key] = role
	}
	linked := func(key, parent, body string) {
		t.Helper()
		r := got[key]
		if r.Extends != parent || r.FlattenedFrom != "" || r.Body != body || r.SkippedReason != "" {
			t.Errorf("%s: want a link to %q with body %q, got %+v", key, parent, body, r)
		}
	}
	linked("deep-reviewer", "reviewer", "Extra checks")
	linked("sec-reviewer", "reviewer", "v1 focus")
	linked("pair-a", "pair-b", "A")
	linked("plain", "", "plain body")
	for _, c := range []struct{ key, from, reason string }{
		{"orphan", "ghost", "missing parent"},
		{"selfish", "selfish", "cycle"},
		{"loop-a", "", "cycle"},
	} {
		r := got[c.key]
		// A role whose parent is unavailable keeps no link; a loop between roles that exist is skipped whatever its link says.
		if (c.from != "" && (r.Extends != "" || r.FlattenedFrom != c.from)) || !strings.Contains(r.SkippedReason, c.reason) {
			t.Errorf("%s: a parent that is unavailable or loops is skipped with a reason: %+v", c.key, r)
		}
	}

	// A parent that v2 will not import (here: a description over v2's 80-character limit) cannot be linked to, so its child is
	// flattened from the v1 text instead, as before, and the import does not fail on a link to nothing.
	source2 := map[string]v1ImportRole{
		"base":      {Key: "base", Body: "base text", Description: strings.Repeat("d", 90), Custom: true},
		"child":     {Key: "child", Body: "child text", Extends: "base", Custom: true},
		"bad-card":  {Key: "bad-card", Body: "bc", Cardinality: "triple", Custom: true},
		"under-bad": {Key: "under-bad", Body: "ub", Extends: "bad-card", Custom: true},
		"Bad Key":   {Key: "Bad Key", Body: "bk", Custom: true},
		"under-key": {Key: "under-key", Body: "uk", Extends: "Bad Key", Custom: true},
		"ok-parent": {Key: "ok-parent", Body: "op", Custom: true},
		"ok-child":  {Key: "ok-child", Body: "oc", Extends: "ok-parent", Custom: true},
	}
	got2 := map[string]V1PreviewRole{}
	for _, role := range resolveV1Roles(source2, destination, nil) {
		got2[role.Key] = role
	}
	if r := got2["base"]; !strings.Contains(r.SkippedReason, "limits") {
		t.Errorf("base should be ruled out by its description: %+v", r)
	}
	for _, child := range []struct{ key, parent, body string }{{"child", "base", "base text\n\nchild text"}, {"under-bad", "bad-card", "bc\n\nub"}, {"under-key", "Bad Key", "bk\n\nuk"}} {
		if r := got2[child.key]; r.Extends != "" || r.FlattenedFrom != child.parent || r.Body != child.body || r.SkippedReason != "" {
			t.Errorf("%s under a skipped parent should be flattened and imported: %+v", child.key, r)
		}
	}
	if r := got2["ok-child"]; r.Extends != "ok-parent" || r.Body != "oc" {
		t.Errorf("a child of a parent that will be imported keeps its link: %+v", r)
	}
}

// v1 vacates an agent's roles when it deregisters and a backup is taken after the agents left, so the roles are rebuilt from the room's system
// messages: the last assignment holds, a person's "role cleared" removes it, and the deregistration's "vacated" notice does not.
func TestV1RolesAreRebuiltFromSystemMessages(t *testing.T) {
	t.Parallel()
	system := func(id int64, room, body string) v1ImportMessage {
		return v1ImportMessage{ID: id, RoomID: room, From: "_system", FromKind: "system", Body: body}
	}
	got := v1RolesFromSystemMessages([]v1ImportMessage{
		// out of order on purpose: the replay follows the message numbers
		system(9, "r1", `role "worker" vacated: owen@test deregistered`),
		system(2, "r1", "owen@test was assigned as worker"),
		system(3, "r1", "alice@test was assigned as worker"),
		system(4, "r1", "alice@test was assigned as reviewer"),
		system(5, "r1", "bob@test was assigned as leader"),
		system(6, "r1", "bob@test role cleared"),
		system(7, "r2", "owen@test was assigned as leader"),
		{ID: 8, RoomID: "r1", From: "owen@test", FromKind: "ai", Body: "carol@test was assigned as leader"},
		system(10, "r1", "Role changed to something else"),
		// a backup without a kind: the author _system is enough
		{ID: 11, RoomID: "r3", From: "_system", Body: "dora@test was assigned as reviewer"},
		// a person who types the same words is not v1
		{ID: 12, RoomID: "r3", From: "mallory", FromKind: "human", Body: "eve@test was assigned as leader"},
	})
	want := map[string]map[string]string{"r1": {"owen@test": "worker", "alice@test": "reviewer"}, "r2": {"owen@test": "leader"}, "r3": {"dora@test": "reviewer"}}
	if len(got) != len(want) {
		t.Fatalf("rooms: %v want %v", got, want)
	}
	for room, roles := range want {
		if len(got[room]) != len(roles) {
			t.Fatalf("room %s: %v want %v", room, got[room], roles)
		}
		for agent, role := range roles {
			if got[room][agent] != role {
				t.Errorf("%s in %s: %q want %q", agent, room, got[room][agent], role)
			}
		}
	}
}

// A backup taken after the agents deregistered has no roles in its rooms: the import still suggests the role the agent last had, from the
// room's system messages, when it is started in v2.
func TestV1ImportSuggestsTheRoleFromTheSystemMessagesOfADeregisteredAgent(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(source, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	var v1Room string
	if err := db.QueryRow(`SELECT id FROM rooms LIMIT 1`).Scan(&v1Room); err != nil {
		t.Fatal(err)
	}
	system := func(id int, body string) string {
		raw, _ := json.Marshal(map[string]any{"id": id, "room_id": v1Room, "from": "_system", "from_kind": "system", "body": body, "created_at": "2026-10-02T09:00:00Z"})
		return string(raw)
	}
	for _, query := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE rooms SET data=json_set(data,'$.roles',json('{}'))`, nil},
		{`CREATE TABLE agent_sessions(full_id TEXT PRIMARY KEY,data TEXT NOT NULL)`, nil},
		{`INSERT INTO agent_sessions VALUES ('bot@repo','{"full_id":"bot@repo","harness":"claude-code","project":"repo","cwd":"/work/repo","harness_session_id":"sess-1"}')`, nil},
		{`INSERT INTO messages(id,room_id,data) VALUES (?,?,?)`, []any{900001, v1Room, system(900001, "bot@repo was assigned as reviewer")}},
		{`INSERT INTO messages(id,room_id,data) VALUES (?,?,?)`, []any{900002, v1Room, system(900002, `role "reviewer" vacated: bot@repo deregistered`)}},
	} {
		if _, err := db.Exec(query.sql, query.args...); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s, _ := testStore(t, Options{})
	if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)", timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportV1(context.Background(), source, mapping, false); err != nil {
		t.Fatal(err)
	}
	var projectID string
	if err := s.db.QueryRow(`SELECT project_id FROM room WHERE id IN (SELECT room_id FROM message WHERE author_id='bot@repo') LIMIT 1`).Scan(&projectID); err != nil {
		// the fixture's messages are not from bot@repo: take any imported project
		if err := s.db.QueryRow(`SELECT id FROM project LIMIT 1`).Scan(&projectID); err != nil {
			t.Fatal(err)
		}
	}
	start, err := s.LegacyStart(t.Context(), projectID, "bot@repo")
	if err != nil || start.RoleKey != "reviewer" {
		t.Fatalf("the role the agent last had is suggested: %+v %v", start, err)
	}
}
