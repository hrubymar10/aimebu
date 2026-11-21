package usages

import (
	"context"
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
	"github.com/hrubymar10/aimebu/internal/telemetry"
	"github.com/hrubymar10/aimebu/internal/usages/providers"
)

// UsageAlert is a global inbox notice. It contains no credentials or email.
// Tier is warning, critical, pace (a window projected to run out before its
// reset), reset (a window reset notice) or expiry (an expiring or expired
// credential). The pace, reset and expiry tiers exist only while their opt-in
// setting is on.
type UsageAlert struct {
	ID        string     `json:"id"`
	Provider  string     `json:"provider"`
	Profile   string     `json:"profile"`
	Window    string     `json:"window"`
	Tier      string     `json:"tier"`
	Remaining float64    `json:"remaining"`
	ResetAt   *time.Time `json:"reset_at,omitempty"`
	// ExpiresAt is the credential expiry of an expiry-tier alert. It is kept
	// apart from ResetAt, whose passing closes an alert: an expired credential
	// is exactly when its alert must stay visible.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
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
	// Pace records that a pace alert is open for this window and cycle; it
	// clears on recovery (the window is again projected to last until its
	// reset), so the alert can re-arm only after that.
	Pace bool `json:"pace,omitempty"`
}

type alertState struct {
	Windows map[string]alertWindow `json:"windows"`
	Alerts  map[string]UsageAlert  `json:"alerts"`
	// Expiry remembers what each account has already been told about its
	// credential: `<expiry>@<announced level>` keyed by the hashed provider
	// and account. A dismissal keeps this record, so the same level is not
	// raised again; only a stronger level (expiring became expired) or a new
	// expiry time raises a fresh notice. It is cleared when the credential
	// is renewed or the alert setting goes off.
	Expiry map[string]string `json:"expiry,omitempty"`
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
		state: alertState{Windows: map[string]alertWindow{}, Alerts: map[string]UsageAlert{}, Expiry: map[string]string{}}}
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
	if t.state.Windows == nil || t.state.Alerts == nil || t.state.Expiry == nil {
		t.recoverCorrupt(errors.New("missing state"))
	}
	return t, nil
}

func (t *AlertTracker) recoverCorrupt(reason error) {
	backup := fmt.Sprintf("%s.corrupt-%d", t.path, time.Now().UTC().UnixNano())
	if err := os.Rename(t.path, backup); err != nil {
		log.Printf("usage alerts: corrupt state (%v); could not move aside: %v", reason, err)
		telemetry.ReportCause(context.Background(), "credentials", "usage alerts: the corrupt state file could not be moved aside")
	} else {
		log.Printf("usage alerts: corrupt state (%v); moved aside to %s", reason, backup)
		telemetry.WarnCause(context.Background(), "credentials", "usage alerts: the corrupt state file was moved aside")
	}
	t.state = alertState{Windows: map[string]alertWindow{}, Alerts: map[string]UsageAlert{}, Expiry: map[string]string{}}
}

func (t *AlertTracker) Observe(snapshot Response) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	before := alertState{Windows: make(map[string]alertWindow, len(t.state.Windows)), Alerts: make(map[string]UsageAlert, len(t.state.Alerts)), Expiry: make(map[string]string, len(t.state.Expiry))}
	for key, window := range t.state.Windows {
		before.Windows[key] = window
	}
	for id, alert := range t.state.Alerts {
		before.Alerts[id] = alert
	}
	for key, stamp := range t.state.Expiry {
		before.Expiry[key] = stamp
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
	liveAccounts := map[string]string{}
	paceOn, resetOn, expiryOn := snapshot.Settings.AlertPaceEnabled, snapshot.Settings.AlertResetNoticeEnabled, snapshot.Settings.AlertExpiryEnabled
	keptExpiry := map[string]string{}
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
			liveAccounts[expiryBaseKey(provider.ProviderName, account)] = profile.ProfileName
			// The credential-expiry alert: the same expiry data that drives the
			// Usages token-expiry badge. It fires once per account and expiry
			// time, and a renewed credential (healthy again, or absent) closes
			// the notice and lets a later decay raise a fresh one.
			expiryBase := expiryBaseKey(provider.ProviderName, account)
			stamp, announced, seen := expiryRecord(t.state.Expiry[expiryBase])
			if expiryOn && profile.ExpiresAt != nil {
				if level, _ := credentialLevel(profile.ExpiresAt, t.clock()); level == TargetLevelExpiring || level == TargetLevelExpired {
					stamped := expiryStamp(profile.ExpiresAt.UTC().Format(time.RFC3339), level)
					if stamp != expiryOf(stamped) {
						if seen {
							t.closeExpiry(expiryBase) // the old expiry's notice is obsolete
						}
						t.state.Expiry[expiryBase] = stamped
						t.addExpiryAlert(expiryBase, provider.ProviderName, profile.ProfileName, *profile.ExpiresAt, level)
						visibleChanged, changed = true, true
					} else if level != announced && levelIsStronger(level, announced) && openTier(t.state.Alerts, expiryBase) != level {
						// Same expiry time, already announced at a weaker
						// level (expiring became expired): replace the
						// wording, unless the stronger one is already open.
						t.state.Expiry[expiryBase] = stamped
						t.closeExpiry(expiryBase)
						t.addExpiryAlert(expiryBase, provider.ProviderName, profile.ProfileName, *profile.ExpiresAt, level)
						visibleChanged, changed = true, true
					}
				} else if seen {
					delete(t.state.Expiry, expiryBase)
					t.closeExpiry(expiryBase)
					visibleChanged, changed = true, true
				}
			} else if seen {
				delete(t.state.Expiry, expiryBase)
				t.closeExpiry(expiryBase)
				visibleChanged, changed = true, true
			}
			if s, seen := t.state.Expiry[expiryBase]; seen {
				keptExpiry[expiryBase] = s
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
					state.Warning, state.Critical, state.Pace = false, false, false
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
				// The predictive pace alert: one notice per risk episode while
				// a session or weekly window is projected to run out before
				// its reset. It re-arms only after recovery, when the window
				// is again projected to last until the reset.
				switch {
				case paceOn && paceAlertWindow(provider.ProviderName, window.Key) && window.Pace != nil && window.Pace.EtaSeconds != nil:
					if !state.Pace {
						state.Pace = true
						t.addAlert(base, cycle, profile.AlertCredential, provider.ProviderName, profile.ProfileName, window, "pace", remaining)
						visibleChanged = true
					}
				case state.Pace && window.Pace != nil && window.Pace.EtaSeconds == nil:
					// Recovery is observed inline: the window is again
					// projected to last until its reset.
					t.closeTier(base, "pace")
					state.Pace = false
					visibleChanged, changed = true, true
				}
				// The reset notice: one note per window and reset cycle, when
				// a new cycle is observed. The next cycle's observation closes
				// it again.
				if resetOn && cycleChanged && window.ResetAt != nil {
					t.addAlert(base, cycle, profile.AlertCredential, provider.ProviderName, profile.ProfileName, window, "reset", remaining)
					visibleChanged = true
				}
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
	// A toggle turned off closes what it had opened, so the opt-in alerts
	// never outlive their setting. The pace toggle also clears the episode
	// state of every tracked window, so re-enabling during an ongoing
	// episode can raise a fresh notice.
	if !paceOn {
		for id, alert := range t.state.Alerts {
			if alert.Open && alert.Tier == "pace" {
				alert.Open = false
				t.state.Alerts[id] = alert
				visibleChanged, changed = true, true
			}
		}
		for key, window := range t.state.Windows {
			if window.Pace {
				window.Pace = false
				t.state.Windows[key] = window
				changed = true
			}
		}
	}
	if !resetOn {
		for id, alert := range t.state.Alerts {
			if alert.Open && alert.Tier == "reset" {
				alert.Open = false
				t.state.Alerts[id] = alert
				visibleChanged, changed = true, true
			}
		}
	}
	t.state.Expiry = keptExpiry
	for id, alert := range t.state.Alerts {
		if !alert.Open {
			continue
		}
		name, present := "", false
		if alert.Tier == "expiry" || alert.Tier == "expiring" || alert.Tier == "expired" {
			name, present = liveAccounts[alert.BaseKey]
		} else {
			name, present = liveBases[alert.BaseKey]
		}
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
			if t.state.Expiry == nil { // before.Expiry is always a map, but a state restored from disk without one stays nil
				t.state.Expiry = map[string]string{}
			}
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

// addExpiryAlert raises the credential-expiry notice: one per credential and
// expiry time. It carries no reset time, so it stays open until the
// credential is renewed, the account disappears or it is dismissed.
// addExpiryAlert raises the credential-expiry notice: one per credential and
// expiry time. It carries no reset time, so it stays open until the
// credential is renewed, the account disappears or it is dismissed.
func (t *AlertTracker) addExpiryAlert(base, provider, profile string, expires time.Time, level string) {
	id := "usage_alert_" + alertHash(base + "\x00" + expires.UTC().Format(time.RFC3339))[:24]
	expiresAt := expires
	t.state.Alerts[id] = UsageAlert{ID: id, Provider: provider, Profile: profile, Tier: level,
		ExpiresAt: &expiresAt, CreatedAt: t.clock().UTC(), Open: true, BaseKey: base}
}

// closeTier closes every open alert of one base and tier.
func (t *AlertTracker) closeTier(base, tier string) {
	for id, alert := range t.state.Alerts {
		if alert.BaseKey == base && alert.Open && alert.Tier == tier {
			alert.Open = false
			t.state.Alerts[id] = alert
		}
	}
}

// closeExpiry closes every open expiry notice of one base, whatever its
// wording (expiry, expiring, expired).
func (t *AlertTracker) closeExpiry(base string) {
	for id, alert := range t.state.Alerts {
		if alert.BaseKey == base && alert.Open && (alert.Tier == "expiry" || alert.Tier == "expiring" || alert.Tier == "expired") {
			alert.Open = false
			t.state.Alerts[id] = alert
		}
	}
}
func openTier(alerts map[string]UsageAlert, base string) string {
	for _, alert := range alerts {
		if alert.BaseKey == base && alert.Open && (alert.Tier == "expiry" || alert.Tier == "expiring" || alert.Tier == "expired") {
			return alert.Tier
		}
	}
	return ""
}

// paceAlertWindow says whether a window's pace can raise the predictive pace
// alert: the session and weekly windows of Codex and Claude Code, the two
// tools whose limits the account switcher also watches.
func paceAlertWindow(provider, key string) bool {
	if provider != providers.ProviderCodex && provider != providers.ProviderClaudeCode {
		return false
	}
	return key == "session" || key == "weekly"
}

func expiryBaseKey(provider, account string) string {
	return "expiry\x00" + alertHash("account\x00"+provider+"\x00"+account)
}

// The Expiry record is `<expiry>@<announced level>`: the notice's expiry and
// the level it was announced at, so a dismissal survives the next observation
// while an escalation still re-raises.
func expiryStamp(stamp, level string) string { return stamp + "@" + level }
func expiryOf(record string) string          { value, _, _ := expiryRecord(record); return value }
func expiryRecord(record string) (stamp, level string, ok bool) {
	value, seen := strings.CutSuffix(record, "@"+TargetLevelExpired)
	if seen {
		return value, TargetLevelExpired, true
	}
	value, seen = strings.CutSuffix(record, "@"+TargetLevelExpiring)
	if seen {
		return value, TargetLevelExpiring, true
	}
	return record, "", false // a pre-follow-up record: its stamp is the whole value
}

// levelIsStronger says whether got escalates from was: expiring became
// expired is the only escalation the credential levels have.
func levelIsStronger(got, was string) bool {
	return got == TargetLevelExpired && was == TargetLevelExpiring
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
