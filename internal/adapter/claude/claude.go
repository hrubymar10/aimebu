// Package claude adapts one Claude Code print invocation to an aimebu turn.
package claude

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/adapter/prompt"
	"github.com/hrubymar10/aimebu/internal/cmdprobe"
	"github.com/hrubymar10/aimebu/internal/harness"
)

type Adapter struct {
	Command       string
	AllowNative   bool
	MaxBudgetUSD  string
	Variant       string
	version       string
	stderrCapture io.Writer
}

func New() *Adapter {
	return &Adapter{Command: "claude", Variant: "native", AllowNative: true}
}

func NewDocker() *Adapter { return &Adapter{Command: "claude-docker", Variant: "docker"} }

func (a *Adapter) variant() string {
	if a.Variant == "" {
		return "native"
	}
	return a.Variant
}

func (a *Adapter) CatalogIdentity() (string, string) { return "claude", a.variant() }

var systemDeltas = prompt.Deltas{
	Opening:     "Complete this aimebu turn from the JSON supplied on standard input.",
	Role:        "Obey the role instructions in context.role_body.",
	Attachments: prompt.AttachmentGetTool,
	Output:      "Return only the JSON object described by the response schema: a messages array with room-targeted bodies, using reply_to, thread_root, attachments, ask, needs_attention, proposed_answers, open_questions, handoff, visual_plan, and appendix_pages only when appropriate. A visual_plan block has id, type, title, data, and order; data is a JSON-encoded object string. Use appendix_pages for a collapsed Full plan. To ask a human through the inbox, include a room message with a nonempty body and \"ask\":{\"kind\":\"question\",\"text\":\"the question\",\"options\":[\"saved choice\"],\"free_text\":false}." + " " + prompt.HumanResponse("short choices"),
	Ack:         prompt.AckByReaction,
	Closing:     "Do not add a message-level \"type\" field. context.rooms is a snapshot at turn start. A room returned by create_room during this turn is a valid target after creation; use its returned ID. Target only rooms where you are a member. Continue after toolbox calls until you have returned the required messages; do not finish with an empty messages array when a question or handoff remains.",
}

var systemPrompt = prompt.Build(systemDeltas)

// turnInput is the JSON on standard input. The prompt overrides are in the system prompt already, so they are not repeated in it.
func turnInput(spec adapter.TurnSpec) ([]byte, error) {
	return json.Marshal(struct {
		TurnID       string          `json:"turn_id"`
		Notes        json.RawMessage `json:"notes"`
		Context      json.RawMessage `json:"context"`
		AllowedTools []string        `json:"allowed_tools"`
	}{spec.TurnID, spec.NotesJSON, prompt.WithoutOverrides(spec.ContextJSON), spec.AllowedTools})
}

func systemPromptFor(spec adapter.TurnSpec) string {
	var context struct {
		RoleBody string `json:"role_body"`
	}
	base := prompt.ForContext(systemDeltas, systemPrompt, spec.ContextJSON)
	if json.Unmarshal(spec.ContextJSON, &context) != nil || context.RoleBody == "" {
		return base
	}
	return base + "\n\nRole instructions:\n" + context.RoleBody
}

var responseSchema = []byte(`{"type":"object","additionalProperties":false,"required":["messages"],"properties":{"messages":{"type":"array","maxItems":10,"items":{"type":"object","additionalProperties":false,"required":["room","body","reply_to","thread_root","attachments","ask","handoff","visual_plan","appendix_pages","needs_attention","proposed_answers","open_questions"],"properties":{"room":{"type":"string"},"body":{"type":"string"},"reply_to":{"type":["string","null"]},"thread_root":{"type":["string","null"]},"attachments":{"type":["array","null"],"items":{"type":"string"}},"ask":{"type":["object","null"],"additionalProperties":false,"required":["kind","text","options","free_text"],"properties":{"kind":{"type":"string","enum":["question","approval"]},"text":{"type":"string"},"options":{"type":"array","items":{"type":"string"}},"free_text":{"type":"boolean"}}},"handoff":{"type":["boolean","null"]},"visual_plan":{"type":["array","null"],"maxItems":80,"items":{"type":"object","additionalProperties":false,"required":["id","type","title","data","order"],"properties":{"id":{"type":["string","null"]},"type":{"type":"string"},"title":{"type":["string","null"]},"data":{"type":"string"},"order":{"type":["integer","null"]}}}},"appendix_pages":{"type":["array","null"],"maxItems":10,"items":{"type":"object","additionalProperties":false,"required":["title","body"],"properties":{"title":{"type":["string","null"]},"body":{"type":"string"}}}},"needs_attention":{"type":["boolean","null"]},"proposed_answers":{"type":["array","null"],"maxItems":4,"items":{"type":"string"}},"open_questions":{"type":["array","null"],"maxItems":10,"items":{"type":"object","additionalProperties":false,"required":["question","description","options"],"properties":{"question":{"type":"string"},"description":{"type":["string","null"]},"options":{"type":"array","minItems":2,"maxItems":8,"items":{"type":"string"}}}}}}}}}}`)

func (a *Adapter) Catalog() []adapter.CatalogEntry {
	version := a.version
	if version == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, a.Command, "--version")
		if a.variant() == "docker" {
			cmd.Env = append(childEnv(), "CLAUDE_LAUNCH_FLAGS=")
		}
		out, err := cmdprobe.Output(cmd) // bounded by the deadline, not by helpers the launcher leaves behind
		if err != nil {
			return nil
		}
		version = strings.TrimSpace(string(out))
	}
	if version == "" {
		return nil
	}
	osName := runtime.GOOS
	if a.variant() == "docker" {
		osName = "linux"
	}
	return []adapter.CatalogEntry{{Tool: "claude", Version: version, Environment: adapter.Environment{Variant: a.variant(), OS: osName, Container: a.variant() == "docker"}}}
}

func (a *Adapter) Capabilities(version string, environment adapter.Environment) adapter.Capabilities {
	if version == "" || environment.Variant != a.variant() {
		return adapter.Capabilities{}
	}
	return adapter.Capabilities{SchemaFlag: true, CompactionEvents: true, ContextWindow: true, Cost: true, CancelConfirmed: a.variant() == "native"}
}

func (a *Adapter) Start(ctx context.Context, spec adapter.TurnSpec) (adapter.Turn, error) {
	if a.variant() == "native" && !a.AllowNative {
		return nil, fmt.Errorf("native execution disabled; use the docker variant")
	}
	if spec.TurnID == "" || spec.Agent.ID == "" || spec.CheckoutPath == "" || spec.Agent.Model == "" {
		return nil, fmt.Errorf("turn ID, agent ID, checkout path, and model required")
	}
	if spec.NativeSessionStarted && spec.NativeSessionHandle == "" {
		return nil, fmt.Errorf("resumed turn requires native session handle")
	}
	if a.variant() == "docker" {
		if err := adapter.DockerCheckout(spec.CheckoutPath); err != nil {
			return nil, err
		}
	}
	input, err := turnInput(spec)
	if err != nil {
		return nil, err
	}
	args := []string{"-p", "--output-format", "stream-json", "--verbose", "--append-system-prompt", systemPromptFor(spec), "--json-schema", string(responseSchema)}
	if spec.Agent.Model != "" {
		args = append(args, "--model", spec.Agent.Model)
	}
	if a.MaxBudgetUSD != "" {
		args = append(args, "--max-budget-usd", a.MaxBudgetUSD)
	}
	if spec.NativeSessionStarted {
		args = append(args, "--resume", spec.NativeSessionHandle)
	} else if spec.NativeSessionHandle != "" {
		args = append(args, "--session-id", spec.NativeSessionHandle)
	}
	if spec.Mode == adapter.ModeDiagnostic {
		args = append(args, "--strict-mcp-config", "--tools", "Read", "--disallowedTools", "Bash,Edit,Write", "--permission-mode", "dontAsk")
	} else {
		if spec.Mode == adapter.ModeNormal {
			if a.variant() == "docker" {
				endpoint, header, err := adapter.DockerMCPAt(spec.AgentToolsConfigPath, spec.Agent.ID, spec.AgentServerURL)
				if err != nil {
					return nil, err
				}
				config, err := json.Marshal(map[string]any{"mcpServers": map[string]any{"aimebu": map[string]any{"type": "http", "url": endpoint, "headers": map[string]string{"Authorization": header}}}})
				if err != nil {
					return nil, err
				}
				args = append(args, "--mcp-config", string(config), "--strict-mcp-config")
			} else {
				if spec.AgentToolsConfigPath == "" {
					return nil, fmt.Errorf("normal turn requires agent tools config")
				}
				args = append(args, "--mcp-config", spec.AgentToolsConfigPath, "--strict-mcp-config")
			}
		}
		if len(spec.AllowedTools) > 0 {
			args = append(args, "--tools", strings.Join(spec.AllowedTools, ","))
		}
		args = append(args, "--permission-mode", "bypassPermissions")
	}
	command := a.Command
	if spec.LauncherPath != "" {
		command = spec.LauncherPath
	}
	cmd := exec.CommandContext(ctx, command, adapter.LauncherArgs(spec, args)...)
	cmd.Dir = spec.CheckoutPath
	cmd.Env = adapter.LauncherEnv(childEnv(), spec.LauncherEnvRefs)
	if a.variant() == "docker" {
		cmd.Env = append(cmd.Env, "CLAUDE_LAUNCH_FLAGS=")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() { _, _ = stdin.Write(input); _ = stdin.Close() }()
	t := &turn{id: spec.TurnID, cmd: cmd, expectedSession: spec.NativeSessionHandle, started: spec.NativeSessionStarted, events: make(chan adapter.Event, 32), done: make(chan struct{}), stderrCapture: adapter.JoinWriters(a.stderrCapture, spec.StderrSink)}
	go t.run(stdout, stderr)
	return t, nil
}

func childEnv() []string {
	allowed := map[string]bool{"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "LANG": true,
		"LC_ALL": true, "LC_CTYPE": true, "TERM": true, "TMPDIR": true, "ANTHROPIC_API_KEY": true,
		"ANTHROPIC_BASE_URL": true, "CLAUDE_CONFIG_DIR": true, "HTTPS_PROXY": true, "HTTP_PROXY": true,
		"NO_PROXY": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
		"DOCKER_HOST": true, "DOCKER_CONTEXT": true, "DOCKER_TLS_VERIFY": true, "DOCKER_CERT_PATH": true}
	var out []string
	for _, item := range os.Environ() {
		key, _, ok := strings.Cut(item, "=")
		if ok && allowed[key] {
			out = append(out, item)
		}
	}
	return out
}

type turn struct {
	lastOutput      atomic.Int64 // unix nanos of the last stdout line
	id              string
	cmd             *exec.Cmd
	expectedSession string
	started         bool
	events          chan adapter.Event
	done            chan struct{}
	mu              sync.Mutex
	cancel          bool
	reap            adapter.Reaper // set as soon as the process has been waited for, before the exit is reported
	exit            adapter.ExitInfo
	err             error
	next            int
	stderrCapture   io.Writer
}

// LastOutput is when the harness last wrote any stdout line, parsed or not.
// The supervisor's idle deadline counts it as liveness, so a long generation
// that only streams partial output is not mistaken for a stall.
func (t *turn) LastOutput() time.Time { return time.Unix(0, t.lastOutput.Load()) }

func (t *turn) Events() <-chan adapter.Event { return t.events }

// Cancel ends the turn's process group. A turn whose process is already reaped, or whose group is already gone, has nothing left to cancel,
// so cancelling again, or after the exit, is a no-op (see adapter.SignalGroup).
func (t *turn) Cancel() error {
	t.mu.Lock()
	if t.reap.Reaped() { // nothing left to cancel, and no intent to record for a process that ended by itself
		t.mu.Unlock()
		return nil
	}
	t.cancel = true
	t.mu.Unlock()
	if t.cmd.Process == nil {
		return nil
	}
	return adapter.SignalGroup(t.cmd.Process.Pid, syscall.SIGKILL, t.reap.Reaped)
}
func (t *turn) Wait() (adapter.ExitInfo, error) {
	<-t.done
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.exit, t.err
}

func (t *turn) run(stdout, stderr interface{ Read([]byte) (int, error) }) {
	defer close(t.events)
	defer close(t.done)
	var tail adapter.StderrTail
	output := io.Writer(&tail)
	if t.stderrCapture != nil {
		output = io.MultiWriter(&tail, t.stderrCapture)
	}
	stderrDone := make(chan struct{})
	go func() { _, _ = io.Copy(output, stderr); close(stderrDone) }()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	terminal := false
	var lastAssistantContext *int64
	steps := map[string]claudeStep{} // the main loop's API responses seen so far, by message id
	sawResult := false
	var candidate json.RawMessage
	var errorResult string
	haveCandidate := false
	for scanner.Scan() {
		t.lastOutput.Store(time.Now().UnixNano())
		line := append([]byte(nil), scanner.Bytes()...)
		event, known := harness.ParseClaude(line)
		if !known {
			continue
		}
		if compact, ok := harness.ParseClaudeCompaction(event); ok {
			t.emit(adapter.Event{Kind: adapter.EventCompaction, Compaction: &adapter.Compaction{PreTokens: compact.PreTokens, PostTokens: compact.PostTokens}})
			continue
		}
		t.confirmSession(event.Raw)
		switch event.Type {
		case "assistant", "tool_progress", "system", "rate_limit_event", "user":
			if event.Type == "rate_limit_event" && strings.Contains(strings.ToLower(string(event.Raw)), "limit reached") {
				errorResult = adapter.SanitizeStderr(string(event.Raw))
			}
			if event.Type == "assistant" {
				var assistant struct {
					ParentToolUseID *string `json:"parent_tool_use_id"`
					Message         struct {
						ID    string `json:"id"`
						Usage struct {
							InputTokens              *int64 `json:"input_tokens"`
							CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
							CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
						} `json:"usage"`
					} `json:"message"`
				}
				if json.Unmarshal(event.Raw, &assistant) == nil && assistant.ParentToolUseID == nil && assistant.Message.Usage.InputTokens != nil {
					used := *assistant.Message.Usage.InputTokens
					if assistant.Message.Usage.CacheReadInputTokens != nil {
						used += *assistant.Message.Usage.CacheReadInputTokens
					}
					if assistant.Message.Usage.CacheCreationInputTokens != nil {
						used += *assistant.Message.Usage.CacheCreationInputTokens
					}
					lastAssistantContext = &used
					// One API response is several assistant events that repeat its usage: each response counts once, by its message id.
					if id := assistant.Message.ID; id != "" {
						steps[id] = claudeStep{in: *assistant.Message.Usage.InputTokens, cacheRead: assistant.Message.Usage.CacheReadInputTokens, cacheWrite: assistant.Message.Usage.CacheCreationInputTokens}
					}
				}
			}
			activity := adapter.Event{Kind: adapter.EventActivity, Summary: event.Type}
			started, finished := harness.ClaudeToolUses(event)
			for _, id := range started {
				activity.Tools = append(activity.Tools, adapter.ToolEvent{ID: id})
			}
			for _, id := range finished {
				activity.Tools = append(activity.Tools, adapter.ToolEvent{ID: id, Done: true})
			}
			t.emit(activity)
		case "result":
			var result struct {
				Result  *string `json:"result"`
				IsError bool    `json:"is_error"`
				Usage   struct {
					InputTokens              *int64 `json:"input_tokens"`
					OutputTokens             *int64 `json:"output_tokens"`
					CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
					CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
				} `json:"usage"`
				ModelUsage map[string]struct {
					ContextWindow *int64   `json:"contextWindow"`
					CostUSD       *float64 `json:"costUSD"`
				} `json:"modelUsage"`
			}
			if json.Unmarshal(event.Raw, &result) != nil {
				continue
			}
			if result.IsError {
				errorResult = adapter.SanitizeStderr(string(event.Raw))
			}
			sawResult = true
			usage := &adapter.Usage{TokensIn: result.Usage.InputTokens, TokensOut: result.Usage.OutputTokens, TokensCacheRead: result.Usage.CacheReadInputTokens, TokensCacheWrite: result.Usage.CacheCreationInputTokens}
			if result.Usage.InputTokens != nil {
				used := *result.Usage.InputTokens
				if result.Usage.CacheReadInputTokens != nil {
					used += *result.Usage.CacheReadInputTokens
				}
				if result.Usage.CacheCreationInputTokens != nil {
					used += *result.Usage.CacheCreationInputTokens
				}
				usage.ContextUsed = &used
			}
			if lastAssistantContext != nil {
				usage.ContextUsed = lastAssistantContext
			}
			var cost float64
			var window int64
			windowSet := false
			costKnown, windowKnown := len(result.ModelUsage) > 0, len(result.ModelUsage) > 0
			for _, model := range result.ModelUsage {
				if model.CostUSD == nil {
					costKnown = false
				} else {
					cost += *model.CostUSD
				}
				if model.ContextWindow == nil || *model.ContextWindow <= 0 {
					windowKnown = false
				} else if windowSet && *model.ContextWindow != window {
					windowKnown = false
				} else {
					window = *model.ContextWindow
					windowSet = true
				}
			}
			if costKnown {
				// The result of a resumed session reports the whole session's spend so far; the store subtracts the previous total.
				usage.CostEstimate, usage.CostCumulative = &cost, true
			}
			if len(result.ModelUsage) == 1 {
				for name := range result.ModelUsage {
					usage.Model = name
				}
			}
			if windowKnown {
				usage.ContextWindow = &window
			}
			t.emit(adapter.Event{Kind: adapter.EventUsage, Usage: usage})
			if result.Result != nil && !result.IsError {
				candidate = append(candidate[:0], (*result.Result)...)
				haveCandidate = true
			}
		}
	}
	// A turn that ended with no result (killed, cancelled, crashed) still spent what its API responses used. The input and cache tokens of
	// those responses are known; the output count and the cost are only in the result, so they stay unknown, and the record says it is partial.
	if !sawResult && len(steps) > 0 {
		t.emit(adapter.Event{Kind: adapter.EventUsage, Usage: partialUsage(steps)})
	}
	scanError := scanner.Err()
	if scanError != nil {
		// Once scanning stops, an active child could block forever writing
		// to the unread stdout pipe, and its open stderr would hold the
		// copy below. End its process group before Wait.
		if t.cmd.Process != nil {
			_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	if haveCandidate && scanError == nil {
		t.emit(adapter.Event{Kind: adapter.EventFinalAnswer, Answer: candidate})
		terminal = true
	}
	<-stderrDone
	err := t.cmd.Wait()
	t.reap.MarkReaped()
	exit := adapter.ExitInfo{TerminalEvent: terminal}
	if err != nil {
		if status, ok := err.(*exec.ExitError); ok {
			exit.ExitCode = status.ExitCode()
		} else {
			exit.ExitCode = 1
		}
	}
	if !terminal {
		exit.StderrTail = tail.String()
		exit.ErrorEvent = errorResult
	}
	t.mu.Lock()
	exit.CancelIntent = t.cancel
	t.exit, t.err = exit, scanError
	t.mu.Unlock()
	t.emit(adapter.Event{Kind: adapter.EventProcessExit, Exit: &exit})
}

// claudeStep is what one API response of the main loop reported about its input.
type claudeStep struct {
	in                    int64
	cacheRead, cacheWrite *int64
}

func partialUsage(steps map[string]claudeStep) *adapter.Usage {
	var in, read, write int64
	var haveRead, haveWrite bool
	for _, step := range steps {
		in += step.in
		if step.cacheRead != nil {
			read, haveRead = read+*step.cacheRead, true
		}
		if step.cacheWrite != nil {
			write, haveWrite = write+*step.cacheWrite, true
		}
	}
	usage := &adapter.Usage{TokensIn: &in, Partial: true}
	if haveRead {
		usage.TokensCacheRead = &read
	}
	if haveWrite {
		usage.TokensCacheWrite = &write
	}
	return usage
}

func (t *turn) confirmSession(raw json.RawMessage) {
	if t.started || t.expectedSession == "" {
		return
	}
	var event struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(raw, &event) != nil || event.SessionID != t.expectedSession {
		return
	}
	if (event.Type == "system" && event.Subtype == "init") || event.Type == "result" {
		t.started = true
		t.emit(adapter.Event{Kind: adapter.EventNativeSessionStarted, NativeSessionHandle: t.expectedSession})
	}
}

func (t *turn) emit(event adapter.Event) {
	t.mu.Lock()
	if event.SourceEventID == "" {
		event.SourceEventID = fmt.Sprintf("%s:%04d", t.id, t.next)
		t.next++
	}
	t.mu.Unlock()
	t.events <- event
}
