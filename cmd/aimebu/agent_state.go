package main

import (
	"bytes"
	"context"
	"io"
	"sync"
	"time"
)

const agentStatePipeBuffer = 64 * 1024

var agentStatePushMinInterval = 300 * time.Millisecond

// stateDetector inspects a single line (or byte chunk) of harness output
// and reports the agent's current state. It returns "" when no state can be
// inferred from the input. The pusher only sends changed states.
type stateDetector interface {
	Detect(line []byte) string
	Name() string
}

// newStateDetector returns a per-harness detector. A nil detector means the
// wrapper does not push active state for this harness.
func newStateDetector(harness string) stateDetector {
	switch harness {
	case "claude-code":
		return claudeCodeStateDetector{}
	case "codex":
		return codexStateDetector{}
	case "pi":
		return &piStateDetector{}
	}
	return nil
}

type agentStateStream struct {
	mu     sync.Mutex
	ch     chan []byte
	closed bool
}

func newAgentStateStream() *agentStateStream {
	return &agentStateStream{ch: make(chan []byte, agentStatePipeBuffer)}
}

func (s *agentStateStream) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	cp := append([]byte(nil), p...)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return len(p), nil
	}
	select {
	case s.ch <- cp:
	default:
	}
	return len(p), nil
}

func (s *agentStateStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	close(s.ch)
	return nil
}

// agentIDProvider lets bootstrap sessions start classifying harness output
// before bus_register has resolved the final full agent ID.
type agentIDProvider struct {
	mu sync.RWMutex
	id string
}

func newAgentIDProvider(agentID string) *agentIDProvider {
	p := &agentIDProvider{}
	p.Set(agentID)
	return p
}

func (p *agentIDProvider) Get() string {
	if p == nil {
		return ""
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.id
}

func (p *agentIDProvider) Set(agentID string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.id = agentID
	p.mu.Unlock()
}

func startAgentStatePusher(ctx context.Context, aimebuURL string, agentID *agentIDProvider, det stateDetector) io.WriteCloser {
	if det == nil {
		return discardWriteCloser{}
	}
	stream := newAgentStateStream()
	go agentStatePusher(ctx, aimebuURL, agentID, det, stream.ch)
	return stream
}

type discardWriteCloser struct{}

func (discardWriteCloser) Write(p []byte) (int, error) {
	return len(p), nil
}

func (discardWriteCloser) Close() error {
	return nil
}

// agentStatePusher reads harness output, classifies complete lines with det,
// and pushes changed states to the bus. State changes inside the debounce
// window are coalesced with the latest state winning.
func agentStatePusher(ctx context.Context, aimebuURL string, agentID *agentIDProvider, det stateDetector, in <-chan []byte) {
	if det == nil {
		for {
			select {
			case _, ok := <-in:
				if !ok {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}

	var pendingLine []byte
	controller := newAgentStatePushController(aimebuURL, agentID, agentStatePushMinInterval)
	var timer *time.Timer
	var timerC <-chan time.Time
	stopTimer := func() {
		if timer == nil {
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer = nil
		timerC = nil
	}
	defer stopTimer()

	schedule := func(delay time.Duration) {
		if delay <= 0 {
			delay = time.Nanosecond
		}
		if timer == nil {
			timer = time.NewTimer(delay)
		} else {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(delay)
		}
		timerC = timer.C
	}

	handleLine := func(line []byte) {
		state := det.Detect(bytes.TrimRight(line, "\r"))
		delay, needsTimer := controller.Observe(state)
		if needsTimer {
			schedule(delay)
		}
	}

	handleChunk := func(chunk []byte) {
		for len(chunk) > 0 {
			idx := bytes.IndexByte(chunk, '\n')
			if idx < 0 {
				pendingLine = append(pendingLine, chunk...)
				return
			}
			pendingLine = append(pendingLine, chunk[:idx]...)
			handleLine(pendingLine)
			pendingLine = pendingLine[:0]
			chunk = chunk[idx+1:]
		}
	}

	for {
		select {
		case chunk, ok := <-in:
			if !ok {
				if len(pendingLine) > 0 {
					handleLine(pendingLine)
				}
				controller.Flush()
				return
			}
			handleChunk(chunk)
		case <-timerC:
			if controller.Flush() {
				if delay, ok := controller.NextDelay(); ok {
					schedule(delay)
				} else {
					stopTimer()
				}
			} else {
				stopTimer()
			}
		case <-ctx.Done():
			return
		}
	}
}

type agentStatePushController struct {
	mu        sync.Mutex
	aimebuURL string
	agentID   *agentIDProvider
	min       time.Duration
	lastPush  time.Time
	lastState string
	pending   string
}

func newAgentStatePushController(aimebuURL string, agentID *agentIDProvider, min time.Duration) *agentStatePushController {
	return &agentStatePushController{
		aimebuURL: aimebuURL,
		agentID:   agentID,
		min:       min,
	}
}

func (c *agentStatePushController) Observe(state string) (time.Duration, bool) {
	if state == "" {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if state == c.lastState || state == c.pending {
		return 0, c.pending != ""
	}
	now := time.Now()
	if c.agentID.Get() == "" {
		c.pending = state
		return c.min, true
	}
	if c.lastPush.IsZero() || now.Sub(c.lastPush) >= c.min {
		c.pushLocked(state, now)
		return 0, false
	}
	c.pending = state
	return c.min - now.Sub(c.lastPush), true
}

func (c *agentStatePushController) Flush() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending == "" || c.pending == c.lastState {
		c.pending = ""
		return false
	}
	if c.agentID.Get() == "" {
		c.lastPush = time.Now()
		return true
	}
	c.pushLocked(c.pending, time.Now())
	c.pending = ""
	return true
}

func (c *agentStatePushController) NextDelay() (time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending == "" {
		return 0, false
	}
	delay := c.min - time.Since(c.lastPush)
	if delay < 0 {
		delay = 0
	}
	return delay, true
}

func (c *agentStatePushController) pushLocked(state string, now time.Time) {
	agentID := c.agentID.Get()
	if agentID == "" {
		c.pending = state
		return
	}
	c.lastState = state
	c.lastPush = now
	agentPushState(c.aimebuURL, agentID, state)
}
