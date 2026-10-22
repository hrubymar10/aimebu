package usages

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"
)

// UsageAlert is a global inbox notice. It contains no credentials or email.
type UsageAlert struct {
	ID        string     `json:"id"`
	Provider  string     `json:"provider"`
	Profile   string     `json:"profile"`
	Window    string     `json:"window"`
	Tier      string     `json:"tier"`
	Remaining float64    `json:"remaining"`
	ResetAt   *time.Time `json:"reset_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	Open      bool       `json:"open"`
	BaseKey   string     `json:"base_key"`
}

type alertWindow struct {
	Cycle       string    `json:"cycle"`
	ResetAt     time.Time `json:"reset_at,omitempty"`
	Credential  string    `json:"credential,omitempty"`
	Generation  int       `json:"generation"`
	LastUsed    float64   `json:"last_used"`
	LastRefresh time.Time `json:"last_refresh"`
	Warning     bool      `json:"warning"`
	Critical    bool      `json:"critical"`
}

type alertState struct {
	Windows map[string]alertWindow `json:"windows"`
	Alerts  map[string]UsageAlert  `json:"alerts"`
}

// AlertTracker records a threshold once per account, window and reset cycle.
// The state is written before a new notice is exposed to the Inbox.
type AlertTracker struct {
	mu    sync.Mutex
	path  string
	state alertState
	clock func() time.Time
}

// NewAlertTracker uses the wall clock unless now is supplied by a caller that
// needs to observe a specific reset cycle.
func NewAlertTracker(root string, now ...func() time.Time) (*AlertTracker, error) {
	clock := time.Now
	if len(now) > 0 && now[0] != nil {
		clock = now[0]
	}
	t := &AlertTracker{path: filepath.Join(root, "alerts.json"), clock: clock,
		state: alertState{Windows: map[string]alertWindow{}, Alerts: map[string]UsageAlert{}}}
	data, err := os.ReadFile(t.path)
	if errors.Is(err, os.ErrNotExist) {
		return t, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &t.state); err != nil {
		t.recoverCorrupt(err)
		return t, nil
	}
	if t.state.Windows == nil || t.state.Alerts == nil {
		t.recoverCorrupt(errors.New("missing state"))
	}
	return t, nil
}

func (t *AlertTracker) recoverCorrupt(reason error) {
	backup := fmt.Sprintf("%s.corrupt-%d", t.path, time.Now().UTC().UnixNano())
	if err := os.Rename(t.path, backup); err != nil {
		log.Printf("usage alerts: corrupt state (%v); could not move aside: %v", reason, err)
	} else {
		log.Printf("usage alerts: corrupt state (%v); moved aside to %s", reason, backup)
	}
	t.state = alertState{Windows: map[string]alertWindow{}, Alerts: map[string]UsageAlert{}}
}

func (t *AlertTracker) Observe(snapshot Response) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	before := alertState{Windows: make(map[string]alertWindow, len(t.state.Windows)), Alerts: make(map[string]UsageAlert, len(t.state.Alerts))}
	for key, window := range t.state.Windows {
		before.Windows[key] = window
	}
	for id, alert := range t.state.Alerts {
		before.Alerts[id] = alert
	}
	warn, critical := snapshot.Settings.AlertWarningRemaining, snapshot.Settings.AlertCriticalRemaining
	if warn == 0 && critical == 0 {
		warn, critical = 20, 10
	}
	if critical < 1 || warn > 99 || critical >= warn {
		return false, errors.New("invalid usage alert thresholds")
	}
	changed, visibleChanged := false, false
	liveBases := map[string]string{}
	for _, provider := range snapshot.Providers {
		if !provider.Enabled || !provider.Available {
			continue
		}
		for _, profile := range provider.Profiles {
			account := profile.AlertIdentity
			if account == "" {
				if profile.ProfileName == "local" {
					account = "local"
				} else if email := strings.ToLower(strings.TrimSpace(profile.Email)); email != "" {
					account = "email\x00" + email
				}
			}
			if account == "" {
				// A named account without an identity must not inherit a different
				// account's alert state when its name is reused.
				continue
			}
			for _, window := range profile.Windows {
				base := alertHash(provider.ProviderName + "\x00" + account + "\x00" + window.Key)
				liveBases[base] = profile.ProfileName
			}
			if profile.Stale || profile.Status != "ok" || profile.LastRefreshAt == nil {
				continue
			}
			for _, window := range profile.Windows {
				if math.IsNaN(window.PercentUsed) || math.IsInf(window.PercentUsed, 0) {
					continue
				}
				base := alertHash(provider.ProviderName + "\x00" + account + "\x00" + window.Key)
				state := t.state.Windows[base]
				if state.Cycle != "" && profile.AlertCredential != "" && state.Credential != profile.AlertCredential {
					for id, alert := range t.state.Alerts {
						if alert.BaseKey == base && alert.Open {
							alert.Open = false
							t.state.Alerts[id] = alert
							visibleChanged = true
						}
					}
					state = alertWindow{}
					changed = true
				}
				if !profile.LastRefreshAt.After(state.LastRefresh) {
					continue
				}
				cycle := ""
				if window.ResetAt != nil {
					cycle = window.ResetAt.UTC().Truncate(time.Minute).Format(time.RFC3339)
					previous := state.ResetAt
					if previous.IsZero() {
						previous, _ = time.Parse(time.RFC3339Nano, state.Cycle)
					}
					if !previous.IsZero() {
						delta := window.ResetAt.Sub(previous)
						if delta >= -2*time.Minute && delta <= 2*time.Minute {
							cycle = state.Cycle
						}
					}
				} else {
					if state.Generation == 0 {
						state.Generation = 1
					} else if state.LastUsed >= float64(100-warn) && window.PercentUsed <= float64(100-warn)/2 {
						state.Generation++
					}
					cycle = fmt.Sprintf("observed-%d", state.Generation)
				}
				cycleChanged := state.Cycle != "" && state.Cycle != cycle
				if cycleChanged {
					for id, alert := range t.state.Alerts {
						if alert.BaseKey == base && alert.Open {
							alert.Open = false
							t.state.Alerts[id] = alert
							visibleChanged = true
						}
					}
					state.Warning, state.Critical = false, false
				}
				state.Cycle, state.Credential, state.LastUsed, state.LastRefresh = cycle, profile.AlertCredential, window.PercentUsed, *profile.LastRefreshAt
				if window.ResetAt != nil && (cycleChanged || state.ResetAt.IsZero()) {
					state.ResetAt = *window.ResetAt
				} else if window.ResetAt == nil {
					state.ResetAt = time.Time{}
				}
				t.state.Windows[base] = state
				changed = true
				remaining := math.Max(0, math.Min(100, 100-window.PercentUsed))
				if remaining <= float64(critical) && !state.Critical {
					state.Warning, state.Critical = true, true
					for id, alert := range t.state.Alerts {
						if alert.BaseKey == base && alert.Open && alert.Tier == "warning" {
							alert.Open = false
							t.state.Alerts[id] = alert
							visibleChanged = true
						}
					}
					t.addAlert(base, cycle, profile.AlertCredential, provider.ProviderName, profile.ProfileName, window, "critical", remaining)
					visibleChanged = true
				} else if remaining <= float64(warn) && !state.Warning {
					state.Warning = true
					t.addAlert(base, cycle, profile.AlertCredential, provider.ProviderName, profile.ProfileName, window, "warning", remaining)
					visibleChanged = true
				}
				t.state.Windows[base] = state
			}
		}
	}
	for id, alert := range t.state.Alerts {
		if !alert.Open {
			continue
		}
		name, present := liveBases[alert.BaseKey]
		if !present || alert.ResetAt != nil && !t.clock().Before(*alert.ResetAt) {
			alert.Open = false
			visibleChanged, changed = true, true
		} else if name != alert.Profile {
			alert.Profile = name
			visibleChanged, changed = true, true
		}
		t.state.Alerts[id] = alert
	}
	if changed {
		for id, alert := range t.state.Alerts {
			if !alert.Open {
				delete(t.state.Alerts, id)
			}
		}
		if err := t.save(); err != nil {
			t.state = before
			return false, err
		}
	}
	return visibleChanged, nil
}

var ErrAlertNotFound = errors.New("usage alert not found")

// Dismiss removes an open notice without resetting its per-cycle threshold.
func (t *AlertTracker) Dismiss(id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	alert, ok := t.state.Alerts[id]
	if !ok || !alert.Open {
		return ErrAlertNotFound
	}
	delete(t.state.Alerts, id)
	if err := t.save(); err != nil {
		t.state.Alerts[id] = alert
		return err
	}
	return nil
}

func (t *AlertTracker) addAlert(base, cycle, credential, provider, profile string, window Window, tier string, remaining float64) {
	id := "usage_alert_" + alertHash(base + "\x00" + cycle + "\x00" + credential + "\x00" + tier)[:24]
	var resetAt *time.Time
	if window.ResetAt != nil {
		value := *window.ResetAt
		resetAt = &value
	}
	t.state.Alerts[id] = UsageAlert{ID: id, Provider: provider, Profile: profile, Window: window.Key,
		Tier: tier, Remaining: remaining, ResetAt: resetAt, CreatedAt: t.clock().UTC(), Open: true, BaseKey: base}
}

func alertHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (t *AlertTracker) OpenAlerts() []UsageAlert {
	t.mu.Lock()
	defer t.mu.Unlock()
	alerts := []UsageAlert{}
	for _, alert := range t.state.Alerts {
		if alert.Open && (alert.ResetAt == nil || t.clock().Before(*alert.ResetAt)) {
			alerts = append(alerts, alert)
		}
	}
	sort.Slice(alerts, func(i, j int) bool {
		return alerts[i].CreatedAt.Before(alerts[j].CreatedAt) || alerts[i].CreatedAt.Equal(alerts[j].CreatedAt) && alerts[i].ID < alerts[j].ID
	})
	return alerts
}

func (t *AlertTracker) save() error {
	if err := os.MkdirAll(filepath.Dir(t.path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(t.state, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(t.path), ".alerts-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), t.path)
}
