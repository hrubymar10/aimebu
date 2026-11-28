// Package codex adapts one Codex CLI exec invocation to an aimebu turn.
package codex

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
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/prompt"
	"github.com/hrubymar10/aimebu/v2/internal/cmdprobe"
	"github.com/hrubymar10/aimebu/v2/internal/harness"
	"github.com/hrubymar10/aimebu/v2/internal/pricing"
)

type Adapter struct {
	Command       string
	AllowNative   bool
	Variant       string
	RuntimeRoot   string
	AuthPath      string
	version       string
	stderrCapture io.Writer
	stdoutCapture io.Writer
}

func New(runtimeRoot string) *Adapter {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		if userHome, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(userHome, ".codex")
		}
	}
	return &Adapter{Command: "codex", Variant: "native", AllowNative: true, RuntimeRoot: runtimeRoot, AuthPath: filepath.Join(home, "auth.json")}
}

func NewDocker() *Adapter { return &Adapter{Command: "codex-docker", Variant: "docker"} }

func (a *Adapter) variant() string {
	if a.Variant == "" {
		return "native"
	}
	return a.Variant
}

func (a *Adapter) CatalogIdentity() (string, string) { return "codex", a.variant() }

func (a *Adapter) Catalog() []adapter.CatalogEntry {
	version := a.version
	if version == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, a.Command, "--version")
		cmd.Env = childEnv("")
		if a.variant() == "docker" {
			cmd.Env = append(cmd.Env, "CODEX_LAUNCH_FLAGS=")
		}
		out, err := cmdprobe.Output(cmd) // bounded by the deadline, not by helpers the launcher leaves behind
		if err != nil {
			return nil
		}
		version = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "codex-cli "))
	}
	if version == "" {
		return nil
	}
	osName := runtime.GOOS
	if a.variant() == "docker" {
		osName = "linux"
	}
	return []adapter.CatalogEntry{{Tool: "codex", Version: version, Environment: adapter.Environment{Variant: a.variant(), OS: osName, Container: a.variant() == "docker"}}}
}

func (a *Adapter) Capabilities(version string, environment adapter.Environment) adapter.Capabilities {
	if version != "0.162.1" || environment.Variant != a.variant() || environment.OS != "linux" {
		return adapter.Capabilities{}
	}
	return adapter.Capabilities{SchemaFlag: true, CancelConfirmed: a.variant() == "native"}
}

func (a *Adapter) Start(ctx context.Context, spec adapter.TurnSpec) (adapter.Turn, error) {
	if a.variant() == "native" && !a.AllowNative {
		return nil, errors.New("native execution disabled; use the docker variant")
	}
	if spec.TurnID == "" || spec.Agent.ID == "" || spec.CheckoutPath == "" || spec.Agent.Model == "" {
		return nil, errors.New("turn ID, agent ID, checkout path, and model required")
	}
	if spec.NativeSessionStarted && spec.NativeSessionHandle == "" {
		return nil, errors.New("resumed turn requires native session handle")
	}
	if a.variant() == "docker" {
		if err := adapter.DockerCheckout(spec.CheckoutPath); err != nil {
			return nil, err
		}
	}
	if a.variant() == "native" && a.RuntimeRoot == "" {
		return nil, errors.New("Codex runtime root required")
	}
	// The person's own MCP servers, read once for the turn: the tables for the private config and the variables to pass go together.
	var own userMCP
	if a.variant() == "native" && spec.Mode == adapter.ModeNormal && !spec.IsolateUserMCP {
		own = readUserMCP(a.AuthPath)
	}
	runtimeDir, err := a.prepareRuntime(spec, own)
	if err != nil {
		return nil, err
	}
	input, err := turnInput(spec)
	if err != nil {
		return nil, err
	}
	visibleCheckout, err := adapter.VisiblePath(spec, spec.CheckoutPath)
	if err != nil {
		return nil, err
	}
	args := []string{"-C", visibleCheckout}
	var agentKey []string
	var forwardNames []string // the names a Docker launcher hands the harness by name, beside the key (none natively)
	if a.variant() == "native" && spec.Mode == adapter.ModeNormal && spec.AgentTools != nil {
		// The private config names the variable (env_vars), and the key itself is only in this process's environment.
		agentKey = []string{adapter.AgentKeyEnv + "=" + spec.AgentTools.Key}
	}
	if a.variant() == "docker" && spec.Mode == adapter.ModeNormal {
		endpoint, header, err := adapter.DockerMCPAt(spec.AgentTools, spec.Agent.ID, spec.AgentServerURL)
		if err != nil {
			return nil, err
		}
		// The key is read from the forwarded variable, never put in the argument list (ps shows those to every local user).
		agentKey = []string{adapter.DockerAgentKeyEnv(header)}
		forwardNames = []string{adapter.AgentKeyEnv}
		config := "mcp_servers.aimebu_docker={url=" + strconv.Quote(endpoint) + ",bearer_token_env_var=" + strconv.Quote(adapter.AgentKeyEnv) + "}"
		// The person's own remote servers, as overrides of their own (the container's config.toml is ignored, see below), then aimebu's own two
		// entries, last, so nothing before them can replace either.
		if !spec.IsolateUserMCP {
			for _, override := range userDockerOverrides() {
				args = append(args, "-c", override)
			}
		}
		args = append(args, "-c", `mcp_servers.aimebu={command="true",enabled=false}`, "-c", config)
	}
	if spec.Mode == adapter.ModeNormal {
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	} else {
		args = append(args, "-a", "never", "-s", "read-only")
	}
	args = append(args, "exec")
	// Keep the container's mounted auth.json, but never load its user's MCPs.
	// This is an exec flag rather than a global Codex flag.
	if a.variant() == "docker" {
		args = append(args, "--ignore-user-config")
	}
	if spec.NativeSessionStarted {
		args = append(args, "resume")
	}
	schemaFile := filepath.Join(runtimeDir, "schema.json")
	schemaPath, err := adapter.VisiblePath(spec, schemaFile)
	if err != nil {
		return nil, err
	}
	args = append(args, "--json", "-m", spec.Agent.Model, "--output-schema", schemaPath)
	if spec.NativeSessionStarted {
		args = append(args, spec.NativeSessionHandle)
	}
	args = append(args, "-")
	command := a.Command
	if spec.LauncherPath != "" {
		command = spec.LauncherPath
	}
	cmd := exec.Command(command, adapter.LauncherArgs(spec, args)...)
	cmd.Dir = spec.CheckoutPath
	if a.variant() == "docker" {
		cmd.Env = append(childEnv(""), "CODEX_LAUNCH_FLAGS=")
	} else {
		cmd.Env = childEnv(runtimeDir, own.envVars...) // what the person's own MCP servers ask Codex to hand them
	}
	cmd.Env = adapter.AppendForwardEnv(adapter.LauncherEnv(cmd.Env, spec.LauncherEnvRefs), forwardNames...)
	cmd.Env = append(cmd.Env, agentKey...)
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
	// A shared checkout can be cleaned between preparation and process launch.
	// Restore a missing schema at the last point before the CLI reads it.
	schema := responseSchema
	if spec.Mode == adapter.ModeDiagnostic {
		schema = diagnosticSchema
	}
	if err := ensureSchema(schemaFile, schema); err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	t := &turn{id: spec.TurnID, model: spec.Agent.Model, expectedSession: spec.NativeSessionHandle, started: spec.NativeSessionStarted, cmd: cmd, events: make(chan adapter.Event, 128), done: make(chan struct{}), stderrCapture: adapter.JoinWriters(a.stderrCapture, spec.StderrSink), stdoutCapture: a.stdoutCapture, conv: adapter.ConversationFor(spec)}
	go func() { _, _ = stdin.Write(input); _ = stdin.Close() }()
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
	Ack:         prompt.AckByReaction + " Call mcp__aimebu__react with the message ID or number and one emoji.",
	Closing:     prompt.OutsideRoomContext,
}

// turnPrompt is the shipped text; a turn whose context carries prompt overrides builds its own (see promptFor).
var turnPrompt = prompt.Build(turnDeltas)

func promptFor(spec adapter.TurnSpec) string {
	if spec.Agent.Variant == "docker" {
		docker := turnDeltas
		docker.Ack = prompt.AckByReaction + " Call mcp__aimebu_docker__react with the message ID or number and one emoji."
		return prompt.ForContext(docker, prompt.Build(docker), spec.ContextJSON)
	}
	return prompt.ForContext(turnDeltas, turnPrompt, spec.ContextJSON)
}

// keepAliveText is the instruction of a keep-alive turn: in the turn's JSON, after the unchanged instruction text, so everything a real turn sends
// ahead of it (and the arguments of the process) stays the same.
func keepAliveText(spec adapter.TurnSpec) string {
	if !spec.KeepAlive {
		return ""
	}
	return "This is a cache keep-alive, not work. Reply with an empty messages array and do nothing else: no commands, no messages."
}

// KeepAliveTool says this adapter can run a keep-alive turn (adapter.KeepAliver).
func (a *Adapter) KeepAliveTool() string { return "codex" }

func turnInput(spec adapter.TurnSpec) ([]byte, error) {
	data, err := json.Marshal(struct {
		TurnID       string          `json:"turn_id"`
		AgentID      string          `json:"agent_id"`
		Notes        json.RawMessage `json:"notes"`
		Context      json.RawMessage `json:"context"`
		AllowedTools []string        `json:"allowed_tools"`
		KeepAlive    string          `json:"keep_alive,omitempty"`
	}{spec.TurnID, spec.Agent.ID, spec.NotesJSON, prompt.WithoutOverrides(spec.ContextJSON), spec.AllowedTools, keepAliveText(spec)})
	if err != nil {
		return nil, err
	}
	return []byte(promptFor(spec) + string(data)), nil
}

func childEnv(runtimeDir string, extra ...string) []string {
	extra = adapter.UserEnvNames(extra)
	allowed := map[string]bool{"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "LANG": true,
		"LC_ALL": true, "LC_CTYPE": true, "TERM": true, "TMPDIR": true, "OPENAI_API_KEY": true,
		"OPENAI_BASE_URL": true, "HTTPS_PROXY": true, "HTTP_PROXY": true, "NO_PROXY": true,
		"SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
		"DOCKER_HOST": true, "DOCKER_CONTEXT": true, "DOCKER_TLS_VERIFY": true, "DOCKER_CERT_PATH": true}
	for _, name := range extra {
		allowed[name] = true
	}
	out := []string{}
	for _, item := range os.Environ() {
		key, _, ok := strings.Cut(item, "=")
		if ok && allowed[key] {
			out = append(out, item)
		}
	}
	// The adapter's own value is written last, so nothing before it can replace it.
	if runtimeDir != "" {
		out = append(out, "CODEX_HOME="+runtimeDir)
	}
	return out
}

var responseSchema = []byte(`{"type":"object","additionalProperties":false,"required":["messages"],"properties":{"messages":{"type":"array","maxItems":10,"items":{"type":"object","additionalProperties":false,"required":["room","body","reply_to","attachments","ask","handoff","outcome","force","visual_plan","appendix_pages","needs_attention","proposed_answers","open_questions"],"properties":{"room":{"type":"string"},"body":{"type":"string"},"reply_to":{"type":["string","null"]},"attachments":{"type":["array","null"],"items":{"type":"string"}},"ask":{"type":["object","null"],"additionalProperties":false,"required":["kind","text","options","free_text"],"properties":{"kind":{"type":"string","enum":["question","approval"]},"text":{"type":"string"},"options":{"type":["array","null"],"items":{"type":"string"}},"free_text":{"type":["boolean","null"]}}},"handoff":{"type":["boolean","null"]},"outcome":{"type":["string","null"],"enum":["done","partial","failed",null]},"force":{"type":["boolean","null"]},"visual_plan":{"type":["array","null"],"maxItems":80,"items":{"type":"object","additionalProperties":false,"required":["id","type","title","data","order"],"properties":{"id":{"type":["string","null"]},"type":{"type":"string"},"title":{"type":["string","null"]},"data":{"type":"string"},"order":{"type":["integer","null"]}}}},"appendix_pages":{"type":["array","null"],"maxItems":10,"items":{"type":"object","additionalProperties":false,"required":["title","body"],"properties":{"title":{"type":["string","null"]},"body":{"type":"string"}}}},"needs_attention":{"type":["boolean","null"]},"proposed_answers":{"type":["array","null"],"maxItems":4,"items":{"type":"string"}},"open_questions":{"type":["array","null"],"maxItems":10,"items":{"type":"object","additionalProperties":false,"required":["question","description","options"],"properties":{"question":{"type":"string"},"description":{"type":["string","null"]},"options":{"type":"array","minItems":2,"maxItems":8,"items":{"type":"string"}}}}}}}}}}`)

var diagnosticSchema = []byte(`{"type":"object","additionalProperties":false,"required":["messages","reconciliation"],"properties":{"messages":{"type":"array","maxItems":0,"items":{"type":"object","additionalProperties":false,"required":["room","body"],"properties":{"room":{"type":"string"},"body":{"type":"string"}}}},"reconciliation":{"type":"object","additionalProperties":false,"required":["status","findings"],"properties":{"status":{"type":"string","enum":["clean","uncertain"]},"findings":{"type":"array","items":{"type":"string"}}}}}}`)

// ForgetAgent deletes the generated config that holds the agent's key. The next start writes it again.
func (a *Adapter) ForgetAgent(agentID string) {
	if a.RuntimeRoot == "" {
		return
	}
	hash := sha256.Sum256([]byte(agentID))
	_ = os.Remove(filepath.Join(a.RuntimeRoot, hex.EncodeToString(hash[:]), "config.toml"))
}

// SweepKeys deletes every generated config under the runtime root. An older supervisor wrote the agent's key into one; the next turn of an
// agent writes its config again, without a key.
func (a *Adapter) SweepKeys() {
	if a.RuntimeRoot == "" {
		return
	}
	matches, _ := filepath.Glob(filepath.Join(a.RuntimeRoot, "*", "config.toml"))
	for _, path := range matches {
		_ = os.Remove(path)
	}
}

func (a *Adapter) prepareRuntime(spec adapter.TurnSpec, own userMCP) (string, error) {
	hash := sha256.Sum256([]byte(spec.Agent.ID))
	root := a.RuntimeRoot
	if a.variant() == "docker" {
		root = filepath.Join(spec.CheckoutPath, ".aimebu", "codex-schemas")
	}
	dir := filepath.Join(root, hex.EncodeToString(hash[:]))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return "", err
	}
	if a.variant() == "native" && a.AuthPath != "" {
		if _, err := os.Stat(a.AuthPath); err == nil {
			link := filepath.Join(dir, "auth.json")
			if target, err := os.Readlink(link); err == nil && target != a.AuthPath {
				return "", fmt.Errorf("Codex auth link points to unexpected path")
			} else if os.IsNotExist(err) {
				if err := os.Symlink(a.AuthPath, link); err != nil {
					return "", err
				}
			} else if err != nil {
				return "", err
			}
		} else if !os.IsNotExist(err) {
			return "", err
		}
	}
	schema := responseSchema
	if spec.Mode == adapter.ModeDiagnostic {
		schema = diagnosticSchema
	}
	if err := writePrivate(filepath.Join(dir, "schema.json"), schema); err != nil {
		return "", err
	}
	if a.variant() == "docker" {
		return dir, nil
	}
	config, err := codexConfig(spec)
	if err != nil {
		return "", err
	}
	if own.tables != "" {
		// The person's own MCP servers, next to aimebu's entry, which is the only one under that name (see user_mcp.go).
		config = append(config, []byte("\n# the person's own MCP servers, copied from their Codex config\n"+own.tables)...)
	}
	configPath := filepath.Join(dir, "config.toml")
	if existing, err := os.ReadFile(configPath); err == nil && string(existing) == string(config) {
		return dir, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := writePrivate(configPath, config); err != nil {
		return "", err
	}
	return dir, nil
}

func codexConfig(spec adapter.TurnSpec) ([]byte, error) {
	if spec.Mode != adapter.ModeNormal || spec.AgentTools == nil {
		return []byte("# aimebu private Codex runtime\n"), nil
	}
	command, args, env, err := adapter.BridgeEntry(spec.AgentTools, spec.Agent.ID, "", spec.CheckoutPath)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString("[mcp_servers.aimebu]\ncommand = ")
	b.WriteString(strconv.Quote(command))
	b.WriteString("\nargs = [")
	for i, arg := range args {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(strconv.Quote(arg))
	}
	// The key is not in this file: Codex hands the bridge the variable of that name from its own environment.
	b.WriteString("]\nenv_vars = [" + strconv.Quote(adapter.AgentKeyEnv) + "]\n[mcp_servers.aimebu.env]\n")
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		b.WriteString(name)
		b.WriteString(" = ")
		b.WriteString(strconv.Quote(env[name]))
		b.WriteByte('\n')
	}
	return []byte(b.String()), nil
}

func writePrivate(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".aimebu-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := tmp.Chmod(0600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func ensureSchema(path string, schema []byte) error {
	data, err := os.ReadFile(path)
	if err == nil && string(data) == string(schema) {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return writePrivate(path, schema)
}

type turn struct {
	lastOutput                 atomic.Int64 // unix nanos of the last stdout line
	id, model, expectedSession string
	started                    bool
	cmd                        *exec.Cmd
	events                     chan adapter.Event
	done                       chan struct{}
	mu                         sync.Mutex
	cancel                     bool
	reap                       adapter.Reaper // set as soon as the process has been waited for, before the exit is reported
	exit                       adapter.ExitInfo
	err                        error
	next                       int
	stderrCapture              io.Writer
	stdoutCapture              io.Writer
	conv                       *adapter.Conversation // the turn's conversation log, nil when it has none
}

// LastOutput is when the harness last wrote any stdout line, parsed or not.
// The supervisor's idle deadline counts it as liveness, so a long generation
// that only streams partial output is not mistaken for a stall.
func (t *turn) LastOutput() time.Time { return time.Unix(0, t.lastOutput.Load()) }

func (t *turn) Events() <-chan adapter.Event { return t.events }

// ProcessIdentity is the harness process this turn runs in. The process leads its own group (Setpgid, where the command is built), so the
// group's id is the pid.
func (t *turn) ProcessIdentity() (pid, pgid int) {
	if t.cmd == nil || t.cmd.Process == nil {
		return 0, 0
	}
	pid = t.cmd.Process.Pid
	return pid, pid
}

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
	t.mu.Unlock()
	if t.cmd.Process == nil {
		return nil
	}
	if err := adapter.SignalGroup(t.cmd.Process.Pid, syscall.SIGTERM, t.reap.Reaped); err != nil {
		return err
	}
	go func() {
		select {
		case <-t.done:
		case <-time.After(5 * time.Second):
			_ = t.Kill()
		}
	}()
	return nil
}

// Terminate asks the turn's process group to end (SIGTERM) and waits up to grace for it to go on its own; only then is it killed. A launcher in a
// container traps the signal, ends the session in the container and exits 143 once it is gone, which is how a caller tells a confirmed end from a
// kill, and without this the supervisor could only Cancel a containerized ping (a kill of the wrapper after five seconds, which confirms nothing). It
// records the cancel intent like Cancel and returns whether the process ended on the terminate.
func (t *turn) Terminate(grace time.Duration) bool {
	select {
	case <-t.done:
		return true
	default:
	}
	t.mu.Lock()
	if t.reap.Reaped() {
		t.mu.Unlock()
		return true
	}
	t.cancel = true
	t.mu.Unlock()
	if t.cmd.Process != nil {
		_ = adapter.SignalGroup(t.cmd.Process.Pid, syscall.SIGTERM, t.reap.Reaped)
	}
	select {
	case <-t.done:
		return true
	case <-time.After(grace):
	}
	if t.cmd.Process != nil {
		_ = adapter.SignalGroup(t.cmd.Process.Pid, syscall.SIGKILL, t.reap.Reaped)
	}
	return false
}

func (t *turn) Kill() error {
	select {
	case <-t.done:
		return nil
	default:
	}
	t.mu.Lock()
	if t.reap.Reaped() {
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

func (t *turn) run(stdout, stderr io.Reader) {
	var tail adapter.StderrTail
	output := io.Writer(&tail)
	if t.stderrCapture != nil {
		output = io.MultiWriter(&tail, t.stderrCapture)
	}
	stderrDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(output, stderr)
		close(stderrDone)
	}()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	var candidate []byte
	completed := false
	failed := false
	pendingError := json.RawMessage(nil) // a non-transient error event: fatal unless turn.completed follows it
	errorEvent := ""
	sawThread := false
	threadID := ""
	var scanError error
	for scanner.Scan() {
		t.lastOutput.Store(time.Now().UnixNano())
		if t.stdoutCapture != nil {
			_, _ = t.stdoutCapture.Write(scanner.Bytes())
			_, _ = t.stdoutCapture.Write([]byte{'\n'})
		}
		event, known := harness.ParseCodex(scanner.Bytes())
		if !known {
			if t.conv.Active() && len(bytes.TrimSpace(scanner.Bytes())) > 0 {
				if event.Type == "" {
					t.conv.Event("(not an event)")
				} else {
					t.conv.Event(event.Type)
				}
			}
			continue
		}
		switch event.Type {
		case "thread.started":
			id := harness.CodexThreadID(event)
			if id == "" || t.started && id != t.expectedSession || sawThread && id != threadID {
				scanError = fmt.Errorf("Codex thread ID missing or changed")
				continue
			}
			if sawThread {
				continue
			}
			sawThread = true
			threadID = id
			if !t.started {
				t.emit(adapter.Event{Kind: adapter.EventNativeSessionStarted, NativeSessionHandle: id})
			}
		case "turn.started":
			t.emit(adapter.Event{Kind: adapter.EventActivity, Summary: event.Type})
		case "item.started", "item.updated", "item.completed":
			if event.Type != "item.updated" {
				transcribeItem(t.conv, event.Type, itemOf(event.Raw), event.Type == "item.completed")
			}
			if text, ok := harness.CodexAgentMessage(event); ok {
				candidate = append(candidate[:0], text...)
			} else {
				activity := adapter.Event{Kind: adapter.EventActivity, Summary: event.Type}
				if id, tool := harness.CodexToolItem(event); tool && event.Type != "item.updated" {
					activity.Tools = []adapter.ToolEvent{{ID: id, Done: event.Type == "item.completed"}}
				}
				t.emit(activity)
			}
		case "turn.completed":
			completed = true
			if pendingError != nil {
				// The turn went on to complete: the error was a notice, not its end.
				transcribeNotice(t.conv, pendingError, true)
				pendingError = nil
			}
			input, output := harness.CodexUsage(event)
			cached := harness.CodexCachedInput(event)
			written := harness.CodexCacheWrite(event)
			consistent := input != nil && *input >= 0
			if consistent {
				shares := int64(0)
				if cached != nil {
					shares += *cached
				}
				if written != nil {
					shares += *written
				}
				consistent = shares >= 0 && shares <= *input
			}
			// Codex reports input_tokens as the whole input, cached part included (cached_input_tokens is a share of it, as in the
			// Responses API it passes through). Cache writes are another share. Split the three categories so a cost estimate and
			// usage view do not count either cache share as ordinary input as well.
			if input != nil && (cached != nil || written != nil) {
				fresh := *input
				if cached != nil {
					fresh -= *cached
				}
				if written != nil {
					fresh -= *written
				}
				fresh = max(fresh, 0)
				input = &fresh
			}
			usage := &adapter.Usage{TokensIn: input, TokensOut: output, TokensCacheRead: cached, TokensCacheWrite: written}
			if consistent {
				if cost := pricing.Estimate(t.model, input, output, cached, written); cost != nil {
					usage.CostEstimate, usage.CostSource = cost, "estimated"
				}
			}
			// exec reports the thread's running totals in turn.completed.usage, not the turn's own: the figures include every earlier
			// turn of a resumed thread (the same totals the warm path baselines against), so the store subtracts the session's previous
			// totals. A thread's first turn is its own usage, and marking it cumulative too seeds the store's baseline with it.
			usage.TokensCumulative = true
			if usage.CostEstimate != nil {
				usage.CostCumulative = true
			}
			t.emit(adapter.Event{Kind: adapter.EventUsage, Usage: usage})
		case "turn.failed":
			failed = true
			errorEvent = adapter.SanitizeStderr(string(event.Raw))
			transcribeFailure(t.conv, event.Raw)
			t.emit(adapter.Event{Kind: adapter.EventActivity, Summary: event.Type})
		case "error":
			// Codex prints "Reconnecting... N/5" as an error while it retries; that, or any error a turn.completed follows, must not
			// withhold the answer. A transient notice is a warning at once; another error waits for what follows.
			errorEvent = adapter.SanitizeStderr(string(event.Raw))
			if transientError(failureMessage(event.Raw)) {
				transcribeNotice(t.conv, event.Raw, true)
			} else {
				if pendingError != nil {
					transcribeFailure(t.conv, pendingError)
				}
				pendingError = append(json.RawMessage(nil), event.Raw...)
			}
			t.emit(adapter.Event{Kind: adapter.EventActivity, Summary: event.Type})
		}
	}
	if err := scanner.Err(); err != nil {
		scanError = err
		// Once scanning stops, an active child could block forever writing
		// to the unread stdout pipe. End its process group before Wait.
		if t.cmd.Process != nil {
			_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	if pendingError != nil {
		failed = true
		transcribeFailure(t.conv, pendingError)
	}
	terminal := sawThread && completed && !failed && candidate != nil && scanError == nil
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
		exit.ErrorEvent = errorEvent
	}
	t.mu.Lock()
	exit.CancelIntent = t.cancel
	t.exit, t.err = exit, scanError
	t.mu.Unlock()
	t.emit(adapter.Event{Kind: adapter.EventProcessExit, Exit: &exit})
	close(t.events)
	close(t.done)
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
