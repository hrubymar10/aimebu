package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/sqliteutil"
)

func openV1Fixture(t *testing.T, source string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(source, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// The workspace folder suggestion is the working directory most of a project's agents ran from, counting registry sessions and the
// cwd each agent registered with; a tie goes to the shorter, then the alphabetically first path.
func TestV1WorkspaceSuggestionIsTheMostFrequentFolder(t *testing.T) {
	t.Parallel()
	source, _ := v1Fixture(t)
	db := openV1Fixture(t, source)
	if _, err := db.Exec(`CREATE TABLE agent_sessions(id TEXT PRIMARY KEY,data TEXT)`); err != nil {
		t.Fatal(err)
	}
	session := func(id, project, cwd string) {
		raw, _ := json.Marshal(map[string]string{"project": project, "cwd": cwd})
		if _, err := db.Exec(`INSERT INTO agent_sessions(id,data) VALUES (?,?)`, id, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	agent := func(id, project, cwd string) {
		raw, _ := json.Marshal(map[string]any{"id": id, "kind": "ai", "project": project, "meta": map[string]string{"cwd": cwd}})
		if _, err := db.Exec(`INSERT INTO agents(id,data) VALUES (?,?)`, id, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	session("s1", "repo", "/work/repo")
	session("s2", "repo", "/work/repo")
	session("s3", "repo", "/work/repo-worktree")
	agent("a1@repo", "repo", "/work/repo-worktree")
	agent("a2@repo", "repo", "/work/repo-worktree") // 3 for the worktree, 2 for the checkout: the worktree wins on count
	session("t1", "tie", "/tmp/zzz")
	session("t2", "tie", "/tmp/aa")
	session("n1", "relative", "not/absolute")
	// An agent whose meta holds a non-string value is skipped for the suggestion; it must not break anything else.
	if _, err := db.Exec(`INSERT INTO agents(id,data) VALUES ('odd@repo','{"id":"odd@repo","kind":"ai","project":"repo","meta":{"cwd":{"nested":true},"n":5}}')`); err != nil {
		t.Fatal(err)
	}
	got := v1WorkspaceSuggestions(source)
	if got["repo"] != "/work/repo-worktree" {
		t.Errorf("repo: want the most frequent folder, got %q", got["repo"])
	}
	if got["tie"] != "/tmp/aa" {
		t.Errorf("a tie goes to the shorter, then the alphabetically first path: %q", got["tie"])
	}
	if _, ok := got["relative"]; ok {
		t.Errorf("a relative cwd is not a folder suggestion: %v", got)
	}
}

// A room with a message in the last 30 days starts open, an older one archived; memory and the debug button follow v1's settings.
func TestV1PreviewDefaultsFollowTheV1Data(t *testing.T) {
	source, _ := v1Fixture(t) // not parallel: it moves the preview clock
	db := openV1Fixture(t, source)
	if _, err := db.Exec(`INSERT INTO settings(id,data) VALUES (1,'{"memory_enabled":true,"debug_button_enabled":true,"theme":"dark"}')`); err != nil {
		t.Fatal(err)
	}
	old := v1PreviewNow
	defer func() { v1PreviewNow = old }()
	ctx := context.Background()

	v1PreviewNow = func() time.Time { return time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC) } // 19 days after the fixture's messages
	preview, err := PreviewV1Import(ctx, source, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if got := preview.Suggested.Rooms["work"].Lifecycle; got != "open" {
		t.Errorf("a room with a message 19 days ago should start open, got %q", got)
	}
	if !preview.V1DebugButton {
		t.Error("v1 had the debug button on")
	}
	for _, skipped := range preview.SkippedSettings {
		if skipped.Key == "debug_button_enabled" {
			t.Errorf("the debug button is carried, not skipped: %+v", skipped)
		}
	}
	if len(preview.Suggested.EnableProjectMemory) == 0 {
		t.Fatal("v1 memory was on, so projects should start with memory on")
	}
	for alias, on := range preview.Suggested.EnableProjectMemory {
		if !on || preview.Suggested.Projects[alias].Name == "" {
			t.Errorf("project %q memory default: %v", alias, on)
		}
	}

	v1PreviewNow = func() time.Time { return time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC) }
	preview, err = PreviewV1Import(ctx, source, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if got := preview.Suggested.Rooms["work"].Lifecycle; got != "closed" {
		t.Errorf("a room quiet for two months should start archived, got %q", got)
	}
}

func TestV1PreviewMemoryAndDebugDefaultOffWhenV1HadThemOff(t *testing.T) {
	t.Parallel()
	source, _ := v1Fixture(t)
	db := openV1Fixture(t, source)
	if _, err := db.Exec(`INSERT INTO settings(id,data) VALUES (1,'{"memory_enabled":false,"debug_button_enabled":false}')`); err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewV1Import(context.Background(), source, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if preview.V1DebugButton || len(preview.Suggested.EnableProjectMemory) != 0 {
		t.Errorf("memory and debug should stay off: debug=%v memory=%v", preview.V1DebugButton, preview.Suggested.EnableProjectMemory)
	}
	if strings.Contains(strings.Join(func() []string {
		var keys []string
		for _, s := range preview.SkippedSettings {
			keys = append(keys, s.Key)
		}
		return keys
	}(), ","), "debug_button_enabled") {
		t.Error("the debug setting is never reported as skipped")
	}
}

// A project the owner removed in the wizard must not leave a memory choice behind: the server names it instead of failing obscurely.
func TestV1ImportRejectsMemoryChoiceForUnknownProject(t *testing.T) {
	t.Parallel()
	source, mapping := v1Fixture(t)
	mapping.EnableProjectMemory = map[string]bool{"repo": true, "removed": true}
	data, err := OpenV1ImportSource(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	_, err = validateV1Import(data, source, mapping)
	if err == nil || !strings.Contains(err.Error(), `enable_project_memory names unknown project "removed"`) {
		t.Fatalf("want a readable error for the leftover memory choice, got %v", err)
	}
}
