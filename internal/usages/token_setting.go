package usages

import (
	"context"
	"sync"
	"time"

	"github.com/hrubymar10/aimebu/internal/usages/providers"
)

// TokenAutoRefreshToolState is one tool's line of the "Auto-refresh tokens"
// setting: what the lifecycle controller last did for it.
//
//	off       the tool is on the link model (or the setting is off)
//	migrated  the server owns the tool's refresh token
//	pending   a change was requested and the controller has not answered yet
//	waiting   a safety refusal (token about to expire, codex running, ...) that is
//	          retried; Reason says which
//	n/a       the tool has no active profile, so there is nothing to convert
type TokenAutoRefreshToolState struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// tokenSettingWaitingBackoff spaces the retries of a waiting tool. A toggle
// bypasses it.
const (
	tokenSettingWaitingBackoff = time.Minute
	tokenSettingTick           = 5 * time.Second
)

// TokenController applies the setting: on converts each tool to the token-copy
// model, off returns it to the link model. It returns the per-tool states. It
// lives behind a function because the work needs the switcher, which this package
// cannot import.
type TokenController func(ctx context.Context, on bool) map[string]TokenAutoRefreshToolState

// TokenSetting owns the lifecycle of the `token_auto_refresh` setting. Applying it
// can block (the account lock, the codex process scan), so it runs on its own
// worker, never on a request goroutine or the usage poller: a single worker is also
// the single-flight guard, so a tick and a toggle can never run the controller at
// the same time. A change is answered immediately with `pending` and the real
// states arrive when the worker finishes; the UI re-polls until they settle.
type TokenSetting struct {
	store      *providers.Store
	controller TokenController
	seed       func(tool string) bool
	available  func() bool
	now        func() time.Time

	request chan struct{}

	mu          sync.RWMutex
	state       map[string]TokenAutoRefreshToolState
	epoch       uint64 // bumped by every change; a pass for an older epoch is dropped
	lastEpoch   uint64
	lastAttempt time.Time
}

// NewTokenSetting builds the lifecycle. seed says whether a tool is already
// migrated (the displayed state before the first pass), available whether the
// refresh executor can run at all.
func NewTokenSetting(store *providers.Store, controller TokenController, seed func(tool string) bool, available func() bool) *TokenSetting {
	return &TokenSetting{
		store:      store,
		controller: controller,
		seed:       seed,
		available:  available,
		now:        time.Now,
		request:    make(chan struct{}, 1),
	}
}

// SetClock replaces the clock the waiting backoff is judged by (tests).
func (t *TokenSetting) SetClock(now func() time.Time) { t.now = now }

// Run applies the setting once at start, then on every Request and every tick,
// until ctx ends. It blocks.
func (t *TokenSetting) Run(ctx context.Context) {
	ticker := time.NewTicker(tokenSettingTick)
	defer ticker.Stop()
	t.Request()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.request:
			t.reconcile(ctx)
		case <-ticker.C:
			t.reconcile(ctx)
		}
	}
}

// Request asks for a pass soon. One queued pass is enough: each pass reads the
// latest config.
func (t *TokenSetting) Request() {
	select {
	case t.request <- struct{}{}:
	default:
	}
}

// Changed is called after the setting was saved with a new value: the displayed
// state becomes `pending` and a pass is requested right away.
func (t *TokenSetting) Changed() {
	t.mu.Lock()
	t.epoch++
	t.state = map[string]TokenAutoRefreshToolState{"claude": {Status: "pending"}, "codex": {Status: "pending"}}
	t.mu.Unlock()
	t.Request()
}

// Available reports whether the refresh executor can run.
func (t *TokenSetting) Available() bool { return t.available == nil || t.available() }

// States is what the settings response shows. Before the first pass it is seeded
// from the migration markers: a migrated tool is migrated, whatever the worker has
// not yet confirmed.
func (t *TokenSetting) States(on bool) map[string]TokenAutoRefreshToolState {
	t.mu.RLock()
	out := make(map[string]TokenAutoRefreshToolState, 2)
	for tool, state := range t.state {
		out[tool] = state
	}
	t.mu.RUnlock()
	if len(out) > 0 {
		return out
	}
	for _, tool := range []string{"claude", "codex"} {
		migrated := t.seed != nil && t.seed(tool)
		status := "off"
		switch {
		case on && migrated:
			status = "migrated"
		case on:
			status = "pending"
		}
		out[tool] = TokenAutoRefreshToolState{Status: status}
	}
	return out
}

func (t *TokenSetting) setOffState(epoch uint64) {
	t.mu.Lock()
	if t.epoch == epoch {
		t.state = map[string]TokenAutoRefreshToolState{"claude": {Status: "off"}, "codex": {Status: "off"}}
	}
	t.mu.Unlock()
}

// reconcile is one pass: read the setting, run the controller for it and record
// the states. It is only ever called from Run's worker.
func (t *TokenSetting) reconcile(ctx context.Context) {
	if t.controller == nil {
		return
	}
	t.mu.RLock()
	epoch := t.epoch
	t.mu.RUnlock()
	cfg, err := t.store.LoadConfig()
	if err != nil {
		return
	}
	if cfg.TokenAutoRefresh && !cfg.TokenAutoRefreshManaged {
		// The setting is on: from now on it owns cleanup of what it migrates.
		if _, err := t.store.UpdateConfig(func(c *providers.Config) error {
			if c.TokenAutoRefresh {
				c.TokenAutoRefreshManaged = true
			}
			return nil
		}); err != nil {
			return
		}
	}
	if !cfg.TokenAutoRefresh && !cfg.TokenAutoRefreshManaged {
		// Off and never owned: a manual migrate-copies is none of our business.
		t.setOffState(epoch)
		return
	}
	now := t.now()
	t.mu.RLock()
	waiting := t.state["claude"].Status == "waiting" || t.state["codex"].Status == "waiting"
	backoff := waiting && t.lastEpoch == epoch && now.Sub(t.lastAttempt) < tokenSettingWaitingBackoff
	t.mu.RUnlock()
	if backoff {
		return
	}
	states := t.controller(ctx, cfg.TokenAutoRefresh)
	t.mu.Lock()
	if t.epoch == epoch {
		t.state = states
		t.lastEpoch = epoch
		t.lastAttempt = now
	}
	t.mu.Unlock()
	if !cfg.TokenAutoRefresh && states["claude"].Status == "off" && states["codex"].Status == "off" {
		// Every tool is back on the link model: the setting no longer owns anything.
		_, _ = t.store.UpdateConfig(func(c *providers.Config) error {
			if !c.TokenAutoRefresh {
				c.TokenAutoRefreshManaged = false
			}
			return nil
		})
	}
}
