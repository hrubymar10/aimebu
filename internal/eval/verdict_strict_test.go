package eval

import "testing"

func TestVerdictCanRequireAnAnswerThatIsNotStrict(t *testing.T) {
	no := false
	s := Scenario{ID: "x", Layer: LayerMock, Output: &Output{Kinds: []string{KindClaude}, Candidate: "x"}, Expect: Expect{Parse: ParseValid, Strict: &no}}
	lone := Played{HasAnswer: true, Answer: `{"room":"` + TaskRoom + `","body":"hi"}`}
	if problem := s.Verdict(lone); problem != "" {
		t.Fatalf("a lone message object is accepted and not strict, as the case says: %s", problem)
	}
	canonical := Played{HasAnswer: true, Answer: `{"messages":[{"room":"` + TaskRoom + `","body":"hi"}]}`}
	if problem := s.Verdict(canonical); problem == "" {
		t.Fatal("an answer that is strict fails a case that says it is not")
	}
	yes := true
	s.Expect.Strict = &yes
	if problem := s.Verdict(lone); problem == "" {
		t.Fatal("a lone object fails a case that requires strict")
	}
}
