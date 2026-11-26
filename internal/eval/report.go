package eval

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Attempt is one turn of a live case: the first try, or a repair after the failure was sent back to the agent.
type Attempt struct {
	Valid   bool   `json:"valid"`
	Strict  bool   `json:"strict"`
	Correct bool   `json:"correct"`
	Class   string `json:"class,omitempty"`
	Err     string `json:"err,omitempty"`
	// TokIn is total prompt input, including cache reads and writes.
	TokIn         int64   `json:"tokens_in,omitempty"`
	TokOut        int64   `json:"tokens_out,omitempty"`
	TokFresh      int64   `json:"tokens_uncached,omitempty"`
	TokRead       int64   `json:"tokens_cache_read,omitempty"`
	TokWrite      int64   `json:"tokens_cache_write,omitempty"`
	HasCacheWrite bool    `json:"has_cache_write,omitempty"`
	CostUSD       float64 `json:"cost_usd,omitempty"`
	Millis        int64   `json:"millis,omitempty"`
	// Candidate is the raw answer the harness reported and Tools the tools the model called, kept so a run can be read afterwards.
	Candidate string   `json:"candidate,omitempty"`
	Tools     []string `json:"tools,omitempty"`
}

// Case is one repetition of one scenario on one model. Attempts[0] is the first try. A harness failure (the CLI crashed, timed out or was
// limited) has Class harness on its attempt and is left out of the rates.
type Case struct {
	Model    string    `json:"model"`
	Scenario string    `json:"scenario"`
	Rep      int       `json:"rep"`
	Attempts []Attempt `json:"attempts"`
}

func (c Case) first() Attempt { return c.Attempts[0] }

// afterRepairs is the last attempt: what the agent ended with after every repair.
func (c Case) last() Attempt { return c.Attempts[len(c.Attempts)-1] }

// Stat is a rate with its 95% Wilson interval.
type Stat struct{ K, N int }

func (s Stat) Pct() float64 {
	if s.N == 0 {
		return 0
	}
	return 100 * float64(s.K) / float64(s.N)
}

// Wilson is the 95% Wilson score interval of the rate, in percent.
func (s Stat) Wilson() (low, high float64) {
	if s.N == 0 {
		return 0, 0
	}
	const z = 1.96
	n, p := float64(s.N), float64(s.K)/float64(s.N)
	denom := 1 + z*z/n
	centre := (p + z*z/(2*n)) / denom
	margin := z * math.Sqrt(p*(1-p)/n+z*z/(4*n*n)) / denom
	return 100 * math.Max(0, centre-margin), 100 * math.Min(1, centre+margin)
}

func (s Stat) String() string {
	if s.N == 0 {
		return "n/a"
	}
	low, high := s.Wilson()
	return fmt.Sprintf("%.0f%% (%d/%d, %.0f-%.0f)", s.Pct(), s.K, s.N, low, high)
}

// ModelSummary is the headline numbers of one model. Cases with a harness failure on their first attempt are counted in Harness and in no rate.
type ModelSummary struct {
	Model string
	// Strict, Accepted and Correct are measured on the first attempt: valid with no tolerance warning, valid, and valid with every check passed.
	Strict, Accepted, Correct Stat
	// ValidAfter and CorrectAfter are measured on the last attempt, after the repairs.
	ValidAfter, CorrectAfter Stat
	// Errors counts the first attempts that were not correct, by class.
	Errors  map[string]int
	Harness int
	// MedianMillis is the median first-attempt time; TokensFirst and TokensAll are the tokens of the first attempts and of every attempt.
	MedianMillis   int64
	TokensFirst    int64
	TokensAll      int64
	FirstBreakdown TokenBreakdown
	AllBreakdown   TokenBreakdown
	CostFirst      float64
	CostAll        float64
}

// TokenBreakdown keeps the input categories behind a report's total token column.
type TokenBreakdown struct {
	Fresh, Read, Write, Out int64
	HasWrite                bool
}

func (b *TokenBreakdown) add(a Attempt) {
	fresh := a.TokFresh
	if fresh == 0 && a.TokRead == 0 && a.TokWrite == 0 && a.TokIn > 0 {
		fresh = a.TokIn
	}
	b.Fresh += fresh
	b.Read += a.TokRead
	b.Write += a.TokWrite
	b.Out += a.TokOut
	b.HasWrite = b.HasWrite || a.HasCacheWrite
}

func (b TokenBreakdown) title() string {
	value := fmt.Sprintf("Input: %d in (%d uncached, %d cache read", b.Fresh+b.Read+b.Write, b.Fresh, b.Read)
	if b.HasWrite {
		value += fmt.Sprintf(", %d cache write", b.Write)
	}
	return value + fmt.Sprintf("); output: %d", b.Out)
}

// Summarize groups cases by model, in the order the models first appear.
func Summarize(cases []Case) []ModelSummary {
	var order []string
	by := map[string][]Case{}
	for _, c := range cases {
		if len(c.Attempts) == 0 {
			continue
		}
		if _, ok := by[c.Model]; !ok {
			order = append(order, c.Model)
		}
		by[c.Model] = append(by[c.Model], c)
	}
	var out []ModelSummary
	for _, model := range order {
		s := ModelSummary{Model: model, Errors: map[string]int{}}
		var times []int64
		for _, c := range by[model] {
			f, l := c.first(), c.last()
			for _, a := range c.Attempts {
				s.TokensAll += a.TokIn + a.TokOut
				s.AllBreakdown.add(a)
				s.CostAll += a.CostUSD
			}
			s.TokensFirst += f.TokIn + f.TokOut
			s.FirstBreakdown.add(f)
			s.CostFirst += f.CostUSD
			if f.Class == ClassHarness {
				s.Harness++
				continue
			}
			times = append(times, f.Millis)
			s.Strict.N++
			s.Accepted.N++
			s.Correct.N++
			s.ValidAfter.N++
			s.CorrectAfter.N++
			if f.Valid && f.Strict {
				s.Strict.K++
			}
			if f.Valid {
				s.Accepted.K++
			}
			if f.Correct {
				s.Correct.K++
			} else {
				s.Errors[f.Class]++
			}
			if l.Valid {
				s.ValidAfter.K++
			}
			if l.Correct {
				s.CorrectAfter.K++
			}
		}
		sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
		if len(times) > 0 {
			mid := len(times) / 2
			s.MedianMillis = times[mid]
			if len(times)%2 == 0 {
				s.MedianMillis = (times[mid-1] + times[mid]) / 2
			}
		}
		out = append(out, s)
	}
	return out
}

// Render is the markdown report of a run: headline rates per model (first attempt against after repairs), the error classes, the model by
// scenario matrix, and the most common errors.
func Render(title string, cases []Case, scenarios []Scenario) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", title)
	summaries := Summarize(cases)
	b.WriteString("## Headline: first attempt against after repairs\n\n")
	b.WriteString("Rates are over cases the harness completed, with the 95% Wilson interval in percent. \"Strict\" is valid with no tolerance warning; \"accepted\" is what the server takes today; \"correct\" also passes the scenario checks. \"After repairs\" is the last attempt.\n\n")
	b.WriteString("| model | strict first try | accepted first try | correct first try | valid after repairs | correct after repairs | harness failures |\n|---|---|---|---|---|---|---|\n")
	for _, s := range summaries {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %d |\n", s.Model, s.Strict, s.Accepted, s.Correct, s.ValidAfter, s.CorrectAfter, s.Harness)
	}
	b.WriteString("\n## First-attempt errors by class\n\n| model | format | field | behaviour |\n|---|---|---|---|\n")
	for _, s := range summaries {
		fmt.Fprintf(&b, "| %s | %d | %d | %d |\n", s.Model, s.Errors[ClassFormat], s.Errors[ClassField], s.Errors[ClassBehaviour])
	}
	b.WriteString("\n## Time and tokens\n\n| model | median first-attempt time | tokens (first attempts) | tokens (all attempts) | cost USD (first) | cost USD (all) |\n|---|---|---|---|---|---|\n")
	for _, s := range summaries {
		fmt.Fprintf(&b, "| %s | %.1f s | <span title=\"%s\">%d</span> | <span title=\"%s\">%d</span> | %.2f | %.2f |\n", s.Model, float64(s.MedianMillis)/1000, s.FirstBreakdown.title(), s.TokensFirst, s.AllBreakdown.title(), s.TokensAll, s.CostFirst, s.CostAll)
	}
	b.WriteString("\n## Correct on the first try, by scenario (passes/repetitions)\n\n| scenario |")
	for _, s := range summaries {
		fmt.Fprintf(&b, " %s |", s.Model)
	}
	b.WriteString("\n|---|")
	for range summaries {
		b.WriteString("---|")
	}
	b.WriteString("\n")
	type key struct{ model, scenario string }
	pass, runs := map[key]int{}, map[key]int{}
	for _, c := range cases {
		if len(c.Attempts) == 0 || c.first().Class == ClassHarness {
			continue
		}
		runs[key{c.Model, c.Scenario}]++
		if c.first().Correct {
			pass[key{c.Model, c.Scenario}]++
		}
	}
	for _, sc := range scenarios {
		fmt.Fprintf(&b, "| %s |", sc.ID)
		for _, s := range summaries {
			if n := runs[key{s.Model, sc.ID}]; n == 0 {
				b.WriteString(" - |")
			} else {
				fmt.Fprintf(&b, " %d/%d |", pass[key{s.Model, sc.ID}], n)
			}
		}
		b.WriteString("\n")
	}
	counts := map[string]int{}
	for _, c := range cases {
		if len(c.Attempts) > 0 && c.first().Err != "" && c.first().Class != ClassHarness {
			counts[c.first().Class+": "+c.first().Err]++
		}
	}
	type kv struct {
		text string
		n    int
	}
	var errs []kv
	for text, n := range counts {
		errs = append(errs, kv{text, n})
	}
	sort.Slice(errs, func(i, j int) bool {
		if errs[i].n != errs[j].n {
			return errs[i].n > errs[j].n
		}
		return errs[i].text < errs[j].text
	})
	b.WriteString("\n## Most common first-attempt errors\n\n")
	if len(errs) == 0 {
		b.WriteString("None.\n")
	}
	for i, e := range errs {
		if i == 10 {
			break
		}
		fmt.Fprintf(&b, "%d. %d x %s\n", i+1, e.n, e.text)
	}
	return b.String()
}
