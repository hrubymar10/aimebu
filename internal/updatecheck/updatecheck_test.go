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

func TestParseTagGrammar(t *testing.T) {
	for _, name := range TestTags {
		if _, ok := ParseTag(name); !ok {
			t.Errorf("ParseTag(%q) refused a shared-table tag", name)
		}
	}
	for _, name := range []string{
		"", "v", "2.1.0", "v2", "v2.1", "v2.1.0.0", "v2.1.x", "v2.1.0-", "v2.1.0-rc", "v2.1.0-rc.", "v2.1.0-rc.x",
		"v2.1.0-foo.1", "v2.1.0-alpha", "v2.1.0-RC.1", "v2.1.0-rc.1.2", "v2.1.0-rc.1-3-gabc", "v2.1.0+build", "v2.-1.0",
		"v123456.0.0", "v2.1.0 ", " v2.1.0", "v2.1.0-rc.123456",
	} {
		if _, ok := ParseTag(name); ok {
			t.Errorf("ParseTag(%q) accepted a malformed tag", name)
		}
	}
}

func TestParseDescribeStripsTheDistanceSuffix(t *testing.T) {
	for version, want := range map[string]Describe{
		"v2.1.0":                    {Tag: "v2.1.0"},
		"v2.1.0-3-gabcdef1":         {Tag: "v2.1.0", Count: 3, SHA: "abcdef1"},
		"v2.1.0-3-gabcdef1-dirty":   {Tag: "v2.1.0", Count: 3, SHA: "abcdef1"},
		"v2.1.0-rc.1":               {Tag: "v2.1.0-rc.1"},
		"v2.1.0-rc.1-12-g0123abc":   {Tag: "v2.1.0-rc.1", Count: 12, SHA: "0123abc"},
		"v2.1.0-alpha.10-1-gdeadbe": {Tag: "v2.1.0-alpha.10", Count: 1, SHA: "deadbe"},
	} {
		got, ok := ParseDescribe(version)
		if !ok || got != want {
			t.Errorf("ParseDescribe(%q) = %+v, %v; want %+v", version, got, ok, want)
		}
	}
	for _, version := range []string{"", "abcdef1", "v2.1.0-x-gabcdef1", "foo-3-gabcdef1", "v2.1.0-+3-gabcdef1"} {
		if _, ok := ParseDescribe(version); ok {
			t.Errorf("ParseDescribe(%q) read a release out of it", version)
		}
	}
}

func TestCompareVersionsWithDescribeSuffixes(t *testing.T) {
	// A checkout past a tag is newer than that tag, so the tag is never offered to it;
	// the same distance on the same base compares equal; the base decides first.
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"v2.1.0", "v2.1.0-3-gabcdef1", -1},
		{"v2.1.0-3-gabcdef1", "v2.1.0", 1},
		{"v2.1.0-3-gabcdef1", "v2.1.0-4-g1234567", -1},
		{"v2.1.0-10-gabcdef1", "v2.1.0-9-g1234567", 1},
		{"v2.1.0-3-gabcdef1", "v2.1.0-3-g1234567", 0},
		{"v2.1.0-3-gabcdef1", "v2.1.0-3-gabcdef1-dirty", 0},
		{"v2.1.0", "v2.1.0-1-gabcdef1-dirty", -1},
		{"v2.1.0-rc.1-9-gabcdef1", "v2.1.0", -1},
		{"v2.1.0-alpha.9-4-gabcdef1", "v2.1.0-alpha.10", -1},
		{"v2.0.0-50-gabcdef1", "v2.1.0-alpha.1", -1},
	} {
		if got, ok := CompareVersions(tc.a, tc.b); !ok || got != tc.want {
			t.Errorf("CompareVersions(%q, %q) = %d, %v; want %d", tc.a, tc.b, got, ok, tc.want)
		}
	}
	if newer, _ := IsNewer("v2.1.0", "v2.1.0-3-gabcdef1"); newer {
		t.Error("v2.1.0 was offered to a checkout three commits past it")
	}
}

func TestSelectTagIgnoresMalformedTags(t *testing.T) {
	tags := append([]string{"v2.9.0-foo.1", "v2.8", "v2.7.0-rc", "v2.6.0-RC.1", "v2.5.0.1"}, TestTags...)
	got, ok := SelectTag(tags, "v2.0.0", Prerelease)
	if !ok || got != "v2.1.0" {
		t.Errorf("SelectTag with malformed tags = %q, %v; want v2.1.0", got, ok)
	}
	if DefaultChannel([]string{"v7.0.0-foo.1", "v7.0"}, "v7.0.0") != Prerelease {
		t.Error("a malformed tag counted as the stable release that flips the default")
	}
}
