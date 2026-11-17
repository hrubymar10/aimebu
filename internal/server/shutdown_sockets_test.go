package server

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/hrubymar10/aimebu/internal/store"
	"github.com/hrubymar10/aimebu/internal/telemetry"
)

func TestEventStreamShutdownClosesBeforeStore(t *testing.T) {
	f := newFixture(t) // the Sentry reporter is process-wide
	f.seed()
	transport := &captureTransport{}
	if _, err := telemetry.Init(telemetry.Config{DSN: "https://public@example.invalid/1", Environment: "test", Release: "shutdown", TracesSampleRate: 0}, transport); err != nil {
		t.Fatal(err)
	}
	defer telemetry.Current().Close()
	_, high, err := f.db.EventBounds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	conn := f.dial(t, high, f.http.URL)
	defer conn.CloseNow()
	readFrame(t, conn) // the poller has started
	entered, release := make(chan struct{}), make(chan struct{})
	releasePoll := sync.OnceFunc(func() { close(release) })
	defer releasePoll()
	f.srv.EventBatchQuery = func(context.Context, string, int64, int64, int) ([]store.Event, error) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release // emulate a query that finishes after shutdown starts
		return nil, sql.ErrConnDone
	}
	response := f.request("POST", "/api/rooms/"+f.room.ID+"/messages", map[string]string{"body": "wake poll"}, true)
	response.Body.Close()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("event poll did not start")
	}
	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		shutdownDone <- f.srv.ShutdownSockets(ctx)
	}()
	readCtx, cancelRead := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelRead()
	_, _, err = conn.Read(readCtx)
	if websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("event stream close = %v, want 1001", err)
	}
	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown finished before the poller: %v", err)
	default:
	}
	releasePoll()
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not join the event poller")
	}
	telemetry.Current().Flush(time.Second)
	transport.mu.Lock()
	defer transport.mu.Unlock()
	for _, event := range transport.events {
		if event.Level == "error" {
			t.Fatalf("normal shutdown reported an error: %+v", event)
		}
	}
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMachineSocketShutdownSendsGoingAway(t *testing.T) {
	f := newFixture(t)
	conn := f.dialMachine(t, nil)
	defer conn.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.srv.ShutdownSockets(ctx) }()
	_, _, err := conn.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("machine close = %v, want 1001", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
