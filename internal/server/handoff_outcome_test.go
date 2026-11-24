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

// machineSocket connects to the fixture's machine endpoint with a hello that does or does not report supports_handoff_outcome, binding the
// agent (with its open turns, as a supervisor reports what its journal holds), and returns the socket's write and read.
func machineSocket(t *testing.T, ctx context.Context, f *fixture, agent store.Agent, supports bool, open ...protocol.OpenTurn) (*websocket.Conn, func(protocol.Frame), func() protocol.Frame) {
	t.Helper()
	header := http.Header{}
	header.Set("X-Machine-ID", f.machineID)
	header.Set("Authorization", "Bearer "+f.machineKey)
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(f.http.URL, "http")+"/api/machine", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
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
		SupportsHandoffOutcome: supports, Agents: []protocol.HelloAgent{{AgentID: agent.ID, Generation: 0, OpenTurns: open}}})
	if ack := read(); ack.Type != protocol.HelloAck {
		t.Fatalf("hello_ack=%+v", ack)
	}
	return conn, write, read
}

func answerFrame(agent store.Agent, turn store.Turn, candidate string) protocol.Frame {
	sum := sha256.Sum256([]byte(candidate))
	return protocol.Frame{V: protocol.Version, Type: protocol.Answer, AgentID: agent.ID, Generation: 0, TurnID: turn.ID, AnswerRev: 0,
		Digest: hex.EncodeToString(sum[:]), Candidate: candidate}
}

func receiptOf(read func() protocol.Frame) protocol.Frame {
	for {
		if frame := read(); frame.Type == protocol.Receipt {
			return frame
		}
	}
}

// handoffOutcomeAnswer dispatches the fixture's turn to a machine whose hello does or does not report supports_handoff_outcome (dispatched),
// then sends one answer for it, over the same connection when answered equals dispatched, else over a new connection with the other flag
// that reports the turn as journaled with its answer (a supervisor swap replaying it). It returns the receipt.
func handoffOutcomeAnswer(t *testing.T, f *fixture, agent store.Agent, turn store.Turn, delivery protocol.Delivery, dispatched, answered bool, candidate string) protocol.Frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, write, read := machineSocket(t, ctx, f, agent, dispatched)
	defer conn.Close(websocket.StatusNormalClosure, "")
	if err := f.srv.Dispatch(ctx, delivery); err != nil {
		t.Fatal(err)
	}
	for read().Type != protocol.Deliver {
	}
	frame := answerFrame(agent, turn, candidate)
	if answered == dispatched {
		write(frame)
		return receiptOf(read)
	}
	conn.Close(websocket.StatusNormalClosure, "swap")
	replay, write, read := machineSocket(t, ctx, f, agent, answered,
		protocol.OpenTurn{TurnID: turn.ID, Exec: "running", ProcessAlive: true, HasAnswer: true, Digest: frame.Digest})
	defer replay.Close(websocket.StatusNormalClosure, "")
	write(frame)
	return receiptOf(read)
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
		agent, turn, _, delivery := f.execution(t)
		receipt := handoffOutcomeAnswer(t, f, agent, turn, delivery, true, true, fmt.Sprintf(`{"messages":[{"room":%q,"body":"it broke","handoff":true,"outcome":"failed"}]}`, f.room.ID))
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
		agent, turn, _, delivery := f.execution(t)
		receipt := handoffOutcomeAnswer(t, f, agent, turn, delivery, true, true, fmt.Sprintf(`{"messages":[{"room":%q,"body":"report","handoff":true,"outcome":null}]}`, f.room.ID))
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
		agent, turn, _, delivery := f.execution(t)
		receipt := handoffOutcomeAnswer(t, f, agent, turn, delivery, true, true, fmt.Sprintf(`{"messages":[{"room":%q,"body":"status","handoff":false,"outcome":null}]}`, f.room.ID))
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
		agent, turn, _, delivery := f.execution(t)
		receipt := handoffOutcomeAnswer(t, f, agent, turn, delivery, false, false, fmt.Sprintf(`{"messages":[{"room":%q,"body":"report","handoff":true}]}`, f.room.ID))
		if receipt.Result != "committed" {
			t.Fatalf("receipt=%+v", receipt)
		}
		if got, ok := listedOutcomes(t, f)["report"]; !ok || got != nil {
			t.Fatalf("listed outcome=%v (%t), want an agent message without one", got, ok)
		}
	})
}

// Whether a handoff needs an outcome is decided by the turn, from the supervisor it was dispatched to, not by the connection that delivers
// the answer: an answer an older supervisor journaled and a newer one replays after a swap commits its handoff with no outcome, and a
// turn dispatched to a supervisor that offered the field still needs one when its answer arrives over an older connection.
func TestMachineHandoffOutcomeFollowsTheDispatch(t *testing.T) {
	t.Parallel()
	t.Run("journaled by an older supervisor, replayed by a newer one", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		agent, turn, _, delivery := f.execution(t)
		receipt := handoffOutcomeAnswer(t, f, agent, turn, delivery, false, true, fmt.Sprintf(`{"messages":[{"room":%q,"body":"report","handoff":true}]}`, f.room.ID))
		if receipt.Result != "committed" {
			t.Fatalf("receipt=%+v details=%s", receipt, receipt.Details)
		}
		if got, ok := listedOutcomes(t, f)["report"]; !ok || got != nil {
			t.Fatalf("listed outcome=%v (%t), want an agent message without one", got, ok)
		}
	})
	t.Run("dispatched to a newer supervisor, answered over an older one", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		agent, turn, _, delivery := f.execution(t)
		receipt := handoffOutcomeAnswer(t, f, agent, turn, delivery, true, false, fmt.Sprintf(`{"messages":[{"room":%q,"body":"report","handoff":true}]}`, f.room.ID))
		if receipt.Result != "held" {
			t.Fatalf("receipt=%+v details=%s", receipt, receipt.Details)
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
