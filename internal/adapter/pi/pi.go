// Package pi adapts one Pi print invocation to an aimebu turn.
package pi

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
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/prompt"
	"github.com/hrubymar10/aimebu/v2/internal/answer"
	"github.com/hrubymar10/aimebu/v2/internal/cmdprobe"
)

type Adapter struct {
	Command     string
	Variant     string
	AllowNative bool
	RuntimeRoot string
	version     string
}

func New(runtimeRoot string) *Adapter {
	return &Adapter{Command: "pi", Variant: "native", AllowNative: true, RuntimeRoot: runtimeRoot}
}

func NewDocker() *Adapter { return &Adapter{Command: "pi-docker", Variant: "docker"} }

func (a *Adapter) variant() string {
	if a.Variant == "" {
		return "native"
	}
	return a.Variant
}

func (a *Adapter) CatalogIdentity() (string, string) { return "pi", a.variant() }

func (a *Adapter) Catalog() []adapter.CatalogEntry {
	version := a.version
	if version == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, a.Command, "--version")
		if a.variant() == "docker" {
			cmd.Env = append(childEnv(""), "PI_LAUNCH_FLAGS=")
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
	return []adapter.CatalogEntry{{Tool: "pi", Version: version, Environment: adapter.Environment{Variant: a.variant(), OS: osName, Container: a.variant() == "docker"}}}
}

func (a *Adapter) Capabilities(version string, environment adapter.Environment) adapter.Capabilities {
	if version != "1.1.0" || environment.Variant != a.variant() || environment.OS != "linux" {
		return adapter.Capabilities{}
	}
	return adapter.Capabilities{NoActionMode: true, CompactionEvents: true, Cost: true, CancelConfirmed: a.variant() == "native"}
}

// ForgetAgent deletes the generated MCP config that holds the agent's key. The next start writes it again.
func (a *Adapter) ForgetAgent(agentID string) {
	if a.RuntimeRoot == "" {
		return
	}
	sum := sha256.Sum256([]byte(agentID))
	_ = os.Remove(filepath.Join(a.RuntimeRoot, hex.EncodeToString(sum[:]), "mcp.json"))
}

// SweepKeys deletes every generated MCP config under the runtime root. An older supervisor wrote the agent's key into one; the next turn of an
// agent writes its config again, which names a variable instead of holding the key.
func (a *Adapter) SweepKeys() {
	if a.RuntimeRoot == "" {
		return
	}
	matches, _ := filepath.Glob(filepath.Join(a.RuntimeRoot, "*", "mcp.json"))
	for _, path := range matches {
		_ = os.Remove(path)
	}
}

func (a *Adapter) Start(ctx context.Context, spec adapter.TurnSpec) (adapter.Turn, error) {
	if a.variant() == "native" && !a.AllowNative {
		return nil, errors.New("native execution disabled; use the docker variant")
	}
	if spec.TurnID == "" || spec.Agent.ID == "" || spec.CheckoutPath == "" || spec.Agent.Model == "" || spec.NativeSessionHandle == "" {
		return nil, errors.New("turn ID, agent ID, checkout path, model, and server session ID required")
	}
	if a.variant() == "docker" {
		if err := adapter.DockerCheckout(spec.CheckoutPath); err != nil {
			return nil, err
		}
	}
	sum := sha256.Sum256([]byte(spec.Agent.ID))
	sessionDir := filepath.Join(spec.CheckoutPath, ".aimebu", "pi-sessions", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		return nil, err
	}
	if err := adapter.ExcludeRuntimeState(spec.CheckoutPath); err != nil {
		return nil, err
	}
	visibleSessionDir, err := adapter.VisiblePath(spec, sessionDir)
	if err != nil {
		return nil, err
	}
	input, err := turnInput(spec)
	if err != nil {
		return nil, err
	}
	args := []string{"-p", "--mode", "json", "--model", spec.Agent.Model, "--session-id", spec.NativeSessionHandle,
		"--session-dir", visibleSessionDir, "--no-context-files", "--no-skills", "--no-approve"}
	command := a.Command
	if spec.LauncherPath != "" {
		command = spec.LauncherPath
	}
	var agentDir string
	var visibleAgentDir string // what the launcher's container sees, for a Docker turn
	var ownEnv []string        // variables the person's own MCP servers read, passed through to a native turn
	var endpoint string
	if spec.Mode == adapter.ModeNormal {
		// mcp__* names every MCP tool: Pi keeps an unmatched MCP tool registered but
		// inactive, and a direct one is never declared to the model until the allowlist
		// matches it (checked in Pi 1.1.0 and 1.6.0). The person's own servers are
		// exposed through codemode, which stays registered without being declared.
		tools := []string{"read", "bash", "edit", "write", "grep", "find", "ls", "mcp__*"}
		if a.variant() == "docker" {
			// The container runs the person's own Pi configuration, which may load the pi-mcp-adapter package instead of Pi's built-in MCP
			// support. The adapter names a server's tools <server>_<tool> (aimebu_run_command), not mcp__<server>__<tool>, so the
			// allowlist must match both spellings or the toolbox is connected but hidden from the model (observed live).
			tools = append(tools, "aimebu_*")
			// The aimebu entry reaches the container as a private mcp.json under the
			// mirrored checkout, in an agent directory that links the container's own
			// (models, login, packages) beside it; its header names the key's variable, which the launcher
			// hands over by name (HARNESS_DOCKER_FORWARD_ENV), so the key is in neither
			// argv nor a file.
			agentDir = filepath.Join(spec.CheckoutPath, ".aimebu", "pi", hex.EncodeToString(sum[:]))
			if err := os.MkdirAll(agentDir, 0700); err != nil {
				return nil, err
			}
			if err := os.Chmod(agentDir, 0700); err != nil {
				return nil, err
			}
			if visibleAgentDir, err = adapter.VisiblePath(spec, agentDir); err != nil {
				return nil, err
			}
			// The server address as the container sees it: loopback becomes host.docker.internal.
			var aerr error
			endpoint, _, aerr = adapter.DockerMCPAt(spec.AgentTools, spec.Agent.ID, spec.AgentServerURL)
			if aerr != nil {
				return nil, aerr
			}
			// The launcher must hand the configuration and the key to the harness by name;
			// an older one would start Pi without either.
			probe, cancel := context.WithTimeout(ctx, 10*time.Second)
			features, probeErr := adapter.DockerFeatures(probe, command, spec.LauncherArgs, spec.CheckoutPath, adapter.DockerBaseEnv())
			cancel()
			if probeErr == nil {
				probeErr = adapter.CheckDockerFeature(features, "Pi", command, adapter.FeatureForwardEnv)
			}
			if probeErr == nil {
				probeErr = adapter.CheckDockerFeature(features, "Pi", command, adapter.FeatureRunCommand)
			}
			if probeErr != nil {
				return nil, probeErr
			}
			// Pi's models, login and packages live in the agent directory, which this turn replaces: link the container's own entries in.
			if err := layerAgentDir(ctx, command, spec.LauncherArgs, spec.CheckoutPath, visibleAgentDir, dockerLauncherRefs(spec.LauncherEnvRefs)); err != nil {
				return nil, err
			}
		} else {
			if a.RuntimeRoot == "" {
				return nil, errors.New("Pi runtime root required")
			}
			agentDir = filepath.Join(a.RuntimeRoot, hex.EncodeToString(sum[:]))
			if err := os.MkdirAll(agentDir, 0700); err != nil {
				return nil, err
			}
			if err := os.Chmod(agentDir, 0700); err != nil {
				return nil, err
			}
			// The private directory would otherwise hide the person's providers and login: a model that only their
			// models.json defines would be "not found". Those two files are linked in; nothing else of theirs is.
			if err := linkUserFiles(agentDir, userAgentDir(), nativeLinked...); err != nil {
				return nil, err
			}
			var aerr error
			endpoint, _, aerr = adapter.NativeMCP(spec.AgentTools, spec.Agent.ID)
			if aerr != nil {
				return nil, aerr
			}
		}
		// The person's own servers (see user_mcp.go), then aimebu's entry, which replaces one of that name.
		servers := map[string]any{}
		own, envVars := userMCPServers(userAgentDir(), "aimebu", spec.NativeSessionHandle, a.variant() == "docker")
		for name, entry := range own {
			servers[name] = entry
		}
		ownEnv = envVars
		if len(own) > 0 {
			// A server of the person's is exposed through Pi's codemode tool unless its entry says otherwise, and the allowlist must name it.
			tools = append(tools, "codemode")
		}
		if adapter.OffersAnswerTool("pi", spec) {
			endpoint = adapter.AnswerToolEndpoint(endpoint)
		}
		servers["aimebu"] = map[string]any{"url": endpoint, "headers": map[string]string{"Authorization": "Bearer ${" + adapter.AgentKeyEnv + "}"}, "exposure": "direct"}
		config, err := json.Marshal(map[string]any{"mcpServers": servers})
		if err != nil {
			return nil, err
		}
		if err := writePrivate(filepath.Join(agentDir, "mcp.json"), config); err != nil {
			return nil, err
		}
		// The bounded Ollama Cloud model is supplied by the user's installed
		// provider extension. Load only that extension into this private Pi
		// agent directory; do not import the user's other packages.
		if a.variant() != "docker" {
			if extension := ollamaCloudExtension(); extension != "" {
				args = append(args, "--extension", extension)
			}
		}
		args = append(args, "--tools", strings.Join(tools, ","))
	} else {
		args = append(args, "--tools", "read,grep,find,ls", "--no-mcp")
	}
	cmd := exec.Command(command, adapter.LauncherArgs(spec, args)...)
	// Pi print mode accepts a prompt from piped stdin. Keep large batches out
	// of argv, where Linux caps each argument at 128 KiB.
	cmd.Stdin = bytes.NewReader(input)
	cmd.Dir = spec.CheckoutPath
	if a.variant() == "docker" {
		// The launcher's environment, not the harness's: it carries the configuration
		// directory and the key only as variables the launcher forwards by name, so
		// neither value can appear in argv. An owner's reference to one of these names
		// configures the launcher, not the agent, and is ignored.
		env := childEnv("")
		env = append(env, "PI_LAUNCH_FLAGS=", "PI_CODING_AGENT_DIR="+visibleAgentDir,
			adapter.AgentKeyEnv+"="+spec.AgentTools.Key)
		// One list names everything the launcher hands over by name; the key's name merges into it.
		env = adapter.AppendForwardEnv(env, "PI_CODING_AGENT_DIR", adapter.AgentKeyEnv)
		cmd.Env = adapter.LauncherEnv(env, dockerLauncherRefs(spec.LauncherEnvRefs))
	} else {
		cmd.Env = childEnv(agentDir, ownEnv...)
		cmd.Env = adapter.LauncherEnv(cmd.Env, spec.LauncherEnvRefs)
		if agentDir != "" && spec.AgentTools != nil {
			// mcp.json names the variable in aimebu's Authorization header (Pi expands it), so the key is only in this process's environment.
			cmd.Env = append(cmd.Env, adapter.AgentKeyEnv+"="+spec.AgentTools.Key)
		}
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
	t := &turn{id: spec.TurnID, expectedSession: spec.NativeSessionHandle, started: spec.NativeSessionStarted, resumedSession: spec.NativeSessionStarted,
		cmd: cmd, events: make(chan adapter.Event, 128), done: make(chan struct{}), stderrCapture: spec.StderrSink, conv: adapter.ConversationFor(spec)}
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

func ollamaCloudExtension() string {
	root := os.Getenv("PI_CODING_AGENT_DIR")
	if root == "" {
		if home, err := os.UserHomeDir(); err == nil {
			root = filepath.Join(home, ".pi", "agent")
		}
	}
	path := filepath.Join(root, "git", "github.com", "fgrehm", "pi-ollama-cloud", "index.ts")
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		return path
	}
	return ""
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

// turnPrompt is the instruction text ahead of the turn JSON on standard input.
var legacyOutput = prompt.ReturnOneObject + " Visual plan blocks have type, optional title, and JSON data; appendix pages have optional title and Markdown body. " + prompt.HumanResponse("choices")

var turnDeltas = prompt.Deltas{
	Opening:     "Complete this aimebu turn from the JSON below.",
	Role:        "Obey context.role_body.",
	Attachments: "use attachment_get when available. In Pi Docker, fetch an attachment ID from attachment_access.base_url plus the ID with an Authorization: Bearer header using attachment_access.bearer. Download only when needed to your own temporary directory; never echo the bearer.",
	Output:      prompt.ReturnOneObject + " End the turn by calling the aimebu submit_answer tool with that answer object. Pass every field required by its schema; use null when a field does not apply. If the tool is unavailable, return the JSON object as text. Answer schema: <json-schema>" + string(answer.ResponseSchema) + "</json-schema>. " + "Visual plan blocks have type, optional title, and JSON data; appendix pages have optional title and Markdown body." + " " + prompt.HumanResponse("choices"),
	Ack:         "Acknowledge only with a reaction when available; never send acknowledgement-only or status-only messages.",
	Closing:     prompt.OutsideRoomContext,
}

// turnPrompt is the shipped text; a turn whose context carries prompt overrides builds its own (see promptFor).
var turnPrompt = prompt.Build(turnDeltas)

func promptFor(spec adapter.TurnSpec) string {
	if !adapter.OffersAnswerTool("pi", spec) {
		legacy := turnDeltas
		legacy.Output = legacyOutput
		return prompt.ForContext(legacy, prompt.Build(legacy), spec.ContextJSON)
	}
	return prompt.ForContext(turnDeltas, turnPrompt, spec.ContextJSON)
}

func turnInput(spec adapter.TurnSpec) ([]byte, error) {
	var access map[string]string
	if spec.AttachmentBaseURL != "" && spec.AttachmentBearer != "" {
		access = map[string]string{"base_url": spec.AttachmentBaseURL, "bearer": spec.AttachmentBearer}
	}
	input, err := json.Marshal(struct {
		TurnID           string            `json:"turn_id"`
		Notes            json.RawMessage   `json:"notes"`
		Context          json.RawMessage   `json:"context"`
		AllowedTools     []string          `json:"allowed_tools"`
		AttachmentAccess map[string]string `json:"attachment_access,omitempty"`
	}{spec.TurnID, spec.NotesJSON, prompt.WithoutOverrides(spec.ContextJSON), spec.AllowedTools, access})
	if err != nil {
		return nil, err
	}
	return []byte(promptFor(spec) + string(input)), nil
}

func childEnv(agentDir string, extra ...string) []string {
	extra = adapter.UserEnvNames(extra)
	allowed := map[string]bool{"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "LANG": true,
		"LC_ALL": true, "LC_CTYPE": true, "TERM": true, "TMPDIR": true, "OLLAMA_API_KEY": true,
		"HTTPS_PROXY": true, "HTTP_PROXY": true, "NO_PROXY": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
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
	if agentDir != "" {
		out = append(out, "PI_CODING_AGENT_DIR="+agentDir)
	}
	return out
}

// dockerLauncherRefs drops an owner's references to the variables a Docker turn sets itself: its launch flags, the configuration directory and
// the key it forwards, and the launcher's own mode switches. A reference to one of them would configure the agent, or swap the agent's key for
// another variable's value, instead of configuring the launcher.
func dockerLauncherRefs(refs map[string]string) map[string]string {
	protected := map[string]bool{"PI_LAUNCH_FLAGS": true, "PI_CODING_AGENT_DIR": true}
	kept := make(map[string]string, len(refs))
	for target, source := range refs {
		if !protected[target] && !adapter.DockerControlEnvName(target) {
			kept[target] = source
		}
	}
	return kept
}

type turn struct {
	lastOutput          atomic.Int64 // unix nanos of the last stdout line
	id, expectedSession string
	started             bool
	resumedSession      bool
	cmd                 *exec.Cmd
	events              chan adapter.Event
	done                chan struct{}
	mu                  sync.Mutex
	cancel              bool
	reap                adapter.Reaper // set as soon as the process has been waited for, before the exit is reported
	exit                adapter.ExitInfo
	err                 error
	next                int
	// stderrCapture receives the harness's stderr as it is written (the per-agent output log); see adapter.TurnSpec.StderrSink.
	stderrCapture io.Writer
	// conv is the turn's conversation log (nil when it has none); see adapter.TurnSpec.ConversationSink.
	conv *adapter.Conversation
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
	go adapter.KillAfter(t.cmd.Process.Pid, 5*time.Second, t.done, t.reap.Reaped)
	return nil
}

func (t *turn) Wait() (adapter.ExitInfo, error) {
	<-t.done
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.exit, t.err
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
	go func() {
		_, _ = io.Copy(output, stderr)
		close(stderrDone)
	}()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	var candidate []byte
	var submitted []byte
	toolArgs := make(map[string]json.RawMessage)
	// lastError is the newest provider failure Pi reported. Pi retries
	// retryable failures itself and may exit 0 once the retries run out, so
	// without it the only evidence of a silent exit is an unrelated stderr
	// line.
	lastError := ""
	settled, aborted, sessionConfirmed, sawTurnEnd := false, false, false, false
	for scanner.Scan() {
		t.lastOutput.Store(time.Now().UnixNano())
		var event piEvent
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			if len(bytes.TrimSpace(scanner.Bytes())) > 0 {
				t.conv.Event("(not an event)")
			}
			continue
		}
		transcribe(t.conv, event, scanner.Bytes())
		switch event.Type {
		case "session":
			if event.ID != t.expectedSession {
				continue
			}
			sessionConfirmed = true
			if !t.started {
				t.started = true
				t.emit(adapter.Event{Kind: adapter.EventNativeSessionStarted, NativeSessionHandle: event.ID})
			}
		case "auto_retry_start":
			if event.ErrorMessage != "" {
				lastError = event.ErrorMessage
			}
		case "auto_retry_end":
			switch {
			case event.Success != nil && *event.Success:
				lastError = ""
			case event.FinalError != "":
				lastError = event.FinalError
			}
		case "message_end", "turn_end":
			if event.Message.Role == "assistant" && event.Message.StopReason == "error" && event.Message.ErrorMessage != "" {
				lastError = event.Message.ErrorMessage
			}
			if event.Message.Role == "assistant" {
				if event.Type == "turn_end" {
					sawTurnEnd = true
				}
				if text := assistantText(event.Message.Content); text != "" {
					candidate = []byte(text)
				}
				if event.Type == "turn_end" {
					t.emitUsage(event.Message.Usage)
				}
			}
		case "agent_settled":
			settled, aborted = true, event.Aborted
		case "tool_execution_start":
			if event.ToolName == "mcp__aimebu__submit_answer" || event.ToolName == "aimebu_submit_answer" {
				toolArgs[event.ToolCallID] = append(json.RawMessage(nil), event.Args...)
			}
			t.emit(adapter.Event{Kind: adapter.EventActivity, Summary: event.Type, Tools: []adapter.ToolEvent{{ID: event.ToolCallID}}})
		case "tool_execution_end":
			if args, ok := toolArgs[event.ToolCallID]; ok {
				if !event.IsError && len(args) > 0 && validAnswer(args) {
					submitted = append([]byte(nil), args...)
				}
				delete(toolArgs, event.ToolCallID)
			}
			t.emit(adapter.Event{Kind: adapter.EventActivity, Summary: event.Type, Tools: []adapter.ToolEvent{{ID: event.ToolCallID, Done: true}}})
		case "compaction_end":
			if !event.Aborted && event.Result.TokensBefore != nil && event.Result.EstimatedTokensAfter != nil {
				t.emit(adapter.Event{Kind: adapter.EventCompaction, Compaction: &adapter.Compaction{
					PreTokens: *event.Result.TokensBefore, PostTokens: *event.Result.EstimatedTokensAfter}})
			}
		default:
			if event.Type == "turn_start" {
				activity := adapter.Event{Kind: adapter.EventActivity, Summary: event.Type}
				switch event.Type {
				case "tool_execution_start":
					activity.Tools = []adapter.ToolEvent{{ID: event.ToolCallID}}
				case "tool_execution_end":
					activity.Tools = []adapter.ToolEvent{{ID: event.ToolCallID, Done: true}}
				}
				t.emit(activity)
			}
		}
	}
	if scanner.Err() != nil && t.cmd.Process != nil {
		_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
	}
	if submitted != nil {
		candidate = submitted
	}
	terminal := sessionConfirmed && sawTurnEnd && settled && !aborted && candidate != nil && scanner.Err() == nil
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
		exit.StderrTail = withoutSessionNotice(tail.String())
		if lastError != "" {
			exit.ErrorEvent = adapter.SanitizeStderr("Pi provider error: " + lastError)
		}
	}
	// Pi prints this notice for a fresh start too. Only a turn that was asked to
	// resume a native session has lost context when Pi creates it again.
	if t.resumedSession && strings.Contains(tail.String(), "Warning: No project session found with id '"+t.expectedSession+"'; creating a new session") {
		exit.SessionRecreated = true
	}
	t.mu.Lock()
	exit.CancelIntent = t.cancel
	t.exit, t.err = exit, scanner.Err()
	t.mu.Unlock()
	t.emit(adapter.Event{Kind: adapter.EventProcessExit, Exit: &exit})
}

// sessionNotice is what Pi prints when it creates the session v2 asked for
// by id. Every fresh turn prints it, so keeping it only misleads whoever reads
// the evidence of a failed turn.
var sessionNotice = regexp.MustCompile(`(?m)^Warning: No project session found with id '[^']*'; creating a new session with that id\.?[ \t]*\r?\n?`)

func withoutSessionNotice(stderr string) string {
	return strings.TrimSpace(sessionNotice.ReplaceAllString(stderr, ""))
}

type piEvent struct {
	Type       string          `json:"type"`
	ID         string          `json:"id"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Args       json.RawMessage `json:"args"`
	IsError    bool            `json:"isError"`
	Aborted    bool            `json:"aborted"`
	// ErrorMessage is set on auto_retry_start (the failure being retried);
	// FinalError and Success describe how auto_retry_end ended.
	ErrorMessage string `json:"errorMessage"`
	FinalError   string `json:"finalError"`
	Success      *bool  `json:"success"`
	Message      struct {
		Role         string          `json:"role"`
		Content      json.RawMessage `json:"content"`
		Usage        piUsage         `json:"usage"`
		StopReason   string          `json:"stopReason"`
		ErrorMessage string          `json:"errorMessage"`
	} `json:"message"`
	Result struct {
		TokensBefore         *int64 `json:"tokensBefore"`
		EstimatedTokensAfter *int64 `json:"estimatedTokensAfter"`
	} `json:"result"`
}

type piUsage struct {
	Input      *int64 `json:"input"`
	Output     *int64 `json:"output"`
	CacheRead  *int64 `json:"cacheRead"`
	CacheWrite *int64 `json:"cacheWrite"`
	Cost       struct {
		Total *float64 `json:"total"`
	} `json:"cost"`
}

func assistantText(raw json.RawMessage) string {
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var text strings.Builder
	for _, block := range blocks {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	return text.String()
}

func (t *turn) emitUsage(usage piUsage) {
	if usage.Input == nil && usage.Output == nil && usage.Cost.Total == nil {
		return
	}
	t.emit(adapter.Event{Kind: adapter.EventUsage, Usage: &adapter.Usage{
		TokensIn: usage.Input, TokensOut: usage.Output, TokensCacheRead: usage.CacheRead, TokensCacheWrite: usage.CacheWrite, CostEstimate: usage.Cost.Total}})
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

// validAnswer is the toolbox's own check: only an input that parses is adopted, so a rejected call falls back to the text candidate.
func validAnswer(input []byte) bool {
	_, err := answer.Parse(input, false)
	return err == nil
}
