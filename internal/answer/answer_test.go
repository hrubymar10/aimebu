package answer

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
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
	parsed, err := Parse([]byte(`{"messages":[{"room":"room-1","body":"x","reply_to":36226},{"room":"room-1","body":"y","reply_to":"msg_1"}]}`), false)
	if err != nil || parsed.Messages[0].ReplyTo != "36226" || parsed.Messages[1].ReplyTo != "msg_1" {
		t.Fatalf("number reply_to: %+v %v", parsed, err)
	}
	for _, bad := range []string{`"reply_to":0`, `"reply_to":-3`, `"reply_to":1.5`, `"reply_to":true`, `"reply_to":[1]`} {
		if _, err := Parse([]byte(`{"messages":[{"room":"room-1","body":"x",`+bad+`}]}`), false); err == nil {
			t.Fatalf("%s was accepted", bad)
		}
	}
}

// The conversation follows reply_to, so a thread_root (a stale prompt, a turn already under way) is dropped from the answer: silently when it is
// null or empty, with a note when it names something, and the message still parses with its reply_to.
func TestParseIgnoresAStaleThreadRoot(t *testing.T) {
	parsed, err := Parse([]byte(`{"messages":[{"room":"room-1","body":"x","reply_to":"msg_1","thread_root":12},{"room":"room-1","body":"y","thread_root":null},{"room":"room-1","body":"z","thread_root":""}]}`), false)
	if err != nil || len(parsed.Messages) != 3 || parsed.Messages[0].ReplyTo != "msg_1" {
		t.Fatalf("a stale thread_root: %+v %v", parsed, err)
	}
	if len(parsed.Warnings) != 1 || !strings.Contains(parsed.Warnings[0], "message 0 ignored thread_root") {
		t.Fatalf("warnings = %q, want one for the message that named a thread_root", parsed.Warnings)
	}
	quiet, err := Parse([]byte(`{"messages":[{"room":"room-1","body":"y","thread_root":null}]}`), false)
	if err != nil || len(quiet.Warnings) != 0 || len(quiet.Messages) != 1 {
		t.Fatalf("a null thread_root is dropped silently: %+v %v", quiet, err)
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
		"Here is the answer:\n```json\n{\"messages\":[{\"room\":\"room-1\",\"body\":\"b\"}]}\n```",
		"```json\n{\"messages\":[{\"room\":\"room-1\",\"body\":\"b\"}]}\n```\nextra",
		"```javascript\n{\"messages\":[{\"room\":\"room-1\",\"body\":\"b\"}]}\n```",
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

func TestParseStripsLeadingReasoningBlocks(t *testing.T) {
	raw, err := os.ReadFile("testdata/opencode_glm_think_prefixed.txt")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(raw, false)
	if err != nil || parsed.Messages == nil || len(parsed.Messages) != 0 || len(parsed.Warnings) != 1 {
		t.Fatalf("think-prefixed candidate=%+v err=%v", parsed, err)
	}
	const body = `{"messages":[{"room":"room-1","body":"done <not reasoning>"}]}`
	for name, text := range map[string]string{
		"any element name":     "<reasoning>step one</reasoning>" + body,
		"hyphenated name":      "<scratch-pad>x</scratch-pad>\n" + body,
		"several blocks":       " <think>a</think>\n<analysis>b</analysis>\n" + body,
		"newlines in block":    "<think>\nfirst\nsecond\n</think>\n\n" + body,
		"before a JSON fence":  "<think>x</think>\n```json\n" + body + "\n```",
		"markup inside answer": "<think>x</think>" + body,
	} {
		parsed, err := Parse([]byte(text), false)
		if err != nil || len(parsed.Messages) != 1 || parsed.Messages[0].Body != "done <not reasoning>" {
			t.Fatalf("%s: parsed=%+v err=%v", name, parsed, err)
		}
	}
	for name, text := range map[string]string{
		"never closed":          "<think>no end" + body,
		"attributes":            `<think level="high">x</think>` + body,
		"prose before":          "Answer: <think>x</think>" + body,
		"text after the answer": "<think>x</think>" + body + "<think>y</think>",
		"closing tag only":      "</think>" + body,
		"reasoning only":        "<think>x</think>",
		"unrelated markup":      "<b>bold</i>" + body,
	} {
		if _, err := Parse([]byte(text), false); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: accepted: %v", name, err)
		}
	}
}

// Many tiny blocks and a long run of trailing spaces, still under the size limit: the stripping must not rescan the tail once per block.
func TestStripLeadingReasoningManyBlocks(t *testing.T) {
	const body = `{"messages":[{"room":"room-1","body":"done"}]}`
	raw := []byte(strings.Repeat("<a></a>", 120_000) + body + strings.Repeat(" ", 1_000_000))
	if len(raw) >= MaxAnswerBytes {
		t.Fatalf("the input is %d bytes, over the answer limit", len(raw))
	}
	parsed, err := Parse(raw, false)
	if err != nil || len(parsed.Messages) != 1 || parsed.Messages[0].Room != "room-1" {
		t.Fatalf("parsed=%+v err=%v", parsed, err)
	}
}

// Comparing inputs in one run catches repeated rescans without depending on
// the absolute speed of the test machine. A quadratic scan grows about 64x
// when the input grows 8x; normal parsing has ample room below 30x.
func TestStripLeadingReasoningScalesWithInput(t *testing.T) {
	input := func(blocks int) []byte {
		const body = `{"messages":[{"room":"room-1","body":"done"}]}`
		return []byte(strings.Repeat("<a></a>", blocks) + body + strings.Repeat(" ", blocks*5))
	}
	measure := func(raw []byte) time.Duration {
		t.Helper()
		best := time.Duration(1<<63 - 1)
		for range 3 {
			start := time.Now()
			parsed, err := Parse(raw, false)
			elapsed := time.Since(start)
			if err != nil || len(parsed.Messages) != 1 {
				t.Fatalf("parsed=%+v err=%v", parsed, err)
			}
			if elapsed < best {
				best = elapsed
			}
		}
		return best
	}
	small := input(8_000)
	large := input(64_000)
	if len(large) >= MaxAnswerBytes {
		t.Fatal("large input exceeds answer limit")
	}
	measure(small) // warm up decoder and allocations before comparing runs
	smallCost := measure(small)
	largeCost := measure(large)
	if largeCost > 30*smallCost {
		t.Fatalf("8x input took more than 30x as long: %s versus %s", largeCost, smallCost)
	}
}

// Report the absolute cost alongside the relative scaling test.
func BenchmarkStripLeadingReasoningManyBlocks(b *testing.B) {
	const body = `{"messages":[{"room":"room-1","body":"done"}]}`
	raw := []byte(strings.Repeat("<a></a>", 120_000) + body + strings.Repeat(" ", 1_000_000))
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Parse(raw, false); err != nil {
			b.Fatal(err)
		}
	}
}

func TestAskDropsAttentionControlsWithAWarning(t *testing.T) {
	raw := `{"messages":[{"room":"r","body":"b","ask":{"kind":"question","text":"?"},"needs_attention":true,"proposed_answers":["a","b"],"open_questions":[{"question":"q","options":["a","b"]}]}]}`
	got, err := Parse([]byte(raw), false)
	if err != nil {
		t.Fatal(err)
	}
	m := got.Messages[0]
	if m.NeedsAttention || m.ProposedAnswers != nil || m.OpenQuestions != nil || m.Ask == nil || len(got.Warnings) != 1 {
		t.Fatalf("message=%+v warnings=%v", m, got.Warnings)
	}
}

// The shape faults the strict decoder would only report as raw JSON errors come back as fixed, room-readable reasons: they name what was
// wrong and how to fix it, and never quote what the agent wrote.
func TestParseNamesTheExactShapeFault(t *testing.T) {
	for _, c := range []struct {
		name, raw, want string
	}{
		{"text before the object", `Clearly done. {"messages":[{"room":"r","body":"b"}]}`, "text around the JSON object"},
		{"text after the object", `{"messages":[{"room":"r","body":"b"}]} hope that helps`, "after the JSON object"},
		{"an array of numbers", `[1,2]`, "the reply is a JSON array"},
		{"unknown top-level field", `{"messages":[],"source":"memory"}`, "unknown field source"},
		{"unknown field with odd characters", `{"messages":[],"a b":1}`, "unknown field a?b"},
		{"messages missing", `{}`, "needs a messages array"},
		{"messages not an array", `{"messages":{"room":"r"}}`, "messages array"},
		{"bool as text", `{"messages":[{"room":"r","body":"b","handoff":"yes"}]}`, "handoff must be true or false"},
		{"text as bool", `{"messages":[{"room":"r","body":"b","outcome":true}]}`, "outcome must be text"},
		{"array as text", `{"messages":[{"room":"r","body":"b","attachments":"att"}]}`, "attachments must be an array"},
		{"reply_to object", `{"messages":[{"room":"r","body":"b","reply_to":{"no":1}}]}`, "reply_to must be a message number"},
		{"ask as text", `{"messages":[{"room":"r","body":"b","ask":"question"}]}`, "ask must be an object"},
		{"ask kind as number", `{"messages":[{"room":"r","body":"b","ask":{"kind":3,"text":"t","options":["a","b"]}}]}`, "ask kind must be text"},
	} {
		_, err := Parse([]byte(c.raw), false)
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), c.want) || strings.ContainsAny(err.Error(), `"'`) {
			t.Fatalf("%s: err=%v, want it to name %q without quoting anything", c.name, err, c.want)
		}
	}
	if _, err := Parse([]byte(`{"messages":[{"room":"r","body":"b"}]}`), false); err != nil {
		t.Fatalf("a well-shaped answer was refused: %v", err)
	}
}

// A12-9: the parser reads three near-miss reply shapes as the envelope they mean: a lone message object and a bare array of message objects
// become {"messages":[...]}, and prose around an otherwise empty {"messages":[]} counts as the empty answer itself, so it neither holds the
// reply nor raises a recovery. Prose around a non-empty answer still holds, with the exact reason.
func TestParseToleratesNearMissAnswerShapes(t *testing.T) {
	lone, err := Parse([]byte(`{"room":"room-1","body":"answer","reply_to":"msg-1"}`), false)
	if err != nil || len(lone.Messages) != 1 || lone.Messages[0].Body != "answer" || lone.Messages[0].ReplyTo != "msg-1" || len(lone.Warnings) != 0 {
		t.Fatalf("lone object: %+v %v", lone, err)
	}
	if fenced, err := Parse([]byte("```json\n{\"room\":\"room-1\",\"body\":\"fenced\"}\n```"), false); err != nil ||
		len(fenced.Messages) != 1 || fenced.Messages[0].Body != "fenced" || len(fenced.Warnings) != 1 {
		t.Fatalf("fenced lone object: %+v %v", fenced, err)
	}
	array, err := Parse([]byte(`[{"room":"room-1","body":"a"},{"room":"room-1","body":"b","handoff":true,"outcome":"done"}]`), false)
	if err != nil || len(array.Messages) != 2 || array.Messages[1].Outcome != "done" || len(array.Warnings) != 0 {
		t.Fatalf("bare array: %+v %v", array, err)
	}
	if empty, err := Parse([]byte(`[]`), false); err != nil || len(empty.Messages) != 0 {
		t.Fatalf("bare empty array: %+v %v", empty, err)
	}
	// A lone object keeps the message-level tolerances: unknown metadata is dropped with its warning and a numeric reply_to is read as text.
	numbered, err := Parse([]byte(`{"room":"room-1","body":"answer","reply_to":36226,"thread_root":"msg-9"}`), false)
	if err != nil || numbered.Messages[0].ReplyTo != "36226" || len(numbered.Warnings) != 1 || !strings.Contains(numbered.Warnings[0], "thread_root") {
		t.Fatalf("lone object tolerances: %+v %v", numbered, err)
	}
	for name, raw := range map[string]string{
		"prose before":                               `Clearly done. {"messages":[]}`,
		"prose both sides":                           `Note: {"messages":[]} that is all`,
		"prose around a fenced empty answer":         "Here is the answer:\n```json\n{\"messages\":[]}\n```",
		"a mislabelled fence around an empty answer": "```javascript\n{\"messages\":[]}\n```",
	} {
		parsed, err := Parse([]byte(raw), false)
		if err != nil || len(parsed.Messages) != 0 || len(parsed.Warnings) != 1 || !strings.Contains(parsed.Warnings[0], "prose around an empty answer") {
			t.Fatalf("%s: parsed=%+v warnings=%q err=%v", name, parsed, parsed.Warnings, err)
		}
	}
	if _, err := Parse([]byte(`Clearly done. {"messages":[{"room":"r","body":"b"}]}`), false); !errors.Is(err, ErrInvalid) ||
		!strings.Contains(err.Error(), "text around the JSON object") {
		t.Fatalf("prose around a non-empty answer: err=%v", err)
	}
	for _, raw := range []string{
		`[1,2]`,
		`["x"]`,
		`[{"room":"r","body":"b"},5]`,
		`[{"payload":1}]`,
	} {
		if _, err := Parse([]byte(raw), false); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "the reply is a JSON array") {
			t.Fatalf("non-message array %s accepted: %v", raw, err)
		}
	}
	// Shapes that only look near-miss keep their exact faults.
	for _, raw := range []string{
		`{}`,
		`{"messages":[]} trailing`,
		`{"source":"memory"}`,
	} {
		if _, err := Parse([]byte(raw), false); !errors.Is(err, ErrInvalid) {
			t.Fatalf("accepted %s: %v", raw, err)
		}
	}
}
