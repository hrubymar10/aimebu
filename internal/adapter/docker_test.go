package adapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDockerMCPReadsPrivateConfigWithoutCopyingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"mcpServers":{"aimebu":{"env":{"AIMEBU2_URL":"http://127.0.0.1:9897","AIMEBU2_AGENT_ID":"agt_one","AIMEBU2_AGENT_KEY":"aak_secret"}}}}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	endpoint, header, err := DockerMCP(path, "agt_one")
	if err != nil || endpoint != "http://host.docker.internal:9897/api/agent/mcp" || header != "Bearer aak_secret" {
		t.Fatalf("endpoint=%q header=%q err=%v", endpoint, header, err)
	}
	if _, _, err := DockerMCP(path, "agt_other"); err == nil {
		t.Fatal("accepted a mismatched agent ID")
	}
}

func TestDockerPreflightRejectsUnmountedCheckout(t *testing.T) {
	root := t.TempDir()
	launcher := filepath.Join(root, "claude-docker")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\necho 'Directory is not mounted in the harness container' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	checkout := filepath.Join(root, "checkout")
	if err := os.Mkdir(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	err := DockerPreflight("claude", checkout)
	if err == nil || !strings.Contains(err.Error(), "not mounted") {
		t.Fatalf("preflight=%v", err)
	}
}

// A launcher that passes its check and leaves a helper holding the output pipe passed: refusing the spawn for it would turn a working
// launcher into an unusable one. (The check also must not wait for the helper.)
func TestDockerPreflightPassesForALauncherThatLeavesAHelperBehind(t *testing.T) {
	root := t.TempDir()
	launcher := filepath.Join(root, "claude-docker")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\necho 'claude 1.0'\nsleep 4 &\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	checkout := filepath.Join(root, "checkout")
	if err := os.Mkdir(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := DockerPreflight("claude", checkout); err != nil {
		t.Fatalf("a launcher that exited 0 failed its preflight: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("the preflight took %v: it waited for the launcher's helper", elapsed)
	}
}
