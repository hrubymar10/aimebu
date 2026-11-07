package telemetry

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	sentryotel "github.com/getsentry/sentry-go/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Telemetry is the running reporter. A nil *Telemetry is the disabled one: every method is safe on it and does nothing, so callers never
// check whether telemetry is on.
type Telemetry struct {
	cfg      Config
	scrub    Scrubber
	hub      *sentry.Hub
	provider *sdktrace.TracerProvider
	tracer   trace.Tracer
}

var current atomic.Pointer[Telemetry]

// Current is the process's reporter set by Init; nil when telemetry is off.
func Current() *Telemetry { return current.Load() }

// Init starts the reporter for cfg and makes it Current, replacing (and flushing) the one that was running. A config without a DSN turns
// reporting off and returns (nil, nil): nothing is created.
// transport replaces the network transport (tests); nil sends to the DSN.
func Init(cfg Config, transport sentry.Transport) (*Telemetry, error) {
	if !cfg.Enabled() {
		if old := current.Swap(nil); old != nil {
			old.Close()
		}
		return nil, nil
	}
	if err := ValidDSN(cfg.DSN); err != nil {
		return nil, fmt.Errorf("sentry: %w", err)
	}
	// Saving Settings applies the same config again: keep the running reporter instead of starting a second one.
	if old := current.Load(); old != nil && transport == nil && old.cfg == cfg {
		return old, nil
	}
	if transport == nil && testing.Testing() {
		// The real transport opens a network connection; a test that wants reporting passes its own transport.
		return nil, errors.New("telemetry: refusing to open the network transport in a test binary")
	}
	scrub := NewScrubber()
	options := sentry.ClientOptions{
		Dsn: cfg.DSN, Environment: cfg.Environment, Release: cfg.Release,
		// Never the default PII (user, IP, cookies, headers), never a host name; the hooks below are the second wall behind these.
		SendDefaultPII: false, ServerName: "-", AttachStacktrace: true, MaxBreadcrumbs: 20, Transport: transport,
		BeforeSend:            func(event *sentry.Event, _ *sentry.EventHint) *sentry.Event { return scrub.Event(event) },
		BeforeSendTransaction: func(event *sentry.Event, _ *sentry.EventHint) *sentry.Event { return scrub.Event(event) },
		BeforeBreadcrumb: func(crumb *sentry.Breadcrumb, _ *sentry.BreadcrumbHint) *sentry.Breadcrumb {
			if crumb == nil || crumb.Category == "console" {
				return nil
			}
			crumb.Data, crumb.Message = nil, scrub.Text(crumb.Message)
			return crumb
		},
		Integrations: func(defaults []sentry.Integration) []sentry.Integration {
			// The defaults include the one that adds the process environment and the command line to every event; none of them is wanted.
			return []sentry.Integration{sentryotel.NewOtelIntegration()}
		},
	}
	client, err := sentry.NewClient(options)
	if err != nil {
		return nil, fmt.Errorf("sentry: %w", err)
	}
	hub := sentry.NewHub(client, sentry.NewScope())
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.TracesSampleRate))),
		sdktrace.WithSpanProcessor(newSpanProcessor(hub)),
	)
	t := &Telemetry{cfg: cfg, scrub: scrub, hub: hub, provider: provider, tracer: provider.Tracer("aimebu")}
	if old := current.Swap(t); old != nil {
		old.Close() // Settings changed while running: the reporter it replaces is flushed and stopped
	}
	return t, nil
}

// Enabled says whether t reports anything.
func (t *Telemetry) Enabled() bool { return t != nil }

// Propagator is the W3C trace-context propagator, for a caller that continues a trace from a request header.
func (t *Telemetry) Propagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})
}

// Flush waits up to timeout for queued events to be sent.
func (t *Telemetry) Flush(timeout time.Duration) {
	if t == nil {
		return
	}
	_ = t.provider.ForceFlush(context.Background())
	t.hub.Flush(timeout)
}

// Close flushes and stops the reporter.
func (t *Telemetry) Close() {
	if t == nil {
		return
	}
	t.Flush(2 * time.Second)
	_ = t.provider.Shutdown(context.Background())
	if client := t.hub.Client(); client != nil {
		client.Close() // stops the transport's goroutine
	}
	current.CompareAndSwap(t, nil)
}

// CaptureError reports an error, linked to the trace of the request in ctx (the OTel resolver of the Sentry integration reads it from the hint).
func (t *Telemetry) CaptureError(ctx context.Context, err error) {
	if t == nil || err == nil {
		return
	}
	t.hub.Client().CaptureException(err, &sentry.EventHint{Context: ctx}, scopeFor(ctx))
}

// CapturePanic reports a recovered panic value, linked to its request's trace, and flushes, since the process may be about to die.
func (t *Telemetry) CapturePanic(ctx context.Context, value any) {
	if t == nil || value == nil || errors.Is(asError(value), errHandlerAbort) {
		return
	}
	scope := scopeFor(ctx)
	scope.SetLevel(sentry.LevelFatal)
	t.hub.Client().RecoverWithContext(ctx, value, &sentry.EventHint{Context: ctx}, scope)
	t.Flush(2 * time.Second)
}

func asError(value any) error {
	err, _ := value.(error)
	return err
}

// Recover is for `defer telemetry.Recover(ctx)` at a goroutine's entry: it reports the panic, flushes, and panics again, so the process
// dies exactly as it would without telemetry.
func Recover(ctx context.Context) {
	if value := recover(); value != nil {
		Current().CapturePanic(ctx, value)
		panic(value)
	}
}

// BrowserConfig is what the page needs to start its SDK (served inside GET /api/buildinfo); nil when telemetry is off. The DSN is public by
// design (it only allows sending events), and the page reads it only after sign-in.
func (t *Telemetry) BrowserConfig() map[string]any {
	if t == nil {
		return nil
	}
	return map[string]any{
		"dsn": t.cfg.DSN, "environment": t.cfg.Environment, "release": t.cfg.Release,
		"traces_sample_rate": t.cfg.TracesSampleRate,
	}
}
