package server

import (
	"strconv"
	"strings"
	"testing"
)

func TestCommandLogPollAdvice(t *testing.T) {
	var p commandLogPoller
	read := func(turn, job, state, log string) string {
		return p.observe("agt_test", turn, job, state, log)
	}
	if got := read("turn_one", "cmd_one", "running", "first line"); got != "" {
		t.Fatalf("first read: %q", got)
	}
	for repeat := 1; repeat <= 3; repeat++ {
		got := read("turn_one", "cmd_one", "running", "first line")
		if !strings.Contains(got, "end your turn") || !strings.Contains(got, "woken when it finishes") {
			t.Fatalf("repeat %d: %q", repeat, got)
		}
		if repeat == 3 && !strings.Contains(got, "Stop polling") {
			t.Fatalf("third repeat did not escalate: %q", got)
		}
	}
	if got := read("turn_one", "cmd_one", "running", "new line"); got != "" {
		t.Fatalf("new output did not reset: %q", got)
	}
	if got := read("turn_one", "cmd_one", "running", "new line"); strings.Contains(got, "Stop polling") {
		t.Fatalf("new output did not reset count: %q", got)
	}
	if got := read("turn_two", "cmd_one", "running", "new line"); got != "" {
		t.Fatalf("new turn did not reset: %q", got)
	}
	if got := read("turn_two", "cmd_one", "done", "new line"); got != "" {
		t.Fatalf("finished job got advice: %q", got)
	}
	if got := read("turn_two", "cmd_one", "running", "new line"); got != "" {
		t.Fatalf("finished job did not clear state: %q", got)
	}
	if got := read("turn_two", "cmd_two", "running", "new line"); got != "" {
		t.Fatalf("new job inherited state: %q", got)
	}
}

func TestCommandLogPollStateIsBounded(t *testing.T) {
	var p commandLogPoller
	for i := 0; i < 512; i++ {
		p.observe("agt_test", "turn_one", "cmd_"+strconv.Itoa(i), "running", "log")
	}
	if len(p.seen) > 256 {
		t.Fatalf("poll state has %d entries", len(p.seen))
	}
}
