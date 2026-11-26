package supervisor

import (
	"errors"
	"slices"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// LauncherFacts is what a delivery learns from the launcher its agent runs through, beyond the fields ApplyLauncher puts in the turn spec.
type LauncherFacts struct {
	// Containerized says the launcher runs the harness in a container.
	Containerized bool
	// SharesHome says the launcher's login is the host's own file: a native launcher always, a Docker launcher only when it reports the
	// feature that shares the harness's home (claude_home_shared, codex_home_shared).
	SharesHome bool
	// ConfirmsEnd says a Docker launcher's wrapper confirms the end of a session in its container, which a warm process needs.
	ConfirmsEnd bool
}

// ApplyLauncher fills the launcher fields of a turn spec from a launcher row, as every delivery does: the resolved command, its arguments and
// environment references, the path maps, the checkout as the launcher sees it, the features it reports, the agent server address and the
// harness version. spec.Agent.Tool and spec.CheckoutPath must already be set. It never changes anything but the spec, and it refuses a launcher
// that is not available or whose roots do not hold the checkout. The eval runner calls it too, so a turn it runs is launched the way an agent's
// is.
func ApplyLauncher(spec *adapter.TurnSpec, row protocol.LauncherRow, checkout string) (LauncherFacts, error) {
	if !row.Available {
		return LauncherFacts{}, errors.New("launcher unavailable on this machine")
	}
	visible, err := launcherCheckout(row.LauncherDefinition, checkout)
	if err != nil {
		return LauncherFacts{}, err
	}
	spec.LauncherPath = row.ResolvedCommand
	spec.LauncherArgs = append([]string(nil), row.Args...)
	spec.LauncherEnvRefs = row.EnvRefs
	spec.LauncherContainerized = row.Containerized
	spec.LauncherSharesClaudeHome = row.Containerized && slices.Contains(row.Features, "claude_home_shared")
	for _, mapping := range row.PathMaps {
		spec.LauncherPathMaps = append(spec.LauncherPathMaps, adapter.LauncherPathMap{Host: mapping.Host, Visible: mapping.Visible})
	}
	if row.Containerized && spec.Agent.Tool == "codex" {
		spec.LauncherCodexHome = row.CodexHome
	}
	if visible != checkout {
		spec.VisibleCheckoutPath = visible
	}
	spec.AgentServerURL = row.AgentServerURL
	feature := "claude_home_shared"
	if spec.Agent.Tool == "codex" {
		feature = "codex_home_shared"
	}
	spec.LauncherReportsContainer = row.Containerized && slices.Contains(row.Features, adapter.FeatureTurnActivity)
	spec.HarnessVersion = row.Version
	return LauncherFacts{
		Containerized: row.Containerized,
		SharesHome:    !row.Containerized || slices.Contains(row.Features, feature),
		ConfirmsEnd:   slices.Contains(row.Features, containerEndFeature),
	}, nil
}
