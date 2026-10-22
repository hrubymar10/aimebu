package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter/claude"
	"github.com/hrubymar10/aimebu/internal/agentlog"
	"github.com/hrubymar10/aimebu/internal/protocol"
)

// A turn's stderr is kept in the agent's output log on the machine, redacted, and the server can read its tail over the machine socket;
// the supervisor says in hello that it can.
func TestAgentOutputIsCapturedAndServedOverTheMachineSocket(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const agentID = "agt_logs"
	peer := newMachinePeer(t, activeAck(agentID))
	dir := t.TempDir()
	j, err := OpenJournal(ctx, filepath.Join(dir, "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	harness := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(harness, []byte("#!/bin/sh\necho 'warning: Bearer aak_secret_value in a header' >&2\necho 'second line of stderr' >&2\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c := NewClient("machine", "key", peer.server.URL, j, claude.New())
	c.OnError = func(error) {}
	if want := filepath.Join(dir, "agent-logs"); c.AgentLogs == nil || c.AgentLogs.Dir() != want {
		t.Fatalf("the output store is not beside the journal: %+v", c.AgentLogs)
	}
	if err := c.writeAgentToolsConfig(agentID, "aak_"+strings.Repeat("s", 43)); err != nil {
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
	hello := <-peer.hellos
	if !hello.SupportsAgentLogs {
		t.Fatal("the supervisor does not say it keeps agent output")
	}
	// Nothing captured yet.
	peer.out <- protocol.Frame{V: protocol.Version, Type: protocol.AgentLogRead, ReqID: "r0", AgentID: agentID}
	if empty := peer.next(t, protocol.AgentLogResult); empty.ReqID != "r0" || empty.Code != "no_log" {
		t.Fatalf("before any turn: %+v", empty)
	}

	peer.out <- testDelivery(agentID, "turn-logs", "claude", "native", t.TempDir())
	peer.next(t, protocol.Process)
	// The turn is over once its exit is reported; the capture is closed right after, so ask until the output is there.
	deadline := time.Now().Add(20 * time.Second)
	var page agentlog.Page
	for i := 0; ; i++ {
		peer.out <- protocol.Frame{V: protocol.Version, Type: protocol.AgentLogRead, ReqID: "r1", AgentID: agentID, LogLines: 50, LogMaxBytes: 4096}
		result := peer.next(t, protocol.AgentLogResult)
		if result.Code == "" {
			if err := json.Unmarshal(result.Details, &page); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(page.Text, "second line of stderr") {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the output never arrived: %+v %+v", result, page)
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, want := range []string{"=== turn turn-logs gen 0 normal ===", "warning: Bearer [redacted] in a header", "second line of stderr"} {
		if !strings.Contains(page.Text, want) {
			t.Errorf("the output lacks %q:\n%s", want, page.Text)
		}
	}
	if strings.Contains(page.Text, "aak_secret_value") {
		t.Errorf("a secret reached the output log:\n%s", page.Text)
	}
	// Another agent has none, and an id that is a path names nothing.
	for _, id := range []string{"agt_other", "../../etc/passwd"} {
		peer.out <- protocol.Frame{V: protocol.Version, Type: protocol.AgentLogRead, ReqID: "r2", AgentID: id}
		if result := peer.next(t, protocol.AgentLogResult); result.Code != "no_log" {
			t.Errorf("%q: %+v", id, result)
		}
	}
	cancel()
	<-done
}
