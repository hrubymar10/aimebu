package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/v2/internal/eval"
	"github.com/hrubymar10/aimebu/v2/internal/eval/live"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

const selfTestUsage = "usage: aimebu self-test [--full] [--yes] [--targets a,b] [--max-usd N] [--stop-above PCT] [--scratch DIR]"

// selfTestMaxBody bounds the answer of the server's self-test route; it holds a launcher row and a model list per target.
const selfTestMaxBody = 8 << 20

// Seams for the tests: the stand-in adapter and the run itself. A test never starts a Docker launcher or spends quota.
var (
	selfTestNewAdapter = live.NewDockerAdapter
	selfTestPreflight  = live.DockerPreflight
	selfTestRun        = func(ctx context.Context, o *live.Options) ([]eval.Case, error) { return o.Run(ctx) }
	// selfTestIsTerminal says whether the confirmation can be asked: in is the terminal the person types at.
	selfTestIsTerminal = func(in io.Reader) bool {
		f, ok := in.(*os.File)
		if !ok {
			return false
		}
		info, err := f.Stat()
		return err == nil && info.Mode()&fs.ModeCharDevice != 0
	}
	selfTestNow = time.Now
)

// selfTestCommand implements `aimebu self-test [--full]`: the live answer-robustness eval (docs/eval.md) on every enabled Docker launcher
// and model of the running server. It reads them, and the usage snapshot its quota guard needs, from the server over the same-host doctor
// guard (no config.json, no cookie), shows what it will spend and asks, runs, writes the report under <data root>/self-test/<UTC time>/ and
// exits 1 when a target fails or the run stopped early.
func selfTestCommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("self-test", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	full := flags.Bool("full", false, "")
	yes := flags.Bool("yes", false, "")
	targetsFlag := flags.String("targets", "", "")
	maxUSD := flags.Float64("max-usd", 30, "")
	stopAbove := flags.Float64("stop-above", 90, "")
	scratchFlag := flags.String("scratch", "", "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(stderr, selfTestUsage)
		return 2
	}
	if *maxUSD < 0 || *stopAbove <= 0 || *stopAbove > 100 {
		fmt.Fprintln(stderr, "--max-usd must not be negative and --stop-above must be in (0, 100]")
		return 2
	}
	config, err := resolveServerConfig()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	key, keyCheck, ok := readDoctorKey(config.root)
	if !ok {
		fmt.Fprintf(stderr, "self-test needs a running server: %s\nStart it with `aimebu server start` (the server writes the key at start and removes it at shutdown).\n", keyCheck.Description)
		return 1
	}
	doc, err := fetchSelfTest(ctx, config, key)
	if err != nil {
		fmt.Fprintln(stderr, "self-test:", err)
		return 1
	}
	all, err := eval.LoadEmbedded()
	if err == nil {
		err = eval.ValidateAll(all)
	}
	if err != nil {
		fmt.Fprintln(stderr, "self-test: the scenarios this build carries are invalid:", err)
		return 1
	}
	chosen, err := selfTestTargets(doc, *targetsFlag)
	if err != nil {
		fmt.Fprintln(stderr, "self-test:", err)
		return 1
	}
	selected, reps := live.PickScenarios(all, *full, ""), 1
	mode := "smoke"
	if *full {
		mode, reps = "full run", 3
	}
	rows := doc.Launchers
	scratch, err := selfTestScratch(*scratchFlag, chosen, rows)
	if err != nil {
		fmt.Fprintln(stderr, "self-test:", err)
		return 1
	}
	// A server with no usage snapshot yet (a fresh install) sends null: the quota guard is skipped, with a warning, and the run goes on.
	hasUsages := selfTestHasUsages(doc.Usages)
	var reading live.Reading
	if hasUsages {
		if reading, err = live.ParseUsages(strings.NewReader(string(doc.Usages))); err != nil {
			fmt.Fprintln(stderr, "self-test: the usage snapshot of the server is unreadable:", err)
			return 1
		}
	} else {
		fmt.Fprintln(stderr, "warning: the server has no usage snapshot yet, so the quota guard (--stop-above) is skipped; only --max-usd limits the run")
	}
	cases := selfTestCaseCount(selected, chosen, reps)
	fmt.Fprint(stdout, selfTestSummary(mode, selected, chosen, reps, cases, *maxUSD, *stopAbove, hasUsages, reading, scratch))
	if !*yes {
		if !selfTestIsTerminal(stdin) {
			fmt.Fprintln(stderr, "self-test spends real subscription quota and asks first, but there is no terminal to ask on: pass --yes to run without asking")
			return 1
		}
		fmt.Fprint(stdout, "Run it? [y/N]: ")
		line, _ := bufio.NewReader(stdin).ReadString('\n')
		if answer := strings.ToLower(strings.TrimSpace(line)); answer != "y" && answer != "yes" {
			fmt.Fprintln(stdout, "aborted; nothing was run")
			return 1
		}
	}

	stamp := selfTestNow().UTC().Format("20060102T150405Z")
	if err := os.MkdirAll(filepath.Join(config.root, "self-test"), 0o700); err != nil {
		fmt.Fprintln(stderr, "self-test:", err)
		return 1
	}
	// Two runs in the same second get distinct folders.
	runID, dir := stamp, filepath.Join(config.root, "self-test", stamp)
	for n := 2; ; n++ {
		err := os.Mkdir(dir, 0o700)
		if err == nil {
			break
		}
		if !os.IsExist(err) {
			fmt.Fprintln(stderr, "self-test:", err)
			return 1
		}
		runID = fmt.Sprintf("%s-%d", stamp, n)
		dir = filepath.Join(config.root, "self-test", runID)
	}
	_ = os.Chmod(filepath.Dir(dir), 0o700)
	_ = os.Chmod(dir, 0o700)
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		fmt.Fprintln(stderr, "self-test: scratch folder:", err)
		return 1
	}
	defer selfTestCleanScratch(scratch, runID)
	mcp, err := live.StartMCP()
	if err != nil {
		fmt.Fprintln(stderr, "self-test:", err)
		return 1
	}
	defer mcp.Close()
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	options := &live.Options{RunID: runID, ScratchDir: scratch, Targets: chosen, Launchers: rows, Scenarios: selected, Reps: reps,
		MaxUSD: *maxUSD, StopAbovePct: *stopAbove, MCP: mcp, Log: stdout,
		NewAdapter: selfTestNewAdapter, Preflight: selfTestPreflight}
	if hasUsages {
		options.Quota = &live.QuotaSource{ReadFn: func(ctx context.Context) (live.Reading, error) {
			fresh, err := fetchSelfTest(ctx, config, key)
			if err != nil {
				return live.Reading{}, err
			}
			return live.ParseUsages(strings.NewReader(string(fresh.Usages)))
		}}
	}
	results, runErr := selfTestRun(ctx, options)
	report := options.Report(fmt.Sprintf("Self-test %s (%s)", runID, mode), results, selected, runErr)
	if err := live.WriteResults(dir, report, results); err != nil {
		fmt.Fprintln(stderr, "self-test: writing the report:", err)
		return 1
	}
	passed := selfTestPrint(stdout, chosen, results, selected)
	if runErr != nil {
		fmt.Fprintf(stdout, "\nThe run stopped early: %v\n", runErr)
	}
	fmt.Fprintf(stdout, "\nReport: %s\n", filepath.Join(dir, "report.md"))
	if runErr != nil || !passed {
		return 1
	}
	return 0
}

// selfTestHasUsages says the server sent a usage snapshot (not null or empty).
func selfTestHasUsages(raw json.RawMessage) bool {
	text := strings.TrimSpace(string(raw))
	return text != "" && text != "null"
}

// fetchSelfTest reads GET /api/doctor/self-test of the server the config names, with the doctor key.
func fetchSelfTest(ctx context.Context, config serverConfig, key string) (protocol.DoctorSelfTest, error) {
	requestURL := "http://" + net.JoinHostPort(config.bind, strconv.Itoa(config.port)) + "/api/doctor/self-test"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return protocol.DoctorSelfTest{}, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := doctorHTTPClient(35 * time.Second).Do(req)
	if err != nil {
		return protocol.DoctorSelfTest{}, fmt.Errorf("the server at %s did not answer (is it running? %v)", net.JoinHostPort(config.bind, strconv.Itoa(config.port)), err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden:
		return protocol.DoctorSelfTest{}, errors.New("the server refused the doctor key (the key file belongs to another start of the server, or the request did not reach it from this host directly)")
	case http.StatusNotFound:
		return protocol.DoctorSelfTest{}, errors.New("the server has no /api/doctor/self-test: it is an older build; restart it from this build")
	default:
		return protocol.DoctorSelfTest{}, fmt.Errorf("GET %s: HTTP %d", requestURL, resp.StatusCode)
	}
	var doc protocol.DoctorSelfTest
	if err := json.NewDecoder(io.LimitReader(resp.Body, selfTestMaxBody)).Decode(&doc); err != nil {
		return protocol.DoctorSelfTest{}, fmt.Errorf("the answer of %s is unreadable: %v", requestURL, err)
	}
	return doc, nil
}

// selfTestTargets are the targets of the run: all the server lists, or the --targets names in that order.
func selfTestTargets(doc protocol.DoctorSelfTest, names string) ([]live.Target, error) {
	var all []live.Target
	for _, t := range doc.Targets {
		all = append(all, live.Target{Name: t.Name, Tool: t.Tool, LauncherID: t.LauncherID, Model: t.Model, Providers: t.Providers})
	}
	if len(all) == 0 {
		return nil, errors.New("the server lists no enabled Docker launcher with a model on this machine: nothing to test (Settings → Agents → Harnesses & models)")
	}
	if names == "" {
		return all, nil
	}
	var chosen []live.Target
	for _, name := range strings.Split(names, ",") {
		name = strings.TrimSpace(name)
		found := false
		for _, t := range all {
			if t.Name == name {
				chosen, found = append(chosen, t), true
			}
		}
		if !found {
			var have []string
			for _, t := range all {
				have = append(have, t.Name)
			}
			return nil, fmt.Errorf("no target %q (the server lists: %s)", name, strings.Join(have, ", "))
		}
	}
	return chosen, nil
}

// selfTestScratch is the folder the run makes its checkouts in: --scratch, or an aimebu-self-test folder in the first root every chosen
// launcher mounts (their visible roots), which the launchers need to see the checkout.
func selfTestScratch(flagValue string, targets []live.Target, rows []protocol.LauncherRow) (string, error) {
	if flagValue != "" {
		abs, err := filepath.Abs(flagValue)
		return abs, err
	}
	byID := map[string]protocol.LauncherRow{}
	for _, row := range rows {
		byID[row.ID] = row
	}
	var common []string
	for i, t := range targets {
		row, ok := byID[t.LauncherID]
		if !ok {
			return "", fmt.Errorf("target %s: the server did not send its launcher %s", t.Name, t.LauncherID)
		}
		if i == 0 {
			common = append(common, row.VisibleRoots...)
			continue
		}
		var keep []string
		for _, root := range common {
			if rootsHold(row.VisibleRoots, root) {
				keep = append(keep, root)
			}
		}
		common = keep
	}
	for _, root := range common {
		if info, err := os.Stat(root); err == nil && info.IsDir() {
			return filepath.Join(root, "aimebu-self-test"), nil
		}
	}
	return "", errors.New("the chosen launchers have no folder in common that exists on this machine (their visible roots); pass --scratch with a folder they all mount")
}

// rootsHold says one of the roots is path or a parent of it.
func rootsHold(roots []string, path string) bool {
	for _, root := range roots {
		if rel, err := filepath.Rel(root, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// selfTestCleanScratch removes the checkouts of this run (aimebu-self-test/aimebu-eval-<run>-*) and the scratch folder when it is then empty.
func selfTestCleanScratch(scratch, runID string) {
	matches, _ := filepath.Glob(filepath.Join(scratch, "aimebu-eval-"+runID+"-*"))
	for _, m := range matches {
		_ = os.RemoveAll(m)
	}
	_ = os.Remove(scratch) // only when empty
}

// selfTestCaseCount is the number of cases of a run: a scenario that runs on some harnesses only (only_kinds) skips the other targets.
func selfTestCaseCount(scenarios []eval.Scenario, targets []live.Target, reps int) int {
	n := 0
	for _, t := range targets {
		for _, s := range scenarios {
			if len(s.OnlyKinds) == 0 || containsString(s.OnlyKinds, t.Tool) {
				n += reps
			}
		}
	}
	return n
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// selfTestSummary is what the person reads before saying yes: the targets, the scenarios, the runs, and what it can spend.
func selfTestSummary(mode string, scenarios []eval.Scenario, targets []live.Target, reps, cases int, maxUSD, stopAbove float64, hasUsages bool, reading live.Reading, scratch string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "aimebu self-test (%s) runs real models and spends subscription quota.\n\n", mode)
	fmt.Fprintf(&b, "Targets (%d):\n", len(targets))
	for _, t := range targets {
		fmt.Fprintf(&b, "  %-40s %s on launcher %s\n", t.Name, t.Model, t.LauncherID)
	}
	ids := make([]string, len(scenarios))
	for i, s := range scenarios {
		ids[i] = s.ID
	}
	fmt.Fprintf(&b, "\nScenarios (%d): %s\n", len(scenarios), strings.Join(ids, ", "))
	fmt.Fprintf(&b, "Repetitions: %d\n", reps)
	fmt.Fprintf(&b, "Cases: %d, each up to 2 turns (a reply the parser rejects is sent back once), so at most %d turns.\n", cases, 2*cases)
	if maxUSD > 0 {
		fmt.Fprintf(&b, "Cost cap: $%.2f as the harnesses report it (an equivalent for a subscription, not a charge); the run stops when it is passed.\n", maxUSD)
	} else {
		b.WriteString("Cost cap: none.\n")
	}
	if !hasUsages {
		b.WriteString("Quota guard: skipped, the server has no usage snapshot yet.\n")
	} else {
		fmt.Fprintf(&b, "Quota guard: the run stops when a weekly window of an active profile of these targets' providers passes %.0f%%.\n", stopAbove)
	}
	if name, used := reading.Usage.Hottest(); hasUsages && used >= 0 {
		fmt.Fprintf(&b, "Quota now: the hottest weekly window is %s at %.0f%%.\n", name, used)
	}
	fmt.Fprintf(&b, "Scratch checkouts: %s (removed after the run)\n\n", scratch)
	return b.String()
}

// selfTestPrint prints the per-target summary and reports whether every target passed (every case correct after its repair, and no harness
// failure).
func selfTestPrint(w io.Writer, targets []live.Target, cases []eval.Case, scenarios []eval.Scenario) bool {
	byTarget := map[string][]eval.Case{}
	for _, c := range cases {
		byTarget[c.Model] = append(byTarget[c.Model], c)
	}
	fmt.Fprintf(w, "\nSummary (%d scenario(s)):\n", len(scenarios))
	all := true
	for _, t := range targets {
		cs := byTarget[t.Name]
		firstOK, lastOK := 0, 0
		counts := map[string]int{}
		for _, c := range cs {
			if len(c.Attempts) == 0 {
				continue
			}
			if c.Attempts[0].Correct {
				firstOK++
			}
			if c.Attempts[len(c.Attempts)-1].Correct {
				lastOK++
			}
			for _, a := range c.Attempts {
				if !a.Correct {
					counts[attemptError(a)]++
				}
			}
		}
		pass := len(cs) > 0 && lastOK == len(cs)
		all = all && pass
		verdict := "PASS"
		if !pass {
			verdict = "FAIL"
		}
		rate := func(k int) float64 {
			if len(cs) == 0 {
				return 0
			}
			return 100 * float64(k) / float64(len(cs))
		}
		fmt.Fprintf(w, "  %s %s: correct on the first try %.0f%% (%d/%d), after repairs %.0f%% (%d/%d)\n", verdict, t.Name, rate(firstOK), firstOK, len(cs), rate(lastOK), lastOK, len(cs))
		if len(cs) == 0 {
			fmt.Fprintln(w, "      no case ran")
		}
		for _, e := range topErrors(counts, 3) {
			fmt.Fprintf(w, "      %dx %s\n", e.count, e.text)
		}
	}
	return all
}

// attemptError says in a line why an attempt was not correct.
func attemptError(a eval.Attempt) string {
	text := strings.TrimSpace(a.Err)
	switch {
	case text == "" && a.Valid:
		text = "valid answer that failed a check"
	case text == "":
		text = "invalid answer (" + a.Class + ")"
	}
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	}
	if len(text) > 140 {
		text = text[:140] + "..."
	}
	if a.Class != "" {
		return a.Class + ": " + text
	}
	return text
}

type errorCount struct {
	text  string
	count int
}

func topErrors(counts map[string]int, n int) []errorCount {
	var list []errorCount
	for text, count := range counts {
		list = append(list, errorCount{text, count})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].count != list[j].count {
			return list[i].count > list[j].count
		}
		return list[i].text < list[j].text
	})
	if len(list) > n {
		list = list[:n]
	}
	return list
}
