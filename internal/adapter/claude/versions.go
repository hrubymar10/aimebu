package claude

import (
	"fmt"
	"strconv"
	"strings"
)

// cliFeature is one thing the adapter passes to, or reads from, the Claude Code CLI whose support depends on the CLI's version.
type cliFeature string

const (
	// featureReplayUserMessages is --replay-user-messages: the CLI repeats each stream-json input line, which is how a warm turn tells its own
	// result from one of the CLI's own queries (see ignoreResult). A warm process needs it, so a CLI without it runs every turn cold.
	featureReplayUserMessages cliFeature = "--replay-user-messages"
	// featureMaxBudgetUSD is --max-budget-usd, passed only when the owner set a budget. A budget is a spending cap, so a CLI without the flag
	// refuses the turn instead of running it uncapped.
	featureMaxBudgetUSD cliFeature = "--max-budget-usd"
	// featureDisableBackgroundTasks is the CLAUDE_CODE_DISABLE_BACKGROUND_TASKS environment variable (see backgroundTasksEnv). An older CLI
	// would ignore it; it is left out so the environment says only what the CLI reads.
	featureDisableBackgroundTasks cliFeature = backgroundTasksEnvVar
)

// cliFeatureRow is one row of cliFeatures.
type cliFeatureRow struct {
	feature cliFeature
	kind    string // "flag", "env", "event" or "control"
	since   cliVersion
	source  string // where since comes from
}

// cliFeatures is which Claude Code CLI version first supports each version-dependent flag, environment variable, stream event and control
// request the adapter uses; a launch for a known CLI version passes only what that version supports (cliVersion.supports). A new
// capability is one row here and one supports check where the launch builds its arguments.
//
// Every since is the release whose changelog entry (CHANGELOG.md of github.com/anthropics/claude-code) says the feature was added. What
// the adapter uses and has no such entry is the baseline, passed to every version and not listed: -p, --output-format stream-json,
// --verbose, --append-system-prompt, --json-schema, --input-format stream-json, --model, --resume, --session-id, --fork-session,
// --disallowedTools, --mcp-config (inline JSON), --strict-mcp-config, --tools and --permission-mode (bypassPermissions, dontAsk), and of the
// stream the system/init and system/compact_boundary events and the result event's structured_output, modelUsage and origin fields (a
// field an older CLI leaves out is read as unknown). The adapter sends no control requests: a cancel is a signal to the process group.
var cliFeatures = []cliFeatureRow{
	{featureReplayUserMessages, "flag", cliVersion{1, 0, 86}, "1.0.86: SDK: Added --replay-user-messages"},
	{featureMaxBudgetUSD, "flag", cliVersion{2, 0, 28}, "2.0.28: SDK: added --max-budget-usd flag"},
	{featureDisableBackgroundTasks, "env", cliVersion{2, 1, 4}, "2.1.4: Added CLAUDE_CODE_DISABLE_BACKGROUND_TASKS"},
}

// cliVersion is a Claude Code version, major.minor.patch; nil is unknown.
type cliVersion []int

// parseCLIVersion reads the version `claude --version` prints ("2.1.330 (Claude Code)"); anything that does not start with
// major.minor.patch is unknown (nil).
func parseCLIVersion(text string) cliVersion {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(fields[0], "v"), ".")
	if len(parts) != 3 {
		return nil
	}
	version := make(cliVersion, 3)
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return nil
		}
		version[i] = n
	}
	return version
}

func (v cliVersion) String() string {
	if v == nil {
		return "unknown"
	}
	return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])
}

func (v cliVersion) atLeast(other cliVersion) bool {
	for i := range 3 {
		if v[i] != other[i] {
			return v[i] > other[i]
		}
	}
	return true
}

// supports says whether the CLI of version v supports feature. An unknown version supports every feature: the supervisor did not learn the
// version (or it is in a form this does not read), and a launch then passes what it passed before the table existed. A feature that is not
// in cliFeatures is baseline and always supported.
func (v cliVersion) supports(feature cliFeature) bool {
	if v == nil {
		return true
	}
	for _, row := range cliFeatures {
		if row.feature == feature {
			return v.atLeast(row.since)
		}
	}
	return true
}

// since is the first version that supports feature, or nil for a baseline feature.
func featureSince(feature cliFeature) cliVersion {
	for _, row := range cliFeatures {
		if row.feature == feature {
			return row.since
		}
	}
	return nil
}
