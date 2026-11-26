package supervisor

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

func TestApplyLauncherFillsTheSpecFromTheRow(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(root, "work", "repo")
	if err := os.MkdirAll(checkout, 0o700); err != nil {
		t.Fatal(err)
	}
	row := protocol.LauncherRow{
		LauncherDefinition: protocol.LauncherDefinition{ID: "lch_1", Tool: "codex", Command: "codex-docker", Args: []string{"--x"}, EnvRefs: map[string]string{"A": "B"},
			Containerized: true, AgentServerURL: "http://host.docker.internal:9897", VisibleRoots: []string{root},
			PathMaps: []protocol.LauncherPathMap{{Host: filepath.Join(root, "work"), Visible: "/workspace"}}},
		ResolvedCommand: "/bin/codex-docker", Version: "1.2.3", Available: true, CodexHome: "/home/u/.codex",
		Features: []string{"codex_home_shared", adapter.FeatureTurnActivity, containerEndFeature},
	}
	spec := adapter.TurnSpec{Agent: adapter.AgentSpec{Tool: "codex"}, CheckoutPath: checkout}
	facts, err := ApplyLauncher(&spec, row, checkout)
	if err != nil {
		t.Fatal(err)
	}
	if !facts.Containerized || !facts.SharesHome || !facts.ConfirmsEnd {
		t.Fatalf("facts %+v", facts)
	}
	if spec.LauncherPath != "/bin/codex-docker" || !reflect.DeepEqual(spec.LauncherArgs, []string{"--x"}) || spec.LauncherEnvRefs["A"] != "B" || !spec.LauncherContainerized ||
		spec.LauncherCodexHome != "/home/u/.codex" || spec.VisibleCheckoutPath != "/workspace/repo" || spec.AgentServerURL != "http://host.docker.internal:9897" ||
		!spec.LauncherReportsContainer || spec.HarnessVersion != "1.2.3" || len(spec.LauncherPathMaps) != 1 || spec.LauncherSharesClaudeHome {
		t.Fatalf("spec %+v", spec)
	}
	// A Claude launcher shares the home through its own feature, and a native one always does.
	claude := row
	claude.Tool, claude.Features = "claude", []string{"claude_home_shared"}
	cs := adapter.TurnSpec{Agent: adapter.AgentSpec{Tool: "claude"}}
	if facts, err := ApplyLauncher(&cs, claude, checkout); err != nil || !facts.SharesHome || !cs.LauncherSharesClaudeHome || facts.ConfirmsEnd {
		t.Fatalf("claude: %+v %v %+v", facts, err, cs)
	}
	claude.Features = nil
	if facts, _ := ApplyLauncher(&adapter.TurnSpec{Agent: adapter.AgentSpec{Tool: "claude"}}, claude, checkout); facts.SharesHome {
		t.Fatal("a Docker launcher without the feature does not share the host's home")
	}
	native := row
	native.Containerized, native.Features = false, nil
	if facts, _ := ApplyLauncher(&adapter.TurnSpec{Agent: adapter.AgentSpec{Tool: "codex"}}, native, checkout); !facts.SharesHome || facts.Containerized {
		t.Fatalf("native: %+v", facts)
	}
}

func TestApplyLauncherRefuses(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	row := protocol.LauncherRow{LauncherDefinition: protocol.LauncherDefinition{VisibleRoots: []string{filepath.Join(root, "other")}}, Available: true}
	if _, err := ApplyLauncher(&adapter.TurnSpec{}, row, root); err == nil {
		t.Fatal("a checkout outside the launcher's visible roots is refused")
	}
	row.Available = false
	if _, err := ApplyLauncher(&adapter.TurnSpec{}, row, root); err == nil {
		t.Fatal("an unavailable launcher is refused")
	}
}
