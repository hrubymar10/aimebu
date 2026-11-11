package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// Every room starts open; memory and the debug button still follow v1's settings.
func TestV1PreviewDefaultsFollowTheV1Data(t *testing.T) {
	source, _ := v1Fixture(t)
	db := openV1Fixture(t, source)
	if _, err := db.Exec(`INSERT INTO settings(id,data) VALUES (1,'{"memory_enabled":true,"debug_button_enabled":true,"theme":"dark"}')`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	preview, err := PreviewV1Import(ctx, source, "owner")
	if err != nil {
		t.Fatal(err)
	}
	for name, room := range preview.Suggested.Rooms {
		if room.Lifecycle != "open" {
			t.Errorf("room %q should start open, got %q", name, room.Lifecycle)
		}
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

// A project whose folder v1 never recorded is looked for beside the folders v1 does know, only where such a directory exists; of several,
// the parent most known folders share wins. The rest stay blank for the owner.
func TestV1SiblingFolderSuggestion(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mkdir := func(path string) string {
		t.Helper()
		full := filepath.Join(root, path)
		if err := os.MkdirAll(full, 0o700); err != nil {
			t.Fatal(err)
		}
		return full
	}
	// Two parents hold the folders v1 knows: "work" has three of them, "other" one.
	known := map[string]string{"a": mkdir("work/a"), "b": mkdir("work/b"), "c": mkdir("work/c"), "d": mkdir("other/d")}
	onlyWork := mkdir("work/only-work")
	onlyOther := mkdir("other/only-other")
	both := mkdir("work/both")
	mkdir("other/both")
	mkdir("elsewhere/hidden") // beside nothing v1 knows: never searched
	if err := os.WriteFile(filepath.Join(root, "work", "afile"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	projects := map[string]V1ImportProject{
		"only-work": {Name: "only-work"}, "only-other": {Name: "only-other"}, "both": {Name: "both"}, "hidden": {Name: "hidden"}, "missing": {Name: "missing"},
		"afile": {Name: "afile"}, "dots": {Name: ".."}, "slash": {Name: "x/y"},
	}
	for alias, folder := range known {
		projects[alias] = V1ImportProject{Name: alias, Folder: folder}
	}
	suggestSiblingFolders(projects)
	for alias, want := range map[string]string{"only-work": onlyWork, "only-other": onlyOther, "both": both, "hidden": "", "missing": "", "afile": "", "dots": "", "slash": ""} {
		if got := projects[alias].Folder; got != want {
			t.Errorf("%s: folder %q, want %q", alias, got, want)
		}
	}
	for alias, folder := range known {
		if projects[alias].Folder != folder {
			t.Errorf("a recorded folder is never replaced: %s is %q", alias, projects[alias].Folder)
		}
	}
	// With nothing recorded at all there is nothing to look beside.
	blank := map[string]V1ImportProject{"x": {Name: "x"}}
	suggestSiblingFolders(blank)
	if blank["x"].Folder != "" {
		t.Errorf("no known folder, no suggestion: %+v", blank)
	}
}
