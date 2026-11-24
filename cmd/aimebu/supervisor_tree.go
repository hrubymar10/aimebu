package main

import (
	"errors"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// processRow is one line of the process table: a process, its parent and its process group, when it started and its command line.
type processRow struct {
	pid, ppid, pgid int
	start           time.Time
	args            string
}

// detachableJob is a run_command job the server counts as open on the supervisor's machine: the log path the job's wrapper was started
// with and the earliest moment its process can have started.
type detachableJob struct {
	logPath string
	since   time.Time
}

// detachableWrapper is the part of the shell wrapper the supervisor starts a detachable job with that no other process has: its own log
// writer reading the command's output.
const detachableWrapper = `"$4" command-log "$2"`

// startLayout is how ps prints a start time (lstart), in the local zone.
const startLayout = "Mon Jan _2 15:04:05 2006"

// isDetachableJob says whether row is the leader of a detachable job in jobs. It must lead a process group of its own (a session: the
// supervisor starts it with Setsid), run the detachable wrapper with the job's exit marker and log as its arguments, and have started
// no earlier than the job did (a process that reused a pid started later and has other arguments). Anything that does not verify is
// not a job and is killed with the rest of the tree.
func isDetachableJob(row processRow, jobs []detachableJob) bool {
	if row.pgid != row.pid || row.start.IsZero() || !strings.Contains(row.args, detachableWrapper) {
		return false
	}
	for _, job := range jobs {
		if job.logPath == "" || !strings.Contains(row.args, " "+job.logPath+".exit "+job.logPath+" ") {
			continue
		}
		if !row.start.Before(job.since.Add(-2 * time.Second)) {
			return true
		}
	}
	return false
}

// psFormat is the process table processTable asks ps for: the tree, and the start time and command line that verify a detachable job.
// psTreeFormat is the tree alone, which every ps prints; BusyBox's ps rejects lstart, so on such a host processTable falls back to it.
const (
	psFormat     = "pid=,ppid=,pgid=,lstart=,args="
	psTreeFormat = "pid=,ppid=,pgid="
)

// runPS runs ps over every process with an output format and returns what it printed (tests replace it).
var runPS = func(format string) ([]byte, error) {
	return exec.Command("ps", "-A", "-o", format).Output()
}

// processTable lists every process on the machine. verifiable says whether the rows carry the start time and command line a detachable
// job is verified by: false when ps rejected that query (BusyBox's ps knows no lstart) and the table comes from the tree columns alone.
// A line whose first three columns are numbers is always a row, whatever follows them, so the full query never lists fewer processes
// than the tree query would (a row with a start time it cannot read just cannot be verified).
func processTable() (rows []processRow, verifiable bool, err error) {
	out, fullErr := runPS(psFormat)
	if fullErr != nil {
		out, err = runPS(psTreeFormat)
		if err != nil {
			return nil, false, errors.Join(fullErr, err)
		}
		log.Printf("supervisor swap: ps rejected the start-time query (%v); listing pid, ppid and pgid only, so no detachable run_command job is spared", fullErr)
		return parseProcessTable(out, false), false, nil
	}
	return parseProcessTable(out, true), true, nil
}

// parseProcessTable reads ps output in psFormat (full) or psTreeFormat.
func parseProcessTable(out []byte, full bool) []processRow {
	var rows []processRow
	for _, line := range strings.Split(string(out), "\n") {
		rest := line
		var fields []string
		for len(fields) < 8 {
			rest = strings.TrimLeft(rest, " \t")
			if rest == "" {
				break
			}
			end := strings.IndexAny(rest, " \t")
			if end < 0 {
				end = len(rest)
			}
			fields = append(fields, rest[:end])
			rest = rest[end:]
		}
		if len(fields) < 3 {
			continue
		}
		pid, errPid := strconv.Atoi(fields[0])
		ppid, errPpid := strconv.Atoi(fields[1])
		pgid, errPgid := strconv.Atoi(fields[2])
		if errPid != nil || errPpid != nil || errPgid != nil {
			continue
		}
		row := processRow{pid: pid, ppid: ppid, pgid: pgid}
		if full && len(fields) == 8 {
			row.start, _ = time.ParseInLocation(startLayout, strings.Join(fields[3:8], " "), time.Local)
			row.args = strings.TrimSpace(rest)
		}
		rows = append(rows, row)
	}
	return rows
}

// descendantsOf returns what a kill of root's process tree has to signal besides root: the process groups one of root's descendants
// leads (a harness CLI and a command outside a turn each run in a group of their own, with whatever they started), and the descendants
// that are in no such group, one by one. A group led by a process that is not a descendant is never returned: it may hold strangers.
// A descendant for which spare says true (a detachable job: it lives on without the supervisor) is left out with everything below it.
func descendantsOf(root int, rows []processRow, spare func(processRow) bool) (groups, singles []int) {
	children := map[int][]processRow{}
	for _, row := range rows {
		children[row.ppid] = append(children[row.ppid], row)
	}
	var descendants []processRow
	seen := map[int]bool{root: true}
	queue := []int{root}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, child := range children[parent] {
			if seen[child.pid] {
				continue
			}
			seen[child.pid] = true
			if spare != nil && spare(child) {
				continue
			}
			descendants = append(descendants, child)
			queue = append(queue, child.pid)
		}
	}
	led := map[int]bool{}
	for _, row := range descendants {
		if row.pgid == row.pid {
			led[row.pid] = true
			groups = append(groups, row.pid)
		}
	}
	for _, row := range descendants {
		if !led[row.pgid] {
			singles = append(singles, row.pid)
		}
	}
	return groups, singles
}

// supervisorTreeTargets reads the process table and returns the groups and single processes killSupervisorTree signals besides pid.
// Detachable jobs are spared only when the table could verify them; from the tree columns alone (BusyBox's ps) nothing is spared, and
// the targets are what they were before jobs were spared at all.
func supervisorTreeTargets(pid int, jobs []detachableJob) (groups, singles []int, err error) {
	rows, verifiable, err := processTable()
	var spare func(processRow) bool
	if verifiable {
		spare = func(row processRow) bool { return isDetachableJob(row, jobs) }
	}
	groups, singles = descendantsOf(pid, rows, spare)
	return groups, singles, err
}

// killSupervisorTree kills a supervisor that hangs together with everything it started. The supervisor alone would leave its harness
// processes and commands running: each is in a process group of its own, so they outlive it and would go on writing in the checkout
// while the replacement continues their turns. The tree is read first, the supervisor is killed next (it can start nothing more), and
// then every group and process read before. What runs in a container behind a launcher is not in this tree; the launcher ends it when
// its host side is gone. The detachable run_command jobs among jobs are spared (their groups are verified by isDetachableJob): they are
// no part of what must stop, and the replacement adopts them; a job that does not verify is killed with the rest.
func killSupervisorTree(pid int, jobs []detachableJob) error {
	groups, singles, tableErr := supervisorTreeTargets(pid, jobs)
	err := syscall.Kill(pid, syscall.SIGKILL)
	for _, group := range groups {
		if killErr := syscall.Kill(-group, syscall.SIGKILL); killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
			err = errors.Join(err, killErr)
		}
	}
	for _, single := range singles {
		if killErr := syscall.Kill(single, syscall.SIGKILL); killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
			err = errors.Join(err, killErr)
		}
	}
	if tableErr != nil {
		err = errors.Join(err, errors.New("the supervisor's child processes could not be listed and were not killed: "+tableErr.Error()))
	}
	return err
}
