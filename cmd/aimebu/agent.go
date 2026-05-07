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
	"regexp"
	"strings"
	"syscall"
	"time"

	aimebuclient "github.com/hrubymar10/aimebu/internal/client"
)

// agentNamePattern mirrors store.go's reclaimNamePattern.
var agentNamePattern = regexp.MustCompile(`^[a-z]{3,12}$`)

// agentSession is one entry in ~/.aimebu/agent-sessions.json.
type agentSession struct {
	CWD       string    `json:"cwd"`
	Harness   string    `json:"harness"`
	SessionID string    `json:"session_id"`
	Name      string    `json:"name"`
	Command   []string  `json:"command"`
	LastUsed  time.Time `json:"last_used"`
}

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
	"codex-docker":  "codex",
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
	name := ""       // --name
	resumeID := ""   // --resume-id
	resumeName := "" // --resume-name

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
		case "--name":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "aimebu agent: --name requires a value")
				os.Exit(1)
			}
			name = args[i+1]
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
			case strings.HasPrefix(args[i], "--name="):
				name = strings.TrimPrefix(args[i], "--name=")
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
		fmt.Fprintf(os.Stderr, "aimebu agent: harness %q is not yet supported.\nCurrently supported: claude-code (claude, claude-docker), codex (codex, codex-docker).\n", harness)
		os.Exit(1)
	}

	aimebuURL := os.Getenv("AIMEBU_URL")
	if aimebuURL == "" {
		aimebuURL = "http://localhost:9997"
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
		childEnv := agentBuildEnv(aimebuURL, harness, "")
		fmt.Fprintf(os.Stderr, "aimebu agent: resuming session %s as %s\n", entry.SessionID, entry.Name)
		agentResumeLoop(harness, command, entry.SessionID, entry.Name, childEnv, aimebuURL, sigCh)
		return
	}

	// --- Bootstrap path ---
	httpc := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpc.Get(aimebuURL + "/health")
	if err != nil || resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "aimebu agent: server unreachable at %s. Start it first:\n  aimebu server start\n", aimebuURL)
		os.Exit(1)
	}
	resp.Body.Close()

	spawnTag := agentGenTag()
	childEnv := agentBuildEnv(aimebuURL, harness, spawnTag)

	prompt := agentBuildBootstrapPrompt(harness, spawnTag, rooms, name)

	spawnLog := fmt.Sprintf("aimebu agent: spawning %s (harness=%s", filepath.Base(command[0]), harness)
	if len(rooms) > 0 {
		spawnLog += ", rooms=" + strings.Join(rooms, ",")
	}
	if name != "" {
		spawnLog += ", name=" + name
	}
	fmt.Fprintln(os.Stderr, spawnLog+")…")

	bootstrapCmd, bootstrapBuf, err := agentBootstrapStart(harness, command, prompt, childEnv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aimebu agent: bootstrap failed: %v\n", err)
		os.Exit(1)
	}

	// When name is known up-front, skip the polling goroutine.
	nameCh := make(chan string, 1)
	if name != "" {
		nameCh <- name
	} else {
		go func() {
			n := agentLookupName(aimebuURL, spawnTag, 30*time.Second)
			nameCh <- n
			if n != "" {
				fmt.Fprintf(os.Stderr, "aimebu agent: registered as %s\n", n)
			}
		}()
	}

	doneCh := make(chan error, 1)
	go func() { doneCh <- bootstrapCmd.Wait() }()

	var waitErr error
	select {
	case <-sigCh:
		shutdownName := name
		if shutdownName == "" {
			select {
			case shutdownName = <-nameCh:
			default:
			}
		}
		if shutdownName == "" {
			shutdownName = agentLookupName(aimebuURL, spawnTag, time.Second)
		}
		agentGracefulShutdown(aimebuURL, spawnTag, shutdownName, bootstrapCmd, doneCh, sigCh)
		return
	case waitErr = <-doneCh:
	}
	if waitErr != nil {
		err = waitErr
		fmt.Fprintf(os.Stderr, "aimebu agent: bootstrap failed: %v\n", err)
		os.Exit(1)
	}
	sessionID, err := agentParseSessionID(harness, bootstrapBuf.Bytes())
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

	if agentName != "" {
		cwd, _ := os.Getwd()
		_ = agentSaveSession(agentSession{
			CWD:       cwd,
			Harness:   harness,
			SessionID: sessionID,
			Name:      agentName,
			Command:   command,
			LastUsed:  time.Now().UTC(),
		})
	}

	agentResumeLoop(harness, command, sessionID, agentName, childEnv, aimebuURL, sigCh)
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
	out = append(out,
		"AIMEBU_URL="+aimebuURL,
		"AIMEBU_HARNESS="+harness,
		"AIMEBU_AGENT_PROTOCOL=agent",
	)
	if spawnTag != "" {
		out = append(out, "AIMEBU_AGENT_SPAWN_TAG="+spawnTag)
	}
	return out
}

// agentBuildBootstrapPrompt builds the prompt for the initial bootstrap session.
// When forceName is set, the agent is instructed to reclaim that identity.
func agentBuildBootstrapPrompt(harness, spawnTag string, rooms []string, forceName string) string {
	var pb strings.Builder
	if forceName != "" {
		fmt.Fprintf(&pb, "You're an aimebu bus agent. Register via the bus_register MCP tool with name=%q, force=true, model=<your model slug>, harness=%q, meta={\"protocol\":\"agent\",\"spawn_tag\":%q}. This reclaims your prior identity.\n\n", forceName, harness, spawnTag)
	} else {
		fmt.Fprintf(&pb, "You're an aimebu bus agent. Register via the bus_register MCP tool (model=<your model slug>, harness=%q, meta={\"protocol\":\"agent\",\"spawn_tag\":%q}). The server will assign you a name.\n\n", harness, spawnTag)
	}
	if len(rooms) > 0 {
		fmt.Fprintf(&pb, "Join these rooms: %s.\n\n", strings.Join(rooms, ", "))
	}
	pb.WriteString("Then call bus_wait (no room argument — that way you receive DMs and traffic across all your rooms) to block on incoming messages. Respond per the etiquette in the MCP server-instructions. Keep listening (re-call bus_wait every time it returns with keep_waiting=true) until the user explicitly tells you to stop.")
	return pb.String()
}

// agentSessionsPath returns the path to the agent sessions state file.
func agentSessionsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".aimebu", "agent-sessions.json"), nil
}

// agentLoadSessions reads ~/.aimebu/agent-sessions.json.
// Returns nil (not an error) if the file does not exist yet.
func agentLoadSessions() ([]agentSession, error) {
	path, err := agentSessionsPath()
	if err != nil {
		return nil, err
	}
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

// agentSaveSession upserts sess into ~/.aimebu/agent-sessions.json by name,
// then writes atomically via a tmp file + rename.
func agentSaveSession(sess agentSession) error {
	path, err := agentSessionsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	sessions, _ := agentLoadSessions() // ignore parse errors; start fresh if corrupt
	updated := false
	for i, s := range sessions {
		if s.Name == sess.Name {
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
		for _, s := range sessions {
			if s.Name == resumeName {
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

func agentCommand(command, args, env []string, stdout io.Writer, stderr io.Writer) *exec.Cmd {
	cmd := exec.Command(command[0], args...)
	cmd.Env = env
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
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

func agentBootstrapStart(harness string, command []string, prompt string, env []string) (*exec.Cmd, *bytes.Buffer, error) {
	args := agentBootstrapArgs(harness, prompt, command[1:])

	buf := &bytes.Buffer{}
	cmd := agentCommand(command, args, env, io.MultiWriter(os.Stdout, buf), os.Stderr)

	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	return cmd, buf, nil
}

func agentResumeLoop(harness string, command []string, sessionID, agentName string, env []string, aimebuURL string, sigCh <-chan os.Signal) {
	retries := 0
	backoff := time.Second

	for {
		args := agentResumeArgs(harness, sessionID, "keep listening", command[1:])

		cmd := agentCommand(command, args, env, os.Stdout, os.Stderr)

		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "aimebu agent: spawn failed: %v\n", err)
			os.Exit(1)
		}

		doneCh := make(chan error, 1)
		go func() { doneCh <- cmd.Wait() }()

		select {
		case <-sigCh:
			agentGracefulShutdown(aimebuURL, "", agentName, cmd, doneCh, sigCh)
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

func agentDeleteRegistration(aimebuURL, agentID string, timeout time.Duration) error {
	agentID = agentFullID(agentID)
	if agentID == "" {
		return nil
	}
	c := &aimebuclient.Client{BaseURL: strings.TrimRight(aimebuURL, "/")}
	return c.DeleteAgent(agentID, timeout)
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

func agentGracefulShutdown(aimebuURL, spawnTag, agentName string, running *exec.Cmd, runDone <-chan error, sigCh <-chan os.Signal) {
	fmt.Fprintln(os.Stderr, "\naimebu agent: shutting down...")

	attemptedID := agentFullID(agentName)
	if attemptedID == "" && spawnTag != "" {
		attemptedID = agentLookupName(aimebuURL, spawnTag, time.Second)
	}

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
}

func agentUsage() {
	fmt.Fprintln(os.Stderr, `Usage: aimebu agent [options] -- <command...>

Wrap a harness CLI with session-lifecycle management. Bootstraps the harness
with a bus-registration prompt, then auto-respawns via --resume when the
session ends (solving the session-length-cap problem transparently).

Options:
  --harness <slug>       Harness slug. Auto-detected from command basename if omitted.
  --room <id>            Room to join on startup (repeatable).
  --name <slug>          Enforce this agent name ([a-z]{3,12}) via force=true reclaim.
                         Usable alone (fresh bootstrap with name continuity) or with
                         --resume-id as an escape hatch when the state file is missing.
  --resume-id <uuid>     Resume a prior session by session UUID. Loads the agent name
                         from ~/.aimebu/agent-sessions.json; pass --name as fallback.
  --resume-name <slug>   Resume a prior session by agent name. Loads the session UUID
                         from ~/.aimebu/agent-sessions.json; errors if not found.
  --                     Separator before the harness command (required).

Session state is persisted in ~/.aimebu/agent-sessions.json after each successful
bootstrap so that --resume-id and --resume-name can look up prior sessions.

Supported harnesses: claude-code (claude, claude-docker), codex (codex, codex-docker)

Examples:
  aimebu agent -- claude
  aimebu agent --room general -- claude-docker
  aimebu agent --name alice --room general -- claude
  aimebu agent --resume-name alice -- claude
  aimebu agent --resume-id <uuid> -- claude
  aimebu agent --resume-id <uuid> --name alice -- claude
  aimebu agent --harness claude-code --room dev --room general -- /usr/local/bin/claude
  aimebu agent --room general -- codex`)
}
