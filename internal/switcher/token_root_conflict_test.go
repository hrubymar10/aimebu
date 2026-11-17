package switcher

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultSwitcherRootUsesSharedDataFolder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AIMEBU_CONFIG_DIR", "")
	got, err := DefaultRoot()
	if want := filepath.Join(home, ".aimebu", "switcher"); err != nil || got != want {
		t.Fatalf("default switcher root = %q, %v; want %q", got, err, want)
	}
}

func TestTokenCopyConflictsArePerToolAcrossDefaultRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	currentRoot := filepath.Join(home, ".aimebu2", "switcher")
	otherRoot := filepath.Join(home, ".aimebu", "switcher")
	for _, root := range []string{currentRoot, otherRoot} {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	current, err := NewReadOnly(currentRoot)
	if err != nil {
		t.Fatal(err)
	}
	current.SetHomeDir(home)
	if conflicts, err := current.TokenCopyConflicts(); err != nil || len(conflicts) != 0 {
		t.Fatalf("without markers = %v, %v", conflicts, err)
	}
	for _, root := range []string{currentRoot, otherRoot} {
		if err := os.WriteFile(filepath.Join(root, "token-copies.json"), []byte(`{"version":2,"migrated":{"claude":"2026-10-17T00:00:00Z"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	conflicts, err := current.TokenCopyConflicts()
	if err != nil || len(conflicts) != 1 || conflicts[ToolClaude] != otherRoot || conflicts[ToolCodex] != "" {
		t.Fatalf("per-tool conflict = %v, %v", conflicts, err)
	}
	// The check is symmetric when the v1 root is the server's current root.
	other, err := NewReadOnly(otherRoot)
	if err != nil {
		t.Fatal(err)
	}
	other.SetHomeDir(home)
	conflicts, err = other.TokenCopyConflicts()
	if err != nil || conflicts[ToolClaude] != currentRoot {
		t.Fatalf("reverse conflict = %v, %v", conflicts, err)
	}
}

// The real cutover layout: v2 runs on the v1 default root (~/.aimebu), so the
// "other" default root that equals its own must not count, and an absent dev root
// must not either.
func TestTokenCopyConflictsIgnoreTheSameRootAndAnAbsentOne(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".aimebu", "switcher")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "token-copies.json"), []byte(`{"version":2,"migrated":{"claude":"2026-10-17T00:00:00Z","codex":"2026-10-17T00:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewReadOnly(root)
	if err != nil {
		t.Fatal(err)
	}
	m.SetHomeDir(home)
	if conflicts, err := m.TokenCopyConflicts(); err != nil || len(conflicts) != 0 {
		t.Fatalf("v2 on the v1 default root, no dev root = %v, %v", conflicts, err)
	}
	// A dev root that exists but holds no marker is quiet too.
	if err := os.MkdirAll(filepath.Join(home, ".aimebu2", "switcher"), 0o700); err != nil {
		t.Fatal(err)
	}
	if conflicts, err := m.TokenCopyConflicts(); err != nil || len(conflicts) != 0 {
		t.Fatalf("v2 on the v1 default root, dev root without a marker = %v, %v", conflicts, err)
	}
	// And the dev instance itself stays quiet while it has no marker of its own,
	// whatever v1 holds.
	dev, err := NewReadOnly(filepath.Join(home, ".aimebu2", "switcher"))
	if err != nil {
		t.Fatal(err)
	}
	dev.SetHomeDir(home)
	if conflicts, err := dev.TokenCopyConflicts(); err != nil || len(conflicts) != 0 {
		t.Fatalf("dev root without a marker next to a v1 marker = %v, %v", conflicts, err)
	}
	// A symlinked alias of the same root is still the same root.
	if err := os.RemoveAll(filepath.Join(home, ".aimebu2")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, ".aimebu"), filepath.Join(home, ".aimebu2")); err != nil {
		t.Fatal(err)
	}
	if conflicts, err := m.TokenCopyConflicts(); err != nil || len(conflicts) != 0 {
		t.Fatalf("a symlinked dev root that is the same directory = %v, %v", conflicts, err)
	}
}
