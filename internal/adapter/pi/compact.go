package pi

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
)

// Compact requests manual compaction in a separate, tool-free Pi RPC process.
// Print-mode /compact is a model prompt and does not compact the session.
func (a *Adapter) Compact(ctx context.Context, spec adapter.TurnSpec) (adapter.Compaction, error) {
	if spec.Agent.ID == "" || spec.Agent.Model == "" || spec.NativeSessionHandle == "" || spec.CheckoutPath == "" {
		return adapter.Compaction{}, errors.New("agent ID, model, session ID, and checkout path required")
	}
	if a.variant() == "native" && !a.AllowNative {
		return adapter.Compaction{}, errors.New("native execution disabled; use the docker variant")
	}
	if a.variant() == "docker" {
		if err := adapter.DockerCheckout(spec.CheckoutPath); err != nil {
			return adapter.Compaction{}, err
		}
	}
	sum := sha256.Sum256([]byte(spec.Agent.ID))
	sessionDir := filepath.Join(spec.CheckoutPath, ".aimebu", "pi-sessions", hex.EncodeToString(sum[:]))
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	command := a.Command
	if spec.LauncherPath != "" {
		command = spec.LauncherPath
	}
	cmd := exec.CommandContext(ctx, command, "--mode", "rpc", "--model", spec.Agent.Model,
		"--session-id", spec.NativeSessionHandle, "--session-dir", sessionDir,
		"--no-tools", "--no-mcp", "--no-context-files", "--no-skills", "--no-approve")
	cmd.Dir = spec.CheckoutPath
	cmd.Env = childEnv("")
	if a.variant() == "docker" {
		cmd.Env = append(cmd.Env, "PI_LAUNCH_FLAGS=")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return adapter.Compaction{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return adapter.Compaction{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return adapter.Compaction{}, err
	}
	if err := cmd.Start(); err != nil {
		return adapter.Compaction{}, err
	}
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
	}()
	if capture := adapter.JoinWriters(spec.StderrSink); capture != nil {
		go io.Copy(capture, stderr)
	} else {
		go io.Copy(io.Discard, stderr)
	}
	if _, err := io.WriteString(stdin, `{"type":"compact"}`+"\n"); err != nil {
		return adapter.Compaction{}, err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var event struct {
			Type         string `json:"type"`
			Command      string `json:"command"`
			Success      *bool  `json:"success"`
			Error        string `json:"error"`
			ErrorMessage string `json:"errorMessage"`
			Result       struct {
				TokensBefore         *int64 `json:"tokensBefore"`
				EstimatedTokensAfter *int64 `json:"estimatedTokensAfter"`
			} `json:"result"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		if event.Type == "response" && event.Command == "compact" && event.Success != nil && !*event.Success {
			return adapter.Compaction{}, fmt.Errorf("Pi compact: %s", event.Error)
		}
		if event.Type == "compaction_end" {
			if event.ErrorMessage != "" {
				return adapter.Compaction{}, fmt.Errorf("Pi compact: %s", event.ErrorMessage)
			}
			if event.Result.TokensBefore == nil || event.Result.EstimatedTokensAfter == nil {
				return adapter.Compaction{}, errors.New("Pi compact returned no token counts")
			}
			return adapter.Compaction{PreTokens: *event.Result.TokensBefore, PostTokens: *event.Result.EstimatedTokensAfter}, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return adapter.Compaction{}, err
	}
	if err := ctx.Err(); err != nil {
		return adapter.Compaction{}, err
	}
	return adapter.Compaction{}, errors.New("Pi compact ended without a result")
}
