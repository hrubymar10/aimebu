package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/usages"
)

func usagesCmd(args []string) {
	format := ""
	provider := ""
	for _, arg := range args {
		switch arg {
		case "--plain":
			if format != "" && format != "plain" {
				fatal("usages", fmt.Errorf("--plain and --json are mutually exclusive"))
			}
			format = "plain"
		case "--json":
			if format != "" && format != "json" {
				fatal("usages", fmt.Errorf("--plain and --json are mutually exclusive"))
			}
			format = "json"
		default:
			if strings.HasPrefix(arg, "-") {
				fatal("usages", fmt.Errorf("unknown flag %s", arg))
			}
			if provider != "" {
				fatal("usages", fmt.Errorf("expected at most one provider"))
			}
			provider = arg
		}
	}
	if format == "" {
		format = "plain"
	}
	if provider != "" && !usages.KnownProvider(provider) {
		fatal("usages", fmt.Errorf("unknown provider %q (allowed: %s)", provider, strings.Join(usages.KnownProviders(), ", ")))
	}

	manager := usages.NewManager(usages.NewStore(), usages.DefaultRegistry())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := manager.Snapshot(ctx, provider)
	if err != nil {
		fatal("usages", err)
	}

	if format == "json" {
		data, err := json.Marshal(resp)
		if err != nil {
			fatal("usages", err)
		}
		fmt.Println(string(data))
		return
	}
	printUsagesPlain(resp)
}

func printUsagesPlain(resp usages.Response) {
	if len(resp.Snapshots) == 0 {
		fmt.Println("No usage providers enabled.")
		return
	}
	const rowFormat = "%-18s %-20s %-32s %-34s %-16s\n"
	keys := make([]string, 0, len(resp.Snapshots))
	for key := range resp.Snapshots {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	fmt.Printf(rowFormat, "PROVIDER", "STATUS", "PLAN", "WINDOWS", "CREDITS")
	for _, key := range keys {
		s := resp.Snapshots[key]
		windows := make([]string, 0, len(s.Windows))
		for _, w := range s.Windows {
			cell := fmt.Sprintf("%s=%.0f%%", w.Key, w.PercentUsed)
			if w.Pace != nil {
				cell += " (" + paceCLIText(w.Pace) + ")"
			}
			windows = append(windows, cell)
		}
		windowsText := "-"
		if len(windows) > 0 {
			windowsText = strings.Join(windows, ", ")
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
		status := string(s.Status)
		if s.Stale {
			status += " (stale)"
		}
		fmt.Printf(rowFormat,
			plainCell(key, 18),
			plainCell(status, 20),
			plainCell(plan, 32),
			plainCell(windowsText, 34),
			plainCell(credits, 16),
		)
	}
}

func paceCLIText(p *usages.Pace) string {
	absDelta := p.DeltaPercent
	if absDelta < 0 {
		absDelta = -absDelta
	}
	var label string
	switch p.State {
	case usages.PaceStateReserve:
		label = fmt.Sprintf("%.0f%% reserve", absDelta)
	case usages.PaceStateDeficit:
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
	if len(value) <= width {
		return value
	}
	if width <= 3 {
		return value[:width]
	}
	return value[:width-3] + "..."
}
