package harnesslog

// vibe: no vibe sessions were present in the top-20 log files by size at the
// time of writing (wren, 2026-08-11 night pack). ParseVibe is a stub so the
// runner can dispatch to it without ceremony if vibe logs appear later; add
// typed structs and a known-type set when a vibe corpus is available.

var vibeKnown = map[string]bool{}

// ParseVibe classifies one vibe stdout line. With no known types every JSON
// line is UnknownType until structs are added from a real vibe corpus.
func ParseVibe(line string) (ParsedEvent, ParseResult) {
	return classify(line, vibeKnown, HarnessVibe)
}
