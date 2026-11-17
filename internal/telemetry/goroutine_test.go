package telemetry

import (
	"context"
	"testing"
)

// A panic under Recover is reported (fatal) and raised again, so a goroutine that panics still takes the process down as before.
func TestRecoverReportsAndRepanics(t *testing.T) {
	_, transport := startTest(t)
	var raised any
	func() {
		defer func() { raised = recover() }()
		func() {
			defer Recover(context.Background())
			panic("boom")
		}()
	}()
	if raised != "boom" {
		t.Fatalf("the panic was not raised again: %v", raised)
	}
	events := transport.sent()
	if len(events) != 1 || events[0].Level != "fatal" {
		t.Fatalf("want one fatal event, got %d: %+v", len(events), events)
	}
}

func TestGoRunsTheFunction(t *testing.T) {
	done := make(chan struct{})
	Go(context.Background(), func() { close(done) })
	<-done
}
