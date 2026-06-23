package server

import "testing"

func TestCanonicalModelSlug(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		harness string
		want    string
	}{
		{name: "empty", raw: "", harness: "codex", want: "unknown"},
		{name: "unknown untouched", raw: " unknown ", harness: "codex", want: "unknown"},
		{name: "claude opus full id", raw: "claude-opus-4-8", harness: "claude-code", want: "opus4.8"},
		{name: "claude sonnet full id", raw: "claude-sonnet-4-6", harness: "claude-code", want: "sonnet4.6"},
		{name: "claude haiku full id", raw: "claude-haiku-4-5", harness: "claude-code", want: "haiku4.5"},
		{name: "claude sibling", raw: "claude-opus-4-7", harness: "claude-code", want: "opus4.7"},
		{name: "claude future family", raw: "claude-neptune-1-0", harness: "claude-code", want: "neptune1.0"},
		{name: "bracket suffix", raw: "claude-opus-4-8 [1m]", harness: "claude-code", want: "opus4.8"},
		{name: "date suffix", raw: "claude-sonnet-4-6-20260622", harness: "claude-code", want: "sonnet4.6"},
		{name: "provider prefix", raw: "anthropic/claude-haiku-4-5", harness: "claude-code", want: "haiku4.5"},
		{name: "gpt hyphen added", raw: "gpt5", harness: "codex", want: "gpt-5"},
		{name: "gpt hyphen kept", raw: "gpt-5", harness: "codex", want: "gpt-5"},
		{name: "gpt minor hyphen added", raw: "gpt5.5", harness: "codex", want: "gpt-5.5"},
		{name: "gpt minor hyphen kept", raw: "gpt-5.5", harness: "codex", want: "gpt-5.5"},
		{name: "gpt codex variant distinct", raw: "gpt-5-codex", harness: "codex", want: "gpt-5-codex"},
		{name: "gpt non-version untouched", raw: "gpt-experimental", harness: "codex", want: "gpt-experimental"},
		{name: "pass through unmapped", raw: "gemma4:31b", harness: "pi", want: "gemma4:31b"},
		{name: "preserve mistral hyphens", raw: "mistral-medium-3.5", harness: "vibe", want: "mistral-medium-3.5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := canonicalModelSlug(tc.raw, tc.harness); got != tc.want {
				t.Fatalf("canonicalModelSlug(%q, %q) = %q, want %q", tc.raw, tc.harness, got, tc.want)
			}
		})
	}
}
