package main

import (
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestParseShards(t *testing.T) {
	got, err := parseShards("internal/server=4, internal/store=3,")
	if err != nil || !reflect.DeepEqual(got, []shardSpec{{"internal/server", 4}, {"internal/store", 3}}) {
		t.Fatalf("got %v, %v", got, err)
	}
	if got, err := parseShards(""); err != nil || len(got) != 0 {
		t.Fatalf("empty is no sharding: %v, %v", got, err)
	}
	for _, bad := range []string{"internal/server", "internal/server=0", "=3", "internal/server=x"} {
		if _, err := parseShards(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestAShardKeyMatchesTheEndOfAnImportPath(t *testing.T) {
	specs := []shardSpec{{"internal/server", 4}}
	for pkg, want := range map[string]int{
		"github.com/x/aimebu/internal/server":         4,
		"internal/server":                             4,
		"github.com/x/aimebu/internal/serverx":        1,
		"github.com/x/aimebu/internal/server/sub":     1,
		"github.com/x/aimebu/notinternal/server":      1,
		"github.com/x/aimebu/cmd/aimebu":              1,
		"github.com/x/aimebu/internal/store/internal": 1,
	} {
		if got := shardsFor(specs, pkg); got != want {
			t.Errorf("shardsFor(%q) = %d, want %d", pkg, got, want)
		}
	}
}

func TestSplitArgsKeepsAFlagsValueWithTheFlag(t *testing.T) {
	flags, pats := splitArgs([]string{"-race", "-timeout", "25m", "-count=1", "./internal/...", "./cmd/..."})
	if !reflect.DeepEqual(flags, []string{"-race", "-timeout", "25m", "-count=1"}) || !reflect.DeepEqual(pats, []string{"./internal/...", "./cmd/..."}) {
		t.Fatalf("flags %v, patterns %v", flags, pats)
	}
	if !hasFlag(flags, "-timeout") || !hasFlag(flags, "-count") || hasFlag(flags, "-run") {
		t.Fatal("hasFlag")
	}
	if got := withoutFlag(flags, "-timeout"); !reflect.DeepEqual(got, []string{"-race", "-count=1"}) {
		t.Fatalf("withoutFlag = %v", got)
	}
	if got := withoutFlag(flags, "-count"); !reflect.DeepEqual(got, []string{"-race", "-timeout", "25m"}) {
		t.Fatalf("withoutFlag = %v", got)
	}
}

func TestPartitionCoversEveryTestOnceAndBalancesByWeight(t *testing.T) {
	tests := []string{"TestA", "TestB", "TestC", "TestD", "TestE", "TestF", "TestG"}
	known := map[string]float64{"TestA": 100, "TestB": 60, "TestC": 40, "TestD": 30}
	groups := partition(tests, known, 3)
	if len(groups) != 3 {
		t.Fatalf("groups = %v", groups)
	}
	var all []string
	loads := []float64{}
	for _, group := range groups {
		sum := 0.0
		for _, test := range group {
			all = append(all, test)
			w, ok := known[test]
			if !ok {
				w = 40 // the median of the known weights (30, 40, 60, 100 -> index 2)
			}
			sum += w
		}
		loads = append(loads, sum)
	}
	sort.Strings(all)
	if !reflect.DeepEqual(all, tests) {
		t.Fatalf("every test exactly once: %v", all)
	}
	// The heaviest test is alone with at most what keeps the groups close: no group may carry more than the heaviest test plus one light one.
	for _, load := range loads {
		if load > 140 {
			t.Fatalf("unbalanced loads %v from %v", loads, groups)
		}
	}
	// The same input gives the same groups.
	if again := partition(tests, known, 3); !reflect.DeepEqual(again, groups) {
		t.Fatalf("not deterministic: %v vs %v", groups, again)
	}
	for _, group := range groups {
		if !sort.StringsAreSorted(group) {
			t.Fatalf("group not sorted: %v", group)
		}
	}
}

func TestPartitionNeverMakesMoreGroupsThanTests(t *testing.T) {
	if got := partition([]string{"TestA", "TestB"}, nil, 5); len(got) != 2 {
		t.Fatalf("got %v", got)
	}
	if got := partition(nil, nil, 3); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestRunPatternMatchesExactlyTheGivenTests(t *testing.T) {
	pattern := runPattern([]string{"TestA", "TestB.c"})
	re := regexp.MustCompile(pattern)
	for name, want := range map[string]bool{"TestA": true, "TestB.c": true, "TestAB": false, "TestBXc": false, "XTestA": false} {
		// go test matches each slash-separated element of -run against the test name, so a top-level name is matched whole.
		if got := re.MatchString(name); got != want {
			t.Errorf("%q: matched %v, want %v (%s)", name, got, want, pattern)
		}
	}
	if !strings.HasPrefix(pattern, "^(") || !strings.HasSuffix(pattern, ")$") {
		t.Fatalf("pattern = %s", pattern)
	}
}

func TestWeightsPickTheLongestMatchingPackageKey(t *testing.T) {
	w := weights{"server": {"TestShort": 1}, "internal/server": {"TestLong": 2}}
	if got := w.forPackage("github.com/x/aimebu/internal/server"); got["TestLong"] != 2 {
		t.Fatalf("got %v", got)
	}
	if got := (weights)(nil).forPackage("anything"); got != nil {
		t.Fatalf("got %v", got)
	}
}

func TestPlanWithoutShardsIsTheOneProcessAsked(t *testing.T) {
	jobs, _ := plan([]string{"-race", "./scripts/gotestprogress"}, nil, nil, func(string) {})
	if len(jobs) != 1 || jobs[0].suffix != "" || !reflect.DeepEqual(jobs[0].args, []string{"-race", "./scripts/gotestprogress"}) {
		t.Fatalf("jobs = %+v", jobs)
	}
}

func TestPlanDoesNotShardWhenRunIsSet(t *testing.T) {
	var notes []string
	jobs, _ := plan([]string{"-run", "TestX", "./scripts/gotestprogress"}, []shardSpec{{"gotestprogress", 3}}, nil, func(text string) { notes = append(notes, text) })
	if len(jobs) != 1 || len(notes) != 1 {
		t.Fatalf("jobs %+v notes %v", jobs, notes)
	}
}

func TestPlanShardsAPackageIntoJobsThatCoverItsTests(t *testing.T) {
	// This package's own tests: a real `go test -list`, split in three.
	jobs, total := plan([]string{"."}, []shardSpec{{"scripts/gotestprogress", 3}}, nil, func(string) {})
	if len(jobs) != 3 || total != 3 {
		t.Fatalf("jobs %d, total %d", len(jobs), total)
	}
	listed, err := listTests(nil, ".")
	if err != nil || len(listed) < 3 {
		t.Fatalf("listed %v, %v", listed, err)
	}
	covered := 0
	for k, j := range jobs {
		if j.suffix != " [shard "+string(rune('1'+k))+"/3]" {
			t.Errorf("suffix %q", j.suffix)
		}
		// The -run value is the argument before the package.
		re := regexp.MustCompile(j.args[len(j.args)-2])
		for _, test := range listed {
			if re.MatchString(test) {
				covered++
			}
		}
	}
	if covered != len(listed) {
		t.Fatalf("the shards cover %d of %d tests, each once", covered, len(listed))
	}
}
