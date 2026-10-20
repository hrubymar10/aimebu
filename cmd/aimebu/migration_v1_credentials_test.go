package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCarryV1CredentialTargetsIsAtomicAndDoesNotOverwrite(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, v1BackupName, "server", credentialTargetsFile)
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	const original = `{"version":1,"targets":[{"id":"external","tool":"claude","path":"/tmp/target"}]}`
	if err := os.WriteFile(source, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	carried, err := carryV1CredentialTargets(root)
	if err != nil || !carried {
		t.Fatalf("carry = %v, %v", carried, err)
	}
	target := filepath.Join(root, "server", credentialTargetsFile)
	if got, err := os.ReadFile(target); err != nil || string(got) != original {
		t.Fatalf("target = %q, %v", got, err)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("target mode = %v, %v", info, err)
	}
	if carried, err := carryV1CredentialTargets(root); err != nil || carried {
		t.Fatalf("second carry = %v, %v", carried, err)
	}
	if err := os.WriteFile(target, []byte("v2 choice"), 0o600); err != nil {
		t.Fatal(err)
	}
	if carried, err := carryV1CredentialTargets(root); err != nil || carried {
		t.Fatalf("existing v2 registry overwritten: %v, %v", carried, err)
	}
	if got, _ := os.ReadFile(target); string(got) != "v2 choice" {
		t.Fatalf("existing registry = %q", got)
	}
}

func TestDoctorWarnsWhenV1CredentialTargetsWereNotCarried(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, v1BackupName, "server", credentialTargetsFile)
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	checks := checkV1CredentialTargetsCarryOver(root)
	if len(checks) != 1 || checks[0].Status != statusWarn {
		t.Fatalf("missing carry-over checks = %+v", checks)
	}
	if _, err := carryV1CredentialTargets(root); err != nil {
		t.Fatal(err)
	}
	if checks := checkV1CredentialTargetsCarryOver(root); len(checks) != 0 {
		t.Fatalf("carried registry still warns: %+v", checks)
	}
}

func TestDoctorFailsWhenAnotherRootOwnsTheSameTool(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	current := filepath.Join(home, ".aimebu2")
	other := filepath.Join(home, ".aimebu", "switcher")
	for _, root := range []string{filepath.Join(current, "switcher"), other} {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "token-copies.json"), []byte(`{"version":2,"migrated":{"claude":"2026-10-17T00:00:00Z"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	checks := checkTokenRootConflicts(current)
	if len(checks) != 1 || checks[0].Status != statusFail || checks[0].Name != "credential refresher claude" {
		t.Fatalf("conflict checks = %+v", checks)
	}
}
