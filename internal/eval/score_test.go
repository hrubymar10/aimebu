package eval

import (
	"strings"
	"testing"

	"github.com/goccy/go-json"
)

func answerOf(messages ...map[string]any) string {
	data, err := json.Marshal(map[string]any{"messages": messages})
	if err != nil {
		panic(err)
	}
	return string(data)
}

func msg(body string, extra ...any) map[string]any {
	m := map[string]any{"room": TaskRoom, "body": body}
	for i := 0; i+1 < len(extra); i += 2 {
		m[extra[i].(string)] = extra[i+1]
	}
	return m
}

// goodAnswers is, for every live scenario, an answer that passes it: the scenarios' checks are tested here against answers a person wrote, so a
// check that never fails (or always does) shows up without a model.
var goodAnswers = map[string]struct {
	good  string
	tools []string
}{
	"s01-plain-reply":        {answerOf(msg("It scores model answers against checks.", "reply_to", "7001")), nil},
	"s02-thread-reply":       {answerOf(msg("A is faster.", "reply_to", "7102")), nil},
	"s03-handoff-done":       {answerOf(msg("@"+DispatcherID+" build and vet pass.", "handoff", true, "outcome", "done")), nil},
	"s04-handoff-partial":    {answerOf(msg("A is done; B remains.", "handoff", true, "outcome", "partial")), nil},
	"s05-handoff-failed":     {answerOf(msg("The migration failed: permission denied.", "handoff", true, "outcome", "failed")), nil},
	"s06-handoff-unhinted":   {answerOf(msg("Done, tests pass.", "handoff", true, "outcome", "done")), nil},
	"s07-ask-question":       {answerOf(msg("Which storage?", "ask", map[string]any{"kind": "question", "text": "Which storage for the cache?", "options": []string{"sqlite", "files", "memory"}, "free_text": false})), nil},
	"s08-ask-approval":       {answerOf(msg("Approval needed.", "ask", map[string]any{"kind": "approval", "text": "Delete the old release branch?", "options": []string{"yes", "no"}, "free_text": false})), nil},
	"s09-proposed-answers":   {answerOf(msg("Which flag name?", "needs_attention", true, "proposed_answers", []string{"--fast", "--quick", "--turbo"})), nil},
	"s10-open-questions":     {answerOf(msg("Three decisions.", "needs_attention", true, "open_questions", []map[string]any{{"question": "Database?", "options": []string{"postgres", "mysql"}}, {"question": "Cache?", "options": []string{"redis", "memcached"}}, {"question": "Deploy?", "options": []string{"docker", "bare metal"}}})), nil},
	"s11-ask-trap":           {answerOf(msg("Proceed?", "ask", map[string]any{"kind": "question", "text": "Proceed with the deploy?", "options": []string{"yes", "no"}, "free_text": false})), nil},
	"s12-held-reply":         {answerOf(msg("Build is green, all tests pass.", "handoff", true, "outcome", "done")), nil},
	"s13-partial-hold":       {answerOf(msg("Summary: all three checks passed.")), nil},
	"s14-command-finished":   {answerOf(msg("The eval tests pass.", "handoff", true, "outcome", "done")), []string{"message_get"}},
	"s15-waiting-on-command": {`{"messages":[]}`, nil},
	"s16-mention-id":         {answerOf(msg("@" + OtherAgentID + " please review my change.")), nil},
	"s17-room-ref":           {answerOf(msg("It is in #" + HomeRoom + ".")), nil},
	"s18-fyi-silence":        {`{"messages":[]}`, nil},
	"s19-thanks":             {`{"messages":[]}`, []string{"react"}},
	"s20-no-tools":           {answerOf(msg("89 is prime; 91 is 7 times 13.")), nil},
	"s21-long-knowledge":     {answerOf(msg("The codeword is QUARTZ-17.")), nil},
	"s22-unchanged-marker":   {answerOf(msg("PONG")), nil},
}

func TestLiveScenariosPassTheirGoodAnswer(t *testing.T) {
	live, err := LoadDir("testdata/live")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range live {
		good, ok := goodAnswers[s.ID]
		if !ok {
			t.Errorf("%s has no good answer in goodAnswers", s.ID)
			continue
		}
		r := Score(good.good, s, good.tools)
		if !r.Correct || !r.Strict {
			t.Errorf("%s: good answer scored valid=%t strict=%t failed=%v err=%s", s.ID, r.Valid, r.Strict, r.Failed, r.Err)
		}
	}
	if len(goodAnswers) != len(live) {
		t.Errorf("%d good answers for %d scenarios", len(goodAnswers), len(live))
	}
}

// badAnswers are wrong in the way a model goes wrong: each must be scored with the class and the failed check named.
func TestLiveScenariosFailTheirTypicalMistake(t *testing.T) {
	live, err := LoadDir("testdata/live")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Scenario{}
	for _, s := range live {
		byID[s.ID] = s
	}
	bad := []struct {
		id, candidate string
		tools         []string
		class, holds  string
	}{
		{"s01-plain-reply", answerOf(msg("x", "reply_to", "7000")), nil, ClassBehaviour, "reply_to_is"},
		{"s01-plain-reply", answerOf(map[string]any{"room": HomeRoom, "body": "x"}), nil, ClassBehaviour, "not one of the context's rooms"},
		{"s02-thread-reply", answerOf(msg("A.", "reply_to", "7100")), nil, ClassBehaviour, "reply_to_is"},
		{"s03-handoff-done", answerOf(msg("@"+DispatcherID+" done", "handoff", true)), nil, ClassField, "handoff without an outcome"},
		{"s03-handoff-done", answerOf(msg("done", "handoff", true, "outcome", "done")), nil, ClassBehaviour, "mentions_id"},
		{"s03-handoff-done", answerOf(msg("@`"+DispatcherID+"` done", "handoff", true, "outcome", "done")), nil, ClassBehaviour, "mentions_id"},
		{"s04-handoff-partial", answerOf(msg("B", "handoff", true, "outcome", "done")), nil, ClassBehaviour, "handoff_outcome"},
		{"s07-ask-question", answerOf(msg("Which?", "ask", map[string]any{"kind": "question", "text": "Which?", "free_text": true})), nil, ClassBehaviour, "ask_options"},
		{"s07-ask-question", answerOf(msg("Which?", "needs_attention", true, "ask", map[string]any{"kind": "question", "text": "Which?", "options": []string{"a", "b"}})), nil, ClassBehaviour, "no_attention_controls"},
		{"s09-proposed-answers", answerOf(msg("Which?", "needs_attention", true, "proposed_answers", []string{"a"})), nil, ClassBehaviour, "proposed_answers"},
		{"s10-open-questions", answerOf(msg("Which?", "needs_attention", true, "open_questions", []map[string]any{{"question": "a", "options": []string{"x", "y"}}})), nil, ClassBehaviour, "open_questions"},
		{"s14-command-finished", answerOf(msg("ok", "handoff", true, "outcome", "done")), []string{"run_command"}, ClassBehaviour, "tool_not_called"},
		{"s15-waiting-on-command", answerOf(msg("still running")), nil, ClassBehaviour, "no_messages"},
		{"s16-mention-id", answerOf(msg("Please review, `@" + OtherAgentID + "`.")), nil, ClassBehaviour, "mentions_id"},
		{"s16-mention-id", answerOf(msg("@agt_01NOTINROSTER00000 please review @" + OtherAgentID)), nil, ClassBehaviour, "not in the roster"},
		{"s17-room-ref", answerOf(msg("It is in the home room.")), nil, ClassBehaviour, "body_contains"},
		{"s18-fyi-silence", answerOf(msg("Thanks for the update!")), nil, ClassBehaviour, "no_messages"},
		{"s19-thanks", `{"messages":[]}`, nil, ClassBehaviour, "react_if_silent"},
		{"s21-long-knowledge", answerOf(msg("I could not find it.")), nil, ClassBehaviour, "body_contains"},
		{"s01-plain-reply", "Sure, it scores answers.", nil, ClassFormat, "text around"},
		{"s01-plain-reply", `{"messages":[{"room":"` + TaskRoom + `","body":"x"}`, nil, ClassFormat, "not valid JSON"},
		{"s01-plain-reply", `{"messages":[{"room":"` + TaskRoom + `","body":"x"}],"status":"ok"}`, nil, ClassField, "unknown field"},
		{"s01-plain-reply", `{"messages":[{"room":"` + TaskRoom + `","body":5}]}`, nil, ClassField, "body must be text"},
	}
	for _, c := range bad {
		r := Score(c.candidate, byID[c.id], c.tools)
		text := r.Err + " " + strings.Join(r.Failed, " ")
		if r.Correct || r.Class != c.class || !strings.Contains(text, c.holds) {
			t.Errorf("%s %.60q: correct=%t class=%q (want %q), result %q lacks %q", c.id, c.candidate, r.Correct, r.Class, c.class, text, c.holds)
		}
	}
}

func TestAnAskWithAttentionControlsIsValidButNotStrict(t *testing.T) {
	live, _ := LoadDir("testdata/live")
	var s Scenario
	for _, c := range live {
		if c.ID == "s11-ask-trap" {
			s = c
		}
	}
	r := Score(answerOf(msg("Proceed?", "needs_attention", true, "ask", map[string]any{"kind": "question", "text": "Proceed?", "options": []string{"yes", "no"}})), s, nil)
	if !r.Valid || r.Strict || r.Correct || r.Class != ClassBehaviour {
		t.Fatalf("an ask with attention controls: valid=%t strict=%t correct=%t class=%s warnings=%v", r.Valid, r.Strict, r.Correct, r.Class, r.Warnings)
	}
}

func TestToleratedShapesAreValidButNotStrict(t *testing.T) {
	s := Scenario{ID: "x", Layer: LayerLive}
	body := `{"messages":[{"room":"` + TaskRoom + `","body":"hi"}]}`
	for name, raw := range map[string]string{"fence": "```json\n" + body + "\n```", "think": "<think>hmm</think>" + body, "lone object": `{"room":"` + TaskRoom + `","body":"hi"}`,
		"bare array": `[{"room":"` + TaskRoom + `","body":"hi"}]`, "fenced bare array": "```json\n[{\"room\":\"" + TaskRoom + "\",\"body\":\"hi\"}]\n```"} {
		if r := Score(raw, s, nil); !r.Valid || r.Strict {
			t.Errorf("%s: valid=%t strict=%t warnings=%v", name, r.Valid, r.Strict, r.Warnings)
		}
	}
	if r := Score(body, s, nil); !r.Valid || !r.Strict || !r.Correct {
		t.Errorf("canonical: %+v", r)
	}
}

func TestClassify(t *testing.T) {
	for err, want := range map[string]string{
		"invalid_answer: the reply has text around the JSON object; send exactly one":   ClassFormat,
		"invalid_answer: the reply is not valid JSON; send":                             ClassFormat,
		"invalid_answer: the reply is a JSON array; send":                               ClassFormat,
		"invalid_answer: the reply object has an unknown field status":                  ClassField,
		"invalid_answer: message 0 needs a room":                                        ClassField,
		"invalid_answer: message 0 has an invalid outcome; use done, partial or failed": ClassField,
	} {
		if got := Classify(err); got != want {
			t.Errorf("Classify(%q) = %s, want %s", err, got, want)
		}
	}
}
