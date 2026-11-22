package store

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestVisualBlocksReachAgentTurnAsBoundedText(t *testing.T) {
	s, _, room, agent, _ := wakeFixture(t)
	ctx := context.Background()
	_, err := s.InsertMessage(ctx, Message{
		RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "Review the summary.",
		VisualPlanJSON:    `[{"type":"checklist","title":"Release","data":{"items":[{"text":"verify checks","checked":false}]}}]`,
		AppendixPagesJSON: `[{"title":"Details","body":"Run the staging probe first."}]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	delivery, ready, err := s.PrepareDispatch(ctx, agent.ID, "")
	if err != nil || !ready || len(delivery.Notes) != 1 {
		t.Fatalf("delivery=%+v ready=%t err=%v", delivery, ready, err)
	}
	body := delivery.Notes[0].Body
	for _, expected := range []string{"Review the summary.", "Visual block: checklist — Release", "verify checks", "Appendix: Details", "Run the staging probe first."} {
		if !strings.Contains(body, expected) {
			t.Fatalf("delivered note missing %q: %q", expected, body)
		}
	}
}

func TestVisualNoteAdditionalTextIsBoundedAndUTF8(t *testing.T) {
	plan := `[{"type":"markdown","data":{"markdown":"` + strings.Repeat("界", 10000) + `"}}]`
	body := deliveredNoteBody("Original", 42, plan, `[{"title":"Later","body":"Page"}]`)
	if !strings.HasPrefix(body, "Original") || len(body)-len("Original") > 16<<10 || !utf8.ValidString(body) || !strings.Contains(body, "Additional visual content omitted") {
		t.Fatalf("unbounded or invalid delivered note: length=%d valid=%t", len(body), utf8.ValidString(body))
	}
}

// A truncated delivery names the way to the full appendix.
func TestTruncatedDeliveryPointsAtMessageGet(t *testing.T) {
	plan := `[{"type":"markdown","data":{"markdown":"` + strings.Repeat("a", 20000) + `"}}]`
	body := deliveredNoteBody("Original", 42, plan, `[{"title":"A","body":"x"},{"body":"y"}]`)
	if !strings.Contains(body, "message_get #42 returns all 2 appendix page(s)") {
		t.Fatalf("no pointer to message_get: %q", body[len(body)-200:])
	}
	if got := deliveredNoteBody("Original", 42, plan, ""); strings.Contains(got, "message_get") {
		t.Fatalf("a message without pages must not mention message_get: %q", got[len(got)-100:])
	}
}
