package buildversion

import "testing"

func TestOlderBuildSemverAndDevSuffix(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		supervisor, server string
		older              bool
	}{
		{"v2.0.0-alpha.15", "v2.0.0-alpha.17", true},
		{"aimebu v2.0.0-alpha.15", "aimebu v2.0.0-alpha.17", true},
		{"v2.0.0-alpha.17-3-gabc123", "v2.0.0-alpha.17", false},
		{"v2.0.0-alpha.17-3-gabc123", "v2.0.0-alpha.17-9-gdef456", false},
		{"v2.0.0-alpha.9", "v2.0.0-alpha.10", true},
		{"unknown", "v2.0.0-alpha.17", false},
		{"v2.0.0-alpha.17", "unknown", false},
		{"v2.0.0-alpha.17", "v2.0.0", true},
		{"v2.0.0", "v2.0.0-alpha.17", false},
	} {
		if got := Older(test.supervisor, test.server); got != test.older {
			t.Errorf("Older(%q, %q)=%t, want %t", test.supervisor, test.server, got, test.older)
		}
	}
}
