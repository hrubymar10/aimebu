package store

import (
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/answer"
)

func TestHeldDetailNamesKnownParserFieldWithoutContent(t *testing.T) {
	got := heldDetail(`invalid_answer: json: cannot unmarshal string into Go struct field Message.messages.body of type bool`)
	if !strings.Contains(got, "body") || strings.Contains(got, "json:") {
		t.Fatalf("held detail=%q", got)
	}
	got = heldDetail(`invalid_answer: json: unknown field "secret payload"`)
	if strings.Contains(got, "secret payload") {
		t.Fatalf("held detail quoted content: %q", got)
	}
}

func TestHeldRepairNoteIsSpecificAndCarriesFallbackSchema(t *testing.T) {
	for _, tool := range []string{"opencode", "pi"} {
		t.Run(tool, func(t *testing.T) {
			s, f := answerFixture(t, false)
			if _, err := s.db.Exec(`UPDATE agent SET tool=? WHERE id=?`, tool, f.agent.ID); err != nil {
				t.Fatal(err)
			}
			if receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, `{"messages":[{"room":"room_1","body":123}]}`); err != nil || receipt.Result != "held" {
				t.Fatalf("receipt=%+v err=%v", receipt, err)
			}
			note := heldNote(t, s, f.room.ID)
			for _, want := range []string{"body", "submit_answer", "<json-schema>" + string(answer.ResponseSchema) + "</json-schema>"} {
				if !strings.Contains(note.Body, want) {
					t.Fatalf("repair note misses %q", want)
				}
			}
		})
	}
}
