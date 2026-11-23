package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

// handoffOutcomeAnswer sends one answer for the fixture's turn over a machine socket whose hello does or does not report
// supports_handoff_outcome, and returns the receipt.
func handoffOutcomeAnswer(t *testing.T, f *fixture, agent store.Agent, turn store.Turn, supports bool, candidate string) protocol.Frame {
	t.Helper()
	header := http.Header{}
	header.Set("X-Machine-ID", f.machineID)
	header.Set("Authorization", "Bearer "+f.machineKey)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(f.http.URL, "http")+"/api/machine", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	write := func(frame protocol.Frame) {
		raw, _ := json.Marshal(frame)
		if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
			t.Fatal(err)
		}
	}
	read := func() protocol.Frame {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var frame protocol.Frame
		if err := json.Unmarshal(raw, &frame); err != nil {
			t.Fatal(err)
		}
		return frame
	}
	write(protocol.Frame{V: protocol.Version, Type: protocol.Hello, MachineID: f.machineID, JournalID: "journal-outcome", SupportsStopReplay: true,
		SupportsHandoffOutcome: supports, Agents: []protocol.HelloAgent{{AgentID: agent.ID, Generation: 0}}})
	if ack := read(); ack.Type != protocol.HelloAck {
		t.Fatalf("hello_ack=%+v", ack)
	}
	sum := sha256.Sum256([]byte(candidate))
	write(protocol.Frame{V: protocol.Version, Type: protocol.Answer, AgentID: agent.ID, Generation: 0, TurnID: turn.ID, AnswerRev: 0,
		Digest: hex.EncodeToString(sum[:]), Candidate: candidate})
	for {
		frame := read()
		if frame.Type == protocol.Receipt {
			return frame
		}
	}
}

func listedOutcomes(t *testing.T, f *fixture) map[string]any {
	t.Helper()
	resp := f.request(http.MethodGet, "/api/rooms/"+f.room.ID+"/messages", nil, false)
	defer resp.Body.Close()
	var messages []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&messages); err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	for _, message := range messages {
		if message["author_kind"] == "agent" {
			out[fmt.Sprint(message["body"])] = message["outcome"]
		}
	}
	return out
}

// From a supervisor that reports supports_handoff_outcome, a handoff with an outcome is committed and the message API returns the outcome; one
// without is held with an error that names the allowed values; and a plain message needs none.
func TestMachineHandoffOutcome(t *testing.T) {
	t.Parallel()
	t.Run("stored and returned", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		agent, turn, _, _ := f.execution(t)
		receipt := handoffOutcomeAnswer(t, f, agent, turn, true, fmt.Sprintf(`{"messages":[{"room":%q,"body":"it broke","handoff":true,"outcome":"failed"}]}`, f.room.ID))
		if receipt.Result != "committed" {
			t.Fatalf("receipt=%+v", receipt)
		}
		if got := listedOutcomes(t, f); got["it broke"] != "failed" {
			t.Fatalf("listed outcomes=%v", got)
		}
	})
	t.Run("missing is rejected", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		agent, turn, _, _ := f.execution(t)
		receipt := handoffOutcomeAnswer(t, f, agent, turn, true, fmt.Sprintf(`{"messages":[{"room":%q,"body":"report","handoff":true,"outcome":null}]}`, f.room.ID))
		var details struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(receipt.Details, &details)
		if receipt.Result != "held" || details.Error != "message 0 is a handoff without an outcome; set outcome to done, partial or failed" {
			t.Fatalf("receipt=%+v details=%s", receipt, receipt.Details)
		}
		if got := listedOutcomes(t, f); len(got) != 0 {
			t.Fatalf("a rejected handoff was listed: %v", got)
		}
	})
	t.Run("a plain message needs none", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		agent, turn, _, _ := f.execution(t)
		receipt := handoffOutcomeAnswer(t, f, agent, turn, true, fmt.Sprintf(`{"messages":[{"room":%q,"body":"status","handoff":false,"outcome":null}]}`, f.room.ID))
		if receipt.Result != "committed" {
			t.Fatalf("receipt=%+v", receipt)
		}
		if got := listedOutcomes(t, f); got["status"] != nil {
			t.Fatalf("listed outcomes=%v", got)
		}
	})
	// An older supervisor's response schema has no outcome field: its agents' handoffs are committed without one and listed with none.
	t.Run("older supervisor", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		agent, turn, _, _ := f.execution(t)
		receipt := handoffOutcomeAnswer(t, f, agent, turn, false, fmt.Sprintf(`{"messages":[{"room":%q,"body":"report","handoff":true}]}`, f.room.ID))
		if receipt.Result != "committed" {
			t.Fatalf("receipt=%+v", receipt)
		}
		if got, ok := listedOutcomes(t, f)["report"]; !ok || got != nil {
			t.Fatalf("listed outcome=%v (%t), want an agent message without one", got, ok)
		}
	})
}

func TestMessageViewOutcome(t *testing.T) {
	t.Parallel()
	if view := messageView(store.Message{ID: "1", AuthorKind: "agent", Body: "x", Outcome: "partial"}); view["outcome"] != "partial" {
		t.Fatalf("view=%+v", view)
	}
	if _, ok := messageView(store.Message{ID: "2", AuthorKind: "agent", Body: "x"})["outcome"]; ok {
		t.Fatal("a message without an outcome shows one")
	}
}
