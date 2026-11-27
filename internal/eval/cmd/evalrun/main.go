// Command evalrun runs the live scenarios of internal/eval on real models through the launchers agents run through, and writes the results and
// the report. It spends subscription quota, so it refuses to run unless AIMEBU_EVAL=1, and it runs a smoke (two scenarios, one model, one
// repetition) unless --full is given. See docs/eval.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/eval"
	"github.com/hrubymar10/aimebu/v2/internal/eval/live"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// fileConfig is the config file: everything a run needs that is the owner's to say.
type fileConfig struct {
	// ScratchDir is a folder the launchers mount; the run makes its checkouts in it.
	ScratchDir string `json:"scratch_dir"`
	// VisibleHost is how a container reaches this machine when it is not host.docker.internal.
	VisibleHost string `json:"visible_host,omitempty"`
	// UsagesURL is a running server's base URL, where GET /api/usages is read; UsagesCookieEnv names the environment variable holding the
	// owner's session cookie. Both are required: a run without a quota reading is refused.
	UsagesURL       string                 `json:"usages_url"`
	UsagesCookieEnv string                 `json:"usages_cookie_env"`
	Targets         []live.Target          `json:"targets"`
	Launchers       []protocol.LauncherRow `json:"launchers"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "evalrun:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "", "the config file (required)")
	full := flag.Bool("full", false, "run every scenario on every target; without it, run the smoke: s01 and s15, the first target, one repetition")
	reps := flag.Int("reps", 3, "repetitions of a case in a full run")
	only := flag.String("scenarios", "", "comma-separated scenario ids (full run)")
	targets := flag.String("targets", "", "comma-separated target names (default: the first for a smoke, all for a full run)")
	maxUSD := flag.Float64("max-usd", 30, "stop when the cost the harnesses report passes this; 0 is no cap")
	stopAbove := flag.Float64("stop-above", 90, "stop when a weekly quota window is above this percent")
	out := flag.String("out", "tmp/eval", "where the run's folder is made")
	flag.Parse()
	if os.Getenv("AIMEBU_EVAL") != "1" {
		return fmt.Errorf("refusing to run: a live run spends subscription quota; set AIMEBU_EVAL=1")
	}
	if *configPath == "" {
		return fmt.Errorf("--config is required (see docs/eval.md)")
	}
	var config fileConfig
	data, err := os.ReadFile(*configPath)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("%s: %w", *configPath, err)
	}
	if config.ScratchDir == "" || config.UsagesURL == "" || len(config.Targets) == 0 || len(config.Launchers) == 0 {
		return fmt.Errorf("the config needs scratch_dir, usages_url, targets and launchers")
	}
	scenarios, err := eval.LoadDir("internal/eval/testdata/live")
	if err != nil {
		return fmt.Errorf("run from the repository root: %w", err)
	}
	if err := eval.ValidateAll(scenarios); err != nil {
		return err
	}
	selected, repetitions := live.PickScenarios(scenarios, *full, *only), *reps
	chosen := config.Targets
	if !*full {
		chosen, repetitions = config.Targets[:1], 1
		selected = live.PickScenarios(scenarios, false, "")
	}
	if *targets != "" {
		chosen = nil
		for _, name := range strings.Split(*targets, ",") {
			found := false
			for _, t := range config.Targets {
				if t.Name == name {
					chosen, found = append(chosen, t), true
				}
			}
			if !found {
				return fmt.Errorf("no target %q in the config", name)
			}
		}
	}
	if len(selected) == 0 {
		return fmt.Errorf("no scenario selected")
	}
	mcp, err := live.StartMCP()
	if err != nil {
		return err
	}
	defer mcp.Close()
	runID := time.Now().UTC().Format("20060102T150405Z")
	dir := filepath.Join(*out, runID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	options := &live.Options{RunID: runID, ScratchDir: config.ScratchDir, Targets: chosen, Launchers: config.Launchers, Scenarios: selected, Reps: repetitions,
		MaxUSD: *maxUSD, StopAbovePct: *stopAbove, VisibleHost: config.VisibleHost, MCP: mcp, Log: os.Stdout,
		Quota:      &live.QuotaSource{URL: config.UsagesURL, CookieEnv: config.UsagesCookieEnv},
		NewAdapter: live.NewDockerAdapter, Preflight: live.DockerPreflight}
	fmt.Printf("run %s: %d scenario(s) x %d target(s) x %d repetition(s), cap $%.2f, results in %s\n", runID, len(selected), len(chosen), repetitions, *maxUSD, dir)
	cases, runErr := options.Run(ctx)
	mode := "smoke"
	if *full {
		mode = "full run"
	}
	report := options.Report(fmt.Sprintf("Eval %s (%s)", runID, mode), cases, selected, runErr)
	if err := live.WriteResults(dir, report, cases); err != nil {
		return err
	}
	fmt.Println(report)
	return runErr
}
