package usages

import (
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/usages/providers"
)

// MinForceCooldown is the minimum gap between two owner-forced refreshes. It
// matches the smallest configurable polling interval, so a forced refresh can
// never run more often than the poller is allowed to.
const MinForceCooldown = 15 * time.Second

// SetUpdateHook registers fn to be told that the published snapshot changed.
// fn runs while the Reader holds its lock, so it must be quick, must not
// block, and must not call back into the Reader; the server uses it to nudge
// live event streams, which then re-read the snapshot themselves.
func (r *Reader) SetUpdateHook(fn func()) {
	r.mu.Lock()
	r.onUpdate = fn
	r.mu.Unlock()
}

// NotifyChanged reports a change to what Snapshot returns that did not go
// through a refresh: saved display settings, or a config write that left the
// provider's stored values as they were but flipped what is shown.
func (r *Reader) NotifyChanged() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.signalLocked()
}

// signalLocked reports a snapshot change. Callers hold r.mu.
func (r *Reader) signalLocked() {
	if r.onUpdate != nil {
		r.onUpdate()
	}
}

// CheckForceCooldown starts a forced refresh when none ran within the
// cooldown. It returns 0 when the refresh may proceed, otherwise the whole
// seconds until the next one is allowed. The cooldown is global, not per
// provider: a forced refresh of one provider also counts against the rest.
func (r *Reader) CheckForceCooldown() int {
	r.forceMu.Lock()
	defer r.forceMu.Unlock()
	now := time.Now()
	if !r.lastForce.IsZero() {
		if elapsed := now.Sub(r.lastForce); elapsed < r.forceCooldown {
			return int((r.forceCooldown - elapsed + time.Second - 1) / time.Second)
		}
	}
	r.lastForce = now
	return 0
}

// ReadCache returns the snapshot the server last wrote to cacheDir without
// starting a Reader: no network, no credential files, no writes. Provider
// enablement, order and display settings come from the saved config, so the
// result matches what GET /api/usages would show for the same cache, with the
// providers in the saved order. The time
// is when the cache file was last written; it is zero when no server has
// written one yet.
func ReadCache(cacheDir string) (Response, time.Time, error) {
	response := defaultResponse()
	var written time.Time
	path := filepath.Join(cacheDir, "cache.json")
	if info, err := os.Stat(path); err == nil {
		written = info.ModTime()
	}
	if data, err := os.ReadFile(path); err == nil {
		var saved Response
		if json.Unmarshal(data, &saved) == nil {
			for _, provider := range saved.Providers {
				if len(provider.Profiles) == 0 {
					continue
				}
				for i := range response.Providers {
					if response.Providers[i].ProviderName == provider.ProviderName {
						response.Providers[i].Profiles = provider.Profiles
					}
				}
			}
		}
	}
	cfg, err := providers.NewStore(cacheDir).LoadConfig()
	decorateResponse(&response, cfg)
	response.Settings.AlertWarningRemaining = cfg.AlertWarningRemaining
	response.Settings.AlertCriticalRemaining = cfg.AlertCriticalRemaining
	sort.SliceStable(response.Providers, func(i, j int) bool { return response.Providers[i].OrderNumber < response.Providers[j].OrderNumber })
	return response, written, err
}
