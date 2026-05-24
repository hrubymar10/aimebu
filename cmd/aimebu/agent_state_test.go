package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type sequenceStateDetector struct {
	name   string
	states map[string]string
}

func (d sequenceStateDetector) Detect(line []byte) string {
	return d.states[string(line)]
}

func (d sequenceStateDetector) Name() string {
	return d.name
}

func TestAgentStatePusher_PushesOnChangeOnly(t *testing.T) {
	restore := setAgentStatePushMinIntervalForTest(20 * time.Millisecond)
	defer restore()

	capture := newAgentStateCapture(t)
	defer capture.Close()

	in := make(chan []byte, 4)
	det := sequenceStateDetector{
		name: "test",
		states: map[string]string{
			"think": "thinking",
			"idle":  "idle",
		},
	}
	go agentStatePusher(context.Background(), capture.URL(), newAgentIDProvider("worker@aimebu"), det, in)

	in <- []byte("think\n")
	in <- []byte("think\n")
	if got := capture.WaitStates(t, 1, time.Second); len(got) != 1 || got[0] != "thinking" {
		t.Fatalf("states after duplicate thinking = %v, want [thinking]", got)
	}

	time.Sleep(agentStatePushMinInterval + 10*time.Millisecond)
	in <- []byte("idle\n")
	got := capture.WaitStates(t, 2, time.Second)
	if want := []string{"thinking", "idle"}; !equalStringSlices(got, want) {
		t.Fatalf("states = %v, want %v", got, want)
	}
	close(in)
}

func TestAgentStatePusher_DebouncesWithinWindow(t *testing.T) {
	restore := setAgentStatePushMinIntervalForTest(60 * time.Millisecond)
	defer restore()

	capture := newAgentStateCapture(t)
	defer capture.Close()

	in := make(chan []byte, 8)
	det := sequenceStateDetector{
		name: "test",
		states: map[string]string{
			"think": "thinking",
			"tool":  "tool_call",
			"idle":  "idle",
		},
	}
	go agentStatePusher(context.Background(), capture.URL(), newAgentIDProvider("worker@aimebu"), det, in)

	in <- []byte("think\n")
	if got := capture.WaitStates(t, 1, time.Second); len(got) != 1 || got[0] != "thinking" {
		t.Fatalf("initial states = %v, want [thinking]", got)
	}
	in <- []byte("tool\n")
	in <- []byte("idle\n")

	got := capture.WaitStates(t, 2, time.Second)
	if want := []string{"thinking", "idle"}; !equalStringSlices(got, want) {
		t.Fatalf("states = %v, want %v", got, want)
	}
	if delta := capture.Delta(0, 1); delta < agentStatePushMinInterval {
		t.Fatalf("push delta = %v, want >= %v", delta, agentStatePushMinInterval)
	}
	close(in)
}

func TestAgentStatePusher_NilDetectorIsNoop(t *testing.T) {
	capture := newAgentStateCapture(t)
	defer capture.Close()

	in := make(chan []byte, 2)
	ctx, cancel := context.WithCancel(context.Background())
	go agentStatePusher(ctx, capture.URL(), newAgentIDProvider("worker@aimebu"), nil, in)
	in <- []byte("thinking\n")
	in <- []byte("idle\n")
	time.Sleep(30 * time.Millisecond)
	cancel()

	if got := capture.States(); len(got) != 0 {
		t.Fatalf("states = %v, want none", got)
	}
}

func TestAgentStatePusher_BuffersUntilAgentIDIsSet(t *testing.T) {
	restore := setAgentStatePushMinIntervalForTest(30 * time.Millisecond)
	defer restore()

	capture := newAgentStateCapture(t)
	defer capture.Close()

	in := make(chan []byte, 8)
	agentID := newAgentIDProvider("")
	det := sequenceStateDetector{
		name: "test",
		states: map[string]string{
			"tool":  "tool_call",
			"think": "thinking",
		},
	}
	go agentStatePusher(context.Background(), capture.URL(), agentID, det, in)

	in <- []byte("tool\n")
	in <- []byte("think\n")
	time.Sleep(2 * agentStatePushMinInterval)
	if got := capture.States(); len(got) != 0 {
		t.Fatalf("states before agent ID = %v, want none", got)
	}

	agentID.Set("worker@aimebu")
	got := capture.WaitStates(t, 1, time.Second)
	if len(got) != 1 || got[0] != "thinking" {
		t.Fatalf("states after agent ID = %v, want exactly [thinking]", got)
	}
	close(in)
}

func TestAgentStatePushController_EmptyAgentIDKeepsRetryCadence(t *testing.T) {
	min := 50 * time.Millisecond
	controller := newAgentStatePushController("http://example.invalid", newAgentIDProvider(""), min)

	delay, needsTimer := controller.Observe("thinking")
	if !needsTimer || delay != min {
		t.Fatalf("Observe = (%v, %v), want (%v, true)", delay, needsTimer, min)
	}
	time.Sleep(min + 5*time.Millisecond)

	if !controller.Flush() {
		t.Fatal("Flush returned false, want true while pending state waits for an agent ID")
	}
	delay, needsTimer = controller.NextDelay()
	if !needsTimer {
		t.Fatal("NextDelay needsTimer = false, want true")
	}
	if delay <= 0 || delay > min {
		t.Fatalf("NextDelay = %v, want within (0, %v]", delay, min)
	}
}

func TestNewStateDetector_UnknownHarness(t *testing.T) {
	if got := newStateDetector("foobar"); got != nil {
		t.Fatalf("newStateDetector returned %T, want nil", got)
	}
}

func setAgentStatePushMinIntervalForTest(d time.Duration) func() {
	old := agentStatePushMinInterval
	agentStatePushMinInterval = d
	return func() { agentStatePushMinInterval = old }
}

type agentStateCapture struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	states []string
	times  []time.Time
}

func newAgentStateCapture(t *testing.T) *agentStateCapture {
	t.Helper()
	c := &agentStateCapture{t: t}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/agents/worker@aimebu/state" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		c.mu.Lock()
		c.states = append(c.states, body["state"])
		c.times = append(c.times, time.Now())
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	return c
}

func (c *agentStateCapture) URL() string {
	return c.srv.URL
}

func (c *agentStateCapture) Close() {
	c.srv.Close()
}

func (c *agentStateCapture) States() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.states...)
}

func (c *agentStateCapture) Delta(i, j int) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.times[j].Sub(c.times[i])
}

func (c *agentStateCapture) WaitStates(t *testing.T, n int, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		got := c.States()
		if len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	return c.States()
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
