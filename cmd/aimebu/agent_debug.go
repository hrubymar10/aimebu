package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hrubymar10/aimebu/internal/config"
)

const agentDebugStdoutLineLimit = 4096

type agentDebugLog struct {
	mu       sync.Mutex
	enabled  bool
	path     string
	file     *os.File
	spawnTag string
}

func newAgentDebugLog(agentName, spawnTag string) *agentDebugLog {
	log := &agentDebugLog{
		enabled:  agentDebugEnabled(os.Getenv("AIMEBU_AGENT_DEBUG")),
		spawnTag: spawnTag,
	}
	if !log.enabled {
		return log
	}
	if err := log.setAgentName(agentName); err != nil {
		fmt.Fprintf(os.Stderr, "aimebu agent: debug logging disabled: %v\n", err)
		_ = log.close()
		log.enabled = false
	}
	return log
}

func agentDebugEnabled(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

// agentDebugDir returns the directory for per-agent JSONL debug logs.
// Sits under agents/ so it follows AIMEBU_CONFIG_DIR and the split layout.
func agentDebugDir() string {
	return filepath.Join(config.AgentsDir(), "agent-logs")
}

func agentDebugShortName(agentName string) string {
	if idx := strings.IndexByte(agentName, '@'); idx >= 0 {
		agentName = agentName[:idx]
	}
	return strings.TrimSpace(agentName)
}

func agentDebugLogPath(agentName, spawnTag string) string {
	dir := agentDebugDir()
	shortName := agentDebugShortName(agentName)
	if shortName != "" {
		return filepath.Join(dir, shortName+".log")
	}
	if spawnTag == "" {
		spawnTag = "unknown"
	}
	return filepath.Join(dir, "_pre-register-"+spawnTag+".log")
}

func (l *agentDebugLog) close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closeLocked()
}

func (l *agentDebugLog) closeLocked() error {
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

func (l *agentDebugLog) setAgentName(agentName string) error {
	if l == nil || !l.enabled {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	nextPath := agentDebugLogPath(agentName, l.spawnTag)
	if nextPath == l.path && l.file != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(nextPath), 0o700); err != nil {
		return err
	}

	oldPath := l.path
	if err := l.closeLocked(); err != nil {
		return err
	}
	if oldPath != "" && oldPath != nextPath {
		if err := agentMergeDebugLogFile(oldPath, nextPath); err != nil {
			return err
		}
	}

	file, err := os.OpenFile(nextPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	l.file = file
	l.path = nextPath
	return nil
}

func agentMergeDebugLogFile(srcPath, dstPath string) error {
	if srcPath == "" || srcPath == dstPath {
		return nil
	}
	if _, err := os.Stat(srcPath); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := os.Stat(dstPath); os.IsNotExist(err) {
		return os.Rename(srcPath, dstPath)
	} else if err != nil {
		return err
	}

	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.OpenFile(dstPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	return os.Remove(srcPath)
}

func (l *agentDebugLog) log(event string, fields map[string]any) {
	if l == nil || !l.enabled {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return
	}

	record := map[string]any{
		"ts":    time.Now().UTC().Format(time.RFC3339),
		"event": event,
	}
	for key, value := range fields {
		record[key] = value
	}
	enc := json.NewEncoder(l.file)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(record)
}

type agentDebugStdoutWriter struct {
	logger   *agentDebugLog
	next     io.Writer
	pending  []byte
	lineNo   int
	lineType string
}

func newAgentDebugStdoutWriter(logger *agentDebugLog, next io.Writer) *agentDebugStdoutWriter {
	return &agentDebugStdoutWriter{
		logger:   logger,
		next:     next,
		lineType: "harness_stdout_raw",
	}
}

func (w *agentDebugStdoutWriter) Write(p []byte) (int, error) {
	n, err := w.next.Write(p)
	if n > 0 {
		w.consume(p[:n])
	}
	return n, err
}

func (w *agentDebugStdoutWriter) Flush() {
	if len(w.pending) == 0 {
		return
	}
	w.emit(w.pending)
	w.pending = nil
}

func (w *agentDebugStdoutWriter) consume(p []byte) {
	for len(p) > 0 {
		idx := bytes.IndexByte(p, '\n')
		if idx < 0 {
			w.pending = append(w.pending, p...)
			return
		}
		w.pending = append(w.pending, p[:idx]...)
		w.emit(w.pending)
		w.pending = nil
		p = p[idx+1:]
	}
}

func (w *agentDebugStdoutWriter) emit(line []byte) {
	if w.logger == nil {
		w.lineNo++
		return
	}
	truncated := false
	if len(line) > agentDebugStdoutLineLimit {
		line = append([]byte(nil), line[:agentDebugStdoutLineLimit]...)
		truncated = true
	}
	w.logger.log(w.lineType, map[string]any{
		"line":       string(line),
		"line_index": w.lineNo + 1,
		"truncated":  truncated,
	})
	w.lineNo++
}

func agentDebugTail(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[len(b)-max:])
}

func agentExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func agentLogWrapperStart(debug *agentDebugLog, wrapperArgs []string, harness string, rooms []string, spawnTag, resumeMode, aimebuURL, aimebuHarness string) {
	if debug == nil {
		return
	}
	debug.log("wrapper_start", map[string]any{
		"args":           append([]string(nil), wrapperArgs...),
		"harness":        harness,
		"rooms":          append([]string(nil), rooms...),
		"spawn_tag":      spawnTag,
		"resume_mode":    resumeMode,
		"aimebu_url":     aimebuURL,
		"aimebu_harness": aimebuHarness,
	})
}

func agentLogHarnessSpawn(debug *agentDebugLog, command []string, args []string) {
	if debug == nil {
		return
	}
	debug.log("harness_spawn", map[string]any{
		"command": command[0],
		"args":    append([]string(nil), args...),
	})
}

func agentLogRegisterObserved(debug *agentDebugLog, agentID string, elapsed time.Duration) {
	if debug == nil {
		return
	}
	_ = debug.setAgentName(agentID)
	debug.log("register_observed", map[string]any{
		"agent_id":   agentID,
		"elapsed_ms": elapsed.Milliseconds(),
	})
}

func agentLogSessionIDParsed(debug *agentDebugLog, harness, parsedID string, lineIndex int) {
	if debug == nil {
		return
	}
	debug.log("session_id_parsed", map[string]any{
		"harness":           harness,
		"parsed_id":         parsedID,
		"source_line_index": lineIndex,
	})
}

// agentLogSessionIDPreGenerated records that the session ID was generated
// driver-side (claude-code PTY path) rather than parsed from child output.
func agentLogSessionIDPreGenerated(debug *agentDebugLog, harness, sessionID string) {
	if debug == nil {
		return
	}
	debug.log("session_id_pregenerated", map[string]any{
		"harness":    harness,
		"session_id": sessionID,
	})
}

func agentLogHarnessExit(debug *agentDebugLog, err error, wallTime time.Duration, stderr []byte) {
	if debug == nil {
		return
	}
	debug.log("harness_exit", map[string]any{
		"exit_code":    agentExitCode(err),
		"wall_time_ms": wallTime.Milliseconds(),
		"stderr_tail":  agentDebugTail(stderr, 1024),
	})
}

func agentLogRecoveryDecision(debug *agentDebugLog, class agentRecoveryClass, trigger string, retryCount int, backoff time.Duration) {
	if debug == nil {
		return
	}
	debug.log("recovery_decision", map[string]any{
		"class":       class,
		"trigger":     trigger,
		"retry_count": retryCount,
		"backoff_ms":  backoff.Milliseconds(),
	})
}

func agentLogWrapperShutdown(debug *agentDebugLog, signalName, attemptedID, result string) {
	if debug == nil {
		return
	}
	debug.log("wrapper_shutdown", map[string]any{
		"signal":               signalName,
		"deregister_attempted": attemptedID != "",
		"agent_id":             attemptedID,
		"result":               result,
	})
}
