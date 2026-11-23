// Package claude adapts one Claude Code print invocation to an aimebu turn.
package claude

import (
	"bufio"
	"cmp"
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
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/prompt"
	"github.com/hrubymar10/aimebu/v2/internal/cmdprobe"
	"github.com/hrubymar10/aimebu/v2/internal/harness"
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

// toolboxServer names the aimebu entry a normal turn passes with --mcp-config, so its tools are mcp__aimebu-agent__<tool>. It is not
// "aimebu": Claude applies a project's disabledMcpServers by name to every source, an inline --mcp-config entry included, and a person
// who switched off a v1 bus called aimebu in a project (in their terminal, or because it was superseded) would switch off the v2 toolbox
// of every agent working there with it. legacyToolsRule hides the tools of such a v1 server instead (a deny rule holds in
// bypassPermissions too), so a v1 bus the person still has enabled never reaches a v2 agent.
const (
	toolboxServer   = "aimebu-agent"
	legacyToolsRule = "mcp__aimebu"
)

// A harness-native background job (a run_in_background shell, the Monitor tool) does not wake the agent when it ends and dies with the
// process, so an agent that starts one and ends its turn waits for a wake that never comes. The aimebu run_command tool is the way to run a
// long command. normalDisallowedTools denies Monitor beside the v1 bus, and backgroundTasksEnv switches Claude Code's background tasks off:
// run_in_background is dropped from the Bash and Agent tools and a long command is no longer moved to the background on its own. The switch
// was checked in the Claude Code 2.1.329 bundle, where it is one of the two inputs of the check that hides run_in_background from the tool
// description and runs a command in the foreground when it is asked for.
const (
	normalDisallowedTools = legacyToolsRule + ",Monitor"
	backgroundTasksEnvVar = "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS"
	backgroundTasksEnv    = backgroundTasksEnvVar + "=1"
)

var systemDeltas = prompt.Deltas{
	Opening:     "Complete this aimebu turn from the JSON supplied on standard input.",
	Role:        "Obey the role instructions in context.role_body. " + prompt.UnchangedContext,
	Attachments: prompt.AttachmentGetTool,
	Output:      "Return only the JSON object described by the response schema: a messages array with room-targeted bodies, using attachments, ask, needs_attention, proposed_answers, open_questions, handoff, visual_plan, and appendix_pages only when appropriate. Set force:true on one message only when you intend to publish it despite a live mention of an agent outside that room. " + prompt.ReplyLinks + " A visual_plan block has id, type, title, data, and order; data is a JSON-encoded object string. Use appendix_pages for a collapsed Full plan. To ask a human through the inbox, include a room message with a nonempty body and \"ask\":{\"kind\":\"question\",\"text\":\"the question\",\"options\":[\"saved choice\"],\"free_text\":false}." + " " + prompt.HumanResponse("short choices"),
	Ack:         prompt.AckByReaction,
	Closing:     "Do not add a message-level \"type\" field. context.rooms is a snapshot at turn start. A room returned by create_room during this turn is a valid target after creation; use its returned ID. Target only rooms where you are a member. Continue after toolbox calls until you have returned the required messages; do not finish with an empty messages array when a question or handoff remains.",
}

var systemPrompt = prompt.Build(systemDeltas)

// turnInput is the JSON on standard input. The prompt overrides are in the system prompt already, so they are not repeated in it, and
// role_body_system (the full role body the system prompt carries) is stripped: the model reads the role instructions from the system
// prompt, so the key would only be turn tokens.
func turnInput(spec adapter.TurnSpec) ([]byte, error) {
	context := withoutRoleBodySystem(prompt.WithoutOverrides(spec.ContextJSON))
	return json.Marshal(struct {
		TurnID       string          `json:"turn_id"`
		Notes        json.RawMessage `json:"notes"`
		Context      json.RawMessage `json:"context"`
		AllowedTools []string        `json:"allowed_tools"`
		KeepAlive    string          `json:"keep_alive,omitempty"`
	}{spec.TurnID, spec.NotesJSON, context, spec.AllowedTools, keepAliveText(spec)})
}

// withoutRoleBodySystem removes context.role_body_system from a turn's context JSON. A context without the key (or unreadable) is returned
// untouched, byte for byte.
func withoutRoleBodySystem(contextJSON []byte) []byte {
	var fields map[string]json.RawMessage
	if len(contextJSON) == 0 || json.Unmarshal(contextJSON, &fields) != nil {
		return contextJSON
	}
	if _, ok := fields["role_body_system"]; !ok {
		return contextJSON
	}
	delete(fields, "role_body_system")
	stripped, err := json.Marshal(fields)
	if err != nil {
		return contextJSON
	}
	return stripped
}

// keepAliveText is the instruction of a keep-alive turn, in the user turn and not the system prompt: the system prompt is part of the cached prefix,
// and changing it would rewrite the cache for every real turn too.
func keepAliveText(spec adapter.TurnSpec) string {
	if !spec.KeepAlive {
		return ""
	}
	return "This is a cache keep-alive, not work. Reply with an empty messages array and do nothing else: no tools, no messages."
}

// KeepAliveTool says this adapter can run a keep-alive turn (adapter.KeepAliver).
func (a *Adapter) KeepAliveTool() string { return "claude" }

// systemPromptFor builds the --append-system-prompt argument. The role body comes from context.role_body_system, which the store puts on
// every turn of a Claude session, so the argument and the warm process fingerprint stay stable while the context's role_body shrinks to
// the unchanged marker; a context without the key (a correction or diagnostic turn) falls back to a string role_body. A marker is not a
// role body and adds no appendix.
func systemPromptFor(spec adapter.TurnSpec) string {
	var context struct {
		RoleBodySystem json.RawMessage `json:"role_body_system"`
		RoleBody       json.RawMessage `json:"role_body"`
	}
	base := prompt.ForContext(systemDeltas, systemPrompt, spec.ContextJSON)
	if json.Unmarshal(spec.ContextJSON, &context) != nil {
		return base
	}
	var roleText string
	switch {
	case len(context.RoleBodySystem) > 0 && json.Unmarshal(context.RoleBodySystem, &roleText) == nil && roleText != "":
	case len(context.RoleBody) > 0 && json.Unmarshal(context.RoleBody, &roleText) == nil && roleText != "":
	default:
		return base
	}
	return base + "\n\nRole instructions:\n" + roleText
}

var responseSchema = []byte(`{"type":"object","additionalProperties":false,"required":["messages"],"properties":{"messages":{"type":"array","maxItems":10,"items":{"type":"object","additionalProperties":false,"required":["room","body","reply_to","attachments","ask","handoff","force","visual_plan","appendix_pages","needs_attention","proposed_answers","open_questions"],"properties":{"room":{"type":"string"},"body":{"type":"string"},"reply_to":{"type":["string","null"]},"attachments":{"type":["array","null"],"items":{"type":"string"}},"ask":{"type":["object","null"],"additionalProperties":false,"required":["kind","text","options","free_text"],"properties":{"kind":{"type":"string","enum":["question","approval"]},"text":{"type":"string"},"options":{"type":"array","items":{"type":"string"}},"free_text":{"type":"boolean"}}},"handoff":{"type":["boolean","null"]},"force":{"type":["boolean","null"]},"visual_plan":{"type":["array","null"],"maxItems":80,"items":{"type":"object","additionalProperties":false,"required":["id","type","title","data","order"],"properties":{"id":{"type":["string","null"]},"type":{"type":"string"},"title":{"type":["string","null"]},"data":{"type":"string"},"order":{"type":["integer","null"]}}}},"appendix_pages":{"type":["array","null"],"maxItems":10,"items":{"type":"object","additionalProperties":false,"required":["title","body"],"properties":{"title":{"type":["string","null"]},"body":{"type":"string"}}}},"needs_attention":{"type":["boolean","null"]},"proposed_answers":{"type":["array","null"],"maxItems":4,"items":{"type":"string"}},"open_questions":{"type":["array","null"],"maxItems":10,"items":{"type":"object","additionalProperties":false,"required":["question","description","options"],"properties":{"question":{"type":"string"},"description":{"type":["string","null"]},"options":{"type":"array","minItems":2,"maxItems":8,"items":{"type":"string"}}}}}}}}}}`)

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
	l, err := a.prepare(spec, false)
	if err != nil {
		return nil, err
	}
	cmd := l.command(ctx)
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
	input := l.input
	go func() { _, _ = stdin.Write(input); _ = stdin.Close() }()
	t := &turn{id: spec.TurnID, cmd: cmd, expectedSession: spec.NativeSessionHandle, started: spec.NativeSessionStarted, forkOf: forkOf(spec), events: make(chan adapter.Event, 32), done: make(chan struct{}), stderrCapture: adapter.JoinWriters(a.stderrCapture, spec.StderrSink), conv: adapter.ConversationFor(spec)}
	go t.run(stdout, stderr)
	return t, nil
}

// launch is the harness process a spec asks for, before it is started.
type launch struct {
	name      string
	args      []string
	env       []string
	dir       string
	input     []byte // what a one-turn process reads on standard input
	sessionAt [2]int // args[sessionAt[0]:sessionAt[1]] say which session to continue: not part of what a warm process is started with
	agentKey  string // the agent's key as an environment entry: never in an argument list, and only a hash of it in a fingerprint
}

func (l *launch) command(ctx context.Context) *exec.Cmd {
	cmd := exec.CommandContext(ctx, l.name, l.args...)
	cmd.Dir = l.dir
	cmd.Env = l.env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// prepare works out the process for a spec; warm asks for one that reads its turns from standard input (stream-json) and stays up between them.
func (a *Adapter) prepare(spec adapter.TurnSpec, warm bool) (*launch, error) {
	if a.variant() == "native" && !a.AllowNative {
		return nil, fmt.Errorf("native execution disabled; use the docker variant")
	}
	if spec.TurnID == "" || spec.Agent.ID == "" || spec.CheckoutPath == "" || spec.Agent.Model == "" {
		return nil, fmt.Errorf("turn ID, agent ID, checkout path, and model required")
	}
	if spec.NativeSessionStarted && spec.NativeSessionHandle == "" {
		return nil, fmt.Errorf("resumed turn requires native session handle")
	}
	var agentKey []string
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
	if warm {
		// --replay-user-messages: the CLI repeats each input line when it reads it, which is how a result is matched to the turn it answers.
		args = append(args, "--input-format", "stream-json", "--replay-user-messages")
	}
	if spec.Agent.Model != "" {
		args = append(args, "--model", spec.Agent.Model)
	}
	if a.MaxBudgetUSD != "" {
		args = append(args, "--max-budget-usd", a.MaxBudgetUSD)
	}
	if spec.ForkSession && !spec.NativeSessionStarted {
		return nil, fmt.Errorf("a forked turn must resume an existing session")
	}
	sessionStart := len(args)
	if spec.NativeSessionStarted {
		args = append(args, "--resume", spec.NativeSessionHandle)
		if spec.ForkSession {
			// The session is worked on as a copy: Claude saves it under a new id (reported by confirmSession) and leaves the original as it is.
			args = append(args, "--fork-session")
		}
	} else if spec.NativeSessionHandle != "" {
		args = append(args, "--session-id", spec.NativeSessionHandle)
	}
	sessionEnd := len(args)
	if spec.Mode == adapter.ModeDiagnostic {
		args = append(args, "--strict-mcp-config", "--tools", "Read", "--disallowedTools", "Bash,Edit,Write", "--permission-mode", "dontAsk")
	} else {
		if spec.Mode == adapter.ModeNormal {
			// Not strict: the person's own MCP servers and connectors stay available to the agent, as in their terminal. The --mcp-config entry
			// is toolboxServer, and a v1 bus named aimebu, of any scope, is denied (see toolboxServer).
			args = append(args, "--disallowedTools", normalDisallowedTools)
			if a.variant() == "docker" {
				endpoint, header, err := adapter.DockerMCPAt(spec.AgentTools, spec.Agent.ID, spec.AgentServerURL)
				if err != nil {
					return nil, err
				}
				// Claude expands ${NAME} in a header from its own environment, so the key is not in the argument list: it travels in the
				// forwarded variable.
				agentKey = adapter.DockerAgentKeyEnv(header)
				config, err := json.Marshal(map[string]any{"mcpServers": map[string]any{toolboxServer: map[string]any{"type": "http", "url": endpoint, "headers": map[string]string{"Authorization": "Bearer ${" + adapter.AgentKeyEnv + "}"}}}})
				if err != nil {
					return nil, err
				}
				args = append(args, "--mcp-config", string(config))
			} else {
				// The bridge's entry is inline, and its key is a reference Claude expands from its own environment, where the turn puts it:
				// the key is in no argument list and no file.
				command, bridgeArgs, bridgeEnv, err := adapter.BridgeEntry(spec.AgentTools, spec.Agent.ID, "${"+adapter.AgentKeyEnv+"}", cmp.Or(spec.LaunchPath, spec.CheckoutPath))
				if err != nil {
					return nil, err
				}
				config, err := json.Marshal(map[string]any{"mcpServers": map[string]any{toolboxServer: map[string]any{"command": command, "args": bridgeArgs, "env": bridgeEnv}}})
				if err != nil {
					return nil, err
				}
				agentKey = []string{adapter.AgentKeyEnv + "=" + spec.AgentTools.Key}
				args = append(args, "--mcp-config", string(config))
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
	env := adapter.LauncherEnv(childEnv(), spec.LauncherEnvRefs)
	dir := spec.CheckoutPath
	if spec.LaunchPath != "" {
		// Claude keys a session by the folder it is started in as spelled, which it learns from PWD (the kernel only knows the resolved path).
		// Go does not add PWD when Env is set, so it is set here.
		dir = spec.LaunchPath
		env = append(env, "PWD="+spec.LaunchPath)
	}
	if a.variant() == "docker" {
		env = append(env, "CLAUDE_LAUNCH_FLAGS=")
	}
	env = append(env, agentKey...)
	env = append(env, backgroundTasksEnv)
	if a.variant() == "docker" {
		// docker exec carries only the variables the launcher is told to forward by name.
		env = forwardToDocker(env, backgroundTasksEnvVar)
	}
	full := adapter.LauncherArgs(spec, args)
	shift := len(full) - len(args) // what the launcher puts before the protocol arguments
	l := &launch{name: command, args: full, env: env, dir: dir, input: input, sessionAt: [2]int{shift + sessionStart, shift + sessionEnd}}
	if len(agentKey) > 0 {
		l.agentKey = agentKey[0]
	}
	return l, nil
}

// forwardToDocker adds name to the HARNESS_DOCKER_FORWARD_ENV list in env, creating the list when there is none.
func forwardToDocker(env []string, name string) []string {
	const key = "HARNESS_DOCKER_FORWARD_ENV="
	for i, item := range env {
		if list, ok := strings.CutPrefix(item, key); ok {
			if list == "" {
				env[i] = key + name
			} else {
				env[i] = key + list + "," + name
			}
			return env
		}
	}
	return append(env, key+name)
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
	forkOf          string // the session this turn forks: the first session id that differs from it is the copy's, reported once
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
	warm            *warmSession // set when the turn runs in a live process that outlives it
	// echoMarker is how this turn's own input begins (`{"turn_id":"…"`), set for a turn in a live process: there the CLI repeats each input
	// line it reads (--replay-user-messages), and only a result that follows that repeat answers this turn (see ignoreResult).
	echoMarker string
	// conv is the turn's conversation log (nil when it has none); see adapter.TurnSpec.ConversationSink.
	conv *adapter.Conversation
}

// LastOutput is when the harness last wrote any stdout line, parsed or not.
// The supervisor's idle deadline counts it as liveness, so a long generation
// that only streams partial output is not mistaken for a stall.
func (t *turn) LastOutput() time.Time {
	if t.warm != nil {
		return time.Unix(0, t.warm.lastOutput.Load())
	}
	return time.Unix(0, t.lastOutput.Load())
}

// reaped says whether the turn's process has been waited for: the turn's own, or the live session's.
func (t *turn) reaped() bool {
	if t.warm != nil {
		return t.warm.reap.Reaped()
	}
	return t.reap.Reaped()
}

func (t *turn) Events() <-chan adapter.Event { return t.events }

// Cancel ends the turn's process group. A turn whose process is already reaped, or whose group is already gone, has nothing left to cancel,
// so cancelling again, or after the exit, is a no-op (see adapter.SignalGroup).
func (t *turn) Cancel() error {
	t.mu.Lock()
	if t.reaped() { // nothing left to cancel, and no intent to record for a process that ended by itself
		t.mu.Unlock()
		return nil
	}
	t.cancel = true
	t.mu.Unlock()
	if t.cmd.Process == nil {
		return nil
	}
	return adapter.SignalGroup(t.cmd.Process.Pid, syscall.SIGKILL, t.reaped)
}

// Terminate asks the turn's process group to end (SIGTERM) and waits up to grace for it to go on its own; only then is it killed. A launcher in a
// container traps the signal, ends the session in the container and exits 143 once it is gone, which is how a caller tells a confirmed end from a
// kill. It records the cancel intent like Cancel. It returns whether the process ended on the terminate.
func (t *turn) Terminate(grace time.Duration) bool {
	t.mu.Lock()
	if t.reaped() {
		t.mu.Unlock()
		return true
	}
	t.cancel = true
	t.mu.Unlock()
	if t.cmd.Process != nil {
		_ = adapter.SignalGroup(t.cmd.Process.Pid, syscall.SIGTERM, t.reaped)
	}
	select {
	case <-t.done:
		return true
	case <-time.After(grace):
	}
	if t.cmd.Process != nil {
		_ = adapter.SignalGroup(t.cmd.Process.Pid, syscall.SIGKILL, t.reaped)
	}
	return false
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
	s := newRunState()
	for scanner.Scan() {
		t.lastOutput.Store(time.Now().UnixNano())
		t.handleLine(s, append([]byte(nil), scanner.Bytes()...))
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
	t.conclude(s, scanError)
	<-stderrDone
	err := t.cmd.Wait()
	t.reap.MarkReaped()
	exit := t.exitInfo(s, tail.String(), err)
	t.mu.Lock()
	t.exit, t.err = exit, scanError
	t.mu.Unlock()
	t.emit(adapter.Event{Kind: adapter.EventProcessExit, Exit: &exit})
}

// runState is what one turn has seen of the harness's output so far.
type runState struct {
	terminal                                                  bool
	lastAssistantContext                                      *int64
	steps                                                     map[string]claudeStep // the main loop's API responses seen so far, by message id
	sawResult                                                 bool
	candidate                                                 json.RawMessage
	errorResult, errorSummary                                 string
	errorIsResult, toolStarted, producedOutput, haveCandidate bool
	// echoed says the CLI has repeated this turn's input (a turn in a live process only); assistants counts the assistant events since the
	// last result or since the repeat, whichever came later.
	echoed     bool
	assistants int
}

// forgetQuery drops what the API responses of a query that does not answer this turn spent and did: the CLI's own query about a background
// task, which runs before this turn's input is read or after its answer.
func (s *runState) forgetQuery() {
	s.steps = map[string]claudeStep{}
	s.lastAssistantContext = nil
	s.assistants = 0
	s.toolStarted = false
}

func newRunState() *runState { return &runState{steps: map[string]claudeStep{}} }

// handleLine takes one line of the harness's standard output.
func (t *turn) handleLine(s *runState, line []byte) {
	event, known := harness.ParseClaude(line)
	if !known {
		t.transcribeUnknown(event, line)
		return
	}
	if compact, ok := harness.ParseClaudeCompaction(event); ok {
		t.emit(adapter.Event{Kind: adapter.EventCompaction, Compaction: &adapter.Compaction{PreTokens: compact.PreTokens, PostTokens: compact.PostTokens}})
		return
	}
	t.confirmSession(event.Raw)
	t.transcribe(event)
	switch event.Type {
	case "assistant", "tool_progress", "system", "rate_limit_event", "user":
		if event.Type == "user" && t.echoMarker != "" && !s.echoed && isEcho(event.Raw, t.echoMarker) {
			// Everything before the repeat of this turn's input belongs to a query the CLI ran on its own.
			s.echoed = true
			s.forgetQuery()
		}
		if event.Type == "rate_limit_event" && strings.Contains(strings.ToLower(string(event.Raw)), "limit reached") {
			s.errorResult = adapter.SanitizeStderr(string(event.Raw))
		}
		if event.Type == "assistant" {
			s.assistants++
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
				s.lastAssistantContext = &used
				// One API response is several assistant events that repeat its usage: each response counts once, by its message id.
				if id := assistant.Message.ID; id != "" {
					s.steps[id] = claudeStep{in: *assistant.Message.Usage.InputTokens, cacheRead: assistant.Message.Usage.CacheReadInputTokens, cacheWrite: assistant.Message.Usage.CacheCreationInputTokens}
				}
			}
		}
		activity := adapter.Event{Kind: adapter.EventActivity, Summary: event.Type}
		started, finished := harness.ClaudeToolUses(event)
		if len(started) > 0 {
			s.toolStarted = true
		}
		for _, id := range started {
			activity.Tools = append(activity.Tools, adapter.ToolEvent{ID: id})
		}
		for _, id := range finished {
			activity.Tools = append(activity.Tools, adapter.ToolEvent{ID: id, Done: true})
		}
		t.emit(activity)
	case "result":
		var result struct {
			Result           *string         `json:"result"`
			IsError          bool            `json:"is_error"`
			Subtype          string          `json:"subtype"`
			NumTurns         int64           `json:"num_turns"`
			SessionID        string          `json:"session_id"`
			StructuredOutput json.RawMessage `json:"structured_output"`
			Origin           struct {
				Kind string `json:"kind"`
			} `json:"origin"`
			Usage struct {
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
			return
		}
		empty := (result.Result == nil || *result.Result == "") && isZero(result.Usage.InputTokens) && isZero(result.Usage.OutputTokens) &&
			isZero(result.Usage.CacheReadInputTokens) && isZero(result.Usage.CacheCreationInputTokens)
		ignore := t.ignoreResult(s, result.IsError, result.Origin.Kind, empty)
		resultLen := 0
		if result.Result != nil {
			resultLen = len(*result.Result)
		}
		structured := len(result.StructuredOutput) > 0 && string(result.StructuredOutput) != "null"
		detail := fmt.Sprintf("result subtype=%s is_error=%t num_turns=%d result_len=%d structured_output=%t session_id=%s origin=%s assistant_events=%d",
			result.Subtype, result.IsError, result.NumTurns, resultLen, structured, result.SessionID, result.Origin.Kind, s.assistants)
		detail = adapter.SanitizeStderr(detail)
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
		if s.lastAssistantContext != nil {
			usage.ContextUsed = s.lastAssistantContext
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
		if ignore != "" {
			// The CLI's own query still spent tokens and money in the session: that is reported as a usage event of its own (the store
			// keeps one record per usage event, and the cost is the session's running total either way). A result that reports no spend at
			// all gets an activity event instead, so it leaves no empty usage record.
			if !empty || usage.CostEstimate != nil && *usage.CostEstimate > 0 {
				usage.Background = true
				t.emit(adapter.Event{Kind: adapter.EventUsage, Usage: usage, Detail: detail + " ignored: " + ignore})
			} else {
				t.emit(adapter.Event{Kind: adapter.EventActivity, Summary: "result", Detail: detail + " ignored: " + ignore})
			}
			s.forgetQuery()
			return
		}
		s.assistants = 0
		if result.IsError {
			s.errorResult = adapter.SanitizeStderr(string(event.Raw))
			s.errorIsResult = true
			if result.Result != nil {
				s.errorSummary = adapter.SanitizeStderr(*result.Result)
			}
			if result.Usage.OutputTokens != nil && *result.Usage.OutputTokens > 0 {
				s.producedOutput = true
			}
		}
		s.sawResult = true
		t.emit(adapter.Event{Kind: adapter.EventUsage, Usage: usage, Detail: detail})
		if result.Result != nil && !result.IsError {
			s.candidate = append(s.candidate[:0], (*result.Result)...)
			s.haveCandidate = true
		}
	}
}

// ignoreResult says why a result event does not answer this turn, or "" when it does. Claude Code runs queries of its own in the same
// session: when a background task of an earlier process ends or is found stopped, it reports it in a query that ends with a result of its own
// (with `origin.kind` "task-notification"), before this turn's input is read or after this turn's answer. In a live process such a result
// can come before this turn's input has been read at all, so there only a result after the CLI repeated the input counts. Wherever it comes,
// a result with that origin (another origin is not known to mean anything and is accepted, and journaled), or an empty one that spent nothing and followed no assistant event, is not this turn's answer. An error result
// always counts: it ends the process, and what it says (a provider limit, a failed sign-in) is the session's.
func (t *turn) ignoreResult(s *runState, isError bool, origin string, empty bool) string {
	switch {
	case isError:
		return ""
	case t.echoMarker != "" && !s.echoed:
		return "it came before the CLI read this turn's input"
	case origin == "task-notification":
		return "the CLI ran that query on its own (origin " + origin + ")"
	case empty && s.assistants == 0:
		return "it is empty, spent nothing and followed no assistant event"
	}
	return ""
}

func isZero(n *int64) bool { return n == nil || *n == 0 }

// isEcho says whether a user event is the CLI's repeat of the input that begins with marker: a top-level user message with a text block that
// begins with it.
func isEcho(raw json.RawMessage, marker string) bool {
	var event struct {
		ParentToolUseID *string `json:"parent_tool_use_id"`
		Message         struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &event) != nil || event.ParentToolUseID != nil {
		return false
	}
	var text string
	if json.Unmarshal(event.Message.Content, &text) == nil {
		return strings.HasPrefix(text, marker)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(event.Message.Content, &blocks) != nil {
		return false
	}
	for _, block := range blocks {
		if block.Type == "text" && strings.HasPrefix(block.Text, marker) {
			return true
		}
	}
	return false
}

// conclude ends a turn's events: what it spent without a result, and its answer if it ended with one.
func (t *turn) conclude(s *runState, scanError error) {
	// A turn that ended with no result (killed, cancelled, crashed) still spent what its API responses used. The input and cache tokens of
	// those responses are known; the output count and the cost are only in the result, so they stay unknown, and the record says it is partial.
	if !s.sawResult && len(s.steps) > 0 {
		usage := partialUsage(s.steps)
		// In a live process, steps before the repeat of this turn's input belong to a query the CLI ran on its own (see ignoreResult): what
		// they spent is reported, but as background usage, which proves nothing about the turn's input.
		usage.Background = t.echoMarker != "" && !s.echoed
		t.emit(adapter.Event{Kind: adapter.EventUsage, Usage: usage})
	}
	if s.haveCandidate && scanError == nil {
		t.emit(adapter.Event{Kind: adapter.EventFinalAnswer, Answer: s.candidate})
		s.terminal = true
	}
}

// exitInfo is the turn's exit evidence once its process has ended (err is what waiting for it returned).
func (t *turn) exitInfo(s *runState, tail string, err error) adapter.ExitInfo {
	exit := adapter.ExitInfo{TerminalEvent: s.terminal}
	if err != nil {
		if status, ok := err.(*exec.ExitError); ok {
			exit.ExitCode = status.ExitCode()
		} else {
			exit.ExitCode = 1
		}
	}
	if !s.terminal {
		exit.StderrTail = tail
		exit.ErrorEvent = s.errorResult
		if s.errorIsResult {
			// An explicit error result of a turn that ran no tool and produced no output (a failed sign-in, an expired session) failed before
			// doing anything: it is a definite failure, not an uncertain outcome.
			exit.ErrorSummary = s.errorSummary
			exit.DefiniteFailure = s.errorSummary != "" && !s.toolStarted && !s.producedOutput
		}
	}
	t.mu.Lock()
	exit.CancelIntent = t.cancel
	t.mu.Unlock()
	return exit
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

// forkOf is the original session of a forked turn, empty for any other.
func forkOf(spec adapter.TurnSpec) string {
	if spec.ForkSession {
		return spec.NativeSessionHandle
	}
	return ""
}

func (t *turn) confirmSession(raw json.RawMessage) {
	if t.forkOf != "" {
		var event struct {
			Type      string `json:"type"`
			Subtype   string `json:"subtype"`
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(raw, &event) != nil || event.SessionID == "" || event.SessionID == t.forkOf ||
			!(event.Type == "system" && event.Subtype == "init" || event.Type == "result") {
			return
		}
		t.forkOf = ""
		t.emit(adapter.Event{Kind: adapter.EventNativeSessionStarted, NativeSessionHandle: event.SessionID})
		return
	}
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
