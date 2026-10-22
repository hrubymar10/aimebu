package usages

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/usages/providers"
)

var extraKeys = []string{providers.ProviderGitHubCopilot, providers.ProviderMistral, providers.ProviderOllamaCloud}

func (r *Reader) ExtraStore() *providers.Store { return r.extra }

func decorateResponse(response *Response, cfg providers.Config) {
	response.Settings.PercentDisplay = cfg.PercentDisplay
	response.Settings.RefreshIntervalSec = cfg.RefreshIntervalSec
	for i := range response.Providers {
		provider := &response.Providers[i]
		for order, key := range cfg.ProviderOrder {
			if key == provider.ProviderName {
				provider.OrderNumber = order
				break
			}
		}
		pc, present := cfg.Providers[provider.ProviderName]
		if present {
			provider.Enabled = pc.Enabled
		}
		if provider.ProviderName == providers.ProviderGitHubCopilot {
			provider.EnterpriseHost = pc.EnterpriseHost
			provider.TokenConfigured = strings.TrimSpace(pc.Token) != ""
			provider.Profiles[0].HasCredentials = provider.TokenConfigured
		}
		if provider.ProviderName == providers.ProviderMistral {
			provider.CookieConfigured = strings.TrimSpace(pc.Cookie) != ""
			provider.Profiles[0].HasCredentials = provider.CookieConfigured
		}
		if provider.ProviderName == providers.ProviderOllamaCloud {
			provider.CookieConfigured = strings.TrimSpace(pc.Cookie) != ""
			provider.APIKeyConfigured = strings.TrimSpace(pc.APIKey) != ""
			provider.AuthMode = providers.NormalizeOllamaAuthMode(pc.AuthMode)
			provider.Profiles[0].HasCredentials = provider.CookieConfigured || provider.APIKeyConfigured
		}
		if !provider.Enabled {
			provider.Profiles = []Profile{{ProfileName: "local", Active: true, Status: "disabled", HasCredentials: provider.Profiles[0].HasCredentials}}
		} else if (provider.ProviderName == providers.ProviderGitHubCopilot || provider.ProviderName == providers.ProviderMistral || provider.ProviderName == providers.ProviderOllamaCloud) && !provider.Profiles[0].HasCredentials {
			provider.Profiles[0] = Profile{ProfileName: "local", Active: true, Status: "not_configured"}
		}
	}
}

// applySnapshot copies a provider snapshot onto base, keeping base's identity
// fields (profile name, active flag, credentials, email, expiry).
func applySnapshot(base Profile, snap providers.Snapshot) Profile {
	base.Status = string(snap.Status)
	base.Plan = snap.Plan
	base.Error = snap.Error
	base.ErrorDetail = snap.ErrorDetail
	base.Windows = nil
	for _, window := range snap.Windows {
		item := Window{Key: window.Key, PercentUsed: window.PercentUsed, ResetAt: window.ResetAt, WindowDurationSeconds: window.WindowDurationSeconds}
		if window.Pace != nil {
			item.Pace = &Pace{ExpectedPercent: window.Pace.ExpectedPercent, DeltaPercent: window.Pace.DeltaPercent, State: window.Pace.State, EtaSeconds: window.Pace.EtaSeconds, LastsToReset: window.Pace.LastsToReset}
		}
		base.Windows = append(base.Windows, item)
	}
	base.Credits = nil
	if snap.Credits != nil {
		base.Credits = &Credits{Label: snap.Credits.Label, Balance: snap.Credits.Balance, SpendLimit: snap.Credits.SpendLimit}
	}
	return base
}

func (r *Reader) RefreshProvider(ctx context.Context, key string) {
	switch key {
	case providers.ProviderCodex:
		r.refreshLocalOne(ctx, 0)
		r.refreshStored(ctx, 0)
		return
	case providers.ProviderClaudeCode:
		r.refreshLocalOne(ctx, 1)
		r.refreshStored(ctx, 1)
		return
	}
	for _, known := range extraKeys {
		if key == known {
			r.refreshExtraOne(ctx, key)
			return
		}
	}
}

func (r *Reader) refreshExtra(ctx context.Context) {
	for _, key := range extraKeys {
		if ctx.Err() != nil {
			return
		}
		r.refreshExtraOne(ctx, key)
	}
}

func (r *Reader) refreshExtraOne(ctx context.Context, key string) {
	cfg, err := r.extra.LoadConfig()
	if err != nil || !cfg.Providers[key].Enabled {
		return
	}
	pc := cfg.Providers[key]
	identityJSON, _ := json.Marshal(pc)
	identityHash := sha256.Sum256(identityJSON)
	identity := hex.EncodeToString(identityHash[:])
	provider, exists := providers.DefaultRegistry().Provider(key)
	if !exists {
		return
	}
	r.mu.RLock()
	generation := r.generation
	providerGeneration := r.providerGeneration[key]
	r.mu.RUnlock()
	snap, fetchErr := provider.Fetch(ctx, r.extra)
	var wrapped *providers.SnapshotError
	if errors.As(fetchErr, &wrapped) {
		snap = wrapped.Snapshot
	}
	snap = snap.Redacted(pc.Token, pc.Cookie, pc.APIKey)
	email, fetchedEmail := "", false
	if key == providers.ProviderGitHubCopilot && snap.Status == providers.StatusOK {
		r.mu.RLock()
		cached := r.copilotEmailScope == identity && time.Since(r.copilotEmailAt) < time.Hour
		if cached {
			email = r.copilotEmail
		}
		r.mu.RUnlock()
		if !cached {
			if fetcher, ok := provider.(providers.EmailFetcher); ok {
				email, _ = fetcher.FetchEmail(ctx, r.extra)
				fetchedEmail = true
			}
		}
	}
	email = providers.RedactString(email, pc.Token, pc.Cookie, pc.APIKey)
	latest, err := r.extra.LoadConfig()
	if err != nil || latest.Providers[key] != pc || !latest.Providers[key].Enabled {
		return
	}
	now := time.Now().UTC()
	profile := applySnapshot(Profile{ProfileName: "local", Active: true, HasCredentials: pc.Token != "" || pc.Cookie != "" || pc.APIKey != "", Email: email,
		AlertCredential: alertHash(key + "\x00" + pc.Token + "\x00" + pc.Cookie + "\x00" + pc.APIKey)}, snap)
	profile.LastRefreshAt = &now
	r.mu.Lock()
	if generation != r.generation || providerGeneration != r.providerGeneration[key] {
		r.mu.Unlock()
		return
	}
	if fetchedEmail {
		r.copilotEmailScope = identity
		r.copilotEmailAt = now
		r.copilotEmail = email
	}
	for i := range r.response.Providers {
		if r.response.Providers[i].ProviderName != key {
			continue
		}
		previous := r.response.Providers[i].Profiles[0]
		if fetchErr != nil && r.extraOwners[key] == identity && (snap.Status == providers.StatusTimeout || snap.Status == providers.StatusFetchError) && (previous.Status == "ok" || previous.Status == "stale_cache") {
			profile.Windows = previous.Windows
			profile.Credits = previous.Credits
			profile.Plan = previous.Plan
			profile.Email = previous.Email
			profile.LastRefreshAt = previous.LastRefreshAt
			profile.Stale = true
			profile.Status = "stale_cache"
		}
		r.response.Providers[i].Profiles = []Profile{profile}
		r.recordHistoryLocked(key, profile)
		r.extraOwners[key] = identity
		break
	}
	data, marshalErr := json.Marshal(r.response)
	if marshalErr == nil {
		_ = writeCache(r.cachePath, data)
	}
	r.signalLocked()
	r.mu.Unlock()
}

// ConfigChanged schedules only the provider whose credentials or enabled
// state changed. A credential change drops that provider's old snapshot.
func (r *Reader) ConfigChanged(key string, enabled, reset bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pendingExtra[key] = true
	r.providerGeneration[key]++
	for i := range r.response.Providers {
		if r.response.Providers[i].ProviderName != key {
			continue
		}
		r.response.Providers[i].Enabled = enabled
		if !enabled {
			hadCredentials := r.response.Providers[i].Profiles[0].HasCredentials && !reset
			r.response.Providers[i].Profiles = []Profile{{ProfileName: "local", Active: true, Status: "disabled", HasCredentials: hadCredentials}}
		} else if reset || r.response.Providers[i].Profiles[0].Status == "disabled" {
			r.response.Providers[i].Profiles = []Profile{{ProfileName: "local", Active: true, Status: "refreshing"}}
		}
	}
	if reset {
		delete(r.extraOwners, key)
	}
	if data, err := json.Marshal(r.response); err == nil {
		_ = writeCache(r.cachePath, data)
	}
	r.signalLocked()
}

func (r *Reader) ConfigPath() string { return filepath.Join(r.extra.Root, "config.json") }

// ModelFlag is the optional model flag ("--model haiku") the token refresher
// passes to a tool's CLI run, from the usage settings; empty when unset or the
// settings cannot be read.
func (r *Reader) ModelFlag(tool string) string {
	cfg, err := r.extra.LoadConfig()
	if err != nil {
		return ""
	}
	if tool == "codex" {
		return cfg.CodexModelFlag
	}
	return cfg.ClaudeModelFlag
}
