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
	"github.com/hrubymar10/aimebu/internal/server"
)

// agentNamePattern is server.SlugPattern re-exported for local use.
var agentNamePattern = server.SlugPattern

var shellSafeTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

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
	agentRecoveryModelTurnTimeout   agentRecoveryClass = "model_turn_timeout"
	agentRecoveryResumeStalled      agentRecoveryClass = "resume_stalled"
	agentRecoveryPiIdleStalled      agentRecoveryClass = "pi_idle_stalled"
	agentRecoveryPiProgressStalled  agentRecoveryClass = "pi_progress_stalled"
	agentRecoveryPiBusWaitStalled   agentRecoveryClass = "pi_bus_wait_stalled"
	agentRecoveryPiTurnEndStalled   agentRecoveryClass = "pi_turn_end_stalled"
)

const (
	agentRecoveryFailureCap = 5
	agentRecoveryMaxBackoff = 16 * time.Second
)

var (
	agentRecoveryInitialBackoff  = time.Second
	agentResumeHeartbeatInterval = 20 * time.Second
	agentResumeStallTimeout      = 5 * time.Minute
)

var agentRegistrationLookupTimeout = 30 * time.Second

func agentRegistrationMissingError(harness string) error {
	listCommand, docsPath := agentHarnessMCPHint(harness)
	return fmt.Errorf("spawned %s session did not call `bus_register` -- verify `%s` shows aimebu and points at an executable reachable from the harness process. See %s", harness, listCommand, docsPath)
}

func agentHarnessMCPHint(harness string) (string, string) {
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
	case "vibe":
		listCommand = "cat ~/.vibe/config.toml"
		docsPath = "docs/vibe.md"
	}
	return listCommand, docsPath
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
	"vibe":          "vibe",
	"vibe-docker":   "vibe",
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
	modelSlug := "" // resolved after flag parsing from passthrough args (pi) or harvest
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
		fmt.Fprintf(os.Stderr, "aimebu agent: --name %q must match %s\n", name, server.SlugPatternStr)
		os.Exit(1)
	}
	if resumeName != "" && !agentNamePattern.MatchString(resumeName) {
		fmt.Fprintf(os.Stderr, "aimebu agent: --resume-name %q must match %s\n", resumeName, server.SlugPatternStr)
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
	case "claude-code", "codex", "pi", "vibe":
		// supported
	default:
		fmt.Fprintf(os.Stderr, "aimebu agent: harness %q is not yet supported.\nCurrently supported: claude-code (claude, claude-docker), codex (codex, codex-docker), pi (pi, pi-docker), vibe (vibe, vibe-docker).\n", harness)
		os.Exit(1)
	}
	if harness == "pi" {
		if _, err := agentProgressConfigFromLookup(os.Getenv); err != nil {
			fmt.Fprintf(os.Stderr, "aimebu agent: %v\n", err)
			os.Exit(1)
		}
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
		if assumeRole == "" {
			assumeRole = entry.AssumeRole
		}
		if harness == "codex" {
			modelSlug = codexModelFromArgs(command[1:])
			if modelSlug == "" {
				modelSlug = agentHarvestCodexDefaultModel(os.Environ())
			}
		}
		if harness == "pi" {
			modelSlug = piModelFromArgs(command[1:])
			if modelSlug == "" {
				modelSlug = agentHarvestPiDefaultModel(os.Environ())
			}
		}
		if harness == "vibe" && agentCanHarvestVibeModel(command) {
			modelSlug = agentHarvestVibeDefaultModel(os.Environ())
		}
		entry = agentPrepareResumeSession(entry, harness, modelSlug, rooms, assumeRole, command, time.Now().UTC())
		if assumeRole != "" && len(entry.Rooms) != 1 {
			fmt.Fprintln(os.Stderr, "aimebu agent: --assume-role requires exactly one saved launch room")
			os.Exit(1)
		}
		debug := newAgentDebugLog(entry.Name, spawnTag)
		defer debug.close()
		agentLogWrapperStart(debug, args, harness, entry.Rooms, spawnTag, resumeMode, aimebuURL, os.Getenv("AIMEBU_HARNESS"))
		childEnv := agentBuildEnv(aimebuURL, harness, spawnTag)
		fmt.Fprintf(os.Stderr, "aimebu agent: resuming session %s as %s\n", entry.SessionID, entry.Name)
		agentPersistSession(debug, aimebuURL, entry)
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

	if harness == "codex" {
		modelSlug = codexModelFromArgs(command[1:])
		if modelSlug == "" {
			modelSlug = agentHarvestCodexDefaultModel(os.Environ())
		}
	}
	if harness == "pi" {
		modelSlug = piModelFromArgs(command[1:])
		if modelSlug == "" {
			modelSlug = agentHarvestPiDefaultModel(os.Environ())
		}
	}
	if harness == "vibe" && agentCanHarvestVibeModel(command) {
		modelSlug = agentHarvestVibeDefaultModel(os.Environ())
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
		agentPersistSession(debug, aimebuURL, agentSession{
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

// agentLookupName polls the server until it finds an AI agent whose
// meta.spawn_tag matches spawnTag. Returns the agent ID or "" after timeout.
func agentLookupName(aimebuURL, spawnTag string, timeout time.Duration) string {
	if timeout <= 0 {
		return ""
	}
	type lookupResp struct {
		Agent struct {
			ID string `json:"id"`
		} `json:"agent"`
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
		resp, err := httpc.Get(strings.TrimRight(aimebuURL, "/") + "/agents/by-spawn-tag?tag=" + url.QueryEscape(spawnTag))
		if err == nil {
			var lr lookupResp
			_ = json.NewDecoder(resp.Body).Decode(&lr)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && lr.Agent.ID != "" {
				return lr.Agent.ID
			}
		}

		resp, err = httpc.Get(strings.TrimRight(aimebuURL, "/") + "/agents")
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

func agentBootstrapFailureClass(harness string, agentName string, output []byte, parseErr error) (string, string) {
	if agentName != "" && parseErr != nil {
		if harness == "pi" && agentOutputHasPiTimeout(output) {
			return "post_registration_turn_timeout", parseErr.Error()
		}
		return "registration_observed_parse_failed", parseErr.Error()
	}
	if harness == "pi" && agentOutputHasPiTimeout(output) {
		return "model_turn_timeout", "pi reported Request timed out before registration was observed"
	}
	return "child_completed_no_registration", ""
}

func agentOutputHasPiTimeout(output []byte) bool {
	s := string(output)
	return strings.Contains(s, `"errorMessage":"Request timed out."`) ||
		strings.Contains(s, `"errorMessage":"Request timed out"`) ||
		strings.Contains(s, "Request timed out")
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

func agentParseClaudeSessionID(output []byte) (string, int) {
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
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal([]byte(line), &r) == nil && r.SessionID != "" {
			return r.SessionID, n
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

// piModelFromArgs extracts and normalizes the --model value from harness args
// (everything after -- in the aimebu agent command). It is pi-specific: the
// returned slug strips the provider prefix (everything up to and including the
// first '/') so "ollama-cloud/minimax-m3" → "minimax-m3". A value with no '/'
// passes through unchanged. Last occurrence of --model wins; returns "" when
// absent. Matches "--model <val>" and "--model=<val>" exactly; "--model-path"
// and similar flags are not matched.
func piModelFromArgs(args []string) string {
	val := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--model" {
			if i+1 < len(args) {
				val = args[i+1]
				i++
			}
		} else if strings.HasPrefix(args[i], "--model=") {
			// Split on first '=' only; value may contain '=' (e.g. query strings).
			val = args[i][len("--model="):]
		}
	}
	if val == "" {
		return ""
	}
	if _, after, ok := strings.Cut(val, "/"); ok {
		return after
	}
	return val
}

// codexModelFromArgs extracts the effective model from codex passthrough args
// (everything after -- in the aimebu agent command). Last explicit model wins.
// It recognizes -m/--model plus -c/--config model=... because codex supports
// config overrides through CLI flags. Returns "" when no model override is
// present.
func codexModelFromArgs(args []string) string {
	val := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-m" || arg == "--model":
			if i+1 < len(args) {
				val = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "--model="):
			val = arg[len("--model="):]
		case arg == "-c" || arg == "--config":
			if i+1 < len(args) {
				if model := codexModelFromConfigArg(args[i+1]); model != "" {
					val = model
				}
				i++
			}
		case strings.HasPrefix(arg, "-c="):
			if model := codexModelFromConfigArg(arg[len("-c="):]); model != "" {
				val = model
			}
		case strings.HasPrefix(arg, "--config="):
			if model := codexModelFromConfigArg(arg[len("--config="):]); model != "" {
				val = model
			}
		}
	}
	return strings.TrimSpace(val)
}

func codexModelFromConfigArg(arg string) string {
	key, val, ok := strings.Cut(strings.TrimSpace(arg), "=")
	if !ok || strings.TrimSpace(key) != "model" {
		return ""
	}
	return strings.Trim(strings.TrimSpace(val), `"'`)
}

var codexTopLevelModelRE = regexp.MustCompile(`^model\s*=\s*(?:"([^"]*)"|'([^']*)')\s*(?:#.*)?$`)
var vibeTopLevelActiveModelRE = regexp.MustCompile(`^active_model\s*=\s*(?:"([^"]*)"|'([^']*)')\s*(?:#.*)?$`)

func agentHarvestCodexDefaultModel(env []string) string {
	configHome := ""
	for _, entry := range env {
		if v, ok := strings.CutPrefix(entry, "CODEX_HOME="); ok {
			configHome = strings.TrimSpace(v)
			break
		}
	}
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		configHome = filepath.Join(home, ".codex")
	}
	f, err := os.Open(filepath.Join(configHome, "config.toml"))
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			break
		}
		if matches := codexTopLevelModelRE.FindStringSubmatch(line); matches != nil {
			if matches[1] != "" {
				return strings.TrimSpace(matches[1])
			}
			return strings.TrimSpace(matches[2])
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

func agentCanHarvestVibeModel(command []string) bool {
	if len(command) == 0 {
		return false
	}
	return filepath.Base(command[0]) != "vibe-docker"
}

func agentHarvestVibeDefaultModel(env []string) string {
	vibeHome := ""
	for _, entry := range env {
		if v, ok := strings.CutPrefix(entry, "VIBE_HOME="); ok {
			vibeHome = strings.TrimSpace(v)
			break
		}
	}
	if vibeHome == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		vibeHome = filepath.Join(home, ".vibe")
	}
	f, err := os.Open(filepath.Join(vibeHome, "config.toml"))
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			break
		}
		if matches := vibeTopLevelActiveModelRE.FindStringSubmatch(line); matches != nil {
			if matches[1] != "" {
				return strings.TrimSpace(matches[1])
			}
			return strings.TrimSpace(matches[2])
		}
	}
	return ""
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

func agentSessionsLockPath() string {
	return filepath.Join(config.AgentsDir(), "agent-sessions.json.lock")
}

type agentSessionsLock struct {
	file *os.File
}

func agentAcquireSessionsLock() (*agentSessionsLock, error) {
	path := agentSessionsLockPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &agentSessionsLock{file: f}, nil
}

func (l *agentSessionsLock) unlock() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if err != nil {
		return err
	}
	return closeErr
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
	lock, err := agentAcquireSessionsLock()
	if err != nil {
		return err
	}
	defer lock.unlock()
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

func agentPersistSession(debug *agentDebugLog, aimebuURL string, sess agentSession) {
	if err := agentSaveSession(sess); err != nil {
		path := agentSessionsPath()
		fmt.Fprintf(os.Stderr, "aimebu agent: failed to save session state %s: %v\n", path, err)
		agentLogSessionSaveFailure(debug, path, err)
	}
	agentPushSession(debug, aimebuURL, sess)
}

func agentPrepareResumeSession(entry agentSession, harness, modelSlug string, rooms []string, assumeRole string, command []string, now time.Time) agentSession {
	cwd, err := os.Getwd()
	if err == nil && cwd != "" {
		entry.CWD = cwd
	}
	entry.Harness = harness
	if modelSlug != "" {
		entry.Model = modelSlug
	}
	if len(rooms) > 0 {
		entry.Rooms = append([]string(nil), rooms...)
	} else {
		entry.Rooms = append([]string(nil), entry.Rooms...)
	}
	entry.AssumeRole = assumeRole
	entry.Command = append([]string(nil), command...)
	entry.LastUsed = now
	return entry
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
// For claude-code: prompt is passed with -p in print mode. Session ID is
// pre-generated driver-side and also appears in Claude's stream-json output.
// Turn completion is signalled by process exit.
// The spawned Claude process must already have an aimebu MCP server registered
// in its own config; we do not inject --mcp-config here because that shadows
// user config and breaks sandboxed wrappers whose filesystem differs from the
// parent wrapper.
//
// For codex and pi: prompt is the final positional argument.
// For vibe: prompt is passed with -p/--prompt in programmatic mode.
func agentBootstrapArgs(harness, prompt, sessionID, aimebuURL string, userArgs []string, modelSlug string) []string {
	switch harness {
	case "claude-code":
		args := []string{
			"--session-id", sessionID,
			"-p", prompt,
			"--output-format", "stream-json",
			"--verbose",
			"--dangerously-skip-permissions",
		}
		return append(args, userArgs...)
	case "codex":
		args := []string{"exec", "--json", "--dangerously-bypass-approvals-and-sandbox"}
		args = append(args, userArgs...)
		return append(args, prompt)
	case "pi":
		args := []string{"--mode", "json"}
		// Only inject --model when the passthrough (userArgs) does not already
		// supply one. piModelFromArgs uses last-wins, so a passthrough --model
		// is already what pi will run; re-injecting would add a duplicate.
		if modelSlug != "" && piModelFromArgs(userArgs) == "" {
			args = append(args, "--model", modelSlug)
		}
		args = append(args, userArgs...)
		return append(args, prompt)
	case "vibe":
		args := []string{"-p", prompt, "--output", "json", "--yolo", "--trust"}
		return append(args, userArgs...)
	}
	return nil
}

// agentResumeArgs returns argv for resuming an established session.
//
// For claude-code: --resume carries the session ID and the prompt is passed
// with -p in print mode.
// For codex and pi: prompt is the final positional argument.
// For vibe: prompt is passed with -p/--prompt and -c resumes the latest
// session because JSON output does not expose a stable session ID.
func agentResumeArgs(harness, sessionID, prompt, aimebuURL string, userArgs []string, modelSlug string) []string {
	switch harness {
	case "claude-code":
		args := []string{
			"--resume", sessionID,
			"-p", prompt,
			"--output-format", "stream-json",
			"--verbose",
			"--dangerously-skip-permissions",
		}
		return append(args, userArgs...)
	case "codex":
		args := []string{"exec", "resume", sessionID, "--json", "--dangerously-bypass-approvals-and-sandbox"}
		args = append(args, userArgs...)
		return append(args, prompt)
	case "pi":
		args := []string{"--resume", "--session", sessionID, "--mode", "json"}
		if modelSlug != "" && piModelFromArgs(userArgs) == "" {
			args = append(args, "--model", modelSlug)
		}
		args = append(args, userArgs...)
		return append(args, prompt)
	case "vibe":
		args := []string{"-c", "-p", prompt, "--output", "json", "--yolo", "--trust"}
		return append(args, userArgs...)
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
	case "claude-code":
		return agentParseClaudeSessionID(output)
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

func agentBootstrapStart(harness string, command []string, prompt, sessionID, aimebuURL, modelSlug string, env []string, debug *agentDebugLog) (*exec.Cmd, *agentCaptureBuffer, *agentCaptureBuffer, *agentDebugStdoutWriter, *agentIDProvider, io.WriteCloser, *agentProgressMonitor, error) {
	args := agentBootstrapArgs(harness, prompt, sessionID, aimebuURL, command[1:], modelSlug)

	buf := &agentCaptureBuffer{}
	stderrBuf := &agentCaptureBuffer{}
	agentID := newAgentIDProvider("")
	stateWriter := startAgentStatePusher(context.Background(), aimebuURL, agentID, newStateDetector(harness))
	var progress *agentProgressMonitor
	writers := []io.Writer{os.Stdout, buf, stateWriter}
	if harness == "pi" {
		config, err := agentProgressConfigFromLookup(os.Getenv)
		if err != nil {
			_ = stateWriter.Close()
			return nil, nil, nil, nil, nil, nil, nil, err
		}
		progress = newAgentProgressMonitor(config)
		writers = append(writers, progress)
	}
	stdoutWriter := newAgentDebugStdoutWriter(debug, io.MultiWriter(writers...))
	cmd := agentCommand(command, args, env, stdoutWriter, io.MultiWriter(os.Stderr, stderrBuf))

	agentLogHarnessSpawn(debug, command, args)
	if err := cmd.Start(); err != nil {
		_ = stateWriter.Close()
		_ = progress.Close()
		return nil, nil, nil, nil, nil, nil, nil, err
	}
	return cmd, buf, stderrBuf, stdoutWriter, agentID, stateWriter, progress, nil
}

func agentBootstrapSession(harness string, command []string, prompt string, modelSlug string, env []string, aimebuURL, spawnTag, knownName string, sigCh <-chan os.Signal, debug *agentDebugLog) (string, string, error) {
	backoff := agentRecoveryInitialBackoff
	for attempt := 1; ; attempt++ {
		sessionID, agentName, err, failureClass := agentBootstrapSessionProcess(harness, command, prompt, modelSlug, env, aimebuURL, spawnTag, knownName, sigCh, debug)
		if err != nil && harness == "pi" && agentName == "" && failureClass == "model_turn_timeout" && attempt == 1 {
			agentLogBootstrapRetry(debug, harness, failureClass, attempt+1)
			continue
		}
		if err != nil && harness == "pi" && agentProgressStallFromClass(failureClass) != "" && attempt <= agentRecoveryFailureCap {
			agentLogBootstrapRetry(debug, harness, failureClass, attempt+1)
			time.Sleep(backoff)
			backoff *= 2
			if backoff > agentRecoveryMaxBackoff {
				backoff = agentRecoveryMaxBackoff
			}
			continue
		}
		return sessionID, agentName, err
	}
}

func agentBootstrapSessionProcess(harness string, command []string, prompt string, modelSlug string, env []string, aimebuURL, spawnTag, knownName string, sigCh <-chan os.Signal, debug *agentDebugLog) (string, string, error, string) {
	startedAt := time.Now()

	preSessionID := ""
	if harness == "claude-code" {
		preSessionID = agentGenSessionID()
		agentLogSessionIDPreGenerated(debug, harness, preSessionID)
	}

	bootstrapCmd, bootstrapBuf, stderrBuf, stdoutWriter, agentID, stateWriter, progress, err := agentBootstrapStart(harness, command, prompt, preSessionID, aimebuURL, modelSlug, env, debug)
	if err != nil {
		return "", "", err, ""
	}
	defer progress.Close()

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

	var (
		waitErr     error
		stallReason agentProgressStall
	)
	select {
	case sig := <-sigCh:
		stdoutSnapshot := bootstrapBuf.Bytes()
		stderrSnapshot := stderrBuf.Bytes()
		agentPrintHarnessDiagnostics("interrupt", stdoutSnapshot, stderrSnapshot)
		agentLogHarnessDiagnostics(debug, "interrupt", stdoutSnapshot, stderrSnapshot)
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
		stdoutWriter.Flush()
		return "", shutdownName, agentErrInterrupted, ""
	case stallReason = <-progress.Stalled():
	case waitErr = <-doneCh:
	}
	if stallReason != "" {
		stdoutSnapshot := bootstrapBuf.Bytes()
		stderrSnapshot := stderrBuf.Bytes()
		agentPrintHarnessDiagnostics(string(stallReason), stdoutSnapshot, stderrSnapshot)
		agentLogHarnessDiagnostics(debug, string(stallReason), stdoutSnapshot, stderrSnapshot)

		agentName := ""
		select {
		case agentName = <-nameCh:
		default:
			agentName = agentLookupName(aimebuURL, spawnTag, time.Second)
		}
		if agentName != "" {
			agentID.Set(agentFullID(agentName))
			agentPushState(aimebuURL, agentFullID(agentName), "respawning")
		}

		sessionID, lineIdx := agentParseSessionID(harness, stdoutSnapshot)
		agentStopChild(bootstrapCmd, doneCh, sigCh)
		stdoutWriter.Flush()
		_ = stateWriter.Close()
		stallErr := fmt.Errorf("pi bootstrap watchdog fired: %s", stallReason)
		agentLogHarnessExit(debug, stallErr, time.Since(startedAt), stderrBuf.Bytes())
		agentLogRecoveryDecision(debug, agentProgressRecoveryClass(stallReason), "pi bootstrap progress watchdog fired", 1, 0)
		if sessionID != "" && agentName != "" {
			agentLogSessionIDParsed(debug, harness, sessionID, lineIdx)
			_ = debug.setAgentName(agentName)
			return sessionID, agentName, nil, string(stallReason)
		}
		return "", agentName, stallErr, string(stallReason)
	}
	stdoutWriter.Flush()
	agentLogHarnessExit(debug, waitErr, time.Since(startedAt), stderrBuf.Bytes())

	if waitErr != nil {
		agentName := ""
		select {
		case agentName = <-nameCh:
		default:
			agentName = agentLookupName(aimebuURL, spawnTag, time.Second)
			if agentName != "" {
				agentID.Set(agentFullID(agentName))
				fmt.Fprintf(os.Stderr, "aimebu agent: registered as %s\n", agentName)
				agentLogRegisterObserved(debug, agentName, time.Since(startedAt))
			}
		}
		if agentName != "" {
			agentLogBootstrapFailure(debug, "registration_observed_child_failed", waitErr.Error())
			_ = stateWriter.Close()
			return "", agentName, waitErr, "registration_observed_child_failed"
		}
		class, detail := agentBootstrapFailureClass(harness, "", bootstrapBuf.Bytes(), nil)
		agentLogBootstrapFailure(debug, class, detail)
		_ = stateWriter.Close()
		return "", "", waitErr, class
	}

	agentName := <-nameCh
	if agentName == "" {
		agentName = agentLookupName(aimebuURL, spawnTag, time.Second)
		if agentName != "" {
			fmt.Fprintf(os.Stderr, "aimebu agent: registered as %s\n", agentName)
			agentLogRegisterObserved(debug, agentName, time.Since(startedAt))
		}
	}
	if agentName != "" {
		agentID.Set(agentFullID(agentName))
	}

	var lineIdx int
	sessionID, lineIdx := agentParseSessionID(harness, bootstrapBuf.Bytes())
	if sessionID == "" && preSessionID != "" {
		sessionID = preSessionID
	}
	var parseErr error
	if sessionID == "" && harness != "vibe" {
		parseErr = fmt.Errorf("could not extract session UUID from output; cannot resume")
		class, detail := agentBootstrapFailureClass(harness, agentName, bootstrapBuf.Bytes(), parseErr)
		agentLogBootstrapFailure(debug, class, detail)
		_ = stateWriter.Close()
		if agentName != "" {
			return "", agentName, parseErr, class
		}
		return "", "", parseErr, class
	}
	if sessionID != "" {
		agentLogSessionIDParsed(debug, harness, sessionID, lineIdx)
	}

	if agentName == "" {
		class, detail := agentBootstrapFailureClass(harness, "", bootstrapBuf.Bytes(), nil)
		agentLogBootstrapFailure(debug, class, detail)
		_ = stateWriter.Close()
		return "", "", agentRegistrationMissingError(harness), class
	}
	_ = stateWriter.Close()
	_ = debug.setAgentName(agentName)
	return sessionID, agentName, nil, ""
}

type agentResumeActivity struct {
	once chan struct{}
}

func newAgentResumeActivity() *agentResumeActivity {
	return &agentResumeActivity{once: make(chan struct{})}
}

func (a *agentResumeActivity) Write(p []byte) (int, error) {
	if len(p) > 0 {
		select {
		case <-a.once:
		default:
			close(a.once)
		}
	}
	return len(p), nil
}

func (a *agentResumeActivity) FirstOutput() <-chan struct{} {
	return a.once
}

func agentPushHeartbeat(aimebuURL, agentID string) error {
	c := &aimebuclient.Client{BaseURL: strings.TrimRight(aimebuURL, "/")}
	return c.HeartbeatAgent(agentID, 5*time.Second)
}

func agentStartResumeHeartbeat(ctx context.Context, aimebuURL, agentID string, withinBudget func() bool, debug *agentDebugLog) {
	if agentID == "" {
		return
	}
	go func() {
		ticker := time.NewTicker(agentResumeHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if withinBudget != nil && !withinBudget() {
					continue
				}
				err := agentPushHeartbeat(aimebuURL, agentID)
				agentLogHeartbeat(debug, agentID, err)
			case <-ctx.Done():
				return
			}
		}
	}()
}

type agentResumeWaitResult string

const (
	agentResumeWaitExited      agentResumeWaitResult = "exited"
	agentResumeWaitInterrupted agentResumeWaitResult = "interrupted"
	agentResumeWaitStalled     agentResumeWaitResult = "stalled"
)

func agentWaitResumeChild(doneCh <-chan error, sigCh <-chan os.Signal, activityCh <-chan struct{}, stallCh <-chan time.Time) (agentResumeWaitResult, error, os.Signal) {
	for {
		select {
		case sig := <-sigCh:
			return agentResumeWaitInterrupted, nil, sig
		case <-activityCh:
			activityCh = nil
			stallCh = nil
		case <-stallCh:
			return agentResumeWaitStalled, nil, nil
		case err := <-doneCh:
			return agentResumeWaitExited, err, nil
		}
	}
}

func agentResumeLoop(harness string, command []string, sessionID, agentName string, rooms []string, assumeRole, modelSlug string, env []string, aimebuURL string, sigCh <-chan os.Signal, debug *agentDebugLog) {
	retries := 0
	backoff := agentRecoveryInitialBackoff
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
		stdoutBuf := &agentCaptureBuffer{}
		stderrBuf := &agentCaptureBuffer{}
		activity := newAgentResumeActivity()
		stateWriter := startAgentStatePusher(context.Background(), aimebuURL, newAgentIDProvider(agentFullID(agentName)), newStateDetector(harness))
		var progress *agentProgressMonitor
		writers := []io.Writer{os.Stdout, stdoutBuf, stateWriter}
		if harness == "pi" {
			config, configErr := agentProgressConfigFromLookup(os.Getenv)
			if configErr != nil {
				fmt.Fprintf(os.Stderr, "aimebu agent: %v\n", configErr)
				agentPushState(aimebuURL, agentFullID(agentName), "error")
				return
			}
			progress = newAgentProgressMonitor(config)
			writers = append(writers, progress)
		} else {
			writers = append(writers, activity)
		}
		stdoutWriter := newAgentDebugStdoutWriter(debug, io.MultiWriter(writers...))
		cmd := agentCommand(command, args, env, stdoutWriter, io.MultiWriter(os.Stderr, stderrBuf))
		startedAt := time.Now()

		agentLogHarnessSpawn(debug, command, args)
		if err := cmd.Start(); err != nil {
			_ = progress.Close()
			_ = stateWriter.Close()
			fmt.Fprintf(os.Stderr, "aimebu agent: spawn failed: %v\n", err)
			os.Exit(1)
		}

		doneCh := make(chan error, 1)
		go func() { doneCh <- cmd.Wait() }()

		heartbeatCtx, stopHeartbeat := context.WithCancel(context.Background())
		var withinBudget func() bool
		if progress != nil {
			withinBudget = progress.WithinBudget
		}
		agentStartResumeHeartbeat(heartbeatCtx, aimebuURL, agentFullID(agentName), withinBudget, debug)
		var stallTimer *time.Timer
		var stallCh <-chan time.Time
		if progress == nil {
			stallTimer = time.NewTimer(agentResumeStallTimeout)
			stallCh = stallTimer.C
		}
		stopStallTimer := func() {
			if stallTimer == nil {
				return
			}
			if !stallTimer.Stop() {
				select {
				case <-stallTimer.C:
				default:
				}
			}
			stallCh = nil
		}

		var (
			waitResult  agentResumeWaitResult
			err         error
			sig         os.Signal
			stallReason agentProgressStall
		)
		if progress != nil {
			select {
			case sig = <-sigCh:
				waitResult = agentResumeWaitInterrupted
			case stallReason = <-progress.Stalled():
				waitResult = agentResumeWaitStalled
			case err = <-doneCh:
				waitResult = agentResumeWaitExited
			}
		} else {
			waitResult, err, sig = agentWaitResumeChild(doneCh, sigCh, activity.FirstOutput(), stallCh)
		}
		switch waitResult {
		case agentResumeWaitInterrupted:
			stopHeartbeat()
			stopStallTimer()
			stdoutSnapshot := stdoutBuf.Bytes()
			stderrSnapshot := stderrBuf.Bytes()
			agentPrintHarnessDiagnostics("interrupt", stdoutSnapshot, stderrSnapshot)
			agentLogHarnessDiagnostics(debug, "interrupt", stdoutSnapshot, stderrSnapshot)
			_ = stateWriter.Close()
			agentGracefulShutdown(aimebuURL, "", agentName, cmd, doneCh, sigCh, debug, sig)
			stdoutWriter.Flush()
			_ = progress.Close()
			return
		case agentResumeWaitStalled:
			stopHeartbeat()
			if stallReason != "" {
				stdoutSnapshot := stdoutBuf.Bytes()
				stderrSnapshot := stderrBuf.Bytes()
				agentPrintHarnessDiagnostics(string(stallReason), stdoutSnapshot, stderrSnapshot)
				agentLogHarnessDiagnostics(debug, string(stallReason), stdoutSnapshot, stderrSnapshot)
				agentPushState(aimebuURL, agentFullID(agentName), "respawning")
			}
			agentStopChild(cmd, doneCh, sigCh)
			stdoutWriter.Flush()
			_ = progress.Close()
			_ = stateWriter.Close()
			outcome := agentRecoveryResumeStalled
			detail := fmt.Sprintf("resume child produced no output before %s", agentResumeStallTimeout)
			if stallReason != "" {
				outcome = agentProgressRecoveryClass(stallReason)
				detail = fmt.Sprintf("pi progress watchdog fired: %s", stallReason)
			}
			stallErr := fmt.Errorf("%s", detail)
			agentLogHarnessExit(debug, stallErr, time.Since(startedAt), stderrBuf.Bytes())
			consecutiveFailureCount = agentAdvanceFailure(outcome, &lastFailure, consecutiveFailureCount)
			agentLogRecoveryDecision(debug, outcome, detail, consecutiveFailureCount, backoff)
			if consecutiveFailureCount > agentRecoveryFailureCap {
				agentFatalRecovery(aimebuURL, outcome, sessionID, agentName)
			}
			agentPushState(aimebuURL, agentFullID(agentName), "respawning")
			if stallReason != "" {
				fmt.Fprintf(os.Stderr, "aimebu agent: pi watchdog detected %s during %s, retry %d/%d in %v\n", stallReason, runMode, consecutiveFailureCount, agentRecoveryFailureCap, backoff)
			} else {
				fmt.Fprintf(os.Stderr, "aimebu agent: %s produced no output for %v, retry %d/%d in %v\n", runMode, agentResumeStallTimeout, consecutiveFailureCount, agentRecoveryFailureCap, backoff)
			}
			time.Sleep(backoff)
			backoff *= 2
			if backoff > agentRecoveryMaxBackoff {
				backoff = agentRecoveryMaxBackoff
			}
			continue
		}
		stopHeartbeat()
		stopStallTimer()
		stdoutWriter.Flush()
		_ = progress.Close()
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
			agentPersistSession(debug, aimebuURL, agentSession{
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
		case agentRecoveryModelTurnTimeout:
			consecutiveFailureCount = agentAdvanceFailure(outcome, &lastFailure, consecutiveFailureCount)
			agentLogRecoveryDecision(debug, outcome, "child output reported model turn timeout", consecutiveFailureCount, backoff)
			if consecutiveFailureCount > agentRecoveryFailureCap {
				agentFatalRecovery(aimebuURL, outcome, sessionID, agentName)
			}
			agentPushState(aimebuURL, agentFullID(agentName), "respawning")
			fmt.Fprintf(os.Stderr, "aimebu agent: model turn timed out during %s, retry %d/%d in %v\n", runMode, consecutiveFailureCount, agentRecoveryFailureCap, backoff)
			time.Sleep(backoff)
			backoff *= 2
			if backoff > agentRecoveryMaxBackoff {
				backoff = agentRecoveryMaxBackoff
			}
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
	case agentRecoveryModelTurnTimeout:
		fmt.Fprintf(os.Stderr, "aimebu agent: model turn timed out %d consecutive times for %s (session %s); giving up\n", agentRecoveryFailureCap, agentFullID(agentName), sessionID)
	case agentRecoveryResumeStalled:
		fmt.Fprintf(os.Stderr, "aimebu agent: resumed harness produced no output %d consecutive times for %s (session %s); giving up\n", agentRecoveryFailureCap, agentFullID(agentName), sessionID)
	case agentRecoveryPiIdleStalled, agentRecoveryPiProgressStalled, agentRecoveryPiBusWaitStalled, agentRecoveryPiTurnEndStalled:
		fmt.Fprintf(os.Stderr, "aimebu agent: pi progress watchdog reported %s %d consecutive times for %s (session %s); giving up\n", class, agentRecoveryFailureCap, agentFullID(agentName), sessionID)
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
	if harness == "pi" && agentOutputHasPiTimeout([]byte(combined)) {
		return agentRecoveryModelTurnTimeout
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

func agentPushSession(debug *agentDebugLog, aimebuURL string, sess agentSession) {
	agentID := agentFullID(sess.Name)
	resumeCommand := agentResumeCommandHint(sess)
	if aimebuURL == "" || agentID == "" || resumeCommand == "" {
		return
	}
	payload, err := json.Marshal(map[string]string{
		"harness_session_id": sess.SessionID,
		"resume_command":     resumeCommand,
		"cwd":                sess.CWD,
	})
	if err != nil {
		agentLogSessionPushFailure(debug, agentID, err)
		return
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(aimebuURL, "/")+"/agents/"+url.PathEscape(agentID)+"/session", bytes.NewReader(payload))
	if err != nil {
		agentLogSessionPushFailure(debug, agentID, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	c := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := c.Do(req)
	if err != nil {
		agentLogSessionPushFailure(debug, agentID, err)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		agentLogSessionPushFailure(debug, agentID, fmt.Errorf("server returned %s", resp.Status))
	}
}

func agentResumeCommandHint(sess agentSession) string {
	parts := []string{"aimebu", "agent"}
	if sess.SessionID != "" {
		parts = append(parts, "--resume-id", sess.SessionID)
	} else if sess.Name != "" {
		parts = append(parts, "--resume-name", strings.Split(agentFullID(sess.Name), "@")[0])
	} else {
		return ""
	}
	if sess.Harness != "" {
		parts = append(parts, "--harness", sess.Harness)
	}
	if len(sess.Rooms) == 0 {
		parts = append(parts, "--auto-room")
	} else {
		for _, room := range sess.Rooms {
			parts = append(parts, "--room", room)
		}
	}
	if sess.AssumeRole != "" {
		parts = append(parts, "--assume-role", sess.AssumeRole)
	}
	parts = append(parts, "--")
	parts = append(parts, sess.Command...)
	for i, part := range parts {
		parts[i] = shellQuote(part)
	}
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if shellSafeTokenPattern.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
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
  --name <slug>          Force-claim this project-scoped slug (3–21 chars, start with letter,
                         end with letter/digit, hyphens/underscores interior only).
                         Usable alone (fresh bootstrap with name continuity) or with
                         --resume-id as an escape hatch when the state file is missing.
  --assume-role <key>    Assign this agent to a role in the single launch room.
                         Requires exactly one resolved --room/--auto-room.
  --resume-id <uuid>     Resume a prior session by session UUID. Loads the agent full ID
                         from agents/agent-sessions.json in the aimebu config dir;
                         pass --name as fallback.
  --resume-name <slug>   Resume a prior session by slug in the current project. Loads
                         the session UUID from agents/agent-sessions.json in the
                         aimebu config dir; errors if not found.
  --                     Separator before the harness command (required).

Session state is persisted in agents/agent-sessions.json under the aimebu
config dir after each successful bootstrap or resume so that --resume-id and
--resume-name can look up prior sessions.

Set AIMEBU_AGENT_DEBUG=1 (or true/yes/y/on) to write JSONL debug logs to
agents/agent-logs/<agent-id>-<spawn_tag>.log under the aimebu config dir.
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
  aimebu agent --room general -- pi --model ollama-cloud/minimax-m3`)
}
