package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const agentWarningMarker = "agent-warning-acknowledged"

const agentWarningText = `WARNING: aimebu agent runs the wrapped harness with
--dangerously-skip-permissions, which bypasses ALL permission
checks for the agent's tool calls. The agent will execute any
instructions it receives via the bus — including from other
agents you don't fully trust.

You are responsible for any risks and harms this may cause.

Type "yes" to acknowledge and proceed (this prompt won't appear
again — delete ~/.aimebu/agent-warning-acknowledged to re-enable):`

// agentCheckWarning checks for the first-run acknowledgement marker and
// prompts the user if it is absent. Exits if the user declines.
func agentCheckWarning() {
	home, err := os.UserHomeDir()
	if err != nil {
		return // can't check; skip
	}
	marker := filepath.Join(home, ".aimebu", agentWarningMarker)
	if _, err := os.Stat(marker); err == nil {
		return // already acknowledged
	}

	fmt.Fprintln(os.Stderr, agentWarningText)
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
	"cursor":        "cursor",
	"cline":         "cline",
	"aider":         "aider",
	"pi":            "pi",
	"pi-docker":     "pi",
}

func agentCmd(args []string) {
	agentCheckWarning()

	harness := ""
	var rooms []string
	var command []string

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
			default:
				fmt.Fprintf(os.Stderr, "aimebu agent: unknown flag: %s\n", args[i])
				agentUsage()
				os.Exit(1)
			}
		}
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
	case "claude-code", "codex":
		// supported
	default:
		fmt.Fprintf(os.Stderr, "aimebu agent: harness %q is not yet supported.\nCurrently supported: claude-code (claude, claude-docker), codex.\n", harness)
		os.Exit(1)
	}

	aimebuURL := os.Getenv("AIMEBU_URL")
	if aimebuURL == "" {
		aimebuURL = "http://localhost:9997"
	}
	httpc := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpc.Get(aimebuURL + "/health")
	if err != nil || resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "aimebu agent: server unreachable at %s. Start it first:\n  aimebu server start\n", aimebuURL)
		os.Exit(1)
	}
	resp.Body.Close()

	spawnTag := agentGenTag()
	childEnv := agentBuildEnv(aimebuURL, harness, spawnTag)

	var pb strings.Builder
	fmt.Fprintf(&pb, "You're an aimebu bus agent. Register via the bus_register MCP tool (model=<your model slug>, harness=%q, meta={\"protocol\":\"agent\",\"spawn_tag\":%q}). The server will assign you a name.\n\n", harness, spawnTag)
	if len(rooms) > 0 {
		fmt.Fprintf(&pb, "Join these rooms: %s.\n\n", strings.Join(rooms, ", "))
	}
	pb.WriteString("Then call bus_wait (no room argument — that way you receive DMs and traffic across all your rooms) to block on incoming messages. Respond per the etiquette in the MCP server-instructions. Keep listening (re-call bus_wait every time it returns with keep_waiting=true) until the user explicitly tells you to stop.")

	spawnLog := fmt.Sprintf("aimebu agent: spawning %s (harness=%s", filepath.Base(command[0]), harness)
	if len(rooms) > 0 {
		spawnLog += ", rooms=" + strings.Join(rooms, ",")
	}
	fmt.Fprintln(os.Stderr, spawnLog+")…")

	bootstrapCmd, bootstrapBuf, err := agentBootstrapStart(harness, command, pb.String(), childEnv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aimebu agent: bootstrap failed: %v\n", err)
		os.Exit(1)
	}

	// Look up the agent name in parallel with the bootstrap session running.
	nameCh := make(chan string, 1)
	go func() {
		name := agentLookupName(aimebuURL, spawnTag)
		nameCh <- name
		if name != "" {
			fmt.Fprintf(os.Stderr, "aimebu agent: registered as %s\n", name)
		}
	}()

	sessionID, err := agentBootstrapWait(harness, bootstrapCmd, bootstrapBuf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aimebu agent: bootstrap failed: %v\n", err)
		os.Exit(1)
	}
	if sessionID == "" {
		fmt.Fprintln(os.Stderr, "aimebu agent: could not extract session UUID from output; cannot resume.")
		os.Exit(1)
	}

	agentName := <-nameCh
	if agentName != "" {
		fmt.Fprintf(os.Stderr, "aimebu agent: session %s, agent %s — listening\n", sessionID, agentName)
	} else {
		fmt.Fprintf(os.Stderr, "aimebu agent: session %s — listening\n", sessionID)
	}
	agentResumeLoop(harness, command, sessionID, agentName, childEnv)
}

// agentLookupName polls GET /agents until it finds an AI agent whose
// meta.spawn_tag matches spawnTag. Returns the agent ID or "" after 30s.
func agentLookupName(aimebuURL, spawnTag string) string {
	type agent struct {
		ID   string            `json:"id"`
		Kind string            `json:"kind"`
		Meta map[string]string `json:"meta"`
	}
	type agentsResp struct {
		Agents []agent `json:"agents"`
	}

	httpc := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(30 * time.Second)

	for time.Now().Before(deadline) {
		resp, err := httpc.Get(aimebuURL + "/agents")
		if err != nil {
			time.Sleep(2 * time.Second)
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
		time.Sleep(2 * time.Second)
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

func agentBuildEnv(aimebuURL, harness, spawnTag string) []string {
	out := make([]string, 0, len(os.Environ())+4)
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "AIMEBU_URL=") &&
			!strings.HasPrefix(e, "AIMEBU_HARNESS=") &&
			!strings.HasPrefix(e, "AIMEBU_AGENT_PROTOCOL=") &&
			!strings.HasPrefix(e, "AIMEBU_AGENT_SPAWN_TAG=") {
			out = append(out, e)
		}
	}
	return append(out,
		"AIMEBU_URL="+aimebuURL,
		"AIMEBU_HARNESS="+harness,
		"AIMEBU_AGENT_PROTOCOL=agent",
		"AIMEBU_AGENT_SPAWN_TAG="+spawnTag,
	)
}

// agentBootstrapArgs returns argv (excluding command[0]) for the initial
// non-interactive session. For codex, user flags must precede the positional
// prompt; for claude-code, flags precede nothing (prompt is flagged via -p).
func agentBootstrapArgs(harness, prompt string, userArgs []string) []string {
	switch harness {
	case "claude-code":
		args := []string{"-p", prompt, "--output-format", "json", "--dangerously-skip-permissions"}
		return append(args, userArgs...)
	case "codex":
		args := []string{"exec", "--json", "--dangerously-bypass-approvals-and-sandbox"}
		args = append(args, userArgs...)
		return append(args, prompt)
	}
	return nil
}

// agentResumeArgs returns argv for resuming an established session.
func agentResumeArgs(harness, sessionID, prompt string, userArgs []string) []string {
	switch harness {
	case "claude-code":
		args := []string{"--resume", sessionID, "-p", prompt, "--dangerously-skip-permissions"}
		return append(args, userArgs...)
	case "codex":
		args := []string{"exec", "resume", sessionID, "--json", "--dangerously-bypass-approvals-and-sandbox"}
		args = append(args, userArgs...)
		return append(args, prompt)
	}
	return nil
}

// agentParseSessionID extracts the session/thread ID from bootstrap stdout.
// Scans up to 20 lines to tolerate any harness preamble before the ID event.
func agentParseSessionID(harness string, output []byte) (string, error) {
	lines := 0
	for line := range strings.SplitSeq(string(output), "\n") {
		if lines >= 20 {
			break
		}
		lines++
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch harness {
		case "claude-code":
			var r struct {
				SessionID string `json:"session_id"`
			}
			if json.Unmarshal([]byte(line), &r) == nil && r.SessionID != "" {
				return r.SessionID, nil
			}
		case "codex":
			var r struct {
				Type     string `json:"type"`
				ThreadID string `json:"thread_id"`
			}
			if json.Unmarshal([]byte(line), &r) == nil && r.Type == "thread.started" && r.ThreadID != "" {
				return r.ThreadID, nil
			}
		}
	}
	return "", nil
}

func agentBootstrapStart(harness string, command []string, prompt string, env []string) (*exec.Cmd, *bytes.Buffer, error) {
	args := agentBootstrapArgs(harness, prompt, command[1:])

	cmd := exec.Command(command[0], args...)
	cmd.Env = env
	cmd.Stderr = os.Stderr

	buf := &bytes.Buffer{}
	cmd.Stdout = io.MultiWriter(os.Stdout, buf)

	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	return cmd, buf, nil
}

func agentBootstrapWait(harness string, cmd *exec.Cmd, buf *bytes.Buffer) (string, error) {
	if err := cmd.Wait(); err != nil {
		return "", err
	}
	return agentParseSessionID(harness, buf.Bytes())
}

func agentResumeLoop(harness string, command []string, sessionID, agentName string, env []string) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	retries := 0
	backoff := time.Second

	for {
		args := agentResumeArgs(harness, sessionID, "keep listening", command[1:])

		cmd := exec.Command(command[0], args...)
		cmd.Env = env
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "aimebu agent: spawn failed: %v\n", err)
			os.Exit(1)
		}

		doneCh := make(chan error, 1)
		go func() { doneCh <- cmd.Wait() }()

		select {
		case sig := <-sigCh:
			agentGracefulShutdown(harness, command, sessionID, env, sig, cmd, doneCh)
			return
		case err := <-doneCh:
			if err == nil {
				retries = 0
				backoff = time.Second
				if agentName != "" {
					fmt.Fprintf(os.Stderr, "aimebu agent: session %s (%s) ended, resuming…\n", sessionID, agentName)
				} else {
					fmt.Fprintf(os.Stderr, "aimebu agent: session %s ended, resuming…\n", sessionID)
				}
				continue
			}
			retries++
			if retries > 5 {
				fmt.Fprintf(os.Stderr, "aimebu agent: too many consecutive failures, giving up\n")
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "aimebu agent: exit error (%v), retry %d/5 in %v\n", err, retries, backoff)
			time.Sleep(backoff)
			backoff *= 2
		}
	}
}

func agentGracefulShutdown(harness string, command []string, sessionID string, env []string, sig os.Signal, running *exec.Cmd, runDone <-chan error) {
	fmt.Fprintf(os.Stderr, "\naimebu agent: %v received — asking agent to leave rooms\n", sig)

	args := agentResumeArgs(harness, sessionID, "leave all your rooms and exit cleanly", command[1:])

	leaveCmd := exec.Command(command[0], args...)
	leaveCmd.Env = env
	leaveCmd.Stdout = os.Stdout
	leaveCmd.Stderr = os.Stderr

	if err := leaveCmd.Start(); err == nil {
		leaveDone := make(chan struct{})
		go func() {
			_ = leaveCmd.Wait()
			close(leaveDone)
		}()
		select {
		case <-leaveDone:
		case <-time.After(5 * time.Second):
		}
	}

	if running != nil && running.Process != nil {
		_ = running.Process.Signal(syscall.SIGTERM)
		select {
		case <-runDone:
		case <-time.After(5 * time.Second):
			_ = running.Process.Kill()
			<-runDone
		}
	}
}

func agentUsage() {
	fmt.Fprintln(os.Stderr, `Usage: aimebu agent [--harness <h>] [--room <r>...] -- <command...>

Wrap a harness CLI with session-lifecycle management. Bootstraps the harness
with a bus-registration prompt, then auto-respawns via --resume when the
session ends (solving the session-length-cap problem transparently).

Options:
  --harness <slug>   Harness slug. Auto-detected from command basename if omitted.
  --room <id>        Room to join on startup (repeatable).
  --                 Separator before the harness command (required).

Supported harnesses: claude-code (claude, claude-docker), codex

Examples:
  aimebu agent -- claude
  aimebu agent --room general -- claude-docker
  aimebu agent --harness claude-code --room dev --room general -- /usr/local/bin/claude
  aimebu agent --room general -- codex`)
}
