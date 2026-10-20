package usages

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// harnessDockerImage is the ephemeral container that runs the real claude or
	// codex binary against a mounted credential dir. Inside the container there is
	// no macOS Keychain, so the CLI's lazy OAuth refresh-on-use writes the rotated
	// tokens straight back into the mounted file.
	harnessDockerImage      = "harness-docker:latest"
	harnessDockerCtrlBinary = "harness-docker-ctrl"

	// refreshPrompt is a trivial no-op prompt: running the CLI at all is enough to
	// trigger the refresh-on-use of an expired access token.
	refreshPrompt = `write "hi"`

	dockerReachableTimeout = 5 * time.Second
)

// refreshExecutor runs whatever side-effecting step rotates the credentials in
// dir. The real implementations run the CLI inside an ephemeral harness-docker
// container; tests inject a fake.
type refreshExecutor interface {
	Refresh(ctx context.Context, dir, modelFlag string) error
}

// dockerRefreshExecutor is the production claude executor.
type dockerRefreshExecutor struct{}

func (e dockerRefreshExecutor) Refresh(ctx context.Context, dir, modelFlag string) error {
	_, err := e.RunStarted(ctx, dir, modelFlag, nil)
	return err
}

// RunStarted is Refresh plus what the refresher needs to classify a failure:
// whether the CLI process was started at all (a refresh that never reached the
// CLI cannot have spent the refresh token), the bounded CLI output, and a
// notification at the moment the process exists.
func (dockerRefreshExecutor) RunStarted(ctx context.Context, dir, modelFlag string, onStart func()) (refreshRunResult, error) {
	if err := ensureHarnessDockerImage(ctx); err != nil {
		return refreshRunResult{Started: false}, err
	}
	result, err := runDockerCapture(ctx, dockerRefreshArgs(dir, modelFlag), onStart)
	if err != nil {
		return result, fmt.Errorf("harness-docker claude refresh run failed: %w: %s", err, strings.TrimSpace(result.Output))
	}
	return result, nil
}

func dockerRefreshArgs(dir, modelFlag string) []string {
	args := []string{
		"run", "--rm",
		"-v", dir + ":/config",
		"-e", "CLAUDE_CONFIG_DIR=/config",
		harnessDockerImage,
		"claude", "-p", refreshPrompt,
	}
	if modelFlag != "" {
		args = append(args, strings.Fields(modelFlag)...)
	}
	return args
}

// codexRefreshExecutor is the production codex executor.
type codexRefreshExecutor struct{}

func (e codexRefreshExecutor) Refresh(ctx context.Context, dir, modelFlag string) error {
	_, err := e.RunStarted(ctx, dir, modelFlag, nil)
	return err
}

// RunStarted: see dockerRefreshExecutor.RunStarted.
func (codexRefreshExecutor) RunStarted(ctx context.Context, dir, modelFlag string, onStart func()) (refreshRunResult, error) {
	if err := ensureHarnessDockerImage(ctx); err != nil {
		return refreshRunResult{Started: false}, err
	}
	result, err := runDockerCapture(ctx, codexRefreshArgs(dir, modelFlag), onStart)
	if err != nil {
		return result, fmt.Errorf("harness-docker codex run failed: %w: %s", err, strings.TrimSpace(result.Output))
	}
	return result, nil
}

func codexRefreshArgs(dir, modelFlag string) []string {
	args := []string{"run", "--rm", "-v", dir + ":/config", "-e", "CODEX_HOME=/config", harnessDockerImage,
		"codex", "exec", "--skip-git-repo-check", refreshPrompt}
	if modelFlag != "" {
		args = append(args, strings.Fields(modelFlag)...)
	}
	return args
}

// ensureHarnessDockerImage builds the image via harness-docker-ctrl when it is
// not already present locally.
func ensureHarnessDockerImage(ctx context.Context) error {
	if exec.CommandContext(ctx, "docker", "image", "inspect", harnessDockerImage).Run() == nil {
		return nil
	}
	build := exec.CommandContext(ctx, harnessDockerCtrlBinary, "build-image")
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("harness-docker-ctrl build-image failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// harnessDockerCtrlAvailable reports whether the harness-docker-ctrl binary is
// on PATH: without it the refresher cannot run a CLI at all.
func harnessDockerCtrlAvailable() bool {
	_, err := exec.LookPath(harnessDockerCtrlBinary)
	return err == nil
}

// HarnessDockerCtrlAvailable reports whether the refresh executor can run at all
// (the Settings UI shows the token auto-refresh toggle as unavailable otherwise).
func HarnessDockerCtrlAvailable() bool { return harnessDockerCtrlAvailable() }

// DockerReachable is dockerReachable for the doctor: the refresher runs `docker` itself (image inspect, run), so the
// binary on PATH and a daemon that answers are what it needs besides harness-docker-ctrl.
func DockerReachable(ctx context.Context) bool { return dockerReachable(ctx) }

// dockerReachable reports whether the docker daemon answers. Kept short so a
// down daemon does not stall a refresh attempt for long.
func dockerReachable(ctx context.Context) bool {
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	checkCtx, cancel := context.WithTimeout(ctx, dockerReachableTimeout)
	defer cancel()
	return exec.CommandContext(checkCtx, "docker", "info").Run() == nil
}

// writeClaudeCredTempIn creates a throwaway dir seeded with only a
// .credentials.json holding data, so the claude binary pollutes the temp dir
// instead of anything real. The dir is created under the switcher root's
// refresh-tmp (0700) when root is set, so a crashed run's copy survives a reboot
// for recovery and is swept later. The caller owns removing it.
func writeClaudeCredTempIn(root string, data []byte, prefix string) (string, string, error) {
	tmpDir, err := makeRefreshTempDir(root, prefix)
	if err != nil {
		return "", "", fmt.Errorf("create temp dir: %w", err)
	}
	tmpCred := filepath.Join(tmpDir, ".credentials.json")
	if err := atomicWriteBytes(tmpCred, data, 0o600); err != nil {
		_ = os.RemoveAll(tmpDir)
		return "", "", fmt.Errorf("seed temp credentials: %w", err)
	}
	return tmpDir, tmpCred, nil
}

// writeCodexAuthTempIn seeds a throwaway CODEX_HOME with the credential bytes as
// a regular file; see writeClaudeCredTempIn.
func writeCodexAuthTempIn(root string, data []byte) (string, string, error) {
	dir, err := makeRefreshTempDir(root, "aimebu-codex-maintenance-*")
	if err != nil {
		return "", "", err
	}
	path := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", err
	}
	return dir, path, nil
}
