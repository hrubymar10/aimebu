package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

var base = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

func at(seconds int) time.Time { return base.Add(time.Duration(seconds) * time.Second) }

func newTestTracker(opts options) (*tracker, *bytes.Buffer) {
	out := &bytes.Buffer{}
	if opts.quiet == 0 {
		opts.quiet = 30 * time.Second
	}
	if opts.slow == 0 {
		opts.slow = 10 * time.Second
	}
	if opts.hang == 0 {
		opts.hang = 5 * time.Minute
	}
	return newTracker(out, opts, base), out
}

func TestPackagesAreCountedAndTimed(t *testing.T) {
	tr, out := newTestTracker(options{total: 3})
	tr.handle(event{Action: "start", Package: "m/a"}, at(0))
	tr.handle(event{Action: "run", Package: "m/a", Test: "TestOne"}, at(0))
	tr.handle(event{Action: "pass", Package: "m/a", Test: "TestOne", Elapsed: 0.01}, at(1))
	tr.handle(event{Action: "pass", Package: "m/a", Elapsed: 1.5}, at(2))
	tr.handle(event{Action: "start", Package: "m/b"}, at(2))
	tr.handle(event{Action: "skip", Package: "m/b"}, at(2))
	got := out.String()
	if !strings.Contains(got, "start  m/a") || !strings.Contains(got, "ok     m/a (1.5s)  [1/3 packages]") {
		t.Fatalf("progress:\n%s", got)
	}
	if strings.Contains(got, "m/b (") {
		t.Fatalf("a package with no test files is only counted, not announced:\n%s", got)
	}
	if tr.done != 2 {
		t.Fatalf("done = %d, want 2 (the package without tests counts)", tr.done)
	}
}

func TestAFailingTestPrintsItsOutputAndPassingOnesDoNot(t *testing.T) {
	tr, out := newTestTracker(options{})
	tr.handle(event{Action: "start", Package: "m/a"}, at(0))
	tr.handle(event{Action: "run", Package: "m/a", Test: "TestGood"}, at(0))
	tr.handle(event{Action: "output", Package: "m/a", Test: "TestGood", Output: "chatter from a passing test\n"}, at(0))
	tr.handle(event{Action: "pass", Package: "m/a", Test: "TestGood", Elapsed: 0.01}, at(0))
	tr.handle(event{Action: "run", Package: "m/a", Test: "TestBad"}, at(0))
	tr.handle(event{Action: "output", Package: "m/a", Test: "TestBad", Output: "    bad_test.go:12: want 1, got 2\n"}, at(1))
	tr.handle(event{Action: "fail", Package: "m/a", Test: "TestBad", Elapsed: 0.2}, at(1))
	tr.handle(event{Action: "fail", Package: "m/a", Elapsed: 0.3}, at(1))
	got := out.String()
	if strings.Contains(got, "chatter") {
		t.Fatalf("a passing test's output was printed:\n%s", got)
	}
	for _, want := range []string{"FAIL   m/a TestBad (0.2s)", "bad_test.go:12: want 1, got 2", "FAIL   m/a (0.3s)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if !tr.sawFailure || len(tr.failedTests) != 1 {
		t.Fatalf("failure state: %+v", tr.failedTests)
	}
}

func TestSlowPassingTestsAreNamedButSubtestsAreNot(t *testing.T) {
	tr, out := newTestTracker(options{slow: 5 * time.Second})
	tr.handle(event{Action: "run", Package: "m/a", Test: "TestSlow"}, at(0))
	tr.handle(event{Action: "pass", Package: "m/a", Test: "TestSlow/sub", Elapsed: 9}, at(9))
	tr.handle(event{Action: "pass", Package: "m/a", Test: "TestSlow", Elapsed: 9.5}, at(10))
	tr.handle(event{Action: "pass", Package: "m/a", Test: "TestFast", Elapsed: 0.5}, at(10))
	got := out.String()
	if !strings.Contains(got, "slow   m/a TestSlow (9.5s)") || strings.Contains(got, "TestSlow/sub") || strings.Contains(got, "TestFast") {
		t.Fatalf("slow report:\n%s", got)
	}
}

func TestAPackageThatFailsWithoutATestShowsItsOutputAndRunningTests(t *testing.T) {
	tr, out := newTestTracker(options{})
	tr.handle(event{Action: "start", Package: "m/a"}, at(0))
	tr.handle(event{Action: "run", Package: "m/a", Test: "TestStuck"}, at(0))
	tr.handle(event{Action: "output", Package: "m/a", Output: "panic: test timed out after 10m0s\n"}, at(600))
	tr.handle(event{Action: "output", Package: "m/a", Output: "goroutine 1 [chan receive]:\n"}, at(600))
	// test2json attributes a goroutine dump to the test that was running.
	tr.handle(event{Action: "output", Package: "m/a", Test: "TestStuck", Output: "goroutine 7 [select]: main.waitForever()\n"}, at(600))
	tr.handle(event{Action: "fail", Package: "m/a", Elapsed: 600}, at(600))
	tr.summary(at(601), 1)
	got := out.String()
	for _, want := range []string{"panic: test timed out after 10m0s", "goroutine 1 [chan receive]:", "did not finish: TestStuck", "goroutine 7 [select]: main.waitForever()", "FAIL package m/a"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}

func TestWatchdogNamesWhatIsRunningAfterQuiet(t *testing.T) {
	tr, out := newTestTracker(options{total: 5})
	tr.handle(event{Action: "start", Package: "m/a"}, at(0))
	tr.handle(event{Action: "run", Package: "m/a", Test: "TestServerFoo"}, at(1))
	tr.handle(event{Action: "start", Package: "m/b"}, at(1))
	tr.watch(at(20))
	if strings.Contains(out.String(), "still running") {
		t.Fatalf("reported before the quiet period:\n%s", out.String())
	}
	tr.watch(at(40))
	got := out.String()
	for _, want := range []string{"still running after 40s", "nothing new for 39s", "running m/a TestServerFoo for 39s", "m/b: no test running"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	before := out.Len()
	tr.watch(at(45))
	if out.Len() != before {
		t.Fatalf("reported again after only 5 s:\n%s", out.String()[before:])
	}
	tr.watch(at(75))
	if !strings.Contains(out.String()[before:], "running m/a TestServerFoo for 1m14s") {
		t.Fatalf("no second report after another quiet period:\n%s", out.String()[before:])
	}
}

func TestPausedParallelTestsAreNotCalledRunning(t *testing.T) {
	tr, out := newTestTracker(options{})
	tr.handle(event{Action: "start", Package: "m/a"}, at(0))
	tr.handle(event{Action: "run", Package: "m/a", Test: "TestParallel"}, at(0))
	tr.handle(event{Action: "pause", Package: "m/a", Test: "TestParallel"}, at(0))
	tr.handle(event{Action: "run", Package: "m/a", Test: "TestSerial"}, at(0))
	tr.watch(at(60))
	got := out.String()
	if strings.Contains(got, "TestParallel") || !strings.Contains(got, "running m/a TestSerial for 1m00s") {
		t.Fatalf("report:\n%s", got)
	}
	tr.handle(event{Action: "cont", Package: "m/a", Test: "TestParallel"}, at(70))
	tr.watch(at(110))
	if !strings.Contains(out.String(), "running m/a TestParallel for 40s") {
		t.Fatalf("a test that continued is running again, counted from the continue:\n%s", out.String())
	}
}

func TestAHungTestIsReportedOnceWithTheCommandToDumpIt(t *testing.T) {
	tr, out := newTestTracker(options{hang: 2 * time.Minute})
	tr.selfPID = 4242
	tr.binariesForReport = func() []procInfo { return []procInfo{{pid: 777, ppid: 1, comm: "/tmp/go-build/aimebu.test"}} }
	tr.handle(event{Action: "start", Package: "m/cmd"}, at(0))
	tr.handle(event{Action: "run", Package: "m/cmd", Test: "TestServerStuck"}, at(0))
	tr.watch(at(60))
	if strings.Contains(out.String(), "HANG?") {
		t.Fatalf("called hung too early:\n%s", out.String())
	}
	tr.watch(at(130))
	got := out.String()
	for _, want := range []string{"HANG?  m/cmd TestServerStuck has been running for 2m10s", "kill -QUIT 777", "aimebu.test", "kill -USR1 4242"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	before := strings.Count(out.String(), "HANG?")
	tr.watch(at(200))
	tr.watch(at(300))
	if strings.Count(out.String(), "HANG?") != before {
		t.Fatalf("the same hang was reported again:\n%s", out.String())
	}
}

func TestQuitOnHangSendsTheSignalThroughTheHook(t *testing.T) {
	tr, out := newTestTracker(options{hang: time.Minute, quitOnHang: true})
	var quit []string
	tr.quitHook = func(pkg string) string { quit = append(quit, pkg); return "sent SIGQUIT to " + pkg }
	tr.handle(event{Action: "start", Package: "m/cmd"}, at(0))
	tr.handle(event{Action: "run", Package: "m/cmd", Test: "TestStuck"}, at(0))
	tr.watch(at(70))
	if len(quit) != 1 || quit[0] != "m/cmd" || !strings.Contains(out.String(), "sent SIGQUIT to m/cmd") {
		t.Fatalf("quit calls %v:\n%s", quit, out.String())
	}
}

func TestSummaryNamesFailuresAndSetsTheExit(t *testing.T) {
	tr, out := newTestTracker(options{total: 2})
	tr.handle(event{Action: "start", Package: "m/a"}, at(0))
	tr.handle(event{Action: "run", Package: "m/a", Test: "TestBad"}, at(0))
	tr.handle(event{Action: "fail", Package: "m/a", Test: "TestBad", Elapsed: 1}, at(1))
	tr.handle(event{Action: "fail", Package: "m/a", Elapsed: 1}, at(1))
	tr.handle(event{Action: "start", Package: "m/b"}, at(1))
	tr.handle(event{Action: "pass", Package: "m/b", Elapsed: 4}, at(5))
	tr.summary(at(6), 1)
	got := out.String()
	for _, want := range []string{"finished in 6.0s: 2/2 packages done, 1 package(s) failed, 1 test(s) failed", "FAIL m/a TestBad", "slowest packages: m/b 4.0s; m/a 1.0s;", "exit status 1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "FAIL package m/a") {
		t.Fatalf("a package with a failing test is not listed twice:\n%s", got)
	}
}

func TestPatternsSkipFlagValues(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"./..."}, "./..."},
		{[]string{"-race", "./..."}, "./..."},
		{[]string{"-run", "TestX", "-count", "3", "./internal/store", "./cmd/aimebu"}, "./internal/store ./cmd/aimebu"},
		{[]string{"-run=TestX", "./x"}, "./x"},
		{[]string{"-race"}, "."},
	} {
		if got := strings.Join(patterns(tc.args), " "); got != tc.want {
			t.Errorf("patterns(%v) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

func TestParseDescendantsFindsTestBinariesBelowGoTest(t *testing.T) {
	ps := `  100     1 /usr/bin/make
  200   100 /usr/bin/go
  300   200 /tmp/go-build123/b001/aimebu.test
  400   300 sleep
  500     1 /tmp/other/stray.test
  600   200 /tmp/go-build123/b002/store.test
`
	got := parseDescendants(ps, 200)
	if len(got) != 3 {
		t.Fatalf("descendants of go test: %+v", got)
	}
	var binaries []procInfo
	for _, p := range got {
		if strings.HasSuffix(p.comm, ".test") {
			binaries = append(binaries, p)
		}
	}
	if len(binaries) != 2 {
		t.Fatalf("test binaries below go test: %+v (the stray one at pid 500 is not below it)", binaries)
	}
	if mine := binaryFor(binaries, "github.com/x/aimebu/cmd/aimebu"); len(mine) != 1 || mine[0].pid != 300 {
		t.Fatalf("binary of cmd/aimebu: %+v", mine)
	}
}

func TestACachedPackageSaysSo(t *testing.T) {
	tr, out := newTestTracker(options{})
	tr.handle(event{Action: "start", Package: "m/a"}, at(0))
	tr.handle(event{Action: "output", Package: "m/a", Output: "ok  \tm/a\t(cached)\n"}, at(0))
	tr.handle(event{Action: "pass", Package: "m/a", Elapsed: 0}, at(0))
	if !strings.Contains(out.String(), "ok     m/a (0.0s, cached)") {
		t.Fatalf("a cached result is not marked:\n%s", out.String())
	}
}

// Compiler output of a build that succeeded (a warning, -gcflags=-m) is shown and is not a failure.
func TestBuildOutputWithoutABuildFailureIsNotAFailure(t *testing.T) {
	tr, out := newTestTracker(options{total: 1})
	tr.handle(event{Action: "build-output", ImportPath: "m/a [m/a.test]", Output: "./a.go:3:6: can inline f\n"}, at(0))
	tr.handle(event{Action: "start", Package: "m/a"}, at(0))
	tr.handle(event{Action: "pass", Package: "m/a", Elapsed: 0.1}, at(1))
	if !strings.Contains(out.String(), "can inline f") {
		t.Fatalf("the compiler's output was dropped:\n%s", out.String())
	}
	if tr.sawFailure || strings.Contains(out.String(), "BUILD") {
		t.Fatalf("a successful build was reported as a failure:\n%s", out.String())
	}
}

func TestBuildErrorsAreShownAsTheyCome(t *testing.T) {
	tr, out := newTestTracker(options{total: 1})
	tr.handle(event{Action: "build-output", ImportPath: "m/a [m/a.test]", Output: "# m/a [m/a.test]\n"}, at(0))
	tr.handle(event{Action: "build-output", ImportPath: "m/a [m/a.test]", Output: "./c_test.go:3:33: undefined: undefinedThing\n"}, at(0))
	tr.handle(event{Action: "build-fail", ImportPath: "m/a [m/a.test]"}, at(0))
	tr.handle(event{Action: "start", Package: "m/a"}, at(0))
	tr.handle(event{Action: "output", Package: "m/a", Output: "FAIL\tm/a [build failed]\n"}, at(0))
	tr.handle(event{Action: "fail", Package: "m/a"}, at(0))
	got := out.String()
	for _, want := range []string{"./c_test.go:3:33: undefined: undefinedThing", "BUILD  m/a [m/a.test] does not compile", "FAIL   m/a (0.0s)  [1/1 packages]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if len(tr.order) != 1 {
		t.Fatalf("build events made a package of their own: %v", tr.order)
	}
	if !tr.sawFailure {
		t.Fatal("a build failure is a failure")
	}
}
