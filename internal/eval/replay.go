package eval

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// StreamFor is the harness output of a replay or mock case for one kind, and the exit code the stand-in harness ends with. moduleRoot is
// the directory a recorded stream_file is relative to.
func (s Scenario) StreamFor(kind, moduleRoot string) ([]byte, int, error) {
	if s.Output == nil {
		return nil, 0, fmt.Errorf("%s: no output", s.ID)
	}
	switch {
	case s.Output.StreamFile != "":
		data, err := os.ReadFile(filepath.Join(moduleRoot, filepath.FromSlash(s.Output.StreamFile)))
		return data, 0, err
	case s.Output.Fault != "":
		stream, code := FaultStream(kind, s.Output.Fault)
		return stream, code, nil
	default:
		return Wrap(kind, s.Output.Candidate), 0, nil
	}
}

// Verdict compares what the adapter made of a stream with the scenario's expectation, and says what differs ("" when it matches).
func (s Scenario) Verdict(p Played) string {
	e := s.Expect
	if !p.HasAnswer {
		if e.Parse != ParseTerminal {
			return fmt.Sprintf("no final answer (exit %d, terminal=%t), want parse %s", p.Exit.ExitCode, p.Exit.TerminalEvent, e.Parse)
		}
		return ""
	}
	if e.Parse == ParseTerminal {
		return fmt.Sprintf("got a final answer %q, want a terminal exit", p.Answer)
	}
	if e.AnswerContains != "" && !strings.Contains(p.Answer, e.AnswerContains) {
		return fmt.Sprintf("the adapter cut out %q, which lacks %q", p.Answer, e.AnswerContains)
	}
	r := Score(p.Answer, s, nil)
	if !r.Valid {
		switch {
		case e.Parse != ParseRepairable:
			return fmt.Sprintf("rejected (%s: %s), want %s", r.Class, r.Err, e.Parse)
		case !strings.Contains(r.Err, e.ErrorContains):
			return fmt.Sprintf("rejected with %q, want an error holding %q", r.Err, e.ErrorContains)
		case r.Class != e.Class:
			return fmt.Sprintf("class %s, want %s (%s)", r.Class, e.Class, r.Err)
		}
		return ""
	}
	if e.Parse == ParseRepairable {
		return fmt.Sprintf("accepted (warnings %v), want a repairable rejection holding %q", r.Warnings, e.ErrorContains)
	}
	if e.Strict && !r.Strict {
		return fmt.Sprintf("accepted with warnings %v, want strict", r.Warnings)
	}
	for _, want := range e.WarningContains {
		found := false
		for _, w := range r.Warnings {
			found = found || strings.Contains(w, want)
		}
		if !found {
			return fmt.Sprintf("warnings %v lack %q", r.Warnings, want)
		}
	}
	if !r.Correct {
		return "checks failed: " + strings.Join(r.Failed, "; ")
	}
	return ""
}
