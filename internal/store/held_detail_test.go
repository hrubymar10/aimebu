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
	for tool, version := range map[string]string{"opencode": "1.19.6", "pi": "1.6.1"} {
		t.Run(tool, func(t *testing.T) {
			s, f := answerFixture(t, false)
			setHarnessVersion(t, s, f.agent.ID, tool, version)
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

// setHarnessVersion makes the agent a tool of the given probed version, as its machine last reported it.
func setHarnessVersion(t *testing.T, s *Store, agentID, tool, version string) {
	t.Helper()
	var variant string
	if err := s.db.QueryRow(`SELECT variant FROM agent WHERE id=?`, agentID).Scan(&variant); err != nil {
		t.Fatal(err)
	}
	rows := `[{"tool":"` + tool + `","variant":"` + variant + `","version":"` + version + `"}]`
	if _, err := s.db.Exec(`UPDATE machine SET harnesses_json=? WHERE id=(SELECT machine_id FROM agent WHERE id=?)`, rows, agentID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET tool=?,launcher_id=NULL WHERE id=?`, tool, agentID); err != nil {
		t.Fatal(err)
	}
}

func TestHeldRepairNoteOmitsSubmitAnswerWithoutTheTool(t *testing.T) {
	for _, tc := range []struct{ name, tool, version string }{
		{"opencode old", "opencode", "1.19.5"}, {"pi old", "pi", "1.6.0"}, {"opencode unprobed", "opencode", ""}, {"pi unprobed", "pi", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f := answerFixture(t, false)
			setHarnessVersion(t, s, f.agent.ID, tc.tool, tc.version)
			if receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, `{"messages":[{"room":"room_1","body":123}]}`); err != nil || receipt.Result != "held" {
				t.Fatalf("receipt=%+v err=%v", receipt, err)
			}
			note := heldNote(t, s, f.room.ID)
			if strings.Contains(note.Body, "submit_answer") || !strings.Contains(note.Body, "<json-schema>"+string(answer.ResponseSchema)+"</json-schema>") {
				t.Fatalf("a turn without the tool must get the schema and no mention of submit_answer: %q", note.Body)
			}
		})
	}
}
