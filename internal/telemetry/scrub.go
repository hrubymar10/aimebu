package telemetry

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/getsentry/sentry-go"
)

// maxText bounds any free text that is sent (an error message, a span description): an error that quotes a message body stays short.
const maxText = 300

// A secret that an error text may quote: an Authorization value, a bearer token, or key=value for a name that says it is secret.
var (
	bearerText = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]+`)
	// The bare tokens aimebu and its providers issue (agent keys, machine keys, sessions, API keys) wherever they appear in a text.
	bareToken = regexp.MustCompile(`\b(?:aak_|amk_|as_|sk-ant-|sk-)[A-Za-z0-9_-]{8,}`)
	// A home directory of whoever built or runs the binary, in any spelling: it does not have to be this process's HOME (a build machine's
	// checkout, a test's temporary HOME, a service user).
	homeLike   = regexp.MustCompile(`(?:/Users|/home)/[^/\s:"']+|[A-Za-z]:\\Users\\[^\\\s:"']+`)
	secretText = regexp.MustCompile(`(?i)\b([A-Za-z0-9_-]*(?:token|secret|password|passwd|cookie|authorization|api[_-]?key|dsn)[A-Za-z0-9_-]*)(\s*[=:]\s*)("[^"]*"|'[^']*'|\S+)`)
)

// Scrubber removes from an event everything that must not leave the machine. Its method set is what both Sentry hooks call.
type Scrubber struct {
	// Home is the home directory that stack frames and messages show as "~". Empty means no replacement.
	Home string
	// ModuleRoot is the directory this program's source was built from (the embedded path of internal/telemetry/scrub.go, minus that suffix): a
	// frame inside it is shown relative to it ("internal/server/server.go"), so the path of a checkout never leaves, wherever it was. GoRoot is the
	// Go toolchain's, shown as "$GOROOT". Either may be empty (a -trimpath build has no absolute paths to begin with).
	ModuleRoot, GoRoot string
}

// NewScrubber returns the scrubber for this process: the current user's home directory becomes "~".
func NewScrubber() Scrubber {
	home, _ := os.UserHomeDir()
	scrubber := Scrubber{Home: home, GoRoot: runtime.GOROOT()}
	if _, file, _, ok := runtime.Caller(0); ok {
		if root, found := strings.CutSuffix(filepath.ToSlash(file), "internal/telemetry/scrub.go"); found {
			scrubber.ModuleRoot = root
		}
	}
	return scrubber
}

// URL keeps the path of a URL: no scheme, host, user info, query or fragment. Something that is not a URL gives "".
func (Scrubber) URL(raw string) string {
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		if cut := strings.IndexAny(raw, "?#"); cut >= 0 {
			raw = raw[:cut]
		}
		if strings.HasPrefix(raw, "/") {
			return raw
		}
		return ""
	}
	return parsed.EscapedPath()
}

// Text scrubs one free-text value: the home directory shown as "~", quoted secrets redacted, and the length bounded.
func (s Scrubber) Text(text string) string {
	text = s.path(text)
	text = bareToken.ReplaceAllString(text, "[redacted]")
	text = bearerText.ReplaceAllString(text, "$1 [redacted]")
	text = secretText.ReplaceAllString(text, "$1$2[redacted]")
	if len(text) > maxText {
		text = strings.ToValidUTF8(text[:maxText], "") + "…"
	}
	return text
}

// A span's data keys that carry no content: everything else is dropped, so a new attribute cannot leak by being added elsewhere.
var allowedSpanData = map[string]bool{
	"db.system": true, "db.operation": true, "db.rows": true, "db.statement": true,
	"http.request.method": true, "http.response.status_code": true, "http.route": true,
}

// Event scrubs an error or transaction event in place and returns it. It is both BeforeSend and BeforeSendTransaction.
func (s Scrubber) Event(event *sentry.Event) *sentry.Event {
	if event == nil {
		return nil
	}
	// Who and where: no user, no host name, no request detail beyond the method and the path.
	event.User, event.ServerName, event.Modules = sentry.User{}, "", nil
	delete(event.Tags, "server_name")
	if event.Request != nil {
		event.Request = &sentry.Request{Method: event.Request.Method, URL: s.URL(event.Request.URL)}
	}
	// Only the contexts that describe the platform and the trace.
	for name := range event.Contexts {
		switch name {
		case "trace", "os", "runtime", "browser":
		default:
			delete(event.Contexts, name)
		}
	}
	// The root span's data is in the trace context; the same allow-list applies to it.
	if trace, ok := event.Contexts["trace"]; ok {
		if data, ok := trace["data"].(map[string]interface{}); ok {
			for key := range data {
				if !allowedSpanData[key] {
					delete(data, key)
				}
			}
		}
	}
	event.Message = s.Text(event.Message)
	event.Transaction = s.Text(strings.TrimSpace(event.Transaction))
	for index := range event.Exception {
		exception := &event.Exception[index]
		exception.Value = s.Text(exception.Value)
		if exception.Stacktrace != nil {
			s.frames(exception.Stacktrace.Frames)
		}
	}
	for _, thread := range event.Threads {
		if thread.Stacktrace != nil {
			s.frames(thread.Stacktrace.Frames)
		}
	}
	kept := event.Breadcrumbs[:0]
	for _, crumb := range event.Breadcrumbs {
		if crumb == nil || crumb.Category == "console" {
			continue
		}
		crumb.Message, crumb.Data = s.Text(crumb.Message), nil
		kept = append(kept, crumb)
	}
	event.Breadcrumbs = kept
	for _, span := range event.Spans {
		if span == nil {
			continue
		}
		span.Description, span.Name = s.Text(span.Description), s.Text(span.Name)
		for key := range span.Data {
			if !allowedSpanData[key] {
				delete(span.Data, key)
			}
		}
		if statement, ok := span.Data["db.statement"].(string); ok {
			span.Data["db.statement"] = s.Text(statement)
		}
		span.Tags = nil
	}
	return event
}

func (s Scrubber) frames(frames []sentry.Frame) {
	for index := range frames {
		frame := &frames[index]
		frame.AbsPath, frame.Filename, frame.Module = s.path(frame.AbsPath), s.path(frame.Filename), s.path(frame.Module)
		// Local variables are values: never sent.
		frame.Vars, frame.PreContext, frame.ContextLine, frame.PostContext = nil, nil, "", nil
	}
}

// path rewrites every place a path can name the machine: this program's source root becomes relative, the Go root "$GOROOT", the home directory
// "~" (this process's, then any other user's home by shape).
func (s Scrubber) path(value string) string {
	if len(s.ModuleRoot) > 1 {
		value = strings.ReplaceAll(value, s.ModuleRoot, "")
	}
	if len(s.GoRoot) > 1 {
		value = strings.ReplaceAll(value, s.GoRoot, "$GOROOT")
	}
	if s.Home != "" && len(s.Home) > 1 {
		value = strings.ReplaceAll(value, s.Home, "~")
	}
	return homeLike.ReplaceAllString(value, "~")
}
