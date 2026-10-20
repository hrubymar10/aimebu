package usages

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goccy/go-json"
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
func (r *Reader) recordHistoryLocked(provider string, profile Profile) {
	if profile.Status != "ok" || profile.Stale {
		return
	}
	now := r.now()
	cutoff := now.Add(-historySpan).Unix()
	changed := false
	for _, window := range profile.Windows {
		key := historyKey(provider, profile.ProfileName, window.Key)
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
func (r *Reader) attachHistory(provider string, profile *Profile) {
	for i := range profile.Windows {
		if points := r.history[historyKey(provider, profile.ProfileName, profile.Windows[i].Key)]; len(points) > 0 {
			profile.Windows[i].History = append([]HistoryPoint(nil), points...)
		}
	}
}

func historyPathFor(cacheDir string) string { return filepath.Join(cacheDir, "history.json") }
