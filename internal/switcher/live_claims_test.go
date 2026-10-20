package switcher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Two refreshers whose roots are anywhere (not the two default ones) but whose live credential file is the same one.
func TestLiveClaimsAreStructuralNotPerRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	roots := []string{filepath.Join(t.TempDir(), "x", "switcher"), filepath.Join(t.TempDir(), "y", "switcher")}
	var claims []*LiveClaims
	for _, root := range roots {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "token-copies.json"), []byte(`{"version":2,"migrated":{"claude":"2026-10-17T00:00:00Z"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		m, err := NewReadOnly(root)
		if err != nil {
			t.Fatal(err)
		}
		m.SetHomeDir(home)
		claims = append(claims, NewLiveClaims(m))
	}
	first, second := claims[0], claims[1]
	defer first.Release()
	defer second.Release()
	first.Refresh()
	second.Refresh()
	if reason := first.Blocked(ToolClaude); reason != "" {
		t.Fatalf("first refresher blocked: %s", reason)
	}
	reason := second.Blocked(ToolClaude)
	if !strings.Contains(reason, "another token refresher already owns") || !strings.Contains(reason, "root="+roots[0]) {
		t.Fatalf("second refresher should name the holder: %q", reason)
	}
	if second.Blocked(ToolCodex) != "" {
		t.Fatal("a tool outside the token-copy model must not be claimed or blocked")
	}
	// The holder keeps it across passes, and a removed claim file is re-established, not silently lost.
	path, err := first.mgr.LiveClaimPath(ToolClaude)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	first.Refresh()
	second.Refresh()
	if first.Blocked(ToolClaude) != "" || second.Blocked(ToolClaude) == "" {
		t.Fatalf("after the claim file was removed: first=%q second=%q", first.Blocked(ToolClaude), second.Blocked(ToolClaude))
	}
	// A dead holder frees the live file for the next pass.
	first.Release()
	second.Refresh()
	if reason := second.Blocked(ToolClaude); reason != "" {
		t.Fatalf("not taken over after the holder released: %s", reason)
	}
}

// Migrating a tool between two timer passes must not leave a window in which the refresher runs unclaimed.
func TestLiveClaimsAreTakenSynchronouslyWhenAToolIsMigrated(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	mk := func() (*LiveClaims, string) {
		root := filepath.Join(t.TempDir(), "switcher")
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		m, err := NewReadOnly(root)
		if err != nil {
			t.Fatal(err)
		}
		m.SetHomeDir(home)
		return NewLiveClaims(m), root
	}
	migrate := func(root string) {
		if err := os.WriteFile(filepath.Join(root, "token-copies.json"), []byte(`{"version":2,"migrated":{"claude":"2026-10-17T00:00:00Z"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	holder, holderRoot := mk()
	late, lateRoot := mk()
	defer holder.Release()
	defer late.Release()
	holder.Refresh()
	late.Refresh() // neither is migrated yet: nothing is claimed or blocked
	migrate(holderRoot)
	if reason := holder.Blocked(ToolClaude); reason != "" {
		t.Fatalf("the first migrated refresher must get the claim at once: %s", reason)
	}
	migrate(lateRoot) // no Refresh in between: the very next question already knows
	if reason := late.Blocked(ToolClaude); !strings.Contains(reason, "another token refresher already owns") {
		t.Fatalf("a refresher migrated between ticks ran unclaimed: %q", reason)
	}
	// A harness directory that does not exist (the harness never ran here, or it was removed) is created for the claim
	// instead of blocking the tool: a block would stop every stored profile from refreshing and let their tokens expire.
	if err := os.RemoveAll(filepath.Join(home, ".claude")); err != nil {
		t.Fatal(err)
	}
	other, otherRoot := mk()
	defer other.Release()
	migrate(otherRoot)
	if reason := other.Blocked(ToolClaude); reason != "" {
		t.Fatalf("a missing live credential directory must not block the tool: %s", reason)
	}
	if info, err := os.Stat(filepath.Join(home, ".claude")); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("the live credential directory was not created 0700: %v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", ".credentials.json"+liveClaimSuffix)); err != nil {
		t.Fatalf("the claim file was not created next to the live file: %v", err)
	}
}
