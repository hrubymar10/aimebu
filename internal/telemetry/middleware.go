package telemetry

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/getsentry/sentry-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

var errHandlerAbort = http.ErrAbortHandler

// statusWriter remembers the status code. It passes Flush and Hijack through (the event stream and the machine WebSocket need them) and
// exposes the wrapped writer to http.ResponseController.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(data)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if w.status == 0 {
		w.status = http.StatusSwitchingProtocols
	}
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

// tracedPath says whether a request is worth a trace of its own: the API, and the pages the app is opened by (the shell, which the same handler
// serves as "/" and "/index.html", and the first-run setup). Everything else the catch-all route answers is a file of the app: the ~240 scripts, styles
// and icons of one page load. Traced, each is a transaction named "GET /", a page load makes hundreds of them, and they were 60% of a server's
// transactions with nothing to learn from any of them. (The reasons a request is not traced: a streaming one, see Middleware, and this.) A panic or
// a 5xx in a file is still reported: only the trace is left out. The path is only read here, never recorded.
func tracedPath(path string) bool {
	return strings.HasPrefix(path, "/api/") || path == "/" || path == "/index.html" || path == "/setup"
}

// Middleware wraps h (a request passes straight through while reporting is off): a panic is reported (and then re-raised, so net/http answers it as it always did), a 5xx answer is reported, and
// each API request and page is a trace span ("http.server") carrying the method, the route pattern and the status; never the path, the query, a header or
// a body. A static file of the app is not traced (see tracedPath), nor is a long-lived request (an event stream, a WebSocket upgrade): its span would last
// as long as the connection.
func Middleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := Current() // read per request: Settings can turn reporting on or off while the server runs
		if t == nil {
			h.ServeHTTP(w, r)
			return
		}
		writer := &statusWriter{ResponseWriter: w}
		ctx := r.Context()
		var span trace.Span
		streaming := r.Header.Get("Upgrade") != "" || strings.Contains(r.Header.Get("Accept"), "text/event-stream")
		if !streaming && tracedPath(r.URL.Path) {
			// Named by the route pattern once the mux has matched it; the raw path never goes in (it can hold names a person chose).
			ctx, span = t.tracer.Start(ctx, r.Method+" (unrouted)", trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(attribute.String(OpKey, "http.server"), attribute.String("http.request.method", r.Method)))
		}
		slot := &userSlot{}
		ctx = context.WithValue(ctx, userSlotKey{}, slot)
		budget := &spanBudget{}
		if span != nil {
			ctx = context.WithValue(ctx, spanBudgetKey{}, budget)
		}
		request := r.WithContext(ctx)
		defer func() {
			value := recover()
			route := request.Pattern
			if route == "" {
				route = r.Method + " (unrouted)"
			}
			status := writer.status
			if value != nil && status == 0 {
				status = http.StatusInternalServerError
			}
			if span != nil {
				span.SetAttributes(attribute.String("http.route", route), attribute.Int("http.response.status_code", status))
				if id := slot.get(); id != "" {
					span.SetAttributes(attribute.String(UserKey, trimUserID(id)))
				}
				if dropped := budget.dropped.Load(); dropped > 0 {
					span.SetAttributes(attribute.Int64("db.calls", budget.calls.Load()), attribute.Int64("db.calls_dropped", dropped))
				}
				span.SetName(route)
				span.End()
			}
			if value != nil {
				t.CapturePanic(ctx, value)
				panic(value)
			}
			if status >= 500 {
				scope := scopeFor(ctx)
				scope.SetTag("http.route", route)
				scope.SetTag("http.status_code", fmt.Sprint(status))
				scope.SetLevel(sentry.LevelError)
				t.hub.Client().CaptureMessage("HTTP "+fmt.Sprint(status)+" "+route, &sentry.EventHint{Context: ctx}, scope)
			}
		}()
		h.ServeHTTP(writer, request)
	})
}
