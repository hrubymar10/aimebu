package telemetry

import (
	"context"
	"sync"

	"github.com/getsentry/sentry-go"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// OpKey is the span attribute that names a span's Sentry operation ("http.server", "db.query").
const OpKey = "aimebu.op"

const (
	maxSpansPerTrace = 200
	maxOpenTraces    = 500
)

// spanProcessor turns finished OpenTelemetry spans into Sentry transactions. sentry-go's own OpenTelemetry module only links errors to
// traces (its span processor went away when Sentry took OTLP instead), and an OTLP exporter is a dependency this does not take; so the
// conversion is here: spans are held per trace until the local root ends, then sent as one transaction.
type spanProcessor struct {
	hub  *sentry.Hub
	mu   sync.Mutex
	open map[oteltrace.TraceID][]*sentry.Span
}

func newSpanProcessor(hub *sentry.Hub) *spanProcessor {
	return &spanProcessor{hub: hub, open: make(map[oteltrace.TraceID][]*sentry.Span)}
}

func (*spanProcessor) OnStart(context.Context, trace.ReadWriteSpan) {}

func (*spanProcessor) Shutdown(context.Context) error   { return nil }
func (*spanProcessor) ForceFlush(context.Context) error { return nil }

func (p *spanProcessor) OnEnd(span trace.ReadOnlySpan) {
	converted := convert(span)
	traceID := span.SpanContext().TraceID()
	root := !span.Parent().IsValid() || span.Parent().IsRemote()
	p.mu.Lock()
	if !root {
		if len(p.open) >= maxOpenTraces && p.open[traceID] == nil {
			p.open = make(map[oteltrace.TraceID][]*sentry.Span) // traces whose root never ended: start over rather than grow
		}
		if len(p.open[traceID]) < maxSpansPerTrace {
			p.open[traceID] = append(p.open[traceID], converted)
		}
		p.mu.Unlock()
		return
	}
	children := p.open[traceID]
	delete(p.open, traceID)
	p.mu.Unlock()
	event := &sentry.Event{
		Type: "transaction", Transaction: span.Name(), Level: sentry.LevelInfo, Platform: "go",
		StartTime: span.StartTime(), Timestamp: span.EndTime(), Spans: children,
		Contexts: map[string]sentry.Context{"trace": sentry.TraceContext{
			TraceID: sentry.TraceID(converted.TraceID), SpanID: converted.SpanID, ParentSpanID: converted.ParentSpanID,
			Op: converted.Op, Description: converted.Description, Status: converted.Status, Data: converted.Data,
		}.Map()},
	}
	// Straight to the client with no scope: a scope writes its own propagation context over the event's trace context, which would drop this
	// transaction's trace id, operation, route and status and orphan its child spans.
	p.hub.Client().CaptureEvent(event, nil, nil)
}

func convert(span trace.ReadOnlySpan) *sentry.Span {
	context := span.SpanContext()
	out := &sentry.Span{
		TraceID: sentry.TraceID(context.TraceID()), SpanID: sentry.SpanID(context.SpanID()), ParentSpanID: sentry.SpanID(span.Parent().SpanID()),
		Description: span.Name(), Status: sentry.SpanStatusOK, StartTime: span.StartTime(), EndTime: span.EndTime(), Data: map[string]interface{}{}, Origin: "manual",
	}
	for _, attribute := range span.Attributes() {
		if string(attribute.Key) == OpKey {
			out.Op = attribute.Value.AsString()
			continue
		}
		out.Data[string(attribute.Key)] = attribute.Value.AsInterface()
	}
	if out.Op == "" {
		out.Op = "function"
	}
	if span.Status().Code == codes.Error {
		out.Status = sentry.SpanStatusInternalError
	}
	if status, ok := out.Data["http.response.status_code"].(int64); ok && status >= 500 {
		out.Status = sentry.SpanStatusInternalError
	}
	return out
}
