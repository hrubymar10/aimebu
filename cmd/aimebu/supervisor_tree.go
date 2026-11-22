package main

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// processRow is one line of the process table: a process, its parent and its process group.
type processRow struct{ pid, ppid, pgid int }

// processTable lists every process on the machine (tests replace it).
var processTable = func() ([]processRow, error) {
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,pgid=").Output()
	if err != nil {
		return nil, err
	}
	var rows []processRow
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		pid, errPid := strconv.Atoi(fields[0])
		ppid, errPpid := strconv.Atoi(fields[1])
		pgid, errPgid := strconv.Atoi(fields[2])
		if errPid != nil || errPpid != nil || errPgid != nil {
			continue
		}
		rows = append(rows, processRow{pid, ppid, pgid})
	}
	return rows, nil
}

// descendantsOf returns what a kill of root's process tree has to signal besides root: the process groups one of root's descendants
// leads (a harness CLI and a command outside a turn each run in a group of their own, with whatever they started), and the descendants
// that are in no such group, one by one. A group led by a process that is not a descendant is never returned: it may hold strangers.
func descendantsOf(root int, rows []processRow) (groups, singles []int) {
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

// killSupervisorTree kills a supervisor that hangs together with everything it started. The supervisor alone would leave its harness
// processes and commands running: each is in a process group of its own, so they outlive it and would go on writing in the checkout
// while the replacement continues their turns. The tree is read first, the supervisor is killed next (it can start nothing more), and
// then every group and process read before. What runs in a container behind a launcher is not in this tree; the launcher ends it when
// its host side is gone.
func killSupervisorTree(pid int) error {
	rows, tableErr := processTable()
	groups, singles := descendantsOf(pid, rows)
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
