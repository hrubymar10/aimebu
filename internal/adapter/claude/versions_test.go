package claude

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

// pinnedCLI is the version of the Claude Code CLI the adapter was last checked against (see docs/adapter.md).
const pinnedCLI = "2.1.330 (Claude Code)"

func TestParseCLIVersion(t *testing.T) {
	for text, want := range map[string]string{
		pinnedCLI:        "2.1.330",
		"2.1.330":        "2.1.330",
		"v1.0.86\n":      "1.0.86",
		"":               "unknown",
		"Claude Code":    "unknown",
		"2.1":            "unknown",
		"2.1.330-beta.1": "unknown",
		"2.x.1":          "unknown",
		"-1.0.0":         "unknown",
	} {
		if got := parseCLIVersion(text).String(); got != want {
			t.Errorf("parseCLIVersion(%q) = %s, want %s", text, got, want)
		}
	}
}

// The table: the pinned version supports every row, an unknown version is given everything (as before the table), an old version only what
// it had, and the boundary is the row's own version. A feature that is not in the table is baseline.
func TestCLIFeatureTable(t *testing.T) {
	all := []cliFeature{featureReplayUserMessages, featureMaxBudgetUSD, featureDisableBackgroundTasks}
	seen := map[cliFeature]bool{}
	for _, row := range cliFeatures {
		if seen[row.feature] {
			t.Errorf("%s is in the table twice", row.feature)
		}
		seen[row.feature] = true
		if !slices.Contains([]string{"flag", "env", "event", "control"}, row.kind) || len(row.since) != 3 || !strings.HasPrefix(row.source, row.since.String()+": ") {
			t.Errorf("malformed row %+v", row)
		}
	}
	for _, feature := range all {
		if !seen[feature] {
			t.Errorf("%s has no row", feature)
		}
	}
	cases := []struct {
		version string
		want    []cliFeature
	}{
		{pinnedCLI, all},
		{"", all},
		{"not a version", all},
		{"99.0.0", all},
		{"2.1.4", all},
		{"2.1.3", []cliFeature{featureReplayUserMessages, featureMaxBudgetUSD}},
		{"2.0.28", []cliFeature{featureReplayUserMessages, featureMaxBudgetUSD}},
		{"2.0.27", []cliFeature{featureReplayUserMessages}},
		{"1.0.86", []cliFeature{featureReplayUserMessages}},
		{"1.0.85", nil},
		{"0.2.100", nil},
	}
	for _, c := range cases {
		version := parseCLIVersion(c.version)
		var got []cliFeature
		for _, feature := range all {
			if version.supports(feature) {
				got = append(got, feature)
			}
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%q supports %v, want %v", c.version, got, c.want)
		}
		if !version.supports("--json-schema") {
			t.Errorf("%q: a baseline feature must always be supported", c.version)
		}
	}
	if featureSince(featureMaxBudgetUSD).String() != "2.0.28" || featureSince("--json-schema") != nil {
		t.Fatal("featureSince")
	}
}

// The launch flags per version: the pinned CLI and an unknown version get exactly the same process; an older CLI gets no flag or variable
// it does not know. A warm process needs --replay-user-messages, so a CLI without it runs cold, and a set budget it cannot enforce refuses
// the turn instead of running it uncapped.
func TestLaunchFlagsFollowTheCLIVersion(t *testing.T) {
	hasEnv := func(l *launch, item string) bool { return slices.Contains(l.env, item) }
	for _, variant := range []string{"native", "docker"} {
		t.Run(variant, func(t *testing.T) {
			base := warmSpec(t, "turn_v") // one checkout for every launch, so launches differ only by the version
			launchFor := func(t *testing.T, a *Adapter, version string, warm bool) (*launch, error) {
				t.Helper()
				spec := base
				spec.HarnessVersion = version
				return a.prepare(spec, warm)
			}
			a := &Adapter{Command: "claude", Variant: variant, AllowNative: true, MaxBudgetUSD: "5"}
			pinned, err := launchFor(t, a, pinnedCLI, true)
			if err != nil {
				t.Fatal(err)
			}
			unknown, err := launchFor(t, a, "", true)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(pinned.args, unknown.args) || !slices.Equal(pinned.env, unknown.env) {
				t.Fatalf("the pinned CLI must launch as an unknown version does:\n%q\n%q", pinned.args, unknown.args)
			}
			if !slices.Contains(pinned.args, "--replay-user-messages") || !slices.Contains(pinned.args, "--max-budget-usd") || !hasEnv(pinned, backgroundTasksEnv) {
				t.Fatalf("pinned launch: %q %q", pinned.args, pinned.env)
			}
			forwarded := slices.ContainsFunc(pinned.env, func(item string) bool {
				return strings.HasPrefix(item, "HARNESS_DOCKER_FORWARD_ENV=") && strings.Contains(item, backgroundTasksEnvVar)
			})
			if forwarded != (variant == "docker") {
				t.Fatalf("forwarded=%v for %s: %q", forwarded, variant, pinned.env)
			}

			// 2.1.3: no background-tasks switch (it came in 2.1.4), the rest as pinned.
			old, err := launchFor(t, a, "2.1.3", true)
			if err != nil {
				t.Fatal(err)
			}
			if hasEnv(old, backgroundTasksEnv) || slices.ContainsFunc(old.env, func(item string) bool { return strings.Contains(item, backgroundTasksEnvVar) }) {
				t.Fatalf("2.1.3 got %s: %q", backgroundTasksEnvVar, old.env)
			}
			if !slices.Equal(old.args, pinned.args) {
				t.Fatalf("2.1.3 args differ:\n%q\n%q", old.args, pinned.args)
			}
			if old.fingerprint() == pinned.fingerprint() {
				t.Fatal("a different launch must give a different warm fingerprint")
			}

			// 2.0.27: no --max-budget-usd, so a set budget refuses the turn.
			if _, err := launchFor(t, a, "2.0.27", false); err == nil || !strings.Contains(err.Error(), "--max-budget-usd") || !strings.Contains(err.Error(), "2.0.28") {
				t.Fatalf("a budget the CLI cannot enforce: %v", err)
			}
			unbudgeted := &Adapter{Command: "claude", Variant: variant, AllowNative: true}
			cold, err := launchFor(t, unbudgeted, "2.0.27", false)
			if err != nil || slices.Contains(cold.args, "--max-budget-usd") {
				t.Fatalf("2.0.27 without a budget: %v %q", err, cold.args)
			}

			// 1.0.85: no --replay-user-messages, so no warm process; a cold turn still runs.
			if _, err := launchFor(t, unbudgeted, "1.0.85", true); !errors.Is(err, adapter.ErrNoWarm) {
				t.Fatalf("warm on 1.0.85: %v", err)
			}
			spec := base
			spec.HarnessVersion = "1.0.85"
			if _, err := unbudgeted.WarmFingerprint(spec); !errors.Is(err, adapter.ErrNoWarm) {
				t.Fatalf("WarmFingerprint on 1.0.85: %v", err)
			}
			if cold, err := launchFor(t, unbudgeted, "1.0.85", false); err != nil || slices.Contains(cold.args, "--replay-user-messages") {
				t.Fatalf("cold on 1.0.85: %v %q", err, cold.args)
			}
		})
	}
}
