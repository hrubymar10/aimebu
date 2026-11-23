package usages

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/usages/providers"
)

const (
	// historySpan is how far back samples are kept, and historyStep the least
	// time between two samples of one series. Together they bound a series to
	// historyMaxPoints, so the file stays a few KB per profile and window.
	historySpan      = 48 * time.Hour
	historyStep      = 15 * time.Minute
	historyMaxPoints = int(historySpan/historyStep) + 1
)

// HistoryPoint is one sample of a window's used percentage: Unix seconds and
// percent used (0-100, whatever the display direction).
type HistoryPoint struct {
	T int64   `json:"t"`
	P float64 `json:"p"`
}

type historyFile struct {
	Version int                       `json:"version"`
	Series  map[string][]HistoryPoint `json:"series"`
}

func historyKey(provider, profile, window string) string {
	return provider + "\x00" + profile + "\x00" + window
}

func (r *Reader) loadHistory() {
	data, err := os.ReadFile(r.historyPath)
	if err != nil {
		return
	}
	var saved historyFile
	if json.Unmarshal(data, &saved) != nil || saved.Version != 1 {
		return
	}
	cutoff := r.now().Add(-historySpan).Unix()
	for key, points := range saved.Series {
		if kept := trimHistory(points, cutoff); len(kept) > 0 {
			r.history[key] = kept
		}
	}
}

// trimHistory drops samples older than cutoff and keeps at most
// historyMaxPoints of the newest ones, in time order.
func trimHistory(points []HistoryPoint, cutoff int64) []HistoryPoint {
	start := 0
	for start < len(points) && points[start].T < cutoff {
		start++
	}
	points = points[start:]
	if len(points) > historyMaxPoints {
		points = points[len(points)-historyMaxPoints:]
	}
	return append([]HistoryPoint(nil), points...)
}

// recordHistoryLocked samples the windows of a fresh, successful snapshot. A
// stale or failed profile adds nothing, so a gap shows as a gap rather than
// as a flat line. Callers hold r.mu.
func (r *Reader) recordHistoryLocked(provider string, profile Profile, series string) {
	if profile.Status != "ok" || profile.Stale || series == "" {
		return
	}
	now := r.now()
	cutoff := now.Add(-historySpan).Unix()
	changed := false
	for _, window := range profile.Windows {
		key := historyKey(provider, series, window.Key)
		points := r.history[key]
		if n := len(points); n > 0 && now.Unix()-points[n-1].T < int64(historyStep/time.Second) {
			continue
		}
		points = append(points, HistoryPoint{T: now.Unix(), P: percent(window.PercentUsed)})
		r.history[key] = trimHistory(points, cutoff)
		changed = true
	}
	if changed {
		r.saveHistoryLocked()
	}
}

func percent(value float64) float64 {
	switch {
	case value < 0 || value != value:
		return 0
	case value > 100:
		return 100
	}
	return value
}

func (r *Reader) saveHistoryLocked() {
	data, err := json.Marshal(historyFile{Version: 1, Series: r.history})
	if err != nil {
		return
	}
	_ = writeCache(r.historyPath, data)
}

// forgetHistoryLocked deletes every series of one profile.
func (r *Reader) forgetHistoryLocked(provider, profile string) {
	prefix := provider + "\x00" + profile + "\x00"
	removed := false
	for key := range r.history {
		if strings.HasPrefix(key, prefix) {
			delete(r.history, key)
			removed = true
		}
	}
	if removed {
		r.saveHistoryLocked()
	}
}

// attachHistory copies the stored samples onto an output profile's windows.
// The ring itself never lives in the snapshot, so cache.json stays small.
func (r *Reader) attachHistory(provider string, profile *Profile, names map[string]string) {
	series := historySeries(provider, *profile, names)
	for i := range profile.Windows {
		if points := r.history[historyKey(provider, series, profile.Windows[i].Key)]; len(points) > 0 {
			profile.Windows[i].History = append([]HistoryPoint(nil), points...)
		}
	}
}

func historyPathFor(cacheDir string) string { return filepath.Join(cacheDir, "history.json") }

// liveHistoryNames maps a switcher-managed provider ("claude-code", "codex") to
// the name of the profile whose credentials are live ("" when profiles exist
// but none matches the live login). A provider the switcher has no profiles
// for is absent, and its live account keeps the series name "local". It asks
// the switcher, so callers must not hold r.mu.
func (r *Reader) liveHistoryNames() map[string]string {
	r.mu.RLock()
	lister := r.profiles
	r.mu.RUnlock()
	if lister == nil {
		return nil
	}
	list, err := lister()
	if err != nil {
		return nil
	}
	names := map[string]string{}
	for _, sp := range list {
		provider := providers.ProviderCodex
		if sp.Tool == "claude" {
			provider = providers.ProviderClaudeCode
		}
		if _, seen := names[provider]; !seen {
			names[provider] = ""
		}
		if sp.Active {
			names[provider] = sp.Name
		}
	}
	return names
}

// historySeries is the series name a profile's samples are kept under. Every
// profile is stored under its own name; the live account, which the poller
// calls "local", is stored under its switcher profile's name so a switch never
// splits or drops a series. "" means there is no stable name to record under.
func historySeries(provider string, profile Profile, names map[string]string) string {
	if !profile.Active || profile.ProfileName != "local" {
		return profile.ProfileName
	}
	if name, managed := names[provider]; managed {
		return name
	}
	return "local"
}

// migrateLocalHistoryLocked folds every "local" series of a switcher-managed
// provider into the profile live now. Invalidate used to delete the live
// account's local series at each switch, so what remains was sampled since the
// last switch, by the profile active now. Idempotent: afterwards no such local
// series is left. Callers hold r.mu.
func (r *Reader) migrateLocalHistoryLocked(names map[string]string) {
	changed := false
	for provider, target := range names {
		for key, points := range r.history {
			prov, rest, _ := strings.Cut(key, "\x00")
			profile, window, _ := strings.Cut(rest, "\x00")
			if prov != provider || profile != "local" {
				continue
			}
			delete(r.history, key)
			changed = true
			if target == "" {
				continue
			}
			dst := historyKey(provider, target, window)
			r.history[dst] = mergeHistory(r.history[dst], points)
		}
	}
	if changed {
		r.saveHistoryLocked()
	}
}

// mergeHistory joins two series in time order, keeping the samples of a that
// are at least historyStep from the previous kept one.
func mergeHistory(a, b []HistoryPoint) []HistoryPoint {
	all := append(append([]HistoryPoint(nil), a...), b...)
	sort.SliceStable(all, func(i, j int) bool { return all[i].T < all[j].T })
	out := all[:0]
	for _, p := range all {
		if n := len(out); n > 0 && p.T-out[n-1].T < int64(historyStep/time.Second) {
			continue
		}
		out = append(out, p)
	}
	if len(out) > historyMaxPoints {
		out = out[len(out)-historyMaxPoints:]
	}
	return out
}
