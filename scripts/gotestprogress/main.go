// Command gotestprogress runs `go test -json` and turns its event stream into progress a person can follow: when each package starts and
// ends (with its duration and an X/Y count), every failing test with its output, tests that took long, and, when nothing has happened
// for a while, which tests are running right now and for how long. A test that runs past a threshold is called out as possibly hung with
// the exact command that makes its test binary dump all goroutines.
//
// It exists because `go test ./...` prints nothing for a package until the package is done, so a hang looks like silence and says nothing
// about where. Usage:
//
//	go run ./scripts/gotestprogress [flags] -- [go test arguments]
//
// The exit status is go test's, or 1 when a failure was reported while go test said 0. Send SIGUSR1 to this process to make the test
// binaries of packages with a test that has been running for a while dump their goroutines (SIGQUIT), see -quiet.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/goccy/go-json"
)

type event struct {
	Time       time.Time
	Action     string
	Package    string
	ImportPath string // build events carry this instead of Package
	Test       string
	Output     string
	Elapsed    float64
}

type options struct {
	quiet      time.Duration // print what is running when nothing happened for this long
	slow       time.Duration // report a passing test that took at least this long
	hang       time.Duration // call a test possibly hung after this long
	quitOnHang bool          // send SIGQUIT to the test binary of a package with a hung test
	total      int           // packages expected, 0 when unknown
}

type runningTest struct {
	since  time.Time
	paused bool
	hung   bool // already reported as possibly hung
}

type pkgState struct {
	started  time.Time
	running  map[string]*runningTest
	output   map[string][]string // per test, until it ends
	pkgLines []string            // output that belongs to no test: build errors, panics, timeouts
	done     bool
}

const (
	maxTestLines    = 2000
	maxPackageLines = 4000
)

type tracker struct {
	out  io.Writer
	opts options

	start      time.Time
	last       time.Time // the last event or line
	lastReport time.Time // the last "still running" report
	pkgs       map[string]*pkgState
	order      []string

	done              int
	failedPkgNames    []string
	failedTests       []string
	slowestPackages   []timed
	slowTests         []timed
	nonJSON           int
	sawFailure        bool
	quitHook          func(pkg string) string // sends SIGQUIT for a package's test binary; returns what it did
	binariesForReport func() []procInfo       // the test binaries below the go test process, for the hang message
	selfPID           int
}

type timed struct {
	name string
	d    time.Duration
}

func newTracker(out io.Writer, opts options, now time.Time) *tracker {
	return &tracker{out: out, opts: opts, start: now, last: now, lastReport: now, pkgs: map[string]*pkgState{}}
}

func (t *tracker) stamp(now time.Time) string { return now.Format("15:04:05") }

func (t *tracker) printf(now time.Time, format string, args ...any) {
	fmt.Fprintf(t.out, "[%s] %s\n", t.stamp(now), fmt.Sprintf(format, args...))
}

func (t *tracker) counter() string {
	if t.opts.total > 0 {
		return fmt.Sprintf("%d/%d packages", t.done, t.opts.total)
	}
	return fmt.Sprintf("%d packages", t.done)
}

func (t *tracker) pkg(name string, now time.Time) *pkgState {
	p, ok := t.pkgs[name]
	if !ok {
		p = &pkgState{started: now, running: map[string]*runningTest{}, output: map[string][]string{}}
		t.pkgs[name] = p
		t.order = append(t.order, name)
	}
	return p
}

func short(d time.Duration) string {
	switch {
	case d >= time.Minute:
		return fmt.Sprintf("%dm%02ds", int(d/time.Minute), int(d%time.Minute/time.Second))
	case d >= 10*time.Second:
		return fmt.Sprintf("%.0fs", d.Seconds())
	default:
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
}

func appendCapped(lines []string, line string, max int) []string {
	lines = append(lines, line)
	if len(lines) > max {
		lines = lines[len(lines)-max:]
	}
	return lines
}

// handle consumes one event.
func (t *tracker) handle(ev event, now time.Time) {
	t.last = now
	// What the compiler said comes before the package's own events and belongs to no package yet (go test -json names it by import path
	// only): it is printed as it comes, because it is exactly what a failed build needs.
	switch ev.Action {
	case "build-output":
		// Printed whatever the build's result: warnings, and what flags like -gcflags=-m ask the compiler to say, are output too. Only
		// build-fail makes it a failure.
		fmt.Fprintf(t.out, "         %s", strings.TrimRight(ev.Output, "\n")+"\n")
		return
	case "build-fail":
		t.sawFailure = true
		t.printf(now, "BUILD  %s does not compile", ev.ImportPath)
		return
	}
	p := t.pkg(ev.Package, now)
	switch ev.Action {
	case "start":
		p.started = now
		t.printf(now, "start  %s", ev.Package)
	case "run":
		p.running[ev.Test] = &runningTest{since: now}
	case "pause":
		if r := p.running[ev.Test]; r != nil {
			r.paused = true
		}
	case "cont":
		if r := p.running[ev.Test]; r != nil {
			r.paused, r.since = false, now
		}
	case "output":
		if ev.Test != "" {
			p.output[ev.Test] = appendCapped(p.output[ev.Test], ev.Output, maxTestLines)
		} else {
			p.pkgLines = appendCapped(p.pkgLines, ev.Output, maxPackageLines)
		}
	case "pass", "fail", "skip":
		if ev.Test != "" {
			t.testEnded(p, ev, now)
		} else {
			t.packageEnded(p, ev, now)
		}
	}
}

func (t *tracker) testEnded(p *pkgState, ev event, now time.Time) {
	d := time.Duration(ev.Elapsed * float64(time.Second))
	lines := p.output[ev.Test]
	delete(p.output, ev.Test)
	delete(p.running, ev.Test)
	switch ev.Action {
	case "fail":
		t.sawFailure = true
		t.failedTests = append(t.failedTests, ev.Package+" "+ev.Test)
		t.printf(now, "FAIL   %s %s (%s)", ev.Package, ev.Test, short(d))
		for _, line := range lines {
			fmt.Fprintf(t.out, "         %s", strings.TrimRight(line, "\n")+"\n")
		}
	case "pass":
		if d >= t.opts.slow && !strings.Contains(ev.Test, "/") {
			t.slowTests = append(t.slowTests, timed{ev.Package + " " + ev.Test, d})
			t.printf(now, "slow   %s %s (%s)", ev.Package, ev.Test, short(d))
		}
	}
}

func (t *tracker) packageEnded(p *pkgState, ev event, now time.Time) {
	if p.done {
		return
	}
	p.done = true
	t.done++
	d := time.Duration(ev.Elapsed * float64(time.Second))
	if d == 0 {
		d = now.Sub(p.started)
	}
	switch ev.Action {
	case "pass":
		t.slowestPackages = append(t.slowestPackages, timed{ev.Package, d})
		cached := ""
		for _, line := range p.pkgLines {
			if strings.Contains(line, "(cached)") {
				cached = ", cached"
			}
		}
		t.printf(now, "ok     %s (%s%s)  [%s]", ev.Package, short(d), cached, t.counter())
	case "skip":
		// No test files: nothing to say, but it counts as done so the X/Y adds up.
	case "fail":
		t.sawFailure = true
		t.failedPkgNames = append(t.failedPkgNames, ev.Package)
		t.slowestPackages = append(t.slowestPackages, timed{ev.Package, d})
		t.printf(now, "FAIL   %s (%s)  [%s]", ev.Package, short(d), t.counter())
		// What belongs to no test: a build error, a panic, a timeout, and the goroutine dump that comes with them.
		for _, line := range p.pkgLines {
			fmt.Fprintf(t.out, "         %s", strings.TrimRight(line, "\n")+"\n")
		}
		// A test that never reported (the binary died, or was sent SIGQUIT) is named, with what it printed: test2json attributes a crash or
		// a goroutine dump to the test that was running.
		var unfinished []string
		for name := range p.running {
			unfinished = append(unfinished, name)
		}
		sort.Strings(unfinished)
		for _, name := range unfinished {
			fmt.Fprintf(t.out, "         did not finish: %s\n", name)
			for _, line := range p.output[name] {
				fmt.Fprintf(t.out, "         %s", strings.TrimRight(line, "\n")+"\n")
			}
		}
	}
	p.running = map[string]*runningTest{}
	p.output = map[string][]string{}
}

// watch runs on a timer: it reports what is running when nothing has happened for opts.quiet, and calls out tests past opts.hang.
func (t *tracker) watch(now time.Time) {
	type running struct {
		pkg, test string
		age       time.Duration
	}
	var live []running
	var starting []running
	for _, name := range t.order {
		p := t.pkgs[name]
		if p.done {
			continue
		}
		active := 0
		for test, r := range p.running {
			if r.paused {
				continue
			}
			active++
			live = append(live, running{name, test, now.Sub(r.since)})
			if age := now.Sub(r.since); age >= t.opts.hang && !r.hung {
				r.hung = true
				t.reportHang(name, test, age, now)
			}
		}
		if active == 0 {
			starting = append(starting, running{name, "", now.Sub(p.started)})
		}
	}
	if now.Sub(t.last) < t.opts.quiet || now.Sub(t.lastReport) < t.opts.quiet {
		return
	}
	t.lastReport = now
	sort.Slice(live, func(i, j int) bool { return live[i].age > live[j].age })
	sort.Slice(starting, func(i, j int) bool { return starting[i].age > starting[j].age })
	t.printf(now, "still running after %s, nothing new for %s  [%s]", short(now.Sub(t.start)), short(now.Sub(t.last)), t.counter())
	for i, r := range live {
		if i == 10 {
			fmt.Fprintf(t.out, "         ... and %d more\n", len(live)-10)
			break
		}
		fmt.Fprintf(t.out, "         running %s %s for %s\n", r.pkg, r.test, short(r.age))
	}
	for _, r := range starting {
		fmt.Fprintf(t.out, "         %s: no test running (building, or between tests) for %s\n", r.pkg, short(r.age))
	}
}

func (t *tracker) reportHang(pkg, test string, age time.Duration, now time.Time) {
	t.printf(now, "HANG?  %s %s has been running for %s", pkg, test, short(age))
	if t.opts.quitOnHang && t.quitHook != nil {
		fmt.Fprintf(t.out, "         %s\n", t.quitHook(pkg))
		return
	}
	var names []string
	if t.binariesForReport != nil {
		for _, b := range t.binariesForReport() {
			names = append(names, fmt.Sprintf("kill -QUIT %d    # %s", b.pid, b.comm))
		}
	}
	if len(names) == 0 {
		names = append(names, "no test binary found below go test; look for the *.test process with ps and kill -QUIT it")
	}
	fmt.Fprintln(t.out, "         to see where it is stuck, make its test binary dump all goroutines (the dump lands in that package's FAIL output):")
	for _, line := range names {
		fmt.Fprintf(t.out, "           %s\n", line)
	}
	if t.selfPID != 0 {
		fmt.Fprintf(t.out, "         or kill -USR1 %d (this program quits the binaries of packages with a test running for %s or more)\n", t.selfPID, short(t.opts.quiet))
	}
}

// summary prints the end of the run.
func (t *tracker) summary(now time.Time, exit int) {
	fmt.Fprintln(t.out)
	t.printf(now, "finished in %s: %s done, %d package(s) failed, %d test(s) failed", short(now.Sub(t.start)), t.counter(), len(t.failedPkgNames), len(t.failedTests))
	for _, name := range t.failedTests {
		fmt.Fprintf(t.out, "         FAIL %s\n", name)
	}
	// A package that failed without a failing test of its own (a build error, a timeout, a crash) is named as well.
	for _, name := range t.failedPkgNames {
		if !hasFailedTest(t.failedTests, name) {
			fmt.Fprintf(t.out, "         FAIL package %s (no test of its own failed: a build error, a timeout or a crash; its output is above)\n", name)
		}
	}
	if len(t.slowestPackages) > 0 {
		sort.Slice(t.slowestPackages, func(i, j int) bool { return t.slowestPackages[i].d > t.slowestPackages[j].d })
		fmt.Fprint(t.out, "         slowest packages:")
		for i, p := range t.slowestPackages {
			if i == 3 {
				break
			}
			fmt.Fprintf(t.out, " %s %s;", p.name, short(p.d))
		}
		fmt.Fprintln(t.out)
	}
	if exit != 0 || t.sawFailure {
		fmt.Fprintf(t.out, "         exit status %d\n", exit)
	}
}

func hasFailedTest(failed []string, pkg string) bool {
	for _, name := range failed {
		if strings.HasPrefix(name, pkg+" ") {
			return true
		}
	}
	return false
}

// ---- processes ----

type procInfo struct {
	pid, ppid int
	comm      string
}

// descendants lists the processes below root, from `ps -axo pid=,ppid=,comm=` (the same on Linux and macOS).
func descendants(root int) []procInfo {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,comm=").Output()
	if err != nil {
		return nil
	}
	return parseDescendants(string(out), root)
}

func parseDescendants(psOutput string, root int) []procInfo {
	children := map[int][]procInfo{}
	for _, line := range strings.Split(psOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue
		}
		children[ppid] = append(children[ppid], procInfo{pid, ppid, strings.Join(fields[2:], " ")})
	}
	var out []procInfo
	queue := []int{root}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		for _, child := range children[next] {
			out = append(out, child)
			queue = append(queue, child.pid)
		}
	}
	return out
}

// testBinaries are the *.test processes below root (go test runs each package's compiled test binary as its own process).
func testBinaries(root int) []procInfo {
	var out []procInfo
	for _, p := range descendants(root) {
		if strings.HasSuffix(filepath.Base(p.comm), ".test") {
			out = append(out, p)
		}
	}
	return out
}

// binaryFor guesses which test binary belongs to a package: it is named after the last element of the import path.
func binaryFor(binaries []procInfo, pkg string) []procInfo {
	want := filepath.Base(pkg) + ".test"
	var out []procInfo
	for _, b := range binaries {
		if filepath.Base(b.comm) == want {
			out = append(out, b)
		}
	}
	return out
}

// ---- main ----

// valueFlags are the go test flags that take a separate value, so that value is not mistaken for a package pattern.
var valueFlags = map[string]bool{"-run": true, "-skip": true, "-count": true, "-timeout": true, "-parallel": true, "-p": true, "-tags": true, "-bench": true,
	"-cpu": true, "-coverprofile": true, "-ldflags": true, "-shuffle": true, "-vet": true, "-gcflags": true, "-covermode": true, "-coverpkg": true, "-fuzz": true, "-o": true}

// patterns are the package arguments among go test's arguments.
func patterns(args []string) []string {
	var out []string
	skip := false
	for _, arg := range args {
		switch {
		case skip:
			skip = false
		case strings.HasPrefix(arg, "-"):
			skip = valueFlags[arg] && !strings.Contains(arg, "=")
		default:
			out = append(out, arg)
		}
	}
	if len(out) == 0 {
		out = []string{"."}
	}
	return out
}

func countPackages(goTestArgs []string) int {
	args := append([]string{"list", "-e", "-f", "{{.ImportPath}}"}, patterns(goTestArgs)...)
	out, err := exec.Command("go", args...).Output()
	if err != nil {
		return 0
	}
	return len(strings.Fields(string(out)))
}

func main() {
	var opts options
	flag.DurationVar(&opts.quiet, "quiet", 30*time.Second, "report what is running when nothing has happened for this long")
	flag.DurationVar(&opts.slow, "slow", 30*time.Second, "report a passing test that took at least this long")
	flag.DurationVar(&opts.hang, "hang", 5*time.Minute, "call a test possibly hung after it has been running this long")
	flag.BoolVar(&opts.quitOnHang, "quit-on-hang", false, "send SIGQUIT to the test binary of a package whose test passes -hang (it dumps all goroutines and exits)")
	flag.Parse()
	goTestArgs := flag.Args()

	opts.total = countPackages(goTestArgs)
	cmd := exec.Command("go", append([]string{"test", "-json"}, goTestArgs...)...)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gotestprogress:", err)
		os.Exit(1)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "gotestprogress:", err)
		os.Exit(1)
	}

	now := time.Now()
	tr := newTracker(os.Stdout, opts, now)
	tr.selfPID = os.Getpid()
	tr.binariesForReport = func() []procInfo { return testBinaries(cmd.Process.Pid) }
	quit := func(pkg string) string {
		binaries := binaryFor(testBinaries(cmd.Process.Pid), pkg)
		if len(binaries) == 0 {
			return "no test binary of " + pkg + " found to quit"
		}
		var done []string
		for _, b := range binaries {
			if err := syscall.Kill(b.pid, syscall.SIGQUIT); err == nil {
				done = append(done, strconv.Itoa(b.pid))
			}
		}
		return "sent SIGQUIT to " + pkg + " test binary pid " + strings.Join(done, ", ") + " (its goroutine dump follows in the package's FAIL output)"
	}
	tr.quitHook = quit

	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1)
	events := make(chan event, 256)
	lines := make(chan string, 16)
	go func() {
		reader := bufio.NewReaderSize(stdout, 1<<20)
		for {
			line, err := reader.ReadBytes('\n')
			if len(line) > 0 {
				var ev event
				if jerr := json.Unmarshal(line, &ev); jerr != nil || ev.Action == "" {
					lines <- string(line)
				} else {
					events <- ev
				}
			}
			if err != nil {
				close(events)
				return
			}
		}
	}()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	interrupted := false
loop:
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				break loop
			}
			tr.handle(ev, time.Now())
		case line := <-lines:
			tr.last = time.Now()
			tr.nonJSON++
			fmt.Fprint(os.Stdout, line)
		case now := <-ticker.C:
			tr.watch(now)
		case sig := <-signals:
			switch sig {
			case syscall.SIGUSR1:
				tr.printf(time.Now(), "SIGUSR1: asking test binaries with a test running for %s or more to dump their goroutines", short(opts.quiet))
				for _, name := range tr.order {
					p := tr.pkgs[name]
					if p.done {
						continue
					}
					for _, r := range p.running {
						if !r.paused && time.Since(r.since) >= opts.quiet {
							fmt.Println("         " + quit(name))
							break
						}
					}
				}
			default:
				interrupted = true
				_ = cmd.Process.Signal(sig)
			}
		}
	}
	for {
		select {
		case line := <-lines:
			fmt.Fprint(os.Stdout, line)
			continue
		default:
		}
		break
	}
	exit := 0
	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else {
			exit = 1
		}
	}
	if exit == 0 && tr.sawFailure {
		exit = 1
	}
	if interrupted && exit == 0 {
		exit = 130
	}
	tr.summary(time.Now(), exit)
	os.Exit(exit)
}
