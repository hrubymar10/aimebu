package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

// blankTestDB is immutable after TestMain initializes it. Every fixture gets
// its own file and connection, so parallel tests share no SQLite state.
var blankTestDB []byte

func TestMain(m *testing.M) {
	home, cleanupHome := testenv.Isolate()
	// The server's supervisor discovers harnesses on PATH: without this these tests ran the real claude, codex, opencode and pi, their docker
	// launchers (about forty times each), as real processes with the person's own configuration.
	testenv.RestrictPATH(home)
	root, err := os.MkdirTemp("", "aimebu-server-tests-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	path := filepath.Join(root, "template.sqlite")
	db, err := store.Open(context.Background(), path, store.Options{})
	if err == nil {
		err = db.Close()
	}
	if err == nil {
		blankTestDB, err = os.ReadFile(path)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		_ = os.RemoveAll(root)
		cleanupHome()
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(root)
	cleanupHome()
	os.Exit(code)
}

// seedTestHarnesses gives a machine the harnesses the old presets used to provide (all disabled, as they were) with the old default
// model lists, so tests that spawn through a harness keep their setup. The product seeds nothing.
func seedTestHarnesses(t *testing.T, db *store.Store, machineID string) {
	t.Helper()
	models := map[string][]store.HarnessModel{
		"claude":   {{Slug: "haiku", Label: "Haiku", Default: true}, {Slug: "sonnet", Label: "Sonnet"}, {Slug: "opus", Label: "Opus"}},
		"codex":    {{Slug: "gpt-6-sol", Label: "GPT-6 Sol", Default: true}, {Slug: "gpt-6-luna", Label: "GPT-6 Luna"}, {Slug: "gpt-6-astra", Label: "GPT-6 Astra"}},
		"pi":       {{Slug: "ollama-cloud/gemma4:31b", Label: "Gemma 4 31B", Default: true}},
		"opencode": {{Slug: "ollama-cloud/gemma4:31b", Label: "Gemma 4 31B", Default: true}},
	}
	for _, item := range []struct {
		tool, name, command  string
		containerized, tool2 bool
	}{
		{"claude", "native", "claude", false, true}, {"claude", "harness-docker", "claude-docker", true, true},
		{"codex", "native", "codex", false, true}, {"codex", "harness-docker", "codex-docker", true, true},
		{"pi", "native", "pi", false, true}, {"pi", "harness-docker", "pi-docker", true, false},
		{"opencode", "native", "opencode", false, true},
	} {
		label := map[string]string{"claude": "Claude Code", "codex": "Codex", "pi": "Pi", "opencode": "OpenCode"}[item.tool]
		if item.containerized {
			label += " (Docker)"
		}
		saved, err := db.SaveLauncher(context.Background(), "test", store.Launcher{MachineID: machineID, Tool: item.tool, Name: item.name, Label: label, Command: item.command,
			Containerized: item.containerized, Toolbox: item.tool2, Args: []string{}, EnvRefs: map[string]string{}, PathMaps: []store.PathMap{}, VisibleRoots: []string{}})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.ReplaceHarnessModels(context.Background(), "test", machineID, saved.ID, models[item.tool]); err != nil {
			t.Fatal(err)
		}
	}
}

// setHarnessModels replaces a harness's model list directly in the store.
func (f *fixture) setHarnessModels(t *testing.T, harness store.Launcher, models ...store.HarnessModel) {
	t.Helper()
	if err := f.db.ReplaceHarnessModels(context.Background(), "test", harness.MachineID, harness.ID, models); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) harness(t *testing.T, tool, name string) store.Launcher {
	t.Helper()
	harness, err := f.db.LauncherByName(context.Background(), f.machineID, tool, name)
	if err != nil {
		t.Fatal(err)
	}
	return harness
}
