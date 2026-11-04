// Package opencode adapts one `opencode run` invocation to an aimebu turn.
//
// OpenCode allocates its own session ID (`ses_…`) on the first run, so like
// Codex the handle comes from the first event and later turns resume it with
// `-s`. The prompt goes in on standard input and the answer comes back as
// JSONL (`--format json`); a turn is terminal only when a step finishes with
// reason "stop" after text was produced and no error was reported.
//
// The Docker variant runs the same turn through the harness-docker launcher.
// OpenCode takes its configuration from the environment alone, and the
// launcher starts the harness in a container that does not inherit the
// supervisor's environment, so the variables that isolate the agent (its
// config document with the toolbox's bearer key, its state paths) are listed
// in HARNESS_DOCKER_FORWARD_ENV: the launcher then passes them by name, and
// the values never appear in an argument list. A launcher that cannot do that
// is refused before the turn runs.
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
	Variant     string // "native" (the default) or "docker"
	RuntimeRoot string
	version     string
}

func New(runtimeRoot string) *Adapter { return &Adapter{Command: "opencode", RuntimeRoot: runtimeRoot} }

func NewDocker() *Adapter { return &Adapter{Command: "opencode-docker", Variant: "docker"} }

func (a *Adapter) variant() string {
	if a.Variant == "" {
		return "native"
	}
	return a.Variant
}

func (a *Adapter) CatalogIdentity() (string, string) { return "opencode", a.variant() }

func (a *Adapter) Catalog() []adapter.CatalogEntry {
	version := a.version
	if version == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, a.Command, "--version")
		cmd.Env = versionEnv()
		if a.variant() == "docker" {
			cmd.Env = append(adapter.DockerBaseEnv(), "OPENCODE_LAUNCH_FLAGS=")
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
	if a.variant() == "docker" {
		return []adapter.CatalogEntry{{Tool: "opencode", Version: version, Environment: adapter.Environment{Variant: "docker", OS: "linux", Container: true}}}
	}
	return []adapter.CatalogEntry{{Tool: "opencode", Version: version, Environment: adapter.Environment{Variant: "native", OS: runtime.GOOS}}}
}

// Capabilities claims nothing for the Docker variant: what the native adapter was verified for (read-only turns, cancel, cost) is not
// carried to a container until the opt-in live canary, AIMEBU2_LIVE_OPENCODE_DOCKER=1, has shown it.
func (a *Adapter) Capabilities(version string, environment adapter.Environment) adapter.Capabilities {
	if version != SupportedVersion || environment.Variant != "native" || environment.OS != "linux" || a.variant() != "native" {
		return adapter.Capabilities{}
	}
	return adapter.Capabilities{NoActionMode: true, Cost: true, CancelConfirmed: true}
}

func (a *Adapter) Start(ctx context.Context, spec adapter.TurnSpec) (adapter.Turn, error) {
	docker := a.variant() == "docker"
	if (spec.Agent.Variant == "docker") != docker {
		return nil, fmt.Errorf("this adapter runs OpenCode in the %s variant, not %q", a.variant(), spec.Agent.Variant)
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
	sum := sha256.Sum256([]byte(spec.Agent.ID))
	// The agent's private state (its config folder and session database) lives under the supervisor's runtime root natively, and below the
	// mirrored checkout for a container, which sees the checkout and nothing of the supervisor's root.
	var runtimeDir string
	if docker {
		if err := adapter.DockerCheckout(spec.CheckoutPath); err != nil {
			return nil, err
		}
		runtimeDir = filepath.Join(spec.CheckoutPath, ".aimebu", "opencode", hex.EncodeToString(sum[:]))
	} else {
		if a.RuntimeRoot == "" {
			return nil, errors.New("OpenCode runtime root required")
		}
		runtimeDir = filepath.Join(a.RuntimeRoot, hex.EncodeToString(sum[:]))
	}
	configHome := filepath.Join(runtimeDir, "config")
	if err := os.MkdirAll(configHome, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(runtimeDir, 0700); err != nil {
		return nil, err
	}
	// What the harness sees: a container may see the checkout under another path; a native process sees everything where it is.
	visibleRuntime, visibleCheckout := runtimeDir, spec.CheckoutPath
	if docker {
		var err error
		if visibleRuntime, err = adapter.VisiblePath(spec, runtimeDir); err != nil {
			return nil, err
		}
		if visibleCheckout, err = adapter.VisiblePath(spec, spec.CheckoutPath); err != nil {
			return nil, err
		}
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
		if docker {
			endpoint, authorization, err = adapter.DockerMCPAt(spec.AgentToolsConfigPath, spec.Agent.ID, spec.AgentServerURL)
		}
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
	providerKeys, err := addCustomProviders(config, spec)
	if err != nil {
		return nil, err
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
	args := []string{"run", "--pure", "--format", "json", "-m", spec.Agent.Model, "--dir", visibleCheckout}
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
	if docker {
		env, forwarded := dockerEnv(filepath.Join(visibleRuntime, "config"), filepath.Join(visibleRuntime, "opencode.db"), string(configJSON), providerKeys, spec.LauncherEnvRefs)
		// An older launcher would run the harness without any of this: refuse it here, before the turn starts, not after a spawn that looked fine.
		probe, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := adapter.RequireDockerFeature(probe, "OpenCode", command, spec.LauncherArgs, spec.CheckoutPath, env, adapter.FeatureForwardEnv)
		cancel()
		if err != nil {
			return nil, err
		}
		cmd.Env = adapter.LauncherEnv(env, launcherRefs(spec.LauncherEnvRefs, forwarded))
	} else {
		cmd.Env = adapter.LauncherEnv(childEnv(configHome, filepath.Join(runtimeDir, "opencode.db"), string(configJSON), providerKeys), spec.LauncherEnvRefs)
	}
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
		cmd: cmd, events: make(chan adapter.Event, 64), done: make(chan struct{}), stderrCapture: spec.StderrSink}
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

// The environment variables a provider of OpenCode's own reads its key from (adapter.KnownProviderKeys). OpenCode takes credentials from its
// own auth.json (never read here) or from these, so only this bounded list reaches the child.

func versionEnv() []string {
	return passThrough(map[string]bool{"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "LANG": true, "LC_ALL": true,
		"XDG_DATA_HOME": true, "XDG_CACHE_HOME": true, "XDG_STATE_HOME": true})
}

// childEnv keeps OpenCode's data directory (so it finds its own credentials)
// but gives the agent a private config directory, session database, and an
// inline config, so the user's global config, MCP servers, plugins, and
// instructions never reach an agent turn.
func childEnv(configHome, dbPath, configJSON string, extraKeys []string) []string {
	allowed := map[string]bool{"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "LANG": true,
		"LC_ALL": true, "LC_CTYPE": true, "TERM": true, "TMPDIR": true,
		"XDG_DATA_HOME": true, "XDG_CACHE_HOME": true, "XDG_STATE_HOME": true,
		"HTTPS_PROXY": true, "HTTP_PROXY": true, "NO_PROXY": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true}
	for _, key := range adapter.KnownProviderKeys {
		allowed[key] = true
	}
	for _, key := range extraKeys {
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

// dockerProtected are the variables a Docker turn sets itself. An owner's environment reference to one of them is ignored: it would
// replace the agent's config document or state paths, switch the launcher's flags back on, or change what the launcher forwards.
var dockerProtected = map[string]bool{"OPENCODE_DB": true, "OPENCODE_CONFIG_CONTENT": true, "XDG_CONFIG_HOME": true,
	"OPENCODE_DISABLE_AUTOUPDATE": true, "OPENCODE_DISABLE_PROJECT_CONFIG": true, "OPENCODE_DISABLE_CLAUDE_CODE": true,
	"OPENCODE_LAUNCH_FLAGS": true, "HARNESS_DOCKER_FORWARD_ENV": true, "HARNESS_DOCKER_PRINT_FEATURES": true}

// launcherRefs drops an owner's references to the variables the adapter sets, and to the ones it forwards: a reference to a provider key
// that is set on the host would otherwise swap that key's value for another variable's.
func launcherRefs(refs map[string]string, forwarded []string) map[string]string {
	named := make(map[string]bool, len(forwarded))
	for _, name := range forwarded {
		named[name] = true
	}
	kept := make(map[string]string, len(refs))
	for target, source := range refs {
		if !dockerProtected[target] && !named[target] {
			kept[target] = source
		}
	}
	return kept
}

// dockerEnv is the environment of the launcher for a Docker turn. The agent's configuration (the same document and private state paths a
// native turn gets, the paths seen from inside the container) and the provider keys that are set are listed in HARNESS_DOCKER_FORWARD_ENV,
// which makes the launcher pass them to the harness by name: their values are in this process's environment, never in an argument list.
// OPENCODE_LAUNCH_FLAGS is cleared because the launcher's built-in --auto comes before the subcommand and would start the interface
// instead of `run`; the adapter passes its own flags after `run`.
func dockerEnv(configHome, dbPath, configJSON string, extraKeys []string, refs map[string]string) (env, forwarded []string) {
	env = adapter.DockerBaseEnv()
	set := func(name, value string) {
		env = append(env, name+"="+value)
		forwarded = append(forwarded, name)
	}
	set("XDG_CONFIG_HOME", configHome)
	set("OPENCODE_DB", dbPath)
	set("OPENCODE_CONFIG_CONTENT", configJSON)
	set("OPENCODE_DISABLE_AUTOUPDATE", "1")
	set("OPENCODE_DISABLE_PROJECT_CONFIG", "1")
	set("OPENCODE_DISABLE_CLAUDE_CODE", "1")
	for _, key := range adapter.KnownProviderKeys {
		if value := os.Getenv(key); value != "" {
			set(key, value)
		}
	}
	// The key variables of a custom provider are forwarded by name like the others; where the owner's launcher references another variable
	// for one, that variable's value is the key.
	for _, key := range extraKeys {
		if value := adapter.EnvValue(key, refs); value != "" && !forwardedName(forwarded, key) {
			set(key, value)
		}
	}
	return append(env, "HARNESS_DOCKER_FORWARD_ENV="+strings.Join(forwarded, ","), "OPENCODE_LAUNCH_FLAGS="), forwarded
}

func forwardedName(forwarded []string, name string) bool {
	for _, item := range forwarded {
		if item == name {
			return true
		}
	}
	return false
}

// addCustomProviders puts the custom providers of the turn into the agent's private config document and returns the names of their key
// variables. A provider's key is only ever written as the reference {env:NAME}, which OpenCode resolves itself from its environment: the
// value is never part of the document, the argument list or a log. A provider whose key variable is not set where the turn runs is refused
// before anything starts, naming the variable, instead of failing later with the endpoint's 401.
func addCustomProviders(config map[string]any, spec adapter.TurnSpec) ([]string, error) {
	if len(spec.Providers) == 0 {
		return nil, nil
	}
	document, keys, err := ProviderDocument(spec.Providers)
	if err != nil {
		return nil, err
	}
	for _, p := range spec.Providers {
		if p.KeyEnv != "" && adapter.EnvValue(p.KeyEnv, spec.LauncherEnvRefs) == "" {
			return nil, fmt.Errorf("custom provider %q: the key variable %s is not set on this machine", p.ID, p.KeyEnv)
		}
	}
	config["provider"] = document
	return keys, nil
}

// ProviderDocument is the "provider" section of an OpenCode config for custom providers, and the names of their key variables. The key is
// only the reference {env:NAME}. Anything a config built from a delivery must not trust is refused: the server checks it when it saves a
// provider, and this checks again because the providers arrive over the wire.
func ProviderDocument(providers []adapter.CustomProvider) (document map[string]any, keys []string, err error) {
	document = map[string]any{}
	for _, p := range providers {
		if p.ID == "" || strings.Contains(p.ID, "/") {
			return nil, nil, fmt.Errorf("custom provider id %q is not usable", p.ID)
		}
		if p.NPM != adapter.CustomProviderNPM {
			return nil, nil, fmt.Errorf("custom provider %q names package %q: only %s is allowed", p.ID, p.NPM, adapter.CustomProviderNPM)
		}
		if !strings.HasPrefix(p.BaseURL, "https://") && !strings.HasPrefix(p.BaseURL, "http://") {
			return nil, nil, fmt.Errorf("custom provider %q has no usable base URL", p.ID)
		}
		options := map[string]any{"baseURL": p.BaseURL}
		if p.KeyEnv != "" {
			if adapter.ProtectedKeyEnv(p.KeyEnv) {
				return nil, nil, fmt.Errorf("custom provider %q: %s is a variable the harness uses itself and cannot hold a key", p.ID, p.KeyEnv)
			}
			options["apiKey"] = "{env:" + p.KeyEnv + "}"
			keys = append(keys, p.KeyEnv)
		}
		models := map[string]any{}
		for _, m := range p.Models {
			model := map[string]any{"name": m.Name}
			if p.ContextTokens > 0 && p.OutputTokens > 0 {
				model["limit"] = map[string]any{"context": p.ContextTokens, "output": p.OutputTokens}
			}
			models[m.ID] = model
		}
		name := p.Name
		if name == "" {
			name = p.ID
		}
		document[p.ID] = map[string]any{"npm": p.NPM, "name": name, "options": options, "models": models}
	}
	return document, keys, nil
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
	cancelSignaled      bool
	reap                adapter.Reaper // set as soon as the process has been waited for, before the exit is reported
	exit                adapter.ExitInfo
	err                 error
	next                int
	// stderrCapture receives the harness's stderr as it is written (the per-agent output log); see adapter.TurnSpec.StderrSink.
	stderrCapture io.Writer
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
	if t.reap.Reaped() { // nothing left to cancel, and no intent to record for a process that ended by itself
		t.mu.Unlock()
		return nil
	}
	t.cancel = true
	if t.cancelSignaled || t.cmd.Process == nil {
		t.mu.Unlock()
		return nil
	}
	err := adapter.SignalGroup(t.cmd.Process.Pid, syscall.SIGTERM, t.reap.Reaped)
	if err == nil {
		t.cancelSignaled = true
	}
	t.mu.Unlock()
	if err != nil {
		return err
	}
	go adapter.KillAfter(t.cmd.Process.Pid, 5*time.Second, t.done, t.reap.Reaped)
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
	in, out               int64
	cacheRead, cacheWrite int64
	haveRead, haveWrite   bool
	cost                  float64
	haveCost              bool // some step reported a cost; none means the cost is unknown, not zero
	contextNow            int64
	seen                  bool
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
		u.cacheRead, u.haveRead = u.cacheRead+*tokens.Cache.Read, true
	}
	if tokens.Cache.Write != nil {
		input += *tokens.Cache.Write
		u.cacheWrite, u.haveWrite = u.cacheWrite+*tokens.Cache.Write, true
	}
	u.contextNow = input
	if e.Part.Cost != nil {
		u.cost += *e.Part.Cost
		u.haveCost = true
	}
}

func (t *turn) run(stdout, stderr io.Reader) {
	defer close(t.events)
	defer close(t.done)
	var tail adapter.StderrTail
	output := io.Writer(&tail)
	if capture := adapter.JoinWriters(t.stderrCapture); capture != nil {
		output = io.MultiWriter(&tail, capture)
	}
	stderrDone := make(chan struct{})
	go func() { _, _ = io.Copy(output, stderr); close(stderrDone) }()
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
	// What the steps cost is known whether or not the turn finished; one that did not is recorded as partial, so a failed or killed turn's
	// spend still counts against the limits.
	if usage.seen {
		in, out := usage.in, usage.out
		total := usage.contextNow
		spent := &adapter.Usage{TokensIn: &in, TokensOut: &out, ContextUsed: &total, Partial: !terminal}
		if usage.haveCost {
			cost := usage.cost
			spent.CostEstimate = &cost
		}
		if usage.haveRead {
			spent.TokensCacheRead = &usage.cacheRead
		}
		if usage.haveWrite {
			spent.TokensCacheWrite = &usage.cacheWrite
		}
		t.emit(adapter.Event{Kind: adapter.EventUsage, Usage: spent})
	}
	if terminal {
		t.emit(adapter.Event{Kind: adapter.EventFinalAnswer, Answer: candidate})
	}
	<-stderrDone
	waitErr := t.cmd.Wait()
	t.reap.MarkReaped()
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
