package supervisor

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

// The turn's answer frame reports whether that turn was offered the submit_answer tool: what the adapter decided for the turn —
// its mode and the probed version, a launcher's included — not a fact the server reads from another source.
func TestAnswerFrameCarriesTheTurnsAnswerTool(t *testing.T) {
	for _, tc := range []struct {
		name, tool, version string
		launcher, want      bool
	}{
		{"opencode at the supported version", "opencode", "1.19.6", false, true},
		{"pi at the supported version", "pi", "1.6.1", false, true},
		{"opencode below the minimum", "opencode", "1.19.5", false, false},
		{"pi below the minimum", "pi", "1.6.0", false, false},
		{"another harness", "claude", "2.0.0", false, false},
		{"a launcher agent at the supported version", "opencode", "1.19.6", true, true},
		{"a launcher agent below the minimum", "opencode", "1.19.5", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Keep NewClient's harness discovery away from any real CLI on PATH.
			t.Setenv("PATH", t.TempDir())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			j, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer j.Close()
			peer := newMachinePeer(t, func(protocol.Frame) protocol.Frame {
				definition := protocol.LauncherDefinition{ID: "ln_tool", Tool: tc.tool, Name: "harness-docker", Enabled: true}
				if tc.launcher {
					// The version the launcher's container answers with, as a probe of its --version would.
					path := filepath.Join(t.TempDir(), "fake-launcher")
					if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\necho "+tc.tool+" "+tc.version+"\n"), 0o700); err != nil {
						t.Fatal(err)
					}
					definition.Command = path
				}
				return protocol.Frame{V: protocol.Version, Type: protocol.HelloAck,
					Desired:             []protocol.DesiredAgent{{AgentID: "agent", Desired: "active"}},
					LauncherDefinitions: []protocol.LauncherDefinition{definition}}
			})
			fake := adapter.NewFake()
			fake.Script("turn-tool", answerStep(`{"messages":[]}`), adapter.Step{Kind: adapter.StepExit, Exit: adapter.ExitInfo{TerminalEvent: true}})
			client := NewClient("machine", "key", peer.server.URL, j, fake)
			if !tc.launcher {
				for i := range client.Harnesses {
					if client.Harnesses[i].Tool == tc.tool && client.Harnesses[i].Variant == "native" {
						client.Harnesses[i].Available = true
						client.Harnesses[i].LauncherPath = filepath.Join(t.TempDir(), "missing")
						client.Harnesses[i].Version = tc.version
					}
				}
			}
			done := make(chan error, 1)
			go func() { done <- client.RunOnce(ctx) }()
			checkout := canonicalTempDir(t)
			delivery := testDelivery("agent", "turn-tool", tc.tool, "native", checkout)
			if tc.launcher {
				delivery = protocol.DeliverFrame(protocol.Delivery{AgentID: "agent", TurnID: "turn-tool", Mode: adapter.ModeNormal,
					Agent:        adapter.AgentSpec{ID: "agent", Tool: tc.tool, Variant: "docker", Model: "test", LauncherID: "ln_tool"},
					CheckoutPath: checkout, Context: json.RawMessage(`{"role_body":"worker"}`),
					Notes: []protocol.DeliveryNote{{NoteID: "note-turn-tool", RoomID: "room", MessageID: "msg", Body: "task", From: "owner"}}})
			}
			peer.out <- delivery
			answer := peer.next(t, protocol.Answer)
			if tc.want && (answer.AnswerTool == nil || !*answer.AnswerTool) {
				t.Fatalf("answer frame carries answer_tool=%v, want true", answer.AnswerTool)
			}
			if !tc.want && (answer.AnswerTool == nil || *answer.AnswerTool) {
				t.Fatalf("answer frame carries answer_tool=%v, want false", answer.AnswerTool)
			}
			select {
			case err := <-done:
				t.Fatalf("RunOnce returned early: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}

// A replayed answer carries the fact again: it is journaled with the answer, so a supervisor that reconnects before the server
// receipted the answer still tells it what the turn had. A row an older supervisor wrote carries no report.
func TestPendingAnswerCarriesTheAnswerToolFact(t *testing.T) {
	ctx := context.Background()
	j, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	for _, turnID := range []string{"turn-new", "turn-old"} {
		if _, err := j.RecordStart(ctx, StartRecord{TurnID: turnID, AgentID: "agent", Mode: "normal", SpecJSON: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := j.RecordAnswer(ctx, "turn-new", 0, "digest-new", json.RawMessage(`{}`), true); err != nil {
		t.Fatal(err)
	}
	// A row an older supervisor wrote, before the journal kept the fact.
	if _, err := j.db.Exec(`INSERT INTO answer(turn_id,answer_rev,digest,candidate,created_at) VALUES ('turn-old',0,'digest-old',X'7B7D',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	answers, err := j.PendingAnswers(ctx)
	if err != nil || len(answers) != 2 {
		t.Fatalf("pending answers=%+v err=%v", answers, err)
	}
	byTurn := map[string]PendingAnswer{}
	for _, answer := range answers {
		byTurn[answer.TurnID] = answer
	}
	if got := byTurn["turn-new"]; got.AnswerTool == nil || !*got.AnswerTool {
		t.Fatalf("a journaled answer replays answer_tool=%v, want true", got.AnswerTool)
	}
	if got := byTurn["turn-old"]; got.AnswerTool != nil {
		t.Fatalf("an older supervisor's row replays answer_tool=%v, want none", got.AnswerTool)
	}
}
