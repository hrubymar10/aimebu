package harnesslog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// parseFor dispatches a line to the parser for the given harness.
func parseFor(h Harness, line string) (ParsedEvent, ParseResult) {
	switch h {
	case HarnessPi:
		return ParsePi(line)
	case HarnessCodex:
		return ParseCodex(line)
	case HarnessClaudeCode:
		return ParseClaudeCode(line)
	case HarnessVibe:
		return ParseVibe(line)
	default:
		return ParsedEvent{}, NonJSON
	}
}

// harnessFromCommand maps a harness_spawn command to a Harness when the
// wrapper_start.harness field is absent. Commands like "pi-docker",
// "codex-docker", "claude" carry the harness name as a substring.
func harnessFromCommand(cmd string) Harness {
	c := strings.ToLower(cmd)
	switch {
	case strings.Contains(c, "codex"):
		return HarnessCodex
	case strings.Contains(c, "claude"):
		return HarnessClaudeCode
	case strings.Contains(c, "vibe"):
		return HarnessVibe
	case strings.Contains(c, "pi"):
		return HarnessPi
	default:
		return HarnessUnknown
	}
}

// Walk reads every *.log file under dir (skipping *.stderr.log — that is the
// aimebu wrapper's own prose, not harness output) and classifies each
// harness_stdout_raw line by the harness of the session that produced it. A
// single file can hold several sessions (re-registrations, pre-register →
// named), so the harness is tracked as a running variable updated by
// wrapper_start / harness_spawn / session_id_parsed events, not fixed per file.
func Walk(dir string) (*Report, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	rep := &Report{
		ByCount:      map[string]int{},
		Histogram:    map[string]map[string]int{},
		UnknownTypes: map[string]map[string]int{},
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".log") || strings.HasSuffix(name, ".stderr.log") {
			continue
		}
		paths = append(paths, filepath.Join(dir, name))
	}
	sort.Strings(paths)
	for _, p := range paths {
		if err := walkFile(rep, p); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	return rep, nil
}

// walkFile makes a single pass, tracking the current harness and classifying
// each harness_stdout_raw line with it. Counts and histograms are accumulated
// directly into rep; bounded samples go into a FileReport appended to rep.Files.
func walkFile(rep *Report, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fr := newFileReport(path, HarnessUnknown)
	h := HarnessUnknown
	add := func(hh Harness, ev ParsedEvent, res ParseResult, line string) {
		switch res {
		case Parsed:
			fr.Parsed++
			rep.ByCount["parsed"]++
			bump(rep.Histogram, string(hh), ev.Type)
		case UnknownType:
			fr.Unknown++
			rep.ByCount["unknown"]++
			bump(rep.UnknownTypes, string(hh), ev.Type)
			addSample(fr, line, ev.Type)
		case NonJSON:
			fr.NonJSON++
			rep.ByCount["nonjson"]++
			addSample(fr, line, "(non-json)")
		}
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	// Lines seen before the first harness event are rare (wrapper_start is
	// logged at session start); hold them and classify once the harness is known.
	var pending [][2]string
	for sc.Scan() {
		var env Envelope
		if json.Unmarshal(sc.Bytes(), &env) != nil {
			continue
		}
		switch env.Event {
		case "wrapper_start":
			if env.Harness != "" {
				h = Harness(env.Harness)
			}
		case "harness_spawn":
			if hh := harnessFromCommand(env.Command); hh != HarnessUnknown {
				h = hh
			}
		case "session_id_parsed":
			if env.Harness != "" {
				h = Harness(env.Harness)
			}
		case "harness_stdout_raw":
			if h == HarnessUnknown {
				pending = append(pending, [2]string{env.Line, env.Line})
				continue
			}
			for _, p := range pending {
				ev, res := parseFor(h, p[0])
				add(h, ev, res, p[1])
			}
			pending = nil
			ev, res := parseFor(h, env.Line)
			add(h, ev, res, env.Line)
		}
	}
	// Flush any pending lines (harness never identified) as non-JSON.
	for _, p := range pending {
		add(HarnessUnknown, ParsedEvent{}, NonJSON, p[1])
	}
	fr.Harness = h
	rep.Files = append(rep.Files, *fr)
	return sc.Err()
}

func bump(m map[string]map[string]int, harness, typ string) {
	sub := m[harness]
	if sub == nil {
		sub = map[string]int{}
		m[harness] = sub
	}
	sub[typ]++
}

func addSample(fr *FileReport, line, typ string) {
	if len(fr.Samples) >= 5 {
		return
	}
	s := strings.TrimSpace(line)
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	fr.Samples = append(fr.Samples, typ+" :: "+s)
}
