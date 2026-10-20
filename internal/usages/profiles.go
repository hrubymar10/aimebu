package usages

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/usages/providers"
)

// StoredProfile is one named account profile of the optional switcher. The
// usages package cannot import the switcher (it validates credentials with
// this package), so the server hands the Reader a lister instead.
type StoredProfile struct {
	Tool           string // "claude" or "codex"
	Name           string
	Active         bool
	CredPath       string // the stored credential copy; unused for the active profile
	HasCredentials bool
	Email          string
	ExpiresAt      *time.Time
	// SwitchedAwayAt is when the profile last stopped being active; nil when it
	// never was or the time is unknown.
	SwitchedAwayAt *time.Time
}

// ProfileLister lists every switcher profile across tools.
type ProfileLister func() ([]StoredProfile, error)

// SetProfiles enables usage snapshots for inactive switcher profiles. withLock
// is the switcher's lock: a stored credential copy is read under it so a
// switch, rename or removal cannot replace the file mid-read. Everything is
// read-only: no token is ever refreshed and no credential file is written.
func (r *Reader) SetProfiles(lister ProfileLister, withLock func(func() error) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.profiles, r.profileLock = lister, withLock
}

// StoredProfile returns the last read-only snapshot of an inactive profile.
func (r *Reader) StoredProfile(provider, name string) (Profile, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for i := range r.response.Providers {
		if r.response.Providers[i].ProviderName != provider {
			continue
		}
		for _, p := range r.response.Providers[i].Profiles[1:] {
			if p.ProfileName == name {
				p.Windows = append([]Window(nil), p.Windows...)
				return p, true
			}
		}
	}
	return Profile{}, false
}

// setStoredLocked records an inactive profile's snapshot after the live one
// and persists it with the rest of the cache. Callers hold r.mu.
func (r *Reader) setStoredLocked(i int, profile Profile) {
	profiles := r.response.Providers[i].Profiles
	replaced := false
	for j := 1; j < len(profiles); j++ {
		if profiles[j].ProfileName == profile.ProfileName {
			profiles[j], replaced = profile, true
			break
		}
	}
	if !replaced {
		r.response.Providers[i].Profiles = append(profiles, profile)
	}
	r.recordHistoryLocked(r.response.Providers[i].ProviderName, profile)
	r.persistLocked()
}

// dropStoredLocked forgets snapshots of profiles that are gone or no longer
// inactive. Callers hold r.mu.
func (r *Reader) dropStoredLocked(i int, keep map[string]bool) {
	profiles := r.response.Providers[i].Profiles
	kept := profiles[:1]
	for _, p := range profiles[1:] {
		if keep[p.ProfileName] {
			kept = append(kept, p)
		} else {
			r.forgetHistoryLocked(r.response.Providers[i].ProviderName, p.ProfileName)
		}
	}
	if len(kept) != len(profiles) {
		r.response.Providers[i].Profiles = kept
		r.persistLocked()
	}
}

func (r *Reader) persistLocked() {
	if data, err := json.Marshal(r.response); err == nil {
		_ = writeCache(r.cachePath, data)
	}
	r.signalLocked()
}

const (
	backoffBase = 2 * time.Minute
	backoffMax  = 30 * time.Minute
)

// backoffState throttles one stored profile after transient failures
// (rate limiting, 5xx, timeouts), so many profiles cannot hammer an endpoint
// that is already unhappy. The delay doubles per consecutive failure up to
// backoffMax and resets on any answer the endpoint gave on purpose.
type backoffState struct {
	fails int
	until time.Time
}

func backoffDelay(fails int) time.Duration {
	delay := backoffBase
	for i := 1; i < fails && delay < backoffMax; i++ {
		delay *= 2
	}
	if delay > backoffMax {
		delay = backoffMax
	}
	return delay
}

// ForgetProfile drops a removed profile's cached snapshot and backoff right
// away (any profile, not only the active one), and invalidates stored fetches
// of that provider that are still in flight.
func (r *Reader) ForgetProfile(provider, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.response.Providers {
		if r.response.Providers[i].ProviderName != provider {
			continue
		}
		r.providerGeneration[provider]++
		delete(r.backoff, provider+"\x00"+name)
		r.forgetHistoryLocked(provider, name)
		r.dropStoredLocked(i, func() map[string]bool {
			keep := map[string]bool{}
			for _, p := range r.response.Providers[i].Profiles[1:] {
				if p.ProfileName != name {
					keep[p.ProfileName] = true
				}
			}
			return keep
		}())
	}
}

func fingerprint(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// fileFingerprint hashes a credential file without keeping its content.
// "unreadable" stands for a missing file so a login appearing or vanishing is
// also a change.
func fileFingerprint(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "unreadable"
	}
	return fingerprint(data)
}

// liveChanged reports whether the live credentials of provider i differ from
// those behind its current active snapshot, so the poller refreshes it without
// waiting for the interval (a re-login, or the harness rotating its token).
func (r *Reader) liveChanged(i int) bool {
	if !r.providerOn(i) {
		return false
	}
	key := []string{providers.ProviderCodex, providers.ProviderClaudeCode}[i]
	path := []string{r.codexPath, r.claudePath}[i]
	r.mu.RLock()
	last, known := r.activePrint[key]
	r.mu.RUnlock()
	return known && last != fileFingerprint(path)
}

// pollLiveChanges re-polls the active snapshot of any enabled provider whose
// live credential file changed (a re-login, or the harness rotating its
// token). Stored profiles are not touched: they wait for the interval.
func (r *Reader) pollLiveChanges(ctx context.Context) {
	for i := 0; i < 2; i++ {
		if r.liveChanged(i) {
			r.refreshLocalOne(ctx, i)
		}
	}
}

func (r *Reader) providerOn(i int) bool {
	key := []string{providers.ProviderCodex, providers.ProviderClaudeCode}[i]
	if cfg, err := r.extra.LoadConfig(); err == nil {
		pc, configured := cfg.Providers[key]
		return !configured || pc.Enabled
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.response.Providers[i].Enabled
}

// readStored reads a stored credential copy under the switcher lock.
func (r *Reader) readStored(path string) ([]byte, error) {
	r.mu.RLock()
	withLock := r.profileLock
	r.mu.RUnlock()
	var data []byte
	read := func() (err error) {
		data, err = os.ReadFile(path)
		return err
	}
	if withLock != nil {
		if err := withLock(read); err != nil {
			return nil, err
		}
		return data, nil
	}
	return data, read()
}

// refreshStored reads usage for every inactive profile of provider i.
func (r *Reader) refreshStored(ctx context.Context, i int) {
	r.mu.RLock()
	lister := r.profiles
	r.mu.RUnlock()
	if lister == nil || !r.providerOn(i) {
		return
	}
	key := []string{providers.ProviderCodex, providers.ProviderClaudeCode}[i]
	tool := []string{"codex", "claude"}[i]
	list, err := lister()
	if err != nil {
		return
	}
	livePrint := fileFingerprint([]string{r.codexPath, r.claudePath}[i])
	present := map[string]bool{}
	for _, sp := range list {
		if sp.Tool != tool || sp.Active {
			continue
		}
		present[sp.Name] = true
		if ctx.Err() != nil {
			return
		}
		r.refreshStoredProfile(ctx, i, key, sp, livePrint, lister)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropStoredLocked(i, present)
	for k := range r.backoff {
		if strings.HasPrefix(k, key+"\x00") && !present[strings.TrimPrefix(k, key+"\x00")] {
			delete(r.backoff, k)
		}
	}
}

func (r *Reader) refreshStoredProfile(ctx context.Context, i int, key string, sp StoredProfile, livePrint string, lister ProfileLister) {
	storeKey := key + "\x00" + sp.Name
	r.mu.RLock()
	generation := r.generation
	providerGeneration := r.providerGeneration[key]
	waiting := r.now().Before(r.backoff[storeKey].until)
	r.mu.RUnlock()
	if waiting {
		return
	}
	previous, _ := r.StoredProfile(key, sp.Name)

	base := Profile{ProfileName: sp.Name, HasCredentials: sp.HasCredentials, Email: sp.Email, ExpiresAt: sp.ExpiresAt, Status: "not_selected"}
	var profile Profile
	var fetchErr error
	var credPrint string
	switch {
	case !sp.HasCredentials || sp.CredPath == "":
		profile = base
	default:
		data, err := r.readStored(sp.CredPath)
		if err != nil {
			// Lock contention or a transient read failure says nothing about the
			// login itself: keep the last snapshot and retry on the next tick.
			base.Status, base.Error = "read_error", "Stored credentials could not be read; will retry."
			profile, fetchErr = base, err
			break
		}
		credPrint = fingerprint(data)
		if credPrint == livePrint {
			// The same login as the active profile (a stale copy of the live
			// account): polling it again would only double the requests.
			base.Error = "Same login as the active profile."
			profile = base
			break
		}
		profile, fetchErr = r.fetchStoredProfile(ctx, i, base, data)
	}
	transient := fetchErr != nil && (profile.Status == "fetch_error" || profile.Status == "timeout")
	var failures int
	if transient {
		r.mu.RLock()
		failures = r.backoff[storeKey].fails + 1
		r.mu.RUnlock()
		retryAt := r.now().Add(backoffDelay(failures))
		profile.Status = "backing_off"
		profile.Error = "Usage endpoint failed; retrying after " + retryAt.UTC().Format("15:04") + " UTC (" + profile.Error + ")"
	}
	if fetchErr != nil {
		profile.Windows, profile.Plan, profile.Credits = previous.Windows, previous.Plan, previous.Credits
		profile.LastRefreshAt = previous.LastRefreshAt
		profile.Stale = len(previous.Windows) > 0
	}

	// Late results are dropped, not stored: the owner may have switched, the
	// profile may have become the active one, or its stored copy may have been
	// replaced while the request was in flight.
	if credPrint != "" {
		if data, err := r.readStored(sp.CredPath); err != nil || fingerprint(data) != credPrint {
			return
		}
	}
	if current, err := lister(); err != nil || !stillInactive(current, sp) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation != r.generation || providerGeneration != r.providerGeneration[key] {
		return
	}
	if transient {
		r.backoff[storeKey] = backoffState{fails: failures, until: r.now().Add(backoffDelay(failures))}
	} else {
		delete(r.backoff, storeKey)
	}
	r.setStoredLocked(i, profile)
}

func stillInactive(list []StoredProfile, sp StoredProfile) bool {
	for _, current := range list {
		if current.Tool == sp.Tool && current.Name == sp.Name {
			return !current.Active
		}
	}
	return false
}

// fetchStoredProfile sends one usage GET with a stored profile's access token,
// held in memory (data). A token already past its expiry is never sent.
func (r *Reader) fetchStoredProfile(ctx context.Context, i int, base Profile, data []byte) (Profile, error) {
	var login providers.Login
	var err error
	var provider providers.UsageProvider
	var name string
	if i == 0 {
		name = "Codex"
		login, err = providers.ParseCodexLogin(data)
		provider = providers.NewCodexProviderWith(providers.CodexOptions{AuthData: data, URL: r.codexURL, Client: r.client})
	} else {
		name = "Claude"
		login, err = providers.ParseClaudeLogin(data)
		provider = providers.NewClaudeCodeProviderWith(providers.ClaudeCodeOptions{AuthData: data, URL: r.claudeURL, Client: r.client})
	}
	if err != nil {
		base.Status = "auth_missing"
		base.Error = err.Error()
		return base, nil
	}
	reread := func() (providers.Login, error) { return login, nil }
	p, ferr := r.fetchLocal(ctx, base, login, name, provider, reread)
	if p.Status == "token_expired" {
		p.Error = "Stored login expired; switch to this profile and run the CLI to renew it."
		return p, errors.New(p.Error)
	}
	return p, ferr
}
