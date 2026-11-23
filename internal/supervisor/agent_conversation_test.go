package supervisor

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/claude"
	"github.com/hrubymar10/aimebu/v2/internal/agentlog"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

// A turn's conversation is kept in the agent's conversation log on the machine: the supervisor frames it (the turn's start, what the
// agent was asked, how it ended) and the adapter writes what the harness's stream said in between, masked. The server reads its tail
// over the machine socket by naming the stream, and the supervisor says in hello that it can.
func TestAgentConversationIsCapturedAndServedOverTheMachineSocket(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const agentID = "agt_conv"
	peer := newMachinePeer(t, activeAck(agentID))
	dir := t.TempDir()
	j, err := OpenJournal(ctx, filepath.Join(dir, "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	harness := filepath.Join(t.TempDir(), "claude")
	// PATH is empty in this test, so the script uses only what the shell has built in.
	script := `#!/bin/sh
printf '%s\n' '{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"I will check the status."},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"echo aak_secret_value > key.txt"}}]}}'
printf '%s\n' '{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"status ok\nsecond line"}]}}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"{\"messages\":[{\"room\":\"room\",\"body\":\"done\"}]}"}'
echo 'a warning on stderr' >&2
`
	if err := testenv.WriteExecutable(harness, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	c := NewClient("machine", "key", peer.server.URL, j, claude.New())
	c.OnError = func(error) {}
	if err := c.setAgentKey(agentID, "aak_"+strings.Repeat("s", 43)); err != nil {
		t.Fatal(err)
	}
	for i := range c.Harnesses {
		if c.Harnesses[i].Tool == "claude" && c.Harnesses[i].Variant == "native" {
			c.Harnesses[i].Available = true
			c.Harnesses[i].LauncherPath = harness
		}
	}
	done := make(chan error, 1)
	go func() { done <- c.RunOnce(ctx) }()
	if hello := <-peer.hellos; !hello.SupportsAgentConversationLog {
		t.Fatal("the supervisor does not say it keeps the conversation log")
	}
	read := func(stream string) protocol.Frame {
		peer.out <- protocol.Frame{V: protocol.Version, Type: protocol.AgentLogRead, ReqID: "r-" + stream, AgentID: agentID, LogLines: 50, LogStream: stream}
		return peer.next(t, protocol.AgentLogResult)
	}
	if empty := read("conversation"); empty.Code != "no_log" {
		t.Fatalf("before any turn: %+v", empty)
	}
	if bad := read("stdout"); bad.Code != "invalid_stream" {
		t.Fatalf("an unknown stream: %+v", bad)
	}

	peer.out <- testDelivery(agentID, "turn-conv", "claude", "native", t.TempDir())
	peer.next(t, protocol.Process)
	// The log is closed right after the exit is reported, so ask until the turn's end is there.
	deadline := time.Now().Add(20 * time.Second)
	var page agentlog.Page
	for {
		result := read("conversation")
		if result.Code == "" {
			if err := json.Unmarshal(result.Details, &page); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(page.Text, " "+string(adapter.ConversationTurnEnd)+" ") {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the conversation never arrived: %+v %+v", result, page)
		}
		time.Sleep(50 * time.Millisecond)
	}
	var kinds []string
	for _, line := range strings.Split(strings.TrimRight(page.Text, "\n"), "\n") {
		if strings.HasPrefix(line, "\t") {
			continue
		}
		fields := strings.SplitN(line, " ", 3)
		if len(fields) < 2 {
			t.Fatalf("not a record line: %q", line)
		}
		if _, err := time.Parse("2006-01-02T15:04:05Z", fields[0]); err != nil {
			t.Fatalf("a record without a time: %q", line)
		}
		kinds = append(kinds, fields[1])
	}
	if got := strings.Join(kinds, " "); got != "turn_start prompt assistant tool_call tool_result turn_end" {
		t.Fatalf("records: %s\n%s", got, page.Text)
	}
	for _, want := range []string{
		" turn_start turn turn-conv gen 0 normal claude/native test\n",
		`"body":"task"`,
		" assistant I will check the status.\n",
		` tool_call Bash {"command":"echo [redacted] > key.txt"}` + "\n",
		" tool_result Bash\n\tstatus ok\n\tsecond line\n",
		" turn_end answered, exit 0\n",
	} {
		if !strings.Contains(page.Text, want) {
			t.Errorf("the conversation lacks %q:\n%s", want, page.Text)
		}
	}
	if strings.Contains(page.Text, "aak_secret_value") {
		t.Errorf("a secret reached the conversation log:\n%s", page.Text)
	}
	// The two streams stay apart: a read that names none is the stderr it always was.
	var stderr agentlog.Page
	if result := read(""); result.Code != "" || json.Unmarshal(result.Details, &stderr) != nil ||
		!strings.Contains(stderr.Text, "a warning on stderr") || strings.Contains(stderr.Text, "tool_call") {
		t.Errorf("the stderr stream: %+v %q", result, stderr.Text)
	}
	cancel()
	<-done
}

func TestTurnEndText(t *testing.T) {
	for want, exit := range map[string]adapter.ExitInfo{
		"answered, exit 0":                             {TerminalEvent: true},
		"answered, process kept warm":                  {TerminalEvent: true, ProcessAlive: true},
		"no answer, exit 1":                            {ExitCode: 1},
		"no answer, cancelled, exit 137":               {ExitCode: 137, CancelIntent: true},
		"no answer, interrupted, exit 130":             {ExitCode: 130, Interrupted: true},
		"no answer, ended by the turn limit, exit 124": {ExitCode: 124, LimitStall: true},
	} {
		if got := turnEndText(exit); got != want {
			t.Errorf("%+v: %q, want %q", exit, got, want)
		}
	}
}

type turnEndSink struct {
	mu  sync.Mutex
	got []string
}

func (s *turnEndSink) Record(kind adapter.ConversationKind, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, string(kind)+"|"+text)
}

// Whether a turn's process was kept warm is known only once the pool has settled it, which is after the exit is journaled: the
// conversation log's turn end is written after that, so a turn that left its process idle says so, and one whose process ended says
// its exit.
func TestConversationTurnEndIsWrittenAfterTheWarmProcessIsSettled(t *testing.T) {
	h := newWarmHarness(t)
	h.spawn("agt_a")
	sink := &turnEndSink{}
	follow := func(spec *adapter.TurnSpec) {
		h.c.conversations.Store(spec.TurnID, &turnConversation{conv: adapter.NewConversation(sink)})
	}
	h.run("agt_a", follow)
	if !h.idle("agt_a") {
		t.Fatal("the turn did not leave a warm process")
	}
	h.run("agt_a", func(spec *adapter.TurnSpec) {
		follow(spec)
		spec.Mode = adapter.ModeCorrection // a turn that runs cold: its own process ends with it
	})
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.got) != 2 || sink.got[0] != "turn_end|answered, process kept warm" || !strings.HasPrefix(sink.got[1], "turn_end|") || strings.Contains(sink.got[1], "kept warm") {
		t.Fatalf("turn ends: %q", sink.got)
	}
}
