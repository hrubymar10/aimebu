package eval

import (
	"context"
	"testing"
)

const moduleRoot = "../.."

func loadCases(t *testing.T) []Scenario {
	t.Helper()
	cases, err := LoadDir("testdata/cases")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateAll(cases); err != nil {
		t.Fatal(err)
	}
	return cases
}

// TestCases plays every replay and mock case through the real adapter of each kind it names, and compares what comes out with the case's
// expectation. A recorded stream is a fixture of what a harness really printed; a mock case is a canned fault.
func TestCases(t *testing.T) {
	for _, s := range loadCases(t) {
		for _, kind := range s.Output.Kinds {
			t.Run(s.ID+"/"+kind, func(t *testing.T) {
				t.Parallel()
				stream, code, err := s.StreamFor(kind, moduleRoot)
				if err != nil {
					t.Fatal(err)
				}
				played, err := Play(context.Background(), kind, stream, code)
				if err != nil {
					t.Fatal(err)
				}
				if problem := s.Verdict(played); problem != "" {
					t.Fatalf("%s: %s", s.Title, problem)
				}
			})
		}
	}
}

func TestLiveScenariosAreValid(t *testing.T) {
	live, err := LoadDir("testdata/live")
	if err != nil {
		t.Fatal(err)
	}
	if len(live) < 20 {
		t.Fatalf("%d live scenarios, want about 22", len(live))
	}
	if err := ValidateAll(live); err != nil {
		t.Fatal(err)
	}
}
