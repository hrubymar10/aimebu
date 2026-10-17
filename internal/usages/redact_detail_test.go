package usages

import (
	"strings"
	"testing"
)

// Fake secrets only; each is shaped like the real thing.
const (
	rdJWT     = "eyJhbGciOiJub25lIn0.eyJlbWFpbCI6ImZha2VAZXhhbXBsZS50ZXN0In0.ZmFrZXNpZw"
	rdRefresh = "rt-FAKE-refresh-token-value-1234567890"
	rdAnthro  = "sk-ant-oat01-FAKEFAKEFAKEFAKE"
	rdOpaque  = "AbCdEf0123456789AbCdEf0123456789AbCdEf0123456789"
)

func TestRedactDetailStripsSecrets(t *testing.T) {
	cases := []struct {
		name, in string
		gone     []string
		kept     []string
	}{
		{"jwt", "request failed for " + rdJWT + " (401)", []string{rdJWT, "eyJhbGci"}, []string{"request failed for", "(401)"}},
		{"anthropic key", "using " + rdAnthro, []string{rdAnthro}, []string{"using"}},
		{"bearer", "Authorization: Bearer abc.def-123", []string{"abc.def-123"}, []string{"Bearer"}},
		{"json refresh field", `body {"refresh_token":"` + rdRefresh + `","x":1}`, []string{rdRefresh}, []string{`"x":1`}},
		{"camel case field", `{"refreshToken": "` + rdRefresh + `"}`, []string{rdRefresh}, nil},
		{"escaped json in a string", `got "{\"access_token\":\"` + rdRefresh + `\"}"`, []string{rdRefresh}, nil},
		{"key=value", "refresh_token=" + rdRefresh + " retry", []string{rdRefresh}, []string{"retry"}},
		{"query string", "https://x.test/cb?id_token=" + rdRefresh + "&state=ok", []string{rdRefresh}, []string{"state=ok"}},
		{"opaque run", "cookie " + rdOpaque + " end", []string{rdOpaque}, []string{"cookie", "end"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactDetailHome(tc.in, "")
			for _, g := range tc.gone {
				if strings.Contains(got, g) {
					t.Errorf("%q still contains %q", got, g)
				}
			}
			for _, k := range tc.kept {
				if !strings.Contains(got, k) {
					t.Errorf("%q lost %q", got, k)
				}
			}
			if !strings.Contains(got, redacted) {
				t.Errorf("nothing was redacted in %q", got)
			}
		})
	}
}

func TestRedactDetailKeepsOrdinaryText(t *testing.T) {
	in := "the CLI failed after starting: exit status 1: dial tcp: lookup api.example.test: no such host"
	if got := redactDetailHome(in, ""); got != in {
		t.Fatalf("ordinary text changed: %q", got)
	}
	// A long all-letter run (a path segment, a word) is not a secret.
	long := strings.Repeat("abcdefghij", 6)
	if got := redactDetailHome("path /x/"+long+"/y", ""); !strings.Contains(got, long) {
		t.Fatalf("a letters-only run was redacted: %q", got)
	}
}

func TestRedactDetailShortensTheHomeDirectory(t *testing.T) {
	got := redactDetailHome("open /Users/someone/.claude/.credentials.json: permission denied", "/Users/someone")
	if strings.Contains(got, "someone") || !strings.Contains(got, "~/.claude/.credentials.json") {
		t.Fatalf("home not shortened: %q", got)
	}
}

func TestRedactDetailIsIdempotent(t *testing.T) {
	once := redactDetailHome(`{"refresh_token":"`+rdRefresh+`"} `+rdJWT, "")
	if twice := redactDetailHome(once, ""); twice != once {
		t.Fatalf("not idempotent:\n%q\n%q", once, twice)
	}
}
