package adaptertest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

// What a recorded stream for RunConversation carries: a key and a bearer token that must not reach the log, in a tool's input and in a
// tool's result, and a tool result longer than a record may hold.
const (
	ConversationKey    = "aak_conformance0123456789abcdef"
	ConversationBearer = "tok-conformance-0123456789abcdef"
	// ConversationCommand is a tool input with a bearer token in it.
	ConversationCommand = `curl -H "Authorization: Bearer ` + ConversationBearer + `" https://example.test/status`
	// ConversationKeyOutput is a tool result with an agent key in it.
	ConversationKeyOutput = "the key is " + ConversationKey
)

// ConversationLongOutput is a tool result several times the size a record keeps.
func ConversationLongOutput() string {
	return strings.Repeat("0123456789 abcdef ghij\n", (4*adapter.ConversationToolLimit)/23)
}

// ConversationRecord is one record an adapter wrote.
type ConversationRecord struct {
	Kind adapter.ConversationKind
	Text string
}

// Masked is what the log holds of a text that fits a record: the same text with its secrets removed.
func Masked(text string) string { return adapter.RedactSecrets(text) }

// CutToolOutput is what the log holds of a tool's output: its secrets removed and no more than a tool record keeps.
func CutToolOutput(text string) string {
	return adapter.CleanConversationText(text, adapter.ConversationToolLimit)
}

// ConversationStream writes events as a JSONL file, one event a line, and returns its path: the recorded stream a scripted harness prints.
func ConversationStream(t *testing.T, dir string, events ...any) string {
	t.Helper()
	var out []byte
	for _, event := range events {
		line, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		out = append(append(out, line...), '\n')
	}
	path := filepath.Join(dir, "stream.jsonl")
	if err := os.WriteFile(path, out, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

type conversationRecorder struct {
	mu      sync.Mutex
	records []ConversationRecord
}

func (r *conversationRecorder) Record(kind adapter.ConversationKind, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, ConversationRecord{kind, text})
}

// RunConversation is the conversation-log contract every adapter shares. It runs one turn of an adapter whose harness prints a recorded
// stream, with a sink in the spec, and checks that the adapter wrote exactly the records in want, that the stream's key and bearer
// token are in none of them, and that the long tool result was cut to what a record keeps and says how much is gone. The stream must
// use ConversationCommand as a tool's input, ConversationKeyOutput as a tool's result and ConversationLongOutput as another's.
func RunConversation(t *testing.T, implementation adapter.Adapter, spec adapter.TurnSpec, want []ConversationRecord) {
	t.Helper()
	CheckConversation(t, ConversationRecords(t, implementation, spec), want)
}

// RunUnknownEvents is the other half of the contract: a stream with events the adapter does not know, or cannot read, still runs to its
// end, and each of them leaves an `event` record naming it (and nothing of its content) among the records the adapter does understand.
// The stream must carry ConversationKey inside every unknown or unreadable event.
func RunUnknownEvents(t *testing.T, implementation adapter.Adapter, spec adapter.TurnSpec, want []ConversationRecord) {
	t.Helper()
	got := ConversationRecords(t, implementation, spec)
	events := 0
	for i := 0; i < len(got) || i < len(want); i++ {
		switch {
		case i >= len(got):
			t.Errorf("record %d is missing: want %s %q", i, want[i].Kind, short(want[i].Text))
		case i >= len(want):
			t.Errorf("record %d is not expected: %s %q", i, got[i].Kind, short(got[i].Text))
		case got[i] != want[i]:
			t.Errorf("record %d: got %s %q, want %s %q", i, got[i].Kind, short(got[i].Text), want[i].Kind, short(want[i].Text))
		}
	}
	for _, record := range got {
		if record.Kind == adapter.ConversationEvent {
			events++
		}
		if strings.Contains(record.Text, ConversationKey) || strings.Contains(record.Text, "[redacted]") {
			t.Errorf("a %s record carries the content of an event the adapter does not know: %q", record.Kind, short(record.Text))
		}
	}
	if events == 0 {
		t.Error("no event record was written for an unknown event")
	}
}

// ConversationRecords runs one turn of an adapter with a sink in the spec, to its end, and returns what the adapter wrote.
func ConversationRecords(t *testing.T, implementation adapter.Adapter, spec adapter.TurnSpec) []ConversationRecord {
	t.Helper()
	recorder := &conversationRecorder{}
	spec.ConversationSink = recorder
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	turn, err := implementation.Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	collect(t, ctx, turn)
	if _, err := turn.Wait(); err != nil {
		t.Fatal(err)
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]ConversationRecord(nil), recorder.records...)
}

// CheckConversation is the comparison RunConversation makes, for an adapter path that is not driven through Start.
func CheckConversation(t *testing.T, got, want []ConversationRecord) {
	t.Helper()
	for i := 0; i < len(got) || i < len(want); i++ {
		switch {
		case i >= len(got):
			t.Errorf("record %d is missing: want %s %q", i, want[i].Kind, short(want[i].Text))
		case i >= len(want):
			t.Errorf("record %d is not expected: %s %q", i, got[i].Kind, short(got[i].Text))
		case got[i] != want[i]:
			t.Errorf("record %d: got %s %q, want %s %q", i, got[i].Kind, short(got[i].Text), want[i].Kind, short(want[i].Text))
		}
	}
	kinds := map[adapter.ConversationKind]bool{}
	cut := false
	for _, record := range got {
		kinds[record.Kind] = true
		if strings.Contains(record.Text, ConversationKey) || strings.Contains(record.Text, ConversationBearer) {
			t.Errorf("a %s record carries a secret: %q", record.Kind, short(record.Text))
		}
		if len(record.Text) > adapter.ConversationTextLimit+256 {
			t.Errorf("a %s record holds %d bytes", record.Kind, len(record.Text))
		}
		if record.Kind == adapter.ConversationToolResult && strings.Contains(record.Text, " bytes cut") {
			cut = true
			if len(record.Text) > adapter.ConversationToolLimit+256 {
				t.Errorf("a cut tool result still holds %d bytes", len(record.Text))
			}
		}
	}
	for _, kind := range []adapter.ConversationKind{adapter.ConversationAssistant, adapter.ConversationToolCall, adapter.ConversationToolResult} {
		if !kinds[kind] {
			t.Errorf("no %s record was written", kind)
		}
	}
	if !cut {
		t.Error("the long tool result was not cut")
	}
}

func short(text string) string {
	if len(text) > 160 {
		return text[:160] + "…"
	}
	return text
}
