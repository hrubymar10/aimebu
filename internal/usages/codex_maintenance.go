package usages

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"
)

const codexMaintenancePrompt = `write "hi"`

type codexRefreshExecutor struct{}

func (e codexRefreshExecutor) Refresh(ctx context.Context, profileDir, modelFlag string) error {
	_, err := e.Run(ctx, profileDir, modelFlag)
	return err
}

// Run is RunStarted without a start notification.
func (e codexRefreshExecutor) Run(ctx context.Context, profileDir, modelFlag string) (refreshRunResult, error) {
	return e.RunStarted(ctx, profileDir, modelFlag, nil)
}

// RunStarted is Refresh plus the facts the token refresher classifies a failure by.
func (codexRefreshExecutor) RunStarted(ctx context.Context, profileDir, modelFlag string, onStart func()) (refreshRunResult, error) {
	if err := ensureHarnessDockerImage(ctx); err != nil {
		return refreshRunResult{Started: false}, err
	}
	result, err := runDockerCapture(ctx, codexRefreshArgs(profileDir, modelFlag), onStart)
	if err != nil {
		return result, fmt.Errorf("harness-docker codex run failed: %w: %s", err, strings.TrimSpace(result.Output))
	}
	return result, nil
}

func codexRefreshArgs(profileDir, modelFlag string) []string {
	args := []string{"run", "--rm", "-v", profileDir + ":/config", "-e", "CODEX_HOME=/config", harnessDockerImage,
		"codex", "exec", "--skip-git-repo-check", codexMaintenancePrompt}
	if modelFlag != "" {
		args = append(args, strings.Fields(modelFlag)...)
	}
	return args
}

type codexMaintenanceState struct {
	inFlight    bool
	release     func() // the account-guard claim of the run in flight
	lastAttempt time.Time
}
type codexMaintainer struct {
	executor     refreshExecutor
	withLock     func(func() error) error
	listProfiles func() ([]ProfileInfo, error)
	available    func() bool
	reachable    func(context.Context) bool
	clock        Clock
	rescueRoot   string
	guard        *maintenanceGuard // shared per-(tool, profile) in-flight guard
	mu           sync.Mutex
	refresh      map[string]*codexMaintenanceState
	warm         map[string]*codexMaintenanceState
}

func newCodexMaintainer(withLock func(func() error) error, listProfiles func() ([]ProfileInfo, error)) *codexMaintainer {
	return &codexMaintainer{executor: codexRefreshExecutor{}, withLock: withLock, listProfiles: listProfiles,
		available: harnessDockerCtrlAvailable, reachable: dockerReachable, clock: realClock{}, guard: newMaintenanceGuard(),
		refresh: map[string]*codexMaintenanceState{}, warm: map[string]*codexMaintenanceState{}}
}

func (m *codexMaintainer) claim(states map[string]*codexMaintenanceState, profile ProfileInfo, cooldown time.Duration) bool {
	name := profile.Name
	m.mu.Lock()
	defer m.mu.Unlock()
	s := states[name]
	if s == nil {
		s = &codexMaintenanceState{}
		states[name] = s
	}
	now := m.clock.Now()
	if s.inFlight || (!s.lastAttempt.IsZero() && now.Sub(s.lastAttempt) < cooldown) {
		return false
	}
	// Refresh and warmup of one codex account must not overlap: both rotate the
	// same refresh token.
	release, ok := m.guard.acquire("codex", profile)
	if !ok {
		return false
	}
	s.release = release
	s.inFlight = true
	s.lastAttempt = now
	return true
}
func (m *codexMaintainer) finish(states map[string]*codexMaintenanceState, name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := states[name]; s != nil {
		if s.release != nil {
			s.release()
			s.release = nil
		}
		s.inFlight = false
	}
}

// codexProfileNeedsRefresh and codexProfileEligibleForWarmup exclude the active
// profile: the running codex sessions own its token and rotate it themselves.
func codexProfileNeedsRefresh(p ProfileInfo, now time.Time) bool {
	return p.Tool == "codex" && maintenanceTargetBlocked(p) == nil && maintenanceGraceBlocked(p, now) == nil && p.HasCredentials && p.ExpiresAt != nil && !now.Before(p.ExpiresAt.Add(-claudeNearExpiryWindow))
}
func codexProfileEligibleForWarmup(p ProfileInfo, now time.Time) bool {
	return p.Tool == "codex" && maintenanceTargetBlocked(p) == nil && maintenanceGraceBlocked(p, now) == nil && p.HasCredentials
}

// writeCodexAuthTemp seeds a throwaway CODEX_HOME with the resolved credential
// bytes as a regular file. The executor bind-mounts this dir, never the profile
// dir: an active profile's stored auth.json is a symlink to the host's live
// file, which would dangle inside the container.
func writeCodexAuthTemp(data []byte) (string, string, error) {
	return writeCodexAuthTempIn("", data)
}

// writeCodexAuthTempIn: see writeClaudeCredTempIn.
func writeCodexAuthTempIn(root string, data []byte) (string, string, error) {
	dir, err := makeRefreshTempDir(root, "aimebu-codex-maintenance-*")
	if err != nil {
		return "", "", err
	}
	path := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		os.RemoveAll(dir)
		return "", "", err
	}
	return dir, path, nil
}

func forceExpireCodexTempCredentials(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var root map[string]json.RawMessage
	if err = json.Unmarshal(data, &root); err != nil {
		return err
	}
	var tokens map[string]json.RawMessage
	if err = json.Unmarshal(root["tokens"], &tokens); err != nil {
		return errors.New("codex OAuth tokens missing")
	}
	key := "access_token"
	if _, ok := tokens[key]; !ok {
		key = "accessToken"
		if _, ok := tokens[key]; !ok {
			return errors.New("codex access token missing")
		}
	}
	tokens[key] = json.RawMessage(`""`)
	raw, _ := json.Marshal(tokens)
	root["tokens"] = raw
	out, err := json.Marshal(root)
	if err != nil {
		return err
	}
	return atomicWriteBytes(path, out, 0o600)
}

func (m *codexMaintainer) run(ctx context.Context, profile ProfileInfo, modelFlag string, force bool) error {
	if err := maintenanceTargetBlocked(profile); err != nil {
		return err
	}
	if err := maintenanceGraceBlockedBy(profile, m.clock); err != nil {
		return err
	}
	runStarted := time.Now()
	start, err := os.ReadFile(profile.CredPath)
	if err != nil {
		return err
	}
	old, _, err := loadCodexAuth(profile.CredPath)
	if err != nil {
		return err
	}
	dir, tmp, err := writeCodexAuthTemp(start)
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if force {
		if err := forceExpireCodexTempCredentials(tmp); err != nil {
			return err
		}
	}
	if m.reachable != nil && !m.reachable(ctx) {
		return errors.New("docker is not reachable")
	}
	executorErr := m.executor.Refresh(ctx, dir, modelFlag)
	rotated, err := os.ReadFile(tmp)
	if err != nil {
		if executorErr != nil {
			return executorErr
		}
		return err
	}
	unchanged := sha256.Sum256(rotated) == sha256.Sum256(start)
	if executorErr == nil && !force && unchanged {
		return nil
	}
	next, _, err := loadCodexAuth(tmp)
	if err != nil {
		if executorErr != nil {
			return executorErr
		}
		return fmt.Errorf("rotated auth invalid: %w", err)
	}
	credentialRotated := next.AccessToken != old.AccessToken && next.LastRefresh != nil && (old.LastRefresh == nil || next.LastRefresh.After(*old.LastRefresh))
	if executorErr != nil && !credentialRotated {
		return executorErr
	}
	if executorErr == nil && force && !credentialRotated {
		return errors.New("codex refresh did not rotate credentials")
	}
	if err := commitCodexAuthCopyBack(m.withLock, m.listProfiles, profile.Name, profile.CredPath, start, rotated, m.rescueRoot, runStarted); err != nil {
		return err
	}
	if executorErr != nil {
		log.Printf("usages: codex maintenance profile=%q outcome=rotated reason=executor-error-but-token-rotated: %v", profile.Name, executorErr)
	}
	return nil
}

func commitCodexAuthCopyBack(withLock func(func() error) error, listProfiles func() ([]ProfileInfo, error), name, path string, started, data []byte, rescueRoot string, runStarted ...time.Time) error {
	return commitRotatedCredentials("codex", withLock, listProfiles, name, path, started, data, rescueRoot, runStarted...)
}

func (m *codexMaintainer) maybeRefresh(ctx context.Context, p ProfileInfo, flag string) {
	if m == nil || m.executor == nil || m.available != nil && !m.available() || !m.claim(m.refresh, p, claudeRefreshFailBackoff) {
		return
	}
	log.Printf("usages: codex auto-refresh profile=%q outcome=starting", p.Name)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("usages: codex auto-refresh profile=%q outcome=failed reason=panic: %v", p.Name, recovered)
			}
			m.finish(m.refresh, p.Name)
		}()
		if err := m.run(ctx, p, flag, true); err != nil {
			log.Printf("usages: codex auto-refresh profile=%q outcome=failed reason=%v", p.Name, err)
		}
	}()
}
func (m *codexMaintainer) maybeWarm(ctx context.Context, p ProfileInfo, flag string) bool {
	if m == nil || m.executor == nil || m.available != nil && !m.available() || !m.claim(m.warm, p, claudeWarmupCooldown) {
		return false
	}
	log.Printf("usages: codex warmup profile=%q outcome=starting", p.Name)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("usages: codex warmup profile=%q outcome=failed reason=panic: %v", p.Name, recovered)
			}
			m.finish(m.warm, p.Name)
		}()
		runCtx, cancel := context.WithTimeout(ctx, claudeWarmupTimeout)
		defer cancel()
		if err := m.run(runCtx, p, flag, false); err != nil {
			log.Printf("usages: codex warmup profile=%q outcome=failed reason=%v", p.Name, err)
		}
	}()
	return true
}
func (m *codexMaintainer) refreshSync(ctx context.Context, p ProfileInfo, flag string) (bool, error) {
	if !m.claim(m.refresh, p, claudeRefreshFailBackoff) {
		return false, nil
	}
	defer m.finish(m.refresh, p.Name)
	return true, m.run(ctx, p, flag, true)
}
func (m *codexMaintainer) warmSync(ctx context.Context, p ProfileInfo, flag string) (bool, error) {
	if !m.claim(m.warm, p, claudeWarmupCooldown) {
		return false, nil
	}
	defer m.finish(m.warm, p.Name)
	return true, m.run(ctx, p, flag, false)
}
