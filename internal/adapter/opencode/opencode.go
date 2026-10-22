// Package opencode adapts one `opencode run` invocation to an aimebu turn.
//
// OpenCode allocates its own session ID (`ses_…`) on the first run, so like
// Codex the handle comes from the first event and later turns resume it with
// `-s`. The prompt goes in on standard input and the answer comes back as
// JSONL (`--format json`); a turn is terminal only when a step finishes with
// reason "stop" after text was produced and no error was reported.
package opencode

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
)

// SupportedVersion is the OpenCode release this adapter's event parsing and
// permission handling were verified against.
const SupportedVersion = "1.19.0"

type Adapter struct {
	Command     string
	RuntimeRoot string
	version     string
}

func New(runtimeRoot string) *Adapter { return &Adapter{Command: "opencode", RuntimeRoot: runtimeRoot} }

func (a *Adapter) CatalogIdentity() (string, string) { return "opencode", "native" }

func (a *Adapter) Catalog() []adapter.CatalogEntry {
	version := a.version
	if version == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, a.Command, "--version")
		cmd.Env = versionEnv()
		out, err := cmdprobe.Output(cmd) // bounded by the deadline, not by helpers the launcher leaves behind
		if err != nil {
			return nil
		}
		version = strings.TrimSpace(string(out))
	}
	if version == "" {
		return nil
	}
	return []adapter.CatalogEntry{{Tool: "opencode", Version: version, Environment: adapter.Environment{Variant: "native", OS: runtime.GOOS}}}
}

func (a *Adapter) Capabilities(version string, environment adapter.Environment) adapter.Capabilities {
	if version != SupportedVersion || environment.Variant != "native" || environment.OS != "linux" {
		return adapter.Capabilities{}
	}
	return adapter.Capabilities{NoActionMode: true, Cost: true, CancelConfirmed: true}
}

func (a *Adapter) Start(ctx context.Context, spec adapter.TurnSpec) (adapter.Turn, error) {
	if spec.Agent.Variant == "docker" {
		return nil, errors.New("OpenCode has no Docker variant")
	}
	if spec.TurnID == "" || spec.Agent.ID == "" || spec.CheckoutPath == "" || spec.Agent.Model == "" {
		return nil, errors.New("turn ID, agent ID, checkout path, and model required")
	}
	if !strings.Contains(spec.Agent.Model, "/") || strings.HasPrefix(spec.Agent.Model, "/") || strings.HasSuffix(spec.Agent.Model, "/") {
		return nil, fmt.Errorf("OpenCode model %q must be provider/model", spec.Agent.Model)
	}
	if spec.NativeSessionStarted && spec.NativeSessionHandle == "" {
		return nil, errors.New("resumed turn requires native session handle")
	}
	if a.RuntimeRoot == "" {
		return nil, errors.New("OpenCode runtime root required")
	}
	sum := sha256.Sum256([]byte(spec.Agent.ID))
	runtimeDir := filepath.Join(a.RuntimeRoot, hex.EncodeToString(sum[:]))
	configHome := filepath.Join(runtimeDir, "config")
	if err := os.MkdirAll(configHome, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(runtimeDir, 0700); err != nil {
		return nil, err
	}
	normal := spec.Mode == adapter.ModeNormal
	config := map[string]any{
		"$schema":    "https://opencode.ai/config.json",
		"autoupdate": false,
		"share":      "disabled",
		// A new session otherwise makes an extra model call to title itself.
		"agent": map[string]any{"title": map[string]any{"disable": true}},
	}
	if normal {
		endpoint, authorization, err := adapter.NativeMCP(spec.AgentToolsConfigPath, spec.Agent.ID)
		if err != nil {
			return nil, err
		}
		config["mcp"] = map[string]any{"aimebu": map[string]any{
			"type": "remote", "url": endpoint, "headers": map[string]string{"Authorization": authorization}, "enabled": true}}
	} else {
		// Diagnostic and correction turns may read but never act: the tools
		// that act are denied, which removes them from the model's tool list.
		config["permission"] = map[string]any{"bash": "deny", "edit": "deny", "webfetch": "deny", "external_directory": "deny"}
	}
	configJSON, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	input, err := turnInput(spec)
	if err != nil {
		return nil, err
	}
	// --pure keeps OpenCode from installing or loading external plugins, which
	// otherwise blocks a cold start on the network.
	args := []string{"run", "--pure", "--format", "json", "-m", spec.Agent.Model, "--dir", spec.CheckoutPath}
	if spec.NativeSessionStarted {
		args = append(args, "-s", spec.NativeSessionHandle)
	}
	if normal {
		args = append(args, "--auto")
	}
	command := a.Command
	if spec.LauncherPath != "" {
		command = spec.LauncherPath
	}
	cmd := exec.Command(command, adapter.LauncherArgs(spec, args)...)
	cmd.Dir = spec.CheckoutPath
	// The prompt arrives on stdin, which must reach EOF: OpenCode appends piped
	// stdin to the message and otherwise waits for it forever.
	cmd.Stdin = bytes.NewReader(input)
	cmd.Env = adapter.LauncherEnv(childEnv(configHome, filepath.Join(runtimeDir, "opencode.db"), string(configJSON)), spec.LauncherEnvRefs)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
	t := &turn{id: spec.TurnID, expectedSession: spec.NativeSessionHandle, started: spec.NativeSessionStarted,
		cmd: cmd, events: make(chan adapter.Event, 64), done: make(chan struct{})}
	go func() {
		select {
		case <-ctx.Done():
			_ = t.Cancel()
		case <-t.done:
		}
	}()
	go t.run(stdout, stderr)
	return t, nil
}

// turnPrompt is the instruction text ahead of the turn JSON on standard input.
var turnDeltas = prompt.Deltas{
	Opening:     "Complete this aimebu turn from the JSON below.",
	Role:        "Obey context.role_body.",
	Attachments: prompt.AttachmentGetTool,
	Output:      prompt.ReturnOneObject + " " + "For visual_plan blocks, data is a JSON-encoded object string; the server accepts and renders that object." + " " + prompt.HumanResponse("choices"),
	Ack:         prompt.AckByReaction,
	Closing:     prompt.OutsideRoomContext,
}

// turnPrompt is the shipped text; a turn whose context carries prompt overrides builds its own (see promptFor).
var turnPrompt = prompt.Build(turnDeltas)

func promptFor(spec adapter.TurnSpec) string {
	return prompt.ForContext(turnDeltas, turnPrompt, spec.ContextJSON)
}

func turnInput(spec adapter.TurnSpec) ([]byte, error) {
	input, err := json.Marshal(struct {
		TurnID       string          `json:"turn_id"`
		AgentID      string          `json:"agent_id"`
		Notes        json.RawMessage `json:"notes"`
		Context      json.RawMessage `json:"context"`
		AllowedTools []string        `json:"allowed_tools"`
	}{spec.TurnID, spec.Agent.ID, spec.NotesJSON, prompt.WithoutOverrides(spec.ContextJSON), spec.AllowedTools})
	if err != nil {
		return nil, err
	}
	return []byte(promptFor(spec) + string(input)), nil
}

// providerKeys are the environment variables a provider reads its key from.
// OpenCode takes credentials from its own auth.json (never read here) or from
// these, so only this bounded list reaches the child.
var providerKeys = []string{"OLLAMA_API_KEY", "OPENCODE_API_KEY", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY",
	"GEMINI_API_KEY", "GOOGLE_GENERATIVE_AI_API_KEY", "MISTRAL_API_KEY", "GROQ_API_KEY", "DEEPSEEK_API_KEY", "XAI_API_KEY"}

func versionEnv() []string {
	return passThrough(map[string]bool{"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "LANG": true, "LC_ALL": true,
		"XDG_DATA_HOME": true, "XDG_CACHE_HOME": true, "XDG_STATE_HOME": true})
}

// childEnv keeps OpenCode's data directory (so it finds its own credentials)
// but gives the agent a private config directory, session database, and an
// inline config, so the user's global config, MCP servers, plugins, and
// instructions never reach an agent turn.
func childEnv(configHome, dbPath, configJSON string) []string {
	allowed := map[string]bool{"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "LANG": true,
		"LC_ALL": true, "LC_CTYPE": true, "TERM": true, "TMPDIR": true,
		"XDG_DATA_HOME": true, "XDG_CACHE_HOME": true, "XDG_STATE_HOME": true,
		"HTTPS_PROXY": true, "HTTP_PROXY": true, "NO_PROXY": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true}
	for _, key := range providerKeys {
		allowed[key] = true
	}
	out := passThrough(allowed)
	return append(out,
		"XDG_CONFIG_HOME="+configHome,
		"OPENCODE_DB="+dbPath,
		"OPENCODE_CONFIG_CONTENT="+configJSON,
		"OPENCODE_DISABLE_AUTOUPDATE=1",
		"OPENCODE_DISABLE_PROJECT_CONFIG=1",
		"OPENCODE_DISABLE_CLAUDE_CODE=1",
	)
}

func passThrough(allowed map[string]bool) []string {
	out := []string{}
	for _, item := range os.Environ() {
		key, _, ok := strings.Cut(item, "=")
		if ok && allowed[key] {
			out = append(out, item)
		}
	}
	return out
}

type turn struct {
	lastOutput          atomic.Int64 // unix nanos of the last stdout line
	id, expectedSession string
	started             bool
	cmd                 *exec.Cmd
	events              chan adapter.Event
	done                chan struct{}
	mu                  sync.Mutex
	cancel              bool
	exit                adapter.ExitInfo
	err                 error
	next                int
}

// LastOutput is when the harness last wrote any stdout line, parsed or not,
// which the supervisor's idle deadline counts as liveness.
func (t *turn) LastOutput() time.Time { return time.Unix(0, t.lastOutput.Load()) }

func (t *turn) Events() <-chan adapter.Event { return t.events }

func (t *turn) Cancel() error {
	select {
	case <-t.done:
		return nil
	default:
	}
	t.mu.Lock()
	t.cancel = true
	t.mu.Unlock()
	if t.cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-t.cmd.Process.Pid, syscall.SIGTERM)
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	go func() {
		select {
		case <-t.done:
		case <-time.After(5 * time.Second):
			_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
		}
	}()
	return nil
}

func (t *turn) Wait() (adapter.ExitInfo, error) {
	<-t.done
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.exit, t.err
}

type ocEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionID"`
	Part      struct {
		Type   string `json:"type"`
		Text   string `json:"text"`
		Reason string `json:"reason"`
		CallID string `json:"callID"`
		Tokens struct {
			Input     *int64 `json:"input"`
			Output    *int64 `json:"output"`
			Reasoning *int64 `json:"reasoning"`
			Cache     struct {
				Read  *int64 `json:"read"`
				Write *int64 `json:"write"`
			} `json:"cache"`
		} `json:"tokens"`
		Cost *float64 `json:"cost"`
	} `json:"part"`
	Error struct {
		Name string `json:"name"`
		Data struct {
			Message    string `json:"message"`
			StatusCode int    `json:"statusCode"`
		} `json:"data"`
	} `json:"error"`
}

// usageTotals sums the steps of one turn: a tool-using turn is several steps.
type usageTotals struct {
	in, out    int64
	cost       float64
	contextNow int64
	seen       bool
}

func (u *usageTotals) add(e ocEvent) {
	u.seen = true
	tokens := e.Part.Tokens
	var input int64
	if tokens.Input != nil {
		input = *tokens.Input
		u.in += *tokens.Input
	}
	if tokens.Output != nil {
		u.out += *tokens.Output
	}
	if tokens.Cache.Read != nil {
		input += *tokens.Cache.Read
	}
	if tokens.Cache.Write != nil {
		input += *tokens.Cache.Write
	}
	u.contextNow = input
	if e.Part.Cost != nil {
		u.cost += *e.Part.Cost
	}
}

func (t *turn) run(stdout, stderr io.Reader) {
	defer close(t.events)
	defer close(t.done)
	var tail adapter.StderrTail
	stderrDone := make(chan struct{})
	go func() { _, _ = io.Copy(&tail, stderr); close(stderrDone) }()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	var candidate []byte
	var usage usageTotals
	lastError := ""
	sessionSeen, stopped := false, false
	firstSession := ""
	var scanError error
	for scanner.Scan() {
		t.lastOutput.Store(time.Now().UnixNano())
		var event ocEvent
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		if event.SessionID != "" {
			switch {
			case t.started && event.SessionID != t.expectedSession, sessionSeen && event.SessionID != firstSession:
				scanError = errors.New("OpenCode session ID changed")
				continue
			case !sessionSeen:
				sessionSeen, firstSession = true, event.SessionID
				if !t.started {
					t.emit(adapter.Event{Kind: adapter.EventNativeSessionStarted, NativeSessionHandle: event.SessionID})
				}
			}
		}
		switch event.Type {
		case "step_start":
			// Only the final step's text is the answer; earlier steps narrate.
			candidate, stopped = nil, false
			t.emit(adapter.Event{Kind: adapter.EventActivity, Summary: event.Type})
		case "text":
			// A step's reply can arrive as several text parts; they are the
			// consecutive pieces of one message, so they join without a separator.
			if event.Part.Text != "" {
				candidate = append(candidate, event.Part.Text...)
			}
		case "tool_use":
			// A tool part is reported once, already finished.
			t.emit(adapter.Event{Kind: adapter.EventActivity, Summary: event.Type, Tools: []adapter.ToolEvent{{ID: event.Part.CallID}, {ID: event.Part.CallID, Done: true}}})
		case "step_finish":
			usage.add(event)
			stopped = event.Part.Reason == "stop"
		case "error":
			lastError = formatError(event)
		}
	}
	if scanner.Err() != nil {
		scanError = scanner.Err()
		if t.cmd.Process != nil {
			_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	terminal := sessionSeen && stopped && candidate != nil && lastError == "" && scanError == nil
	if terminal {
		if usage.seen {
			in, out := usage.in, usage.out
			total := usage.contextNow
			cost := usage.cost
			t.emit(adapter.Event{Kind: adapter.EventUsage, Usage: &adapter.Usage{TokensIn: &in, TokensOut: &out, ContextUsed: &total, CostEstimate: &cost}})
		}
		t.emit(adapter.Event{Kind: adapter.EventFinalAnswer, Answer: candidate})
	}
	<-stderrDone
	waitErr := t.cmd.Wait()
	exit := adapter.ExitInfo{TerminalEvent: terminal}
	if waitErr != nil {
		if status, ok := waitErr.(*exec.ExitError); ok {
			exit.ExitCode = status.ExitCode()
		} else {
			exit.ExitCode = 1
		}
	}
	if !terminal {
		exit.StderrTail = tail.String()
		if lastError != "" {
			exit.ErrorEvent = adapter.SanitizeStderr("OpenCode error: " + lastError)
		}
	}
	t.mu.Lock()
	exit.CancelIntent = t.cancel
	t.exit, t.err = exit, scanError
	t.mu.Unlock()
	t.emit(adapter.Event{Kind: adapter.EventProcessExit, Exit: &exit})
}

// formatError renders an error event as "Name (HTTP 429): message" so the
// provider's own words, which the server matches usage-limit wording against,
// and its status code both reach the evidence.
func formatError(e ocEvent) string {
	name := e.Error.Name
	if name == "" {
		name = "error"
	}
	if e.Error.Data.StatusCode != 0 {
		name = fmt.Sprintf("%s (HTTP %d)", name, e.Error.Data.StatusCode)
	}
	if e.Error.Data.Message == "" {
		return name
	}
	return name + ": " + e.Error.Data.Message
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
