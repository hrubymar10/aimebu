package supervisor

import (
	"fmt"
	"strings"
	"time"

	"github.com/hrubymar10/aimebu/internal/adapter"
)

const (
	// TurnIdleEnv bounds silence from a running harness turn: no adapter
	// event, and no tool call in flight, for this long.
	TurnIdleEnv = "AIMEBU_TURN_IDLE"
	// TurnMaxEnv is the wall-clock cap for one turn, tools included.
	TurnMaxEnv = "AIMEBU_TURN_MAX"

	defaultTurnIdle = 600 * time.Second
	defaultTurnMax  = 30 * time.Minute
)

// TurnLimits are the stall deadlines enforced by Client.runTurn for one turn.
// A zero field disables that deadline.
type TurnLimits struct {
	Idle time.Duration
	Max  time.Duration
}

// DefaultTurnLimits returns the per-harness defaults. Pi keeps v1's watchdog
// (600s idle, 30m overall). Claude Code and Codex had no bound before, and a
// legitimate turn there can run for hours, so only the idle deadline applies
// by default; it is suspended while a tool call is in flight.
func DefaultTurnLimits(tool string) TurnLimits {
	if tool == "pi" {
		return TurnLimits{Idle: defaultTurnIdle, Max: defaultTurnMax}
	}
	return TurnLimits{Idle: defaultTurnIdle}
}

// TurnLimitOverrides are the owner's AIMEBU_TURN_* settings. A nil field
// leaves that deadline at its per-harness default.
type TurnLimitOverrides struct {
	Idle *time.Duration
	Max  *time.Duration
}

// For resolves the limits for one harness tool.
func (o TurnLimitOverrides) For(tool string) TurnLimits {
	limits := DefaultTurnLimits(tool)
	if o.Idle != nil {
		limits.Idle = *o.Idle
	}
	if o.Max != nil {
		limits.Max = *o.Max
	}
	return limits
}

// TurnLimitsFromEnv reads AIMEBU_TURN_IDLE / AIMEBU_TURN_MAX. Values are
// non-negative Go durations; "0" disables that deadline for every harness.
func TurnLimitsFromEnv(lookup func(string) string) (TurnLimitOverrides, error) {
	var overrides TurnLimitOverrides
	for _, item := range []struct {
		name   string
		target **time.Duration
	}{{TurnIdleEnv, &overrides.Idle}, {TurnMaxEnv, &overrides.Max}} {
		raw := strings.TrimSpace(lookup(item.name))
		if raw == "" {
			continue
		}
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed < 0 {
			return TurnLimitOverrides{}, fmt.Errorf("%s must be a non-negative Go duration (for example 600s or 30m; 0 disables)", item.name)
		}
		*item.target = &parsed
	}
	return overrides, nil
}

type turnStall string

const (
	turnStallIdle turnStall = "idle"
	turnStallMax  turnStall = "max"
)

func (s turnStall) describe(limits TurnLimits) string {
	if s == turnStallMax {
		return fmt.Sprintf("turn exceeded the %s wall-clock limit (%s)", limits.Max, TurnMaxEnv)
	}
	return fmt.Sprintf("turn produced no output for %s (%s)", limits.Idle, TurnIdleEnv)
}

// turnWatch tracks when a turn's idle deadline falls due. Tool executions in
// flight suspend the idle deadline (a long tool call is legitimately silent);
// only the wall-clock cap bounds them. Pure and clock-injected so the policy
// is testable without timers.
type turnWatch struct {
	limits  TurnLimits
	started time.Time
	last    time.Time
	tools   map[string]int
}

func newTurnWatch(limits TurnLimits, now time.Time) *turnWatch {
	return &turnWatch{limits: limits, started: now, last: now, tools: map[string]int{}}
}

// touch records liveness seen outside the event stream (raw stdout lines).
func (w *turnWatch) touch(at time.Time) {
	if at.After(w.last) {
		w.last = at
	}
}

func (w *turnWatch) observe(tools []adapter.ToolEvent, now time.Time) {
	w.last = now
	for _, tool := range tools {
		if !tool.Done {
			w.tools[tool.ID]++
		} else if w.tools[tool.ID] > 0 {
			w.tools[tool.ID]--
			if w.tools[tool.ID] == 0 {
				delete(w.tools, tool.ID)
			}
		}
	}
}

// next returns the earliest pending deadline and its reason; ok is false when
// no deadline applies.
func (w *turnWatch) next() (deadline time.Time, reason turnStall, ok bool) {
	if w.limits.Max > 0 {
		deadline, reason, ok = w.started.Add(w.limits.Max), turnStallMax, true
	}
	if w.limits.Idle > 0 && len(w.tools) == 0 {
		idle := w.last.Add(w.limits.Idle)
		if !ok || idle.Before(deadline) {
			deadline, reason, ok = idle, turnStallIdle, true
		}
	}
	return deadline, reason, ok
}
