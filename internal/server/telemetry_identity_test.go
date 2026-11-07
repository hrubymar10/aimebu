package server

import (
	"context"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/telemetry"
)

type captureTransport struct {
	mu     sync.Mutex
	events []*sentry.Event
}

func (*captureTransport) Flush(time.Duration) bool              { return true }
func (*captureTransport) FlushWithContext(context.Context) bool { return true }
func (*captureTransport) Configure(sentry.ClientOptions)        {}
func (c *captureTransport) SendEvent(event *sentry.Event) {
	c.mu.Lock()
	c.events = append(c.events, event)
	c.mu.Unlock()
}
func (*captureTransport) Close() {}
func (c *captureTransport) users(route string) []sentry.User {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []sentry.User
	for _, event := range c.events {
		if event.Type == "transaction" && event.Transaction == route {
			out = append(out, event.User)
		}
	}
	return out
}

func identityCall(t *testing.T, f *fixture, method, path string, body any) (int, telemetryIdentityView) {
	t.Helper()
	resp := f.request(method, path, body, method != "GET")
	defer resp.Body.Close()
	var view telemetryIdentityView
	_ = json.NewDecoder(resp.Body).Decode(&view)
	return resp.StatusCode, view
}

// A person's fingerprint is random and lazy, their login replaces it only by their choice (never both), a roll gives a new anonymous id, and the
// reports of their requests carry that one id and nothing else.
func TestTelemetryIdentityAPIAndReports(t *testing.T) {
	f := newFixture(t) // not parallel: the reporter is process-wide
	hex16 := regexp.MustCompile(`^[0-9a-f]{16}$`)
	status, first := identityCall(t, f, "GET", "/api/me/telemetry", nil)
	if status != 200 || !hex16.MatchString(first.Fingerprint) || first.SendName || first.Login != "owner" || first.ReportID != first.Fingerprint {
		t.Fatalf("first read: %d %+v", status, first)
	}
	if _, again := identityCall(t, f, "GET", "/api/me/telemetry", nil); again.Fingerprint != first.Fingerprint {
		t.Fatal("the fingerprint is stable")
	}
	if status, _ := identityCall(t, f, "PUT", "/api/me/telemetry", map[string]any{}); status != 400 {
		t.Fatalf("send_name is required: %d", status)
	}

	transport := &captureTransport{}
	if _, err := telemetry.Init(telemetry.Config{DSN: "https://public@example.invalid/1", Environment: "test", Release: "v1", TracesSampleRate: 1}, transport); err != nil {
		t.Fatal(err)
	}
	defer telemetry.Current().Close()
	buildinfoID := func() string {
		resp := f.request("GET", "/api/buildinfo", nil, false)
		defer resp.Body.Close()
		var info struct {
			Sentry map[string]any `json:"sentry"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&info)
		id, _ := info.Sentry["user_id"].(string)
		return id
	}
	if got := buildinfoID(); got != first.Fingerprint {
		t.Fatalf("the page is told the id its reports carry: %q", got)
	}
	lastUser := func() sentry.User {
		t.Helper()
		telemetry.Current().Flush(time.Second)
		users := transport.users("GET /api/me/telemetry")
		if len(users) == 0 {
			t.Fatal("no transaction for the identity request")
		}
		return users[len(users)-1]
	}
	identityCall(t, f, "GET", "/api/me/telemetry", nil)
	if user := lastUser(); user.ID != first.Fingerprint || user.Email != "" || user.Username != "" || user.IPAddress != "" || user.Name != "" {
		t.Fatalf("transactions name the person by the fingerprint only: %+v", user)
	}

	status, named := identityCall(t, f, "PUT", "/api/me/telemetry", map[string]any{"send_name": true})
	if status != 200 || !named.SendName || named.ReportID != "owner" || named.Fingerprint != first.Fingerprint {
		t.Fatalf("send_name on: %d %+v", status, named)
	}
	if got := buildinfoID(); got != "owner" {
		t.Fatalf("on: the login, never both: %q", got)
	}
	identityCall(t, f, "GET", "/api/me/telemetry", nil)
	if user := lastUser(); user.ID != "owner" || user.Username != "" {
		t.Fatalf("with the name on, the id is the login (user.id), and nothing else: %+v", user)
	}

	status, rolled := identityCall(t, f, "POST", "/api/me/telemetry/fingerprint", map[string]any{})
	if status != 200 || rolled.Fingerprint == first.Fingerprint || !hex16.MatchString(rolled.Fingerprint) || rolled.SendName || rolled.ReportID != rolled.Fingerprint {
		t.Fatalf("a roll: a new anonymous id, the name turned off: %d %+v", status, rolled)
	}
	identityCall(t, f, "GET", "/api/me/telemetry", nil)
	if user := lastUser(); user.ID != rolled.Fingerprint {
		t.Fatalf("the next reports carry the new fingerprint: %+v", user)
	}
}
