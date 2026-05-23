package usages

import (
	"context"
	"fmt"
	"sort"
	"time"
)

type Status string

const (
	StatusOK            Status = "ok"
	StatusAuthMissing   Status = "auth_missing"
	StatusScopeMissing  Status = "scope_missing"
	StatusNotConfigured Status = "not_configured"
	StatusFetchError    Status = "fetch_error"
	StatusTimeout       Status = "timeout"
	StatusStaleCache    Status = "stale_cache"
)

const (
	ProviderCodex         = "codex"
	ProviderClaudeCode    = "claude-code"
	ProviderGitHubCopilot = "github-copilot"
	ProviderOllamaCloud   = "ollama-cloud"
)

var knownProviders = []string{
	ProviderCodex,
	ProviderClaudeCode,
	ProviderGitHubCopilot,
	ProviderOllamaCloud,
}

var providerLabels = map[string]string{
	ProviderCodex:         "Codex",
	ProviderClaudeCode:    "Claude Code",
	ProviderGitHubCopilot: "GitHub Copilot",
	ProviderOllamaCloud:   "Ollama Cloud",
}

type Window struct {
	Key         string     `json:"key"`
	PercentUsed float64    `json:"percent_used"`
	ResetAt     *time.Time `json:"reset_at,omitempty"`
}

type Credits struct {
	Label      string  `json:"label,omitempty"`
	Balance    float64 `json:"balance"`
	SpendLimit float64 `json:"spend_limit,omitempty"`
}

type ErrorDetail struct {
	Fields map[string]string `json:"fields,omitempty"`
}

type Snapshot struct {
	Provider      string       `json:"provider"`
	Status        Status       `json:"status"`
	Plan          string       `json:"plan,omitempty"`
	Windows       []Window     `json:"windows,omitempty"`
	Credits       *Credits     `json:"credits,omitempty"`
	LastRefreshAt *time.Time   `json:"last_refresh_at,omitempty"`
	Stale         bool         `json:"stale,omitempty"`
	Error         string       `json:"error,omitempty"`
	ErrorDetail   *ErrorDetail `json:"error_detail,omitempty"`
}

type Response struct {
	Snapshots map[string]Snapshot `json:"snapshots"`
	Settings  *SettingsInfo       `json:"settings,omitempty"`
	Providers []ProviderInfo      `json:"providers,omitempty"`
}

type SettingsInfo struct {
	RefreshIntervalSec int    `json:"refresh_interval_sec"`
	MinRefreshSec      int    `json:"min_refresh_sec"`
	EnvOverride        bool   `json:"env_override"`
	EnvValue           string `json:"env_value,omitempty"`
	PercentDisplay     string `json:"percent_display"`
}

type ProviderInfo struct {
	Key              string `json:"key"`
	Label            string `json:"label"`
	Enabled          bool   `json:"enabled"`
	Available        bool   `json:"available"`
	EnterpriseHost   string `json:"enterprise_host,omitempty"`
	CookieConfigured bool   `json:"cookie_configured,omitempty"`
}

type Provider interface {
	Key() string
	Fetch(ctx context.Context, store *Store) (Snapshot, error)
}

type RawDecoder[T any] interface {
	Decode([]byte) (T, *ErrorDetail, error)
}

type Normalizer[T any] interface {
	Normalize(T) (Snapshot, *ErrorDetail, error)
}

type Registry struct {
	providers map[string]Provider
}

func NewRegistry(providers ...Provider) *Registry {
	r := &Registry{providers: make(map[string]Provider)}
	for _, p := range providers {
		if p == nil {
			continue
		}
		r.providers[p.Key()] = p
	}
	return r
}

func EmptyRegistry() *Registry { return NewRegistry() }

func DefaultRegistry() *Registry {
	return NewRegistry(NewCodexProvider(), NewClaudeCodeProvider(), NewCopilotProvider(), NewOllamaCloudProvider())
}

func (r *Registry) Provider(key string) (Provider, bool) {
	if r == nil {
		return nil, false
	}
	p, ok := r.providers[key]
	return p, ok
}

func (r *Registry) Keys() []string {
	if r == nil || len(r.providers) == 0 {
		return nil
	}
	keys := make([]string, 0, len(r.providers))
	for key := range r.providers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (r *Registry) HasProvider(key string) bool {
	_, ok := r.Provider(key)
	return ok
}

func KnownProviders() []string {
	out := append([]string(nil), knownProviders...)
	return out
}

func KnownProvider(key string) bool {
	for _, known := range knownProviders {
		if key == known {
			return true
		}
	}
	return false
}

func ProviderLabel(key string) string {
	if label, ok := providerLabels[key]; ok {
		return label
	}
	return key
}

func normalizeProviderOrder(order []string) []string {
	seen := make(map[string]bool, len(knownProviders))
	out := make([]string, 0, len(knownProviders))
	for _, key := range order {
		if !KnownProvider(key) || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	for _, key := range knownProviders {
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}

func ProviderInfos(cfg Config, registry *Registry) []ProviderInfo {
	out := make([]ProviderInfo, 0, len(knownProviders))
	for _, key := range normalizeProviderOrder(cfg.ProviderOrder) {
		pc := cfg.Providers[key]
		out = append(out, ProviderInfo{
			Key:              key,
			Label:            ProviderLabel(key),
			Enabled:          pc.Enabled,
			Available:        registry.HasProvider(key),
			EnterpriseHost:   pc.EnterpriseHost,
			CookieConfigured: key == ProviderOllamaCloud && pc.Cookie != "",
		})
	}
	return out
}

func unknownProviderError(key string) error {
	return fmt.Errorf("unknown provider %q (allowed: %v)", key, knownProviders)
}

type SnapshotError struct {
	Snapshot Snapshot
	Err      error
}

func (e *SnapshotError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Snapshot.Error
}

func (e *SnapshotError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}
