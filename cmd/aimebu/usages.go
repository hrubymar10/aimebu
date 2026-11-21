package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/switcher"
	"github.com/hrubymar10/aimebu/v2/internal/usages"
	"github.com/hrubymar10/aimebu/v2/internal/usages/providers"
)

const usagesUsage = "usage: aimebu usages [provider] [--json|--plain]"

// usagesCommand prints the usage snapshots the running server last cached.
// It reads usages/cache.json (and the saved usages config for enablement and
// order) straight from the config root, the way `sessions` reads SQLite: the
// v2 HTTP API needs a login session, and polling or refreshing from the CLI
// would duplicate the server's requests. It makes no requests and writes
// nothing (the switcher is opened read-only). The only credential files it
// reads are the switcher profiles', to say whether a login exists, when it
// expires and which account it is; tokens are never printed. It works with the
// server stopped and shows only what was cached.
func usagesCommand(args []string, stdout, stderr io.Writer) int {
	format, provider := "plain", ""
	chosen := ""
	for _, arg := range args {
		switch arg {
		case "--plain", "--json":
			if chosen != "" && chosen != arg {
				fmt.Fprintln(stderr, "--plain and --json are mutually exclusive")
				return 2
			}
			chosen, format = arg, strings.TrimPrefix(arg, "--")
		default:
			if strings.HasPrefix(arg, "-") {
				fmt.Fprintf(stderr, "unknown flag %s\n%s\n", arg, usagesUsage)
				return 2
			}
			if provider != "" {
				fmt.Fprintf(stderr, "expected at most one provider\n%s\n", usagesUsage)
				return 2
			}
			provider = arg
		}
	}
	if provider != "" && !providers.KnownProvider(provider) {
		fmt.Fprintf(stderr, "unknown provider %q (allowed: %s)\n", provider, strings.Join(providers.KnownProviders(), ", "))
		return 2
	}
	root, err := sessionsConfigRoot()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	resp, written, err := usages.ReadCache(filepath.Join(root, "usages"))
	if err != nil {
		fmt.Fprintf(stderr, "warn: usages config: %v\n", err)
	}
	if mgr, mgrErr := switcher.NewReadOnly(filepath.Join(root, "switcher")); mgrErr == nil {
		if err := mgr.OverlayUsages(&resp); err != nil {
			fmt.Fprintf(stderr, "warn: switcher profiles: %v\n", err)
		}
	}
	if provider != "" {
		kept := resp.Providers[:0]
		for _, p := range resp.Providers {
			if p.ProviderName == provider {
				kept = append(kept, p)
			}
		}
		resp.Providers = kept
	}
	if written.IsZero() {
		fmt.Fprintln(stderr, "note: no usage snapshot cached yet; start the server to collect one")
	} else if age := time.Since(written); age > 2*time.Duration(resp.Settings.RefreshIntervalSec)*time.Second {
		fmt.Fprintf(stderr, "note: snapshot is %s old; the server may not be running\n", formatCLIDuration(age))
	}
	if format == "json" {
		data, err := json.Marshal(resp)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, string(data))
		return 0
	}
	printUsagesPlain(stdout, resp)
	return 0
}

func printUsagesPlain(w io.Writer, resp usages.Response) {
	hasData := false
	for _, prov := range resp.Providers {
		if prov.Enabled && len(prov.Profiles) > 0 {
			hasData = true
			break
		}
	}
	if !hasData {
		fmt.Fprintln(w, "No usage providers enabled.")
		return
	}
	// Windows come last and are never cut: each one gets its own line, the
	// later ones under blank leading cells.
	const rowFormat = "%-14s %-14s %-20s %-32s %-16s %s\n"
	fmt.Fprintf(w, rowFormat, "PROVIDER", "PROFILE", "STATUS", "PLAN", "CREDITS", "WINDOWS")
	for _, prov := range resp.Providers {
		if !prov.Enabled {
			continue
		}
		for _, s := range prov.Profiles {
			windows := make([]string, 0, len(s.Windows))
			for _, win := range s.Windows {
				cell := fmt.Sprintf("%s=%.0f%%", win.Key, win.PercentUsed)
				if win.Pace != nil {
					cell += " (" + paceCLIText(win.Pace) + ")"
				}
				windows = append(windows, cell)
			}
			if len(windows) == 0 {
				windows = []string{"-"}
			}
			credits := "-"
			if s.Credits != nil {
				credits = fmt.Sprintf("%.2f", s.Credits.Balance)
				if s.Credits.SpendLimit > 0 {
					credits = fmt.Sprintf("%.2f/%.2f", s.Credits.Balance, s.Credits.SpendLimit)
				}
			}
			plan := s.Plan
			if plan == "" {
				plan = "-"
			}
			status := s.Status
			if s.Stale {
				status += " (stale)"
			}
			profile := s.ProfileName
			if profile == "" {
				profile = "local"
			}
			fmt.Fprintf(w, rowFormat,
				plainCell(prov.Label, 14),
				plainCell(profile, 14),
				plainCell(status, 20),
				plainCell(plan, 32),
				plainCell(credits, 16),
				windows[0],
			)
			for _, extra := range windows[1:] {
				fmt.Fprintf(w, rowFormat, "", "", "", "", "", extra)
			}
		}
	}
}

func paceCLIText(p *usages.Pace) string {
	absDelta := p.DeltaPercent
	if absDelta < 0 {
		absDelta = -absDelta
	}
	var label string
	switch p.State {
	case providers.PaceStateReserve:
		label = fmt.Sprintf("%.0f%% reserve", absDelta)
	case providers.PaceStateDeficit:
		label = fmt.Sprintf("%.0f%% deficit", absDelta)
	default:
		label = "on track"
	}
	if p.LastsToReset {
		return label + " · lasts to reset"
	}
	if p.EtaSeconds != nil {
		return label + " · runs out in " + formatCLIDuration(time.Duration(*p.EtaSeconds)*time.Second)
	}
	return label
}

func formatCLIDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	if days > 0 {
		return fmt.Sprintf("%dd %dh", days, hours)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}

func plainCell(value string, width int) string {
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	if width <= 3 {
		return string(runes[:width])
	}
	return string(runes[:width-3]) + "..."
}
