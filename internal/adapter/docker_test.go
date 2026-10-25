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

// The feature probe: a launcher that has it answers with its features before it touches Docker; one that predates it runs the harness's
// --version, whose answer names no feature. The probe always carries --version so the older launcher only reports one.
func TestDockerFeaturesTellANewLauncherFromAnOldOne(t *testing.T) {
	root := t.TempDir()
	probing := filepath.Join(root, "new-launcher")
	if err := os.WriteFile(probing, []byte("#!/bin/sh\nif [ \"$HARNESS_DOCKER_PRINT_FEATURES\" = 1 ]; then printf 'forward_env\\nsomething_else\\n'; exit 0; fi\necho 'not the probe' >&2; exit 9\n"), 0700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(root, "old-launcher")
	if err := os.WriteFile(old, []byte("#!/bin/sh\ncase \"$*\" in *--version*) echo 1.19.4;; *) echo 'started the harness' >&2; exit 9;; esac\n"), 0700); err != nil {
		t.Fatal(err)
	}
	features, err := DockerFeatures(t.Context(), probing, []string{"--wrapper-flag"}, root, os.Environ())
	if err != nil || !features[FeatureForwardEnv] || !features["something_else"] || len(features) != 2 {
		t.Fatalf("features=%v err=%v", features, err)
	}
	features, err = DockerFeatures(t.Context(), old, nil, root, os.Environ())
	if err != nil || len(features) != 0 {
		t.Fatalf("an older launcher reports no feature: %v %v", features, err)
	}
	if err := RequireDockerFeature(t.Context(), "OpenCode", old, nil, root, os.Environ(), FeatureForwardEnv); err == nil ||
		!strings.Contains(err.Error(), "too old") || !strings.Contains(err.Error(), "HARNESS_DOCKER_FORWARD_ENV") {
		t.Fatalf("an older launcher is refused with the way out: %v", err)
	}
	if err := RequireDockerFeature(t.Context(), "OpenCode", probing, nil, root, os.Environ(), FeatureForwardEnv); err != nil {
		t.Fatalf("a launcher with the feature passes: %v", err)
	}
	failing := filepath.Join(root, "failing")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\necho 'Docker is not running.' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := DockerFeatures(t.Context(), failing, nil, root, os.Environ()); err == nil || !strings.Contains(err.Error(), "Docker is not running") {
		t.Fatalf("a probe that fails says why: %v", err)
	}
	if !DockerNeedsForwardEnv("opencode") || !DockerNeedsForwardEnv("vibe") || DockerNeedsForwardEnv("claude") || DockerNeedsForwardEnv("pi") {
		t.Fatal("only the tools configured through the environment need the feature")
	}
}

func TestDockerPreflightOfOpenCodeNeedsALauncherThatForwards(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(root, "checkout")
	if err := os.Mkdir(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(root, "opencode-docker")
	if err := os.WriteFile(old, []byte("#!/bin/sh\necho 1.19.4\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := DockerPreflight("opencode", checkout, old); err == nil || !strings.Contains(err.Error(), "too old") {
		t.Fatalf("an older launcher: %v", err)
	}
	if err := os.WriteFile(old, []byte("#!/bin/sh\nif [ \"$HARNESS_DOCKER_PRINT_FEATURES\" = 1 ]; then echo forward_env; exit 0; fi\n[ \"${OPENCODE_LAUNCH_FLAGS+x}\" = x ] && echo 1.19.4\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := DockerPreflight("opencode", checkout, old); err != nil {
		t.Fatalf("a launcher with the feature: %v", err)
	}
	if err := DockerPreflight("cursor", checkout, old); err == nil {
		t.Fatal("an unknown tool is refused")
	}
}
