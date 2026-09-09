package usages

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var ErrForceCooldown = errors.New("force refresh cooldown active")

// emailRefreshFloor is the minimum interval for email refresh (Copilot's /user
// call and captured profile addresses, via EmailFetchedAt). Usage snapshots
// for all profiles — active and inactive — refresh on the configured provider
// interval. Email is the only thing on a separate, slower cadence.
const emailRefreshFloor = 1 * time.Hour

type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type Manager struct {
	store    *Store
	registry *Registry
	clock    Clock

	forceMu       sync.Mutex
	lastForce     map[string]time.Time
	forceCooldown time.Duration

	onUpdate        func(UsagesResponse)
	profileLister   ProfileLister
	switcherEnabled SwitcherEnabledProvider
}

func NewManager(store *Store, registry *Registry) *Manager {
	if store == nil {
		store = NewStore()
	}
	if registry == nil {
		registry = EmptyRegistry()
	}
	return &Manager{
		store:         store,
		registry:      registry,
		clock:         realClock{},
		lastForce:     map[string]time.Time{},
		forceCooldown: MinRefreshSec * time.Second,
	}
}

func (m *Manager) SetClock(clock Clock) {
	if clock != nil {
		m.clock = clock
	}
}

func (m *Manager) SetUpdateHook(fn func(UsagesResponse)) {
	m.onUpdate = fn
}

// ProfileInfo carries a switcher profile's identity and credential path.
// The usages package uses this to fetch per-profile snapshots without importing
// the switcher package — the server wires ProfileLister to the switcher.
type ProfileInfo struct {
	Tool           string     // switcher tool name ("claude", "codex")
	Name           string     // profile name
	Active         bool       // whether this is the currently-active profile
	CredPath       string     // credential file path for this profile
	Email          string     // captured account email from profile metadata
	HasCredentials bool       // whether stored credentials exist for this profile
	ExpiresAt      *time.Time // token expiry from the credential file
}

// ProfileLister returns switcher profiles for a given tool. nil/empty means
// no switcher is enabled or the tool has no profiles.
type ProfileLister func(tool string) []ProfileInfo

func (m *Manager) SetProfileLister(fn ProfileLister) {
	m.profileLister = fn
}

// SwitcherEnabledProvider returns whether the switcher is currently enabled.
type SwitcherEnabledProvider func() bool

func (m *Manager) SetSwitcherSettingsProvider(fn SwitcherEnabledProvider) {
	m.switcherEnabled = fn
}

func (m *Manager) Settings(ctx context.Context) (Settings, error) {
	var info Settings
	err := m.store.WithLock(func() error {
		cfg, err := m.store.LoadConfig()
		if err != nil {
			return err
		}
		_, info = m.store.RefreshInterval(cfg)
		return nil
	})
	return info, err
}

// Snapshot is a pure cache read — it never fetches. All network work is owned
// by the poller (Manager.Start) and ForceRefresh. On a cold cache (server
// restart, first call before the first poller tick) it returns empty arrays so
// the FE can distinguish "cold" from "broken".
// assembleResponse builds a UsagesResponse from config and cache. Every
// known provider appears (even if disabled), with its cached profiles (or an
// empty profiles slice — never nil).
func (m *Manager) assembleResponse(cfg Config, cache Cache) UsagesResponse {
	_, settings := m.store.RefreshInterval(cfg)
	resp := UsagesResponse{Settings: settings}
	if m.switcherEnabled != nil {
		resp.SwitcherEnabled = m.switcherEnabled()
	}
	for i, key := range normalizeProviderOrder(cfg.ProviderOrder) {
		pc := cfg.Providers[key]
		profiles := []Profile{}
		for _, e := range cache.Snapshots[key] {
			profiles = append(profiles, e.Profile)
		}
		resp.Providers = append(resp.Providers, Provider{
			ProviderName: key,
			Label:        ProviderLabel(key),
			OrderNumber:  i,
			Enabled:      pc.Enabled,
			Available:    m.registry.HasProvider(key),
			Profiles:     profiles,
		})
	}
	return resp
}

// profileFromSnapshot converts an internal Snapshot (provider return type) to
// a Profile (cache + response type), setting the ProfileName.
func profileFromSnapshot(snap Snapshot, profileName string) Profile {
	return Profile{
		ProfileName: profileName,
		Snapshot:    snap,
	}
}

func (m *Manager) Snapshot(ctx context.Context, provider string) (UsagesResponse, error) {
	if provider != "" && !KnownProvider(provider) {
		return UsagesResponse{}, unknownProviderError(provider)
	}
	var resp UsagesResponse
	err := m.store.WithLock(func() error {
		cfg, err := m.store.LoadConfig()
		if err != nil {
			return err
		}
		cache, err := m.store.LoadCache()
		if err != nil {
			return err
		}
		resp = m.assembleResponse(cfg, cache)
		return nil
	})
	return resp, err
}

func (m *Manager) ForceRefresh(ctx context.Context, provider string) (UsagesResponse, int, error) {
	if provider != "" && !KnownProvider(provider) {
		return UsagesResponse{}, 0, unknownProviderError(provider)
	}
	retry := m.checkForceCooldown(provider)
	if retry > 0 {
		return UsagesResponse{}, retry, ErrForceCooldown
	}
	resp, changed, retry, err := m.refreshWithChange(ctx, provider, true)
	if err == nil && changed && m.onUpdate != nil {
		m.onUpdate(resp)
	}
	return resp, retry, err
}

func (m *Manager) UpdateSettings(ctx context.Context, intervalSec int, percentDisplay string, providerOrder []string, updateProviderOrder bool) (Settings, error) {
	if intervalSec != 0 && intervalSec < MinRefreshSec {
		return Settings{}, errors.New("refresh_interval_sec is below minimum")
	}
	if percentDisplay != "" && !validPercentDisplay(percentDisplay) {
		return Settings{}, errors.New("percent_display must be left or used")
	}
	var info Settings
	err := m.store.WithLock(func() error {
		cfg, err := m.store.LoadConfig()
		if err != nil {
			return err
		}
		if intervalSec != 0 {
			cfg.RefreshIntervalSec = intervalSec
		}
		if percentDisplay != "" {
			cfg.PercentDisplay = percentDisplay
		}
		if updateProviderOrder {
			cfg.ProviderOrder = normalizeProviderOrder(providerOrder)
		}
		if err := m.store.SaveConfig(cfg); err != nil {
			return err
		}
		_, info = m.store.RefreshInterval(cfg)
		return nil
	})
	return info, err
}

func (m *Manager) SetProviderEnabled(ctx context.Context, provider string, enabled bool) (Config, error) {
	if !KnownProvider(provider) {
		return Config{}, unknownProviderError(provider)
	}
	var cfg Config
	err := m.store.WithLock(func() error {
		var err error
		cfg, err = m.store.LoadConfig()
		if err != nil {
			return err
		}
		pc := cfg.Providers[provider]
		pc.Enabled = enabled
		cfg.Providers[provider] = pc
		return m.store.SaveConfig(cfg)
	})
	return cfg, err
}

func (m *Manager) SetOllamaCookie(ctx context.Context, cookie string) (Config, error) {
	return m.SetOllamaConfig(ctx, "", nil, &cookie)
}

func (m *Manager) SetOllamaConfig(ctx context.Context, authMode string, apiKey *string, cookie *string) (Config, error) {
	var cfg Config
	err := m.store.WithLock(func() error {
		var err error
		cfg, err = m.store.LoadConfig()
		if err != nil {
			return err
		}
		pc := cfg.Providers[ProviderOllamaCloud]
		if authMode != "" {
			pc.AuthMode = normalizeOllamaAuthMode(authMode)
		} else {
			pc.AuthMode = normalizeOllamaAuthMode(pc.AuthMode)
		}
		if apiKey != nil {
			pc.APIKey = normalizeOllamaAPIKey(*apiKey)
		}
		if cookie != nil {
			rawCookie := strings.TrimSpace(*cookie)
			if rawCookie == "" {
				pc.Cookie = ""
			} else {
				normalized, detail, err := normalizeOllamaCookieHeader(rawCookie)
				if err != nil {
					if detail != nil {
						return &SnapshotError{Snapshot: Snapshot{Status: StatusAuthMissing, Error: err.Error(), ErrorDetail: detail}, Err: err}
					}
					return err
				}
				pc.Cookie = normalized
			}
		}
		if pc.AuthMode == "" {
			pc.AuthMode = OllamaAuthAuto
		}
		switch pc.AuthMode {
		case OllamaAuthCookie:
			pc.Enabled = pc.Cookie != ""
		case OllamaAuthAPIKey:
			pc.Enabled = pc.APIKey != ""
		default:
			pc.Enabled = pc.Cookie != "" || pc.APIKey != ""
		}
		cfg.Providers[ProviderOllamaCloud] = pc
		if err := m.store.SaveConfig(cfg); err != nil {
			return err
		}
		cache, err := m.store.LoadCache()
		if err != nil {
			return err
		}
		delete(cache.Snapshots, ProviderOllamaCloud)
		return m.store.SaveCache(cache)
	})
	return cfg, err
}

func (m *Manager) SetMistralConfig(ctx context.Context, cookie *string) (Config, error) {
	var cfg Config
	err := m.store.WithLock(func() error {
		var err error
		cfg, err = m.store.LoadConfig()
		if err != nil {
			return err
		}
		pc := cfg.Providers[ProviderMistral]
		if cookie != nil {
			rawCookie := strings.TrimSpace(*cookie)
			if rawCookie == "" {
				pc.Cookie = ""
			} else {
				normalized, detail, err := normalizeMistralCookieHeader(rawCookie)
				if err != nil {
					if detail != nil {
						return &SnapshotError{Snapshot: Snapshot{Status: StatusAuthMissing, Error: err.Error(), ErrorDetail: detail}, Err: err}
					}
					return err
				}
				pc.Cookie = normalized
			}
		}
		pc.Enabled = pc.Cookie != ""
		cfg.Providers[ProviderMistral] = pc
		if err := m.store.SaveConfig(cfg); err != nil {
			return err
		}
		cache, err := m.store.LoadCache()
		if err != nil {
			return err
		}
		delete(cache.Snapshots, ProviderMistral)
		return m.store.SaveCache(cache)
	})
	return cfg, err
}

// Start launches the background refresh goroutine. If wg is non-nil, the
// goroutine registers with it (Add/Done) so callers can wait for it to exit
// after cancelling ctx.
func (m *Manager) Start(ctx context.Context, wg *sync.WaitGroup) {
	ticker := time.NewTicker(5 * time.Second)
	if wg != nil {
		wg.Add(1)
	}
	go func() {
		if wg != nil {
			defer wg.Done()
		}
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				resp, changed, _, err := m.refreshWithChange(ctx, "", false)
				if err != nil {
					continue
				}
				// Refresh per-profile snapshots (switcher). The active profile is
				// derived from the provider fetch (same credentials, no separate
				// network call); inactive profiles are fetched if stale.
				if m.profileLister != nil {
					if pChanged, pErr := m.refreshProfiles(ctx); pErr == nil && pChanged {
						changed = true
					}
					// Rebuild from cache to include profile entries that
					// refreshWithChange didn't see.
					resp, _ = m.Snapshot(ctx, "")
				}
				if m.switcherEnabled != nil {
					resp.SwitcherEnabled = m.switcherEnabled()
				}
				if changed && m.onUpdate != nil && len(resp.Providers) > 0 {
					m.onUpdate(resp)
				}
			}
		}
	}()
}

func (m *Manager) refresh(ctx context.Context, provider string, force bool) (UsagesResponse, int, error) {
	resp, _, retry, err := m.refreshWithChange(ctx, provider, force)
	return resp, retry, err
}

func (m *Manager) refreshWithChange(ctx context.Context, provider string, force bool) (UsagesResponse, bool, int, error) {
	var resp UsagesResponse
	var toFetch []string
	err := m.store.WithLock(func() error {
		cfg, err := m.store.LoadConfig()
		if err != nil {
			return err
		}
		cache, err := m.store.LoadCache()
		if err != nil {
			return err
		}
		interval, _ := m.store.RefreshInterval(cfg)
		resp = m.assembleResponse(cfg, cache)
		keys := knownProviders
		if provider != "" {
			keys = []string{provider}
		}
		now := m.clock.Now()
		for _, key := range keys {
			pc := cfg.Providers[key]
			if !pc.Enabled {
				continue
			}
			entries := cache.Snapshots[key]
			def := activeCacheEntry(entries)
			shouldFetch := force || def == nil || def.Profile.LastRefreshAt == nil || now.Sub(*def.Profile.LastRefreshAt) >= interval
			if shouldFetch {
				toFetch = append(toFetch, key)
			}
		}
		return nil
	})
	if err != nil {
		return UsagesResponse{}, false, 0, err
	}
	if len(toFetch) == 0 {
		return resp, false, 0, nil
	}

	results := make(chan fetchResult, len(toFetch))
	var wg sync.WaitGroup
	for _, key := range toFetch {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			results <- m.fetchWithProviderLock(ctx, key, force)
		}(key)
	}
	wg.Wait()
	close(results)

	changed := false
	successes := 0
	var firstErr error
	for result := range results {
		if result.err != nil {
			if firstErr == nil {
				firstErr = result.err
			}
			continue
		}
		successes++
		changed = changed || result.changed
	}
	if firstErr != nil && (provider != "" || successes == 0) {
		return UsagesResponse{}, false, 0, firstErr
	}

	err = m.store.WithLock(func() error {
		cfg, err := m.store.LoadConfig()
		if err != nil {
			return err
		}
		cache, err := m.store.LoadCache()
		if err != nil {
			return err
		}
		resp = m.assembleResponse(cfg, cache)
		keys := knownProviders
		if provider != "" {
			keys = []string{provider}
		}
		for _, key := range keys {
			if !cfg.Providers[key].Enabled {
				continue
			}
		}
		return nil
	})
	if err != nil {
		return UsagesResponse{}, false, 0, err
	}
	return resp, changed, 0, nil
}

// defaultCacheEntry returns the "default" (non-profile) entry for a provider,
// or nil if the array is empty. The default entry has Profile == "" or
// "default"; falling back to the first entry handles caches written before
// profile support.
func defaultCacheEntry(entries []CacheEntry) *CacheEntry {
	for i := range entries {
		if entries[i].Profile.ProfileName == "" || entries[i].Profile.ProfileName == "default" {
			return &entries[i]
		}
	}
	return nil
}

// latestCacheEntry returns the entry with the most recent LastRefreshAt, or nil
// if the array is empty or no entry has a timestamp. Used for staleness checks
// on switcher tools where the provider fetch writes directly under the active
// profile's name — defaultCacheEntry returns nil for those, and falling back to
// entries[0] would read the wrong entry's timestamp (e.g. an inactive profile
// fetched within the last hour, blocking the provider fetch indefinitely).
func latestCacheEntry(entries []CacheEntry) *CacheEntry {
	var best *CacheEntry
	for i := range entries {
		if entries[i].Profile.LastRefreshAt == nil {
			continue
		}
		if best == nil || entries[i].Profile.LastRefreshAt.After(*best.Profile.LastRefreshAt) {
			best = &entries[i]
		}
	}
	return best
}

// activeCacheEntry returns the entry whose staleness gates the provider usage
// fetch. That fetch writes the active profile's entry, so the active profile's
// freshness — not the freshest sibling's — must decide whether to re-fetch.
// Otherwise an inactive profile that refreshProfiles keeps fresh marks the
// whole provider fresh and starves the active profile's usage numbers, which
// then only update on a manual (force) refresh. Falls back to the default
// entry, then to the most recently refreshed entry.
func activeCacheEntry(entries []CacheEntry) *CacheEntry {
	for i := range entries {
		if entries[i].Profile.Active {
			return &entries[i]
		}
	}
	if def := defaultCacheEntry(entries); def != nil {
		return def
	}
	return latestCacheEntry(entries)
}

// setDefaultCacheEntry replaces the default entry in entries, or prepends one
// if no default exists.
func setDefaultCacheEntry(entries []CacheEntry, entry CacheEntry) []CacheEntry {
	for i := range entries {
		if entries[i].Profile.ProfileName == "" || entries[i].Profile.ProfileName == "default" {
			entries[i] = entry
			return entries
		}
	}
	return append([]CacheEntry{entry}, entries...)
}

// setProfileCacheEntry replaces the entry for a named profile, or appends one.
func setProfileCacheEntry(entries []CacheEntry, profile string, entry CacheEntry) []CacheEntry {
	for i := range entries {
		if entries[i].Profile.ProfileName == profile {
			entries[i] = entry
			return entries
		}
	}
	return append(entries, entry)
}

type fetchResult struct {
	changed bool
	err     error
}

func (m *Manager) fetchWithProviderLock(ctx context.Context, key string, force bool) fetchResult {
	lock, err := acquireProviderLock(m.store.root, key)
	if err != nil {
		return fetchResult{err: err}
	}
	defer lock.Unlock()

	var previous CacheEntry
	var interval time.Duration
	var now time.Time
	shouldFetch := false
	err = m.store.WithLock(func() error {
		cfg, err := m.store.LoadConfig()
		if err != nil {
			return err
		}
		if !cfg.Providers[key].Enabled {
			return nil
		}
		cache, err := m.store.LoadCache()
		if err != nil {
			return err
		}
		interval, _ = m.store.RefreshInterval(cfg)
		now = m.clock.Now()
		def := activeCacheEntry(cache.Snapshots[key])
		if def != nil {
			previous = *def
		}
		shouldFetch = force || def == nil || def.Profile.LastRefreshAt == nil || now.Sub(*def.Profile.LastRefreshAt) >= interval
		return nil
	})
	if err != nil || !shouldFetch {
		return fetchResult{err: err}
	}

	entry := m.fetchOne(ctx, key, previous)
	entry.Profile.ProfileName = "default"

	// If this provider has a switcher, write the active profile's name and
	// switcher facts now, under the same lock that saves the entry. This
	// prevents a "default" entry from being observable between the provider
	// fetch and the separate deriveActiveProfile call.
	if m.profileLister != nil {
		tool := toolForRegistryKey(key)
		if tool != "" {
			for _, pi := range m.profileLister(tool) {
				if pi.Active {
					entry.Profile.ProfileName = pi.Name
					entry.Profile.Active = pi.Active
					entry.Profile.HasCredentials = pi.HasCredentials
					entry.Profile.ExpiresAt = pi.ExpiresAt
					if pi.Email != "" {
						entry.Profile.Email = pi.Email
					}
					break
				}
			}
		}
	}

	// Email refresh for providers whose email isn't in the usage fetch (Copilot,
	// via a separate GitHub /user call). Same floor as inactive profiles:
	// max(providerInterval, 1h), absent means fetch now. The email persists
	// across usage refreshes — only re-fetched when EmailFetchedAt is stale.
	if p, ok := m.registry.Provider(key); ok {
		if ef, ok := p.(EmailFetcher); ok {
			emailFloor := interval
			if emailFloor < emailRefreshFloor {
				emailFloor = emailRefreshFloor
			}
			if previous.EmailFetchedAt == nil || now.Sub(*previous.EmailFetchedAt) >= emailFloor {
				if email, eerr := ef.FetchEmail(ctx, m.store); eerr == nil && email != "" {
					entry.Profile.Email = email
					emailNow := m.clock.Now()
					entry.EmailFetchedAt = &emailNow
				}
			} else {
				entry.Profile.Email = previous.Profile.Email
				entry.EmailFetchedAt = previous.EmailFetchedAt
			}
		}
	}

	err = m.store.WithLock(func() error {
		cfg, err := m.store.LoadConfig()
		if err != nil {
			return err
		}
		if !cfg.Providers[key].Enabled {
			return nil
		}
		cache, err := m.store.LoadCache()
		if err != nil {
			return err
		}
		entry.Profile = entry.Profile.Redacted(configSecrets(cfg)...)
		// Use setProfileCacheEntry when the entry has a real profile name (switcher
		// active profile), setDefaultCacheEntry for the non-switcher "default" case.
		// setDefaultCacheEntry only matches ""/"default" — using it for a named
		// profile would prepend a duplicate on the second cycle.
		if entry.Profile.ProfileName != "" && entry.Profile.ProfileName != "default" {
			cache.Snapshots[key] = setProfileCacheEntry(cache.Snapshots[key], entry.Profile.ProfileName, entry)
		} else {
			cache.Snapshots[key] = setDefaultCacheEntry(cache.Snapshots[key], entry)
		}
		return m.store.SaveCache(cache)
	})
	if err != nil {
		return fetchResult{err: err}
	}
	return fetchResult{changed: true}
}

func (m *Manager) fetchOne(ctx context.Context, key string, previous CacheEntry) CacheEntry {
	now := m.clock.Now()
	p, ok := m.registry.Provider(key)
	if !ok {
		return CacheEntry{Profile: Profile{ProfileName: "default", Snapshot: Snapshot{Status: StatusNotConfigured}}}
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	snap, err := p.Fetch(fetchCtx, m.store)
	if err == nil {
		if snap.Status == "" {
			snap.Status = StatusOK
		}
		snap.LastRefreshAt = &now
		return CacheEntry{Profile: profileFromSnapshot(snap, "default")}
	}
	status := StatusFetchError
	errSnapshot := Snapshot{Status: status, Error: err.Error()}
	if snapErr, ok := err.(*SnapshotError); ok {
		errSnapshot = snapErr.Snapshot
		if errSnapshot.Status == "" {
			errSnapshot.Status = status
		}
		if errSnapshot.Error == "" {
			errSnapshot.Error = snapErr.Error()
		}
	}
	if errors.Is(fetchCtx.Err(), context.DeadlineExceeded) {
		status = StatusTimeout
		errSnapshot.Status = status
	}
	if shouldPreservePreviousSnapshot(err, errSnapshot, previous) {
		prof := previous.Profile
		prof.Status = StatusStaleCache
		prof.Stale = true
		prof.Error = errSnapshot.Error
		prof.ErrorDetail = errSnapshot.ErrorDetail
		prof.LastRefreshAt = previous.Profile.LastRefreshAt
		return CacheEntry{Profile: prof}
	}
	errSnapshot.LastRefreshAt = &now
	return CacheEntry{Profile: profileFromSnapshot(errSnapshot, "default")}
}

func shouldPreservePreviousSnapshot(err error, errSnapshot Snapshot, previous CacheEntry) bool {
	if previous.Profile.ProfileName == "" {
		return false
	}
	if previous.Profile.Status != StatusOK && previous.Profile.Status != StatusStaleCache {
		return false
	}
	if errSnapshot.Status == StatusTimeout {
		return true
	}
	if errSnapshot.Status != StatusFetchError {
		return false
	}
	return isTransientTransportError(err) || hasTransientHTTPStatus(errSnapshot.ErrorDetail)
}

func isTransientTransportError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	for _, target := range []error{
		syscall.ECONNABORTED,
		syscall.ECONNREFUSED,
		syscall.ECONNRESET,
		syscall.EHOSTDOWN,
		syscall.EHOSTUNREACH,
		syscall.ENETDOWN,
		syscall.ENETUNREACH,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func hasTransientHTTPStatus(detail *ErrorDetail) bool {
	if detail == nil {
		return false
	}
	for _, value := range detail.Fields {
		codeText, ok := strings.CutPrefix(value, "http_")
		if !ok {
			continue
		}
		code, err := strconv.Atoi(codeText)
		if err == nil && code >= 500 && code <= 599 {
			return true
		}
	}
	return false
}

// FetchProfileSnapshot returns the cached usage snapshot for a credential
// profile, fetching fresh data when the cache is stale. Pass the live
// credential file path for the active profile, the stored path for others.
// Always uses persist=false so reading a stored profile never rotates its
// tokens; the main provider's background poll refreshes the active profile.
// registryKeyForTool maps a switcher tool name ("claude", "codex") to the
// usages provider registry key ("claude-code", "codex"). Codex is the same in
// both; claude differs because the registry uses the harness name and the
// switcher uses the tool name.
func registryKeyForTool(tool string) string {
	switch tool {
	case "claude":
		return ProviderClaudeCode
	default:
		return tool
	}
}

func (m *Manager) FetchProfileSnapshot(ctx context.Context, tool, name, credPath string) (Profile, error) {
	regKey := registryKeyForTool(tool)
	var cached *CacheEntry
	var interval time.Duration
	_ = m.store.WithLock(func() error {
		cfg, err := m.store.LoadConfig()
		if err != nil {
			return err
		}
		cache, err := m.store.LoadCache()
		if err != nil {
			return err
		}
		interval, _ = m.store.RefreshInterval(cfg)
		// All profiles — active and inactive — refresh on the configured provider
		// interval. No separate floor; email is the only thing on a slower cadence.
		now := m.clock.Now()
		for i := range cache.Snapshots[regKey] {
			if cache.Snapshots[regKey][i].Profile.ProfileName == name {
				if cache.Snapshots[regKey][i].Profile.LastRefreshAt != nil && now.Sub(*cache.Snapshots[regKey][i].Profile.LastRefreshAt) < interval {
					cp := cache.Snapshots[regKey][i]
					cached = &cp
				}
				break
			}
		}
		return nil
	})
	if cached != nil {
		return cached.Profile, nil
	}
	_ = interval

	var snap Snapshot
	var fetchErr error
	switch tool {
	case "claude":
		snap, fetchErr = fetchClaudeSnapshotFromPathFunc(ctx, credPath)
	case "codex":
		snap, fetchErr = fetchCodexSnapshotFromPathFunc(ctx, credPath, false, nil)
	default:
		return Profile{}, fmt.Errorf("unsupported switcher tool %q for profile snapshot", tool)
	}
	if fetchErr != nil {
		// Write a placeholder entry with the profile name so setProfileFields
		// can set identity fields on it. Identity is not conditional on a
		// successful fetch — a profile whose fetch errors must still carry
		// profile_name, has_credentials, etc.
		now := m.clock.Now()
		errSnap := Snapshot{Status: StatusFetchError, Error: fetchErr.Error()}
		if snapErr, ok := fetchErr.(*SnapshotError); ok {
			errSnap = snapErr.Snapshot
			if errSnap.Status == "" {
				errSnap.Status = StatusFetchError
			}
			if errSnap.Error == "" {
				errSnap.Error = snapErr.Error()
			}
		}
		errSnap.LastRefreshAt = &now
		_ = m.store.WithLock(func() error {
			cache, err := m.store.LoadCache()
			if err != nil {
				return err
			}
			entry := CacheEntry{Profile: profileFromSnapshot(errSnap, name)}
			cache.Snapshots[regKey] = setProfileCacheEntry(cache.Snapshots[regKey], name, entry)
			return m.store.SaveCache(cache)
		})
		return profileFromSnapshot(errSnap, name), fetchErr
	}
	now := m.clock.Now()
	snap.LastRefreshAt = &now
	_ = m.store.WithLock(func() error {
		cache, err := m.store.LoadCache()
		if err != nil {
			return err
		}
		entry := CacheEntry{Profile: profileFromSnapshot(snap, name)}
		cache.Snapshots[regKey] = setProfileCacheEntry(cache.Snapshots[regKey], name, entry)
		return m.store.SaveCache(cache)
	})
	return profileFromSnapshot(snap, name), nil
}

// InvalidateProfileSnapshot removes a profile's entry from the cache array,
// forcing a fresh fetch on the next poller tick. Called after a switch to
// ensure the two affected profiles refresh immediately.
func (m *Manager) InvalidateProfileSnapshot(tool, name string) {
	regKey := registryKeyForTool(tool)
	_ = m.store.WithLock(func() error {
		cache, err := m.store.LoadCache()
		if err != nil {
			return err
		}
		entries := cache.Snapshots[regKey]
		for i := range entries {
			if entries[i].Profile.ProfileName == name {
				cache.Snapshots[regKey] = append(entries[:i], entries[i+1:]...)
				break
			}
		}
		return m.store.SaveCache(cache)
	})
}

// toolForRegistryKey maps a usages provider registry key to the switcher tool
// name. Returns "" for providers without switcher support (copilot, mistral,
// ollama).
func toolForRegistryKey(regKey string) string {
	switch regKey {
	case ProviderClaudeCode:
		return "claude"
	case ProviderCodex:
		return "codex"
	default:
		return ""
	}
}

// refreshProfiles fetches/derives per-profile snapshots for switcher-enabled
// providers. The active profile is derived from the provider snapshot (no
// separate fetch — same credentials, same API). Inactive profiles are fetched
// if stale. Email is set from ProfileInfo, preferring the profile's captured
// value and falling through to the fetch's when empty. Returns true if any
// profile entry was created or updated.
func (m *Manager) refreshProfiles(ctx context.Context) (bool, error) {
	if m.profileLister == nil {
		return false, nil
	}
	changed := false
	for _, key := range knownProviders {
		tool := toolForRegistryKey(key)
		if tool == "" {
			continue
		}
		profiles := m.profileLister(tool)
		if len(profiles) == 0 {
			continue
		}
		for _, p := range profiles {
			if p.Active {
				m.deriveActiveProfile(key, p)
				if !m.profileEntryExists(key, p.Name) {
					_, _ = m.FetchProfileSnapshot(ctx, p.Tool, p.Name, p.CredPath)
				}
				m.setProfileFields(key, p.Name, p.Email, p.HasCredentials, p.ExpiresAt, p.Active)
				changed = true
				continue
			}
			_, err := m.FetchProfileSnapshot(ctx, tool, p.Name, p.CredPath)
			if err == nil {
				changed = true
			}
			m.setProfileFields(key, p.Name, p.Email, p.HasCredentials, p.ExpiresAt, p.Active)
		}
		// Reconciliation: drop any cache entry whose name isn't in the current
		// profile list. This removes orphans from older builds (e.g. a "default"
		// entry from before the fix) and handles profiles deleted in the switcher.
		validNames := make(map[string]bool, len(profiles))
		for _, p := range profiles {
			validNames[p.Name] = true
		}
		m.reconcileProfileEntries(key, validNames)
	}
	return changed, nil
}

func (m *Manager) profileEntryExists(regKey, profileName string) bool {
	found := false
	_ = m.store.WithLock(func() error {
		cache, err := m.store.LoadCache()
		if err != nil {
			return err
		}
		for _, e := range cache.Snapshots[regKey] {
			if e.Profile.ProfileName == profileName {
				found = true
				break
			}
		}
		return nil
	})
	return found
}

// deriveActiveProfile copies the provider's "default" cache entry as the active
// profile's entry, setting Profile = name and Email (prefer profile's, fall
// through to the fetch's). No network fetch — the active profile uses the same
// credentials as the provider fetch.
// deriveActiveProfile copies the provider's "default" cache entry as the active
// profile's entry, setting Profile = name and Email (prefer profile's, fall
// through to the fetch's). No network fetch — the active profile uses the same
// credentials as the provider fetch.
//
// Design property: the active profile IS the provider fetch result. There is
// no separate provider-level cache entry — the "default" entry and the active
// profile entry are the same data. This means a switch cannot leave the old
// account's numbers visible: InvalidateProfileSnapshot drops the old active
// entry, the next provider fetch creates a fresh "default" entry with the new
// account's data, and deriveActiveProfile copies it as the new active profile.
// Someone reintroducing a separate provider entry would bring the
// old-account-persists hazard straight back.
func (m *Manager) deriveActiveProfile(regKey string, p ProfileInfo) {
	_ = m.store.WithLock(func() error {
		cache, err := m.store.LoadCache()
		if err != nil {
			return err
		}
		entries := cache.Snapshots[regKey]
		def := defaultCacheEntry(entries)
		if def == nil {
			return nil // no provider snapshot yet; skip until the poller fetches one
		}
		entry := *def
		entry.Profile.ProfileName = p.Name
		entry.Profile.Active = p.Active
		entry.Profile.HasCredentials = p.HasCredentials
		entry.Profile.ExpiresAt = p.ExpiresAt
		if p.Email != "" {
			entry.Profile.Email = p.Email
		}
		// Replace the "default" entry with the active profile's entry (rename,
		// not duplicate). Drop the old default and any stale entry for this
		// profile, then append the fresh one.
		var newEntries []CacheEntry
		for _, e := range entries {
			if e.Profile.ProfileName == "" || e.Profile.ProfileName == "default" {
				continue
			}
			if e.Profile.ProfileName == p.Name {
				continue
			}
			newEntries = append(newEntries, e)
		}
		newEntries = append(newEntries, entry)
		cache.Snapshots[regKey] = newEntries
		return m.store.SaveCache(cache)
	})
}

// setProfileFields sets the switcher fields on a profile's cache entry,
// preferring the profile's captured email. No-op for email when empty (falls
// through to whatever the fetch produced). HasCredentials, ExpiresAt, and
// Active are always set from the switcher profile.
func (m *Manager) setProfileFields(regKey, profileName, email string, hasCreds bool, expiresAt *time.Time, active bool) {
	_ = m.store.WithLock(func() error {
		cache, err := m.store.LoadCache()
		if err != nil {
			return err
		}
		entries := cache.Snapshots[regKey]
		for i := range entries {
			if entries[i].Profile.ProfileName == profileName {
				if email != "" {
					entries[i].Profile.Email = email
				}
				entries[i].Profile.HasCredentials = hasCreds
				entries[i].Profile.ExpiresAt = expiresAt
				entries[i].Profile.Active = active
				break
			}
		}
		cache.Snapshots[regKey] = entries
		return m.store.SaveCache(cache)
	})
}

// reconcileProfileEntries removes cache entries for a switcher tool whose
// ProfileName is not in validNames. This removes orphans from older builds
// (e.g. a "default" entry from before the fix) and handles profiles deleted in
// the switcher. Without this, stale entries survive forever because no write
// path ever deletes — setProfileCacheEntry replaces by name, and nothing else
// cleans up.
func (m *Manager) reconcileProfileEntries(regKey string, validNames map[string]bool) {
	_ = m.store.WithLock(func() error {
		cache, err := m.store.LoadCache()
		if err != nil {
			return err
		}
		entries := cache.Snapshots[regKey]
		kept := entries[:0]
		for _, e := range entries {
			if validNames[e.Profile.ProfileName] {
				kept = append(kept, e)
			}
		}
		cache.Snapshots[regKey] = kept
		return m.store.SaveCache(cache)
	})
}

func (m *Manager) checkForceCooldown(provider string) int {
	key := "*"
	now := m.clock.Now()
	m.forceMu.Lock()
	defer m.forceMu.Unlock()
	last, ok := m.lastForce[key]
	if ok {
		elapsed := now.Sub(last)
		if elapsed < m.forceCooldown {
			return int((m.forceCooldown - elapsed + time.Second - 1) / time.Second)
		}
	}
	m.lastForce[key] = now
	return 0
}

func configSecrets(cfg Config) []string {
	secrets := make([]string, 0, len(cfg.Providers)*3)
	for _, pc := range cfg.Providers {
		secrets = append(secrets, pc.Token, pc.APIKey, pc.Cookie)
	}
	return secrets
}
