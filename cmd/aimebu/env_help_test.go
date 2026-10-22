package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestEnvironmentHelpMatchesSourceAndDocs keeps three things in step: the
// variables the non-test sources name, the `aimebu help` table, and the guides.
func TestEnvironmentHelpMatchesSourceAndDocs(t *testing.T) {
	literal := regexp.MustCompile(`"(AIMEBU2?_[A-Z0-9_]+)"`)
	inSource := map[string]string{}
	for _, root := range []string{"../../cmd", "../../internal"} {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || filepath.Base(path) == "env_help.go" {
				return err
			}
			// internal/testenv is test support, imported by _test.go files only: it names the variables it clears, it does not read them.
			if strings.HasPrefix(filepath.ToSlash(path), "../../internal/testenv/") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, match := range literal.FindAllSubmatch(data, -1) {
				if _, seen := inSource[string(match[1])]; !seen {
					inSource[string(match[1])] = path
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	inTable := map[string]bool{}
	for _, entry := range environment {
		if len(entry.help) == 0 {
			t.Fatalf("%v has no help text", entry.names)
		}
		for _, name := range entry.names {
			if inTable[name] {
				t.Fatalf("%s is listed twice", name)
			}
			inTable[name] = true
		}
	}
	var problems []string
	for name, path := range inSource {
		if !inTable[name] {
			problems = append(problems, name+" is read in "+path+" but missing from the help table (cmd/aimebu/env_help.go)")
		}
	}
	for name := range inTable {
		if _, ok := inSource[name]; !ok {
			problems = append(problems, name+" is in the help table but no source names it any more")
		}
	}

	docs := []string{"../../README.md"}
	guides, err := filepath.Glob("../../docs/*.md")
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, path := range append(docs, guides...) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text.Write(data)
	}
	var help bytes.Buffer
	printUsage(&help)
	for name := range inTable {
		if !strings.Contains(text.String(), name) {
			problems = append(problems, name+" is not documented in README.md or docs/")
		}
		if !strings.Contains(help.String(), name) {
			problems = append(problems, name+" is missing from the `aimebu help` output")
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("environment variable drift:\n  %s", strings.Join(problems, "\n  "))
	}
}
