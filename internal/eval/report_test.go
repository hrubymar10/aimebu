package eval

import (
	"math"
	"strings"
	"testing"
)

func TestWilson(t *testing.T) {
	low, high := Stat{K: 8, N: 10}.Wilson()
	if math.Abs(low-49.0) > 0.5 || math.Abs(high-94.3) > 0.5 {
		t.Fatalf("8/10: %.1f-%.1f, want about 49.0-94.3", low, high)
	}
	if low, high := (Stat{K: 0, N: 0}).Wilson(); low != 0 || high != 0 {
		t.Fatal("0/0 has no interval")
	}
	if got := (Stat{K: 60, N: 60}).String(); !strings.HasPrefix(got, "100% (60/60, 94-100)") {
		t.Fatalf("60/60: %s", got)
	}
}

func TestSummarizeSeparatesFirstAttemptFromRepairs(t *testing.T) {
	ok := Attempt{Valid: true, Strict: true, Correct: true, Millis: 1000, TokIn: 100, TokOut: 10, CostUSD: 0.01}
	fenced := Attempt{Valid: true, Strict: false, Correct: true, Millis: 2000, TokIn: 100, TokOut: 10}
	held := Attempt{Class: ClassField, Err: "message 0 is a handoff without an outcome", Millis: 3000, TokIn: 100, TokOut: 10}
	wrong := Attempt{Valid: true, Strict: true, Class: ClassBehaviour, Millis: 1500}
	crash := Attempt{Class: ClassHarness, Err: "exit 17"}
	cases := []Case{
		{Model: "m1", Scenario: "a", Attempts: []Attempt{ok}},
		{Model: "m1", Scenario: "a", Rep: 1, Attempts: []Attempt{fenced}},
		{Model: "m1", Scenario: "b", Attempts: []Attempt{held, ok}},      // fixed by a repair
		{Model: "m1", Scenario: "b", Rep: 1, Attempts: []Attempt{wrong}}, // valid but wrong: no repair can see it
		{Model: "m1", Scenario: "b", Rep: 2, Attempts: []Attempt{crash}},
	}
	got := Summarize(cases)
	if len(got) != 1 {
		t.Fatalf("%d summaries", len(got))
	}
	s := got[0]
	if s.Strict != (Stat{2, 4}) || s.Accepted != (Stat{3, 4}) || s.Correct != (Stat{2, 4}) || s.ValidAfter != (Stat{4, 4}) || s.CorrectAfter != (Stat{3, 4}) {
		t.Fatalf("rates strict=%v accepted=%v correct=%v validAfter=%v correctAfter=%v", s.Strict, s.Accepted, s.Correct, s.ValidAfter, s.CorrectAfter)
	}
	if s.Harness != 1 || s.Errors[ClassField] != 1 || s.Errors[ClassBehaviour] != 1 || s.MedianMillis != 1750 {
		t.Fatalf("harness=%d errors=%v median=%d", s.Harness, s.Errors, s.MedianMillis)
	}
	if s.TokensFirst != 330 || s.TokensAll != 440 {
		t.Fatalf("tokens first=%d all=%d", s.TokensFirst, s.TokensAll)
	}
}

func TestRender(t *testing.T) {
	cases := []Case{
		{Model: "claude-x", Scenario: "s01", Attempts: []Attempt{{Valid: true, Strict: true, Correct: true, Millis: 1000}}},
		{Model: "glm-y", Scenario: "s01", Attempts: []Attempt{{Class: ClassFormat, Err: "the reply has text around the JSON object"}, {Valid: true, Correct: true}}},
		{Model: "glm-y", Scenario: "s01", Rep: 1, Attempts: []Attempt{{Class: ClassFormat, Err: "the reply has text around the JSON object"}}},
	}
	text := Render("Eval", cases, []Scenario{{ID: "s01"}, {ID: "s02"}})
	for _, want := range []string{"# Eval", "| claude-x | 100% (1/1", "| glm-y | 0% (0/2", "| glm-y | 2 | 0 | 0 |", "| s01 | 1/1 | 0/2 |", "| s02 | - | - |", "1. 2 x format: the reply has text around the JSON object"} {
		if !strings.Contains(text, want) {
			t.Errorf("report lacks %q:\n%s", want, text)
		}
	}
}

func TestRenderTokenColumnExplainsCacheShare(t *testing.T) {
	a := Attempt{Valid: true, TokIn: 106, TokOut: 3, TokFresh: 12, TokRead: 90, TokWrite: 4, HasCacheWrite: true}
	text := Render("Eval", []Case{{Model: "m", Scenario: "s", Attempts: []Attempt{a}}}, nil)
	if !strings.Contains(text, `<span title="Input: 106 in (12 uncached, 90 cache read, 4 cache write); output: 3">109</span>`) {
		t.Fatalf("token column has no input breakdown:\n%s", text)
	}
}

func TestMedianOfAnEvenCountIsTheMeanOfTheTwoMiddleValues(t *testing.T) {
	ok := func(ms int64) Attempt { return Attempt{Valid: true, Strict: true, Correct: true, Millis: ms} }
	got := Summarize([]Case{{Model: "m", Scenario: "a", Attempts: []Attempt{ok(1000)}}, {Model: "m", Scenario: "a", Rep: 1, Attempts: []Attempt{ok(100000)}}})
	if got[0].MedianMillis != 50500 {
		t.Fatalf("median of 1000 and 100000 ms is %d ms, want 50500", got[0].MedianMillis)
	}
	odd := Summarize([]Case{{Model: "m", Scenario: "a", Attempts: []Attempt{ok(3000)}}, {Model: "m", Scenario: "a", Rep: 1, Attempts: []Attempt{ok(1000)}}, {Model: "m", Scenario: "a", Rep: 2, Attempts: []Attempt{ok(2000)}}})
	if odd[0].MedianMillis != 2000 {
		t.Fatalf("odd median %d", odd[0].MedianMillis)
	}
	if text := Render("t", []Case{{Model: "m", Scenario: "a", Attempts: []Attempt{ok(1000)}}, {Model: "m", Scenario: "a", Rep: 1, Attempts: []Attempt{ok(100000)}}}, nil); !strings.Contains(text, "50.5 s") {
		t.Fatalf("the report shows the median:\n%s", text)
	}
}

func TestReportShowsHowTheAnswerArrived(t *testing.T) {
	cases := []Case{
		{Model: "m1", Scenario: "a", Attempts: []Attempt{{Valid: true, Correct: true, Via: ViaTool}}},
		{Model: "m1", Scenario: "a", Rep: 1, Attempts: []Attempt{{Valid: true, Correct: true, Via: ViaTool}}},
		{Model: "m1", Scenario: "a", Rep: 2, Attempts: []Attempt{{Valid: true, Correct: true, Via: ViaText}}},
		{Model: "m2", Scenario: "a", Attempts: []Attempt{{Valid: true, Correct: true, Via: ViaText}}},
		{Model: "m2", Scenario: "a", Rep: 1, Attempts: []Attempt{{Class: ClassHarness, Err: "exit 1"}}},
	}
	out := Render("t", cases, nil)
	for _, want := range []string{"## How the first answer was submitted", "| m1 | 2 | 1 |", "| m2 | 0 | 1 |"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}
