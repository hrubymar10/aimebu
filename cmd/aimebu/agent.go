package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	aimebuclient "github.com/hrubymar10/aimebu/internal/client"
	"github.com/hrubymar10/aimebu/internal/config"
)

// agentNamePattern mirrors store.go's reclaimNamePattern.
var agentNamePattern = regexp.MustCompile(`^[a-z]{3,12}$`)

// agentSession is one entry in agents/agent-sessions.json.
type agentSession struct {
	CWD        string    `json:"cwd"`
	Harness    string    `json:"harness"`
	SessionID  string    `json:"session_id"`
	Name       string    `json:"name"`
	Model      string    `json:"model,omitempty"`
	Rooms      []string  `json:"rooms,omitempty"`
	AssumeRole string    `json:"assume_role,omitempty"`
	Command    []string  `json:"command"`
	LastUsed   time.Time `json:"last_used"`
}

const agentWarningMarker = "agent-warning-acknowledged"

type agentRecoveryClass string

const (
	agentRecoveryNormalEnd          agentRecoveryClass = "normal_end"
	agentRecoveryRegistrationLost   agentRecoveryClass = "registration_lost"
	agentRecoveryCodexThreadMissing agentRecoveryClass = "codex_thread_not_found"
	agentRecoveryServerUnreachable  agentRecoveryClass = "server_unreachable"
)

const (
	agentRecoveryFailureCap = 5
	agentRecoveryMaxBackoff = 16 * time.Second
)

var agentRegistrationLookupTimeout = 30 * time.Second

func agentRegistrationMissingError(harness string) error {
	listCommand := "the harness MCP server list"
	docsPath := "the harness documentation"
	switch harness {
	case "claude-code":
		listCommand = "claude mcp list"
		docsPath = "docs/claude-code.md"
	case "codex":
		listCommand = "codex mcp list"
		docsPath = "docs/codex.md"
	case "pi":
		listCommand = "cat ~/.pi/agent/mcp.json"
		docsPath = "docs/pi.md"
	}
	return fmt.Errorf("spawned %s session did not call `bus_register` -- verify `%s` shows aimebu and points at an executable reachable from the harness process. See %s", harness, listCommand, docsPath)
}

var (
	agentErrInterrupted        = errors.New("agent interrupted")
	agentCodexThreadNotFoundRE = regexp.MustCompile(`thread [0-9A-Za-z-]+ not found`)
	agentRoleKeyPattern        = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
)

const agentWarningText = `WARNING: aimebu agent runs the wrapped harness with
--dangerously-skip-permissions, which bypasses ALL permission
checks for the agent's tool calls. The agent will execute any
instructions it receives via the bus — including from other
agents you don't fully trust.

You are responsible for any risks and harms this may cause.

Type "yes" to acknowledge and proceed (this prompt won't appear
again — delete %s to re-enable):`

// agentInit migrates agent-owned state once per process startup. Migration
// failures are warnings, not fatal, so transient FS issues do not brick
// aimebu agent before the user can even answer the warning prompt.
func agentInit() {
	if err := config.MigrateAgents(config.Root()); err != nil {
		fmt.Fprintf(os.Stderr, "aimebu agent: failed to migrate agent state: %v\n", err)
	}
}

// agentCheckWarning checks for the first-run acknowledgement marker and
// prompts the user if it is absent. Exits if the user declines.
func agentCheckWarning() {
	marker := filepath.Join(config.AgentsDir(), agentWarningMarker)
	if _, err := os.Stat(marker); err == nil {
		return // already acknowledged
	}

	fmt.Fprintf(os.Stderr, agentWarningText+"\n", marker)
	fmt.Fprint(os.Stderr, "> ")

	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() || strings.TrimSpace(scanner.Text()) != "yes" {
		fmt.Fprintln(os.Stderr, "Aborted.")
		os.Exit(1)
	}

	_ = os.MkdirAll(filepath.Dir(marker), 0o700)
	f, err := os.Create(marker)
	if err == nil {
		fmt.Fprintln(f, time.Now().UTC().Format(time.RFC3339))
		f.Close()
	}
}

// harnessDetect maps command basenames to harness slugs.
var harnessDetect = map[string]string{
	"claude":        "claude-code",
	"claude-docker": "claude-code",
	"codex":         "codex",
	"codex-docker":  "codex",
	"cursor":        "cursor",
	"cline":         "cline",
	"aider":         "aider",
	"pi":            "pi",
	"pi-docker":     "pi",
}

func agentCmd(args []string) {
	agentInit()
	agentCheckWarning()

	harness := ""
	var rooms []string
	var command []string
	name := ""       // --name
	resumeID := ""   // --resume-id
	resumeName := "" // --resume-name
	autoRoom := false
	assumeRole := ""
	modelSlug := ""

	i := 0
	for i < len(args) {
		switch args[i] {
		case "--harness":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "aimebu agent: --harness requires a value")
				os.Exit(1)
			}
			harness = args[i+1]
			i += 2
		case "--room":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "aimebu agent: --room requires a value")
				os.Exit(1)
			}
			rooms = append(rooms, args[i+1])
			i += 2
		case "--auto-room":
			autoRoom = true
			i++
		case "--name":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "aimebu agent: --name requires a value")
				os.Exit(1)
			}
			name = args[i+1]
			i += 2
		case "--assume-role":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "aimebu agent: --assume-role requires a value")
				os.Exit(1)
			}
			assumeRole = args[i+1]
			i += 2
		case "--model":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "aimebu agent: --model requires a value")
				os.Exit(1)
			}
			modelSlug = args[i+1]
			i += 2
		case "--resume-id":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "aimebu agent: --resume-id requires a value")
				os.Exit(1)
			}
			resumeID = args[i+1]
			i += 2
		case "--resume-name":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "aimebu agent: --resume-name requires a value")
				os.Exit(1)
			}
			resumeName = args[i+1]
			i += 2
		case "--":
			command = args[i+1:]
			i = len(args)
		default:
			switch {
			case strings.HasPrefix(args[i], "--harness="):
				harness = strings.TrimPrefix(args[i], "--harness=")
				i++
			case strings.HasPrefix(args[i], "--room="):
				rooms = append(rooms, strings.TrimPrefix(args[i], "--room="))
				i++
			case args[i] == "--auto-room=true":
				autoRoom = true
				i++
			case args[i] == "--auto-room=false":
				autoRoom = false
				i++
			case strings.HasPrefix(args[i], "--name="):
				name = strings.TrimPrefix(args[i], "--name=")
				i++
			case strings.HasPrefix(args[i], "--assume-role="):
				assumeRole = strings.TrimPrefix(args[i], "--assume-role=")
				i++
			case strings.HasPrefix(args[i], "--model="):
				modelSlug = strings.TrimPrefix(args[i], "--model=")
				i++
			case strings.HasPrefix(args[i], "--resume-id="):
				resumeID = strings.TrimPrefix(args[i], "--resume-id=")
				i++
			case strings.HasPrefix(args[i], "--resume-name="):
				resumeName = strings.TrimPrefix(args[i], "--resume-name=")
				i++
			default:
				fmt.Fprintf(os.Stderr, "aimebu agent: unknown flag: %s\n", args[i])
				agentUsage()
				os.Exit(1)
			}
		}
	}

	var err error
	rooms, err = agentResolveRooms(rooms, autoRoom)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aimebu agent: %v\n", err)
		os.Exit(1)
	}

	// Validate flag combinations.
	if resumeID != "" && resumeName != "" {
		fmt.Fprintln(os.Stderr, "aimebu agent: --resume-id and --resume-name are mutually exclusive")
		os.Exit(1)
	}
	if resumeName != "" && name != "" {
		fmt.Fprintln(os.Stderr, "aimebu agent: --resume-name and --name cannot be used together")
		os.Exit(1)
	}
	if name != "" && !agentNamePattern.MatchString(name) {
		fmt.Fprintf(os.Stderr, "aimebu agent: --name %q must match [a-z]{3,12}\n", name)
		os.Exit(1)
	}
	if resumeName != "" && !agentNamePattern.MatchString(resumeName) {
		fmt.Fprintf(os.Stderr, "aimebu agent: --resume-name %q must match [a-z]{3,12}\n", resumeName)
		os.Exit(1)
	}
	if assumeRole != "" && !agentRoleKeyPattern.MatchString(assumeRole) {
		fmt.Fprintf(os.Stderr, "aimebu agent: --assume-role %q must match ^[a-z][a-z0-9_-]*$\n", assumeRole)
		os.Exit(1)
	}
	if assumeRole != "" && len(rooms) != 1 && resumeID == "" && resumeName == "" {
		fmt.Fprintln(os.Stderr, "aimebu agent: --assume-role requires exactly one resolved launch room via --room or --auto-room")
		os.Exit(1)
	}

	if len(command) == 0 {
		fmt.Fprintln(os.Stderr, "aimebu agent: command is required after --")
		agentUsage()
		os.Exit(1)
	}

	if harness == "" {
		base := filepath.Base(command[0])
		h, ok := harnessDetect[base]
		if !ok {
			fmt.Fprintf(os.Stderr, "aimebu agent: cannot detect harness from %q.\nUse --harness <slug> (e.g. --harness claude-code).\n", base)
			os.Exit(1)
		}
		harness = h
	}

	switch harness {
	case "claude-code", "codex", "pi":
		// supported
	default:
		fmt.Fprintf(os.Stderr, "aimebu agent: harness %q is not yet supported.\nCurrently supported: claude-code (claude, claude-docker), codex (codex, codex-docker), pi (pi, pi-docker).\n", harness)
		os.Exit(1)
	}

	aimebuURL := os.Getenv("AIMEBU_URL")
	if aimebuURL == "" {
		aimebuURL = "http://localhost:9997"
	}

	spawnTag := agentGenTag()
	resumeMode := "bootstrap"
	switch {
	case resumeID != "":
		resumeMode = "resume-id"
	case resumeName != "":
		resumeMode = "resume-name"
	}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// --- Resume path: skip bootstrap entirely ---
	if resumeID != "" || resumeName != "" {
		sessions, err := agentLoadSessions()
		if err != nil {
			fmt.Fprintf(os.Stderr, "aimebu agent: failed to load sessions file: %v\n", err)
			os.Exit(1)
		}
		entry, err := agentResolveResume(resumeID, resumeName, name, harness, sessions)
		if err != nil {
			fmt.Fprintln(os.Stderr, "aimebu agent:", err)
			os.Exit(1)
		}
		if autoRoom {
			entry.Rooms, err = agentResolveRooms(entry.Rooms, true)
			if err != nil {
				fmt.Fprintf(os.Stderr, "aimebu agent: %v\n", err)
				os.Exit(1)
			}
		}
		if assumeRole == "" {
			assumeRole = entry.AssumeRole
		}
		if modelSlug == "" {
			modelSlug = entry.Model
		}
		if harness == "pi" && modelSlug == "" {
			modelSlug = agentHarvestPiDefaultModel(os.Environ())
		}
		if assumeRole != "" && len(entry.Rooms) != 1 {
			fmt.Fprintln(os.Stderr, "aimebu agent: --assume-role requires exactly one saved launch room")
			os.Exit(1)
		}
		debug := newAgentDebugLog(entry.Name, spawnTag)
		defer debug.close()
		agentLogWrapperStart(debug, args, harness, entry.Rooms, spawnTag, resumeMode, aimebuURL, os.Getenv("AIMEBU_HARNESS"))
		childEnv := agentBuildEnv(aimebuURL, harness, spawnTag)
		fmt.Fprintf(os.Stderr, "aimebu agent: resuming session %s as %s\n", entry.SessionID, entry.Name)
		agentPushState(aimebuURL, agentFullID(entry.Name), "bootstrapping")
		agentResumeLoop(harness, command, entry.SessionID, entry.Name, entry.Rooms, assumeRole, modelSlug, childEnv, aimebuURL, sigCh, debug)
		return
	}

	// --- Bootstrap path ---
	debug := newAgentDebugLog(name, spawnTag)
	defer debug.close()
	agentLogWrapperStart(debug, args, harness, rooms, spawnTag, resumeMode, aimebuURL, os.Getenv("AIMEBU_HARNESS"))

	httpc := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpc.Get(aimebuURL + "/health")
	if err != nil || resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "aimebu agent: server unreachable at %s. Start it first:\n  aimebu server start\n", aimebuURL)
		os.Exit(1)
	}
	resp.Body.Close()

	childEnv := agentBuildEnv(aimebuURL, harness, spawnTag)

	if harness == "pi" && modelSlug == "" {
		modelSlug = agentHarvestPiDefaultModel(os.Environ())
	}

	prompt := agentBuildBootstrapPrompt(aimebuURL, harness, spawnTag, rooms, name, assumeRole, modelSlug)

	spawnLog := fmt.Sprintf("aimebu agent: spawning %s (harness=%s", filepath.Base(command[0]), harness)
	if len(rooms) > 0 {
		spawnLog += ", rooms=" + strings.Join(rooms, ",")
	}
	if name != "" {
		spawnLog += ", name=" + name
	}
	fmt.Fprintln(os.Stderr, spawnLog+")…")

	sessionID, agentName, err := agentBootstrapSession(harness, command, prompt, modelSlug, childEnv, aimebuURL, spawnTag, name, sigCh, debug)
	if errors.Is(err, agentErrInterrupted) {
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "aimebu agent: bootstrap failed: %v\n", err)
		os.Exit(1)
	}

	if agentName != "" {
		fmt.Fprintf(os.Stderr, "aimebu agent: session %s, agent %s — listening\n", sessionID, agentName)
	} else {
		fmt.Fprintf(os.Stderr, "aimebu agent: session %s — listening\n", sessionID)
	}

	if agentName != "" {
		cwd, _ := os.Getwd()
		_ = agentSaveSession(agentSession{
			CWD:        cwd,
			Harness:    harness,
			SessionID:  sessionID,
			Name:       agentName,
			Model:      modelSlug,
			Rooms:      append([]string(nil), rooms...),
			AssumeRole: assumeRole,
			Command:    command,
			LastUsed:   time.Now().UTC(),
		})
	}

	agentResumeLoop(harness, command, sessionID, agentName, rooms, assumeRole, modelSlug, childEnv, aimebuURL, sigCh, debug)
}

func agentResolveRooms(rooms []string, autoRoom bool) ([]string, error) {
	resolved := append([]string(nil), rooms...)
	if !autoRoom {
		return resolved, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("--auto-room failed to determine cwd: %w", err)
	}
	room, err := agentRoomFromCWD(cwd)
	if err != nil {
		return nil, err
	}
	if slices.Contains(resolved, room) {
		return resolved, nil
	}
	return append(resolved, room), nil
}

func agentRoomFromCWD(cwd string) (string, error) {
	room := filepath.Base(filepath.Clean(cwd))
	if room == "" || room == "." || room == string(filepath.Separator) {
		return "", fmt.Errorf("--auto-room could not derive a room name from cwd %q", cwd)
	}
	return room, nil
}

// agentLookupName polls GET /agents until it finds an AI agent whose
// meta.spawn_tag matches spawnTag. Returns the agent ID or "" after timeout.
func agentLookupName(aimebuURL, spawnTag string, timeout time.Duration) string {
	if timeout <= 0 {
		return ""
	}
	type agent struct {
		ID   string            `json:"id"`
		Kind string            `json:"kind"`
		Meta map[string]string `json:"meta"`
	}
	type agentsResp struct {
		Agents []agent `json:"agents"`
	}

	httpc := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(timeout)
	sleep := func() {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		if remaining > 250*time.Millisecond {
			remaining = 250 * time.Millisecond
		}
		time.Sleep(remaining)
	}

	for time.Now().Before(deadline) {
		resp, err := httpc.Get(aimebuURL + "/agents")
		if err != nil {
			sleep()
			continue
		}
		var ar agentsResp
		_ = json.NewDecoder(resp.Body).Decode(&ar)
		resp.Body.Close()

		for _, a := range ar.Agents {
			if a.Kind == "ai" && a.Meta["spawn_tag"] == spawnTag {
				return a.ID
			}
		}
		sleep()
	}
	return ""
}

// agentGenTag returns a random 16-char hex string used to identify the agent
// spawned by this wrapper invocation, unambiguously even when multiple
// wrappers run concurrently.
func agentGenTag() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// Fallback: use timestamp nanoseconds in hex.
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// claudeCodeAuthEnvVars holds the CLAUDE_CODE_* env vars that must not be
// stripped — they carry authentication credentials the child process needs.
var claudeCodeAuthEnvVars = map[string]bool{
	"CLAUDE_CODE_OAUTH_TOKEN": true,
	"CLAUDE_CODE_USE_BEDROCK": true,
	"CLAUDE_CODE_USE_VERTEX":  true,
}

func agentBuildEnv(aimebuURL, harness, spawnTag string) []string {
	out := make([]string, 0, len(os.Environ())+5)
	for _, e := range os.Environ() {
		key, _, _ := strings.Cut(e, "=")
		// Strip keys set explicitly below.
		switch key {
		case "AIMEBU_URL", "AIMEBU_HARNESS", "AIMEBU_AGENT_PROTOCOL", "AIMEBU_AGENT_SPAWN_TAG", "MCP_CONNECTION_NONBLOCKING":
			continue
		}
		// Strip CLAUDE_CODE_* env vars to prevent nested-session identity leaks,
		// except auth credentials which the child process needs.
		if strings.HasPrefix(key, "CLAUDE_CODE_") && !claudeCodeAuthEnvVars[key] {
			continue
		}
		// Strip vars that leak debugger state into child processes.
		if key == "NODE_OPTIONS" || key == "VSCODE_INSPECTOR_OPTIONS" {
			continue
		}
		out = append(out, e)
	}
	out = append(out,
		"AIMEBU_URL="+aimebuURL,
		"AIMEBU_HARNESS="+harness,
		"AIMEBU_AGENT_PROTOCOL=agent",
		"MCP_CONNECTION_NONBLOCKING=true",
	)
	if spawnTag != "" {
		out = append(out, "AIMEBU_AGENT_SPAWN_TAG="+spawnTag)
	}
	return out
}

// agentGenSessionID returns a random UUID v4 string for use as --session-id.
// Session IDs are pre-generated driver-side (VS Code extension pattern) so
// they are known before the child process runs and no output parsing is needed.
func agentGenSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		ns := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(ns >> uint(i*4))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// agentParseCodexThreadID scans the first 20 lines of codex bootstrap output
// for the thread.started event. Returns the thread ID and the 1-based line
// index, or ("", -1) if not found.
func agentParseCodexThreadID(output []byte) (string, int) {
	n := 0
	for line := range strings.SplitSeq(string(output), "\n") {
		if n >= 20 {
			break
		}
		n++
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
		}
		if json.Unmarshal([]byte(line), &r) == nil && r.Type == "thread.started" && r.ThreadID != "" {
			return r.ThreadID, n
		}
	}
	return "", -1
}

func agentEnvValue(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if v, ok := strings.CutPrefix(entry, prefix); ok {
			return v
		}
	}
	return ""
}

func agentHarvestPiDefaultModel(env []string) string {
	agentDir := ""
	for _, entry := range env {
		if v, ok := strings.CutPrefix(entry, "PI_CODING_AGENT_DIR="); ok {
			agentDir = strings.TrimSpace(v)
			break
		}
	}
	if agentDir == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		agentDir = filepath.Join(home, ".pi", "agent")
	}
	data, err := os.ReadFile(filepath.Join(agentDir, "settings.json"))
	if err != nil {
		return ""
	}
	var settings struct {
		DefaultModel string `json:"defaultModel"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return ""
	}
	model := strings.TrimSpace(settings.DefaultModel)
	if model == "" {
		return ""
	}
	return model
}

// ── Spawn prompt templates ─────────────────────────────────────────
// These are the compiled-in defaults. The running server may override any of
// these via /settings/prompts; agentFetchPromptTemplate falls back to the
// compiled default if the server is unreachable or the key is unknown.
//
// Tokens substituted at runtime by agentApplyPromptTokens:
//   {{force_name}}    — quoted agent name (fmt.Sprintf("%q", name))
//   {{harness}}       — quoted harness slug (fmt.Sprintf("%q", harness))
//   {{meta_json}}     — raw meta JSON (e.g. {"protocol":"agent","spawn_tag":"…"})
//   {{rooms_section}} — "Join these rooms: X.\n\n" when rooms are set, else ""
//   {{assume_role_section}} — optional role-consumption instructions.
//   {{model_instruction}} — optional instruction for harnesses with wrapper-known model metadata.

const agentBootstrapTemplate = `You're an aimebu bus agent. Register via the bus_register MCP tool (model=<your model slug>, harness={{harness}}, meta={{meta_json}}). The server will assign you a name.
{{model_instruction}}

{{rooms_section}}{{assume_role_section}}Then call bus_wait (no room argument — that way you receive DMs and traffic across all your rooms) to block on incoming messages. Respond per the etiquette in the MCP server-instructions. Keep listening (re-call bus_wait every time it returns with keep_waiting=true) until the user explicitly tells you to stop.`

const agentBootstrapReclaimTemplate = `You're an aimebu bus agent. Register via the bus_register MCP tool with name={{force_name}}, force=true, model=<your model slug>, harness={{harness}}, meta={{meta_json}}. This force-claims that slug in the current project.
{{model_instruction}}

{{rooms_section}}{{assume_role_section}}Then call bus_wait (no room argument — that way you receive DMs and traffic across all your rooms) to block on incoming messages. Respond per the etiquette in the MCP server-instructions. Keep listening (re-call bus_wait every time it returns with keep_waiting=true) until the user explicitly tells you to stop.`

const agentRecoveryTemplate = `You're an aimebu bus agent recovering a stale bus session. Register via the bus_register MCP tool with name={{force_name}}, force=true, model=<your model slug>, harness={{harness}}, meta={{meta_json}}. This force-claims that slug in the current project so the wrapper can recover the saved full identity.
{{model_instruction}}

{{rooms_section}}{{assume_role_section}}Then call bus_wait (no room argument — that way you receive DMs and traffic across all your rooms) to block on incoming messages. Respond per the etiquette in the MCP server-instructions. Keep listening (re-call bus_wait every time it returns with keep_waiting=true) until the user explicitly tells you to stop.`

// AgentBuiltinSpawnDefaults returns the compiled-in default bodies for the
// three agent spawn prompt keys. Called from main.go to register defaults
// with the server before it starts.
func AgentBuiltinSpawnDefaults() map[string]string {
	return map[string]string{
		"agent.bootstrap":         agentBootstrapTemplate,
		"agent.bootstrap_reclaim": agentBootstrapReclaimTemplate,
		"agent.recovery":          agentRecoveryTemplate,
	}
}

// agentFetchPromptTemplate fetches the configured template for key from the
// running server. Falls back to fallback if the server is unreachable or the
// key is not found.
func agentFetchPromptTemplate(aimebuURL, key, fallback string) string {
	// Use a short timeout so a wedged server degrades to the compiled default
	// rather than blocking agent startup indefinitely.
	httpClient := &http.Client{Timeout: 3 * time.Second}
	resp, err := httpClient.Get(aimebuURL + "/settings/prompts")
	if err != nil {
		return fallback
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var entries []struct {
		Key  string `json:"key"`
		Body string `json:"body"`
	}
	if json.Unmarshal(body, &entries) != nil {
		return fallback
	}
	for _, e := range entries {
		if e.Key == key {
			return e.Body // empty string is a valid user override
		}
	}
	return fallback
}

// agentApplyPromptTokens substitutes {{tokens}} in a prompt template.
func agentApplyPromptTokens(template, harness, metaJSON, forceName, roomsSection, assumeRoleSection, modelInstruction string) string {
	r := strings.NewReplacer(
		"{{harness}}", fmt.Sprintf("%q", harness),
		"{{meta_json}}", metaJSON,
		"{{force_name}}", fmt.Sprintf("%q", forceName),
		"{{rooms_section}}", roomsSection,
		"{{assume_role_section}}", assumeRoleSection,
		"{{model_instruction}}", modelInstruction,
	)
	out := r.Replace(template)
	if modelInstruction != "" && !strings.Contains(template, "{{model_instruction}}") {
		out = strings.TrimRight(out, "\n") + "\n" + modelInstruction
	}
	return out
}

func agentAssumeRoleSection(roleKey string, rooms []string) string {
	if roleKey == "" {
		return ""
	}
	room := ""
	if len(rooms) == 1 {
		room = rooms[0]
	}
	return "Assume-role opt-in: after registering and joining room " + fmt.Sprintf("%q", room) + ", assign yourself that room role by calling bus_role_assign with your full agent ID and role_key " + fmt.Sprintf("%q", roleKey) + ". If assignment succeeds, immediately call bus_role_get for that room and internalize the returned instructions before listening. If assignment fails, immediately send one concise room message describing the failed role assignment and the returned error, then keep listening.\n\n"
}

// agentBuildBootstrapPrompt builds the prompt for the initial bootstrap session.
// When forceName is set, the agent is instructed to force-claim that
// project-scoped slug.
func agentBuildBootstrapPrompt(aimebuURL, harness, spawnTag string, rooms []string, forceName, assumeRole, modelSlug string) string {
	roomsSection := ""
	if len(rooms) > 0 {
		roomsSection = "Join these rooms: " + strings.Join(rooms, ", ") + ".\n\n"
	}
	var tmpl string
	if forceName != "" {
		tmpl = agentFetchPromptTemplate(aimebuURL, "agent.bootstrap_reclaim", agentBootstrapReclaimTemplate)
	} else {
		tmpl = agentFetchPromptTemplate(aimebuURL, "agent.bootstrap", agentBootstrapTemplate)
	}
	return agentApplyPromptTokens(tmpl, harness, agentPromptMetaJSON(spawnTag), forceName, roomsSection, agentAssumeRoleSection(assumeRole, rooms), agentModelInstruction(modelSlug))
}

func agentBuildRecoveryPrompt(aimebuURL, harness, spawnTag, forceName string, rooms []string, assumeRole, modelSlug string) string {
	roomsSection := ""
	if len(rooms) > 0 {
		roomsSection = "Join these rooms: " + strings.Join(rooms, ", ") + ".\n\n"
	}
	tmpl := agentFetchPromptTemplate(aimebuURL, "agent.recovery", agentRecoveryTemplate)
	return agentApplyPromptTokens(tmpl, harness, agentPromptMetaJSON(spawnTag), forceName, roomsSection, agentAssumeRoleSection(assumeRole, rooms), agentModelInstruction(modelSlug))
}

func agentModelInstruction(modelSlug string) string {
	modelSlug = strings.TrimSpace(modelSlug)
	if modelSlug == "" {
		return ""
	}
	return "For this wrapped session, pass model=" + fmt.Sprintf("%q", modelSlug) + " exactly when calling bus_register.\n"
}

func agentPromptMetaJSON(spawnTag string) string {
	meta := map[string]string{"protocol": "agent"}
	if spawnTag != "" {
		meta["spawn_tag"] = spawnTag
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return `{"protocol":"agent"}`
	}
	return string(data)
}

// agentSessionsPath returns the path to the agent sessions state file.
func agentSessionsPath() string {
	return filepath.Join(config.AgentsDir(), "agent-sessions.json")
}

// agentLoadSessions reads agents/agent-sessions.json.
// Returns nil (not an error) if the file does not exist yet.
func agentLoadSessions() ([]agentSession, error) {
	path := agentSessionsPath()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sessions []agentSession
	if err := json.Unmarshal(data, &sessions); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return sessions, nil
}

// agentSaveSession upserts sess into agents/agent-sessions.json by full
// identity, then writes atomically via a tmp file + rename.
func agentSaveSession(sess agentSession) error {
	path := agentSessionsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	sessions, _ := agentLoadSessions() // ignore parse errors; start fresh if corrupt
	updated := false
	for i, s := range sessions {
		if agentSessionsSameIdentity(s, sess) {
			sessions[i] = sess
			updated = true
			break
		}
	}
	if !updated {
		sessions = append(sessions, sess)
	}
	data, err := json.MarshalIndent(sessions, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func agentSessionProject(sess agentSession) string {
	if strings.Contains(sess.Name, "@") {
		parts := strings.SplitN(sess.Name, "@", 2)
		if parts[1] != "" {
			return parts[1]
		}
	}
	if sess.CWD == "" {
		return ""
	}
	project := filepath.Base(filepath.Clean(sess.CWD))
	if project == "." || project == string(filepath.Separator) {
		return ""
	}
	return project
}

func agentSessionSlug(sess agentSession) string {
	if i := strings.IndexByte(sess.Name, '@'); i > 0 {
		return sess.Name[:i]
	}
	return sess.Name
}

func agentSessionFullID(sess agentSession) string {
	if sess.Name == "" || strings.Contains(sess.Name, "@") {
		return sess.Name
	}
	project := agentSessionProject(sess)
	if project == "" {
		return sess.Name
	}
	return sess.Name + "@" + project
}

func agentSessionsSameIdentity(a, b agentSession) bool {
	aID := agentSessionFullID(a)
	bID := agentSessionFullID(b)
	if aID == "" || bID == "" {
		return false
	}
	return aID == bID
}

func agentCurrentProject() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	project := filepath.Base(filepath.Clean(cwd))
	if project == "." || project == string(filepath.Separator) {
		return ""
	}
	return project
}

// agentResolveResume resolves a resume entry from the sessions file given the
// provided flags. resumeID and resumeName are mutually exclusive (caller validates).
// harness is the harness resolved from flags/auto-detection; it is checked against
// the stored entry's harness to catch mismatches (e.g. --harness codex with a
// claude-code session UUID).
func agentResolveResume(resumeID, resumeName, name, harness string, sessions []agentSession) (agentSession, error) {
	if resumeID != "" {
		for _, s := range sessions {
			if s.SessionID == resumeID {
				if s.Harness != "" && s.Harness != harness {
					return agentSession{}, fmt.Errorf("session %q was created with harness %q but current harness is %q; check --harness flag", resumeID, s.Harness, harness)
				}
				return s, nil
			}
		}
		if name != "" {
			return agentSession{SessionID: resumeID, Name: name, Harness: harness}, nil
		}
		return agentSession{}, fmt.Errorf("no state-file entry for session %q; pass --name to supply identity", resumeID)
	}
	if resumeName != "" {
		currentProject := agentCurrentProject()
		for _, s := range sessions {
			if agentSessionSlug(s) == resumeName && (currentProject == "" || agentSessionProject(s) == currentProject) {
				if s.Harness != "" && s.Harness != harness {
					return agentSession{}, fmt.Errorf("agent %q was registered with harness %q but current harness is %q; check --harness flag", resumeName, s.Harness, harness)
				}
				return s, nil
			}
		}
		return agentSession{}, fmt.Errorf("no state-file entry for name %q; run without --resume-name to bootstrap fresh with --name %s", resumeName, resumeName)
	}
	return agentSession{}, fmt.Errorf("internal error: no resume flag set")
}

// agentBootstrapArgs returns argv (excluding command[0]) for the initial
// session bootstrap.
//
// For claude-code: PTY interactive mode. Prompt is written to the PTY
// after the ❯ ready canary is seen, not via argv. Session ID is pre-generated
// driver-side (--session-id confirmed to work in interactive mode). No
// stream-json flags: PTY mode runs the normal interactive UI; turn completion
// is signalled by process exit (context-cap hit), not per-turn result events.
// The spawned Claude process must already have an aimebu MCP server registered
// in its own config; we do not inject --mcp-config here because that shadows
// user config and breaks sandboxed wrappers whose filesystem differs from the
// parent wrapper.
//
// For codex and pi: prompt is the final positional argument.
func agentBootstrapArgs(harness, prompt, sessionID, aimebuURL string, userArgs []string, modelSlug string) []string {
	switch harness {
	case "claude-code":
		args := []string{
			"--session-id", sessionID,
			"--dangerously-skip-permissions",
		}
		return append(args, userArgs...)
	case "codex":
		args := []string{"exec", "--json", "--dangerously-bypass-approvals-and-sandbox"}
		args = append(args, userArgs...)
		return append(args, prompt)
	case "pi":
		args := []string{"--mode", "json"}
		if modelSlug != "" {
			args = append(args, "--model", modelSlug)
		}
		args = append(args, userArgs...)
		return append(args, prompt)
	}
	return nil
}

// agentResumeArgs returns argv for resuming an established session.
//
// For claude-code: PTY interactive mode. --resume carries the session
// ID; prompt is written to the PTY after the ❯ ready canary. No stream-json
// flags.
// For codex and pi: prompt is the final positional argument.
func agentResumeArgs(harness, sessionID, prompt, aimebuURL string, userArgs []string, modelSlug string) []string {
	switch harness {
	case "claude-code":
		args := []string{
			"--resume", sessionID,
			"--dangerously-skip-permissions",
		}
		return append(args, userArgs...)
	case "codex":
		args := []string{"exec", "resume", sessionID, "--json", "--dangerously-bypass-approvals-and-sandbox"}
		args = append(args, userArgs...)
		return append(args, prompt)
	case "pi":
		args := []string{"--resume", "--session", sessionID, "--mode", "json"}
		if modelSlug != "" {
			args = append(args, "--model", modelSlug)
		}
		args = append(args, userArgs...)
		return append(args, prompt)
	}
	return nil
}

func agentCommand(command, args, env []string, stdout io.Writer, stderr io.Writer) *exec.Cmd {
	cmd := exec.Command(command[0], args...)
	cmd.Env = env
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

func agentParseSessionID(harness string, output []byte) (string, int) {
	switch harness {
	case "codex":
		return agentParseCodexThreadID(output)
	case "pi":
		return agentParsePiSessionID(output)
	}
	return "", -1
}

func agentFullID(agentName string) string {
	if agentName == "" || strings.Contains(agentName, "@") {
		return agentName
	}
	cwd, err := os.Getwd()
	if err != nil {
		return agentName
	}
	project := filepath.Base(cwd)
	if project == "" || project == "." {
		return agentName
	}
	return agentName + "@" + project
}

func agentBootstrapStart(harness string, command []string, prompt, sessionID, aimebuURL, modelSlug string, env []string, debug *agentDebugLog) (*exec.Cmd, *bytes.Buffer, *bytes.Buffer, *agentDebugStdoutWriter, *agentIDProvider, io.WriteCloser, error) {
	args := agentBootstrapArgs(harness, prompt, sessionID, aimebuURL, command[1:], modelSlug)

	buf := &bytes.Buffer{}
	stderrBuf := &bytes.Buffer{}
	agentID := newAgentIDProvider("")
	stateWriter := startAgentStatePusher(context.Background(), aimebuURL, agentID, newStateDetector(harness))
	stdoutWriter := newAgentDebugStdoutWriter(debug, io.MultiWriter(os.Stdout, buf, stateWriter))
	cmd := agentCommand(command, args, env, stdoutWriter, io.MultiWriter(os.Stderr, stderrBuf))

	if err := cmd.Start(); err != nil {
		_ = stateWriter.Close()
		return nil, nil, nil, nil, nil, nil, err
	}
	agentLogHarnessSpawn(debug, command, args)
	return cmd, buf, stderrBuf, stdoutWriter, agentID, stateWriter, nil
}

func agentBootstrapSession(harness string, command []string, prompt string, modelSlug string, env []string, aimebuURL, spawnTag, knownName string, sigCh <-chan os.Signal, debug *agentDebugLog) (string, string, error) {
	// claude-code uses PTY interactive mode.
	if harness == "claude-code" {
		return agentBootstrapSessionPTY(harness, command, prompt, env, aimebuURL, spawnTag, knownName, sigCh, debug)
	}

	startedAt := time.Now()

	// Codex: sessionID is extracted from stdout.
	preSessionID := ""

	bootstrapCmd, bootstrapBuf, stderrBuf, stdoutWriter, agentID, stateWriter, err := agentBootstrapStart(harness, command, prompt, preSessionID, aimebuURL, modelSlug, env, debug)
	if err != nil {
		return "", "", err
	}

	nameCh := make(chan string, 1)
	go func() {
		n := agentLookupName(aimebuURL, spawnTag, agentRegistrationLookupTimeout)
		if n != "" {
			agentID.Set(agentFullID(n))
			fmt.Fprintf(os.Stderr, "aimebu agent: registered as %s\n", n)
			agentLogRegisterObserved(debug, n, time.Since(startedAt))
		}
		nameCh <- n
	}()

	doneCh := make(chan error, 1)
	go func() { doneCh <- bootstrapCmd.Wait() }()

	var waitErr error
	select {
	case sig := <-sigCh:
		_ = stateWriter.Close()
		shutdownName := knownName
		if shutdownName == "" {
			select {
			case shutdownName = <-nameCh:
			default:
			}
		}
		if shutdownName == "" {
			shutdownName = agentLookupName(aimebuURL, spawnTag, time.Second)
		}
		agentGracefulShutdown(aimebuURL, spawnTag, shutdownName, bootstrapCmd, doneCh, sigCh, debug, sig)
		return "", shutdownName, agentErrInterrupted
	case waitErr = <-doneCh:
	}
	stdoutWriter.Flush()
	agentLogHarnessExit(debug, waitErr, time.Since(startedAt), stderrBuf.Bytes())

	if waitErr != nil {
		_ = stateWriter.Close()
		return "", "", waitErr
	}

	var lineIdx int
	sessionID, lineIdx := agentParseSessionID(harness, bootstrapBuf.Bytes())
	if sessionID == "" {
		_ = stateWriter.Close()
		return "", "", fmt.Errorf("could not extract session UUID from output; cannot resume")
	}
	agentLogSessionIDParsed(debug, harness, sessionID, lineIdx)

	agentName := <-nameCh
	if agentName == "" {
		_ = stateWriter.Close()
		return "", "", agentRegistrationMissingError(harness)
	}
	agentID.Set(agentFullID(agentName))
	_ = stateWriter.Close()
	_ = debug.setAgentName(agentName)
	return sessionID, agentName, nil
}

func agentResumeLoop(harness string, command []string, sessionID, agentName string, rooms []string, assumeRole, modelSlug string, env []string, aimebuURL string, sigCh <-chan os.Signal, debug *agentDebugLog) {
	// claude-code uses PTY interactive mode.
	if harness == "claude-code" {
		agentResumeLoopPTY(harness, command, sessionID, agentName, rooms, assumeRole, env, aimebuURL, sigCh, debug)
		return
	}

	retries := 0
	backoff := time.Second
	lastFailure := agentRecoveryNormalEnd
	consecutiveFailureCount := 0
	spawnTag := agentEnvValue(env, "AIMEBU_AGENT_SPAWN_TAG")
	if len(rooms) == 0 {
		rooms = nil
	}

	for {
		recoveryClass := agentRecoveryNormalEnd
		if agentName != "" {
			recoveryClass = agentPreflight(aimebuURL, agentFullID(agentName), rooms)
			if recoveryClass == agentRecoveryServerUnreachable {
				consecutiveFailureCount = agentAdvanceFailure(recoveryClass, &lastFailure, consecutiveFailureCount)
				agentLogRecoveryDecision(debug, recoveryClass, "preflight health check failed", consecutiveFailureCount, backoff)
				if consecutiveFailureCount > agentRecoveryFailureCap {
					agentFatalRecovery(aimebuURL, recoveryClass, sessionID, agentName)
				}
				agentPushState(aimebuURL, agentFullID(agentName), "respawning")
				fmt.Fprintf(os.Stderr, "aimebu agent: server unreachable before respawn, retry %d/%d in %v\n", consecutiveFailureCount, agentRecoveryFailureCap, backoff)
				time.Sleep(backoff)
				backoff *= 2
				if backoff > agentRecoveryMaxBackoff {
					backoff = agentRecoveryMaxBackoff
				}
				continue
			}
		}

		if recoveryClass == agentRecoveryCodexThreadMissing {
			// unreachable: codex-thread recovery is only set from child output below
			recoveryClass = agentRecoveryNormalEnd
		}

		prompt := "keep listening"
		runMode := "resume"
		if recoveryClass == agentRecoveryRegistrationLost {
			prompt = agentBuildRecoveryPrompt(aimebuURL, harness, spawnTag, agentName, rooms, assumeRole, modelSlug)
			fmt.Fprintf(os.Stderr, "aimebu agent: registration missing for %s, re-registering in-session\n", agentFullID(agentName))
			agentLogRecoveryDecision(debug, recoveryClass, "preflight room membership missing", consecutiveFailureCount, 0)
		}

		args := agentResumeArgs(harness, sessionID, prompt, aimebuURL, command[1:], modelSlug)
		stdoutBuf := &bytes.Buffer{}
		stderrBuf := &bytes.Buffer{}
		stateWriter := startAgentStatePusher(context.Background(), aimebuURL, newAgentIDProvider(agentFullID(agentName)), newStateDetector(harness))
		stdoutWriter := newAgentDebugStdoutWriter(debug, io.MultiWriter(os.Stdout, stdoutBuf, stateWriter))
		cmd := agentCommand(command, args, env, stdoutWriter, io.MultiWriter(os.Stderr, stderrBuf))
		startedAt := time.Now()

		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "aimebu agent: spawn failed: %v\n", err)
			os.Exit(1)
		}
		agentLogHarnessSpawn(debug, command, args)

		doneCh := make(chan error, 1)
		go func() { doneCh <- cmd.Wait() }()

		select {
		case sig := <-sigCh:
			_ = stateWriter.Close()
			agentGracefulShutdown(aimebuURL, "", agentName, cmd, doneCh, sigCh, debug, sig)
			return
		case err := <-doneCh:
			stdoutWriter.Flush()
			_ = stateWriter.Close()
			agentLogHarnessExit(debug, err, time.Since(startedAt), stderrBuf.Bytes())
			outcome := agentClassifyChildResult(harness, stdoutBuf.Bytes(), stderrBuf.Bytes())

			switch outcome {
			case agentRecoveryServerUnreachable:
				consecutiveFailureCount = agentAdvanceFailure(outcome, &lastFailure, consecutiveFailureCount)
				agentLogRecoveryDecision(debug, outcome, "child output reported server unreachable", consecutiveFailureCount, backoff)
				if consecutiveFailureCount > agentRecoveryFailureCap {
					agentFatalRecovery(aimebuURL, outcome, sessionID, agentName)
				}
				agentPushState(aimebuURL, agentFullID(agentName), "respawning")
				fmt.Fprintf(os.Stderr, "aimebu agent: server became unreachable during %s, retry %d/%d in %v\n", runMode, consecutiveFailureCount, agentRecoveryFailureCap, backoff)
				time.Sleep(backoff)
				backoff *= 2
				if backoff > agentRecoveryMaxBackoff {
					backoff = agentRecoveryMaxBackoff
				}
				continue
			case agentRecoveryRegistrationLost:
				consecutiveFailureCount = agentAdvanceFailure(outcome, &lastFailure, consecutiveFailureCount)
				agentLogRecoveryDecision(debug, outcome, "child output reported missing bus registration", consecutiveFailureCount, 0)
				if consecutiveFailureCount > agentRecoveryFailureCap {
					agentFatalRecovery(aimebuURL, outcome, sessionID, agentName)
				}
				agentPushState(aimebuURL, agentFullID(agentName), "respawning")
				fmt.Fprintf(os.Stderr, "aimebu agent: %s lost its bus registration, retrying in-session (%d/%d)\n", agentFullID(agentName), consecutiveFailureCount, agentRecoveryFailureCap)
				continue
			case agentRecoveryCodexThreadMissing:
				consecutiveFailureCount = agentAdvanceFailure(outcome, &lastFailure, consecutiveFailureCount)
				agentLogRecoveryDecision(debug, outcome, "codex reported missing thread during resume", consecutiveFailureCount, backoff)
				if consecutiveFailureCount > agentRecoveryFailureCap {
					agentFatalRecovery(aimebuURL, outcome, sessionID, agentName)
				}
				agentPushState(aimebuURL, agentFullID(agentName), "respawning")
				recoveryPrompt := agentBuildRecoveryPrompt(aimebuURL, harness, spawnTag, agentName, rooms, assumeRole, modelSlug)
				fmt.Fprintf(os.Stderr, "aimebu agent: codex thread %s vanished, bootstrapping a fresh thread (%d/%d)\n", sessionID, consecutiveFailureCount, agentRecoveryFailureCap)
				newSessionID, recoveredName, bootErr := agentBootstrapSession(harness, command, recoveryPrompt, modelSlug, env, aimebuURL, spawnTag, agentName, sigCh, debug)
				if errors.Is(bootErr, agentErrInterrupted) {
					return
				}
				if bootErr != nil {
					fmt.Fprintf(os.Stderr, "aimebu agent: fresh-thread bootstrap failed: %v\n", bootErr)
					time.Sleep(backoff)
					backoff *= 2
					if backoff > agentRecoveryMaxBackoff {
						backoff = agentRecoveryMaxBackoff
					}
					continue
				}
				sessionID = newSessionID
				if recoveredName != "" {
					agentName = recoveredName
				}
				cwd, _ := os.Getwd()
				_ = agentSaveSession(agentSession{
					CWD:        cwd,
					Harness:    harness,
					SessionID:  sessionID,
					Name:       agentName,
					Model:      modelSlug,
					Rooms:      append([]string(nil), rooms...),
					AssumeRole: assumeRole,
					Command:    command,
					LastUsed:   time.Now().UTC(),
				})
				backoff = time.Second
				retries = 0
				continue
			}

			if err == nil {
				retries = 0
				backoff = time.Second
				lastFailure = agentRecoveryNormalEnd
				consecutiveFailureCount = 0
				if agentName != "" {
					fmt.Fprintf(os.Stderr, "aimebu agent: session %s (%s) ended, resuming…\n", sessionID, agentName)
				} else {
					fmt.Fprintf(os.Stderr, "aimebu agent: session %s ended, resuming…\n", sessionID)
				}
				agentPushState(aimebuURL, agentFullID(agentName), "respawning")
				continue
			}
			retries++
			if retries > agentRecoveryFailureCap {
				fmt.Fprintf(os.Stderr, "aimebu agent: too many consecutive harness failures, giving up\n")
				agentPushState(aimebuURL, agentFullID(agentName), "error")
				os.Exit(1)
			}
			agentPushState(aimebuURL, agentFullID(agentName), "respawning")
			fmt.Fprintf(os.Stderr, "aimebu agent: exit error (%v), retry %d/%d in %v\n", err, retries, agentRecoveryFailureCap, backoff)
			time.Sleep(backoff)
			backoff *= 2
			if backoff > agentRecoveryMaxBackoff {
				backoff = agentRecoveryMaxBackoff
			}
		}
	}
}

func agentAdvanceFailure(class agentRecoveryClass, last *agentRecoveryClass, count int) int {
	if class == agentRecoveryNormalEnd {
		*last = agentRecoveryNormalEnd
		return 0
	}
	if *last == class {
		count++
	} else {
		*last = class
		count = 1
	}
	return count
}

func agentFatalRecovery(aimebuURL string, class agentRecoveryClass, sessionID, agentName string) {
	switch class {
	case agentRecoveryRegistrationLost:
		fmt.Fprintf(os.Stderr, "aimebu agent: registration recovery failed %d consecutive times for %s (session %s); giving up\n", agentRecoveryFailureCap, agentFullID(agentName), sessionID)
	case agentRecoveryCodexThreadMissing:
		fmt.Fprintf(os.Stderr, "aimebu agent: codex thread recovery failed %d consecutive times for %s; giving up\n", agentRecoveryFailureCap, sessionID)
	case agentRecoveryServerUnreachable:
		fmt.Fprintf(os.Stderr, "aimebu agent: server remained unreachable for %d consecutive checks; giving up\n", agentRecoveryFailureCap)
	default:
		fmt.Fprintf(os.Stderr, "aimebu agent: unrecoverable wrapper state (%s); giving up\n", class)
	}
	agentPushState(aimebuURL, agentFullID(agentName), "error")
	os.Exit(1)
}

func agentClassifyChildResult(harness string, stdout, stderr []byte) agentRecoveryClass {
	combined := string(stdout) + "\n" + string(stderr)
	if strings.Contains(combined, "not registered — call bus_register first") ||
		strings.Contains(combined, "is not registered; call POST /agents first") {
		return agentRecoveryRegistrationLost
	}
	if strings.Contains(combined, "connection refused") ||
		strings.Contains(combined, "aimebu unreachable") {
		return agentRecoveryServerUnreachable
	}
	if harness == "codex" &&
		strings.Contains(combined, "failed to record rollout items") &&
		agentCodexThreadNotFoundRE.MatchString(combined) {
		return agentRecoveryCodexThreadMissing
	}
	return agentRecoveryNormalEnd
}

func agentPreflight(aimebuURL, agentID string, expectedRooms []string) agentRecoveryClass {
	httpc := &http.Client{Timeout: 5 * time.Second}
	healthResp, err := httpc.Get(aimebuURL + "/health")
	if err != nil {
		return agentRecoveryServerUnreachable
	}
	io.Copy(io.Discard, healthResp.Body)
	healthResp.Body.Close()
	if healthResp.StatusCode != http.StatusOK {
		return agentRecoveryServerUnreachable
	}
	if agentID == "" {
		return agentRecoveryNormalEnd
	}

	resp, err := httpc.Get(aimebuURL + "/agents/" + url.PathEscape(agentID) + "/rooms")
	if err != nil {
		return agentRecoveryServerUnreachable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return agentRecoveryRegistrationLost
	}

	var payload struct {
		Rooms []struct {
			ID string `json:"id"`
		} `json:"rooms"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return agentRecoveryRegistrationLost
	}
	if len(expectedRooms) == 0 {
		if agentID == "" {
			return agentRecoveryNormalEnd
		}
		ok, err := agentIDRegistered(aimebuURL, agentID)
		if err != nil {
			return agentRecoveryServerUnreachable
		}
		if !ok {
			return agentRecoveryRegistrationLost
		}
		return agentRecoveryNormalEnd
	}
	if !agentRoomsContainExpected(payload.Rooms, expectedRooms) {
		return agentRecoveryRegistrationLost
	}
	return agentRecoveryNormalEnd
}

func agentRoomsContainExpected(actual []struct {
	ID string `json:"id"`
}, expected []string) bool {
	if len(expected) == 0 {
		return true
	}
	seen := make(map[string]struct{}, len(actual))
	for _, room := range actual {
		seen[room.ID] = struct{}{}
	}
	for _, roomID := range expected {
		if _, ok := seen[roomID]; !ok {
			return false
		}
	}
	return true
}

func agentIDRegistered(aimebuURL, agentID string) (bool, error) {
	type agent struct {
		ID string `json:"id"`
	}
	var payload struct {
		Agents []agent `json:"agents"`
	}

	httpc := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpc.Get(aimebuURL + "/agents")
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("GET /agents: %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return false, err
	}
	for _, agent := range payload.Agents {
		if agent.ID == agentID {
			return true, nil
		}
	}
	return false, nil
}

func agentDeleteRegistration(aimebuURL, agentID string, timeout time.Duration) error {
	agentID = agentFullID(agentID)
	if agentID == "" {
		return nil
	}
	c := &aimebuclient.Client{BaseURL: strings.TrimRight(aimebuURL, "/")}
	return c.DeleteAgent(agentID, timeout)
}

// agentPushState posts a wrapper-known lifecycle state to the bus. It is
// intentionally best-effort so state visibility never blocks recovery.
func agentPushState(aimebuURL, agentID, state string) {
	agentID = agentFullID(agentID)
	if aimebuURL == "" || agentID == "" || state == "" {
		return
	}
	payload, err := json.Marshal(map[string]string{"state": state})
	if err != nil {
		return
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(aimebuURL, "/")+"/agents/"+url.PathEscape(agentID)+"/state", bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	c := &http.Client{Timeout: 250 * time.Millisecond}
	resp, err := c.Do(req)
	if err != nil {
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func agentSignalProcessGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, sig)
	if err == syscall.ESRCH {
		return nil
	}
	return err
}

func agentStopChild(running *exec.Cmd, runDone <-chan error, sigCh <-chan os.Signal) {
	if running == nil || running.Process == nil {
		return
	}
	_ = agentSignalProcessGroup(running, syscall.SIGTERM)

	grace := time.NewTimer(500 * time.Millisecond)
	defer grace.Stop()

	select {
	case <-runDone:
		return
	case <-grace.C:
	case <-sigCh:
	}

	_ = agentSignalProcessGroup(running, syscall.SIGKILL)
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
	}
}

func agentGracefulShutdown(aimebuURL, spawnTag, agentName string, running *exec.Cmd, runDone <-chan error, sigCh <-chan os.Signal, debug *agentDebugLog, signal os.Signal) {
	fmt.Fprintln(os.Stderr, "\naimebu agent: shutting down...")

	attemptedID := agentFullID(agentName)
	if attemptedID == "" && spawnTag != "" {
		attemptedID = agentLookupName(aimebuURL, spawnTag, time.Second)
	}
	agentPushState(aimebuURL, attemptedID, "stopped")

	deleteDone := make(chan error, 1)
	if attemptedID != "" {
		go func() {
			deleteDone <- agentDeleteRegistration(aimebuURL, attemptedID, 2*time.Second)
		}()
	} else {
		close(deleteDone)
	}

	agentStopChild(running, runDone, sigCh)

	var (
		deleteErr      error
		deleteTimedOut bool
	)
	select {
	case err, ok := <-deleteDone:
		if ok && err != nil {
			deleteErr = err
		}
	case <-time.After(250 * time.Millisecond):
		deleteTimedOut = true
	}

	if spawnTag != "" {
		retryID := agentLookupName(aimebuURL, spawnTag, 2*time.Second)
		shouldRetry := retryID != "" && (retryID != attemptedID || deleteErr != nil || attemptedID == "")
		if deleteTimedOut && retryID == attemptedID {
			shouldRetry = false
		}
		if shouldRetry {
			deleteErr = agentDeleteRegistration(aimebuURL, retryID, 2*time.Second)
		}
	}

	if deleteErr != nil {
		fmt.Fprintf(os.Stderr, "aimebu agent: deregister failed: %v\n", deleteErr)
	}
	result := "ok"
	switch {
	case deleteErr != nil:
		result = deleteErr.Error()
	case deleteTimedOut:
		result = "delete timed out"
	}
	signalName := "unknown"
	if signal != nil {
		signalName = signal.String()
	}
	agentLogWrapperShutdown(debug, signalName, attemptedID, result)
}

func agentUsage() {
	fmt.Fprintln(os.Stderr, `Usage: aimebu agent [options] -- <command...>

Wrap a harness CLI with session-lifecycle management. Bootstraps the harness
with a bus-registration prompt, then auto-respawns via --resume when the
session ends (solving the session-length-cap problem transparently).

Options:
  --harness <slug>       Harness slug. Auto-detected from command basename if omitted.
  --room <id>            Room to join on startup (repeatable).
  --auto-room            Join the current working directory basename as a room.
  --name <slug>          Force-claim this project-scoped slug ([a-z]{3,12}).
                         Usable alone (fresh bootstrap with name continuity) or with
                         --resume-id as an escape hatch when the state file is missing.
  --assume-role <key>    Assign this agent to a role in the single launch room.
                         Requires exactly one resolved --room/--auto-room.
  --model <slug>         Model slug to record for harnesses that need one-time
                         wrapper-supplied model metadata (currently pi).
  --resume-id <uuid>     Resume a prior session by session UUID. Loads the agent full ID
                         from agents/agent-sessions.json in the aimebu config dir;
                         pass --name as fallback.
  --resume-name <slug>   Resume a prior session by slug in the current project. Loads
                         the session UUID from agents/agent-sessions.json in the
                         aimebu config dir; errors if not found.
  --                     Separator before the harness command (required).

Session state is persisted in agents/agent-sessions.json under the aimebu
config dir after each successful bootstrap so that --resume-id and
--resume-name can look up prior sessions.

Set AIMEBU_AGENT_DEBUG=1 (or true/yes/y/on) to write JSONL debug logs to
agents/agent-logs/<agent-name>.log under the aimebu config dir.
Logs are runtime diagnostics and are removed by both prune and prune -a.

Supported harnesses: claude-code (claude, claude-docker), codex (codex, codex-docker), pi (pi, pi-docker)

Examples:
  aimebu agent -- claude
  aimebu agent --auto-room -- claude
  aimebu agent --room general -- claude-docker
  aimebu agent --name alice --room general -- claude
  aimebu agent --resume-name alice -- claude
  aimebu agent --resume-id <uuid> -- claude
  aimebu agent --resume-id <uuid> --name alice -- claude
  aimebu agent --harness claude-code --room dev --room general -- /usr/local/bin/claude
  aimebu agent --room general -- codex
  aimebu agent --model ollama-cloud/gemma4:31b --room general -- pi`)
}
