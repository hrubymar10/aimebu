package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	agentProgressIdleEnv      = "AIMEBU_AGENT_STALL_IDLE"
	agentProgressForwardEnv   = "AIMEBU_AGENT_STALL_PROGRESS"
	agentProgressBusWaitBase  = 30 * time.Second
	agentProgressBusWaitMax   = 600 * time.Second
	agentProgressBusWaitGrace = 60 * time.Second
	agentProgressTurnEndGrace = 90 * time.Second
)

var agentDefaultProgressConfig = agentProgressConfig{
	IdleTimeout:    600 * time.Second,
	ForwardTimeout: 30 * time.Minute,
}

type agentProgressConfig struct {
	IdleTimeout    time.Duration
	ForwardTimeout time.Duration
}

func agentProgressConfigFromLookup(lookup func(string) string) (agentProgressConfig, error) {
	config := agentDefaultProgressConfig
	values := []struct {
		name   string
		target *time.Duration
	}{
		{name: agentProgressIdleEnv, target: &config.IdleTimeout},
		{name: agentProgressForwardEnv, target: &config.ForwardTimeout},
	}
	for _, value := range values {
		raw := strings.TrimSpace(lookup(value.name))
		if raw == "" {
			continue
		}
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			return agentProgressConfig{}, fmt.Errorf("%s must be a positive Go duration (for example 600s or 30m)", value.name)
		}
		*value.target = parsed
	}
	return config, nil
}

type agentProgressStall string

const (
	agentProgressStallIdle    agentProgressStall = "pi_idle_stalled"
	agentProgressStallForward agentProgressStall = "pi_progress_stalled"
	agentProgressStallBusWait agentProgressStall = "pi_bus_wait_stalled"
	agentProgressStallTurnEnd agentProgressStall = "pi_turn_end_stalled"
)

func agentProgressRecoveryClass(reason agentProgressStall) agentRecoveryClass {
	switch reason {
	case agentProgressStallIdle:
		return agentRecoveryPiIdleStalled
	case agentProgressStallForward:
		return agentRecoveryPiProgressStalled
	case agentProgressStallBusWait:
		return agentRecoveryPiBusWaitStalled
	case agentProgressStallTurnEnd:
		return agentRecoveryPiTurnEndStalled
	default:
		return agentRecoveryResumeStalled
	}
}

func agentProgressStallFromClass(class string) agentProgressStall {
	switch agentRecoveryClass(class) {
	case agentRecoveryPiIdleStalled:
		return agentProgressStallIdle
	case agentRecoveryPiProgressStalled:
		return agentProgressStallForward
	case agentRecoveryPiBusWaitStalled:
		return agentProgressStallBusWait
	case agentRecoveryPiTurnEndStalled:
		return agentProgressStallTurnEnd
	default:
		return ""
	}
}

type agentProgressSnapshot struct {
	InTurn           bool
	ToolsInFlight    int
	BusWaitsInFlight int
	LastOutput       time.Time
	LastProgress     time.Time
	BusWaitDeadline  time.Time
	TurnEndedAt      time.Time
}

func (snapshot agentProgressSnapshot) observe(event piProgressEvent, now time.Time) agentProgressSnapshot {
	switch event.Type {
	case "turn_start":
		if snapshot.LastProgress.IsZero() {
			snapshot.LastProgress = now
		}
		snapshot.InTurn = true
		snapshot.ToolsInFlight = 0
		snapshot.BusWaitsInFlight = 0
		snapshot.BusWaitDeadline = time.Time{}
		snapshot.TurnEndedAt = time.Time{}
	case "tool_execution_start":
		snapshot.InTurn = true
		snapshot.TurnEndedAt = time.Time{}
		snapshot.ToolsInFlight++
		snapshot.LastProgress = now
		if piToolIsBusWait(event.ToolName) {
			snapshot.BusWaitsInFlight++
			deadline := now.Add(piBusWaitTimeout(event.Args) + agentProgressBusWaitGrace)
			if deadline.After(snapshot.BusWaitDeadline) {
				snapshot.BusWaitDeadline = deadline
			}
		}
	case "tool_execution_end":
		if snapshot.ToolsInFlight > 0 {
			snapshot.ToolsInFlight--
		}
		if piToolIsBusWait(event.ToolName) && snapshot.BusWaitsInFlight > 0 {
			snapshot.BusWaitsInFlight--
			if snapshot.BusWaitsInFlight == 0 {
				snapshot.BusWaitDeadline = time.Time{}
			}
		}
	case "turn_end":
		snapshot.InTurn = false
		snapshot.ToolsInFlight = 0
		snapshot.BusWaitsInFlight = 0
		snapshot.BusWaitDeadline = time.Time{}
		snapshot.LastProgress = now
		snapshot.TurnEndedAt = now
	}
	return snapshot
}

// agentProgressDeadline is deliberately pure so deadline policy can be
// exhaustively tested without timers or child processes.
func agentProgressDeadline(snapshot agentProgressSnapshot, config agentProgressConfig, now time.Time) (agentProgressStall, time.Time) {
	if snapshot.ToolsInFlight > 0 {
		if snapshot.ToolsInFlight == snapshot.BusWaitsInFlight && !snapshot.BusWaitDeadline.IsZero() {
			if !now.Before(snapshot.BusWaitDeadline) {
				return agentProgressStallBusWait, snapshot.BusWaitDeadline
			}
			return "", snapshot.BusWaitDeadline
		}
		return "", time.Time{}
	}
	if !snapshot.InTurn {
		if !snapshot.TurnEndedAt.IsZero() {
			deadline := snapshot.TurnEndedAt.Add(agentProgressTurnEndGrace)
			if !now.Before(deadline) {
				return agentProgressStallTurnEnd, deadline
			}
			return "", deadline
		}
		return "", time.Time{}
	}

	idleDeadline := snapshot.LastOutput.Add(config.IdleTimeout)
	progressDeadline := snapshot.LastProgress.Add(config.ForwardTimeout)
	deadline := idleDeadline
	reason := agentProgressStallIdle
	if progressDeadline.Before(deadline) {
		deadline = progressDeadline
		reason = agentProgressStallForward
	}
	if !now.Before(deadline) {
		return reason, deadline
	}
	return "", deadline
}

type piProgressEvent struct {
	Type     string         `json:"type"`
	ToolName string         `json:"toolName"`
	Args     map[string]any `json:"args"`
}

func parsePiProgressEvent(line []byte) (piProgressEvent, bool) {
	var event piProgressEvent
	if err := json.Unmarshal(bytes.TrimSpace(line), &event); err != nil {
		return piProgressEvent{}, false
	}
	if event.Type == "" {
		return piProgressEvent{}, false
	}
	return event, true
}

func piToolIsBusWait(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return name == "bus_wait" || strings.HasSuffix(name, "_bus_wait")
}

func piBusWaitTimeout(args map[string]any) time.Duration {
	seconds := float64(agentProgressBusWaitBase / time.Second)
	if raw, ok := args["timeout"]; ok {
		switch value := raw.(type) {
		case float64:
			seconds = value
		case json.Number:
			if parsed, err := value.Float64(); err == nil {
				seconds = parsed
			}
		case string:
			if parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
				seconds = parsed
			}
		}
	}
	if seconds <= 0 {
		seconds = float64(agentProgressBusWaitBase / time.Second)
	}
	timeout := time.Duration(seconds * float64(time.Second))
	if timeout > agentProgressBusWaitMax {
		timeout = agentProgressBusWaitMax
	}
	return timeout
}

type agentProgressMonitor struct {
	mu      sync.Mutex
	config  agentProgressConfig
	now     func() time.Time
	pending []byte
	state   agentProgressSnapshot
	wake    chan struct{}
	stalled chan agentProgressStall
	done    chan struct{}
	once    sync.Once
	expired bool
}

func newAgentProgressMonitor(config agentProgressConfig) *agentProgressMonitor {
	monitor := &agentProgressMonitor{
		config:  config,
		now:     time.Now,
		wake:    make(chan struct{}, 1),
		stalled: make(chan agentProgressStall, 1),
		done:    make(chan struct{}),
	}
	go monitor.run()
	return monitor
}

func (m *agentProgressMonitor) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	now := m.now()
	m.mu.Lock()
	m.state.LastOutput = now
	m.pending = append(m.pending, p...)
	for {
		index := bytes.IndexByte(m.pending, '\n')
		if index < 0 {
			break
		}
		line := append([]byte(nil), m.pending[:index]...)
		m.pending = m.pending[index+1:]
		m.observeLineLocked(line, now)
	}
	m.mu.Unlock()
	m.signalWake()
	return len(p), nil
}

func (m *agentProgressMonitor) observeLineLocked(line []byte, now time.Time) {
	if !piWatchdogEventLine(line) {
		return
	}
	event, ok := parsePiProgressEvent(line)
	if !ok {
		return
	}
	m.state = m.state.observe(event, now)
}

func piWatchdogEventLine(line []byte) bool {
	return bytes.Contains(line, []byte(`"turn_start"`)) ||
		bytes.Contains(line, []byte(`"turn_end"`)) ||
		bytes.Contains(line, []byte(`"tool_execution_start"`)) ||
		bytes.Contains(line, []byte(`"tool_execution_end"`))
}

func (m *agentProgressMonitor) signalWake() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *agentProgressMonitor) snapshot() agentProgressSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *agentProgressMonitor) run() {
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		now := m.now()
		reason, deadline := agentProgressDeadline(m.snapshot(), m.config, now)
		if reason != "" {
			m.mu.Lock()
			m.expired = true
			m.mu.Unlock()
			select {
			case m.stalled <- reason:
			default:
			}
			return
		}

		var timerC <-chan time.Time
		if !deadline.IsZero() {
			delay := deadline.Sub(now)
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

		select {
		case <-m.wake:
		case <-timerC:
		case <-m.done:
			return
		}
	}
}

func (m *agentProgressMonitor) Stalled() <-chan agentProgressStall {
	if m == nil {
		return nil
	}
	return m.stalled
}

func (m *agentProgressMonitor) WithinBudget() bool {
	if m == nil {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.expired
}

func (m *agentProgressMonitor) Close() error {
	if m == nil {
		return nil
	}
	m.once.Do(func() {
		close(m.done)
	})
	return nil
}

var _ io.WriteCloser = (*agentProgressMonitor)(nil)
