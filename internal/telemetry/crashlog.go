package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/getsentry/sentry-go"
)

// A fatal error (a `fatal error: fault`, a SIGSEGV or SIGBUS in native code, an out-of-memory throw) is a runtime
// throw: the program cannot recover from it, so a panic hook never sees it and it used to die without anything in
// Sentry. The runtime can instead also write its crash report into a file (`debug.SetCrashOutput`), and the next
// start reads that file and reports it. The server and the standalone supervisor install the file at
// `<config root>/server/crash.log`; see docs/telemetry.md. Recording is local and needs no consent; only the report
// of a crash a previous run left waits for the reporting decision (the environment's DSN, else Settings) and is
// skipped entirely while reporting is off — the rotated file keeps the trace on disk either way.
const (
	// CrashLogName is the crash file's name under the config root's `server` directory.
	CrashLogName = "crash.log"
	// crashKeepLogs is how many rotated crash files (crash-<timestamp>.log) a root keeps.
	crashKeepLogs = 5
	// maxCrashReport bounds the trace sent to Sentry: a runtime dump can be megabytes, and the throwing goroutine is
	// at its top, so the head of the report is what matters.
	maxCrashReport = 256 << 10
)

// CrashLogPath is the crash output file of the config root root.
func CrashLogPath(root string) string { return filepath.Join(root, "server", CrashLogName) }

// InstallCrashOutput makes the runtime write its fatal-error report into the config root's server/crash.log
// (0600), in addition to standard error. A non-empty file a previous run left is first renamed to
// crash-<timestamp>.log (the newest crashKeepLogs kept); reporting it is the caller's job (ReportCrashFile) once
// the reporting decision is final, which the returned path names ("" when there was nothing to report).
// SetCrashOutput duplicates the file descriptor, so the file is closed here. An error is the caller's to report
// (a start goes on without a crash file rather than fail for one).
func InstallCrashOutput(root string) (string, error) {
	path := CrashLogPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	previous := ""
	if info, err := os.Stat(path); err == nil && info.Size() > 0 {
		rotated, err := renameCrashLog(path)
		if err != nil {
			return "", err
		}
		previous = rotated
		pruneCrashLogs(filepath.Dir(path))
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return previous, err
	}
	if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		f.Close()
		return previous, err
	}
	return previous, f.Close()
}

// renameCrashLog moves the crash file to crash-<UTC timestamp>.log beside it, with a -2, -3 … suffix when a file of
// that second is already there.
func renameCrashLog(path string) (string, error) {
	stamp := time.Now().UTC().Format("20060102-150405")
	dir := filepath.Dir(path)
	for attempt := 1; ; attempt++ {
		name := fmt.Sprintf("crash-%s.log", stamp)
		if attempt > 1 {
			name = fmt.Sprintf("crash-%s-%d.log", stamp, attempt)
		}
		target := filepath.Join(dir, name)
		if _, err := os.Stat(target); err == nil {
			continue
		}
		return target, os.Rename(path, target)
	}
}

// pruneCrashLogs keeps only the newest crashKeepLogs rotated crash files of a directory.
func pruneCrashLogs(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, entry := range entries {
		if name := entry.Name(); strings.HasPrefix(name, "crash-") && strings.HasSuffix(name, ".log") {
			names = append(names, name)
		}
	}
	// The timestamp in the name sorts lexically oldest first.
	sort.Strings(names)
	for _, name := range names[:max(0, len(names)-crashKeepLogs)] {
		_ = os.Remove(filepath.Join(dir, name))
	}
}

// hexAddr matches a hexadecimal address as the runtime prints it in a crash's first line ("unexpected fault address
// 0x1068eea38"): one crash kind is one issue, whatever the address was. Classify's own patterns pass a value this
// shape through, because it sits inside a word.
var hexAddr = regexp.MustCompile(`0[xX][0-9a-fA-F]{4,}`)

// ReportCrashFile reports a rotated crash file (the path InstallCrashOutput returned) to Sentry: the first line of
// the runtime report as the event's message ("crash-log: fatal error: fault"), the scrubbed trace as an attachment.
// Nothing happens while reporting is off, or for an empty or missing path: the trace stays on disk either way.
// The event is flushed before it returns, so a start that fails soon after still sends it.
func ReportCrashFile(ctx context.Context, path string) error {
	t := Current()
	if t == nil || path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	trace := scrubTrace(t.scrub, strings.ToValidUTF8(string(raw), ""))
	if strings.TrimSpace(trace) == "" {
		return nil
	}
	first, _, _ := strings.Cut(trace, "\n")
	key, title := Classify("crash-log", errors.New(hexAddr.ReplaceAllString(strings.TrimSpace(first), "<addr>")))
	scope := sentry.NewScope()
	scope.SetLevel(sentry.LevelFatal)
	scope.SetTag("area", "crash-log")
	scope.SetFingerprint([]string{key})
	event := &sentry.Event{
		Level:   sentry.LevelFatal,
		Message: title.Error(),
		Attachments: []*sentry.Attachment{{
			Filename: CrashLogName, ContentType: "text/plain",
			Payload: []byte(trimRunes(trace, maxCrashReport)),
		}},
	}
	t.hub.Client().CaptureEvent(event, &sentry.EventHint{Context: ctx}, scope)
	t.Flush(10 * time.Second)
	return nil
}

// scrubTrace removes from a runtime crash report what must not leave the machine (the home directory and the source
// roots, any bearer or bare token) without the 300-character bound of Text: the trace is the event's whole point,
// bounded by maxCrashReport at the caller instead.
func scrubTrace(s Scrubber, trace string) string {
	trace = s.path(trace)
	trace = bareToken.ReplaceAllString(trace, "[redacted]")
	trace = bearerText.ReplaceAllString(trace, "$1 [redacted]")
	trace = secretText.ReplaceAllString(trace, "$1$2[redacted]")
	return trace
}

// trimRunes cuts a string to at most limit bytes without splitting a character.
func trimRunes(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}
