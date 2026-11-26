package eval

import (
	"fmt"
	"strings"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/answer"
)

// Result is the score of one candidate answer.
type Result struct {
	// Valid says the answer parser accepts the candidate; Strict says it does so with no tolerance warning (a fence, a reasoning block, a
	// dropped field) and the text is the envelope itself, one JSON object with a messages field, which is what a stricter tool such as submit_answer would require.
	Valid    bool
	Strict   bool
	Warnings []string
	// Err is the parser's error when the candidate is not valid.
	Err string
	// Class is the error class of an invalid candidate, or of a valid one that fails a check (ClassBehaviour); "" when correct.
	Class string
	// Failed lists the checks the answer fails.
	Failed []string
	// Correct says the answer is valid and passes every check and the universal checks.
	Correct bool
	// Candidate is the parsed answer of a valid candidate.
	Candidate answer.Candidate
}

// Score judges a raw candidate against a scenario. tools names the tools the model called (live only).
func Score(raw string, s Scenario, tools []string) Result {
	candidate, err := answer.Parse([]byte(raw), false)
	if err != nil {
		return Result{Err: err.Error(), Class: Classify(err.Error())}
	}
	// A handoff without an outcome parses, and the server then holds it (internal/store/answers.go, handoffOutcomeRequired) for a turn
	// dispatched to a supervisor that reports supports_handoff_outcome, which every current supervisor does: the eval judges it the same way.
	for i, m := range candidate.Messages {
		if m.Handoff && m.Outcome == "" {
			msg := fmt.Sprintf("message %d is a handoff without an outcome; set outcome to %s", i, answer.OutcomeChoices)
			return Result{Err: msg, Class: ClassField}
		}
	}
	r := Result{Valid: true, Strict: len(candidate.Warnings) == 0 && isEnvelope(raw), Warnings: candidate.Warnings, Candidate: candidate}
	r.Failed = append(r.Failed, universal(s.Input, candidate)...)
	for _, check := range s.Expect.Checks {
		if problem := check.run(candidate, tools); problem != "" {
			r.Failed = append(r.Failed, problem)
		}
	}
	if len(r.Failed) > 0 {
		r.Class = ClassBehaviour
		return r
	}
	r.Correct = true
	return r
}

// Classify says what kind of fault a parser error is: the reply is not one JSON object (format), or it is one whose fields break the contract
// (field). It reads the error text of internal/answer, which pins those texts in its own tests.
func Classify(err string) string {
	for _, text := range []string{"text around the JSON", "text after the JSON", "is a JSON array", "not valid JSON", "invalid character", "unexpected end", "unexpected EOF", "invalid UTF-8", "answer too large"} {
		if strings.Contains(err, text) {
			return ClassFormat
		}
	}
	return ClassField
}

// isEnvelope says the candidate, as the model wrote it, is the envelope: one JSON object with a messages field. The parser also reads a lone
// message object and a bare array of message objects as the envelope they mean, without a warning (internal/answer/answer.go,
// tolerateAnswerShapes), so a candidate with no warning can still be one of those. A fence or a reasoning block is a warning of its own, so when
// this is asked of a candidate that has none, the text is the JSON itself.
func isEnvelope(raw string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(strings.TrimSpace(raw)), &fields) != nil {
		return false
	}
	_, ok := fields["messages"]
	return ok
}
