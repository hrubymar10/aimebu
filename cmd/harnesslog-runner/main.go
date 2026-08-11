// Command harnesslog-runner is the corpus runner for the harnesslog package.
// It walks a directory of aimebu agent JSONL debug logs, classifies every
// harness_stdout_raw line using the typed per-harness structs, and prints ONLY
// aggregates: per-harness event-type histograms, parsed/unknown/non-JSON counts,
// and at most five truncated samples of unrecognised shapes per file. It never
// prints raw log content beyond those bounded samples — iterate the structs in
// the harnesslog package until the unknown bucket approaches zero.
//
// Usage:
//
//	harnesslog-runner [log-dir]   (default: ~/.aimebu/agents/agent-logs/)
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/hrubymar10/aimebu/harnesslog"
)

func main() {
	dir := ""
	if len(os.Args) > 1 {
		dir = os.Args[1]
	} else {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".aimebu", "agents", "agent-logs")
	}
	rep, err := harnesslog.Walk(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "harnesslog-runner:", err)
		os.Exit(1)
	}

	fmt.Printf("harnesslog corpus: %s\n", dir)
	fmt.Printf("files scanned: %d\n", len(rep.Files))
	fmt.Printf("totals: parsed=%d unknown=%d nonjson=%d\n\n",
		rep.ByCount["parsed"], rep.ByCount["unknown"], rep.ByCount["nonjson"])

	harnesses := make([]string, 0, len(rep.Histogram))
	for h := range rep.Histogram {
		harnesses = append(harnesses, h)
	}
	sort.Strings(harnesses)
	for _, h := range harnesses {
		hm := rep.Histogram[h]
		fmt.Printf("=== %s (parsed) ===\n", h)
		types := make([]string, 0, len(hm))
		for t := range hm {
			types = append(types, t)
		}
		sort.Strings(types)
		for _, t := range types {
			fmt.Printf("  %-24s %d\n", t, hm[t])
		}
		fmt.Println()
	}

	if len(rep.UnknownTypes) > 0 {
		fmt.Println("=== unknown types (add to structs) ===")
		uhs := make([]string, 0, len(rep.UnknownTypes))
		for h := range rep.UnknownTypes {
			uhs = append(uhs, h)
		}
		sort.Strings(uhs)
		for _, h := range uhs {
			um := rep.UnknownTypes[h]
			fmt.Printf("-- %s\n", h)
			ts := make([]string, 0, len(um))
			for t := range um {
				ts = append(ts, t)
			}
			sort.Strings(ts)
			for _, t := range ts {
				fmt.Printf("   %-24s %d\n", t, um[t])
			}
		}
		fmt.Println()
	}

	any := false
	for _, fr := range rep.Files {
		if len(fr.Samples) == 0 {
			continue
		}
		if !any {
			fmt.Println("=== unrecognised samples (truncated) ===")
			any = true
		}
		fmt.Printf("-- %s [%s] unknown=%d nonjson=%d\n", filepath.Base(fr.Path), fr.Harness, fr.Unknown, fr.NonJSON)
		for _, s := range fr.Samples {
			fmt.Printf("   %s\n", s)
		}
	}
}
