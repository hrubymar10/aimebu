package answer

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestParseCanonicalCandidate(t *testing.T) {
	raw := []byte(`{"messages":[{"room":"room-1","body":"answer","reply_to":"msg-1","attachments":["att-1"],"ask":{"kind":"question","text":"Which?","options":["A","B"],"free_text":true},"handoff":true}]}`)
	parsed, err := Parse(raw, false)
	if err != nil || len(parsed.Messages) != 1 || parsed.Messages[0].Ask.Kind != "question" || !parsed.Messages[0].Handoff {
		t.Fatalf("parsed=%+v error=%v", parsed, err)
	}
	if parsed, err := Parse([]byte(`{"messages":[]}`), false); err != nil || len(parsed.Messages) != 0 {
		t.Fatalf("empty completion=%+v error=%v", parsed, err)
	}
	if parsed, err := Parse([]byte(`{"messages":[],"reconciliation":{"status":"clean","findings":[]}}`), true); err != nil || parsed.Reconciliation.Status != "clean" {
		t.Fatalf("diagnostic=%+v error=%v", parsed, err)
	}
}

func TestParseAcceptsAMessageNumberAsReplyTo(t *testing.T) {
	parsed, err := Parse([]byte(`{"messages":[{"room":"room-1","body":"x","reply_to":36226,"thread_root":12},{"room":"room-1","body":"y","reply_to":"msg_1"}]}`), false)
	if err != nil || parsed.Messages[0].ReplyTo != "36226" || parsed.Messages[0].ThreadRoot != "12" || parsed.Messages[1].ReplyTo != "msg_1" {
		t.Fatalf("number reply_to: %+v %v", parsed, err)
	}
	for _, bad := range []string{`"reply_to":0`, `"reply_to":-3`, `"reply_to":1.5`, `"reply_to":true`, `"reply_to":[1]`} {
		if _, err := Parse([]byte(`{"messages":[{"room":"room-1","body":"x",`+bad+`}]}`), false); err == nil {
			t.Fatalf("%s was accepted", bad)
		}
	}
}

func TestParseSingleMarkdownFence(t *testing.T) {
	for _, opening := range []string{"```json", "```"} {
		for _, newline := range []string{"\n", "\r\n"} {
			raw := []byte("  " + newline + opening + newline + "{\"messages\":[{\"room\":\"room-1\",\"body\":\"reviewed\"}]}" + newline + "```" + newline)
			parsed, err := Parse(raw, false)
			if err != nil || len(parsed.Messages) != 1 || parsed.Messages[0].Body != "reviewed" || len(parsed.Warnings) != 1 {
				t.Fatalf("opening=%q newline=%q parsed=%+v err=%v", opening, newline, parsed, err)
			}
		}
	}
	withInnerFence := []byte("```json\n{\"messages\":[{\"room\":\"room-1\",\"body\":\"Example:\\n```go\\nfmt.Println(1)\\n```\"}]}\n```")
	parsed, err := Parse(withInnerFence, false)
	if err != nil || !strings.Contains(parsed.Messages[0].Body, "```go") {
		t.Fatalf("embedded fence parsed=%+v err=%v", parsed, err)
	}
	for _, raw := range []string{
		"Here is the answer:\n```json\n{\"messages\":[]}\n```",
		"```javascript\n{\"messages\":[]}\n```",
		"```json\n{\"messages\":[]}\n```\nextra",
		"```json\n{\"messages\":[]}\n```\n```",
		"```json\n{\"messages\":[]} trailing\n```",
	} {
		if _, err := Parse([]byte(raw), false); !errors.Is(err, ErrInvalid) {
			t.Fatalf("accepted malformed fence %q: %v", raw, err)
		}
	}
}

func TestParsePiGemmaRehearsalCandidate(t *testing.T) {
	raw, err := os.ReadFile("testdata/pi_gemma_rehearsal_fenced.txt")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(raw, false)
	if err != nil || len(parsed.Messages) != 1 || !strings.Contains(parsed.Messages[0].Body, "Status: live rehearsal complete.") {
		t.Fatalf("rehearsal candidate=%+v err=%v", parsed, err)
	}
}

func TestAttentionControls(t *testing.T) {
	good := []byte(`{"messages":[{"room":"room-1","body":"Choose","needs_attention":true,"open_questions":[{"question":"Scope?","options":["Small","Large"]}]}]}`)
	parsed, err := Parse(good, false)
	if err != nil || !parsed.Messages[0].NeedsAttention || len(parsed.Messages[0].OpenQuestions) != 1 {
		t.Fatalf("parsed=%+v err=%v", parsed, err)
	}
	for _, raw := range [][]byte{
		[]byte(`{"messages":[{"room":"r","body":"x","ask":{"kind":"question","text":"x"},"needs_attention":true}]}`),
		[]byte(`{"messages":[{"room":"r","body":"x","proposed_answers":["Only one"]}]}`),
		[]byte(`{"messages":[{"room":"r","body":"x","open_questions":[{"question":"x","options":["One"]}]}]}`),
		[]byte(`{"messages":[{"room":"r","body":"x","proposed_answers":["Yes","No"],"open_questions":[{"question":"x","options":["One","Two"]}]}]}`),
	} {
		if _, err := Parse(raw, false); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid control accepted: %s, err=%v", raw, err)
		}
	}
}

func TestAnswerSizeLimit(t *testing.T) {
	block := `{"type":"markdown","data":{"markdown":"` + strings.Repeat("x", 62000) + `"}}`
	raw := []byte(`{"messages":[{"room":"room","body":"plan","visual_plan":[` + strings.Repeat(block+",", 32) + block + `]}]}`)
	if len(raw) > MaxAnswerBytes {
		t.Fatalf("fixture exceeds limit: %d", len(raw))
	}
	raw = append(raw, bytes.Repeat([]byte{' '}, MaxAnswerBytes-len(raw))...)
	if _, err := Parse(raw, false); err != nil {
		t.Fatalf("answer at limit rejected: %v", err)
	}
	if _, err := Parse(append(raw, ' '), false); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "answer too large") {
		t.Fatalf("answer over limit error = %v", err)
	}
}

func TestVisualPlanPreservesUnknownShapesWithReceiptWarnings(t *testing.T) {
	raw := []byte(`{"messages":[{"room":"room","body":"plan","visual_plan":[{"type":"markdown","data":{"markdown":"safe"}},{"type":"future","data":{"payload":"<script>bad</script>"}},{"type":"checklist","data":{"other":1}},42],"appendix_pages":[{"title":"Full plan","body":"Details"}]}]}`)
	parsed, err := Parse(raw, false)
	if err != nil || len(parsed.Messages[0].VisualPlan) != 4 || len(parsed.Warnings) != 3 ||
		!strings.Contains(string(parsed.Messages[0].VisualPlan[1]), "bad") ||
		!strings.Contains(string(parsed.Messages[0].VisualPlan[1]), `"order":1`) ||
		!strings.Contains(string(parsed.Messages[0].VisualPlan[1]), `"id":"block-1"`) {
		t.Fatalf("parsed=%+v err=%v", parsed, err)
	}
	bad := []byte(`{"messages":[{"room":"room","body":"plan","visual_plan":[{"type":"markdown","data":{"markdown":"` + strings.Repeat("x", 66000) + `"}}]}]}`)
	if _, err := Parse(bad, false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unbounded plan accepted: %v", err)
	}
}

func TestVisualPlanReceiptWarnsForBrowserFallbackShapes(t *testing.T) {
	cases := []struct {
		kind, data string
		valid      bool
	}{
		{"markdown", `{"markdown":"ok"}`, true},
		{"markdown", `{"markdown":4,"text":"ok"}`, false},
		{"file-tree", `{"root":{"name":"src","children":[{"name":7}]}}`, false},
		{"data-model", `{"entities":[{"name":"User","fields":[{"type":8}]}]}`, false},
		{"api-endpoint", `{"method":7}`, false},
		{"annotated-code", `{"code":"x","annotations":[{"text":3}]}`, false},
		{"checklist", `{"items":[{"text":7}]}`, false},
		{"checklist", `{"items":[{"text":"done","checked":true}]}`, true},
		{"question-form", `{"questions":[{"question":"Q","options":[4]}]}`, false},
		{"canvas", `{"nodes":[{"x":"outside"}]}`, false},
		{"prototype", `{"screens":[{"elements":[{"target":4}]}]}`, false},
		{"diagram", `{"mermaid":"graph TD"}`, true},
	}
	for _, tc := range cases {
		if got := validVisualPlanData(tc.kind, []byte(tc.data)); got != tc.valid {
			t.Errorf("%s %s valid=%t, want %t", tc.kind, tc.data, got, tc.valid)
		}
	}
	raw := []byte(`{"messages":[{"room":"room","body":"checklist summary","visual_plan":[{"type":"checklist","data":{"items":[{"text":7}]}}]}]}`)
	parsed, err := Parse(raw, false)
	if err != nil || len(parsed.Warnings) != 1 || !strings.Contains(parsed.Warnings[0], "unknown shape") {
		t.Fatalf("receipt warnings=%+v err=%v", parsed.Warnings, err)
	}
}

// The live Haiku receipt for milo's first lead turn failed on a message-level
// "type" field. The full candidate was private, so this fixture preserves the
// observed extra field with an otherwise valid structured team question.
func TestParseHaikuQuestionMetadataWithWarning(t *testing.T) {
	raw, err := os.ReadFile("testdata/haiku_question_type.json")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(raw, false)
	if err != nil || len(parsed.Messages) != 1 || parsed.Messages[0].Ask == nil ||
		parsed.Messages[0].Ask.Kind != "question" || len(parsed.Warnings) != 1 ||
		!strings.Contains(parsed.Warnings[0], `"type"`) {
		t.Fatalf("parsed=%+v err=%v", parsed, err)
	}
}

func TestParseRejectsMalformedOrUnboundedCandidate(t *testing.T) {
	for _, raw := range []string{
		`{`, `{"messages":null}`, `{"messages":[]} trailing`,
		`{"messages":[],"extra":1}`,
		`{"messages":[{"room":"room","body":"ok","ask":{"kind":"question","text":"?","extra":1}}]}`,
		`{"messages":[{"room":"room","body":"ok","type":"question"}]}`,
		`{"messages":[{"room":"","body":"ok"}]}`,
		`{"messages":[{"room":"room","body":""}]}`,
		`{"messages":[{"room":"room","body":"ok"}],"reconciliation":{"status":"clean","findings":[]}}`,
		`{"messages":[{"room":"room","body":"ok","ask":{"kind":"bad","text":"?"}}]}`,
		`{"messages":[],"reconciliation":{"status":"bad","findings":[]}}`,
		`{"messages":[{"room":"room","body":"` + strings.Repeat("x", 32769) + `"}]}`,
	} {
		if _, err := Parse([]byte(raw), false); !errors.Is(err, ErrInvalid) {
			t.Fatalf("accepted invalid candidate %q: %v", raw[:min(len(raw), 100)], err)
		}
	}
	if _, err := Parse([]byte(`{"messages":[{"room":"room","body":"ok"}]}`), true); !errors.Is(err, ErrInvalid) {
		t.Fatalf("diagnostic room output accepted: %v", err)
	}
}
