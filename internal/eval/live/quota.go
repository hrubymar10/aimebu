package live

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

// Usage is the share used of each quota window the server reports, keyed "provider/profile/window". It is read from a running server's
// GET /api/usages (docs/usages.md), which is where the repo already reads it: the runner reads no credential itself.
type Usage map[string]float64

// QuotaSource reads Usage from a server. CookieEnv names the environment variable that holds the owner's session cookie value (never a flag, so
// it is not in a process list); the cookie is sent as is.
type QuotaSource struct {
	URL       string // the server's base URL
	CookieEnv string
	Client    *http.Client
}

// Fetch reads the current usage.
func (q QuotaSource) Fetch(ctx context.Context) (Usage, error) {
	client := q.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(q.URL, "/")+"/api/usages", nil)
	if err != nil {
		return nil, err
	}
	if q.CookieEnv != "" {
		req.Header.Set("Cookie", os.Getenv(q.CookieEnv))
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /api/usages answered %d", resp.StatusCode)
	}
	var body struct {
		Providers []struct {
			Name     string `json:"provider_name"`
			Profiles []struct {
				Name    string `json:"profile_name"`
				Windows []struct {
					Key  string  `json:"key"`
					Used float64 `json:"percent_used"`
				} `json:"windows"`
			} `json:"profiles"`
		} `json:"providers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	out := Usage{}
	for _, p := range body.Providers {
		for _, profile := range p.Profiles {
			for _, w := range profile.Windows {
				out[p.Name+"/"+profile.Name+"/"+w.Key] = w.Used
			}
		}
	}
	return out, nil
}

// isWeekly says a window key names a weekly window (Claude's seven_day family, "weekly" and the scoped weekly ones).
func isWeekly(window string) bool {
	return strings.HasPrefix(window, "weekly") || strings.HasPrefix(window, "seven_day")
}

// Hottest is the weekly window with the highest share used, and that share.
func (u Usage) Hottest() (string, float64) {
	name, high := "", -1.0
	for key, used := range u {
		if isWeekly(key[strings.LastIndex(key, "/")+1:]) && used > high {
			name, high = key, used
		}
	}
	return name, high
}

// OverLimit says which weekly window is above limit percent, or "".
func (u Usage) OverLimit(limit float64) string {
	if name, used := u.Hottest(); used > limit {
		return fmt.Sprintf("%s is at %.0f%% (stop above %.0f%%)", name, used, limit)
	}
	return ""
}

// Table is the before-and-after usage as a markdown table, one row per window that exists in either reading.
func Table(before, after Usage) string {
	keys := map[string]bool{}
	for k := range before {
		keys[k] = true
	}
	for k := range after {
		keys[k] = true
	}
	var sorted []string
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	var b strings.Builder
	b.WriteString("| provider / profile / window | before | after | change |\n|---|---|---|---|\n")
	cell := func(u Usage, k string) (string, float64, bool) {
		v, ok := u[k]
		if !ok {
			return "n/a", 0, false
		}
		return fmt.Sprintf("%.1f%%", v), v, true
	}
	for _, k := range sorted {
		bs, bv, bok := cell(before, k)
		as, av, aok := cell(after, k)
		change := "n/a"
		if bok && aok {
			change = fmt.Sprintf("%+.1f points", av-bv)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", k, bs, as, change)
	}
	return b.String()
}
