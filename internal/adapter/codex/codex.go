// Package codex adapts one Codex CLI exec invocation to an aimebu turn.
package codex

import (
	"bufio"
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
	"strconv"
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
	runtimeDir, err := a.prepareRuntime(spec)
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
	if a.variant() == "docker" && spec.Mode == adapter.ModeNormal {
		endpoint, header, err := adapter.DockerMCPAt(spec.AgentToolsConfigPath, spec.Agent.ID, spec.AgentServerURL)
		if err != nil {
			return nil, err
		}
		config := "mcp_servers.aimebu_docker={url=" + strconv.Quote(endpoint) + ",http_headers={Authorization=" + strconv.Quote(header) + "}}"
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
	schemaPath, err := adapter.VisiblePath(spec, filepath.Join(runtimeDir, "schema.json"))
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
		cmd.Env = childEnv(runtimeDir)
	}
	cmd.Env = adapter.LauncherEnv(cmd.Env, spec.LauncherEnvRefs)
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
	t := &turn{id: spec.TurnID, expectedSession: spec.NativeSessionHandle, started: spec.NativeSessionStarted, cmd: cmd, events: make(chan adapter.Event, 128), done: make(chan struct{}), stderrCapture: adapter.JoinWriters(a.stderrCapture, spec.StderrSink), stdoutCapture: a.stdoutCapture}
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
	data, err := json.Marshal(struct {
		TurnID       string          `json:"turn_id"`
		AgentID      string          `json:"agent_id"`
		Notes        json.RawMessage `json:"notes"`
		Context      json.RawMessage `json:"context"`
		AllowedTools []string        `json:"allowed_tools"`
	}{spec.TurnID, spec.Agent.ID, spec.NotesJSON, prompt.WithoutOverrides(spec.ContextJSON), spec.AllowedTools})
	if err != nil {
		return nil, err
	}
	return []byte(promptFor(spec) + string(data)), nil
}

func childEnv(runtimeDir string) []string {
	allowed := map[string]bool{"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "LANG": true,
		"LC_ALL": true, "LC_CTYPE": true, "TERM": true, "TMPDIR": true, "OPENAI_API_KEY": true,
		"OPENAI_BASE_URL": true, "HTTPS_PROXY": true, "HTTP_PROXY": true, "NO_PROXY": true,
		"SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
		"DOCKER_HOST": true, "DOCKER_CONTEXT": true, "DOCKER_TLS_VERIFY": true, "DOCKER_CERT_PATH": true}
	out := []string{}
	if runtimeDir != "" {
		out = append(out, "CODEX_HOME="+runtimeDir)
	}
	for _, item := range os.Environ() {
		key, _, ok := strings.Cut(item, "=")
		if ok && allowed[key] {
			out = append(out, item)
		}
	}
	return out
}

var responseSchema = []byte(`{"type":"object","additionalProperties":false,"required":["messages"],"properties":{"messages":{"type":"array","maxItems":10,"items":{"type":"object","additionalProperties":false,"required":["room","body","reply_to","thread_root","attachments","ask","handoff","visual_plan","appendix_pages","needs_attention","proposed_answers","open_questions"],"properties":{"room":{"type":"string"},"body":{"type":"string"},"reply_to":{"type":["string","null"]},"thread_root":{"type":["string","null"]},"attachments":{"type":["array","null"],"items":{"type":"string"}},"ask":{"type":["object","null"],"additionalProperties":false,"required":["kind","text","options","free_text"],"properties":{"kind":{"type":"string","enum":["question","approval"]},"text":{"type":"string"},"options":{"type":["array","null"],"items":{"type":"string"}},"free_text":{"type":["boolean","null"]}}},"handoff":{"type":["boolean","null"]},"visual_plan":{"type":["array","null"],"maxItems":80,"items":{"type":"object","additionalProperties":false,"required":["id","type","title","data","order"],"properties":{"id":{"type":["string","null"]},"type":{"type":"string"},"title":{"type":["string","null"]},"data":{"type":"string"},"order":{"type":["integer","null"]}}}},"appendix_pages":{"type":["array","null"],"maxItems":10,"items":{"type":"object","additionalProperties":false,"required":["title","body"],"properties":{"title":{"type":["string","null"]},"body":{"type":"string"}}}},"needs_attention":{"type":["boolean","null"]},"proposed_answers":{"type":["array","null"],"maxItems":4,"items":{"type":"string"}},"open_questions":{"type":["array","null"],"maxItems":10,"items":{"type":"object","additionalProperties":false,"required":["question","description","options"],"properties":{"question":{"type":"string"},"description":{"type":["string","null"]},"options":{"type":"array","minItems":2,"maxItems":8,"items":{"type":"string"}}}}}}}}}}`)

var diagnosticSchema = []byte(`{"type":"object","additionalProperties":false,"required":["messages","reconciliation"],"properties":{"messages":{"type":"array","maxItems":0,"items":{"type":"object","additionalProperties":false,"required":["room","body"],"properties":{"room":{"type":"string"},"body":{"type":"string"}}}},"reconciliation":{"type":"object","additionalProperties":false,"required":["status","findings"],"properties":{"status":{"type":"string","enum":["clean","uncertain"]},"findings":{"type":"array","items":{"type":"string"}}}}}}`)

func (a *Adapter) prepareRuntime(spec adapter.TurnSpec) (string, error) {
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
	if err := writePrivate(filepath.Join(dir, "config.toml"), config); err != nil {
		return "", err
	}
	return dir, nil
}

func codexConfig(spec adapter.TurnSpec) ([]byte, error) {
	if spec.Mode != adapter.ModeNormal || spec.AgentToolsConfigPath == "" {
		return []byte("# aimebu private Codex runtime\n"), nil
	}
	data, err := os.ReadFile(spec.AgentToolsConfigPath)
	if err != nil {
		return nil, err
	}
	var source struct {
		Servers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &source); err != nil {
		return nil, err
	}
	server, ok := source.Servers["aimebu"]
	if !ok || !filepath.IsAbs(server.Command) || server.Env["AIMEBU2_URL"] == "" || server.Env["AIMEBU2_AGENT_KEY"] == "" || server.Env["AIMEBU2_AGENT_ID"] != spec.Agent.ID {
		return nil, errors.New("invalid private aimebu MCP config")
	}
	var b strings.Builder
	b.WriteString("[mcp_servers.aimebu]\ncommand = ")
	b.WriteString(strconv.Quote(server.Command))
	b.WriteString("\nargs = [")
	for i, arg := range server.Args {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(strconv.Quote(arg))
	}
	b.WriteString("]\n[mcp_servers.aimebu.env]\n")
	for _, key := range []string{"AIMEBU2_URL", "AIMEBU2_AGENT_ID", "AIMEBU2_AGENT_KEY"} {
		b.WriteString(key)
		b.WriteString(" = ")
		b.WriteString(strconv.Quote(server.Env[key]))
		b.WriteByte('\n')
	}
	if server.Env["AIMEBU2_INSECURE_SKIP_VERIFY"] == "1" {
		b.WriteString("AIMEBU2_INSECURE_SKIP_VERIFY = \"1\"\n")
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
	stderrCapture       io.Writer
	stdoutCapture       io.Writer
}

// LastOutput is when the harness last wrote any stdout line, parsed or not.
// The supervisor's idle deadline counts it as liveness, so a long generation
// that only streams partial output is not mistaken for a stall.
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
			_ = t.Kill()
		}
	}()
	return nil
}

func (t *turn) Kill() error {
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
	err := syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
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
			input, output := harness.CodexUsage(event)
			cached := harness.CodexCachedInput(event)
			// Codex reports input_tokens as the whole input, cached part included (cached_input_tokens is a share of it, as in the
			// Responses API it passes through). Every other adapter reports input and cache reads apart, and the usage views show both
			// lines, so the cached share is taken out of input here; otherwise a reader would count it twice.
			if input != nil && cached != nil {
				fresh := max(*input-*cached, 0)
				input = &fresh
			}
			t.emit(adapter.Event{Kind: adapter.EventUsage, Usage: &adapter.Usage{TokensIn: input, TokensOut: output, TokensCacheRead: cached}})
		case "turn.failed", "error":
			failed = true
			errorEvent = adapter.SanitizeStderr(string(event.Raw))
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
	terminal := sawThread && completed && !failed && candidate != nil && scanError == nil
	if terminal {
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
