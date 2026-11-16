package server

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/telemetry"
)

func TestHandledHTTP500LogsAndReportsTheScrubbedErrorChain(t *testing.T) {
	transport := &captureTransport{}
	if _, err := telemetry.Init(telemetry.Config{DSN: "https://public@example.invalid/1", Environment: "test", TracesSampleRate: 1}, transport); err != nil {
		t.Fatal(err)
	}
	defer telemetry.Current().Close()
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	root := errors.New("database token=verysecret failed")
	handler := http.NewServeMux()
	handler.HandleFunc("POST /api/rooms/{room}/agents", func(w http.ResponseWriter, _ *http.Request) {
		fail(w, fmt.Errorf("spawn agent: %w", root))
	})
	response := httptest.NewRecorder()
	telemetry.Middleware(handler).ServeHTTP(response, httptest.NewRequest("POST", "/api/rooms/room1/agents", nil))
	if response.Code != 500 {
		t.Fatalf("status = %d", response.Code)
	}
	if text := logged.String(); !strings.Contains(text, "spawn agent:") || !strings.Contains(text, "database token=[redacted] failed") || strings.Contains(text, "verysecret") {
		t.Fatalf("server log lost cause or leaked token: %q", text)
	}
	telemetry.Current().Flush(time.Second)
	var captured, generic bool
	transport.mu.Lock()
	for _, event := range transport.events {
		if strings.HasPrefix(event.Message, "HTTP 500") {
			generic = true
		}
		for _, exception := range event.Exception {
			if strings.Contains(exception.Value, "spawn agent") && strings.Contains(exception.Value, "database token=[redacted] failed") {
				captured = true
			}
			if strings.Contains(exception.Value, "verysecret") {
				t.Errorf("Sentry leaked token: %q", exception.Value)
			}
		}
	}
	transport.mu.Unlock()
	if !captured || generic {
		t.Fatalf("underlying error captured=%t generic report=%t", captured, generic)
	}
}
