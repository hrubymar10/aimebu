// Package vibe adapts one `vibe -p` invocation (Mistral Vibe, programmatic mode) to an aimebu turn.
//
// Vibe allocates its own session ID on the first run and resumes it with `--resume`, so like Codex and OpenCode the handle comes from the
// stream and later turns resume it. The prompt goes in on standard input and the turn comes back as newline-delimited JSON entries
// (`--output streaming`): the answer is the last assistant message after the turn's own user message, which also marks where the entries
// Vibe replays from a resumed session end. Vibe streams no usage; what a turn spent is read from the session's meta.json once the process
// has exited cleanly. A session exists only after such an exit, so the native session handle is reported then, never earlier.
//
// Everything Vibe is configured with comes from the environment and a private VIBE_HOME per agent, so the person's own config (their MCP
// servers, hooks, skills, agents) never reaches an agent turn; see seedHome.
//
// The Docker variant runs the same turn through the harness-docker launcher. The container does not inherit the supervisor's environment,
// so the variables above (and the bearer key of the toolbox) are listed in HARNESS_DOCKER_FORWARD_ENV and their values put in the launcher's
// own environment: the launcher passes them by name, and no value appears in an argument list. A launcher that cannot do that is refused
// before the turn runs. The private home and the session folder then live below the mirrored checkout.
package vibe

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	"github.com/hrubymar10/aimebu/internal/vibeconfig"
)

// SupportedVersion is the Vibe release this adapter's stream parsing and session accounting were verified against.
const SupportedVersion = "2.25.0"

// stopEventTag wraps the text Vibe gives an assistant message when it stops a turn itself (a turn, price or token limit).
const stopEventTag = "<vibe_stop_event>"

type Adapter struct {
	Command     string
	Variant     string // "native" (the default) or "docker"
	RuntimeRoot string
	// UserHome is the person's own Vibe home, where their config.toml and .env live; empty means $VIBE_HOME, then ~/.vibe.
	UserHome string
	version  string
}

func New(runtimeRoot string) *Adapter { return &Adapter{Command: "vibe", RuntimeRoot: runtimeRoot} }

func NewDocker() *Adapter { return &Adapter{Command: "vibe-docker", Variant: "docker"} }

func (a *Adapter) variant() string {
	if a.Variant == "" {
		return "native"
	}
	return a.Variant
}

func (a *Adapter) CatalogIdentity() (string, string) { return "vibe", a.variant() }

func (a *Adapter) Catalog() []adapter.CatalogEntry {
	version := a.version
	if version == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, a.Command, "--version")
		cmd.Env = versionEnv()
		if a.variant() == "docker" {
			cmd.Env = append(adapter.DockerBaseEnv(), "VIBE_LAUNCH_FLAGS=")
		}
		out, err := cmdprobe.Output(cmd) // bounded by the deadline, not by helpers the launcher leaves behind
		if err != nil {
			return nil
		}
		// `vibe --version` prints "vibe 2.25.0".
		fields := strings.Fields(string(out))
		if len(fields) == 0 {
			return nil
		}
		version = fields[len(fields)-1]
	}
	if a.variant() == "docker" {
		return []adapter.CatalogEntry{{Tool: "vibe", Version: version, Environment: adapter.Environment{Variant: "docker", OS: "linux", Container: true}}}
	}
	return []adapter.CatalogEntry{{Tool: "vibe", Version: version, Environment: adapter.Environment{Variant: "native", OS: runtime.GOOS}}}
}

// Capabilities claims no no-action mode: restricting a turn to the read tools is only trusted after the opt-in live canary has shown that
// Vibe's MCP servers are not loaded with it. The Docker variant claims nothing until its own live canary has run.
func (a *Adapter) Capabilities(version string, environment adapter.Environment) adapter.Capabilities {
	if version != SupportedVersion || environment.Variant != "native" || environment.OS != "linux" || a.variant() != "native" {
		return adapter.Capabilities{}
	}
	return adapter.Capabilities{Cost: true, CancelConfirmed: true}
}

var modelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,127}$`)

func (a *Adapter) Start(ctx context.Context, spec adapter.TurnSpec) (adapter.Turn, error) {
	docker := a.variant() == "docker"
	if (spec.Agent.Variant == "docker") != docker {
		return nil, fmt.Errorf("this adapter runs Vibe in the %s variant, not %q", a.variant(), spec.Agent.Variant)
	}
	if spec.TurnID == "" || spec.Agent.ID == "" || spec.CheckoutPath == "" || spec.Agent.Model == "" {
		return nil, errors.New("turn ID, agent ID, checkout path, and model required")
	}
	if !modelPattern.MatchString(spec.Agent.Model) {
		return nil, fmt.Errorf("invalid Vibe model alias %q", spec.Agent.Model)
	}
	if spec.NativeSessionStarted && spec.NativeSessionHandle == "" {
		return nil, errors.New("resumed turn requires native session handle")
	}
	sum := sha256.Sum256([]byte(spec.Agent.ID))
	// The agent's private home and session folder live under the supervisor's runtime root natively, and below the mirrored checkout for a
	// container, which sees the checkout and nothing of the supervisor's root.
	var runtimeDir string
	if docker {
		if err := adapter.DockerCheckout(spec.CheckoutPath); err != nil {
			return nil, err
		}
		runtimeDir = filepath.Join(spec.CheckoutPath, ".aimebu", "vibe", hex.EncodeToString(sum[:]))
	} else {
		if a.RuntimeRoot == "" {
			return nil, errors.New("Vibe runtime root required")
		}
		runtimeDir = filepath.Join(a.RuntimeRoot, hex.EncodeToString(sum[:]))
	}
	home, saveDir := filepath.Join(runtimeDir, "home"), filepath.Join(runtimeDir, "sessions")
	for _, dir := range []string{home, saveDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
	}
	if err := os.Chmod(runtimeDir, 0700); err != nil {
		return nil, err
	}
	scope := mcpNone
	if spec.Mode == adapter.ModeNormal {
		scope = mcpAll
		if docker {
			scope = mcpRemote
		}
	}
	providerEnv, err := seedHome(home, a.userHome(), scope)
	if err != nil {
		return nil, err
	}
	normal := spec.Mode == adapter.ModeNormal
	// What the harness sees: a container may see the checkout under another path; a native process sees everything where it is.
	visibleHome, visibleSave, visibleCheckout := home, saveDir, spec.CheckoutPath
	if docker {
		if visibleHome, err = adapter.VisiblePath(spec, home); err != nil {
			return nil, err
		}
		if visibleSave, err = adapter.VisiblePath(spec, saveDir); err != nil {
			return nil, err
		}
		if visibleCheckout, err = adapter.VisiblePath(spec, spec.CheckoutPath); err != nil {
			return nil, err
		}
	}
	// What isolates an agent is set last and cannot be named by an owner's environment references: a launcher that forwarded VIBE_HOME or
	// VIBE_MCP_SERVERS would otherwise point Vibe at the person's own config or hand a read-only turn a toolbox.
	protected := []string{"VIBE_HOME=" + visibleHome, "VIBE_ACTIVE_MODEL=" + spec.Agent.Model, "VIBE_SESSION_LOGGING__SAVE_DIR=" + visibleSave}
	args := []string{"-p", "--output", "streaming", "--workdir", visibleCheckout}
	if spec.NativeSessionStarted {
		args = append(args, "--resume", spec.NativeSessionHandle)
	}
	if normal {
		endpoint, authorization, err := adapter.NativeMCP(spec.AgentToolsConfigPath, spec.Agent.ID)
		if docker {
			endpoint, authorization, err = adapter.DockerMCPAt(spec.AgentToolsConfigPath, spec.Agent.ID, spec.AgentServerURL)
		}
		if err != nil {
			return nil, err
		}
		// The toolbox is injected through the environment: the bearer travels in its own variable, never in argv or a file.
		servers, err := json.Marshal([]map[string]any{{"name": "aimebu", "transport": "streamable-http", "url": endpoint, "api_key_env": "AIMEBU2_AGENT_KEY"}})
		if err != nil {
			return nil, err
		}
		protected = append(protected, "VIBE_MCP_SERVERS="+string(servers), "AIMEBU2_AGENT_KEY="+strings.TrimPrefix(authorization, "Bearer "))
		// --trust makes Vibe load the checkout's own configuration (a .vibe or .agents folder, and AGENTS.md), as the other harnesses read
		// theirs; the agent already runs with every tool approved here.
		args = append(args, "--trust", "--yolo")
	} else {
		// A diagnostic or correction turn may read but never act: with -p, naming the enabled tools disables every other one. It is not
		// trusted either, so nothing the checkout carries (hooks, MCP servers, skills, agents) is loaded into it.
		args = append(args, "--enabled-tools", "read_file", "--enabled-tools", "grep")
	}
	input, err := turnInput(spec)
	if err != nil {
		return nil, err
	}
	var before *sessionStats
	if spec.NativeSessionStarted {
		before = readStats(saveDir, spec.NativeSessionHandle)
	}
	command := a.Command
	if spec.LauncherPath != "" {
		command = spec.LauncherPath
	}
	cmd := exec.Command(command, adapter.LauncherArgs(spec, args)...)
	cmd.Dir = spec.CheckoutPath
	// With no TEXT argument `vibe -p` reads the prompt from stdin, which must reach EOF.
	cmd.Stdin = bytes.NewReader(input)
	if docker {
		// The launcher passes the listed variables to the harness by name; the provider keys that are set on the host go with them. The
		// launcher's own flags are cleared: its built-in --yolo would also apply to a read-only turn, so the adapter passes its flags itself.
		env := adapter.DockerBaseEnv()
		forwarded := make([]string, 0, len(protected)+len(providerEnv)+2)
		for _, item := range protected {
			name, _, _ := strings.Cut(item, "=")
			forwarded = append(forwarded, name)
		}
		// Python buffers a pipe: without this the streamed entries would come in one lump at the end, as natively.
		protected = append(protected, "PYTHONUNBUFFERED=1")
		forwarded = append(forwarded, "PYTHONUNBUFFERED")
		for _, key := range append([]string{"MISTRAL_API_KEY"}, providerEnv...) {
			if value := os.Getenv(key); value != "" {
				env = append(env, key+"="+value)
				forwarded = append(forwarded, key)
			}
		}
		protected = append(protected, "HARNESS_DOCKER_FORWARD_ENV="+strings.Join(forwarded, ","), "VIBE_LAUNCH_FLAGS=")
		// An older launcher would run the harness without any of this: refuse it here, before the turn starts.
		probe, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := adapter.RequireDockerFeature(probe, "Vibe", command, spec.LauncherArgs, spec.CheckoutPath, env, adapter.FeatureForwardEnv)
		cancel()
		if err != nil {
			return nil, err
		}
		cmd.Env = append(adapter.LauncherEnv(env, launcherRefs(spec.LauncherEnvRefs, forwarded)), protected...)
	} else {
		cmd.Env = append(adapter.LauncherEnv(childEnv(providerEnv), launcherRefs(spec.LauncherEnvRefs, nil)), protected...)
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
	t := &turn{id: spec.TurnID, expectedSession: spec.NativeSessionHandle, started: spec.NativeSessionStarted, saveDir: saveDir, before: before,
		docker: docker, userHome: a.userHome(), cmd: cmd, events: make(chan adapter.Event, 64), done: make(chan struct{}), stderrCapture: spec.StderrSink}
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

// protectedEnv names the variables the adapter sets itself; an owner's environment reference to one of them is ignored.
var protectedEnv = map[string]bool{"VIBE_HOME": true, "VIBE_ACTIVE_MODEL": true, "VIBE_SESSION_LOGGING__SAVE_DIR": true, "VIBE_MCP_SERVERS": true, "AIMEBU2_AGENT_KEY": true,
	"VIBE_LAUNCH_FLAGS": true, "HARNESS_DOCKER_FORWARD_ENV": true, "HARNESS_DOCKER_PRINT_FEATURES": true}

// launcherRefs drops an owner's references to the variables the adapter sets and, for a container, to the ones it forwards: a reference to
// a provider key that is set on the host would otherwise swap that key's value for another variable's.
func launcherRefs(refs map[string]string, forwarded []string) map[string]string {
	named := make(map[string]bool, len(forwarded))
	for _, name := range forwarded {
		named[name] = true
	}
	kept := make(map[string]string, len(refs))
	for target, source := range refs {
		if !protectedEnv[target] && !named[target] {
			kept[target] = source
		}
	}
	return kept
}

func (a *Adapter) userHome() string {
	if a.UserHome != "" {
		return a.UserHome
	}
	if home := os.Getenv("VIBE_HOME"); home != "" {
		return home
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".vibe")
	}
	return ""
}

// seedHome makes the private home an agent runs in: a config.toml with the person's models and providers (the only parts of their config an
// agent needs, so that a model they listed resolves) under settings that keep Vibe quiet, and their .env linked in so its key still
// authenticates. The .env is never opened here. It returns the names of the environment variables the providers read their key from.
// mcpScope says which of the person's own MCP servers a turn carries: none (a read-only turn), every one (a native turn), or the remote ones (a
// container, which has none of the host's commands).
type mcpScope int

const (
	mcpNone mcpScope = iota
	mcpAll
	mcpRemote
)

func seedHome(home, userHome string, scope mcpScope) ([]string, error) {
	var blocks []vibeconfig.Block
	if userHome != "" {
		var err error
		if blocks, err = vibeconfig.Read(filepath.Join(userHome, "config.toml")); err != nil {
			return nil, fmt.Errorf("read the Vibe config: %w", err)
		}
	}
	// The person's own MCP servers go along (a read-only turn never connects to any: it names the tools it may use). aimebu's own entry reaches
	// the turn through VIBE_MCP_SERVERS and replaces a server of that name from any file, so a v1 bus of that name is dropped here.
	blocks = vibeconfig.WithoutMCPServer(blocks, "aimebu")
	// Nor the entry that is aimebu's own bus of v1 under another name (it runs the aimebu binary, or points at v1's port).
	var kept []vibeconfig.Block
	for _, block := range blocks {
		if block.Kind == "mcp_servers" {
			name := block.Fields["name"]
			if name == "" {
				name = block.Alias
			}
			if adapter.LooksLikeV1Bus(name, strings.Join(block.Lines, "\n")) {
				log.Printf("vibe: the MCP server %q is not carried: it is aimebu's own bus of v1", name)
				continue
			}
		}
		kept = append(kept, block)
	}
	blocks = kept
	switch scope {
	case mcpNone:
		blocks = vibeconfig.WithoutMCPServers(blocks)
	case mcpRemote:
		// A container has none of the host's commands: it keeps the servers that connect over the network.
		var dropped []string
		blocks, dropped = vibeconfig.WithOnlyRemoteMCPServers(blocks)
		for _, name := range dropped {
			log.Printf("vibe: the MCP server %q is not carried to the container: a command on the host is not in the container", name)
		}
	}
	config := "# Written by aimebu for this agent before every turn. Edit your own Vibe config instead.\n" +
		"enable_telemetry = false\nenable_update_checks = false\nenable_auto_update = false\nenable_notifications = false\nenable_connectors = false\n\n" +
		vibeconfig.Render(blocks)
	path := filepath.Join(home, "config.toml")
	if current, err := os.ReadFile(path); err != nil || string(current) != config {
		if err := writeFileAtomic(path, []byte(config)); err != nil {
			return nil, err
		}
	}
	if userHome != "" {
		target, link := filepath.Join(userHome, ".env"), filepath.Join(home, ".env")
		if _, err := os.Stat(target); err == nil {
			if current, err := os.Readlink(link); err != nil || current != target {
				_ = os.Remove(link)
				if err := os.Symlink(target, link); err != nil {
					return nil, err
				}
			}
		}
	}
	return append(vibeconfig.ProviderEnv(blocks), adapter.UserEnvNames(vibeconfig.MCPEnv(blocks))...), nil
}

func writeFileAtomic(path string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	_, err = temp.Write(data)
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(name, 0600)
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		_ = os.Remove(name)
	}
	return err
}

// turnDeltas is the instruction text ahead of the turn JSON on standard input.
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

func versionEnv() []string {
	return passThrough(map[string]bool{"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "LANG": true, "LC_ALL": true})
}

// childEnv passes the little Vibe needs and the key of the Mistral provider (and of any provider the person's models name); the rest of
// the environment, including every other harness's credentials, stays behind.
func childEnv(providerEnv []string) []string {
	allowed := map[string]bool{"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "LANG": true,
		"LC_ALL": true, "LC_CTYPE": true, "TERM": true, "TMPDIR": true,
		"HTTPS_PROXY": true, "HTTP_PROXY": true, "NO_PROXY": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "MISTRAL_API_KEY": true}
	for _, name := range providerEnv {
		allowed[name] = true
	}
	return append(passThrough(allowed), "PYTHONUNBUFFERED=1")
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
	saveDir             string
	before              *sessionStats // the resumed session's totals before this turn, to price the turn alone
	docker              bool
	userHome            string // the person's own Vibe home, named when a container cannot see the key in it
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

// LastOutput is when the harness last wrote any stdout line, parsed or not, which the supervisor's idle deadline counts as liveness.
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

// entry is one line of `vibe -p --output streaming`: a completed entry of the session's history.
type entry struct {
	Type             string `json:"type"`
	SessionID        string `json:"sessionId"`
	Role             string `json:"role"`
	Source           string `json:"source"`
	GenerationStatus string `json:"generationStatus"`
	Level            string `json:"level"`
	Message          string `json:"message"`
	Content          []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	ID string `json:"id"`
}

func (e entry) text() string {
	var parts []string
	for _, block := range e.Content {
		if block.Type == "text" && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n\n")
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
	stopText, lastNotice := "", ""
	sessionSeen, turnSeen := false, false
	firstSession := ""
	var scanError error
	for scanner.Scan() {
		t.lastOutput.Store(time.Now().UnixNano())
		var item entry
		if json.Unmarshal(scanner.Bytes(), &item) != nil || item.Type == "" {
			continue
		}
		if item.SessionID != "" {
			switch {
			case t.started && item.SessionID != t.expectedSession, sessionSeen && item.SessionID != firstSession:
				scanError = errors.New("Vibe session ID changed")
				continue
			case !sessionSeen:
				sessionSeen, firstSession = true, item.SessionID
			}
		}
		switch item.Type {
		case "message":
			switch {
			case item.Role == "user" && item.Source == "turn_start":
				// This turn's own prompt. A resumed session replays its earlier history before it, so what came before is not this turn's.
				turnSeen, candidate, stopText = true, nil, ""
			case item.Role == "assistant" && item.GenerationStatus == "completed":
				text := item.text()
				if strings.Contains(text, stopEventTag) {
					candidate, stopText = nil, text
				} else if text != "" {
					candidate, stopText = []byte(text), ""
				}
				t.emit(adapter.Event{Kind: adapter.EventActivity, Summary: "message"})
			}
		case "reasoning":
			t.emit(adapter.Event{Kind: adapter.EventActivity, Summary: "reasoning"})
		case "effect":
			// An effect is reported once it has finished.
			t.emit(adapter.Event{Kind: adapter.EventActivity, Summary: "effect", Tools: []adapter.ToolEvent{{ID: item.ID}, {ID: item.ID, Done: true}}})
		case "notice":
			if item.Level == "error" {
				lastNotice = item.Message
			}
		}
	}
	if scanner.Err() != nil {
		scanError = scanner.Err()
		if t.cmd.Process != nil {
			_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	<-stderrDone
	waitErr := t.cmd.Wait()
	t.reap.MarkReaped()
	exit := adapter.ExitInfo{}
	if waitErr != nil {
		if status, ok := waitErr.(*exec.ExitError); ok {
			exit.ExitCode = status.ExitCode()
		} else {
			exit.ExitCode = 1
		}
	}
	clean := exit.ExitCode == 0 && scanError == nil
	terminal := clean && sessionSeen && turnSeen && candidate != nil && stopText == ""
	// A session exists only after a clean exit: a killed or cancelled turn persists nothing, so its handle must not be reported.
	if clean && sessionSeen && !t.started {
		t.emit(adapter.Event{Kind: adapter.EventNativeSessionStarted, NativeSessionHandle: firstSession})
	}
	if sessionSeen {
		if spent := t.usage(firstSession, terminal); spent != nil {
			t.emit(adapter.Event{Kind: adapter.EventUsage, Usage: spent})
		}
	}
	if terminal {
		t.emit(adapter.Event{Kind: adapter.EventFinalAnswer, Answer: candidate})
	}
	exit.TerminalEvent = terminal
	if !terminal {
		exit.StderrTail = tail.String()
		switch {
		case stopText != "":
			exit.ErrorEvent = adapter.SanitizeStderr("Vibe stopped the turn: " + stopText)
		case lastErrorLine(exit.StderrTail) != "":
			exit.ErrorEvent = adapter.SanitizeStderr("Vibe error: " + lastErrorLine(exit.StderrTail) + t.keyHint(lastErrorLine(exit.StderrTail)))
		case lastNotice != "":
			exit.ErrorEvent = adapter.SanitizeStderr("Vibe error: " + lastNotice)
		}
	}
	t.mu.Lock()
	exit.CancelIntent = t.cancel
	t.exit, t.err = exit, scanError
	t.mu.Unlock()
	t.emit(adapter.Event{Kind: adapter.EventProcessExit, Exit: &exit})
}

// keyHint says how a key can reach a container when Vibe reports it missing. The key comes from the host environment (forwarded by name),
// or from the .env of the person's Vibe home, which the agent's home links to: that link resolves in the container only if the container
// has that folder mounted at the same path, which the adapter cannot see from here.
func (t *turn) keyHint(line string) string {
	if !t.docker || !strings.HasPrefix(line, "Missing ") || !strings.Contains(line, "_API_KEY") {
		return ""
	}
	home := t.userHome
	if home == "" {
		home = "the Vibe home"
	}
	return " (in Docker the key reaches the container from the host environment, where it is forwarded by name, or from " + home +
		"/.env, which works only if the harness container mounts that folder at the same path)"
}

// lastErrorLine is the last line Vibe printed that starts with "Error:", which is how it reports an API or configuration failure.
func lastErrorLine(stderr string) string {
	last := ""
	for _, line := range strings.Split(stderr, "\n") {
		if text, ok := strings.CutPrefix(strings.TrimSpace(line), "Error:"); ok {
			last = strings.TrimSpace(text)
		}
	}
	return last
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
