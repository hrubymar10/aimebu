package telemetry

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/getsentry/sentry-go"
)

// The parts of an error text that name a thing rather than a cause: an id, a request id, a timestamp, an e-mail address, a hex digest, a path (Unix or
// Windows), a count. They are replaced before the text is keyed or sent, so one cause is one issue whichever agent, turn, folder or moment it happened
// to, and no id or path is in the event. The patterns catch what has a recognisable shape; a NAME (an account, a profile) has none, so an error that can
// carry one is reported with ReportCause, a fixed phrase, instead.
var (
	idLike    = regexp.MustCompile(`\b(?:agt|turn|msg|rm|prj|mch|mln|usr|tfm|evt|ses|inb|cmd|att|mem|role|key|req)_[0-9A-Za-z]{6,}\b`)
	timeLike  = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?`)
	emailLike = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+`)
	hexLike   = regexp.MustCompile(`\b[0-9a-fA-F]{12,}\b`)
	winPath   = regexp.MustCompile(`\b[A-Za-z]:\\[^\s:;,"'()\[\]]+`)
	pathLike  = regexp.MustCompile(`(?:~|\.{1,2})?/[^\s:;,"'()\[\]]{2,}`)
	numLike   = regexp.MustCompile(`\b\d{3,}\b`)
)

// maxClass bounds the part of the text a key is made of.
const maxClass = 120

// Classify names an error's cause for reporting: the key (the area and the error text with ids, timestamps, e-mail addresses, hex digests, paths and counts
// replaced, its first line, bounded) and an error carrying that same redacted text. Equal causes give equal keys.
func Classify(area string, err error) (string, error) {
	if err == nil {
		return "", nil
	}
	text := err.Error()
	if line, _, found := strings.Cut(text, "\n"); found {
		text = line
	}
	text = idLike.ReplaceAllString(text, "<id>")
	text = timeLike.ReplaceAllString(text, "<time>")
	text = emailLike.ReplaceAllString(text, "<email>")
	text = hexLike.ReplaceAllString(text, "<hex>")
	text = winPath.ReplaceAllString(text, "<path>")
	text = pathLike.ReplaceAllString(text, "<path>")
	text = numLike.ReplaceAllString(text, "<n>")
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) > maxClass {
		text = string([]rune(text)[:maxClass]) + "…"
	}
	return area + ": " + text, errors.New(text)
}

// ReportError sends an error the program catches and carries on from (a failed write it retries, a frame it refuses, a refresh it skips) to Sentry
// as an error, once per cause per day. `area` is a fixed name of the place ("supervisor", "machine", "dispatcher", "credentials"); it is the issue's
// `area` tag and the start of its key. Nothing happens while telemetry is off, and the event carries no id, path or count.
func ReportError(ctx context.Context, area string, err error) {
	report(ctx, area, sentry.LevelError, err)
}

// ReportWarning is ReportError for a degraded state the program recovers from by itself (a fallback, a retry that will succeed).
func ReportWarning(ctx context.Context, area string, err error) {
	report(ctx, area, sentry.LevelWarning, err)
}

// ReportCause is ReportError for an error whose text may carry what must not leave: an account or profile name, a person's file, a harness's own output.
// `cause` is a fixed phrase of the caller's, and it alone (with the area) is the event's message and key; the error itself is not sent.
func ReportCause(ctx context.Context, area, cause string) {
	reportCause(ctx, area, cause, sentry.LevelError)
}

// WarnCause is ReportCause for a degraded state the program recovers from by itself.
func WarnCause(ctx context.Context, area, cause string) {
	reportCause(ctx, area, cause, sentry.LevelWarning)
}

func report(ctx context.Context, area string, level sentry.Level, err error) {
	// A connection that went away (the request or the process context was cancelled) is not a failure of ours.
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return
	}
	key, redacted := Classify(area, err)
	send(ctx, area, key, level, redacted)
}

func reportCause(ctx context.Context, area, cause string, level sentry.Level) {
	if cause == "" || ctx.Err() != nil {
		return
	}
	send(ctx, area, area+": "+cause, level, errors.New(cause))
}

func send(ctx context.Context, area, key string, level sentry.Level, err error) {
	t := Current()
	if t == nil || area == "" || err == nil {
		return
	}
	if !firstTime(key, time.Now()) {
		return
	}
	scope := scopeFor(ctx)
	scope.SetLevel(level)
	scope.SetTag("area", area)
	scope.SetFingerprint([]string{key})
	t.hub.Client().CaptureException(err, &sentry.EventHint{Context: ctx}, scope)
}
