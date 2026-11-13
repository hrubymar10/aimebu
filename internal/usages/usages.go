// Package usages reads local harness credentials and publishes usage snapshots.
// The Reader never refreshes tokens or writes to credential paths; the only
// refresher is TargetRefresher (target_refresher.go), which the server runs on a
// migrated install.
package usages

import (
	"context"
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/usages/providers"
)

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
type Window struct {
	Key                   string     `json:"key"`
	PercentUsed           float64    `json:"percent_used"`
	ResetAt               *time.Time `json:"reset_at,omitempty"`
	WindowDurationSeconds int64      `json:"window_duration_seconds,omitempty"`
	Pace                  *Pace      `json:"pace,omitempty"`
	// History is attached to API output only (see history.go); it is not part
	// of the persisted snapshot.
	History []HistoryPoint `json:"history,omitempty"`
}
type Profile struct {
	ProfileName     string                 `json:"profile_name"`
	AlertIdentity   string                 `json:"-"`
	AlertCredential string                 `json:"-"`
	Active          bool                   `json:"active"`
	HasCredentials  bool                   `json:"has_credentials"`
	Email           string                 `json:"email,omitempty"`
	ExpiresAt       *time.Time             `json:"expires_at,omitempty"`
	Status          string                 `json:"status"`
	Plan            string                 `json:"plan,omitempty"`
	Windows         []Window               `json:"windows,omitempty"`
	Credits         *Credits               `json:"credits,omitempty"`
	LastRefreshAt   *time.Time             `json:"last_refresh_at,omitempty"`
	Stale           bool                   `json:"stale,omitempty"`
	Error           string                 `json:"error,omitempty"`
	ErrorDetail     *providers.ErrorDetail `json:"error_detail,omitempty"`
}
type Provider struct {
	ProviderName     string    `json:"provider_name"`
	Label            string    `json:"label"`
	OrderNumber      int       `json:"order_number"`
	Enabled          bool      `json:"enabled"`
	Available        bool      `json:"available"`
	EnterpriseHost   string    `json:"enterprise_host,omitempty"`
	TokenConfigured  bool      `json:"token_configured,omitempty"`
	CookieConfigured bool      `json:"cookie_configured,omitempty"`
	APIKeyConfigured bool      `json:"api_key_configured,omitempty"`
	AuthMode         string    `json:"auth_mode,omitempty"`
	Profiles         []Profile `json:"profiles"`
}
type Response struct {
	Providers []Provider `json:"providers"`
	Settings  struct {
		PercentDisplay         string `json:"percent_display"`
		RefreshIntervalSec     int    `json:"refresh_interval_sec"`
		AlertWarningRemaining  int    `json:"alert_warning_remaining"`
		AlertCriticalRemaining int    `json:"alert_critical_remaining"`
		// TokenAutoRefresh is the "Auto-refresh tokens" setting; the claude_ key
		// is its read-only legacy alias.
		TokenAutoRefresh          bool                                 `json:"token_auto_refresh"`
		ClaudeAutoRefresh         bool                                 `json:"claude_auto_refresh"`
		TokenAutoRefreshAvailable bool                                 `json:"token_auto_refresh_available"`
		TokenAutoRefreshState     map[string]TokenAutoRefreshToolState `json:"token_auto_refresh_state"`
		WarmupMode                string                               `json:"warmup_mode"`
		WarmupSchedule            string                               `json:"warmup_schedule"`
		WarmupAvailable           bool                                 `json:"warmup_available"`
	} `json:"settings"`
	SwitcherEnabled bool `json:"switcher_enabled"`
	// CredentialAlerts are the credentials of the token-copy model that need a
	// human (a rejected refresh token, one about to expire); empty unless the
	// refresher is on.
	CredentialAlerts []CredentialAlert `json:"credential_alerts,omitempty"`
}

// Reader keeps only normalized usage data. The cache never contains tokens.
type Reader struct {
	mu                 sync.RWMutex
	cachePath          string
	claudePath         string
	codexPath          string
	claudeURL          string
	codexURL           string
	client             *http.Client
	response           Response
	generation         uint64
	extra              *providers.Store
	extraOwners        map[string]string
	pendingExtra       map[string]bool
	providerGeneration map[string]uint64
	copilotEmailScope  string
	copilotEmailAt     time.Time
	copilotEmail       string
	onUpdate           func()
	tokenSetting       *TokenSetting
	credentialAlerts   func() []CredentialAlert
	forceMu            sync.Mutex
	lastForce          time.Time
	forceCooldown      time.Duration
	// profiles lists switcher profiles whose read-only usage is kept in
	// response.Providers[i].Profiles[1:] (index 0 is the live account);
	// activePrint is the fingerprint of the live credentials behind each
	// provider's last active snapshot.
	profiles    ProfileLister
	profileLock func(func() error) error
	activePrint map[string]string
	backoff     map[string]backoffState
	history     map[string][]HistoryPoint
	historyPath string
	now         func() time.Time
}

func New(cacheDir string) (*Reader, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	claudeDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if claudeDir == "" {
		claudeDir = filepath.Join(home, ".claude")
	}
	codexDir := os.Getenv("CODEX_HOME")
	if codexDir == "" {
		codexDir = filepath.Join(home, ".codex")
	}
	r := &Reader{
		cachePath:          filepath.Join(cacheDir, "cache.json"),
		claudePath:         filepath.Join(claudeDir, ".credentials.json"),
		codexPath:          filepath.Join(codexDir, "auth.json"),
		claudeURL:          "https://api.anthropic.com/api/oauth/usage",
		codexURL:           "https://chatgpt.com/backend-api/wham/usage",
		client:             providers.NewUsageHTTPClient(12 * time.Second),
		extra:              providers.NewStore(cacheDir),
		extraOwners:        map[string]string{},
		pendingExtra:       map[string]bool{},
		providerGeneration: map[string]uint64{},
		forceCooldown:      MinForceCooldown,
		activePrint:        map[string]string{},
		backoff:            map[string]backoffState{},
		history:            map[string][]HistoryPoint{},
		historyPath:        historyPathFor(cacheDir),
		now:                time.Now,
	}
	r.response = defaultResponse()
	r.loadHistory()
	if data, err := os.ReadFile(r.cachePath); err == nil {
		var saved Response
		if json.Unmarshal(data, &saved) == nil {
			for i := 0; i < 2; i++ {
				if i < len(saved.Providers) && len(saved.Providers[i].Profiles) > 0 {
					for j, p := range saved.Providers[i].Profiles {
						p.Stale = true
						if j == 0 {
							r.response.Providers[i].Profiles[0] = p
						} else {
							r.response.Providers[i].Profiles = append(r.response.Providers[i].Profiles, p)
						}
					}
				}
			}
		}
	}
	if cfg, err := r.extra.LoadConfig(); err == nil {
		decorateResponse(&r.response, cfg)
	}
	return r, nil
}

func defaultResponse() Response {
	r := Response{Providers: []Provider{
		{ProviderName: "codex", Label: "Codex", OrderNumber: 0, Enabled: true, Available: true, Profiles: []Profile{{ProfileName: "local", Active: true, Status: "not_configured"}}},
		{ProviderName: "claude-code", Label: "Claude Code", OrderNumber: 1, Enabled: true, Available: true, Profiles: []Profile{{ProfileName: "local", Active: true, Status: "not_configured"}}},
		{ProviderName: "github-copilot", Label: "GitHub Copilot", OrderNumber: 2, Available: true, Profiles: []Profile{{ProfileName: "local", Active: true, Status: "not_configured"}}},
		{ProviderName: "mistral", Label: "Mistral", OrderNumber: 3, Available: true, Profiles: []Profile{{ProfileName: "local", Active: true, Status: "not_configured"}}},
		{ProviderName: "ollama-cloud", Label: "Ollama Cloud", OrderNumber: 4, Available: true, Profiles: []Profile{{ProfileName: "local", Active: true, Status: "not_configured"}}},
	}}
	r.Settings.PercentDisplay = "left"
	r.Settings.RefreshIntervalSec = 120
	r.Settings.AlertWarningRemaining = 20
	r.Settings.AlertCriticalRemaining = 10
	return r
}

// Snapshot returns an independent value safe for concurrent JSON encoding.
func (r *Reader) Snapshot() Response {
	cfg, configErr := r.extra.LoadConfig()
	r.mu.Lock()
	if configErr == nil {
		for i := range r.response.Providers {
			if pc, present := cfg.Providers[r.response.Providers[i].ProviderName]; present {
				r.response.Providers[i].Enabled = pc.Enabled
			}
		}
	}
	out := r.response
	out.Providers = append([]Provider(nil), out.Providers...)
	for i := range out.Providers {
		out.Providers[i].Profiles = append([]Profile(nil), out.Providers[i].Profiles...)
		for j := range out.Providers[i].Profiles {
			p := &out.Providers[i].Profiles[j]
			p.Windows = append([]Window(nil), p.Windows...)
			r.attachHistory(out.Providers[i].ProviderName, p)
		}
	}
	setting := r.tokenSetting
	alerts := r.credentialAlerts
	r.mu.Unlock()
	if configErr == nil {
		decorateResponse(&out, cfg)
		out.Settings.TokenAutoRefresh = cfg.TokenAutoRefresh
		out.Settings.ClaudeAutoRefresh = cfg.TokenAutoRefresh
		out.Settings.AlertWarningRemaining = cfg.AlertWarningRemaining
		out.Settings.AlertCriticalRemaining = cfg.AlertCriticalRemaining
		out.Settings.WarmupMode = cfg.WarmupMode
		out.Settings.WarmupSchedule = cfg.WarmupSchedule
	} else {
		for i := range out.Providers {
			if !out.Providers[i].Enabled {
				out.Providers[i].Profiles = []Profile{{ProfileName: "local", Active: true, Status: "disabled", HasCredentials: out.Providers[i].Profiles[0].HasCredentials}}
			}
		}
	}
	if alerts != nil {
		out.CredentialAlerts = alerts()
	}
	out.Settings.TokenAutoRefreshState = map[string]TokenAutoRefreshToolState{"claude": {Status: "off"}, "codex": {Status: "off"}}
	if setting != nil {
		out.Settings.TokenAutoRefreshAvailable = setting.Available()
		out.Settings.TokenAutoRefreshState = setting.States(out.Settings.TokenAutoRefresh)
		if out.Settings.TokenAutoRefreshAvailable && out.Settings.TokenAutoRefresh {
			for _, state := range out.Settings.TokenAutoRefreshState {
				if state.Status == "migrated" {
					out.Settings.WarmupAvailable = true
				}
			}
		}
	}
	return out
}

// SetCredentialAlerts installs the source of the response's credential_alerts.
func (r *Reader) SetCredentialAlerts(fn func() []CredentialAlert) {
	r.mu.Lock()
	r.credentialAlerts = fn
	r.mu.Unlock()
}

// SetTokenSetting installs the lifecycle of the token auto-refresh setting; the
// snapshot then reports its availability and per-tool state.
func (r *Reader) SetTokenSetting(t *TokenSetting) {
	r.mu.Lock()
	r.tokenSetting = t
	r.mu.Unlock()
}

// SetHTTPClient replaces the client that the provider fetches go through. A test that wants refreshes but not the provider's real servers (a
// fixture's credentials are made up) installs one that answers locally; the production client, with its timeout and retries, is the default. Set it before the first refresh.
func (r *Reader) SetHTTPClient(client *http.Client) {
	r.mu.Lock()
	r.client = client
	r.mu.Unlock()
}

// Invalidate clears the one local snapshot after a credential swap. The next
// refresh must fetch limits with the newly active account.
func (r *Reader) Invalidate(provider string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.generation++
	_ = os.Remove(r.cachePath)
	for i := range r.response.Providers {
		if r.response.Providers[i].ProviderName == provider {
			r.response.Providers[i].Profiles = []Profile{{ProfileName: "local", Active: true, Status: "refreshing"}}
			// The live account changed: its past samples belong to another login.
			r.forgetHistoryLocked(provider, "local")
		}
	}
	r.signalLocked()
}

func (r *Reader) Run(ctx context.Context) {
	r.Refresh(ctx)
	knownProfiles, haveProfiles := r.profileNames()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	last := time.Now()
	lastConfigError := ""
	r.mu.RLock()
	lastGeneration := r.generation
	r.mu.RUnlock()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if current, ok := r.profileNames(); ok {
				if haveProfiles && !sameProfileNames(knownProfiles, current) {
					for key := range knownProfiles {
						if !current[key] {
							tool, name, _ := strings.Cut(key, "\x00")
							provider := tool
							if tool == "claude" {
								provider = providers.ProviderClaudeCode
							}
							r.ForgetProfile(provider, name)
						}
					}
					// A newly added or removed empty profile may have no cached usage
					// row, but Settings still needs a fresh switcher list.
					r.NotifyChanged()
				}
				knownProfiles, haveProfiles = current, true
			}
			cfg, err := r.extra.LoadConfig()
			if err != nil && err.Error() != lastConfigError {
				log.Printf("usages: config read failed, using defaults: %v", err)
				lastConfigError = err.Error()
			} else if err == nil {
				lastConfigError = ""
			}
			r.mu.RLock()
			generation := r.generation
			r.mu.RUnlock()
			if generation != lastGeneration || time.Since(last) >= time.Duration(cfg.RefreshIntervalSec)*time.Second {
				r.mu.Lock()
				r.pendingExtra = map[string]bool{}
				r.mu.Unlock()
				r.Refresh(ctx)
				last = time.Now()
				lastGeneration = generation
			} else {
				r.mu.Lock()
				pending := r.pendingExtra
				r.pendingExtra = map[string]bool{}
				r.mu.Unlock()
				r.pollLiveChanges(ctx)
				for _, key := range providers.KnownProviders() {
					if pending[key] {
						r.RefreshProvider(ctx, key)
					}
				}
			}
		}
	}
}

// Refresh reads credential files, then only sends GETs with unexpired access tokens.
func (r *Reader) Refresh(ctx context.Context) {
	for i := 0; i < 2; i++ {
		r.refreshLocalOne(ctx, i)
		r.refreshStored(ctx, i)
	}
	r.refreshExtra(ctx)
}

func (r *Reader) refreshLocalOne(ctx context.Context, i int) {
	key := []string{providers.ProviderCodex, providers.ProviderClaudeCode}[i]
	if cfg, err := r.extra.LoadConfig(); err == nil {
		if pc, configured := cfg.Providers[key]; configured && !pc.Enabled {
			return
		}
	} else {
		r.mu.RLock()
		enabled := r.response.Providers[i].Enabled
		r.mu.RUnlock()
		if !enabled {
			return
		}
	}
	r.mu.RLock()
	generation := r.generation
	providerGeneration := r.providerGeneration[key]
	r.mu.RUnlock()
	fetch := []func(context.Context) (Profile, error){r.fetchCodex, r.fetchClaude}[i]
	livePath := []string{r.codexPath, r.claudePath}[i]
	printBefore := fileFingerprint(livePath)
	p, err := fetch(ctx)
	// The harness may have rotated the file, or the owner switched accounts,
	// while the request was in flight: that result describes a login that is no
	// longer live, so drop it. The next poll tick sees the new fingerprint.
	printAfter := fileFingerprint(livePath)
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation != r.generation || providerGeneration != r.providerGeneration[key] || printBefore != printAfter {
		return
	}
	r.activePrint[key] = printAfter
	previous := r.response.Providers[i].Profiles[0]
	if err != nil {
		p.Windows = previous.Windows
		p.Plan = previous.Plan
		p.Credits = previous.Credits
		p.LastRefreshAt = previous.LastRefreshAt
		p.Stale = len(previous.Windows) > 0
	}
	r.response.Providers[i].Profiles[0] = p
	r.recordHistoryLocked(key, p)
	if data, marshalErr := json.Marshal(r.response); marshalErr == nil {
		_ = writeCache(r.cachePath, data)
	}
	r.signalLocked()
}

func writeCache(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".usage-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func (r *Reader) fetchCodex(ctx context.Context) (Profile, error) {
	base := Profile{ProfileName: "local", Active: true, Status: "auth_missing"}
	login, err := providers.ReadCodexLogin(r.codexPath)
	if err != nil {
		return failedLogin(base, "Codex credentials unavailable", err)
	}
	provider := providers.NewCodexProviderWith(providers.CodexOptions{AuthPath: r.codexPath, URL: r.codexURL, Client: r.client})
	return r.fetchLocal(ctx, base, login, "Codex", provider, func() (providers.Login, error) { return providers.ReadCodexLogin(r.codexPath) })
}

func (r *Reader) fetchClaude(ctx context.Context) (Profile, error) {
	base := Profile{ProfileName: "local", Active: true, Status: "auth_missing"}
	login, err := providers.ReadClaudeLogin(r.claudePath)
	if err != nil {
		return failedLogin(base, "Claude credentials unavailable", err)
	}
	provider := providers.NewClaudeCodeProviderWith(providers.ClaudeCodeOptions{AuthPath: r.claudePath, URL: r.claudeURL, Client: r.client})
	return r.fetchLocal(ctx, base, login, "Claude", provider, func() (providers.Login, error) { return providers.ReadClaudeLogin(r.claudePath) })
}

// failedLogin reports a login file that is missing or unusable. The loader's
// error text never contains credential content, so it is shown as is.
func failedLogin(base Profile, fallback string, err error) (Profile, error) {
	base.Error = fallback
	if msg := strings.TrimSpace(err.Error()); msg != "" {
		base.Error = msg
	}
	return base, err
}

// fetchLocal sends one usage GET with the live access token. A token that has
// already expired is never sent: the previous snapshot stays, marked stale,
// until the harness renews its own login.
func (r *Reader) fetchLocal(ctx context.Context, p Profile, login providers.Login, name string, provider providers.UsageProvider, reread func() (providers.Login, error)) (Profile, error) {
	p.HasCredentials = true
	// Rotating access tokens must not rearm an alert in the same account and
	// cycle. A missing durable account claim leaves this fingerprint empty.
	if login.StableAccountID != "" {
		p.AlertCredential = alertHash("account\x00" + login.StableAccountID)
	}
	if login.ExpiresAt != nil {
		p.ExpiresAt = login.ExpiresAt
		if !login.ExpiresAt.After(time.Now()) {
			p.Status = "token_expired"
			p.Error = "Stale – token expired"
			return p, errors.New(p.Error)
		}
	}
	snap, fetchErr := provider.Fetch(ctx, nil)
	var wrapped *providers.SnapshotError
	if errors.As(fetchErr, &wrapped) {
		snap = wrapped.Snapshot
	} else if fetchErr != nil {
		snap = providers.Snapshot{Status: providers.StatusFetchError, Error: name + " usage request failed."}
	}
	// The provider may have retried with a token the harness rotated into the
	// file meanwhile (Codex re-reads after a 401); redact that one too.
	secrets := []string{login.AccessToken}
	if current, err := reread(); err == nil && current.AccessToken != "" {
		secrets = append(secrets, current.AccessToken)
	}
	snap = snap.Redacted(secrets...)
	p = applySnapshot(p, snap)
	switch snap.Status {
	case providers.StatusOK:
		now := time.Now().UTC()
		p.LastRefreshAt = &now
		return p, nil
	case providers.StatusFetchError, providers.StatusTimeout:
		// Transient: the caller keeps the previous windows, marked stale.
		return p, errors.New(p.Error)
	}
	// Rejected or unusable login (auth_missing, scope_missing): show the
	// status rather than a stale snapshot of a different state.
	return p, nil
}

func jwtExpiry(token string) *time.Time {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(data, &claims) != nil || claims.Exp <= 0 {
		return nil
	}
	t := time.Unix(claims.Exp, 0).UTC()
	return &t
}

func first(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
