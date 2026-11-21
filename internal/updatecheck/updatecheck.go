// Package updatecheck implements the release-channel rules shared by the server's
// own update checker and the bin/aimebu wrapper's tests: channel parsing, the semver
// ordering of release tags, and the choice of the tag a channel points at.
//
// The wrapper re-states the rules in bash (it cannot import Go), so the shared
// TestTags table below pins the ordering both sides must agree on.
package updatecheck

import (
	"strconv"
	"strings"
)

// Channel names. edge tracks a branch (the remote's default branch, or the one
// named after the colon in edge:<branch>); the other two pick release tags.
const (
	Stable     = "stable"
	Prerelease = "prerelease"
	Edge       = "edge"
)

// ParseChannel validates one channel word: stable, prerelease, edge, or
// edge:<branch>. An empty word means "no channel set".
func ParseChannel(word string) (string, error) {
	switch word {
	case "":
		return "", nil
	case Stable, Prerelease, Edge:
		return word, nil
	}
	if branch, ok := strings.CutPrefix(word, Edge+":"); ok && branch != "" && !strings.ContainsAny(branch, " \t\n") {
		return word, nil
	}
	return "", strconv.ErrSyntax
}

// Tag is a parsed release tag, "v2.1.0" style.
type Tag struct {
	Name       string
	Major      int
	Prerelease bool
}

// ParseTag reads a release tag. It returns false for anything that is not a
// vMAJOR.MINOR.PATCH name, optionally with a -<pre> suffix.
func ParseTag(name string) (Tag, bool) {
	rest, ok := strings.CutPrefix(name, "v")
	if !ok {
		return Tag{}, false
	}
	pre := ""
	if i := strings.IndexByte(rest, '-'); i >= 0 {
		pre, rest = rest[i+1:], rest[:i]
		if pre == "" {
			return Tag{}, false
		}
	}
	major, rest, ok := numbers(rest)
	if !ok || !strings.HasPrefix(rest, ".") {
		return Tag{}, false
	}
	_, rest, ok = numbers(rest[1:])
	if !ok || !strings.HasPrefix(rest, ".") {
		return Tag{}, false
	}
	_, rest, ok = numbers(rest[1:])
	if !ok || rest != "" {
		return Tag{}, false
	}
	return Tag{Name: name, Major: major, Prerelease: pre != ""}, true
}

func numbers(s string) (int, string, bool) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, s, false
	}
	n, err := strconv.Atoi(s[:i])
	return n, s[i:], err == nil
}

// CompareTags orders release tags so that alpha.9 < alpha.10 < beta.1 < rc.1 <
// release. An unparseable name sorts last.
func CompareTags(a, b string) int {
	pa, oka := ParseTag(a)
	pb, okb := ParseTag(b)
	switch {
	case !oka && !okb:
		return strings.Compare(a, b)
	case !oka:
		return 1
	case !okb:
		return -1
	}
	if c := compareCore(pa, pb); c != 0 {
		return c
	}
	// A release sorts after every prerelease of the same version.
	if pa.Prerelease != pb.Prerelease {
		if pa.Prerelease {
			return -1
		}
		return 1
	}
	return comparePrerelease(pa.Name, pb.Name)
}

func compareCore(a, b Tag) int {
	aCore, bCore := coreOf(a.Name[1:]), coreOf(b.Name[1:])
	for range 3 {
		an, arest, _ := numbers(aCore)
		bn, brest, _ := numbers(bCore)
		if an != bn {
			return sign(an - bn)
		}
		aCore, bCore = strings.TrimPrefix(arest, "."), strings.TrimPrefix(brest, ".")
	}
	return 0
}

func coreOf(s string) string {
	if i := strings.IndexByte(s, '-'); i >= 0 {
		return s[:i]
	}
	return s
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// prereleaseWeight ranks the known labels; an unknown label sorts before them,
// so a future label never overtakes rc by accident.
func prereleaseWeight(label string) int {
	switch label {
	case "alpha":
		return 1
	case "beta":
		return 2
	case "rc":
		return 3
	}
	return 0
}

func comparePrerelease(a, b string) int {
	_, as, _ := strings.Cut(a[1:], "-")
	_, bs, _ := strings.Cut(b[1:], "-")
	for {
		switch {
		case as == "" && bs == "":
			return 0
		case as == "":
			return -1 // b carries a prerelease part a does not: a is newer (a release beat it to the caller already)
		case bs == "":
			return 1
		}
		aLabel, aRest, _ := strings.Cut(as, ".")
		bLabel, bRest, _ := strings.Cut(bs, ".")
		if c := comparePrereleasePart(aLabel, bLabel); c != 0 {
			return c
		}
		as, bs = aRest, bRest
	}
}

// comparePrereleasePart compares one dot-separated part, "alpha" or "9": labels
// by weight, numbers numerically, and a number after a label of the same weight
// class is the newer.
func comparePrereleasePart(a, b string) int {
	an, _, aNum := numbers(a)
	bn, _, bNum := numbers(b)
	switch {
	case aNum && bNum:
		return sign(an - bn)
	case aNum:
		return 1
	case bNum:
		return -1
	}
	return sign(prereleaseWeight(a) - prereleaseWeight(b))
}

// MajorOf reads the major of a version string that starts with a (possibly
// v-prefixed) tag name, "v2.0.0-3-gabcd" style. It reports false when the string
// carries no release tag at all (a plain commit hash, for instance).
func MajorOf(version string) (int, bool) {
	_, rest, ok := strings.Cut(version, "v")
	if !ok {
		rest = version
	}
	major, tail, ok := numbers(rest)
	if !ok || (tail != "" && !strings.HasPrefix(tail, ".")) {
		return 0, false
	}
	return major, true
}

// SelectTag picks the newest tag of the running version's major that the channel
// allows, from the tag names in any order. stable counts releases only;
// prerelease counts everything. It reports false when nothing matches
// (the wrapper's no-release state).
func SelectTag(tags []string, runningVersion, channel string) (string, bool) {
	major, ok := MajorOf(runningVersion)
	if !ok {
		return "", false
	}
	best := ""
	for _, name := range tags {
		tag, ok := ParseTag(name)
		if !ok || tag.Major != major {
			continue
		}
		if channel == Stable && tag.Prerelease {
			continue
		}
		if best == "" || CompareTags(name, best) > 0 {
			best = name
		}
	}
	return best, best != ""
}

// DefaultChannel is the computed channel when nothing is set: prerelease while
// the remote has no stable tag of the running major, otherwise stable.
func DefaultChannel(tags []string, runningVersion string) string {
	major, ok := MajorOf(runningVersion)
	if !ok {
		return Stable
	}
	for _, name := range tags {
		if tag, ok := ParseTag(name); ok && tag.Major == major && !tag.Prerelease {
			return Stable
		}
	}
	return Prerelease
}

// TestTags is the ordered (oldest first) tag table the unit tests and the
// bin/aimebu wrapper tests both build their remotes from, so the bash ordering
// cannot drift from this package's.
var TestTags = []string{
	"v1.9.0",
	"v2.0.0",
	"v2.1.0-alpha.9",
	"v2.1.0-alpha.10",
	"v2.1.0-beta.1",
	"v2.1.0-rc.1",
	"v2.1.0",
	"v3.0.0-alpha.1",
}
