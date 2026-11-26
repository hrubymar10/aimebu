package store

import (
	"os"
	"sort"
	"testing"

	"github.com/goccy/go-json"
)

// fenceCase is one row of testdata/fence_cases.json, the table the web renderer's test (scripts/test-markdown.cjs) reads too: the
// tokens in fenced is code on screen and must not be live prose here, the tokens in live must be.
type fenceCase struct {
	Name   string   `json:"name"`
	Body   string   `json:"body"`
	Fenced []string `json:"fenced"`
	Live   []string `json:"live"`
	// KnownDivergence marks a case where the scanner and the renderer still differ; the case is recorded as data and skipped, with
	// this text as the reason.
	KnownDivergence string `json:"known_divergence"`
}

func TestFencesUnderListItemsAgreeWithTheRenderer(t *testing.T) {
	data, err := os.ReadFile("testdata/fence_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []fenceCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("no cases")
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			if tc.KnownDivergence != "" {
				t.Skip("known divergence: " + tc.KnownDivergence)
			}
			var got []string
			for _, span := range proseMentionSpans(tc.Body) {
				got = append(got, "@"+span.Token)
			}
			for _, span := range proseRoomTitleSpans(tc.Body) {
				got = append(got, "#"+span.Token)
			}
			want := append([]string(nil), tc.Live...)
			sort.Strings(got)
			sort.Strings(want)
			if len(got) != len(want) {
				t.Fatalf("live tokens = %v, want %v", got, want)
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("live tokens = %v, want %v", got, want)
				}
			}
		})
	}
}
