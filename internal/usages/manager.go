package usages

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
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

	profileMu          sync.Mutex
	profileGeneration  map[string]uint64
	activeFingerprints map[string]string
}

func NewManager(store *Store, registry *Registry) *Manager {
	if store == nil {
		store = NewStore()
	}
	if registry == nil {
		registry = EmptyRegistry()
	}
	return &Manager{
		store:              store,
		registry:           registry,
		clock:              realClock{},
		lastForce:          map[string]time.Time{},
		forceCooldown:      MinRefreshSec * time.Second,
		profileGeneration:  map[string]uint64{},
		activeFingerprints: map[string]string{},
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

const implicitProfileName = "local"

func (m *Manager) SetProfileLister(fn ProfileLister) {
	m.profileLister = fn
}

func hasFile(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func (m *Manager) implicitProfile(key string) ProfileInfo {
	p := ProfileInfo{Name: implicitProfileName, Active: true}
	switch key {
	case ProviderClaudeCode:
		p.Tool = "claude"
		p.CredPath, _ = claudeAuthPath()
		p.HasCredentials = hasFile(p.CredPath)
	case ProviderCodex:
		p.Tool = "codex"
		p.CredPath, _ = codexAuthPath()
		p.HasCredentials = hasFile(p.CredPath)
	default:
		cfg, err := m.store.LoadConfig()
		if err != nil {
			return p
		}
		pc := cfg.Providers[key]
		switch key {
		case ProviderGitHubCopilot:
			p.HasCredentials = strings.TrimSpace(pc.Token) != ""
		case ProviderMistral:
			p.HasCredentials = strings.TrimSpace(pc.Cookie) != ""
		case ProviderOllamaCloud:
			p.HasCredentials = strings.TrimSpace(pc.Cookie) != "" || strings.TrimSpace(pc.APIKey) != ""
		}
	}
	return p
}

func (m *Manager) profilesForProvider(key string) []ProfileInfo {
	tool := toolForRegistryKey(key)
	if tool != "" && m.profileLister != nil {
		if profiles := m.profileLister(tool); len(profiles) > 0 {
			return profiles
		}
	}
	return []ProfileInfo{m.implicitProfile(key)}
}

func (m *Manager) resolveProfiles(keys []string) map[string][]ProfileInfo {
	resolved := make(map[string][]ProfileInfo, len(keys))
	for _, key := range keys {
		resolved[key] = m.profilesForProvider(key)
	}
	return resolved
}

type activeProfileOwner struct {
	profile     ProfileInfo
	fingerprint string
	generation  uint64
}

func credentialFingerprint(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "unreadable"
	}
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (m *Manager) activeProfileOwner(key string) (activeProfileOwner, bool) {
	for _, profile := range m.profilesForProvider(key) {
		if profile.Active {
			return activeProfileOwner{
				profile:     profile,
				fingerprint: credentialFingerprint(profile.CredPath),
			}, true
		}
	}
	return activeProfileOwner{}, false
}

func sameActiveProfileOwner(left, right activeProfileOwner) bool {
	return left.profile.Name == right.profile.Name &&
		left.profile.CredPath == right.profile.CredPath &&
		left.fingerprint == right.fingerprint
}

func (m *Manager) activeCredentialsChanged(key string) bool {
	owner, ok := m.activeProfileOwner(key)
	if !ok || owner.profile.Name == implicitProfileName || owner.profile.CredPath == "" {
		return false
	}
	m.profileMu.Lock()
	defer m.profileMu.Unlock()
	previous, known := m.activeFingerprints[key]
	return !known || previous != owner.fingerprint
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
// by the poller (Manager.Start) and ForceRefresh. On a cold cache it still
// returns the resolved profile identities with empty usage snapshots.
// assembleResponse builds a UsagesResponse from config and cache. Every
// known provider appears (even if disabled), with at least one profile.
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

func newerCacheEntry(current, candidate CacheEntry) CacheEntry {
	if current.Profile.LastRefreshAt == nil {
		return candidate
	}
	if candidate.Profile.LastRefreshAt != nil && candidate.Profile.LastRefreshAt.After(*current.Profile.LastRefreshAt) {
		return candidate
	}
	return current
}

// normalizeProfileEntries migrates legacy default entries and reconciles the
// cache to the currently resolved profiles. Legacy history is safe to carry to
// the implicit local profile; with named switcher profiles its account owner is
// unknowable, so it is discarded rather than attributed to the wrong account.
func normalizeProfileEntries(entries []CacheEntry, profiles []ProfileInfo) ([]CacheEntry, bool) {
	valid := make(map[string]ProfileInfo, len(profiles))
	implicit := len(profiles) == 1 && profiles[0].Name == implicitProfileName
	for _, profile := range profiles {
		valid[profile.Name] = profile
	}
	byName := make(map[string]CacheEntry, len(entries))
	for _, entry := range entries {
		name := entry.Profile.ProfileName
		if name == "" || name == "default" {
			if !implicit {
				continue
			}
			name = implicitProfileName
			entry.Profile.ProfileName = name
		}
		if _, ok := valid[name]; !ok {
			continue
		}
		if current, ok := byName[name]; ok {
			entry = newerCacheEntry(current, entry)
		}
		byName[name] = entry
	}

	normalized := make([]CacheEntry, 0, len(profiles))
	for _, profile := range profiles {
		entry, ok := byName[profile.Name]
		if !ok {
			entry = CacheEntry{Profile: Profile{ProfileName: profile.Name}}
		}
		entry.Profile.ProfileName = profile.Name
		entry.Profile.Active = profile.Active
		entry.Profile.HasCredentials = profile.HasCredentials
		entry.Profile.ExpiresAt = profile.ExpiresAt
		if profile.Email != "" {
			entry.Profile.Email = profile.Email
		}
		normalized = append(normalized, entry)
	}
	return normalized, !reflect.DeepEqual(entries, normalized)
}

func normalizeCache(cache *Cache, resolved map[string][]ProfileInfo) bool {
	changed := false
	for _, key := range knownProviders {
		entries, entryChanged := normalizeProfileEntries(cache.Snapshots[key], resolved[key])
		cache.Snapshots[key] = entries
		changed = changed || entryChanged
	}
	return changed
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
	resolved := m.resolveProfiles(knownProviders)
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
		if normalizeCache(&cache, resolved) {
			if err := m.store.SaveCache(cache); err != nil {
				return err
			}
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

type profileFetch struct {
	provider string
	profile  ProfileInfo
}

func (m *Manager) refreshWithChange(ctx context.Context, provider string, force bool) (UsagesResponse, bool, int, error) {
	var resp UsagesResponse
	var toFetch []profileFetch
	credentialChanges := make(map[string]bool)
	keys := knownProviders
	if provider != "" {
		keys = []string{provider}
	}
	resolved := m.resolveProfiles(knownProviders)
	for _, key := range keys {
		credentialChanges[key] = m.activeCredentialsChanged(key)
	}
	err := m.store.WithLock(func() error {
		cfg, err := m.store.LoadConfig()
		if err != nil {
			return err
		}
		cache, err := m.store.LoadCache()
		if err != nil {
			return err
		}
		cacheChanged := normalizeCache(&cache, resolved)
		if cacheChanged {
			if err := m.store.SaveCache(cache); err != nil {
				return err
			}
		}
		interval, _ := m.store.RefreshInterval(cfg)
		resp = m.assembleResponse(cfg, cache)
		now := m.clock.Now()
		for _, key := range keys {
			pc := cfg.Providers[key]
			if !pc.Enabled {
				continue
			}
			for _, profile := range resolved[key] {
				entry := profileCacheEntry(cache.Snapshots[key], profile.Name)
				credentialsChanged := profile.Active && credentialChanges[key]
				shouldFetch := force || entry == nil || entry.Profile.LastRefreshAt == nil || now.Sub(*entry.Profile.LastRefreshAt) >= interval || credentialsChanged
				if shouldFetch {
					toFetch = append(toFetch, profileFetch{provider: key, profile: profile})
				}
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
	for _, target := range toFetch {
		wg.Add(1)
		go func(target profileFetch) {
			defer wg.Done()
			results <- m.fetchProfileWithProviderLock(ctx, target.provider, target.profile, force, true)
		}(target)
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

	// A switch can complete while profile fetches are in flight. Resolve again
	// before publishing so force-refresh responses and broadcasts never reuse
	// the pre-fetch active markers or profile set.
	resolved = m.resolveProfiles(knownProviders)
	err = m.store.WithLock(func() error {
		cfg, err := m.store.LoadConfig()
		if err != nil {
			return err
		}
		cache, err := m.store.LoadCache()
		if err != nil {
			return err
		}
		if normalizeCache(&cache, resolved) {
			if err := m.store.SaveCache(cache); err != nil {
				return err
			}
		}
		resp = m.assembleResponse(cfg, cache)
		return nil
	})
	if err != nil {
		return UsagesResponse{}, false, 0, err
	}
	return resp, changed, 0, nil
}

func profileCacheEntry(entries []CacheEntry, profile string) *CacheEntry {
	for i := range entries {
		if entries[i].Profile.ProfileName == profile {
			return &entries[i]
		}
	}
	return nil
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
	changed  bool
	err      error
	fetchErr error
}

func (m *Manager) fetchProfileWithProviderLock(ctx context.Context, key string, profile ProfileInfo, force, enabledOnly bool) fetchResult {
	lock, err := acquireProviderLock(m.store.root, key)
	if err != nil {
		return fetchResult{err: err}
	}
	defer lock.Unlock()

	owner, hasOwner := activeProfileOwner{}, false
	if profile.Active {
		owner, hasOwner = m.activeProfileOwner(key)
		if !hasOwner || owner.profile.Name != profile.Name || owner.profile.CredPath != profile.CredPath {
			return fetchResult{}
		}
		profile = owner.profile
	}
	if hasOwner {
		m.profileMu.Lock()
		owner.generation = m.profileGeneration[key]
		m.profileMu.Unlock()
	}

	var previous CacheEntry
	var interval time.Duration
	var now time.Time
	shouldFetch := false
	credentialsChanged := profile.Active && m.activeCredentialsChanged(key)
	err = m.store.WithLock(func() error {
		cfg, err := m.store.LoadConfig()
		if err != nil {
			return err
		}
		if enabledOnly && !cfg.Providers[key].Enabled {
			return nil
		}
		cache, err := m.store.LoadCache()
		if err != nil {
			return err
		}
		interval, _ = m.store.RefreshInterval(cfg)
		now = m.clock.Now()
		entry := profileCacheEntry(cache.Snapshots[key], profile.Name)
		if entry != nil {
			previous = *entry
		}
		shouldFetch = force || entry == nil || entry.Profile.LastRefreshAt == nil || now.Sub(*entry.Profile.LastRefreshAt) >= interval || credentialsChanged
		return nil
	})
	if err != nil || !shouldFetch {
		return fetchResult{err: err}
	}

	storedFingerprint := ""
	if !profile.Active {
		storedFingerprint = credentialFingerprint(profile.CredPath)
	}
	var entry CacheEntry
	var fetchErr error
	if profile.Active {
		entry = m.fetchOne(ctx, key, previous)
	} else {
		entry, fetchErr = m.fetchStoredProfile(ctx, profile, previous)
	}
	entry.Profile.ProfileName = profile.Name
	entry.Profile.Active = profile.Active
	entry.Profile.HasCredentials = profile.HasCredentials
	entry.Profile.ExpiresAt = profile.ExpiresAt
	if profile.Email != "" {
		entry.Profile.Email = profile.Email
	}
	if !profile.Active && storedFingerprint != credentialFingerprint(profile.CredPath) {
		return fetchResult{fetchErr: errors.New("credentials changed during usage fetch")}
	}

	// Email refresh for providers whose email isn't in the usage fetch (Copilot,
	// via a separate GitHub /user call). Same floor as inactive profiles:
	// max(providerInterval, 1h), absent means fetch now. The email persists
	// across usage refreshes — only re-fetched when EmailFetchedAt is stale.
	if profile.Active {
		if p, ok := m.registry.Provider(key); ok {
			if ef, ok := p.(EmailFetcher); ok {
				emailScope := ""
				if scoped, ok := p.(EmailScopeProvider); ok {
					emailScope = scoped.EmailScope(m.store)
				}
				emailFloor := interval
				if emailFloor < emailRefreshFloor {
					emailFloor = emailRefreshFloor
				}
				if previous.EmailFetchedAt == nil || previous.EmailScope != emailScope || now.Sub(*previous.EmailFetchedAt) >= emailFloor {
					if email, eerr := ef.FetchEmail(ctx, m.store); eerr == nil && email != "" {
						entry.Profile.Email = email
						emailNow := m.clock.Now()
						entry.EmailFetchedAt = &emailNow
						entry.EmailScope = emailScope
					}
				} else {
					entry.Profile.Email = previous.Profile.Email
					entry.EmailFetchedAt = previous.EmailFetchedAt
					entry.EmailScope = previous.EmailScope
				}
			}
		}
	}

	if hasOwner {
		m.profileMu.Lock()
		defer m.profileMu.Unlock()
		current, ok := m.activeProfileOwner(key)
		if !ok || owner.generation != m.profileGeneration[key] || !sameActiveProfileOwner(owner, current) {
			return fetchResult{}
		}
	}

	err = m.store.WithLock(func() error {
		cfg, err := m.store.LoadConfig()
		if err != nil {
			return err
		}
		if enabledOnly && !cfg.Providers[key].Enabled {
			return nil
		}
		cache, err := m.store.LoadCache()
		if err != nil {
			return err
		}
		entry.Profile = entry.Profile.Redacted(configSecrets(cfg)...)
		cache.Snapshots[key] = setProfileCacheEntry(cache.Snapshots[key], profile.Name, entry)
		return m.store.SaveCache(cache)
	})
	if err != nil {
		return fetchResult{err: err}
	}
	if hasOwner {
		m.activeFingerprints[key] = owner.fingerprint
	}
	return fetchResult{changed: true, fetchErr: fetchErr}
}

func (m *Manager) fetchOne(ctx context.Context, key string, previous CacheEntry) CacheEntry {
	now := m.clock.Now()
	p, ok := m.registry.Provider(key)
	if !ok {
		return CacheEntry{Profile: Profile{ProfileName: implicitProfileName, Snapshot: Snapshot{Status: StatusNotConfigured}}}
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	snap, err := p.Fetch(fetchCtx, m.store)
	if err == nil {
		if snap.Status == "" {
			snap.Status = StatusOK
		}
		snap.LastRefreshAt = &now
		return CacheEntry{Profile: profileFromSnapshot(snap, implicitProfileName)}
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
	return CacheEntry{Profile: profileFromSnapshot(errSnapshot, implicitProfileName)}
}

func (m *Manager) fetchStoredProfile(ctx context.Context, profile ProfileInfo, previous CacheEntry) (CacheEntry, error) {
	now := m.clock.Now()
	fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var snap Snapshot
	var err error
	switch profile.Tool {
	case "claude":
		snap, err = fetchClaudeSnapshotFromPathFunc(fetchCtx, profile.CredPath)
	case "codex":
		snap, err = fetchCodexSnapshotFromPathFunc(fetchCtx, profile.CredPath, false, nil)
	default:
		err = fmt.Errorf("unsupported switcher tool %q for profile snapshot", profile.Tool)
	}
	if err == nil {
		if snap.Status == "" {
			snap.Status = StatusOK
		}
		snap.LastRefreshAt = &now
		return CacheEntry{Profile: profileFromSnapshot(snap, profile.Name)}, nil
	}

	errSnapshot := Snapshot{Status: StatusFetchError, Error: err.Error()}
	if snapErr, ok := err.(*SnapshotError); ok {
		errSnapshot = snapErr.Snapshot
		if errSnapshot.Status == "" {
			errSnapshot.Status = StatusFetchError
		}
		if errSnapshot.Error == "" {
			errSnapshot.Error = snapErr.Error()
		}
	}
	if errors.Is(fetchCtx.Err(), context.DeadlineExceeded) {
		errSnapshot.Status = StatusTimeout
	}
	if shouldPreservePreviousSnapshot(err, errSnapshot, previous) {
		prof := previous.Profile
		prof.Status = StatusStaleCache
		prof.Stale = true
		prof.Error = errSnapshot.Error
		prof.ErrorDetail = errSnapshot.ErrorDetail
		return CacheEntry{Profile: prof}, err
	}
	errSnapshot.LastRefreshAt = &now
	return CacheEntry{Profile: profileFromSnapshot(errSnapshot, profile.Name)}, err
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
	if detail.Fields["usage.challenge"] == "cloudflare" {
		return true
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

// FetchProfileSnapshot returns the cached usage snapshot for a stored
// credential profile, fetching fresh data when the cache is stale. It uses
// persist=false so reading a stored profile never rotates its tokens; the
// target-driven poller uses the normal provider for the active profile.
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
	target := ProfileInfo{Tool: tool, Name: name, CredPath: credPath, HasCredentials: hasFile(credPath)}
	result := m.fetchProfileWithProviderLock(ctx, regKey, target, false, false)
	if result.err != nil {
		return Profile{}, result.err
	}
	var profile Profile
	err := m.store.WithLock(func() error {
		cache, err := m.store.LoadCache()
		if err != nil {
			return err
		}
		entry := profileCacheEntry(cache.Snapshots[regKey], name)
		if entry != nil {
			profile = entry.Profile
		}
		return nil
	})
	if err != nil {
		return Profile{}, err
	}
	return profile, result.fetchErr
}

// InvalidateProfileSnapshot removes a profile's entry from the cache array,
// forcing a fresh fetch on the next poller tick. Called after a switch to
// ensure the two affected profiles refresh immediately.
func (m *Manager) InvalidateProfileSnapshot(tool, name string) {
	regKey := registryKeyForTool(tool)
	m.profileMu.Lock()
	defer m.profileMu.Unlock()
	m.profileGeneration[regKey]++
	delete(m.activeFingerprints, regKey)
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
