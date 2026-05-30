package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	aimebuclient "github.com/hrubymar10/aimebu/internal/client"
)

const (
	agentPTYCols = 80
	agentPTYRows = 24
	// agentPTYReadySignal is rendered by claude-code's agent-aware chat UI once
	// first-run modals are gone and the composer is ready for input.
	agentPTYReadySignal      = "← for agents"
	agentPTYTrustModalNeedle = "externalclaude.mdfileimports"
	// agentPTYStartupTimeout is the maximum time to wait for the ready signal
	// during bootstrap or resume startup before declaring a spawn failure.
	agentPTYStartupTimeout = 15 * time.Second
)

var agentPTYSubmitDelay = 150 * time.Millisecond
var agentPTYRegistrationStallTimeout = 30 * time.Second
var agentPTYReadyNeedle = strings.ToLower(strings.ReplaceAll(agentPTYReadySignal, " ", ""))
var agentPTYHeartbeatInterval = 20 * time.Second
var agentPTYIdleNudgeDelay = 5 * time.Second

// agentCommandForPTY creates an exec.Cmd for PTY spawning. Unlike agentCommand,
// stdout and stderr are NOT pre-piped: pty.Start wires them through the
// pseudo-terminal. SysProcAttr is left nil so that pty.Start can set Setsid
// and Setctty without conflicting with a caller-supplied Setpgid — combining
// those on Linux causes fork/exec to fail with EPERM.
func agentCommandForPTY(command, args, env []string) *exec.Cmd {
	cmd := exec.Command(command[0], args...)
	cmd.Env = env
	return cmd
}

// agentPTYWaitCanary reads from ptyFile until claude-code's agent-ready
// composer signal appears or timeout expires. All bytes read are copied to dst.
// Known first-run modals are dismissed before returning, because their
// highlight cursor can look like a prompt while the composer is not ready.
// Returns a non-nil error if the timeout expires, the PTY closes (EOF = child
// exited before the ready signal), or an unexpected read error occurs.
func agentPTYWaitCanary(ptyFile *os.File, dst io.Writer, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var seen []byte
	trustModalDismissed := false
	buf := make([]byte, 1024)
	fd := int(ptyFile.Fd())
	if err := syscall.SetNonblock(fd, true); err != nil {
		return fmt.Errorf("PTY: failed to set nonblocking mode: %w", err)
	}
	defer func() { _ = syscall.SetNonblock(fd, false) }()
	for !agentPTYHasReadySignal(seen) && time.Now().Before(deadline) {
		n, err := syscall.Read(fd, buf)
		if n > 0 {
			chunk := buf[:n]
			seen = append(seen, chunk...)
			if dst != nil {
				_, _ = dst.Write(chunk)
			}
			if len(seen) > 64*1024 {
				seen = append([]byte(nil), seen[len(seen)-64*1024:]...)
			}
			if !trustModalDismissed && agentPTYHasTrustModal(seen) {
				// The known external-imports trust modal appears at most once for
				// a project; dismiss it once, then keep waiting for the composer.
				if _, writeErr := syscall.Write(fd, []byte{'\r'}); writeErr != nil {
					return fmt.Errorf("PTY: failed to dismiss claude-code trust modal: %w", writeErr)
				}
				trustModalDismissed = true
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("PTY: closed before ready signal (child likely exited)")
			}
			if !errors.Is(err, syscall.EAGAIN) {
				return fmt.Errorf("PTY read error: %w", err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !agentPTYHasReadySignal(seen) {
		return fmt.Errorf("PTY: timed out waiting for %q ready signal", agentPTYReadySignal)
	}
	return nil
}

func agentPTYHasReadySignal(b []byte) bool {
	if bytes.Contains(b, []byte(agentPTYReadySignal)) {
		return true
	}
	normalized := strings.ToLower(agentPTYNormalizeForScreenDetection(b))
	return strings.Contains(normalized, agentPTYReadyNeedle)
}

func agentPTYHasTrustModal(b []byte) bool {
	normalized := strings.ToLower(agentPTYNormalizeForScreenDetection(b))
	return strings.Contains(normalized, agentPTYTrustModalNeedle)
}

func agentPTYNormalizeForScreenDetection(b []byte) string {
	var out strings.Builder
	out.Grow(len(b))
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c == 0x1b {
			i++
			if i < len(b) && b[i] == '[' {
				for i+1 < len(b) {
					i++
					if b[i] >= 0x40 && b[i] <= 0x7e {
						break
					}
				}
			}
			continue
		}
		if c < 0x20 || c == 0x7f || c == ' ' {
			continue
		}
		out.WriteByte(c)
	}
	return out.String()
}

// agentPTYWritePrompt writes text to the PTY, waits briefly for Claude to
// process any multi-line paste compaction, then sends a separate carriage
// return to simulate the user pressing Enter. Logs write failures to the debug
// log.
func agentPTYWritePrompt(w io.Writer, text string, debug *agentDebugLog) {
	if debug != nil {
		debug.log("pty_prompt_write", map[string]any{
			"bytes":          len(text),
			"separate_enter": true,
			"enter_delay_ms": agentPTYSubmitDelay.Milliseconds(),
		})
	}
	if _, err := io.WriteString(w, text); err != nil {
		if debug != nil {
			debug.log("stdin_write_error", map[string]any{"error": err.Error(), "mode": "pty"})
		}
		return
	}
	if agentPTYSubmitDelay > 0 {
		time.Sleep(agentPTYSubmitDelay)
	}
	if _, err := io.WriteString(w, "\r"); err != nil {
		if debug != nil {
			debug.log("stdin_write_error", map[string]any{"error": err.Error(), "mode": "pty"})
		}
	}
}

func agentPTYRegistrationStalledError(harness string) error {
	listCommand, docsPath := agentHarnessMCPHint(harness)
	return fmt.Errorf("spawned %s session did not call `bus_register` within %s after prompt delivery -- verify the harness reached the chat composer and `%s` shows aimebu. See %s",
		harness,
		agentPTYRegistrationStallTimeout,
		listCommand,
		docsPath,
	)
}

func agentPTYLogRegistrationStalled(debug *agentDebugLog, harness string, timeout time.Duration) {
	debug.log("registration_stalled", map[string]any{
		"harness":    harness,
		"timeout_ms": timeout.Milliseconds(),
	})
}

func agentPTYTerminateStalledProcess(cmd *exec.Cmd, doneCh <-chan error) {
	if cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-doneCh
	}
}

// agentPTYCopyOut reads bytes from ptyFile and writes them to dst until the PTY
// returns an error or EOF (which happens when the child process exits). Drains
// the PTY buffer continuously so the child never blocks on write. Runs in its
// own goroutine.
func agentPTYCopyOut(ptyFile *os.File, dst io.Writer) {
	if flusher, ok := dst.(interface{ Flush() }); ok {
		defer flusher.Flush()
	}
	buf := make([]byte, 4096)
	for {
		n, err := ptyFile.Read(buf)
		if n > 0 && dst != nil {
			_, _ = dst.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func agentPTYOutputWriter(debug *agentDebugLog) *agentDebugStdoutWriter {
	return newAgentDebugStdoutWriter(debug, io.Discard)
}

type agentPTYIdleTracker struct {
	mu         sync.Mutex
	seen       []byte
	idle       bool
	idleSince  time.Time
	nudgeSent  bool
	lastChange time.Time
}

func (t *agentPTYIdleTracker) Write(p []byte) (int, error) {
	t.Observe(p)
	return len(p), nil
}

func (t *agentPTYIdleTracker) Observe(p []byte) {
	if len(p) == 0 {
		return
	}
	now := time.Now()
	readyCurrent := agentPTYHasReadySignal(p) || agentPTYHasComposerPromptSignal(p)
	activeCurrent := agentPTYHasActiveTurnSignal(p)
	t.mu.Lock()
	if activeCurrent && !readyCurrent {
		t.seen = nil
		t.idle = false
		t.idleSince = time.Time{}
		t.nudgeSent = false
		t.lastChange = now
		t.mu.Unlock()
		return
	}
	t.seen = append(t.seen, p...)
	if len(t.seen) > 64*1024 {
		t.seen = append([]byte(nil), t.seen[len(t.seen)-64*1024:]...)
	}
	ready := readyCurrent || agentPTYHasReadySignal(t.seen) || agentPTYHasComposerPromptSignal(t.seen)
	t.mu.Unlock()

	if ready {
		t.mu.Lock()
		if !t.idle {
			t.idleSince = now
			t.nudgeSent = false
		}
		t.idle = true
		t.lastChange = now
		t.mu.Unlock()
		return
	}
}

func agentPTYHasActiveTurnSignal(b []byte) bool {
	normalized := strings.ToLower(agentPTYNormalizeForScreenDetection(b))
	if normalized == "" {
		return false
	}
	for _, needle := range []string{
		"esctointerrupt",
		"·thinking)",
		"runningstophooks",
		"runninghooks",
	} {
		if strings.Contains(normalized, needle) {
			return true
		}
	}
	return strings.Contains(normalized, "↓") && strings.Contains(normalized, "tokens")
}

func agentPTYHasComposerPromptSignal(b []byte) bool {
	normalized := strings.ToLower(agentPTYNormalizeForScreenDetection(b))
	if normalized == "" {
		return false
	}
	if !strings.Contains(normalized, "❯") {
		return false
	}
	return strings.Contains(normalized, "shifttabtocycle") || strings.Contains(normalized, "?forshortcuts")
}

func (t *agentPTYIdleTracker) IsIdle() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.idle
}

func (t *agentPTYIdleTracker) MarkNudgeReady(after time.Duration) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.idle || t.idleSince.IsZero() || t.nudgeSent {
		return 0, false
	}
	idleFor := time.Since(t.idleSince)
	if idleFor < after {
		return idleFor, false
	}
	t.nudgeSent = true
	return idleFor, true
}

func agentPTYSessionWriter(debug *agentDebugLog, stateWriter io.Writer, tracker *agentPTYIdleTracker) io.Writer {
	return io.MultiWriter(newAgentDebugStdoutWriter(debug, stateWriter), tracker)
}

func agentPushHeartbeat(aimebuURL, agentID string) error {
	c := &aimebuclient.Client{BaseURL: strings.TrimRight(aimebuURL, "/")}
	return c.HeartbeatAgent(agentID, 5*time.Second)
}

func agentPTYStartKeepalive(ctx context.Context, aimebuURL, agentID string, ptyFile io.Writer, tracker *agentPTYIdleTracker, debug *agentDebugLog) {
	go func() {
		ticker := time.NewTicker(agentPTYHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if tracker.IsIdle() {
					err := agentPushHeartbeat(aimebuURL, agentID)
					agentLogHeartbeat(debug, agentID, err)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				idleFor, ok := tracker.MarkNudgeReady(agentPTYIdleNudgeDelay)
				if !ok {
					continue
				}
				agentLogIdleNudge(debug, agentID, idleFor)
				agentPTYWriteIdleNudge(ptyFile, "keep listening", debug)
			case <-ctx.Done():
				return
			}
		}
	}()
}

func agentPTYWriteIdleNudge(w io.Writer, text string, debug *agentDebugLog) {
	if _, err := io.WriteString(w, "\x15"); err != nil {
		if debug != nil {
			debug.log("stdin_write_error", map[string]any{"error": err.Error(), "mode": "pty_idle_nudge_clear"})
		}
		return
	}
	agentPTYWritePrompt(w, text, debug)
}

// agentBootstrapSessionPTY is the PTY-mode bootstrap for the
// claude-code harness:
//
//  1. Pre-generates a session UUID and passes it via --session-id so resume
//     works without any output parsing. (--session-id confirmed to work in
//     interactive mode via smoke test with claude 2.1.140.)
//  2. Spawns claude under a pseudo-TTY.
//  3. Waits for the agent-ready composer signal, then writes the bootstrap prompt.
//  4. Drains PTY output to debug logging via a background goroutine.
//  5. Polls for agent-name registration on the bus.
//  6. Waits for the process to exit (context-cap hit or session end).
//
// No --debug or "Logging to:" parse is needed; --session-id gives us the UUID.
func agentBootstrapSessionPTY(harness string, command []string, prompt string, env []string, aimebuURL, spawnTag, knownName string, sigCh <-chan os.Signal, debug *agentDebugLog) (string, string, error) {
	startedAt := time.Now()

	preSessionID := agentGenSessionID()
	agentLogSessionIDPreGenerated(debug, harness, preSessionID)

	args := agentBootstrapArgs(harness, prompt, preSessionID, aimebuURL, command[1:], "")
	cmd := agentCommandForPTY(command, args, env)
	ptyFile, err := pty.Start(cmd)
	if err != nil {
		return "", "", err
	}
	_ = pty.Setsize(ptyFile, &pty.Winsize{Rows: agentPTYRows, Cols: agentPTYCols})
	agentLogHarnessSpawn(debug, command, args)

	agentID := newAgentIDProvider("")
	stateWriter := startAgentStatePusher(context.Background(), aimebuURL, agentID, newStateDetector(harness))
	ptyOutput := newAgentDebugStdoutWriter(debug, stateWriter)
	// Wait for the composer-ready signal while keeping the TUI hidden from the user.
	if err := agentPTYWaitCanary(ptyFile, ptyOutput, agentPTYStartupTimeout); err != nil {
		_ = stateWriter.Close()
		_ = ptyFile.Close()
		_ = cmd.Process.Signal(syscall.SIGTERM)
		return "", "", err
	}
	ptyOutput.Flush()
	agentPTYWritePrompt(ptyFile, prompt, debug)

	tracker := &agentPTYIdleTracker{}
	keepaliveCtx, stopKeepalive := context.WithCancel(context.Background())
	defer stopKeepalive()

	// Background goroutine drains the PTY for the rest of the session.
	go agentPTYCopyOut(ptyFile, agentPTYSessionWriter(debug, stateWriter, tracker))

	// Poll for agent-name registration in parallel.
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
	go func() { doneCh <- cmd.Wait() }()

	var waitErr error
	var agentName string
	registrationStallTimer := time.NewTimer(agentPTYRegistrationStallTimeout)
	defer registrationStallTimer.Stop()
	stallCh := registrationStallTimer.C
	registrationCh := (<-chan string)(nameCh)
	for waiting := true; waiting; {
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
			agentGracefulShutdown(aimebuURL, spawnTag, shutdownName, cmd, doneCh, sigCh, debug, sig)
			_ = ptyFile.Close()
			return "", shutdownName, agentErrInterrupted
		case agentName = <-registrationCh:
			if agentName == "" {
				agentPTYLogRegistrationStalled(debug, harness, agentPTYRegistrationStallTimeout)
				agentPTYTerminateStalledProcess(cmd, doneCh)
				_ = stateWriter.Close()
				_ = ptyFile.Close()
				return "", "", agentPTYRegistrationStalledError(harness)
			}
			agentPTYStartKeepalive(keepaliveCtx, aimebuURL, agentFullID(agentName), ptyFile, tracker, debug)
			if !registrationStallTimer.Stop() {
				select {
				case <-registrationStallTimer.C:
				default:
				}
			}
			stallCh = nil
			registrationCh = nil
		case <-stallCh:
			agentPTYLogRegistrationStalled(debug, harness, agentPTYRegistrationStallTimeout)
			agentPTYTerminateStalledProcess(cmd, doneCh)
			_ = stateWriter.Close()
			_ = ptyFile.Close()
			return "", "", agentPTYRegistrationStalledError(harness)
		case waitErr = <-doneCh:
			waiting = false
		}
	}

	stopKeepalive()
	_ = ptyFile.Close()
	agentLogHarnessExit(debug, waitErr, time.Since(startedAt), nil)

	// Bus-state-as-truth rescue: if the agent registered despite non-zero exit
	// (e.g. session cap hit mid-registration turn), treat this as a clean turn
	// end. Bus membership is the authoritative signal in PTY mode (no stream
	// events to parse).
	if waitErr != nil {
		if n := agentLookupName(aimebuURL, spawnTag, time.Second); n != "" {
			waitErr = nil
		}
	}
	if waitErr != nil {
		_ = stateWriter.Close()
		return "", "", waitErr
	}

	if agentName == "" {
		agentName = <-nameCh
	}
	if agentName == "" {
		_ = stateWriter.Close()
		return "", "", agentRegistrationMissingError(harness)
	}
	agentID.Set(agentFullID(agentName))
	_ = stateWriter.Close()
	_ = debug.setAgentName(agentName)
	return preSessionID, agentName, nil
}

// agentResumeLoopPTY is the PTY-mode resume loop for claude-code.
// Each iteration spawns a new claude process via --resume, waits for the
// composer-ready signal, writes "keep listening" (or a recovery prompt), drains
// PTY output to debug logging, and waits for the process to exit (context-cap
// hit). Preflight checks and recovery accounting mirror agentResumeLoop; the
// only difference is I/O (PTY ready-wait and pty.Write instead of stdin output).
func agentResumeLoopPTY(harness string, command []string, sessionID, agentName string, rooms []string, assumeRole string, env []string, aimebuURL string, sigCh <-chan os.Signal, debug *agentDebugLog) {
	retries := 0
	backoff := time.Second
	lastFailure := agentRecoveryNormalEnd
	consecutiveFailureCount := 0
	spawnTag := agentEnvValue(env, "AIMEBU_AGENT_SPAWN_TAG")
	if len(rooms) == 0 {
		rooms = nil
	}

respawnLoop:
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
				backoff = agentPTYBackoff(backoff)
				continue respawnLoop
			}
		}

		prompt := "keep listening"
		runMode := "resume"
		if recoveryClass == agentRecoveryRegistrationLost {
			prompt = agentBuildRecoveryPrompt(aimebuURL, harness, spawnTag, agentName, rooms, assumeRole, "")
			fmt.Fprintf(os.Stderr, "aimebu agent: registration missing for %s, re-registering in-session\n", agentFullID(agentName))
			agentLogRecoveryDecision(debug, recoveryClass, "preflight room membership missing", consecutiveFailureCount, 0)
		}

		args := agentResumeArgs(harness, sessionID, prompt, aimebuURL, command[1:], "")
		cmd := agentCommandForPTY(command, args, env)
		startedAt := time.Now()

		ptyFile, err := pty.Start(cmd)
		if err != nil {
			retries++
			if retries > agentRecoveryFailureCap {
				fmt.Fprintf(os.Stderr, "aimebu agent: too many consecutive harness failures, giving up\n")
				agentPushState(aimebuURL, agentFullID(agentName), "error")
				os.Exit(1)
			}
			agentPushState(aimebuURL, agentFullID(agentName), "respawning")
			fmt.Fprintf(os.Stderr, "aimebu agent: PTY spawn failed on %s (retry %d/%d in %v): %v\n", runMode, retries, agentRecoveryFailureCap, backoff, err)
			time.Sleep(backoff)
			backoff = agentPTYBackoff(backoff)
			continue
		}
		_ = pty.Setsize(ptyFile, &pty.Winsize{Rows: agentPTYRows, Cols: agentPTYCols})
		agentLogHarnessSpawn(debug, command, args)

		stateWriter := startAgentStatePusher(context.Background(), aimebuURL, newAgentIDProvider(agentFullID(agentName)), newStateDetector(harness))
		ptyOutput := newAgentDebugStdoutWriter(debug, stateWriter)
		// Wait for the composer-ready signal before writing the prompt.
		if err := agentPTYWaitCanary(ptyFile, ptyOutput, agentPTYStartupTimeout); err != nil {
			_ = stateWriter.Close()
			_ = ptyFile.Close()
			retries++
			if retries > agentRecoveryFailureCap {
				fmt.Fprintf(os.Stderr, "aimebu agent: too many consecutive harness failures, giving up\n")
				agentPushState(aimebuURL, agentFullID(agentName), "error")
				os.Exit(1)
			}
			agentPushState(aimebuURL, agentFullID(agentName), "respawning")
			fmt.Fprintf(os.Stderr, "aimebu agent: PTY ready-signal timeout on %s (retry %d/%d in %v): %v\n", runMode, retries, agentRecoveryFailureCap, backoff, err)
			time.Sleep(backoff)
			backoff = agentPTYBackoff(backoff)
			continue
		}
		ptyOutput.Flush()
		agentPTYWritePrompt(ptyFile, prompt, debug)

		tracker := &agentPTYIdleTracker{}
		keepaliveCtx, stopKeepalive := context.WithCancel(context.Background())
		agentPTYStartKeepalive(keepaliveCtx, aimebuURL, agentFullID(agentName), ptyFile, tracker, debug)
		go agentPTYCopyOut(ptyFile, agentPTYSessionWriter(debug, stateWriter, tracker))

		doneCh := make(chan error, 1)
		go func() { doneCh <- cmd.Wait() }()
		var registrationCh <-chan string
		if recoveryClass == agentRecoveryRegistrationLost {
			ch := make(chan string, 1)
			registrationCh = ch
			go func() {
				ch <- agentLookupName(aimebuURL, spawnTag, agentPTYRegistrationStallTimeout)
			}()
		}

		for waiting := true; waiting; {
			select {
			case sig := <-sigCh:
				stopKeepalive()
				_ = stateWriter.Close()
				agentGracefulShutdown(aimebuURL, "", agentName, cmd, doneCh, sigCh, debug, sig)
				_ = ptyFile.Close()
				return
			case n := <-registrationCh:
				if n == "" {
					stopKeepalive()
					agentPTYLogRegistrationStalled(debug, harness, agentPTYRegistrationStallTimeout)
					agentPTYTerminateStalledProcess(cmd, doneCh)
					_ = ptyFile.Close()
					_ = stateWriter.Close()
					fmt.Fprintf(os.Stderr, "aimebu agent: %v\n", agentPTYRegistrationStalledError(harness))
					retries++
					if retries > agentRecoveryFailureCap {
						fmt.Fprintf(os.Stderr, "aimebu agent: too many consecutive harness failures, giving up\n")
						agentPushState(aimebuURL, agentFullID(agentName), "error")
						os.Exit(1)
					}
					agentPushState(aimebuURL, agentFullID(agentName), "respawning")
					time.Sleep(backoff)
					backoff = agentPTYBackoff(backoff)
					continue respawnLoop
				}
				agentName = n
				stopKeepalive()
				keepaliveCtx, stopKeepalive = context.WithCancel(context.Background())
				agentPTYStartKeepalive(keepaliveCtx, aimebuURL, agentFullID(agentName), ptyFile, tracker, debug)
				registrationCh = nil
			case err = <-doneCh:
				if registrationCh != nil {
					select {
					case n := <-registrationCh:
						if n != "" {
							agentName = n
						}
					default:
					}
					registrationCh = nil
				}
				waiting = false
			}
		}
		stopKeepalive()
		_ = ptyFile.Close()
		_ = stateWriter.Close()
		agentLogHarnessExit(debug, err, time.Since(startedAt), nil)

		// PTY mode: rely on exit code; no result-event rescue.
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
		fmt.Fprintf(os.Stderr, "aimebu agent: %s failed (exit %d), retry %d/%d in %v\n",
			runMode, agentExitCode(err), retries, agentRecoveryFailureCap, backoff)
		time.Sleep(backoff)
		backoff = agentPTYBackoff(backoff)
	}
}

// agentPTYBackoff doubles backoff up to agentRecoveryMaxBackoff.
func agentPTYBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > agentRecoveryMaxBackoff {
		return agentRecoveryMaxBackoff
	}
	return d
}
