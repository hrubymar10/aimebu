package usages

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var ErrForceCooldown = errors.New("force refresh cooldown active")

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

	onUpdate func(Response)
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

func (m *Manager) SetUpdateHook(fn func(Response)) {
	m.onUpdate = fn
}

func (m *Manager) Settings(ctx context.Context) (SettingsInfo, error) {
	var info SettingsInfo
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

func (m *Manager) Snapshot(ctx context.Context, provider string) (Response, error) {
	if provider != "" && !KnownProvider(provider) {
		return Response{}, unknownProviderError(provider)
	}
	resp, _, err := m.refresh(ctx, provider, false)
	return resp, err
}

func (m *Manager) ForceRefresh(ctx context.Context, provider string) (Response, int, error) {
	if provider != "" && !KnownProvider(provider) {
		return Response{}, 0, unknownProviderError(provider)
	}
	retry := m.checkForceCooldown(provider)
	if retry > 0 {
		return Response{}, retry, ErrForceCooldown
	}
	resp, changed, retry, err := m.refreshWithChange(ctx, provider, true)
	if err == nil && changed && m.onUpdate != nil {
		m.onUpdate(resp)
	}
	return resp, retry, err
}

func (m *Manager) UpdateSettings(ctx context.Context, intervalSec int, percentDisplay string, providerOrder []string, updateProviderOrder bool) (SettingsInfo, error) {
	if intervalSec != 0 && intervalSec < MinRefreshSec {
		return SettingsInfo{}, errors.New("refresh_interval_sec is below minimum")
	}
	if percentDisplay != "" && !validPercentDisplay(percentDisplay) {
		return SettingsInfo{}, errors.New("percent_display must be left or used")
	}
	var info SettingsInfo
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
	var cfg Config
	err := m.store.WithLock(func() error {
		var err error
		cfg, err = m.store.LoadConfig()
		if err != nil {
			return err
		}
		pc := cfg.Providers[ProviderOllamaCloud]
		if cookie == "" {
			pc.Cookie = ""
			pc.Enabled = false
		} else {
			normalized, detail, err := normalizeOllamaCookieHeader(cookie)
			if err != nil {
				if detail != nil {
					return &SnapshotError{Snapshot: Snapshot{Provider: ProviderOllamaCloud, Status: StatusAuthMissing, Error: err.Error(), ErrorDetail: detail}, Err: err}
				}
				return err
			}
			pc.Cookie = normalized
			pc.Enabled = true
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

func (m *Manager) Start(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				resp, changed, _, err := m.refreshWithChange(ctx, "", false)
				if err == nil && changed && m.onUpdate != nil && len(resp.Snapshots) > 0 {
					m.onUpdate(resp)
				}
			}
		}
	}()
}

func (m *Manager) refresh(ctx context.Context, provider string, force bool) (Response, int, error) {
	resp, _, retry, err := m.refreshWithChange(ctx, provider, force)
	return resp, retry, err
}

func (m *Manager) refreshWithChange(ctx context.Context, provider string, force bool) (Response, bool, int, error) {
	var resp Response
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
		interval, info := m.store.RefreshInterval(cfg)
		resp.Settings = &info
		resp.Providers = ProviderInfos(cfg, m.registry)
		resp.Snapshots = map[string]Snapshot{}
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
			entry, ok := cache.Snapshots[key]
			shouldFetch := force || !ok || entry.LastRefreshAt == nil || now.Sub(*entry.LastRefreshAt) >= interval
			if shouldFetch {
				toFetch = append(toFetch, key)
			}
			resp.Snapshots[key] = entry.Snapshot
		}
		return nil
	})
	if err != nil {
		return Response{}, false, 0, err
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
	for result := range results {
		if result.err != nil {
			return Response{}, false, 0, result.err
		}
		changed = changed || result.changed
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
		_, info := m.store.RefreshInterval(cfg)
		resp.Settings = &info
		resp.Providers = ProviderInfos(cfg, m.registry)
		resp.Snapshots = map[string]Snapshot{}
		keys := knownProviders
		if provider != "" {
			keys = []string{provider}
		}
		for _, key := range keys {
			if !cfg.Providers[key].Enabled {
				continue
			}
			if entry, ok := cache.Snapshots[key]; ok {
				resp.Snapshots[key] = entry.Snapshot
			}
		}
		return nil
	})
	if err != nil {
		return Response{}, false, 0, err
	}
	return resp, changed, 0, nil
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
		interval, _ := m.store.RefreshInterval(cfg)
		now := m.clock.Now()
		var ok bool
		previous, ok = cache.Snapshots[key]
		shouldFetch = force || !ok || previous.LastRefreshAt == nil || now.Sub(*previous.LastRefreshAt) >= interval
		return nil
	})
	if err != nil || !shouldFetch {
		return fetchResult{err: err}
	}

	entry := m.fetchOne(ctx, key, previous)

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
		entry.Snapshot = entry.Snapshot.Redacted(configSecrets(cfg)...)
		cache.Snapshots[key] = entry
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
		snap := Snapshot{Provider: key, Status: StatusNotConfigured, LastRefreshAt: &now}
		return CacheEntry{Snapshot: snap, LastRefreshAt: &now}
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	snap, err := p.Fetch(fetchCtx, m.store)
	if err == nil {
		snap.Provider = key
		if snap.Status == "" {
			snap.Status = StatusOK
		}
		snap.LastRefreshAt = &now
		return CacheEntry{Snapshot: snap, LastRefreshAt: &now}
	}
	status := StatusFetchError
	errSnapshot := Snapshot{Provider: key, Status: status, Error: err.Error()}
	if snapErr, ok := err.(*SnapshotError); ok {
		errSnapshot = snapErr.Snapshot
		if errSnapshot.Provider == "" {
			errSnapshot.Provider = key
		}
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
		snap = previous.Snapshot
		snap.Status = StatusStaleCache
		snap.Stale = true
		snap.Error = errSnapshot.Error
		snap.ErrorDetail = errSnapshot.ErrorDetail
		snap.LastRefreshAt = previous.LastRefreshAt
		return CacheEntry{Snapshot: snap, LastRefreshAt: previous.LastRefreshAt}
	}
	snap = errSnapshot
	snap.LastRefreshAt = &now
	return CacheEntry{Snapshot: snap, LastRefreshAt: &now}
}

func shouldPreservePreviousSnapshot(err error, errSnapshot Snapshot, previous CacheEntry) bool {
	if previous.Snapshot.Provider == "" {
		return false
	}
	if previous.Snapshot.Status != StatusOK && previous.Snapshot.Status != StatusStaleCache {
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
	secrets := make([]string, 0, len(cfg.Providers)*2)
	for _, pc := range cfg.Providers {
		secrets = append(secrets, pc.Token, pc.Cookie)
	}
	return secrets
}
