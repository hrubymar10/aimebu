package usages

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"
)

const codexMaintenancePrompt = `write "hi"`

type codexRefreshExecutor struct{}

func (codexRefreshExecutor) Refresh(ctx context.Context, profileDir, modelFlag string) error {
	if err := ensureHarnessDockerImage(ctx); err != nil {
		return err
	}
	args := codexRefreshArgs(profileDir, modelFlag)
	if out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("harness-docker codex run failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
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
	lastAttempt time.Time
}
type codexMaintainer struct {
	executor  refreshExecutor
	withLock  func(func() error) error
	reresolve func(string) (ProfileInfo, bool)
	available func() bool
	reachable func(context.Context) bool
	clock     Clock
	mu        sync.Mutex
	refresh   map[string]*codexMaintenanceState
	warm      map[string]*codexMaintenanceState
}

func newCodexMaintainer(withLock func(func() error) error, reresolve func(string) (ProfileInfo, bool)) *codexMaintainer {
	return &codexMaintainer{executor: codexRefreshExecutor{}, withLock: withLock, reresolve: reresolve,
		available: harnessDockerCtrlAvailable, reachable: dockerReachable, clock: realClock{},
		refresh: map[string]*codexMaintenanceState{}, warm: map[string]*codexMaintenanceState{}}
}

func (m *codexMaintainer) claim(states map[string]*codexMaintenanceState, name string, cooldown time.Duration) bool {
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
	s.inFlight = true
	s.lastAttempt = now
	return true
}
func (m *codexMaintainer) finish(states map[string]*codexMaintenanceState, name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	states[name].inFlight = false
}

func codexProfileNeedsRefresh(p ProfileInfo, now time.Time) bool {
	return p.Tool == "codex" && p.HasCredentials && p.ExpiresAt != nil && !now.Before(p.ExpiresAt.Add(-claudeNearExpiryWindow))
}
func codexProfileEligibleForWarmup(p ProfileInfo) bool { return p.Tool == "codex" && p.HasCredentials }

func writeCodexAuthTemp(data []byte) (string, string, error) {
	dir, err := os.MkdirTemp("", "aimebu-codex-maintenance-*")
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
	if err := m.executor.Refresh(ctx, dir, modelFlag); err != nil {
		return err
	}
	rotated, err := os.ReadFile(tmp)
	if err != nil {
		return err
	}
	if !force && sha256.Sum256(rotated) == sha256.Sum256(start) {
		return nil
	}
	next, _, err := loadCodexAuth(tmp)
	if err != nil {
		return fmt.Errorf("rotated auth invalid: %w", err)
	}
	if force && (next.AccessToken == old.AccessToken || next.LastRefresh == nil || (old.LastRefresh != nil && !next.LastRefresh.After(*old.LastRefresh))) {
		return errors.New("codex refresh did not rotate credentials")
	}
	return commitCodexAuthCopyBack(m.withLock, m.reresolve, profile.Name, profile.CredPath, sha256.Sum256(start), rotated)
}

func commitCodexAuthCopyBack(withLock func(func() error) error, reresolve func(string) (ProfileInfo, bool), name, path string, fingerprint [32]byte, data []byte) error {
	commit := func() error {
		if reresolve != nil {
			p, ok := reresolve(name)
			if !ok {
				return errClaudeRefreshProfileGone
			}
			if p.CredPath != path {
				return errClaudeRefreshProfileChanged
			}
		}
		cur, err := os.ReadFile(path)
		if err != nil {
			return errClaudeRefreshProfileGone
		}
		if sha256.Sum256(cur) != fingerprint {
			return errClaudeRefreshProfileChanged
		}
		return atomicWriteBytes(path, data, 0o600)
	}
	if withLock != nil {
		return withLock(commit)
	}
	return commit()
}

func (m *codexMaintainer) maybeRefresh(ctx context.Context, p ProfileInfo, flag string) {
	if m == nil || m.executor == nil || m.available != nil && !m.available() || !m.claim(m.refresh, p.Name, claudeRefreshFailBackoff) {
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
	if m == nil || m.executor == nil || m.available != nil && !m.available() || !m.claim(m.warm, p.Name, claudeWarmupCooldown) {
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
	if !m.claim(m.refresh, p.Name, claudeRefreshFailBackoff) {
		return false, nil
	}
	defer m.finish(m.refresh, p.Name)
	return true, m.run(ctx, p, flag, true)
}
func (m *codexMaintainer) warmSync(ctx context.Context, p ProfileInfo, flag string) (bool, error) {
	if !m.claim(m.warm, p.Name, claudeWarmupCooldown) {
		return false, nil
	}
	defer m.finish(m.warm, p.Name)
	return true, m.run(ctx, p, flag, false)
}
