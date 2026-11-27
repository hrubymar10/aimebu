package live

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/claude"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/codex"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/opencode"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/pi"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/vibe"
	"github.com/hrubymar10/aimebu/v2/internal/eval"
)

// The two scenarios of a smoke.
var smokeScenarios = []string{"s01-plain-reply", "s15-waiting-on-command"}

// PickScenarios is the scenarios of a run: the two of a smoke, the named ones (comma separated), or all.
func PickScenarios(all []eval.Scenario, full bool, names string) []eval.Scenario {
	want := map[string]bool{}
	switch {
	case !full:
		for _, id := range smokeScenarios {
			want[id] = true
		}
	case names != "":
		for _, n := range strings.Split(names, ",") {
			want[strings.TrimSpace(n)] = true
		}
	default:
		return all
	}
	var out []eval.Scenario
	for _, s := range all {
		if want[s.ID] {
			out = append(out, s)
		}
	}
	return out
}

// NewDockerAdapter is the Docker adapter of a tool, the one the supervisor starts for an agent on a Docker launcher. It does not ask any CLI for its
// version (adapter.NewMulti would): the launcher is the owner's and was inspected there.
func NewDockerAdapter(tool string) (adapter.Adapter, error) {
	switch tool {
	case eval.KindClaude:
		a := claude.NewDocker()
		a.StandIn = true
		return a, nil
	case eval.KindCodex:
		return codex.NewDocker(), nil
	case eval.KindPi:
		return pi.NewDocker(), nil
	case eval.KindOpenCode:
		return opencode.NewDocker(), nil
	case eval.KindVibe:
		return vibe.NewDocker(), nil
	}
	return nil, fmt.Errorf("unknown tool %q", tool)
}

// DockerPreflight is Options.Preflight for the real run.
func DockerPreflight(tool, checkout, launcher string) error {
	return adapter.DockerPreflight(tool, checkout, launcher)
}

// Report is the markdown report of a finished (or stopped) run: the eval report, the cost and the quota before and after.
func (o *Options) Report(title string, cases []eval.Case, scenarios []eval.Scenario, runErr error) string {
	report := eval.Render(title, cases, scenarios)
	report += fmt.Sprintf("\n## Cost\n\nThe harnesses reported $%.2f (an estimate for a subscription; the cap was $%.2f).\n", o.Spent(), o.MaxUSD)
	report += "\n## Quota\n\nShare used of each window the server reports, before and after the run.\n\n" + Table(o.QuotaBefore, o.QuotaAfter)
	if runErr != nil {
		report += fmt.Sprintf("\n## The run stopped early\n\n%v\n", runErr)
	}
	return report
}

// WriteResults writes report.md and results.json (mode 0600) in dir, which must exist.
func WriteResults(dir, report string, cases []eval.Case) error {
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte(report), 0o600); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cases, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "results.json"), raw, 0o600)
}
