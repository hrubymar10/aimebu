package adapter

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// A harness that reports a tool call only after it has finished (OpenCode, Vibe) gives the supervisor no event while a long silent command
// runs. What it can see instead is the command itself: a process under the harness that began after the harness last wrote anything. A
// helper the harness started at launch (a person's own MCP server) began before that; a tool call, or a helper that tool call starts, began
// after it. This is the signal ToolProcessSince reports.

// toolProcessSlack is how much earlier than the harness's last output a process may have begun and still count: the clock of a process's
// age is whole seconds, and a harness starts a command a moment after the output that announced it.
const toolProcessSlack = 2 * time.Second

type procInfo struct {
	pid, ppid int
	zombie    bool
	age       time.Duration
}

// ToolProcessSince says whether a live process under root began at or after since (less the slack): a tool call in flight. It reads the
// process table with ps, which both Linux and macOS answer; where that fails it says false, so the idle deadline keeps its old meaning.
// A harness whose tools run in a container reports nothing here (its processes are not under root), so its callers do not ask.
func ToolProcessSince(root int, since time.Time) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,stat=,etime=").Output()
	if err != nil {
		return false
	}
	return toolProcessInTable(string(out), root, since, time.Now())
}

func toolProcessInTable(table string, root int, since, now time.Time) bool {
	procs := parseProcessTable(table)
	children := map[int][]procInfo{}
	for _, proc := range procs {
		children[proc.ppid] = append(children[proc.ppid], proc)
	}
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
			queue = append(queue, child.pid)
			if !child.zombie && !now.Add(-child.age).Before(since.Add(-toolProcessSlack)) {
				return true
			}
		}
	}
	return false
}

// parseProcessTable reads `ps -axo pid=,ppid=,stat=,etime=` lines; a line it cannot read is skipped.
func parseProcessTable(table string) []procInfo {
	var procs []procInfo
	for _, line := range strings.Split(table, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		age, ok := parseElapsed(fields[3])
		if err1 != nil || err2 != nil || !ok {
			continue
		}
		procs = append(procs, procInfo{pid: pid, ppid: ppid, zombie: strings.HasPrefix(fields[2], "Z"), age: age})
	}
	return procs
}

// parseElapsed reads ps's etime, [[dd-]hh:]mm:ss.
func parseElapsed(value string) (time.Duration, bool) {
	var days int64
	if before, after, found := strings.Cut(value, "-"); found {
		d, err := strconv.ParseInt(before, 10, 64)
		if err != nil {
			return 0, false
		}
		days, value = d, after
	}
	parts := strings.Split(value, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	var total int64
	for _, part := range parts {
		n, err := strconv.ParseInt(part, 10, 64)
		if err != nil || n < 0 {
			return 0, false
		}
		total = total*60 + n
	}
	return time.Duration(days*24*3600+total) * time.Second, true
}
