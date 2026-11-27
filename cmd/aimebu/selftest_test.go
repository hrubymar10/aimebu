package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/eval"
	"github.com/hrubymar10/aimebu/v2/internal/eval/live"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

const selfTestKey = "selftestkey"

const selfTestUsages = `{"providers":[{"provider_name":"claude-code","profiles":[{"profile_name":"main","active":true,"windows":[{"key":"weekly","percent_used":41}]}]}]}`

// selfTestServer stands for the running server: it answers GET /api/doctor/self-test to the doctor key and points the CLI at itself.
type selfTestServer struct {
	root    string
	scratch string
	usages  json.RawMessage
	hits    atomic.Int32
}

func newSelfTestServer(t *testing.T, targets ...string) *selfTestServer {
	t.Helper()
	s := &selfTestServer{root: t.TempDir(), scratch: t.TempDir()}
	s.usages = json.RawMessage(selfTestUsages)
	doc := protocol.DoctorSelfTest{}
	for _, name := range targets {
		id := "lch_" + name
		doc.Launchers = append(doc.Launchers, protocol.LauncherRow{LauncherDefinition: protocol.LauncherDefinition{ID: id, Tool: "claude", Name: name, Enabled: true, Containerized: true, VisibleRoots: []string{s.scratch}},
			ResolvedCommand: "/bin/claude-docker", Available: true})
		doc.Targets = append(doc.Targets, protocol.DoctorSelfTestTarget{Name: name, Tool: "claude", LauncherID: id, Model: "model-" + name})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		if r.URL.Path != "/api/doctor/self-test" || r.Header.Get("Authorization") != "Bearer "+selfTestKey {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		doc := doc
		doc.Usages = s.usages
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	t.Setenv("AIMEBU_CONFIG_DIR", s.root)
	t.Setenv("AIMEBU_BIND", host)
	t.Setenv("AIMEBU_PORT", port)
	s.writeKey(t, selfTestKey, 0o600)
	return s
}

func (s *selfTestServer) writeKey(t *testing.T, key string, mode os.FileMode) {
	t.Helper()
	dir := filepath.Join(s.root, "server")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "doctor.key")
	if err := os.WriteFile(path, []byte(key+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(path, mode)
}

// fakeRun replaces the run itself: it records the options and answers every case correct, except the cases of the targets in bad.
type fakeRun struct {
	calls   int
	options *live.Options
	bad     map[string]bool
}

func (f *fakeRun) install(t *testing.T) {
	t.Helper()
	previous, previousTerminal := selfTestRun, selfTestIsTerminal
	selfTestRun = func(_ context.Context, o *live.Options) ([]eval.Case, error) {
		f.calls++
		f.options = o
		var cases []eval.Case
		for _, target := range o.Targets {
			for _, s := range o.Scenarios {
				for rep := 0; rep < o.Reps; rep++ {
					a := eval.Attempt{Valid: true, Correct: true}
					if f.bad[target.Name] {
						a = eval.Attempt{Class: eval.ClassFormat, Err: "text around the JSON"}
					}
					cases = append(cases, eval.Case{Model: target.Name, Scenario: s.ID, Rep: rep, Attempts: []eval.Attempt{a}})
				}
			}
		}
		return cases, nil
	}
	selfTestIsTerminal = func(io.Reader) bool { return false }
	t.Cleanup(func() { selfTestRun, selfTestIsTerminal = previous, previousTerminal })
}

func runSelfTest(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := selfTestCommand(context.Background(), args, strings.NewReader(stdin), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestSelfTestWithoutARunningServerFailsClearly(t *testing.T) {
	t.Setenv("AIMEBU_CONFIG_DIR", t.TempDir())
	t.Setenv("AIMEBU_BIND", "127.0.0.1")
	fake := &fakeRun{}
	fake.install(t)
	// No doctor key: the server is not running.
	code, stdout, stderr := runSelfTest(t, "", "--yes")
	if code != 1 || !strings.Contains(stderr, "needs a running server") || fake.calls != 0 || strings.Contains(stdout, "Targets") {
		t.Fatalf("no key: code %d stdout %q stderr %q", code, stdout, stderr)
	}
	// A stale key and nobody listening.
	s := &selfTestServer{root: os.Getenv("AIMEBU_CONFIG_DIR")}
	s.writeKey(t, selfTestKey, 0o600)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIMEBU_PORT", strconv.Itoa(listener.Addr().(*net.TCPAddr).Port))
	listener.Close()
	code, _, stderr = runSelfTest(t, "", "--yes")
	if code != 1 || !strings.Contains(stderr, "did not answer") || fake.calls != 0 {
		t.Fatalf("stale key: code %d stderr %q", code, stderr)
	}
}

func TestSelfTestRefusesWithoutTheDoctorKey(t *testing.T) {
	s := newSelfTestServer(t, "a")
	fake := &fakeRun{}
	fake.install(t)
	// A key the server does not know: its guard answers 403.
	s.writeKey(t, "another-start", 0o600)
	code, _, stderr := runSelfTest(t, "", "--yes")
	if code != 1 || !strings.Contains(stderr, "refused the doctor key") || fake.calls != 0 {
		t.Fatalf("wrong key: code %d stderr %q", code, stderr)
	}
	// A key file others can read is not used at all.
	s.writeKey(t, selfTestKey, 0o644)
	hits := s.hits.Load()
	code, _, stderr = runSelfTest(t, "", "--yes")
	if code != 1 || !strings.Contains(stderr, "needs a running server") || s.hits.Load() != hits || fake.calls != 0 {
		t.Fatalf("world-readable key: code %d stderr %q", code, stderr)
	}
}

func TestSelfTestLoadsTargetsFromTheServerAndRunsASmoke(t *testing.T) {
	s := newSelfTestServer(t, "alpha", "beta")
	fake := &fakeRun{}
	fake.install(t)
	code, stdout, stderr := runSelfTest(t, "", "--yes", "--scratch", s.scratch)
	if code != 0 || stderr != "" || fake.calls != 1 {
		t.Fatalf("code %d calls %d stdout %q stderr %q", code, fake.calls, stdout, stderr)
	}
	o := fake.options
	if len(o.Targets) != 2 || o.Targets[0].Name != "alpha" || o.Targets[1].Model != "model-beta" || len(o.Launchers) != 2 || o.Launchers[0].ResolvedCommand != "/bin/claude-docker" {
		t.Fatalf("targets %+v launchers %+v", o.Targets, o.Launchers)
	}
	if len(o.Scenarios) != 2 || o.Scenarios[0].ID != "s01-plain-reply" || o.Scenarios[1].ID != "s15-waiting-on-command" || o.Reps != 1 || o.MaxUSD != 30 || o.StopAbovePct != 90 {
		t.Fatalf("smoke options: %d scenarios, reps %d, cap %v, stop %v", len(o.Scenarios), o.Reps, o.MaxUSD, o.StopAbovePct)
	}
	// The quota guard reads the server's snapshot through the doctor route, not a cookie.
	reading, err := o.Quota.Read(context.Background())
	if err != nil || reading.Usage["claude-code/main/weekly"] != 41 || reading.Active["claude-code"] != "main" {
		t.Fatalf("quota reading %+v: %v", reading, err)
	}
	for _, want := range []string{"alpha", "beta", "model-alpha", "Repetitions: 1", "at most 8 turns", "Cost cap: $30.00", "stop", "weekly window is claude-code/main/weekly at 41%", "PASS alpha", "PASS beta", "Report: "} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
}

func TestSelfTestWithoutAUsageSnapshotWarnsOnceAndRuns(t *testing.T) {
	s := newSelfTestServer(t, "alpha")
	s.usages = json.RawMessage("null")
	fake := &fakeRun{}
	fake.install(t)
	code, stdout, stderr := runSelfTest(t, "", "--yes", "--scratch", s.scratch)
	if code != 0 || fake.calls != 1 || fake.options.Quota != nil || strings.Count(stderr, "warning:") != 1 || !strings.Contains(stderr, "quota guard") || !strings.Contains(stdout, "Quota guard: skipped") {
		t.Fatalf("code %d calls %d quota %v stdout %q stderr %q", code, fake.calls, fake.options.Quota, stdout, stderr)
	}
}

func TestSelfTestEmptyUsageWindowsAlsoWarns(t *testing.T) {
	s := newSelfTestServer(t, "alpha")
	s.usages = json.RawMessage(`{"providers":[]}`)
	fake := &fakeRun{}
	fake.install(t)
	code, stdout, stderr := runSelfTest(t, "", "--yes", "--scratch", s.scratch)
	if code != 0 || fake.calls != 1 || fake.options.Quota != nil || strings.Count(stderr, "warning:") != 1 || !strings.Contains(stdout, "Quota guard: skipped") {
		t.Fatalf("code %d calls %d quota %v stdout %q stderr %q", code, fake.calls, fake.options.Quota, stdout, stderr)
	}
}

func TestSelfTestKeepsExistingScratchPath(t *testing.T) {
	s := newSelfTestServer(t, "alpha")
	fake := &fakeRun{}
	fake.install(t)
	for _, path := range []string{s.scratch, filepath.Join(t.TempDir(), "scratch-link")} {
		if strings.HasSuffix(path, "scratch-link") {
			if err := os.Symlink(s.scratch, path); err != nil {
				t.Fatal(err)
			}
		}
		code, _, stderr := runSelfTest(t, "", "--yes", "--scratch", path)
		if code != 0 || stderr != "" {
			t.Fatalf("code %d stderr %q", code, stderr)
		}
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("scratch path %q removed: %v", path, err)
		}
	}
}

func TestSelfTestFindsNestedSharedVisibleRoot(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "project")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	rows := []protocol.LauncherRow{
		{LauncherDefinition: protocol.LauncherDefinition{ID: "outer", VisibleRoots: []string{root}}},
		{LauncherDefinition: protocol.LauncherDefinition{ID: "inner", VisibleRoots: []string{nested}}},
	}
	targets := []live.Target{{LauncherID: "outer"}, {LauncherID: "inner"}}
	got, err := selfTestScratch("", targets, rows)
	if err != nil || got != filepath.Join(nested, "aimebu-self-test") {
		t.Fatalf("scratch=%q err=%v", got, err)
	}
}

func TestSelfTestUsesLaunchersContainerVisibleHost(t *testing.T) {
	rows := []protocol.LauncherRow{
		{LauncherDefinition: protocol.LauncherDefinition{ID: "one", AgentServerURL: "http://gateway.internal:8877"}},
		{LauncherDefinition: protocol.LauncherDefinition{ID: "two", AgentServerURL: "http://gateway.internal:8878"}},
	}
	if got := selfTestVisibleHost([]live.Target{{LauncherID: "one"}, {LauncherID: "two"}}, rows); got != "gateway.internal" {
		t.Fatalf("visible host=%q", got)
	}
	rows[1].AgentServerURL = "http://another.internal:8878"
	if got := selfTestVisibleHost([]live.Target{{LauncherID: "one"}, {LauncherID: "two"}}, rows); got != "" {
		t.Fatalf("mixed visible hosts=%q", got)
	}
}

func TestSelfTestFullRunsEveryScenarioThreeTimesWithTheFlagsGiven(t *testing.T) {
	s := newSelfTestServer(t, "alpha")
	fake := &fakeRun{}
	fake.install(t)
	all, err := eval.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runSelfTest(t, "", "--full", "--yes", "--scratch", s.scratch, "--max-usd", "5.5", "--stop-above", "70")
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	o := fake.options
	if len(o.Scenarios) != len(all) || o.Reps != 3 || o.MaxUSD != 5.5 || o.StopAbovePct != 70 {
		t.Fatalf("full options: %d of %d scenarios, reps %d, cap %v, stop %v", len(o.Scenarios), len(all), o.Reps, o.MaxUSD, o.StopAbovePct)
	}
}

func TestSelfTestTargetsFlagFilters(t *testing.T) {
	s := newSelfTestServer(t, "alpha", "beta", "gamma")
	fake := &fakeRun{}
	fake.install(t)
	code, _, stderr := runSelfTest(t, "", "--yes", "--scratch", s.scratch, "--targets", "gamma, alpha")
	if code != 0 || stderr != "" || len(fake.options.Targets) != 2 || fake.options.Targets[0].Name != "gamma" || fake.options.Targets[1].Name != "alpha" {
		t.Fatalf("code %d stderr %q targets %+v", code, stderr, fake.options)
	}
	fake.calls = 0
	code, _, stderr = runSelfTest(t, "", "--yes", "--scratch", s.scratch, "--targets", "alpha,nope")
	if code != 1 || !strings.Contains(stderr, `no target "nope"`) || !strings.Contains(stderr, "alpha, beta, gamma") || fake.calls != 0 {
		t.Fatalf("unknown target: code %d stderr %q", code, stderr)
	}
}

func TestSelfTestNoTargetsOnTheServerIsAnError(t *testing.T) {
	newSelfTestServer(t)
	fake := &fakeRun{}
	fake.install(t)
	code, _, stderr := runSelfTest(t, "", "--yes")
	if code != 1 || !strings.Contains(stderr, "nothing to test") || fake.calls != 0 {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
}

func TestSelfTestAsksFirstAndRefusesWithoutATerminalOrYes(t *testing.T) {
	s := newSelfTestServer(t, "alpha")
	fake := &fakeRun{}
	fake.install(t)
	// No terminal and no --yes: it refuses, and runs nothing, whatever stdin holds.
	code, stdout, stderr := runSelfTest(t, "y\n", "--scratch", s.scratch)
	if code != 1 || !strings.Contains(stderr, "pass --yes") || fake.calls != 0 || strings.Contains(stdout, "Run it?") {
		t.Fatalf("no tty: code %d stdout %q stderr %q", code, stdout, stderr)
	}
	// A terminal: the summary comes first, then the question; anything but yes aborts.
	selfTestIsTerminal = func(io.Reader) bool { return true }
	for _, answer := range []string{"\n", "n\n", "no\n", ""} {
		code, stdout, stderr = runSelfTest(t, answer, "--scratch", s.scratch)
		if code != 1 || fake.calls != 0 || !strings.Contains(stdout, "Targets (1)") || !strings.Contains(stdout, "Run it? [y/N]: ") || !strings.Contains(stdout, "nothing was run") {
			t.Fatalf("answer %q: code %d calls %d stdout %q stderr %q", answer, code, fake.calls, stdout, stderr)
		}
	}
	for _, answer := range []string{"y\n", "YES\n"} {
		fake.calls = 0
		if code, stdout, stderr = runSelfTest(t, answer, "--scratch", s.scratch); code != 0 || fake.calls != 1 {
			t.Fatalf("answer %q: code %d calls %d stdout %q stderr %q", answer, code, fake.calls, stdout, stderr)
		}
	}
	// --yes does not ask.
	fake.calls = 0
	if code, stdout, _ = runSelfTest(t, "", "--yes", "--scratch", s.scratch); code != 0 || fake.calls != 1 || strings.Contains(stdout, "Run it?") {
		t.Fatalf("--yes: code %d calls %d stdout %q", code, fake.calls, stdout)
	}
}

func TestSelfTestWritesAPrivateReportAndPrintsItsPath(t *testing.T) {
	s := newSelfTestServer(t, "alpha", "beta")
	fake := &fakeRun{bad: map[string]bool{"beta": true}}
	fake.install(t)
	code, stdout, _ := runSelfTest(t, "", "--yes", "--scratch", s.scratch)
	if code != 1 {
		t.Fatalf("a failing target exits non-zero, got %d", code)
	}
	var reportPath string
	for _, line := range strings.Split(stdout, "\n") {
		if rest, ok := strings.CutPrefix(line, "Report: "); ok {
			reportPath = rest
		}
	}
	dir := filepath.Dir(reportPath)
	if filepath.Base(reportPath) != "report.md" || filepath.Dir(dir) != filepath.Join(s.root, "self-test") {
		t.Fatalf("report path %q (data root %s)", reportPath, s.root)
	}
	mode := func(path string) os.FileMode {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return info.Mode().Perm()
	}
	if mode(dir) != 0o700 || mode(filepath.Dir(dir)) != 0o700 || mode(reportPath) != 0o600 || mode(filepath.Join(dir, "results.json")) != 0o600 {
		t.Fatalf("modes: dir %v self-test %v report %v results %v", mode(dir), mode(filepath.Dir(dir)), mode(reportPath), mode(filepath.Join(dir, "results.json")))
	}
	report, _ := os.ReadFile(reportPath)
	var cases []eval.Case
	raw, _ := os.ReadFile(filepath.Join(dir, "results.json"))
	if err := json.Unmarshal(raw, &cases); err != nil || len(cases) != 4 || !strings.Contains(string(report), "Self-test ") {
		t.Fatalf("results %d err %v report %.80q", len(cases), err, report)
	}
	for _, want := range []string{"PASS alpha", "FAIL beta", "2x format: text around the JSON"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
}

func TestSelfTestSummaryKeepsTheThreeMostCommonErrors(t *testing.T) {
	target := live.Target{Name: "t"}
	bad := func(class, err string) eval.Case {
		return eval.Case{Model: "t", Attempts: []eval.Attempt{{Class: class, Err: err}}}
	}
	cases := []eval.Case{bad("format", "one"), bad("format", "one"), bad("format", "one"), bad("field", "two"), bad("field", "two"), bad("harness", "three"), bad("harness", "three"), bad("format", "rare"),
		{Model: "t", Attempts: []eval.Attempt{{Class: "format", Err: "first miss"}, {Valid: true, Correct: true}}}}
	var out bytes.Buffer
	if selfTestPrint(&out, []live.Target{target}, cases, nil) {
		t.Fatal("a target with a case that never became correct does not pass")
	}
	text := out.String()
	for _, want := range []string{"FAIL t", "after repairs 11% (1/9)", "3x format: one", "2x field: two", "2x harness: three"} {
		if !strings.Contains(text, want) {
			t.Errorf("lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "rare") || strings.Contains(text, "first miss") {
		t.Errorf("only the three most common errors are shown:\n%s", text)
	}
}

// standIn answers every turn with one fixed text: the run end to end, with no launcher and no model.
type standIn struct{ starts atomic.Int32 }

func (s *standIn) Catalog() []adapter.CatalogEntry { return nil }
func (s *standIn) Capabilities(string, adapter.Environment) adapter.Capabilities {
	return adapter.Capabilities{}
}
func (s *standIn) Start(_ context.Context, spec adapter.TurnSpec) (adapter.Turn, error) {
	s.starts.Add(1)
	ch := make(chan adapter.Event, 1)
	ch <- adapter.Event{Kind: adapter.EventFinalAnswer, Answer: json.RawMessage(`{"messages":[]}`)}
	close(ch)
	return standInTurn{ch}, nil
}

type standInTurn struct{ ch chan adapter.Event }

func (t standInTurn) Events() <-chan adapter.Event { return t.ch }
func (t standInTurn) Cancel() error                { return nil }
func (t standInTurn) Wait() (adapter.ExitInfo, error) {
	return adapter.ExitInfo{TerminalEvent: true}, nil
}

func TestSelfTestRunsEndToEndOnAStandInAdapter(t *testing.T) {
	s := newSelfTestServer(t, "alpha")
	in := &standIn{}
	previousAdapter, previousPreflight := selfTestNewAdapter, selfTestPreflight
	selfTestNewAdapter = func(string) (adapter.Adapter, error) { return in, nil }
	selfTestPreflight = func(string, string, string) error { return nil }
	t.Cleanup(func() { selfTestNewAdapter, selfTestPreflight = previousAdapter, previousPreflight })
	code, stdout, stderr := runSelfTest(t, "", "--yes", "--scratch", s.scratch)
	if in.starts.Load() == 0 {
		t.Fatalf("the stand-in was never started: code %d stdout %q stderr %q", code, stdout, stderr)
	}
	// Both smoke scenarios ran on the one target, each at least once (a case the parser rejects is sent back once).
	if !strings.Contains(stdout, "alpha s01-plain-reply rep 0:") || !strings.Contains(stdout, "alpha s15-waiting-on-command rep 0:") || !strings.Contains(stdout, "Report: ") {
		t.Fatalf("code %d stdout %q stderr %q", code, stdout, stderr)
	}
	// The scratch checkouts of the run are gone, and the scratch folder with them.
	entries, _ := os.ReadDir(s.scratch)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "aimebu-eval-") {
			t.Errorf("checkout left behind: %s", e.Name())
		}
	}
}

func TestSelfTestUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"--bogus"}, {"extra"}, {"--max-usd", "-1"}, {"--stop-above", "0"}, {"--stop-above", "101"}} {
		code, _, stderr := runSelfTest(t, "", args...)
		if code != 2 || stderr == "" {
			t.Errorf("%v: code %d stderr %q", args, code, stderr)
		}
	}
}
