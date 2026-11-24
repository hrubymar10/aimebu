package main

import (
	"bytes"
	"fmt"
	"log"
	"sort"
	"strings"
	"testing"
	"time"
)

// fakePS replaces ps for one test: full answers the start-time query (psFormat), tree the tree-only one (psTreeFormat). A nil answer
// is ps rejecting that format, as BusyBox's ps rejects lstart. It returns the formats ps was asked for.
func fakePS(t *testing.T, full, tree []byte) *[]string {
	t.Helper()
	var asked []string
	previous := runPS
	runPS = func(format string) ([]byte, error) {
		asked = append(asked, format)
		switch {
		case format == psFormat && full != nil:
			return full, nil
		case format == psTreeFormat && tree != nil:
			return tree, nil
		}
		return nil, fmt.Errorf("ps: bad -o argument %q", format)
	}
	t.Cleanup(func() { runPS = previous })
	return &asked
}

// captureLog collects what the package logs during a test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buffer bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buffer)
	t.Cleanup(func() { log.SetOutput(previous) })
	return &buffer
}

// treeFixture is a supervisor (100) with a harness group (200, with a tool 201), a plain command's group (300), a helper in its own
// group (400), and a detachable job leading its own session (310, with a child group 311), as ps prints it with and without start times.
func treeFixture(started time.Time, jobStart string) (full, tree []byte, jobs []detachableJob) {
	stamp := started.Format(startLayout)
	wrapper := `sh -c sleep 300; : "$4" command-log "$2" sh /tmp/tree/job.log.exit /tmp/tree/job.log true /bin/logger`
	lines := []struct {
		pid, ppid, pgid int
		start, args     string
	}{
		{1, 0, 1, stamp, "/sbin/init"}, {100, 1, 100, stamp, "aimebu supervisor"},
		{200, 100, 200, stamp, "claude"}, {201, 200, 200, stamp, "bash -c go test"},
		{300, 100, 300, stamp, "sleep 300"}, {400, 100, 100, stamp, "helper"},
		{310, 100, 310, jobStart, wrapper}, {311, 310, 311, stamp, "go test ./..."},
		{900, 1, 900, stamp, "a stranger"},
	}
	var fullText, treeText strings.Builder
	for _, line := range lines {
		fmt.Fprintf(&fullText, "%5d %5d %5d %s %s\n", line.pid, line.ppid, line.pgid, line.start, line.args)
		fmt.Fprintf(&treeText, "%5d %5d %5d\n", line.pid, line.ppid, line.pgid)
	}
	return []byte(fullText.String()), []byte(treeText.String()), []detachableJob{{logPath: "/tmp/tree/job.log", since: started}}
}

func sortedTargets(groups, singles []int) string {
	sort.Ints(groups)
	sort.Ints(singles)
	return fmt.Sprint(groups, singles)
}

// With lstart, the tree kill spares the verified detachable job and what it started, and kills the rest: the normal path, unchanged.
func TestSupervisorTreeTargetsSpareAVerifiedJob(t *testing.T) {
	started := time.Now().Add(-time.Minute).Truncate(time.Second)
	full, tree, jobs := treeFixture(started, started.Format(startLayout))
	asked := fakePS(t, full, tree)
	logged := captureLog(t)
	groups, singles, err := supervisorTreeTargets(100, jobs)
	if err != nil || sortedTargets(groups, singles) != "[200 300] [400]" {
		t.Fatalf("targets %v %v, err %v", groups, singles, err)
	}
	if fmt.Sprint(*asked) != fmt.Sprint([]string{psFormat}) || logged.Len() != 0 {
		t.Fatalf("ps asked %v, logged %q", *asked, logged)
	}
}

// BusyBox's ps rejects lstart: the table falls back to pid, ppid and pgid, every descendant group is listed, the job included, nothing is
// spared, and the log says so. The targets are what the three-column query gave before jobs were spared.
func TestSupervisorTreeTargetsFallBackWithoutLstart(t *testing.T) {
	started := time.Now().Add(-time.Minute).Truncate(time.Second)
	_, tree, jobs := treeFixture(started, started.Format(startLayout))
	asked := fakePS(t, nil, tree)
	logged := captureLog(t)
	groups, singles, err := supervisorTreeTargets(100, jobs)
	if err != nil || sortedTargets(groups, singles) != "[200 300 310 311] [400]" {
		t.Fatalf("targets %v %v, err %v", groups, singles, err)
	}
	if fmt.Sprint(*asked) != fmt.Sprint([]string{psFormat, psTreeFormat}) {
		t.Fatalf("ps asked %v", *asked)
	}
	if !strings.Contains(logged.String(), "ps rejected the start-time query") || !strings.Contains(logged.String(), "no detachable run_command job is spared") {
		t.Fatalf("log: %q", logged)
	}
	rows := parseProcessTable(tree, false)
	oldGroups, oldSingles := descendantsOf(100, rows, nil)
	if sortedTargets(groups, singles) != sortedTargets(oldGroups, oldSingles) {
		t.Fatalf("fallback targets %v %v, the three-column query's %v %v", groups, singles, oldGroups, oldSingles)
	}
}

// A line of the full query whose start time ps prints in a shape this parser does not know is still a row of the tree: its process is
// killed rather than missed (it cannot be verified, so it is not spared).
func TestProcessTableKeepsARowWithAnUnreadableStart(t *testing.T) {
	started := time.Now().Add(-time.Minute).Truncate(time.Second)
	full, tree, jobs := treeFixture(started, "yesterday")
	fakePS(t, full, tree)
	groups, singles, err := supervisorTreeTargets(100, jobs)
	if err != nil || sortedTargets(groups, singles) != "[200 300 310 311] [400]" {
		t.Fatalf("targets %v %v, err %v", groups, singles, err)
	}
}

// When neither query answers, nothing is listed and the error names both.
func TestProcessTableReportsBothFailures(t *testing.T) {
	fakePS(t, nil, nil)
	captureLog(t)
	groups, singles, err := supervisorTreeTargets(100, nil)
	if len(groups) != 0 || len(singles) != 0 || err == nil || !strings.Contains(err.Error(), psFormat) || !strings.Contains(err.Error(), psTreeFormat) {
		t.Fatalf("targets %v %v, err %v", groups, singles, err)
	}
}
