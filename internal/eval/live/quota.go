package live

import (
	"context"
	"fmt"
	"io"
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
	// ReadFn, when set, is the reading itself: the URL, the cookie and the client are not used. `aimebu self-test` sets it to read the
	// usages document of the doctor route of the local server (docs/eval.md).
	ReadFn func(ctx context.Context) (Reading, error)
}

// Reading is one read of /api/usages: the usage and, per provider, the profile that is active ("provider" -> profile name). A provider with no
// profile flagged active has no entry.
type Reading struct {
	Usage  Usage
	Active map[string]string
}

// Fetch reads the current usage.
func (q QuotaSource) Fetch(ctx context.Context) (Usage, error) {
	r, err := q.Read(ctx)
	return r.Usage, err
}

// Read reads the current usage and which profile of each provider is active.
func (q QuotaSource) Read(ctx context.Context) (Reading, error) {
	if q.ReadFn != nil {
		return q.ReadFn(ctx)
	}
	client := q.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(q.URL, "/")+"/api/usages", nil)
	if err != nil {
		return Reading{}, err
	}
	if q.CookieEnv != "" {
		req.Header.Set("Cookie", os.Getenv(q.CookieEnv))
	}
	resp, err := client.Do(req)
	if err != nil {
		return Reading{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Reading{}, fmt.Errorf("GET /api/usages answered %d", resp.StatusCode)
	}
	return ParseUsages(resp.Body)
}

// ParseUsages reads a /api/usages document (the usages of the doctor route have the same shape) into a Reading.
func ParseUsages(r io.Reader) (Reading, error) {
	var body struct {
		Providers []struct {
			Name     string `json:"provider_name"`
			Profiles []struct {
				Name    string `json:"profile_name"`
				Active  bool   `json:"active"`
				Windows []struct {
					Key  string  `json:"key"`
					Used float64 `json:"percent_used"`
				} `json:"windows"`
			} `json:"profiles"`
		} `json:"providers"`
	}
	if err := json.NewDecoder(r).Decode(&body); err != nil {
		return Reading{}, err
	}
	out := Usage{}
	active := map[string]string{}
	for _, p := range body.Providers {
		for _, profile := range p.Profiles {
			if profile.Active {
				active[p.Name] = profile.Name
			}
			for _, w := range profile.Windows {
				out[p.Name+"/"+profile.Name+"/"+w.Key] = w.Used
			}
		}
	}
	return Reading{Usage: out, Active: active}, nil
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

// OverLimitFor is OverLimit restricted to what a run can spend: only the weekly windows of the providers in tools, and of each only its
// active profile (a provider with no profile flagged active keeps all its profiles, as there is nothing to tell them apart by). A provider no
// target uses is ignored entirely.
func (u Usage) OverLimitFor(limit float64, tools map[string]bool, active map[string]string) string {
	name, high := "", -1.0
	for key, used := range u {
		parts := strings.SplitN(key, "/", 3)
		if len(parts) != 3 || !tools[parts[0]] || !isWeekly(parts[2]) {
			continue
		}
		if want, ok := active[parts[0]]; ok && parts[1] != want {
			continue
		}
		if used > high {
			name, high = key, used
		}
	}
	if high > limit {
		return fmt.Sprintf("%s is at %.0f%% (stop above %.0f%%)", name, high, limit)
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
