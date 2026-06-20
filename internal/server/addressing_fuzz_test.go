package server

import (
	"strings"
	"testing"
)

func FuzzParseAddressedTo(f *testing.F) {
	f.Add("@alice please review")
	f.Add("@alice@project check this")
	f.Add("```go\n@not-an-address\n```\nnow @real")
	f.Add("\\@escaped and @real")
	f.Add("@channel @here @all @everyone @humans @ais")
	f.Add("@alice-")
	f.Add("@alice_ @bob__")
	f.Add("")
	f.Add("no mentions at all")
	f.Add("`@inline` @outside")

	f.Fuzz(func(t *testing.T, body string) {
		results := parseAddressedTo(body)

		seen := make(map[string]bool, len(results))
		for _, r := range results {
			if strings.ToLower(r) != r {
				t.Fatalf("parseAddressedTo(%q) returned non-lowercase token %q", body, r)
			}
			if seen[r] {
				t.Fatalf("parseAddressedTo(%q) returned duplicate token %q", body, r)
			}
			seen[r] = true
		}
	})
}

func FuzzMaskCodeForAddressing(f *testing.F) {
	f.Add("hello @alice")
	f.Add("```go\n@notanaddress\n```\n@real")
	f.Add("    indented @code\n\nnot @code")
	f.Add("`inline @code` @real")
	f.Add("~~~\n@fenced\n~~~\n@prose")
	f.Add("~~~go\ncontent\n~~~")
	f.Add("")
	f.Add("no code here at all")
	f.Add("```\n```")

	f.Fuzz(func(t *testing.T, body string) {
		view := maskCodeForAddressing(body)

		// Masked output must preserve newline count.
		inNewlines := strings.Count(body, "\n")
		outNewlines := strings.Count(view.masked, "\n")
		if inNewlines != outNewlines {
			t.Fatalf("maskCodeForAddressing(%q): newline count changed: in=%d out=%d",
				body, inNewlines, outNewlines)
		}

		// Masked output must preserve byte length (masking replaces content
		// with spaces/tabs, never adding or removing bytes).
		if len(view.masked) != len(body) {
			t.Fatalf("maskCodeForAddressing(%q): byte length changed: in=%d out=%d",
				body, len(body), len(view.masked))
		}
	})
}
