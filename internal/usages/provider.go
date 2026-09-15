package usages

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
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
	ProviderMistral       = "mistral"
	ProviderOllamaCloud   = "ollama-cloud"
)

var knownProviders = []string{
	ProviderCodex,
	ProviderClaudeCode,
	ProviderGitHubCopilot,
	ProviderMistral,
	ProviderOllamaCloud,
}

var providerLabels = map[string]string{
	ProviderCodex:         "Codex",
	ProviderClaudeCode:    "Claude Code",
	ProviderGitHubCopilot: "GitHub Copilot",
	ProviderMistral:       "Mistral",
	ProviderOllamaCloud:   "Ollama Cloud",
}

type Window struct {
	Key                   string     `json:"key"`
	PercentUsed           float64    `json:"percent_used"`
	ResetAt               *time.Time `json:"reset_at,omitempty"`
	WindowDurationSeconds int64      `json:"window_duration_seconds,omitempty"`
	Pace                  *Pace      `json:"pace,omitempty"`
}

func normalizedWindow(key string, percentUsed float64, resetAt *time.Time, durationSeconds int64) Window {
	return Window{
		Key:                   key,
		PercentUsed:           clampUsagePercent(percentUsed),
		ResetAt:               resetAt,
		WindowDurationSeconds: durationSeconds,
	}
}

func clampUsagePercent(value float64) float64 {
	if value < 0 || math.IsNaN(value) {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

// Pace holds the precomputed linear-spend pace for a usage window.
type Pace struct {
	ExpectedPercent float64  `json:"expected_percent"`
	DeltaPercent    float64  `json:"delta_percent"`
	State           string   `json:"state"`
	EtaSeconds      *float64 `json:"eta_seconds,omitempty"`
	LastsToReset    bool     `json:"lasts_to_reset,omitempty"`
}

type Credits struct {
	Label      string  `json:"label,omitempty"`
	Balance    float64 `json:"balance"`
	SpendLimit float64 `json:"spend_limit,omitempty"`
}

type ErrorDetail struct {
	Fields map[string]string `json:"fields,omitempty"`
}

// Snapshot is the internal provider return type — usage data only. The manager
// converts it to a Profile (which adds switcher facts) at fetch time.
type Snapshot struct {
	Status        Status       `json:"status"`
	Plan          string       `json:"plan,omitempty"`
	Windows       []Window     `json:"windows,omitempty"`
	Credits       *Credits     `json:"credits,omitempty"`
	LastRefreshAt *time.Time   `json:"last_refresh_at,omitempty"`
	Stale         bool         `json:"stale,omitempty"`
	Error         string       `json:"error,omitempty"`
	ErrorDetail   *ErrorDetail `json:"error_detail,omitempty"`
}

// UsagesResponse is the unified response for GET /api/usages. There is no
// join: each Profile carries both usage data and switcher facts. Providers is
// an ordered slice (by OrderNumber), not a map.
type UsagesResponse struct {
	Providers       []Provider `json:"providers"`
	Settings        Settings   `json:"settings"`
	SwitcherEnabled bool       `json:"switcher_enabled"`
}

type Settings struct {
	RefreshIntervalSec int    `json:"refresh_interval_sec"`
	MinRefreshSec      int    `json:"min_refresh_sec"`
	PercentDisplay     string `json:"percent_display"` // "left" | "used"
	EnvOverride        bool   `json:"env_override"`
	EnvValue           string `json:"env_value,omitempty"`
	// ClaudeAutoRefresh is the stored on/off flag for claude auto-refresh.
	ClaudeAutoRefresh bool `json:"claude_auto_refresh"`
	// ClaudeAutoRefreshAvailable reports whether the feature can run at all —
	// currently whether harness-docker-ctrl is on PATH. The UI presents the
	// toggle as unavailable when this is false.
	ClaudeAutoRefreshAvailable bool `json:"claude_auto_refresh_available"`
}

// Provider absorbs the old ProviderInfo, provider_order setting, and switcher
// eligibility into one object. Profiles carries the per-profile data.
type Provider struct {
	ProviderName     string    `json:"provider_name"`
	Label            string    `json:"label"`
	OrderNumber      int       `json:"order_number"`
	Enabled          bool      `json:"enabled"`
	Available        bool      `json:"available"`
	SwitchEligible   bool      `json:"switch_eligible,omitempty"`
	SwitchIneligible string    `json:"switch_ineligible_reason,omitempty"`
	Profiles         []Profile `json:"profiles"`
}

// Profile carries both usage data (Status, Plan, Windows, ...) and switcher
// facts (ProfileName, Active, HasCredentials, ExpiresAt, Email). There is no
// separate Snapshot or ProfileEntry — every fact has exactly one home.
// ProfileName is never empty ("local" when there is no switcher profile).
type Profile struct {
	ProfileName    string     `json:"profile_name"`
	Active         bool       `json:"active,omitempty"`
	HasCredentials bool       `json:"has_credentials"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	Email          string     `json:"email,omitempty"`
	Snapshot                  // embedded — usage fields promoted and JSON-flattened
}

type UsageProvider interface {
	Key() string
	Fetch(ctx context.Context, store *Store) (Snapshot, error)
}

// EmailFetcher is an optional interface for providers whose email isn't
// available in the usage fetch (e.g., Copilot, which needs a separate GitHub
// /user call). The poller calls FetchEmail on a separate cadence
// (max(providerInterval, 1h)) and caches the result in CacheEntry.EmailFetchedAt.
type EmailFetcher interface {
	FetchEmail(ctx context.Context, store *Store) (string, error)
}

// EmailScopeProvider identifies the account namespace used by EmailFetcher.
// A changed scope invalidates a cached identity even when its refresh floor has
// not elapsed.
type EmailScopeProvider interface {
	EmailScope(store *Store) string
}

type RawDecoder[T any] interface {
	Decode([]byte) (T, *ErrorDetail, error)
}

type Normalizer[T any] interface {
	Normalize(T) (Snapshot, *ErrorDetail, error)
}

type Registry struct {
	providers map[string]UsageProvider
}

func NewRegistry(providers ...UsageProvider) *Registry {
	r := &Registry{providers: make(map[string]UsageProvider)}
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
	return NewRegistry(NewCodexProvider(), NewClaudeCodeProvider(), NewCopilotProvider(), NewMistralProvider(), NewOllamaCloudProvider())
}

func (r *Registry) Provider(key string) (UsageProvider, bool) {
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

func ollamaProviderAuthMode(key string, pc ProviderConfig) string {
	if key != ProviderOllamaCloud {
		return ""
	}
	if strings.TrimSpace(pc.AuthMode) == "" {
		return ""
	}
	return normalizeOllamaAuthMode(pc.AuthMode)
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
