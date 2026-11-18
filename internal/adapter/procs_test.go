package adapter

import (
	"os/exec"
	"testing"
	"time"
)

func TestParseElapsed(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"00:05": 5 * time.Second, "01:05": 65 * time.Second, "2:03:04": 2*time.Hour + 3*time.Minute + 4*time.Second,
		"3-02:03:04": 3*24*time.Hour + 2*time.Hour + 3*time.Minute + 4*time.Second,
	} {
		if got, ok := parseElapsed(in); !ok || got != want {
			t.Errorf("%q: got %v %t want %v", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "5", "a:b", "-1:00", "1:2:3:4"} {
		if _, ok := parseElapsed(bad); ok {
			t.Errorf("%q must not parse", bad)
		}
	}
}

// A process under the harness that began after its last output is a tool in flight; an older helper, an unrelated process and a zombie
// are not.
func TestToolProcessInTable(t *testing.T) {
	now := time.Unix(10_000, 0)
	since := now.Add(-100 * time.Second) // the harness last wrote 100 s ago
	table := `
 100     1 S    05:00
 200   100 S    10:00
 300   100 S    01:30
 400   300 S    00:50
 500     1 S    00:10
 600   100 Z    00:10
`
	// 100 is the harness: its children are the MCP helper 200 (10 min old), the tool 300 (90 s: older than the slack), and a zombie.
	if toolProcessInTable(table, 100, since, now) != true {
		t.Fatal("the grandchild 400 began after the last output: a tool is running")
	}
	table = " 100 1 S 05:00\n 200 100 S 10:00\n 300 100 S 01:45\n 500 1 S 00:10\n 600 100 Z 00:10\n"
	if toolProcessInTable(table, 100, since, now) {
		t.Fatal("a helper started before the last output, an unrelated process and a zombie are not tools")
	}
	if toolProcessInTable(table, 999, since, now) {
		t.Fatal("a harness with no such process has no tool")
	}
}

func TestToolProcessSinceSeesARealChild(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 30; true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for !ToolProcessSince(cmd.Process.Pid, time.Now().Add(-time.Minute)) {
		if time.Now().After(deadline) {
			t.Fatal("the running child was never seen")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if ToolProcessSince(cmd.Process.Pid, time.Now().Add(time.Hour)) {
		t.Fatal("a process that began before the harness's last output is not a tool")
	}
	if ToolProcessSince(cmd.Process.Pid+1_000_000, time.Now().Add(-time.Minute)) {
		t.Fatal("no process, no tool")
	}
}
