package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/goccy/go-json"
)

// Sharding splits one package's tests over several `go test` processes that run side by side. Go runs the packages of one `go test` in
// parallel but the tests of one package one after the other (these tests call t.Setenv, which rules out t.Parallel), so one big package is
// the whole run's critical path: internal/server alone takes as long as everything else together. A shard runs `-run '^(A|B|C)$'` for its
// share of the package's top-level tests; the tracker shows each shard as its own package ("m/server [shard 2/4]").

// shardSpec maps a package to the number of shards it runs in.
type shardSpec struct {
	key    string // an import path, or the end of one ("internal/server")
	shards int
}

// parseShards reads "internal/server=4,internal/store=3". An empty string is no sharding.
func parseShards(text string) ([]shardSpec, error) {
	var specs []shardSpec
	for _, part := range strings.Split(text, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, count, ok := strings.Cut(part, "=")
		n, err := strconv.Atoi(strings.TrimSpace(count))
		if !ok || key == "" || err != nil || n < 1 {
			return nil, fmt.Errorf("bad shard %q: want package=count with a count of at least 1", part)
		}
		specs = append(specs, shardSpec{strings.TrimSpace(key), n})
	}
	return specs, nil
}

// shardsFor is how many shards import path pkg runs in (1 when it is not sharded). A key matches the whole import path or its last elements.
func shardsFor(specs []shardSpec, pkg string) int {
	for _, spec := range specs {
		if pkg == spec.key || strings.HasSuffix(pkg, "/"+spec.key) {
			return spec.shards
		}
	}
	return 1
}

// splitArgs separates go test's package patterns from its flags (a flag's separate value stays with the flag).
func splitArgs(args []string) (flags, pats []string) {
	skip := false
	for _, arg := range args {
		switch {
		case skip:
			flags = append(flags, arg)
			skip = false
		case strings.HasPrefix(arg, "-"):
			flags = append(flags, arg)
			skip = valueFlags[arg] && !strings.Contains(arg, "=")
		default:
			pats = append(pats, arg)
		}
	}
	return flags, pats
}

// hasFlag reports whether go test's flags set name (-run or -run=...).
func hasFlag(flags []string, name string) bool {
	for _, flag := range flags {
		if flag == name || strings.HasPrefix(flag, name+"=") {
			return true
		}
	}
	return false
}

// withoutFlag drops a flag and its value.
func withoutFlag(flags []string, name string) []string {
	var out []string
	skip := false
	for _, flag := range flags {
		switch {
		case skip:
			skip = false
		case flag == name:
			skip = true
		case strings.HasPrefix(flag, name+"="):
		default:
			out = append(out, flag)
		}
	}
	return out
}

// weights are the known durations in seconds of top-level tests, per package (the end of its import path), from a file written by
// scripts/shard-weights.cjs. A test the file does not list weighs the package's "*" entry (or the median of the listed ones), so a stale file
// only balances worse.
type weights map[string]map[string]float64

func loadWeights(path string) weights {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var w weights
	if json.Unmarshal(data, &w) != nil {
		return nil
	}
	return w
}

func (w weights) forPackage(pkg string) map[string]float64 {
	best, bestLen := map[string]float64(nil), 0
	for key, tests := range w {
		if (pkg == key || strings.HasSuffix(pkg, "/"+key)) && len(key) > bestLen {
			best, bestLen = tests, len(key)
		}
	}
	return best
}

// partition splits tests into n groups of about equal total weight: longest first, each to the group with the least so far (ties go to the
// lower group, so the same input always gives the same groups). Every group is sorted by name. Empty groups are dropped.
func partition(tests []string, known map[string]float64, n int) [][]string {
	if n > len(tests) {
		n = len(tests)
	}
	if n < 1 {
		return nil
	}
	// What a test the file does not list weighs: the package's "*" entry (the average of its short tests), else the median of the listed ones.
	fallback, hasFallback := known["*"]
	if !hasFallback {
		var seen []float64
		for _, test := range tests {
			if w, ok := known[test]; ok {
				seen = append(seen, w)
			}
		}
		fallback = 0.3
		if len(seen) > 0 {
			sort.Float64s(seen)
			fallback = seen[len(seen)/2]
		}
	}
	weight := func(test string) float64 {
		if w, ok := known[test]; ok {
			return w
		}
		return fallback
	}
	order := append([]string(nil), tests...)
	sort.Slice(order, func(i, j int) bool {
		if wi, wj := weight(order[i]), weight(order[j]); wi != wj {
			return wi > wj
		}
		return order[i] < order[j]
	})
	groups := make([][]string, n)
	load := make([]float64, n)
	for _, test := range order {
		least := 0
		for i := 1; i < n; i++ {
			if load[i] < load[least] {
				least = i
			}
		}
		groups[least] = append(groups[least], test)
		load[least] += weight(test)
	}
	var out [][]string
	for _, group := range groups {
		if len(group) > 0 {
			sort.Strings(group)
			out = append(out, group)
		}
	}
	return out
}

// runPattern is the -run value that selects exactly these top-level tests.
func runPattern(tests []string) string {
	quoted := make([]string, len(tests))
	for i, test := range tests {
		quoted[i] = regexp.QuoteMeta(test)
	}
	return "^(" + strings.Join(quoted, "|") + ")$"
}

// listTests asks go test for a package's top-level tests (`-list`), with the same flags as the run so the test binary it builds is the one
// the run reuses from the build cache.
func listTests(flags []string, pkg string) ([]string, error) {
	args := append([]string{"test", "-list", "^(Test|Fuzz)"}, withoutFlag(withoutFlag(flags, "-run"), "-count")...)
	args = append(args, pkg)
	out, err := exec.Command("go", args...).Output()
	if err != nil {
		return nil, err
	}
	var tests []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "Test") || strings.HasPrefix(line, "Fuzz") {
			tests = append(tests, line)
		}
	}
	return tests, nil
}
