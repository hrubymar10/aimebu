package store

import (
	"context"
	"testing"
)

// v1's explicit inline-plans choice is carried into the global setting once: a v2 value already saved wins, and an unreadable v1 value
// changes nothing.
func TestV1ImportCarriesTheInlinePlansChoice(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		v1, saved       string
		want            string
		imported, there int
	}{
		"imported":        {v1: `"always"`, want: InlinePlanAlways, imported: 1},
		"already present": {v1: `"always"`, saved: InlinePlanOptional, want: InlinePlanOptional, there: 1},
		"invalid":         {v1: `"sometimes"`, want: InlinePlanOptional},
		"not a string":    {v1: `7`, want: InlinePlanOptional},
		"never chosen":    {want: InlinePlanOptional},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			source, mapping := v1Fixture(t)
			settings := `{}`
			if c.v1 != "" {
				settings = `{"inline_plan_appendix":` + c.v1 + `}`
			}
			if _, err := openV1Fixture(t, source).Exec(`INSERT OR REPLACE INTO settings(id,data) VALUES (1,?)`, settings); err != nil {
				t.Fatal(err)
			}
			s := newV1RoleStore(t)
			ctx := context.Background()
			if c.saved != "" {
				if err := s.SetInlinePlanAppendix(ctx, "usr_owner", c.saved); err != nil {
					t.Fatal(err)
				}
			}
			report, err := s.ImportV1(ctx, source, mapping, false)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := s.InlinePlanAppendix(ctx); err != nil || got != c.want {
				t.Errorf("setting = %q %v, want %q", got, err, c.want)
			}
			if report.Imported["inline_plan_appendix"] != c.imported || report.AlreadyPresent["inline_plan_appendix"] != c.there {
				t.Errorf("counts: imported %v, already present %v", report.Imported, report.AlreadyPresent)
			}
		})
	}
}
