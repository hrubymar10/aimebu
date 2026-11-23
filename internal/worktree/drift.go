package worktree

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// driftTimeout bounds each of the two git calls BaseDrift makes.
const driftTimeout = 5 * time.Second

// maxDriftSubjectRunes caps one commit subject shown to an agent.
const maxDriftSubjectRunes = 120

// Drift says how far the tip of a room's base ref has moved past the commit its worktree was made from.
type Drift struct {
	Behind   int      // commits on the base ref that the worktree's base commit lacks
	Subjects []string // the newest of them (at most subjectCount), newest first, already sanitised
}

// ErrNoDrift is returned when the drift cannot be told: the base ref or the base commit does not resolve, or git failed.
var ErrNoDrift = errors.New("worktree: base drift unknown")

// BaseDrift counts the commits reachable from baseRef but not from baseCommit in the repository of dir (a room worktree shares its refs),
// and, when that count reaches min, reads the subjects of the newest subjectCount of them. Both calls are bounded by a timeout, run through
// GitCommand and read only. Below min it answers a Drift with no subjects; an unresolvable ref or commit answers ErrNoDrift.
func BaseDrift(ctx context.Context, dir, baseRef, baseCommit string, min, subjectCount int) (Drift, error) {
	if baseRef == "" || !isHexCommit(baseCommit) || strings.ContainsAny(baseRef, "\x00\n") || strings.HasPrefix(baseRef, "-") {
		return Drift{}, ErrNoDrift
	}
	rng := baseCommit + ".." + baseRef
	countCtx, cancel := context.WithTimeout(ctx, driftTimeout)
	defer cancel()
	cmd := GitCommand(countCtx, dir, "rev-list", "--count", rng, "--")
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return Drift{}, ErrNoDrift
	}
	behind, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || behind < 0 {
		return Drift{}, ErrNoDrift
	}
	drift := Drift{Behind: behind}
	if behind < min || behind == 0 || subjectCount <= 0 {
		return drift, nil
	}
	logCtx, cancelLog := context.WithTimeout(ctx, driftTimeout)
	defer cancelLog()
	cmd = GitCommand(logCtx, dir, "log", "-"+strconv.Itoa(subjectCount), "--format=%s%x00", rng, "--")
	cmd.WaitDelay = time.Second
	out, err = cmd.Output()
	if err != nil {
		return Drift{}, ErrNoDrift
	}
	for _, subject := range strings.Split(string(out), "\x00") {
		if subject = CleanSubject(subject); subject != "" {
			drift.Subjects = append(drift.Subjects, subject)
		}
	}
	return drift, nil
}

func isHexCommit(s string) bool {
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

// CleanSubject makes a commit subject safe to show an agent: it is untrusted text, so control, format and invalid bytes are dropped,
// whitespace runs collapse to one space, and the length is capped.
func CleanSubject(s string) string {
	var b strings.Builder
	space := false
	n := 0
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		if r == utf8.RuneError && size <= 1 {
			continue
		}
		if unicode.IsSpace(r) {
			space = b.Len() > 0
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
			n++
		}
		if n >= maxDriftSubjectRunes {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}
