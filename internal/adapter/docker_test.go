package adapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
