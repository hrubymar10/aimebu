package updatecheck

import "testing"

func TestParseChannel(t *testing.T) {
	for word, want := range map[string]string{
		"": "", "stable": "stable", "prerelease": "prerelease", "edge": "edge",
		"edge:main": "edge:main",
	} {
		if got, err := ParseChannel(word); err != nil || got != want {
			t.Errorf("ParseChannel(%q) = %q, %v; want %q", word, got, err, want)
		}
	}
	for _, word := range []string{"beta", "edge:", "EDGE", "edge:main x"} {
		if _, err := ParseChannel(word); err == nil {
			t.Errorf("ParseChannel(%q) accepted an invalid channel", word)
		}
	}
}

// The ordering table: each entry must sort strictly after the one before it.
func TestCompareTagsOrdersTheSharedTable(t *testing.T) {
	for i := 1; i < len(TestTags); i++ {
		if c := CompareTags(TestTags[i-1], TestTags[i]); c >= 0 {
			t.Fatalf("CompareTags(%s, %s) = %d, want < 0", TestTags[i-1], TestTags[i], c)
		}
		if c := CompareTags(TestTags[i], TestTags[i-1]); c <= 0 {
			t.Fatalf("CompareTags(%s, %s) = %d, want > 0", TestTags[i], TestTags[i-1], c)
		}
	}
	if c := CompareTags(TestTags[0], TestTags[0]); c != 0 {
		t.Fatalf("CompareTags(x, x) = %d, want 0", c)
	}
}

func TestCompareTagsSpotsTheClassicMistakes(t *testing.T) {
	// alpha.9 < alpha.10 kills a naive string compare; rc.1 < release kills a
	// prerelease-suffix string compare.
	for _, pair := range [][2]string{
		{"v2.1.0-alpha.9", "v2.1.0-alpha.10"},
		{"v2.1.0-alpha.10", "v2.1.0-beta.1"},
		{"v2.1.0-beta.1", "v2.1.0-rc.1"},
		{"v2.1.0-rc.1", "v2.1.0"},
		{"v2.1.0-alpha.1", "v2.2.0"},
		{"v2.9.9", "v2.10.0"},
	} {
		if c := CompareTags(pair[0], pair[1]); c >= 0 {
			t.Errorf("CompareTags(%s, %s) = %d, want < 0", pair[0], pair[1], c)
		}
	}
}

func TestMajorOf(t *testing.T) {
	for version, want := range map[string]int{"v2.0.0": 2, "v2.0.0-3-gabcd": 2, "2.1.0": 2} {
		if got, ok := MajorOf(version); !ok || got != want {
			t.Errorf("MajorOf(%q) = %d, %v; want %d", version, got, ok, want)
		}
	}
	for _, version := range []string{"", "abcd1234", "v"} {
		if _, ok := MajorOf(version); ok {
			t.Errorf("MajorOf(%q) read a major out of nothing", version)
		}
	}
}

func TestSelectTag(t *testing.T) {
	for _, tc := range []struct {
		running, channel, want string
	}{
		{"v2.0.0", Stable, "v2.1.0"},
		{"v2.0.0", Prerelease, "v2.1.0"},
		{"v2.1.0-rc.1", Stable, "v2.1.0"},
		{"v2.1.0-rc.1", Prerelease, "v2.1.0"},
		{"v2.1.0", Stable, "v2.1.0"},
		{"v1.9.0", Stable, "v1.9.0"},
		{"v3.0.0-alpha.1", Stable, ""},
		{"v3.0.0-alpha.1", Prerelease, "v3.0.0-alpha.1"},
		{"abcd1234", Stable, ""},
	} {
		got, ok := SelectTag(TestTags, tc.running, tc.channel)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("SelectTag(running=%s, %s) = %q, %v; want %q", tc.running, tc.channel, got, ok, tc.want)
		}
	}
}

func TestDefaultChannel(t *testing.T) {
	// Major 2 has the stable v2.1.0; major 3 only an alpha; a bare hash has no
	// major at all and falls back to stable.
	for _, tc := range []struct{ running, want string }{
		{"v2.0.0", Stable},
		{"v2.1.0-rc.1", Stable},
		{"v3.0.0-alpha.1", Prerelease},
		{"v9.9.9", Prerelease},
		{"abcd1234", Stable},
	} {
		if got := DefaultChannel(TestTags, tc.running); got != tc.want {
			t.Errorf("DefaultChannel(%s) = %s, want %s", tc.running, got, tc.want)
		}
	}
}
