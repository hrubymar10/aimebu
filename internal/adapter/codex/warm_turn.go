package codex

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/pricing"
)

type codexTokenUsage struct {
	Input   int64 `json:"inputTokens"`
	Output  int64 `json:"outputTokens"`
	Cached  int64 `json:"cachedInputTokens"`
	Written int64 `json:"cacheWriteInputTokens"`
}

func (u codexTokenUsage) minus(before codexTokenUsage) codexTokenUsage {
	return codexTokenUsage{max(0, u.Input-before.Input), max(0, u.Output-before.Output),
		max(0, u.Cached-before.Cached), max(0, u.Written-before.Written)}
}

type codexWarmTurn struct {
	id, model           string
	serverTurnID        string
	warm                *codexWarmSession
	events              chan adapter.Event
	done                chan struct{}
	once                sync.Once
	mu                  sync.Mutex
	next                int
	exit                adapter.ExitInfo
	err                 error
	cancel              atomic.Bool
	errorText           string
	lastOutput          atomic.Int64
	candidate, fallback []byte
	usage               codexTokenUsage
	hasUsage            bool
	firstResumed        bool
	total               codexTokenUsage
	resumedBaseline     codexTokenUsage
}

func (w *codexWarmSession) Run(spec adapter.TurnSpec) (adapter.Turn, error) {
	if spec.TurnID == "" {
		return nil, fmt.Errorf("turn ID required")
	}
	input, err := turnInput(spec)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	if w.dead {
		w.mu.Unlock()
		return nil, adapter.ErrWarmGone
	}
	if w.cur != nil {
		w.mu.Unlock()
		return nil, adapter.ErrWarmBusy
	}
	if w.threadID != "" && (!spec.NativeSessionStarted || spec.NativeSessionHandle != w.threadID) {
		w.mu.Unlock()
		return nil, fmt.Errorf("Codex warm thread differs from the requested session")
	}
	t := &codexWarmTurn{id: spec.TurnID, model: spec.Agent.Model, warm: w, firstResumed: w.threadID == "" && spec.NativeSessionStarted,
		events: make(chan adapter.Event, 128), done: make(chan struct{})}
	w.cur = t
	threadID := w.threadID
	w.mu.Unlock()
	defer func() {
		if err != nil {
			w.mu.Lock()
			if w.cur == t {
				w.cur = nil
			}
			w.mu.Unlock()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if threadID == "" {
		method := "thread/start"
		params := map[string]any{"model": spec.Agent.Model, "cwd": w.cwd,
			"approvalPolicy": "never", "sandbox": "danger-full-access", "ephemeral": false}
		if spec.NativeSessionStarted {
			method = "thread/resume"
			params = map[string]any{"threadId": spec.NativeSessionHandle, "model": spec.Agent.Model,
				"cwd": w.cwd, "approvalPolicy": "never", "sandbox": "danger-full-access", "excludeTurns": true}
		}
		var answer struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		err = w.call(ctx, method, params, &answer)
		if err != nil {
			return nil, err
		}
		if answer.Thread.ID == "" || (spec.NativeSessionStarted && answer.Thread.ID != spec.NativeSessionHandle) {
			err = fmt.Errorf("Codex warm thread ID missing or changed")
			return nil, err
		}
		threadID = answer.Thread.ID
		w.mu.Lock()
		w.threadID = threadID
		w.mu.Unlock()
		if !spec.NativeSessionStarted {
			t.emit(adapter.Event{Kind: adapter.EventNativeSessionStarted, NativeSessionHandle: threadID})
		}
	}
	var answer struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	err = w.call(ctx, "turn/start", map[string]any{"threadId": threadID,
		"input":        []map[string]any{{"type": "text", "text": string(input), "text_elements": []any{}}},
		"outputSchema": json.RawMessage(responseSchema), "model": spec.Agent.Model, "cwd": w.cwd,
		"approvalPolicy": "never", "effort": w.defaultEffort}, &answer)
	if err != nil {
		return nil, err
	}
	if answer.Turn.ID == "" {
		err = fmt.Errorf("Codex warm turn ID missing")
		return nil, err
	}
	t.mu.Lock()
	t.serverTurnID = answer.Turn.ID
	t.mu.Unlock()
	return t, nil
}

func (t *codexWarmTurn) Events() <-chan adapter.Event { return t.events }
func (t *codexWarmTurn) LastOutput() time.Time        { return time.Unix(0, t.lastOutput.Load()) }
func (t *codexWarmTurn) Wait() (adapter.ExitInfo, error) {
	<-t.done
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.exit, t.err
}
func (t *codexWarmTurn) Cancel() error {
	select {
	case <-t.done:
		return nil
	default:
	}
	t.cancel.Store(true)
	if pid := t.warm.Pid(); pid > 0 {
		return adapter.SignalGroup(pid, syscall.SIGTERM, t.warm.reap.Reaped)
	}
	return nil
}
func (t *codexWarmTurn) Kill() error                        { return t.warm.Kill() }
func (t *codexWarmTurn) Terminate(grace time.Duration) bool { return t.warm.Close(grace) == nil }
func (t *codexWarmTurn) emit(event adapter.Event) {
	t.mu.Lock()
	if event.SourceEventID == "" {
		event.SourceEventID = fmt.Sprintf("%s:%04d", t.id, t.next)
		t.next++
	}
	t.mu.Unlock()
	t.events <- event
}
func (t *codexWarmTurn) finish(exit adapter.ExitInfo, err error) {
	t.once.Do(func() {
		t.mu.Lock()
		t.exit, t.err = exit, err
		t.mu.Unlock()
		t.emit(adapter.Event{Kind: adapter.EventProcessExit, Exit: &exit})
		close(t.events)
		close(t.done)
	})
}

func (t *codexWarmTurn) handleNotification(method string, raw json.RawMessage) {
	var identity struct {
		TurnID string `json:"turnId"`
		Turn   struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(raw, &identity) == nil {
		id := identity.TurnID
		if id == "" {
			id = identity.Turn.ID
		}
		t.mu.Lock()
		serverID := t.serverTurnID
		t.mu.Unlock()
		if id != "" && serverID != "" && id != serverID {
			return
		}
	}
	t.lastOutput.Store(time.Now().UnixNano())
	switch method {
	case "turn/started":
		t.emit(adapter.Event{Kind: adapter.EventActivity, Summary: "turn.started"})
	case "item/started", "item/completed":
		var value struct {
			Item struct {
				ID    string `json:"id"`
				Type  string `json:"type"`
				Text  string `json:"text"`
				Phase string `json:"phase"`
			} `json:"item"`
		}
		if json.Unmarshal(raw, &value) != nil {
			return
		}
		if value.Item.Type == "agentMessage" {
			if method == "item/completed" && value.Item.Text != "" {
				if value.Item.Phase == "final_answer" {
					t.candidate = []byte(value.Item.Text)
				} else {
					t.fallback = []byte(value.Item.Text)
				}
			}
			return
		}
		event := adapter.Event{Kind: adapter.EventActivity, Summary: strings.ReplaceAll(method, "/", ".")}
		switch value.Item.Type {
		case "commandExecution", "mcpToolCall", "fileChange", "webSearch", "dynamicToolCall":
			event.Tools = []adapter.ToolEvent{{ID: value.Item.ID, Done: method == "item/completed"}}
		}
		t.emit(event)
	case "thread/tokenUsage/updated":
		var value struct {
			TokenUsage struct {
				Total codexTokenUsage `json:"total"`
				Last  codexTokenUsage `json:"last"`
			} `json:"tokenUsage"`
		}
		if json.Unmarshal(raw, &value) != nil {
			return
		}
		w := t.warm
		w.mu.Lock()
		t.total = value.TokenUsage.Total
		if t.firstResumed {
			if !t.hasUsage {
				// The resumed thread's total includes older turns. The first
				// update's last request establishes the pre-turn baseline.
				t.resumedBaseline = t.total.minus(value.TokenUsage.Last)
			}
			t.usage = t.total.minus(t.resumedBaseline)
		} else {
			t.usage = value.TokenUsage.Total.minus(w.lastTotal)
		}
		t.hasUsage = true
		w.mu.Unlock()
	case "error":
		var value struct {
			WillRetry bool            `json:"willRetry"`
			Error     json.RawMessage `json:"error"`
		}
		if json.Unmarshal(raw, &value) != nil {
			return
		}
		if !value.WillRetry {
			t.errorText = adapter.SanitizeStderr(string(value.Error))
		}
		t.emit(adapter.Event{Kind: adapter.EventActivity, Summary: "error"})
	case "turn/completed":
		var value struct {
			Turn struct {
				Status string          `json:"status"`
				Error  json.RawMessage `json:"error"`
			} `json:"turn"`
		}
		if json.Unmarshal(raw, &value) != nil {
			return
		}
		if value.Turn.Status == "completed" {
			candidate := t.candidate
			if len(candidate) == 0 {
				candidate = t.fallback
			}
			if len(candidate) > 0 {
				t.reportUsage()
				t.emit(adapter.Event{Kind: adapter.EventFinalAnswer, Answer: candidate})
				w := t.warm
				w.mu.Lock()
				if w.cur == t {
					w.cur = nil
					if t.hasUsage {
						w.lastTotal = t.total
					}
				}
				w.mu.Unlock()
				t.finish(adapter.ExitInfo{TerminalEvent: true, ExitCode: 0}, nil)
				return
			}
		}
		t.errorText = adapter.SanitizeStderr(string(value.Turn.Error))
		if t.errorText == "" || t.errorText == "null" {
			t.errorText = "Codex app-server turn ended without a final answer"
		}
		if value.Turn.Status == "interrupted" {
			t.cancel.Store(true)
		}
		_ = t.warm.stdin.Close()
		time.AfterFunc(2*time.Second, t.warm.killGroup)
	}
}

func (t *codexWarmTurn) reportUsage() {
	if !t.hasUsage {
		return
	}
	input, output, cached, written := t.usage.Input, t.usage.Output, t.usage.Cached, t.usage.Written
	fresh := max(0, input-cached-written)
	usage := &adapter.Usage{TokensIn: &fresh, TokensOut: &output, TokensCacheRead: &cached, TokensCacheWrite: &written}
	if cost := pricing.Estimate(t.model, &fresh, &output, &cached, &written); cost != nil {
		usage.CostEstimate, usage.CostSource = cost, "estimated"
	}
	t.emit(adapter.Event{Kind: adapter.EventUsage, Usage: usage})
}
