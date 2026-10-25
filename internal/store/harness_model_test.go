package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateHarnessModels(t *testing.T) {
	t.Parallel()
	ok := []HarnessModel{{Slug: "claude-sonnet-5-5", Label: "Sonnet 5.5", Note: "fast", Default: true}, {Slug: "claude-opus-5-5", Label: "Opus 5.5"}}
	if err := ValidateHarnessModels("claude", ok); err != nil {
		t.Fatalf("valid list: %v", err)
	}
	if err := ValidateHarnessModels("claude", nil); err != nil {
		t.Fatalf("an empty list is allowed: %v", err)
	}
	for name, c := range map[string]struct {
		family string
		models []HarnessModel
	}{
		"unknown family":    {"fake", ok},
		"no default":        {"claude", []HarnessModel{{Slug: "a", Label: "A"}}},
		"two defaults":      {"claude", []HarnessModel{{Slug: "a", Label: "A", Default: true}, {Slug: "b", Label: "B", Default: true}}},
		"duplicate slug":    {"claude", []HarnessModel{{Slug: "a", Label: "A", Default: true}, {Slug: "a", Label: "A2"}}},
		"bad slug":          {"claude", []HarnessModel{{Slug: "bad id", Label: "A", Default: true}}},
		"no label":          {"claude", []HarnessModel{{Slug: "a", Default: true}}},
		"label too long":    {"claude", []HarnessModel{{Slug: "a", Label: strings.Repeat("界", 81), Default: true}}},
		"note too long":     {"claude", []HarnessModel{{Slug: "a", Label: "A", Note: strings.Repeat("n", 281), Default: true}}},
		"opencode no slash": {"opencode", []HarnessModel{{Slug: "gemma", Label: "G", Default: true}}},
		"too many":          {"pi", manyModels(65)},
	} {
		if err := ValidateHarnessModels(c.family, c.models); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if err := ValidateHarnessModels("opencode", []HarnessModel{{Slug: "ollama-cloud/gemma4:31b", Label: "Gemma", Default: true}}); err != nil {
		t.Errorf("provider/model: %v", err)
	}
	// A Vibe model is the alias of its config: one word, no provider prefix needed.
	if err := ValidateHarnessModels("vibe", []HarnessModel{{Slug: "mistral-medium-3.5", Label: "Mistral Medium 3.5", Default: true}}); err != nil {
		t.Errorf("a vibe alias: %v", err)
	}
}

func manyModels(n int) []HarnessModel {
	models := make([]HarnessModel, n)
	for i := range models {
		models[i] = HarnessModel{Slug: "m" + strings.Repeat("x", i%5) + string(rune('a'+i%26)) + string(rune('a'+i/26)), Label: "m", Default: i == 0}
	}
	return models
}

func TestReplaceHarnessModelsAndTheDefaultFallback(t *testing.T) {
	t.Parallel()
	s, _, _, agent, _ := wakeFixture(t)
	ctx := t.Context()
	harness, err := s.SaveLauncher(ctx, "owner", Launcher{MachineID: agent.MachineID, Tool: "codex", Name: "native", Label: "Codex", Command: "codex", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceHarnessModels(ctx, "owner", agent.MachineID, harness.ID, []HarnessModel{{Slug: "gpt-a", Label: "A", Note: "first"}, {Slug: "gpt-b", Label: "B", Default: true}}); err != nil {
		t.Fatal(err)
	}
	models, err := s.HarnessModels(ctx, harness.ID)
	if err != nil || len(models) != 2 || models[0].Slug != "gpt-a" || models[0].Note != "first" || !models[1].Default {
		t.Fatalf("models: %+v %v", models, err)
	}
	// A bad list is refused whole and changes nothing.
	err = s.ReplaceHarnessModels(ctx, "owner", agent.MachineID, harness.ID, []HarnessModel{{Slug: "gpt-a", Label: "A"}})
	if !errors.Is(err, ErrInvalidHarnessModels) {
		t.Fatalf("no default: %v", err)
	}
	if again, _ := s.HarnessModels(ctx, harness.ID); len(again) != 2 {
		t.Fatalf("a refused list must not change the stored one: %+v", again)
	}
	// Another machine's harness is not reachable.
	if err := s.ReplaceHarnessModels(ctx, "owner", "mch_other", harness.ID, nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("wrong machine: %v", err)
	}
	for wanted, want := range map[string]struct {
		model    string
		replaced bool
	}{"gpt-a": {"gpt-a", false}, "gpt-removed": {"gpt-b", true}, "": {"gpt-b", true}} {
		got, replaced, err := s.HarnessModelFor(ctx, harness.ID, wanted)
		if err != nil || got != want.model || replaced != want.replaced {
			t.Errorf("model for %q = %q %v %v, want %+v", wanted, got, replaced, err, want)
		}
	}
	// A harness with no list keeps whatever model was asked for.
	empty, err := s.SaveLauncher(ctx, "owner", Launcher{MachineID: agent.MachineID, Tool: "pi", Name: "native", Label: "Pi", Command: "pi", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, replaced, err := s.HarnessModelFor(ctx, empty.ID, "whatever"); err != nil || got != "whatever" || replaced {
		t.Errorf("no list: %q %v %v", got, replaced, err)
	}
	// Deleting the harness takes its models with it.
	if err := s.DeleteLauncher(ctx, agent.MachineID, harness.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM harness_model WHERE launcher_id=?`, harness.ID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("models left after delete: %d %v", left, err)
	}
}

// Migration 69 keeps launchers that are enabled or used and deletes the unused seeded ones, gives every kept one a label, and copies the
// old per-tool model list into each kept harness with its first model as the default.
func TestMigrationSixtyNineTurnsLaunchersIntoHarnesses(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(t.TempDir(), "from-68.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1) // one connection, so the pragma below holds for every statement
	if err := migrateUpTo(context.Background(), db, 68); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil { // the rows below name agents and rooms that do not exist
		t.Fatal(err)
	}
	launcher := func(id, tool, name string, enabled, containerized, preset int) string {
		return `INSERT INTO machine_launcher(id,machine_id,tool,name,command,enabled,containerized,supports_toolbox,preset) VALUES ('` + id + `','mch_a','` + tool + `','` + name + `','` + tool + `',` +
			string(rune('0'+enabled)) + `,` + string(rune('0'+containerized)) + `,1,` + string(rune('0'+preset)) + `)`
	}
	for _, statement := range []string{
		`INSERT INTO machine(id,name,key_hash,approved,is_host) VALUES ('mch_a','host','k',1,1)`,
		launcher("l_native", "claude", "native", 1, 0, 1),         // enabled preset: kept
		launcher("l_docker", "claude", "harness-docker", 0, 1, 1), // disabled but an agent switch uses it: kept
		launcher("l_unused", "codex", "native", 0, 0, 1),          // disabled and unused, but its tool's list was edited and no other codex launcher stays: kept so the list survives
		launcher("l_oc", "opencode", "native", 0, 0, 1),           // disabled and unused, and its tool's list is the seeded one: deleted
		launcher("l_custom", "pi", "mine", 0, 0, 0),               // not a preset: kept
		`INSERT INTO agent_switch(agent_id,room_id,tool,variant,launcher_id,model,provider,state,requested_at) VALUES ('agt_x','room_x','claude','docker','l_docker','sonnet','anthropic','draining','now')`,
		// An agent on the enabled harness uses a model the old list never had: it must stay usable, but not become the default.
		`INSERT INTO agent_switch(agent_id,room_id,tool,variant,launcher_id,model,provider,state,requested_at) VALUES ('agt_y','room_y','claude','native','l_native','claude-opus-5-5','anthropic','draining','now')`,
		// A failed switch is never retried: the model it names must not come back as a choice the owner had removed.
		`INSERT INTO agent_switch(agent_id,room_id,tool,variant,launcher_id,model,provider,state,requested_at) VALUES ('agt_z','room_z','claude','native','l_native','claude-removed','anthropic','failed','now')`,
		`DELETE FROM spawn_model`,
		`INSERT INTO spawn_model(tool,model_id,label,position) VALUES ('claude','sonnet','Sonnet',0),('claude','opus','Opus',1),('codex','gpt-x','GPT X',0),('pi','pi-a','Pi A',3),('pi','pi-b','Pi B',4),('opencode','ollama-cloud/gemma4:31b','Gemma 4 31B',0)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT id,label,preset FROM machine_launcher ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for rows.Next() {
		var id, label string
		var preset int
		if err := rows.Scan(&id, &label, &preset); err != nil {
			t.Fatal(err)
		}
		if preset != 0 {
			t.Errorf("%s is still a preset", id)
		}
		got[id] = label
	}
	rows.Close()
	want := map[string]string{"l_custom": "Pi · mine", "l_docker": "Claude Code (Docker)", "l_native": "Claude Code", "l_unused": "Codex"}
	if len(got) != len(want) {
		t.Fatalf("kept launchers: %v", got)
	}
	for id, label := range want {
		if got[id] != label {
			t.Errorf("%s label = %q, want %q (all %v)", id, got[id], label, got)
		}
	}
	models := map[string][]string{}
	mrows, err := db.Query(`SELECT launcher_id,slug,is_default FROM harness_model ORDER BY launcher_id,position`)
	if err != nil {
		t.Fatal(err)
	}
	for mrows.Next() {
		var id, slug string
		var def int
		if err := mrows.Scan(&id, &slug, &def); err != nil {
			t.Fatal(err)
		}
		tag := slug
		if def == 1 {
			tag += "*"
		}
		models[id] = append(models[id], tag)
	}
	mrows.Close()
	for id, want := range map[string]string{"l_native": "sonnet* opus claude-opus-5-5", "l_docker": "sonnet* opus", "l_custom": "pi-a* pi-b", "l_unused": "gpt-x*"} {
		if strings.Join(models[id], " ") != want {
			t.Errorf("models of %s = %v, want %s", id, models[id], want)
		}
	}
	if len(models["l_oc"]) != 0 {
		t.Errorf("the deleted launcher has models: %v", models["l_oc"])
	}
	var table int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='spawn_model'`).Scan(&table); err != nil || table != 0 {
		t.Errorf("spawn_model still exists: %d %v", table, err)
	}
}

// An agent whose model the owner removed from the harness's list starts with the harness default: its row is updated and the change is
// logged. A model still listed, an agent with no harness and a harness with no list are left alone.
func TestSettleAgentModelFallsBackToTheDefaultAndLogsIt(t *testing.T) {
	t.Parallel()
	s, _, _, agent, _ := wakeFixture(t)
	ctx := t.Context()
	harness, err := s.SaveLauncher(ctx, "owner", Launcher{MachineID: agent.MachineID, Tool: "codex", Name: "native", Label: "Codex", Command: "codex", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceHarnessModels(ctx, "owner", agent.MachineID, harness.ID, []HarnessModel{{Slug: "gpt-a", Label: "A", Default: true}, {Slug: "gpt-b", Label: "B"}}); err != nil {
		t.Fatal(err)
	}
	settle := func(launcherID, model string) string {
		t.Helper()
		var got string
		if err := s.Tx(ctx, func(tx *Tx) error {
			var err error
			got, err = tx.settleAgentModel(agent.ID, launcherID, model)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := settle(harness.ID, "gpt-b"); got != "gpt-b" {
		t.Fatalf("a listed model stays: %q", got)
	}
	if got := settle("", "anything"); got != "anything" {
		t.Fatalf("no harness: %q", got)
	}
	var logged int
	count := func() int {
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM machine_launcher_activity WHERE launcher_id=? AND action='model_default'`, harness.ID).Scan(&logged); err != nil {
			t.Fatal(err)
		}
		return logged
	}
	if count() != 0 {
		t.Fatalf("nothing was replaced yet: %d", logged)
	}
	if got := settle(harness.ID, "gpt-gone"); got != "gpt-a" {
		t.Fatalf("a removed model becomes the default: %q", got)
	}
	var model string
	if err := s.db.QueryRow(`SELECT model FROM agent WHERE id=?`, agent.ID).Scan(&model); err != nil || model != "gpt-a" {
		t.Fatalf("the agent row shows the new model: %q %v", model, err)
	}
	var before, after string
	if err := s.db.QueryRow(`SELECT before_json,after_json FROM machine_launcher_activity WHERE launcher_id=? AND action='model_default'`, harness.ID).Scan(&before, &after); err != nil || before != `["gpt-gone"]` || after != `["gpt-a"]` {
		t.Fatalf("activity: %q %q %v", before, after, err)
	}
	// A harness with no list changes nothing.
	empty, err := s.SaveLauncher(ctx, "owner", Launcher{MachineID: agent.MachineID, Tool: "pi", Name: "native", Label: "Pi", Command: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	if got := settle(empty.ID, "whatever"); got != "whatever" {
		t.Fatalf("no list: %q", got)
	}
}

// The log of migration 69 says what it removed, what it kept and why, and writes out in full a model list that is dropped.
func TestHarnessMigrationFactsDescribeWhatChanges(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(t.TempDir(), "facts.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := migrateUpTo(context.Background(), db, 68); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO machine(id,name,key_hash,approved,is_host) VALUES ('mch_a','host','k',1,1)`,
		`INSERT INTO machine_launcher(id,machine_id,tool,name,command,enabled,containerized,supports_toolbox,preset) VALUES ('l_oc','mch_a','opencode','native','opencode',0,0,1,1)`,
		`INSERT INTO machine_launcher(id,machine_id,tool,name,command,enabled,containerized,supports_toolbox,preset) VALUES ('l_cx','mch_a','codex','native','codex',0,0,1,1)`,
		`INSERT INTO machine_launcher(id,machine_id,tool,name,command,enabled,containerized,supports_toolbox,preset) VALUES ('l_cl','mch_a','claude','native','claude',1,0,1,1)`,
		`INSERT INTO machine_launcher(id,machine_id,tool,name,command,enabled,containerized,supports_toolbox,preset) VALUES ('l_pi','mch_a','pi','native','pi',0,0,1,1)`,
		`INSERT INTO agent_switch(agent_id,room_id,tool,variant,launcher_id,model,provider,state,requested_at) VALUES ('agt_y','room_y','claude','native','l_cl','claude-custom','anthropic','draining','now')`,
		`DELETE FROM spawn_model`,
		// pi's list has the seeded id but the owner renamed it: that is an edit too.
		`INSERT INTO spawn_model(tool,model_id,label,position) VALUES ('opencode','ollama-cloud/gemma4:31b','Gemma 4 31B',0),('codex','gpt-x','GPT X',0),('claude','sonnet','Sonnet',0),('pi','ollama-cloud/gemma4:31b','Gemma, renamed',0)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	lines, err := harnessMigrationFacts(context.Background(), tx)
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(lines, "\n")
	for _, want := range []string{
		"1 unused seeded launchers deleted (host opencode/native)",
		"the pi model list was edited and no launcher of that tool is enabled or used: one disabled harness is kept so it survives (0:ollama-cloud/gemma4:31b=\"Gemma, renamed\")",
		"the opencode model list is dropped with its launchers (position:slug=label): 0:ollama-cloud/gemma4:31b=\"Gemma 4 31B\"",
		"the codex model list was edited and no launcher of that tool is enabled or used: one disabled harness is kept so it survives (0:gpt-x=\"GPT X\")",
		"host claude/native: claude-custom",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("the log lacks %q:\n%s", want, all)
		}
	}
}

// When both seeded presets of a tool are unused and its list was edited, the harness kept for the list is the native one, not the Docker
// wrapper that happens to sort first by name.
func TestMigrationKeepsTheNativeHarnessForAnEditedList(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(t.TempDir(), "native.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := migrateUpTo(context.Background(), db, 68); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO machine(id,name,key_hash,approved,is_host) VALUES ('mch_a','host','k',1,1)`,
		// "harness-docker" sorts before "native" by name; only the containerized flag says which is which.
		`INSERT INTO machine_launcher(id,machine_id,tool,name,command,enabled,containerized,supports_toolbox,preset) VALUES ('l_docker','mch_a','codex','harness-docker','codex-docker',0,1,1,1)`,
		`INSERT INTO machine_launcher(id,machine_id,tool,name,command,enabled,containerized,supports_toolbox,preset) VALUES ('l_native','mch_a','codex','native','codex',0,0,1,1)`,
		`DELETE FROM spawn_model`,
		`INSERT INTO spawn_model(tool,model_id,label,position) VALUES ('codex','gpt-x','GPT X',0)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var ids []string
	rows, err := db.Query(`SELECT id FROM machine_launcher ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if strings.Join(ids, " ") != "l_native" {
		t.Fatalf("the kept harness should be the native one only, got %v", ids)
	}
	var slug string
	if err := db.QueryRow(`SELECT slug FROM harness_model WHERE launcher_id='l_native' AND is_default=1`).Scan(&slug); err != nil || slug != "gpt-x" {
		t.Fatalf("the edited list lives on the native harness: %q %v", slug, err)
	}
}

// An edit sends the revision of the list it was based on; one over a list that changed in the meantime is refused and changes nothing,
// while an edit without a revision still replaces the list.
func TestReplaceHarnessModelsIfRevRefusesAStaleList(t *testing.T) {
	t.Parallel()
	s, _, _, agent, _ := wakeFixture(t)
	ctx := t.Context()
	harness, err := s.SaveLauncher(ctx, "owner", Launcher{MachineID: agent.MachineID, Tool: "codex", Name: "native", Label: "Codex", Command: "codex", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	empty := HarnessModelsRev(nil)
	first := []HarnessModel{{Slug: "gpt-a", Label: "A", Default: true}}
	if err := s.ReplaceHarnessModelsIfRev(ctx, "owner", agent.MachineID, harness.ID, first, empty); err != nil {
		t.Fatalf("an edit over the list it read: %v", err)
	}
	rev := HarnessModelsRev(first)
	if rev == empty || rev != HarnessModelsRev([]HarnessModel{{Slug: "gpt-a", Label: "A", Default: true}}) {
		t.Fatalf("the revision must follow the content: %q %q", rev, empty)
	}
	for name, other := range map[string][]HarnessModel{"label": {{Slug: "gpt-a", Label: "B", Default: true}}, "note": {{Slug: "gpt-a", Label: "A", Note: "n", Default: true}}, "order": {{Slug: "gpt-b", Label: "A"}, {Slug: "gpt-a", Label: "A", Default: true}}} {
		if HarnessModelsRev(other) == rev {
			t.Errorf("a change of %s must change the revision", name)
		}
	}
	// Someone else edits the list: the first editor's revision is now stale.
	if err := s.ReplaceHarnessModelsIfRev(ctx, "owner", agent.MachineID, harness.ID, []HarnessModel{{Slug: "gpt-a", Label: "A", Default: true}, {Slug: "gpt-c", Label: "C"}}, rev); err != nil {
		t.Fatalf("the other editor: %v", err)
	}
	err = s.ReplaceHarnessModelsIfRev(ctx, "owner", agent.MachineID, harness.ID, []HarnessModel{{Slug: "gpt-z", Label: "Z", Default: true}}, rev)
	if !errors.Is(err, ErrStaleHarnessModels) {
		t.Fatalf("a stale edit: %v", err)
	}
	if kept, _ := s.HarnessModels(ctx, harness.ID); len(kept) != 2 || kept[1].Slug != "gpt-c" {
		t.Fatalf("a refused edit must change nothing: %+v", kept)
	}
	// An invalid list on a current revision is still the usual 400, and without a revision the list is replaced.
	current := HarnessModelsRev([]HarnessModel{{Slug: "gpt-a", Label: "A", Default: true}, {Slug: "gpt-c", Label: "C"}})
	if err := s.ReplaceHarnessModelsIfRev(ctx, "owner", agent.MachineID, harness.ID, []HarnessModel{{Slug: "bad id", Label: "x", Default: true}}, current); !errors.Is(err, ErrInvalidHarnessModels) {
		t.Fatalf("invalid on a current revision: %v", err)
	}
	if err := s.ReplaceHarnessModels(ctx, "owner", agent.MachineID, harness.ID, []HarnessModel{{Slug: "gpt-z", Label: "Z", Default: true}}); err != nil {
		t.Fatalf("no revision: %v", err)
	}
}
