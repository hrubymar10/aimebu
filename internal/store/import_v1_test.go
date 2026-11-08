package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/sqliteutil"
)

// skipFsync turns off fsync on a fixture database that a test builds by hand. Each autocommit
// statement otherwise waits for the disk, and on a slow or busy disk those waits (not any CPU work)
// made the import tests take minutes. The database is thrown away with the test directory, and the
// pragma is per connection, so the pool is limited to one.
func skipFsync(t *testing.T, db *sql.DB) {
	t.Helper()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA synchronous=OFF"); err != nil {
		t.Fatal(err)
	}
}

func v1Fixture(t *testing.T) (string, V1ImportMap) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "server")
	if err := os.MkdirAll(filepath.Join(dir, "attachments"), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(dir, "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	skipFsync(t, db)
	for _, statement := range []string{
		"CREATE TABLE schema_meta(version INTEGER)",
		"INSERT INTO schema_meta(version) VALUES (1)",
		"CREATE TABLE rooms(id TEXT PRIMARY KEY,data TEXT)",
		"CREATE TABLE messages(id INTEGER PRIMARY KEY,room_id TEXT,data TEXT)",
		"CREATE TABLE agents(id TEXT PRIMARY KEY,data TEXT)",
		"CREATE TABLE reactions(message_id INTEGER PRIMARY KEY,data TEXT)",
		"CREATE TABLE memory(id TEXT PRIMARY KEY,data TEXT)",
		"CREATE TABLE attachments(id TEXT PRIMARY KEY,data TEXT)",
		"CREATE TABLE macros(key TEXT PRIMARY KEY,value TEXT NOT NULL,seen_default INTEGER NOT NULL DEFAULT 0)",
		"CREATE TABLE settings(id INTEGER PRIMARY KEY CHECK(id=1),data TEXT NOT NULL)",
		"CREATE TABLE role_overrides(key TEXT PRIMARY KEY,data TEXT NOT NULL)",
		"CREATE TABLE role_custom(key TEXT PRIMARY KEY,data TEXT NOT NULL)",
		"CREATE TABLE prompts(key TEXT PRIMARY KEY,value TEXT NOT NULL)",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	put := func(table, key, room string, value any) {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		query := "INSERT INTO " + table + "(id,data) VALUES (?,?)"
		args := []any{key, string(raw)}
		if table == "messages" {
			query = "INSERT INTO messages(id,room_id,data) VALUES (?,?,?)"
			args = []any{key, room, string(raw)}
		}
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	at := "2026-10-01T12:00:00Z"
	put("rooms", "work", "", map[string]any{"id": "work", "members": []string{"alice", "bot@repo"}, "created_at": at, "created_by": "alice", "memory_enabled": false})
	put("rooms", "dm:alice:bot@repo", "", map[string]any{"id": "dm:alice:bot@repo", "members": []string{"alice", "bot@repo"}, "created_at": at, "created_by": "alice"})
	put("agents", "alice", "", map[string]any{"id": "alice", "kind": "human", "name": "alice"})
	put("agents", "bot@repo", "", map[string]any{"id": "bot@repo", "kind": "ai", "name": "bot", "harness": "codex", "model": "gpt-6-sol"})
	put("messages", "1", "work", map[string]any{"id": 1, "room_id": "work", "from": "alice", "from_kind": "human", "body": "question about #2", "created_at": at, "needs_human_attention": true, "attachments": []map[string]string{{"id": "file1"}}, "open_questions": []map[string]any{{"question": "Which?", "options": []string{"a", "b"}}}})
	put("messages", "2", "work", map[string]any{"id": 2, "room_id": "work", "from": "bot@repo", "from_kind": "ai", "from_harness": "codex", "from_model": "gpt-6-sol", "body": "answer", "created_at": at, "reply_to": 1})
	put("messages", "3", "dm:alice:bot@repo", map[string]any{"id": 3, "room_id": "dm:alice:bot@repo", "from": "alice", "from_kind": "human", "body": "private", "created_at": at})
	put("attachments", "file1", "", map[string]any{"id": "file1", "name": "image.png", "mime": "image/png", "size": 3, "uploaded_at": at, "ext": "png"})
	if err := os.WriteFile(filepath.Join(dir, "attachments", "file1.png"), []byte("PNG"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO reactions(message_id,data) VALUES (2,?)", "[{\"agent_id\":\"alice\",\"emoji\":\"✅\",\"created_at\":\"2026-10-01T12:00:00Z\"}]"); err != nil {
		t.Fatal(err)
	}
	put("memory", "fact", "", map[string]any{"id": "fact", "scope": "project_facts", "scope_key": "repo", "body": "fact", "version": 2, "author": "alice", "source_message_id": 1, "created_at": at, "updated_at": at})
	put("memory", "profile", "", map[string]any{"id": "profile", "scope": "user_profile", "scope_key": "alice", "body": "preference", "version": 1, "author": "alice", "created_at": at, "updated_at": at})
	put("memory", "shared", "", map[string]any{"id": "shared", "scope": "agent_shared_notes", "scope_key": "global", "body": "shared", "version": 1, "author": "bot@repo", "source_message_id": 99, "created_at": at, "updated_at": at})
	if _, err := db.Exec("INSERT INTO macros(key,value,seen_default) VALUES ('greet','Hello <name>',1),('name','world',0)"); err != nil {
		t.Fatal(err)
	}
	mapping := V1ImportMap{
		SourceKey: "fixture", OwnerLogin: "owner", DefaultProject: "repo",
		Projects: map[string]V1ImportProject{"repo": {Name: "Imported", Folder: filepath.Join(root, "project")}},
		Rooms:    map[string]V1ImportRoom{"work": {Project: "repo", Lifecycle: "open"}},
		Humans:   map[string]string{"alice": "owner"}, MemoryProjects: map[string]string{"repo": "repo"},
	}
	return root, mapping
}

func TestV1MacroPreviewDryRunConflictAndRerun(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)", timestamp()); err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewV1Import(ctx, source, "owner")
	if err != nil || preview.Counts["macros"] != 2 || preview.Macros["greet"] != "Hello <name>" || len(preview.MacroDefaults) != 1 || preview.MacroDefaults[0] != "greet" {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	if !reflect.DeepEqual(preview.MemoryCounts, V1MemoryCounts{Projects: map[string]int{"repo": 1}, Profiles: 1, Shared: 1, ProfileOwners: map[string]int{"alice": 1}}) {
		t.Fatalf("memory counts: %+v", preview.MemoryCounts)
	}
	if preview.Suggested.SkipProfiles || preview.Suggested.SkipShared {
		t.Fatalf("preview unexpectedly skips global memory: %+v", preview.Suggested)
	}
	if err := s.ReplaceMacros(ctx, map[string]string{"greet": "v2 edit"}); err != nil {
		t.Fatal(err)
	}
	dry, err := s.ImportV1(ctx, source, mapping, true)
	if err != nil || dry.Imported["macros"] != 1 || dry.Skipped["macro_conflicts"] != 1 || dry.MacroConflicts["greet"] != "keep_v2" {
		t.Fatalf("dry run: %+v %v", dry, err)
	}
	macros, _ := s.ListMacros(ctx)
	if len(macros) != 1 || macros["greet"] != "v2 edit" {
		t.Fatalf("dry run wrote: %+v", macros)
	}
	first, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil || first.Imported["macros"] != 1 {
		t.Fatalf("import: %+v %v", first, err)
	}
	importedKeys, err := s.V1ImportedMacroKeys(ctx, mapping.SourceKey)
	if err != nil || len(importedKeys) != 2 || importedKeys[0] != "greet" || importedKeys[1] != "name" {
		t.Fatalf("macro ledger keys: %+v %v", importedKeys, err)
	}
	macros, _ = s.ListMacros(ctx)
	if macros["greet"] != "v2 edit" || macros["name"] != "world" {
		t.Fatalf("imported: %+v", macros)
	}
	secondSource, secondMap := v1Fixture(t)
	secondMap.SourceKey = "second-v1-copy"
	// v1 numbers are kept, so a second copy of the same history cannot be imported on top of the first:
	// the macro ledger is exercised with every room skipped.
	if _, err := s.ImportV1(ctx, secondSource, secondMap, false); err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("a second copy of the history must be refused, got %v", err)
	}
	for key, room := range secondMap.Rooms {
		room.Skip = true
		secondMap.Rooms[key] = room
	}
	secondMap.Rooms["dm:alice:bot@repo"] = V1ImportRoom{Skip: true}
	second, err := s.ImportV1(ctx, secondSource, secondMap, false)
	if err != nil || second.AlreadyPresent["macros"] != 1 || second.Skipped["macro_conflicts"] != 1 {
		t.Fatalf("second source with same keys: %+v %v", second, err)
	}
	secondKeys, err := s.V1ImportedMacroKeys(ctx, secondMap.SourceKey)
	if err != nil || len(secondKeys) != 2 {
		t.Fatalf("second macro ledger: %+v %v", secondKeys, err)
	}
	if err := s.ReplaceMacros(ctx, map[string]string{"greet": "edited again", "name": "new name"}); err != nil {
		t.Fatal(err)
	}
	again, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil || again.AlreadyPresent["macros"] != 2 {
		t.Fatalf("rerun: %+v %v", again, err)
	}
	macros, _ = s.ListMacros(ctx)
	if macros["greet"] != "edited again" || macros["name"] != "new name" {
		t.Fatalf("rerun replaced edits: %+v", macros)
	}
}

func TestV1SettingsAndRoleImportPreview(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(source, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO settings(id,data) VALUES (1,'{"memory_enabled":true,"theme":"dark"}')`,
		`INSERT INTO role_custom(key,data) VALUES
		('researcher','{"description":"Researcher","emoji":"🔬","body":"Research facts","cardinality":"singleton"}'),
		('deep-reviewer','{"description":"Deep reviewer","emoji":"🔍","body":"Extra checks","extends":"reviewer"}'),
		('cycle-a','{"body":"A","extends":"cycle-b"}'),
		('cycle-b','{"body":"B","extends":"cycle-a"}')`,
		`INSERT INTO role_overrides(key,data) VALUES ('worker','{"body":"v1 worker"}'),('sec-reviewer','{"body":"v1 security focus"}')`,
		`INSERT INTO prompts(key,value) VALUES ('core','legacy prompt')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)", timestamp()); err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewV1Import(ctx, source, "owner")
	if err != nil || !preview.V1MemoryEnabled || len(preview.SkippedSettings) != 1 {
		t.Fatalf("settings preview=%+v err=%v", preview, err)
	}
	roles, err := s.ListRoles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	preview.ResolveRoles(roles)
	seen := map[string]V1PreviewRole{}
	for _, role := range preview.Roles {
		seen[role.Key] = role
	}
	// A v1 role that extends another keeps the link: its own text and the parent's key, not a flattened copy.
	if d := seen["deep-reviewer"]; d.Extends != "reviewer" || d.FlattenedFrom != "" || d.Body != "Extra checks" || d.SkippedReason != "" {
		t.Fatalf("linked role=%+v", d)
	}
	// The v1 sec-reviewer override (a built-in specialist) extends reviewer, and v2's own sec-reviewer does too.
	if sec := seen["sec-reviewer"]; sec.Extends != "reviewer" || sec.FlattenedFrom != "" || sec.Body != "v1 security focus" || !sec.Conflict || sec.CurrentExtends != "reviewer" || !strings.HasPrefix(sec.CurrentBody, "Additional focus:") {
		t.Fatalf("builtin role=%+v", sec)
	}
	if !strings.Contains(seen["cycle-a"].SkippedReason, "cycle") {
		t.Fatalf("cycle role=%+v", seen["cycle-a"])
	}
	mapping.EnableProjectMemory = map[string]bool{"repo": true}
	mapping.RoleConflicts = map[string]string{"sec-reviewer": "use_v1"}
	dry, err := s.ImportV1(ctx, source, mapping, true)
	if err != nil || dry.Imported["roles"] != 3 || dry.Skipped["roles"] != 2 || dry.Skipped["role_conflicts"] != 1 || dry.Skipped["settings"] != 1 || !dry.MemoryEnabled {
		t.Fatalf("dry settings/roles=%+v err=%v", dry, err)
	}
	report, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil || report.Imported["room_memory_settings"] != 1 {
		t.Fatalf("import settings/roles=%+v err=%v", report, err)
	}
	var projectID, roomID string
	if err := s.db.QueryRow("SELECT new_id FROM legacy_import_map WHERE source_key=? AND entity_kind='project' AND old_id='repo'", mapping.SourceKey).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT new_id FROM legacy_import_map WHERE source_key=? AND entity_kind='room' AND old_id='work'", mapping.SourceKey).Scan(&roomID); err != nil {
		t.Fatal(err)
	}
	if enabled, err := s.ProjectMemoryEnabled(ctx, projectID); err != nil || !enabled {
		t.Fatalf("project memory=%t err=%v", enabled, err)
	}
	var roomMemory sql.NullBool
	if err := s.db.QueryRow("SELECT memory_enabled FROM room WHERE id=?", roomID).Scan(&roomMemory); err != nil || !roomMemory.Valid || roomMemory.Bool {
		t.Fatalf("room memory=%+v err=%v", roomMemory, err)
	}
	facts, err := s.ListKnowledge(ctx, "project", projectID)
	if err != nil || len(facts) != 1 || !facts[0].Active {
		t.Fatalf("imported fact=%+v err=%v", facts, err)
	}
	allRoles, err := s.ListRoles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byKey := destinationRoleMap(allRoles)
	if byKey["researcher"].Cardinality != "singleton" || byKey["deep-reviewer"].Body != "Extra checks" || byKey["sec-reviewer"].Body != "v1 security focus" {
		t.Fatalf("imported roles=%+v", byKey)
	}
	// The links came across, and the text an agent would get is the parent's followed by the role's own, as in v1.
	if byKey["deep-reviewer"].Extends != "reviewer" || byKey["sec-reviewer"].Extends != "reviewer" ||
		byKey["deep-reviewer"].ResolvedBody != reviewerRoleBody+"\n\nExtra checks" || byKey["sec-reviewer"].ResolvedBody != reviewerRoleBody+"\n\nv1 security focus" {
		t.Fatalf("imported links deep=%+v sec=%+v", byKey["deep-reviewer"], byKey["sec-reviewer"])
	}
	importedRoles, err := s.V1ImportedRoleKeys(ctx, mapping.SourceKey)
	if err != nil || len(importedRoles) != 4 {
		t.Fatalf("imported role keys=%v err=%v", importedRoles, err)
	}
	if _, err := s.SetProjectMemoryEnabled(ctx, projectID, "usr_owner", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRoomMemoryEnabled(ctx, roomID, "usr_owner", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportV1(ctx, source, mapping, false); err != nil {
		t.Fatalf("rerun after owner edit: %v", err)
	}
	if err := s.db.QueryRow("SELECT memory_enabled FROM room WHERE id=?", roomID).Scan(&roomMemory); err != nil || roomMemory.Valid {
		t.Fatalf("rerun replaced owner inherit choice=%+v err=%v", roomMemory, err)
	}
	if _, err := s.db.Exec("DELETE FROM legacy_import_map WHERE source_key=? AND entity_kind='room_memory' AND old_id='work'", mapping.SourceKey); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportV1(ctx, source, mapping, false); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if enabled, err := s.ProjectMemoryEnabled(ctx, projectID); err != nil || enabled {
		t.Fatalf("rerun changed owner memory choice=%t err=%v", enabled, err)
	}
	if err := s.db.QueryRow("SELECT memory_enabled FROM room WHERE id=?", roomID).Scan(&roomMemory); err != nil || !roomMemory.Valid || roomMemory.Bool {
		t.Fatalf("rerun failed to restore v1 room restriction=%+v err=%v", roomMemory, err)
	}
	if _, err := s.SetRoomMemoryEnabled(ctx, roomID, "usr_owner", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportV1(ctx, source, mapping, false); err != nil {
		t.Fatalf("rerun after owner edit: %v", err)
	}
	if err := s.db.QueryRow("SELECT memory_enabled FROM room WHERE id=?", roomID).Scan(&roomMemory); err != nil || roomMemory.Valid {
		t.Fatalf("rerun replaced owner inherit choice=%+v err=%v", roomMemory, err)
	}
}

func TestV1MalformedSettingsAndRolesAreReported(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(source, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO settings(id,data) VALUES (1,'{"memory_enabled":"yes","theme":"dark"}')`,
		`INSERT INTO role_custom(key,data) VALUES ('broken','{bad'),('reader','{"body":"Read"}')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewV1Import(context.Background(), source, "owner")
	if err != nil || preview.V1MemoryEnabled || len(preview.SkippedSettings) != 1 {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	preview.ResolveRoles(nil)
	if len(preview.Roles) != 2 || preview.SkippedSourceRoles["broken"] != "invalid v1 JSON" {
		t.Fatalf("roles=%+v", preview.Roles)
	}
	s, _ := testStore(t, Options{})
	if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)", timestamp()); err != nil {
		t.Fatal(err)
	}
	for _, choice := range []map[string]string{{"missing": "keep_v2"}, {"reader": "replace"}} {
		mapping.RoleConflicts = choice
		if _, err := s.ImportV1(context.Background(), source, mapping, true); err == nil || !strings.Contains(err.Error(), "role") {
			t.Fatalf("invalid role choice %v: %v", choice, err)
		}
	}
	mapping.RoleConflicts = nil
	report, err := s.ImportV1(context.Background(), source, mapping, true)
	if err != nil || report.SkippedRoles["broken"] != "invalid v1 JSON" || report.Skipped["settings"] != 1 {
		t.Fatalf("dry run=%+v err=%v", report, err)
	}
	db, err = sql.Open("sqlite", sqliteutil.DSN(filepath.Join(source, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE settings SET data='{bad' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	preview, err = PreviewV1Import(context.Background(), source, "owner")
	if err != nil || len(preview.SkippedSettings) != 1 || preview.SkippedSettings[0].Key != "settings" {
		t.Fatalf("malformed settings preview=%+v err=%v", preview, err)
	}
}

func TestV1MacroExplicitUseSource(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)", timestamp()); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceMacros(ctx, map[string]string{"greet": "v2 edit"}); err != nil {
		t.Fatal(err)
	}
	mapping.MacroConflicts = map[string]string{"greet": "use_v1"}
	report, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil || report.MacroConflicts["greet"] != "use_v1" {
		t.Fatalf("import: %+v %v", report, err)
	}
	macros, _ := s.ListMacros(ctx)
	if macros["greet"] != "Hello <name>" {
		t.Fatalf("source was not selected: %+v", macros)
	}
}

func TestV1PreviewEmptyCollectionsUseArrays(t *testing.T) {
	source, _ := v1Fixture(t)
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(source, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM memory; UPDATE rooms SET data='{"id":"work","created_at":"2026-10-01T12:00:00Z","created_by":"alice"}' WHERE id='work'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewV1Import(context.Background(), source, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if preview.MemoryProjects == nil || preview.DestinationUsers == nil {
		t.Fatalf("empty preview collections must be arrays: %+v", preview)
	}
	for _, room := range preview.Rooms {
		if room.ID == "work" && room.Members == nil {
			t.Fatalf("empty room members must be an array: %+v", room)
		}
	}
}

func TestV1ImportDryRunRerunAndHistory(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)", timestamp()); err != nil {
		t.Fatal(err)
	}
	dry, err := s.ImportV1(ctx, source, mapping, true)
	if err != nil || dry.Imported["messages"] != 3 || dry.Imported["memory"] != 3 {
		t.Fatalf("dry run: %+v %v", dry, err)
	}
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM project WHERE system=0").Scan(&count); err != nil || count != 0 {
		t.Fatalf("dry run wrote project: %d %v", count, err)
	}
	first, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil || first.Imported["rooms"] != 2 || first.Imported["attachments"] != 1 || first.Imported["reactions"] != 1 || first.Skipped["unresolved_memory_source_links"] != 1 {
		t.Fatalf("import: %+v %v", first, err)
	}
	for _, table := range []string{"note", "event", "inbox_item", "agent", "turn"} {
		if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("import created %s: %d %v", table, count, err)
		}
	}
	var roomID, lifecycle string
	if err := s.db.QueryRow("SELECT r.id,r.lifecycle FROM room r JOIN legacy_room l ON l.room_id=r.id WHERE r.title='work'").Scan(&roomID, &lifecycle); err != nil || lifecycle != "open" {
		t.Fatalf("work lifecycle: %s %v", lifecycle, err)
	}
	messages, err := s.ListMessagesAfter(ctx, roomID, 0)
	if err != nil || len(messages) != 2 || messages[0].ID != "1" || messages[0].Seq != 1 || messages[1].ID != "2" || messages[1].Seq != 2 || messages[1].ReplyTo != "1" ||
		messages[0].Body != "question about #2" || !messages[0].NeedsAttention || messages[0].OpenQuestionsJSON == "" ||
		messages[1].AuthorHarness != "codex" || messages[1].AuthorModel != "gpt-6-sol" {
		t.Fatalf("history: %+v %v", messages, err)
	}
	var attachmentMessage, reactionMessage string
	if err := s.db.QueryRow(`SELECT message_id FROM attachment LIMIT 1`).Scan(&attachmentMessage); err != nil || attachmentMessage != "1" {
		t.Fatalf("attachment message=%q err=%v", attachmentMessage, err)
	}
	// An imported attachment arrived now, whatever age v1 gave it: retention counts from here.
	var arrived, madeAt string
	if err := s.db.QueryRow(`SELECT arrived_at,created_at FROM attachment LIMIT 1`).Scan(&arrived, &madeAt); err != nil || arrived == "" || arrived <= madeAt {
		t.Fatalf("imported attachment arrival: arrived_at=%q created_at=%q err=%v", arrived, madeAt, err)
	}
	if err := s.db.QueryRow(`SELECT message_id FROM reaction LIMIT 1`).Scan(&reactionMessage); err != nil || reactionMessage != "2" {
		t.Fatalf("reaction message=%q err=%v", reactionMessage, err)
	}
	native, err := s.InsertMessage(ctx, Message{RoomID: roomID, AuthorKind: "user", AuthorID: "usr_owner", Body: "new message", TargetsJSON: "[]"})
	if err != nil || native.Seq <= 3 {
		t.Fatalf("native message after import=%+v err=%v", native, err)
	}
	// The imported messages keep their v1 numbers (#N), and the first native message continues after the highest.
	if messages[0].No != 1 || messages[1].No != 2 || native.No != 4 {
		t.Fatalf("message numbers after import: v1 #%d #%d, native #%d", messages[0].No, messages[1].No, native.No)
	}
	profiles, err := s.ListGlobalKnowledge(ctx, "user_profile", "usr_owner")
	if err != nil || len(profiles) != 1 || profiles[0].Body != "preference" || profiles[0].ScopeLabel != "owner" || !profiles[0].Active {
		t.Fatalf("profiles: %+v %v", profiles, err)
	}
	var legacyFileName string
	if err := s.db.QueryRow("SELECT filename FROM attachment LIMIT 1").Scan(&legacyFileName); err != nil || legacyFileName != "image.png" {
		t.Fatalf("attachment name: %q %v", legacyFileName, err)
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM room r JOIN legacy_room l ON l.room_id=r.id WHERE r.kind='dm' AND r.lifecycle='open'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("a DM asked to be archived still imports open: %d %v", count, err)
	}
	var dmID, projectID string
	if err := s.db.QueryRow("SELECT id,project_id FROM room WHERE kind='dm'").Scan(&dmID, &projectID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_viewer','viewer','hash','{}',?)", timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_viewer','viewer')", projectID); err != nil {
		t.Fatal(err)
	}
	if err := s.UserRoomAccess(ctx, "usr_viewer", roomID); err != nil {
		t.Fatalf("project viewer should see ordinary room: %v", err)
	}
	if err := s.UserRoomAccess(ctx, "usr_viewer", dmID); err == nil {
		t.Fatal("project viewer saw imported private room without DM membership")
	}
	again, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil || len(again.Imported) != 0 || again.AlreadyPresent["messages"] != 3 {
		t.Fatalf("rerun: %+v %v", again, err)
	}
	mapping.Rooms["work"] = V1ImportRoom{Project: "repo", Lifecycle: "closed"}
	if _, err := s.ImportV1(ctx, source, mapping, false); err == nil {
		t.Fatal("changed map should conflict")
	}
	mapping.Wizard = true
	if _, err := s.ImportV1(ctx, source, mapping, true); err == nil {
		t.Fatal("wizard changed a previously imported CLI mapping")
	}
	if _, err := s.SetProjectMemoryEnabled(ctx, projectID, "usr_owner", true); err != nil {
		t.Fatal(err)
	}
	active, err := s.ListActiveGlobalKnowledge(ctx, "user_profile")
	if err != nil || len(active) != 1 || active[0].ID != profiles[0].ID {
		t.Fatalf("an imported profile is an ordinary active record: %+v %v", active, err)
	}
	if _, err := s.SetGlobalKnowledgeActive(ctx, "user_profile", "usr_owner", "usr_viewer", profiles[0].ID, profiles[0].Rev, false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer parked an imported profile: %v", err)
	}
	parked, err := s.SetGlobalKnowledgeActive(ctx, "user_profile", "usr_owner", "usr_owner", profiles[0].ID, profiles[0].Rev, false)
	if err != nil || parked.Active || parked.Rev != profiles[0].Rev+1 {
		t.Fatalf("owner parking: %+v %v", parked, err)
	}
	active, err = s.ListActiveGlobalKnowledge(ctx, "user_profile")
	if err != nil || len(active) != 0 {
		t.Fatalf("parked profile still active: %+v %v", active, err)
	}
	if _, err := s.SetGlobalKnowledgeActive(ctx, "user_profile", "usr_owner", "usr_owner", profiles[0].ID, parked.Rev, true); err != nil {
		t.Fatal(err)
	}
}

func TestFirstRunReservesV1MessageNumbers(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	if overlap, err := s.ReserveV1MessageIDs(ctx, source); err != nil || overlap {
		t.Fatalf("reservation overlap=%v err=%v", overlap, err)
	}
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.AppendEvent(Event{Kind: "user_created", PayloadJSON: `{}`})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	mapping.Wizard = true
	if _, err := s.ImportV1(ctx, source, mapping, false); err != nil {
		t.Fatal(err)
	}
	var collisions int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM event e JOIN message m ON m.seq=e.seq`).Scan(&collisions); err != nil || collisions != 0 {
		t.Fatalf("event/message sequence collisions=%d err=%v", collisions, err)
	}
	var highest int64
	if err := s.db.QueryRow(`SELECT MAX(seq) FROM message`).Scan(&highest); err != nil || highest != 3 {
		t.Fatalf("imported high-water=%d err=%v", highest, err)
	}
	var roomID string
	if err := s.db.QueryRow(`SELECT id FROM room WHERE title='work'`).Scan(&roomID); err != nil {
		t.Fatal(err)
	}
	native, err := s.InsertMessage(ctx, Message{RoomID: roomID, AuthorKind: "user", AuthorID: "usr_owner", Body: "after import"})
	if err != nil || native.Seq <= highest {
		t.Fatalf("native sequence=%d err=%v", native.Seq, err)
	}
}

func TestV1WizardSkipAndLaterSelectRoom(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)", timestamp()); err != nil {
		t.Fatal(err)
	}
	mapping.Wizard = true
	mapping.Rooms["dm:alice:bot@repo"] = V1ImportRoom{Project: "repo", Skip: true}
	mapping.Humans["alice"] = "-"
	mapping.MemoryProjects["repo"] = "-"
	mapping.SkipProfiles = true
	mapping.SkipShared = true
	if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_viewer','viewer','hash','{}',?)", timestamp()); err != nil {
		t.Fatal(err)
	}
	mapping.ProjectMembers = map[string][]string{"repo": {"viewer"}}
	if _, err := s.ImportV1(ctx, source, mapping, true); err == nil {
		t.Fatal("skipped human content was allowed into a shared project")
	}
	mapping.ProjectMembers = nil
	first, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.Skipped["rooms"] != 1 || first.Skipped["messages"] != 1 || first.Skipped["memory"] != 3 || first.Skipped["reactions"] != 1 {
		t.Fatalf("skip report: %+v", first)
	}
	if len(first.SkippedMemory) != 3 || first.SkippedMemory[0].Reason == "" || first.SkippedMemory[1].Reason == "" || first.SkippedMemory[2].Reason == "" {
		t.Fatalf("memory skip reasons: %+v", first.SkippedMemory)
	}
	var kind, author string
	if err := s.db.QueryRow("SELECT author_kind,author_id FROM message WHERE id='1'").Scan(&kind, &author); err != nil || kind != "user" || author != "alice" {
		t.Fatalf("historical author: %q %q %v", kind, author, err)
	}
	mapping.Rooms["dm:alice:bot@repo"] = V1ImportRoom{Project: "repo", Lifecycle: "closed"}
	second, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil || second.Imported["rooms"] != 1 || second.Imported["messages"] != 1 {
		t.Fatalf("extension: %+v %v", second, err)
	}
	var ownerDMs int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM room_member m JOIN room r ON r.id=m.room_id WHERE r.kind='dm' AND m.member_id='usr_owner'").Scan(&ownerDMs); err != nil || ownerDMs != 1 {
		t.Fatalf("owner DM membership: %d %v", ownerDMs, err)
	}
	mapping.Rooms["work"] = V1ImportRoom{Project: "repo", Lifecycle: "closed"}
	if _, err := s.ImportV1(ctx, source, mapping, false); err == nil {
		t.Fatal("changed imported room choice should conflict")
	}
}

func TestV1WizardCanImportPreviouslySkippedGlobalMemory(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)", timestamp()); err != nil {
		t.Fatal(err)
	}
	mapping.Wizard = true
	mapping.SkipProfiles = true
	mapping.SkipShared = true
	first, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil || first.Imported["memory"] != 1 || first.Skipped["memory"] != 2 || len(first.SkippedMemory) != 2 {
		t.Fatalf("first import: %+v %v", first, err)
	}
	mapping.SkipProfiles = false
	mapping.SkipShared = false
	dry, err := s.ImportV1(ctx, source, mapping, true)
	if err != nil || dry.Imported["memory"] != 2 || dry.Skipped["memory"] != 0 {
		t.Fatalf("expanded dry run: %+v %v", dry, err)
	}
	second, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil || second.Imported["memory"] != 2 {
		t.Fatalf("expanded import: %+v %v", second, err)
	}
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM global_memory WHERE active=1").Scan(&count); err != nil || count != 2 {
		t.Fatalf("imported global memory is not active: %d %v", count, err)
	}
}

func TestV1WizardExplainsMergedProfileCap(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(source, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	for id, size := range map[string]int{"second-profile": 1800, "third-profile": 300} {
		raw, err := json.Marshal(map[string]any{
			"id": id, "scope": "user_profile", "scope_key": "bob",
			"body": strings.Repeat("x", size), "version": 1, "author": "alice",
			"created_at": "2026-10-01T12:00:00Z", "updated_at": "2026-10-01T12:00:00Z",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO memory(id,data) VALUES (?,?)", id, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	mapping.Humans["bob"] = "owner"
	mapping.Wizard = true
	s, _ := testStore(t, Options{})
	if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)", timestamp()); err != nil {
		t.Fatal(err)
	}
	_, err = s.ImportV1(context.Background(), source, mapping, true)
	if err == nil || !strings.Contains(err.Error(), "Skip global user profiles") || !strings.Contains(err.Error(), "2048-byte") {
		t.Fatalf("missing profile cap hint: %v", err)
	}
	mapping.SkipProfiles = true
	report, err := s.ImportV1(context.Background(), source, mapping, true)
	if err != nil || report.Skipped["memory"] != 3 {
		t.Fatalf("skip profiles after cap hint: %+v %v", report, err)
	}
}

func TestV1ImportFlatLayoutAttachment(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	for _, name := range []string{"aimebu.sqlite", "attachments"} {
		if err := os.Rename(filepath.Join(source, "server", name), filepath.Join(source, name)); err != nil {
			t.Fatal(err)
		}
	}
	s, _ := testStore(t, Options{})
	if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)", timestamp()); err != nil {
		t.Fatal(err)
	}
	report, err := s.ImportV1(context.Background(), source, mapping, false)
	if err != nil || report.Imported["attachments"] != 1 {
		t.Fatalf("flat attachment import: %+v %v", report, err)
	}
}

func TestV1PreviewSuggestsOnlyPairedRoomsAndMostUsedFolders(t *testing.T) {
	t.Parallel()
	source, _ := v1Fixture(t)
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(source, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, query := range []string{
		`INSERT INTO rooms(id,data) VALUES ('work-leader','{"id":"work-leader","members":[],"created_at":"2026-10-01T12:00:00Z","created_by":"alice"}')`,
		`INSERT INTO rooms(id,data) VALUES ('_system','{"id":"_system","members":[],"created_at":"2026-10-01T12:00:00Z","created_by":"system"}')`,
		`CREATE TABLE agent_sessions(full_id TEXT PRIMARY KEY,data TEXT NOT NULL)`,
		`INSERT INTO agent_sessions(full_id,data) VALUES ('a@work','{"project":"work","cwd":"/tmp/work"}')`,
		`INSERT INTO agent_sessions(full_id,data) VALUES ('b@work','{"project":"work","cwd":"/tmp/work"}')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	preview, err := PreviewV1Import(context.Background(), source, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if preview.Suggested.Rooms["work"].Project != "work" || preview.Suggested.Rooms["work-leader"].Project != "work" || preview.Suggested.Rooms["_system"].Skip != true || preview.Suggested.Projects["work"].Folder != "/tmp/work" {
		t.Fatalf("suggestions: %+v", preview.Suggested)
	}
	if _, err := db.Exec(`UPDATE agent_sessions SET data='{"project":"work","cwd":"/different"}' WHERE full_id='b@work'`); err != nil {
		t.Fatal(err)
	}
	preview, err = PreviewV1Import(context.Background(), source, "owner")
	// Disagreeing sessions no longer blank the field: the most frequent folder wins, and an even split goes to the shorter path.
	if err != nil || preview.Suggested.Projects["work"].Folder != "/tmp/work" {
		t.Fatalf("an even split should suggest the shorter folder: %+v %v", preview.Suggested.Projects["work"], err)
	}
}

func TestV1ImportSniffsAttachmentAndSanitizesName(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	data := []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"></svg>`)
	path := filepath.Join(source, "server", "attachments", "file1.png")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(source, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE attachments SET data=json_set(data,'$.mime','image/svg+xml','$.name','../../evil.svg','$.size',?) WHERE id='file1'`, len(data)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, _ := testStore(t, Options{})
	if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)", timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportV1(context.Background(), source, mapping, false); err != nil {
		t.Fatal(err)
	}
	var mime, name string
	if err := s.db.QueryRow(`SELECT mime,filename FROM attachment LIMIT 1`).Scan(&mime, &name); err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(mime, "image/") || name != "evil.svg" {
		t.Fatalf("unsafe imported attachment: mime=%q name=%q", mime, name)
	}
}

func TestV1ImportReportsAndGrantsMappedHumanAccess(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	mapping.Humans["alice"] = "guest"
	s, _ := testStore(t, Options{})
	for _, user := range []struct{ id, login string }{{"usr_owner", "owner"}, {"usr_guest", "guest"}} {
		if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES (?,?,'hash','{}',?)", user.id, user.login, timestamp()); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	dry, err := s.ImportV1(ctx, source, mapping, true)
	if err != nil || dry.ProjectAccessGaps["repo"] != 1 || dry.UnreadableDMParticipants["repo"] != 1 {
		t.Fatalf("missing access not reported: %+v %v", dry, err)
	}
	mapping.ProjectMembers = map[string][]string{"repo": {"guest"}}
	report, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil || report.ProjectAccessGaps["repo"] != 0 || report.UnreadableDMParticipants["repo"] != 0 || report.GrantedViewers["repo"] != 1 {
		t.Fatalf("grant report: %+v %v", report, err)
	}
	var dmID string
	if err := s.db.QueryRow("SELECT id FROM room WHERE kind='dm'").Scan(&dmID); err != nil {
		t.Fatal(err)
	}
	if err := s.UserRoomAccess(ctx, "usr_guest", dmID); err != nil {
		t.Fatalf("mapped participant cannot read imported DM: %v", err)
	}
}

func TestV1ImportRejectsSystemMemberOrReactionInPreflight(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, query, want string
	}{
		{"room member", `UPDATE rooms SET data=json_set(data,'$.members[2]','system') WHERE id='work'`, "system cannot be a room member"},
		{"reaction author", `UPDATE reactions SET data=json_set(data,'$[0].agent_id','system') WHERE message_id=2`, "system cannot be a reaction author"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, mapping := v1Fixture(t)
			db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(source, "server", "aimebu.sqlite"), false))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(tc.query); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			s, _ := testStore(t, Options{})
			_, err = s.ImportV1(context.Background(), source, mapping, false)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("preflight error = %v, want %q", err, tc.want)
			}
			var count int
			if err := s.db.QueryRow("SELECT COUNT(*) FROM project WHERE system=0").Scan(&count); err != nil || count != 0 {
				t.Fatalf("preflight wrote project: %d %v", count, err)
			}
		})
	}
}

func TestMigration24To25AddsImportAndActivationSchema(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "migration24.sqlite")
	db, err := sql.Open("sqlite", sqliteutil.DSN(path, false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE schema_version(version INTEGER PRIMARY KEY CHECK(version > 0),applied_at TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations {
		if migration.version > 24 {
			break
		}
		if _, err := db.Exec(migration.sql); err != nil {
			t.Fatalf("migration %d: %v", migration.version, err)
		}
		if _, err := db.Exec("INSERT INTO schema_version(version,applied_at) VALUES (?,?)", migration.version, timestamp()); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow("SELECT MAX(version) FROM schema_version").Scan(&version); err != nil || version != migrations[len(migrations)-1].version {
		t.Fatalf("schema version = %d, %v", version, err)
	}
	var applied25 int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM schema_version WHERE version=25").Scan(&applied25); err != nil || applied25 != 1 {
		t.Fatalf("migration 25 applied=%d, %v", applied25, err)
	}
	if _, err := s.db.Exec("INSERT INTO global_memory(id,scope,scope_key,body,rev,author_kind,author_id,created_at,updated_at) VALUES ('knw_new','agent_shared_notes','global','new',1,'user','usr_owner',?,?)", timestamp(), timestamp()); err != nil {
		t.Fatalf("migration 25 global memory: %v", err)
	}
	var active bool
	if err := s.db.QueryRow("SELECT active FROM global_memory WHERE id='knw_new'").Scan(&active); err != nil || !active {
		t.Fatalf("new global record default activation = %v, %v", active, err)
	}
}

func TestV1ImportMissingBlobDoesNotWriteDestination(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	if err := os.Remove(filepath.Join(source, "server", "attachments", "file1.png")); err != nil {
		t.Fatal(err)
	}
	s, _ := testStore(t, Options{})
	if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)", timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportV1(context.Background(), source, mapping, false); err == nil {
		t.Fatal("missing attachment bytes should fail preflight")
	}
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM project WHERE system=0").Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed preflight wrote project: %d %v", count, err)
	}
}

func TestV1ImportReportsTablesItDoesNotCarry(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(source, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`CREATE TABLE plans(id TEXT PRIMARY KEY,data TEXT)`,
		`INSERT INTO plans VALUES ('p1','{}'),('p2','{}'),('p3','{}')`,
		`CREATE TABLE "odd""name"(id TEXT PRIMARY KEY)`,
		`INSERT INTO "odd""name" VALUES ('x')`,
		`CREATE TABLE fleets(name TEXT PRIMARY KEY,data TEXT)`,
		`INSERT INTO fleets VALUES ('f','{}')`,
		`UPDATE memory SET data=json_set(data,'$.pinned',json('true')) WHERE id='profile'`,
		`UPDATE attachments SET data=json_set(data,'$.thumb','t.png')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"plans": 3, "fleets": 1, `odd"name`: 1, "memory.pinned": 1, "attachments.thumb": 1}
	preview, err := PreviewV1Import(context.Background(), source, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(preview.NotCarried, want) {
		t.Fatalf("preview not_carried=%v, want %v (empty tables and imported tables are not listed; fleets are no longer imported)", preview.NotCarried, want)
	}
	s, _ := testStore(t, Options{})
	if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)", timestamp()); err != nil {
		t.Fatal(err)
	}
	report, err := s.ImportV1(context.Background(), source, mapping, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report.NotCarried, want) {
		t.Fatalf("report not_carried=%v, want %v", report.NotCarried, want)
	}
	raw, err := json.Marshal(report)
	if err != nil || !strings.Contains(string(raw), `"not_carried":{"attachments.thumb":1,"fleets":1,"memory.pinned":1,"odd\"name":1,"plans":3}`) {
		t.Fatalf("report JSON: %s %v", raw, err)
	}
}

func TestV1NotCarriedIsAnEmptyObjectWhenNothingIsLeftBehind(t *testing.T) {
	t.Parallel()
	source, _ := v1Fixture(t)
	preview, err := PreviewV1Import(context.Background(), source, "owner")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(preview)
	if preview.NotCarried == nil || !strings.Contains(string(raw), `"not_carried":{}`) {
		t.Fatalf("not_carried must be {} not null: %s", raw)
	}
}

// An imported message is a native message: neither it nor an attachment keeps a v1 payload column.
func TestMessageAndAttachmentTablesHaveNoV1PayloadColumns(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	rows, err := s.db.Query(`SELECT m.name||'.'||c.name FROM sqlite_master m JOIN pragma_table_info(m.name) c WHERE m.type='table' AND m.name IN ('message','attachment','reaction','project_memory','global_memory') AND (c.name LIKE '%legacy%' OR c.name LIKE '%import%')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatal(err)
		}
		t.Errorf("column %s remembers where the row came from", column)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var pending int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('project_memory') WHERE name='active'`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("project facts have no pending-review state: %d %v", pending, err)
	}
	var native int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('message') WHERE name IN ('author_harness','author_model')`).Scan(&native); err != nil || native != 2 {
		t.Fatalf("harness and model are native message columns: %d %v", native, err)
	}
}

func firstRunStore(t *testing.T) *Store {
	t.Helper()
	s, _ := testStore(t, Options{})
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	return s
}

func addNativeMessage(t *testing.T, s *Store, kind string) {
	t.Helper()
	for _, statement := range []string{
		`INSERT OR IGNORE INTO project(id,name,folder,created_at) VALUES ('prj_native','Native','/tmp/native','now')`,
		`INSERT OR IGNORE INTO room(id,project_id,depth,kind,title,created_by,created_at) VALUES ('room_native','prj_native',0,'home','Native','system','now')`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at) VALUES ('msg_native_'||?,(SELECT COALESCE(MAX(seq),0)+1000 FROM message),'room_native',?,?, 'hello','[]','now')`, kind, kind, kind); err != nil {
		t.Fatal(err)
	}
}

// The once-only rule lives in the store, so no caller can route around it: a used install refuses a
// dry run and a commit alike, while system notices do not make an install used.
func TestImportRefusesAnInstallThatAlreadyHasMessages(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	mapping.Wizard = true
	ctx := context.Background()
	s := firstRunStore(t)
	addNativeMessage(t, s, "system")
	if used, err := s.V1ImportUsed(ctx); err != nil || used {
		t.Fatalf("a system notice does not make an install used: %v %v", used, err)
	}
	if has, err := s.HasMessages(ctx); err != nil || has {
		t.Fatalf("HasMessages counts system notices: %v %v", has, err)
	}
	addNativeMessage(t, s, "user")
	if used, err := s.V1ImportUsed(ctx); err != nil || !used {
		t.Fatalf("a user message makes an install used: %v %v", used, err)
	}
	for _, dryRun := range []bool{true, false} {
		if _, err := s.ImportV1(ctx, source, mapping, dryRun); !errors.Is(err, ErrV1InstallUsed) {
			t.Fatalf("dry_run=%v on a used install: %v", dryRun, err)
		}
	}
	var imported int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM legacy_import_source`).Scan(&imported); err != nil || imported != 0 {
		t.Fatalf("a refused import left ledger rows: %d %v", imported, err)
	}
}

// The reservation always raises the counter; an overlap with earlier activity is only reported, and
// the import then refuses rather than renumber.
func TestReservationAlwaysRaisesTheCounterAndAMissedOneIsReported(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	mapping.Wizard = true
	ctx := context.Background()
	s := firstRunStore(t)
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.AppendEvent(Event{Kind: "user_created", PayloadJSON: `{}`})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if missed, err := s.V1ReservationMissed(ctx, 3); err != nil || !missed {
		t.Fatalf("an event inside the v1 range is a missed reservation: %v %v", missed, err)
	}
	overlap, err := s.ReserveV1MessageIDs(ctx, source)
	if err != nil || !overlap {
		t.Fatalf("overlap=%v err=%v", overlap, err)
	}
	var next int64
	if err := s.Tx(ctx, func(tx *Tx) error {
		event, err := tx.AppendEvent(Event{Kind: "user_created", PayloadJSON: `{}`})
		next = event.Seq
		return err
	}); err != nil || next != 4 {
		t.Fatalf("the counter was raised to the highest v1 number even with an overlap: next=%d err=%v", next, err)
	}
	if _, err := s.ImportV1(ctx, source, mapping, false); err == nil || !strings.Contains(err.Error(), "overlaps existing v2 activity") {
		t.Fatalf("a missed reservation refuses the import: %v", err)
	}
	fresh := firstRunStore(t)
	if _, err := fresh.ReserveV1MessageIDs(ctx, source); err != nil {
		t.Fatal(err)
	}
	if missed, err := fresh.V1ReservationMissed(ctx, 3); err != nil || missed {
		t.Fatalf("a made reservation is not reported as missed: %v %v", missed, err)
	}
}

// A v1 message without targets is stored with an empty list, never the text "null".
func TestImportedMessagesWithoutTargetsStoreAnEmptyList(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	mapping.Wizard = true
	ctx := context.Background()
	s := firstRunStore(t)
	if _, err := s.ReserveV1MessageIDs(ctx, source); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportV1(ctx, source, mapping, false); err != nil {
		t.Fatal(err)
	}
	var null, empty int
	if err := s.db.QueryRow(`SELECT COALESCE(SUM(targets_json='null'),0), COALESCE(SUM(targets_json='[]'),0) FROM message`).Scan(&null, &empty); err != nil || null != 0 || empty == 0 {
		t.Fatalf("targets_json null=%d empty=%d err=%v", null, empty, err)
	}
	var nullOptional int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM message WHERE open_questions_json='null' OR visual_plan_json='null' OR appendix_pages_json='null' OR proposed_answers_json='null'`).Scan(&nullOptional); err != nil || nullOptional != 0 {
		t.Fatalf("an absent optional v1 field is stored as the text null: %d %v", nullOptional, err)
	}
}

// v2 has no leaderboards, so v1's rating cards and its on/off setting are left in the backup: listed as not carried, never
// imported, and never able to fail the preview or the import, whatever the rows hold.
func TestV1ImportLeavesRatingCardsBehind(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(source, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE leaderboards(seq TEXT PRIMARY KEY,data TEXT)`,
		`INSERT INTO leaderboards VALUES ('000000000001','{"subject_model":"m","subject_harness":"h","ratings":{}}')`,
		`INSERT INTO leaderboards VALUES ('000000000002','not json')`,
		`INSERT INTO leaderboards VALUES ('000000000003',NULL)`,
		`INSERT INTO settings(id,data) VALUES (1,'{"leaderboard_enabled":false}') ON CONFLICT(id) DO UPDATE SET data='{"leaderboard_enabled":false}'`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)", timestamp()); err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewV1Import(ctx, source, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if preview.NotCarried["leaderboards"] != 3 {
		t.Errorf("the rating cards are listed as not carried: %v", preview.NotCarried)
	}
	if _, ok := preview.Counts["leaderboard_cards"]; ok {
		t.Errorf("no card count is promised: %v", preview.Counts)
	}
	skipped := false
	for _, entry := range preview.SkippedSettings {
		skipped = skipped || entry.Key == "leaderboard_enabled"
	}
	if !skipped {
		t.Errorf("the on/off setting is reported as skipped: %+v", preview.SkippedSettings)
	}
	report, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil {
		t.Fatalf("rating cards must never stop the import: %v", err)
	}
	for _, counts := range []map[string]int{report.Imported, report.AlreadyPresent, report.Skipped} {
		for key := range counts {
			if strings.Contains(key, "leaderboard") {
				t.Errorf("nothing about rating cards is imported or counted: %v", counts)
			}
		}
	}
	for _, table := range []string{"leaderboard_card", "leaderboard_submission"} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 0 {
			t.Errorf("table %s must not exist: %d %v", table, n, err)
		}
	}
}

// A v1 role whose parent v2 will not take (its description is over the limit) used to leave its child linked to a role that never
// arrives, and the closing check of the links then rolled the whole import back. The child is flattened instead and the import
// goes through.
func TestV1ImportChildOfASkippedParentIsFlattened(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(source, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("d", 90)
	for _, statement := range []string{
		`INSERT INTO role_custom(key,data) VALUES
		('base','{"description":"` + long + `","body":"base text"}'),
		('child','{"description":"Child","body":"child text","extends":"base"}'),
		('fine','{"description":"Fine","body":"fine text","extends":"reviewer"}')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)", timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportV1(ctx, source, mapping, true); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	report, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if report.SkippedRoles["base"] == "" {
		t.Errorf("the over-long parent should be reported as skipped: %v", report.SkippedRoles)
	}
	roles, err := s.ListRoles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byKey := destinationRoleMap(roles)
	if c := byKey["child"]; c.Extends != "" || c.Body != "base text\n\nchild text" {
		t.Errorf("child should be flattened from the v1 text: %+v", c)
	}
	if f := byKey["fine"]; f.Extends != "reviewer" || f.Body != "fine text" {
		t.Errorf("a child of a role v2 has keeps its link: %+v", f)
	}
	if _, found := byKey["base"]; found {
		t.Error("the skipped parent must not have been imported")
	}
}

// v1RoleImportRun imports the roles currently in the fixture and returns the v2 global roles before and after, plus what the
// preview resolved for the same destination, so a test can hold the preview and the apply to the same outcome.
func v1RoleImportRun(t *testing.T, s *Store, source string, mapping V1ImportMap) ([]V1PreviewRole, map[string]RoleDefinition, V1ImportReport, error) {
	t.Helper()
	ctx := context.Background()
	preview, err := PreviewV1Import(ctx, source, mapping.OwnerLogin)
	if err != nil {
		t.Fatal(err)
	}
	if preview.ImportedRoles, err = s.V1ImportedRoleKeys(ctx, mapping.SourceKey); err != nil {
		t.Fatal(err)
	}
	before, err := s.ListRoles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	preview.ResolveRoles(before)
	report, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil {
		return preview.Roles, nil, report, err
	}
	after, err := s.ListRoles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return preview.Roles, destinationRoleMap(after), report, nil
}

func insertV1CustomRoles(t *testing.T, source string, rows map[string]string) {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(source, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	skipFsync(t, db)
	for key, data := range rows {
		if _, err := db.Exec(`INSERT INTO role_custom(key,data) VALUES (?,?)`, key, data); err != nil {
			t.Fatal(err)
		}
	}
}

func v1RoleData(body, extends string) string {
	raw, _ := json.Marshal(map[string]string{"description": "Role", "body": body, "extends": extends})
	return string(raw)
}

func newV1RoleStore(t *testing.T) *Store {
	t.Helper()
	s, _ := testStore(t, Options{})
	if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)", timestamp()); err != nil {
		t.Fatal(err)
	}
	return s
}

// checkImportedRoleLinks fails the test when a live global role has a parent that is not live or a chain v2 would refuse.
func checkImportedRoleLinks(t *testing.T, s *Store) {
	t.Helper()
	all, err := allLiveRoles(context.Background(), s.db)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range all {
		if role.Extends == "" {
			continue
		}
		if err := checkRoleExtends(role, all); err != nil {
			t.Errorf("role %s -> %s is not a valid link: %v", role.Key, role.Extends, err)
		}
	}
}

func TestV1ImportLinksOnlyToParentsThatExist(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	s := newV1RoleStore(t)
	ctx := context.Background()

	// Run 1 brings a parent and a child; the owner then deletes the parent. Run 2 brings a new child of the deleted parent (F002),
	// a pair where the parent is too long to flatten into its child (F003), and a child of an empty-bodied specialist (F005).
	insertV1CustomRoles(t, source, map[string]string{"gone": v1RoleData("gone text", ""), "kept": v1RoleData("kept text", "gone")})
	if _, _, _, err := v1RoleImportRun(t, s, source, mapping); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	roles, err := s.ListRoles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byKey := destinationRoleMap(roles)
	if byKey["kept"].Extends != "gone" {
		t.Fatalf("run 1 should link kept to gone: %+v", byKey["kept"])
	}
	// kept must be removed before its parent, as the owner would have to.
	if _, err := s.ManageRole(ctx, "usr_owner", byKey["kept"], true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ManageRole(ctx, "usr_owner", byKey["gone"], true); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("d", 90) // over v2's description limit, so gamma is ruled out
	huge := strings.Repeat("p", 20000)
	gamma, _ := json.Marshal(map[string]string{"description": long, "body": huge})
	insertV1CustomRoles(t, source, map[string]string{
		"orphan":  v1RoleData("orphan text", "gone"),
		"gamma":   string(gamma),
		"parent":  v1RoleData(huge, "gamma"),
		"child":   v1RoleData("child text", "parent"),
		"sec-rev": v1RoleData("", "reviewer"),
	})
	preview, got, report, err := v1RoleImportRun(t, s, source, mapping)
	if err != nil {
		t.Fatalf("run 2 must not fail on a link to a deleted parent: %v", err)
	}
	if o := got["orphan"]; o.Extends != "" || o.Body != "gone text\n\norphan text" && o.Body != "orphan text" {
		t.Errorf("orphan has no parent to link to: %+v", o)
	}
	if _, ok := got["gone"]; ok {
		t.Error("an already-imported role the owner deleted must not come back")
	}
	// parent is flattened onto the skipped gamma and is then too long, so it is skipped as well; child's parent is therefore missing
	// and a link to it would dangle (F003): child is flattened from the v1 text, whose total is too long too, and skipped.
	for _, key := range []string{"gamma", "parent", "child"} {
		if _, ok := got[key]; ok {
			t.Errorf("%s must not be imported", key)
		}
		if report.SkippedRoles[key] == "" {
			t.Errorf("%s should be reported as skipped: %v", key, report.SkippedRoles)
		}
	}
	for _, role := range preview {
		if role.SkippedReason != "" {
			if _, ok := got[role.Key]; ok && role.Key != "gone" && role.Key != "kept" {
				t.Errorf("preview skipped %s but apply wrote it", role.Key)
			}
			continue
		}
		if g, ok := got[role.Key]; ok && (g.Extends != role.Extends || g.Body != role.Body) {
			t.Errorf("preview and apply disagree on %s: preview %q/%q apply %q/%q", role.Key, role.Extends, role.Body, g.Extends, g.Body)
		}
	}
	checkImportedRoleLinks(t, s)
}

func TestV1ImportFlattensWhenTheChainWouldBeTooDeep(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	s := newV1RoleStore(t)
	// Five chained roles hang below v2's reviewer already (sec-reviewer is one more), so a v1 child of the deepest one cannot link.
	rows := map[string]string{}
	prev := "reviewer"
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("c%d", i)
		rows[key] = v1RoleData(key+" text", prev)
		prev = key
	}
	rows["deep"] = v1RoleData("deep text", "c4")
	insertV1CustomRoles(t, source, rows)
	preview, got, _, err := v1RoleImportRun(t, s, source, mapping)
	if err != nil {
		t.Fatalf("a chain that is too deep must flatten, not abort: %v", err)
	}
	var flattened []string
	for _, role := range preview {
		if role.FlattenedFrom != "" && role.Extends == "" && role.SkippedReason == "" {
			flattened = append(flattened, role.Key)
		}
	}
	if len(flattened) == 0 {
		t.Fatalf("the preview must show which roles were flattened: %+v", preview)
	}
	for _, key := range flattened {
		if got[key].Extends != "" {
			t.Errorf("%s flattened in the preview but linked on apply", key)
		}
	}
	checkImportedRoleLinks(t, s)
}

func TestV1ImportEmptyBodyTakesTheDestinationParent(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	s := newV1RoleStore(t)
	// v1 has the shipped specialist with no body of its own, so v2's text stands, and v2's parent with it.
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(source, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO role_overrides(key,data) VALUES ('sec-reviewer','{"description":"Security reviewer","body":""}')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, got, _, err := v1RoleImportRun(t, s, source, mapping)
	if err != nil {
		t.Fatal(err)
	}
	if r := got["sec-reviewer"]; r.Extends != "reviewer" {
		t.Errorf("an empty v1 body falls back to v2's text and keeps v2's parent: %+v", r)
	}
	checkImportedRoleLinks(t, s)
}

func TestV1ImportRandomRoleChainsNeverFail(t *testing.T) {
	t.Parallel()
	for seed := int64(0); seed < 40; seed++ {
		rng := rand.New(rand.NewSource(seed))
		source, mapping := v1Fixture(t)
		s := newV1RoleStore(t)
		ctx := context.Background()
		const n = 12
		keyOf := func(i int) string { return fmt.Sprintf("r%02d", i) }
		parents := []string{"", "reviewer", "ghost"}
		for i := 0; i < n; i++ {
			parents = append(parents, keyOf(i))
		}
		rows := func(from, to int) map[string]string {
			out := map[string]string{}
			for i := from; i < to; i++ {
				body := fmt.Sprintf("body %d", i)
				if rng.Intn(6) == 0 {
					body = strings.Repeat("x", 20000)
				}
				desc := "Role"
				if rng.Intn(8) == 0 {
					desc = strings.Repeat("d", 90) // ruled out by v2's limits
				}
				raw, _ := json.Marshal(map[string]string{"description": desc, "body": body, "extends": parents[rng.Intn(len(parents))]})
				out[keyOf(i)] = string(raw)
			}
			return out
		}
		// First run, then delete some of what it brought (leaves first), then a second run with the rest and the same keys as parents.
		insertV1CustomRoles(t, source, rows(0, n/2))
		if _, _, _, err := v1RoleImportRun(t, s, source, mapping); err != nil {
			t.Fatalf("seed %d run 1: %v", seed, err)
		}
		for pass := 0; pass < n; pass++ {
			roles, err := s.ListRoles(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, role := range roles {
				if strings.HasPrefix(role.Key, "r") && len(role.Key) == 3 && rng.Intn(3) == 0 {
					_, _ = s.ManageRole(ctx, "usr_owner", role, true) // refused while something extends it, which is fine
				}
			}
		}
		insertV1CustomRoles(t, source, rows(n/2, n))
		preview, got, _, err := v1RoleImportRun(t, s, source, mapping)
		if err != nil {
			t.Fatalf("seed %d run 2: %v", seed, err)
		}
		for _, role := range preview {
			if g, ok := got[role.Key]; ok && role.SkippedReason == "" && (g.Extends != role.Extends || g.Body != role.Body) && !role.Conflict {
				t.Errorf("seed %d: preview and apply disagree on %s: preview %q apply %q", seed, role.Key, role.Extends, g.Extends)
			}
		}
		checkImportedRoleLinks(t, s)
	}
}

// A use_v1 choice can lengthen a chain that resolution did not look at (the v2 roles already below the replaced role). Apply repairs
// that: a role whose whole text fits is flattened and reported; one that would not fit is left out and v2's role stays as it was,
// never stored with its parent's text missing.
func TestV1ImportRepairsALinkMadeTooDeepByAConflictChoice(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		size    int
		flatten bool
	}{{"fits", 100, true}, {"too long", 20000, false}} {
		t.Run(c.name, func(t *testing.T) {
			source, mapping := v1Fixture(t)
			s := newV1RoleStore(t)
			ctx := context.Background()
			make := func(key, extends, body string) RoleDefinition {
				def, err := s.ManageRole(ctx, "usr_owner", RoleDefinition{Key: key, Label: key, Body: body, Extends: extends, Cardinality: "multi"}, false)
				if err != nil {
					t.Fatal(err)
				}
				return def
			}
			dest := make("dd", "", "v2 text")
			prev := "dd"
			for i := 1; i <= 4; i++ { // kk4 has four ancestors; one more above dd would make it six
				key := fmt.Sprintf("kk%d", i)
				make(key, prev, key)
				prev = key
			}
			text := strings.Repeat("t", c.size)
			make("bigp", "reviewer", text)
			insertV1CustomRoles(t, source, map[string]string{"dd": v1RoleData(text, "bigp")})
			mapping.RoleConflicts = map[string]string{"dd": "use_v1"}
			_, got, report, err := v1RoleImportRun(t, s, source, mapping)
			if err != nil {
				t.Fatalf("a link made too deep by a conflict choice must not stop the import: %v", err)
			}
			checkImportedRoleLinks(t, s)
			d := got["dd"]
			if c.flatten {
				if d.Extends != "" || !strings.Contains(d.Body, text) || !slices.Contains(report.FlattenedRoles, "dd") {
					t.Errorf("dd should be flattened and reported: %+v %v", d, report.FlattenedRoles)
				}
				return
			}
			if d.Body != dest.Body || d.Extends != "" || report.SkippedRoles["dd"] == "" {
				t.Errorf("dd should be back to what v2 had and reported as skipped: %+v %v", d, report.SkippedRoles)
			}
		})
	}
}

// v1 agent sessions and image sizes are carried: a sender with no live record keeps its session for its profile, and a thumbnail its size.
func TestV1ImportCarriesAgentSessionsAndImageSizes(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(source, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`UPDATE attachments SET data=json_set(data,'$.width',640,'$.height',480)`,
		`UPDATE rooms SET data=json_set(data,'$.roles',json('{"bot@repo":"reviewer","other@repo":"worker"}'))`,
		`CREATE TABLE agent_sessions(full_id TEXT PRIMARY KEY,data TEXT NOT NULL)`,
		`INSERT INTO agent_sessions VALUES ('bot@repo','{"full_id":"bot@repo","harness":"codex","project":"repo","cwd":"/work/repo","harness_session_id":"sess-1","resume_command":"codex resume sess-1","registered_at":"2026-10-01T10:00:00Z","last_seen":"2026-10-02T10:00:00Z"}')`,
		`INSERT INTO agent_sessions VALUES ('ghost@repo','{"full_id":"ghost@repo","cwd":"/nowhere"}')`,
		`INSERT INTO agent_sessions VALUES ('broken@repo','not json')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	preview, err := PreviewV1Import(context.Background(), source, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if preview.Counts["agent_sessions"] != 2 || len(preview.NotCarried) != 0 {
		t.Fatalf("sessions and sizes are carried, so nothing is left behind: counts=%v not_carried=%v", preview.Counts, preview.NotCarried)
	}
	s, _ := testStore(t, Options{})
	if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)", timestamp()); err != nil {
		t.Fatal(err)
	}
	report, err := s.ImportV1(context.Background(), source, mapping, false)
	if err != nil || report.Imported["agent_sessions"] != 2 {
		t.Fatalf("import: %+v %v", report, err)
	}
	var width, height int
	if err := s.db.QueryRow(`SELECT width,height FROM attachment LIMIT 1`).Scan(&width, &height); err != nil || width != 640 || height != 480 {
		t.Fatalf("image size: %d x %d %v", width, height, err)
	}
	var roomID string
	if err := s.db.QueryRow(`SELECT room_id FROM message WHERE author_id='bot@repo' LIMIT 1`).Scan(&roomID); err != nil {
		t.Fatal(err)
	}
	profile, err := s.LegacyAgentProfile(t.Context(), roomID, "bot@repo")
	if err != nil || !profile.Imported || profile.NativeSessionID != "sess-1" || profile.ResumeCommand != "" || profile.Cwd != "/work/repo" || profile.LastSeen != "2026-10-02T10:00:00Z" {
		t.Fatalf("imported profile: %+v %v", profile, err)
	}
	if _, err := s.LegacyAgentProfile(t.Context(), roomID, "ghost@repo"); err == nil {
		t.Fatal("a session for an agent that never wrote in the room is not shown")
	}
	// What v1 had recorded is what "Start as v2 agent" works from: the session id and folder carry over, the role v1 gave the agent most
	// often is suggested, and codex (which v2 cannot continue a v1 session for) is not resumable.
	var projectID string
	if err := s.db.QueryRow(`SELECT project_id FROM room WHERE id=?`, roomID).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	start, err := s.LegacyStart(t.Context(), projectID, "bot@repo")
	if err != nil || start.Tool != "" || start.Harness != "codex" || start.SessionID != "sess-1" || start.CWD != "/work/repo" || start.RoleKey != "reviewer" || start.Slug != "bot" || start.StartedID != "" {
		t.Fatalf("legacy start: %+v %v", start, err)
	}
	if _, err := s.LegacyAgentProfile(t.Context(), roomID, "broken@repo"); err == nil {
		t.Fatal("a row that is not JSON was skipped")
	}
}

// A v1 agent_sessions table of another shape is not a reason to refuse the preview or the import.
func TestV1ImportToleratesAnAgentSessionsTableOfAnotherShape(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(source, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`CREATE TABLE agent_sessions(id TEXT PRIMARY KEY,payload TEXT NOT NULL)`,
		`INSERT INTO agent_sessions VALUES ('bot@repo','{"cwd":"/x"}')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	if _, err := PreviewV1Import(context.Background(), source, "owner"); err != nil {
		t.Fatalf("preview: %v", err)
	}
	s, _ := testStore(t, Options{})
	if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)", timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportV1(context.Background(), source, mapping, false); err != nil {
		t.Fatalf("import: %v", err)
	}
}

// v1's server wrote "x joined", "x left" and the like into rooms. They are bookkeeping, so they are not imported (and not counted as the
// room's messages or its activity); a person who types the same words, and replies to such a line, lose nothing.
func TestV1ImportSkipsPresenceLinesButNotPeopleTypingThem(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	mapping.Wizard = true
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(source, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	at := "2026-10-02T09:00:00Z"
	add := func(id int, from, kind, body string, replyTo int) {
		raw, _ := json.Marshal(map[string]any{"id": id, "room_id": "work", "from": from, "from_kind": kind, "body": body, "created_at": at, "reply_to": replyTo})
		if _, err := db.Exec("INSERT INTO messages(id,room_id,data) VALUES (?,?,?)", id, "work", string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	add(10, "_system", "system", "bot@repo joined", 0)
	add(11, "_system", "system", "alice pruned after stale timeout", 0)
	add(12, "_system", "system", "bot@repo registered (gpt-6-sol/codex)", 0)
	add(13, "alice", "human", "bob joined", 0)                                // a person's words, kept
	add(14, "_system", "system", "role \"reviewer\" assigned to bot@repo", 0) // not presence, kept
	add(16, "_system", "system", "alice added memory user_profile/alice [20efdc8e]", 0)
	add(17, "_system", "system", "memory feed enabled", 0)
	add(15, "bot@repo", "ai", "replying to the join line", 10) // its parent is not imported
	if _, err := db.Exec("INSERT INTO reactions(message_id,data) VALUES (11,?)", "[{\"agent_id\":\"alice\",\"emoji\":\"👍\",\"created_at\":\"2026-10-02T09:00:00Z\"}]"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	preview, err := PreviewV1Import(context.Background(), source, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if preview.NotCarried["presence_lines"] != 5 {
		t.Fatalf("not carried: %v", preview.NotCarried)
	}
	for _, room := range preview.Rooms {
		if room.ID == "work" && room.Messages != 5 { // 1, 2, 13, 14, 15
			t.Fatalf("work counts %d messages", room.Messages)
		}
	}
	ctx := context.Background()
	s := firstRunStore(t)
	if _, err := s.ReserveV1MessageIDs(ctx, source); err != nil {
		t.Fatal(err)
	}
	report, err := s.ImportV1(ctx, source, mapping, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Skipped["presence_lines"] != 5 {
		t.Fatalf("skipped: %v", report.Skipped)
	}
	var bodies []string
	rows, err := s.db.Query(`SELECT body FROM message WHERE seq IN (10,11,12,13,14,15,16,17) ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, body)
	}
	rows.Close()
	if strings.Join(bodies, "|") != "bob joined|role \"reviewer\" assigned to bot@repo|replying to the join line" {
		t.Fatalf("imported: %q", bodies)
	}
	var reply sql.NullString
	if err := s.db.QueryRow(`SELECT reply_to FROM message WHERE seq=15`).Scan(&reply); err != nil || reply.Valid {
		t.Fatalf("a reply to a skipped line keeps no link: %v %v", reply, err)
	}
}

// A v1 build that stamps messages with their session and keeps every session of a name brings both into v2, for "Start as v2 agent".
func TestV1ImportKeepsMessageSessionsAndTheSessionHistory(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(source, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`UPDATE messages SET data=json_set(data,'$.from_session','session-older') WHERE id=2`,
		`CREATE TABLE agent_session_history(id INTEGER PRIMARY KEY AUTOINCREMENT, full_id TEXT NOT NULL, harness_session_id TEXT NOT NULL, harness TEXT, model TEXT, project TEXT, cwd TEXT, origin TEXT, registered_at TEXT, last_seen TEXT, UNIQUE(full_id, harness_session_id))`,
		`INSERT INTO agent_session_history(full_id,harness_session_id,harness,model,project,cwd,origin,registered_at,last_seen) VALUES
			('bot@repo','session-older','codex','gpt-6-sol','repo','/tmp/work/a','wrapper','2026-10-01T10:00:00Z','2026-10-02T10:00:00Z'),
			('bot@repo','session-newer','codex','gpt-6-sol','repo','/tmp/work/b','wrapper','2026-10-03T10:00:00Z','2026-10-04T10:00:00Z')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewV1Import(context.Background(), source, "owner")
	if err != nil || preview.Counts["agent_session_history"] != 2 {
		t.Fatalf("preview counts: %+v %v", preview.Counts, err)
	}
	if preview.NotCarried["agent_session_history"] != 0 || preview.NotCarried["messages.from_session"] != 0 {
		t.Fatalf("the history and the message session are carried, not reported as lost: %+v", preview.NotCarried)
	}
	s, _ := testStore(t, Options{})
	if _, err := s.db.Exec("INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)", timestamp()); err != nil {
		t.Fatal(err)
	}
	report, err := s.ImportV1(context.Background(), source, mapping, false)
	if err != nil || report.Imported["agent_session_history"] != 2 {
		t.Fatalf("import: %+v %v", report, err)
	}
	var session string
	if err := s.db.QueryRow(`SELECT author_session FROM message WHERE author_id='bot@repo'`).Scan(&session); err != nil || session != "session-older" {
		t.Fatalf("author_session = %q, %v", session, err)
	}
	var cwd string
	if err := s.db.QueryRow(`SELECT cwd FROM legacy_agent_session_history WHERE agent_id='bot@repo' AND harness_session_id='session-newer'`).Scan(&cwd); err != nil || cwd != "/tmp/work/b" {
		t.Fatalf("history row: cwd=%q %v", cwd, err)
	}
	// Importing again changes nothing.
	again, err := s.ImportV1(context.Background(), source, mapping, true)
	if err != nil || again.Imported["agent_session_history"] != 0 {
		t.Fatalf("a repeated import: %+v %v", again, err)
	}
}
