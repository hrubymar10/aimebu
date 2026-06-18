package server

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/types"
)

func sqliteCount(t *testing.T, dbPath, table string) int {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestSQLiteLegacyCoreImportArchivesJSON(t *testing.T) {
	dir := t.TempDir()
	room := types.Room{ID: "general", Members: []string{"alice@test"}, CreatedAt: "2026-01-01T00:00:00Z"}
	msg := types.Message{ID: 7, RoomID: "general", From: "alice@test", FromKind: "ai", Body: "hello", CreatedAt: "2026-01-01T00:00:01Z"}
	agent := types.Agent{ID: "alice@test", Name: "alice", Kind: "ai", Model: "gpt5", Harness: "codex", Project: "test", RegisteredAt: "2026-01-01T00:00:00Z", LastSeen: now()}
	writeJSONFixture(t, filepath.Join(dir, "rooms.json"), []types.Room{room})
	writeJSONFixture(t, filepath.Join(dir, "messages.json"), []types.Message{msg})
	writeJSONFixture(t, filepath.Join(dir, "agents.json"), []types.Agent{agent})
	writeJSONFixture(t, filepath.Join(dir, "reactions.json"), struct {
		Reactions map[int64][]types.Reaction `json:"reactions"`
	}{Reactions: map[int64][]types.Reaction{7: {{AgentID: "alice@test", Emoji: "👍", CreatedAt: now()}}}})
	writeJSONFixture(t, filepath.Join(dir, "settings.json"), Settings{Theme: "light"})
	writeJSONFixture(t, filepath.Join(dir, "schema.json"), map[string]int{"version": storeSchemaVersion})

	s, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.db == nil {
		t.Fatal("sqlite db was not opened")
	}
	if got := sqliteCount(t, s.sqlitePath(), "rooms"); got != 2 {
		t.Fatalf("rooms rows = %d, want imported room plus _system", got)
	}
	if got := sqliteCount(t, s.sqlitePath(), "messages"); got != 1 {
		t.Fatalf("message rows = %d, want 1", got)
	}
	if got := sqliteCount(t, s.sqlitePath(), "agents"); got != 1 {
		t.Fatalf("agent rows = %d, want 1", got)
	}
	if got := sqliteCount(t, s.sqlitePath(), "reactions"); got != 1 {
		t.Fatalf("reaction rows = %d, want 1", got)
	}
	if got := s.getSettings().Theme; got != "light" {
		t.Fatalf("settings theme = %q, want light", got)
	}
	if mode := fileMode(t, s.sqlitePath()); mode != 0o600 {
		t.Fatalf("sqlite mode = %o, want 600", mode)
	}
	for _, name := range []string{"rooms.json", "messages.json", "agents.json", "reactions.json", "settings.json", "schema.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s should have moved from top-level, err=%v", name, err)
		}
		if _, err := os.Stat(filepath.Join(dir, ".old", name)); err != nil {
			t.Fatalf("%s should exist in .old: %v", name, err)
		}
	}
}

func TestSQLiteLegacyCoreImportFailureLeavesJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rooms.json"), []byte(`not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := newStore(dir); err == nil {
		t.Fatal("newStore succeeded with corrupt legacy JSON")
	}
	if _, err := os.Stat(filepath.Join(dir, "rooms.json")); err != nil {
		t.Fatalf("legacy JSON should remain in place: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "aimebu.sqlite")); !os.IsNotExist(err) {
		t.Fatalf("partial sqlite db should be removed, err=%v", err)
	}
}

func TestSQLitePruneDataDirClearsCoreDB(t *testing.T) {
	dir := t.TempDir()
	s, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	human, err := s.registerHuman("martin", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", human.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.roomSend("general", human.ID, "hi", false, nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := PruneDataDir(dir, false); err != nil {
		t.Fatal(err)
	}
	if got := sqliteCount(t, filepath.Join(dir, "aimebu.sqlite"), "agents"); got != 0 {
		t.Fatalf("agents after prune = %d, want 0", got)
	}
	if got := sqliteCount(t, filepath.Join(dir, "aimebu.sqlite"), "messages"); got != 0 {
		t.Fatalf("messages after prune = %d, want 0", got)
	}
}

func TestSQLiteAgentTouchDoesNotRewriteMessages(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	agent, _, err := s.registerAI("gpt5", "codex", "test", nil, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", agent.ID); err != nil {
		t.Fatal(err)
	}
	msgID, err := s.roomSend("general", agent.ID, "hello", false, nil, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE messages SET data='sentinel' WHERE id=?`, msgID); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.agents[agent.ID].LastSeen = "2026-01-01T00:00:00Z"
	s.mu.Unlock()

	s.touchAgent(agent.ID)

	var data string
	if err := s.db.QueryRow(`SELECT data FROM messages WHERE id=?`, msgID).Scan(&data); err != nil {
		t.Fatal(err)
	}
	if data != "sentinel" {
		t.Fatalf("message row was rewritten by touchAgent: %q", data)
	}
}

func writeJSONFixture(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
