package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/switcher"
)

// ── Interface ────────────────────────────────────────────────────────

// switcherManager is the subset of *switcher.Manager the CLI uses.
// *switcher.Manager satisfies this interface directly.
type switcherManager interface {
	Enabled() (bool, error)
	SetEnabled(v bool) error
	Eligibility() ([]switcher.Eligibility, error)
	List() ([]switcher.Profile, error)
	Active(tool string) (string, error)
	Add(tool, name string) error
	Import(tool, name string) error
	Rename(tool, oldName, newName string) error
	Remove(tool, name string, force bool) error
	Switch(tool, name string) (string, bool, error)
}

// ── Factory ──────────────────────────────────────────────────────────

func newSwitcherManager() (switcherManager, error) {
	root, err := switcher.DefaultRoot()
	if err != nil {
		return nil, err
	}
	return switcher.New(root)
}

// ── Entry point ──────────────────────────────────────────────────────

func switcherCmd(args []string) {
	if len(args) == 0 {
		switcherUsage(0)
	}

	sub := args[0]
	rest := args[1:]

	switch sub {
	case "list", "status", "use", "add", "import", "rename", "remove", "enable", "disable":
		// handled below
	case "help", "-h", "--help":
		switcherUsage(0)
	default:
		fmt.Fprintf(os.Stderr, "Unknown switcher command: %s\n\n", sub)
		switcherUsage(1)
	}

	m, err := newSwitcherManager()
	if err != nil {
		fatal("switcher", err)
	}

	var cmdErr error
	switch sub {
	case "list":
		cmdErr = switcherList(os.Stdout, m, rest)
	case "status":
		cmdErr = switcherStatus(os.Stdout, m, rest)
	case "use":
		cmdErr = switcherUse(os.Stdin, os.Stdout, m, rest)
	case "add":
		cmdErr = switcherAdd(os.Stdout, m, rest)
	case "import":
		cmdErr = switcherImport(os.Stdout, m, rest)
	case "rename":
		cmdErr = switcherRename(os.Stdout, m, rest)
	case "remove":
		cmdErr = switcherRemove(os.Stdin, os.Stdout, m, rest)
	case "enable":
		cmdErr = switcherSetEnabled(os.Stdout, m, true)
	case "disable":
		cmdErr = switcherSetEnabled(os.Stdout, m, false)
	}
	if cmdErr != nil {
		fatal("switcher "+sub, cmdErr)
	}
}

// ── enable / disable ─────────────────────────────────────────────────

func switcherSetEnabled(out io.Writer, m switcherManager, enabled bool) error {
	if err := m.SetEnabled(enabled); err != nil {
		return err
	}
	if enabled {
		fmt.Fprintln(out, "Switcher enabled.")
	} else {
		fmt.Fprintln(out, "Switcher disabled.")
	}
	return nil
}

// ── list ─────────────────────────────────────────────────────────────

type switcherListJSON struct {
	Profiles []switcher.Profile `json:"profiles"`
}

func switcherList(out io.Writer, m switcherManager, args []string) error {
	jsonOut, err := parseSwitcherFlags(args, "list")
	if err != nil {
		return err
	}
	profiles, err := m.List()
	if err != nil {
		return switcherErrMsg(err)
	}
	if jsonOut {
		data, err := json.Marshal(switcherListJSON{Profiles: profiles})
		if err != nil {
			return err
		}
		fmt.Fprintln(out, string(data))
		return nil
	}
	printSwitcherList(out, profiles)
	return nil
}

func printSwitcherList(out io.Writer, profiles []switcher.Profile) {
	if len(profiles) == 0 {
		fmt.Fprintln(out, "No profiles found. Run 'aimebu switcher import <tool> <name>' to get started.")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TOOL\tPROFILE\tACTIVE\tCREDS\tEMAIL")
	for _, p := range profiles {
		active := "-"
		if p.Active {
			active = "*"
		}
		creds := "yes"
		if !p.HasCreds {
			creds = "no"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.Tool, p.Name, active, creds, emptyDash(p.Email))
	}
	_ = tw.Flush()
}

// ── status ───────────────────────────────────────────────────────────

type switcherStatusJSON struct {
	Eligibility []switcher.Eligibility `json:"eligibility"`
	Active      map[string]string      `json:"active"`
}

func switcherStatus(out io.Writer, m switcherManager, args []string) error {
	jsonOut, err := parseSwitcherFlags(args, "status")
	if err != nil {
		return err
	}
	eligs, err := m.Eligibility()
	if err != nil {
		return switcherErrMsg(err)
	}
	active := make(map[string]string, len(eligs))
	for _, e := range eligs {
		name, err := m.Active(e.Tool)
		if err != nil {
			return switcherErrMsg(err)
		}
		active[e.Tool] = name
	}
	if jsonOut {
		data, err := json.Marshal(switcherStatusJSON{Eligibility: eligs, Active: active})
		if err != nil {
			return err
		}
		fmt.Fprintln(out, string(data))
		return nil
	}
	printSwitcherStatus(out, eligs, active)
	return nil
}

func printSwitcherStatus(out io.Writer, eligs []switcher.Eligibility, active map[string]string) {
	if len(eligs) == 0 {
		fmt.Fprintln(out, "No tools configured.")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TOOL\tACTIVE\tELIGIBLE\tREASON")
	for _, e := range eligs {
		elig := "yes"
		reason := ""
		if !e.Eligible {
			elig = "no"
			reason = e.Reason
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.Tool, emptyDash(active[e.Tool]), elig, reason)
	}
	_ = tw.Flush()
}

// ── use ──────────────────────────────────────────────────────────────

func switcherUse(in io.Reader, out io.Writer, m switcherManager, args []string) error {
	autoYes, positional, err := parseSwitcherPositional(args, "use", 2)
	if err != nil {
		return err
	}
	tool, profile := positional[0], positional[1]

	// Pre-check: warn before switching to an empty profile (HasCreds == false).
	profiles, err := m.List()
	if err != nil {
		return switcherErrMsg(err)
	}
	for i := range profiles {
		p := &profiles[i]
		if p.Tool == tool && p.Name == profile && !p.HasCreds {
			fmt.Fprintf(out,
				"Profile %q is empty — switching will delete the live %s credentials and force a fresh login on next start.\n",
				profile, tool,
			)
			if err := switcherConfirm(in, out, "Proceed?", autoYes); err != nil {
				return err
			}
			break
		}
	}

	_, switched, err := m.Switch(tool, profile)
	if err != nil {
		return switcherErrMsg(err)
	}
	if switched {
		fmt.Fprintf(out, "Switched %s to profile %q.\n", tool, profile)
	} else {
		fmt.Fprintf(out, "Already on %s profile %q; nothing to do.\n", tool, profile)
	}
	return nil
}

// ── add ──────────────────────────────────────────────────────────────

func switcherAdd(out io.Writer, m switcherManager, args []string) error {
	_, positional, err := parseSwitcherPositional(args, "add", 2)
	if err != nil {
		return err
	}
	tool, name := positional[0], positional[1]
	if err := m.Add(tool, name); err != nil {
		return switcherErrMsg(err)
	}
	fmt.Fprintf(out, "Created empty %s profile %q.\n", tool, name)
	return nil
}

// ── import ───────────────────────────────────────────────────────────

func switcherImport(out io.Writer, m switcherManager, args []string) error {
	_, positional, err := parseSwitcherPositional(args, "import", 2)
	if err != nil {
		return err
	}
	tool, name := positional[0], positional[1]
	if err := m.Import(tool, name); err != nil {
		return switcherErrMsg(err)
	}
	fmt.Fprintf(out, "Imported current %s login as profile %q.\n", tool, name)
	return nil
}

// ── rename ───────────────────────────────────────────────────────────

func switcherRename(out io.Writer, m switcherManager, args []string) error {
	_, positional, err := parseSwitcherPositional(args, "rename", 3)
	if err != nil {
		return err
	}
	tool, oldName, newName := positional[0], positional[1], positional[2]
	if err := m.Rename(tool, oldName, newName); err != nil {
		return switcherErrMsg(err)
	}
	fmt.Fprintf(out, "Renamed %s profile %q to %q.\n", tool, oldName, newName)
	return nil
}

// ── remove ───────────────────────────────────────────────────────────

func switcherRemove(in io.Reader, out io.Writer, m switcherManager, args []string) error {
	autoYes, positional, err := parseSwitcherPositional(args, "remove", 2)
	if err != nil {
		return err
	}
	tool, name := positional[0], positional[1]

	removeErr := m.Remove(tool, name, false)
	if errors.Is(removeErr, switcher.ErrActiveProfile) {
		fmt.Fprintf(out,
			"Profile %q is currently active — removing it will clear the live %s credentials and force a fresh login on next start.\n",
			name, tool,
		)
		if err := switcherConfirm(in, out, "Proceed?", autoYes); err != nil {
			return err
		}
		removeErr = m.Remove(tool, name, true)
	}
	if removeErr != nil {
		return switcherErrMsg(removeErr)
	}
	fmt.Fprintf(out, "Removed %s profile %q.\n", tool, name)
	return nil
}

// ── Helpers ──────────────────────────────────────────────────────────

// switcherErrMsg maps sentinel errors to actionable user messages.
// Known sentinels produce fixed messages to prevent credential leakage from
// wrapped error context. Unknown errors pass through: S1 holds the invariant
// that no exported error value embeds credential content.
func switcherErrMsg(err error) error {
	switch {
	case errors.Is(err, switcher.ErrDisabled):
		return fmt.Errorf("switcher is disabled; enable it with 'aimebu switcher enable' or in Settings → Usages")
	case errors.Is(err, switcher.ErrNotEligible):
		// Use errors.As to extract the Reason field from NotEligibleError.
		// The Reason is written by S1 to be shown verbatim; it never contains credential content.
		var ne *switcher.NotEligibleError
		if errors.As(err, &ne) {
			return fmt.Errorf("%s", ne.Reason)
		}
		return fmt.Errorf("tool is not eligible for switching")
	case errors.Is(err, switcher.ErrNoActiveProfile):
		return fmt.Errorf("no active profile: run 'aimebu switcher import <tool> <name>' first — without a profile to capture into, switching would discard the current login")
	case errors.Is(err, switcher.ErrProfileNotFound):
		return fmt.Errorf("profile not found")
	case errors.Is(err, switcher.ErrProfileExists):
		return fmt.Errorf("profile already exists")
	case errors.Is(err, switcher.ErrInvalidCredentials):
		return fmt.Errorf("credentials failed validation — switch aborted before writing anything")
	case errors.Is(err, switcher.ErrActiveProfile):
		// Unreachable in normal flow: switcherRemove intercepts ErrActiveProfile before
		// calling switcherErrMsg. Present as a safety net if S1 returns it from Remove(force=true).
		return fmt.Errorf("profile is active; use -y to confirm removal")
	case errors.Is(err, switcher.ErrInvalidName):
		return fmt.Errorf("invalid profile name")
	default:
		// Pass unknown errors through. Protection belongs at the source: S1 must
		// never embed credential content in error values (see S1 invariant test).
		// OS and JSON errors (path, position) are safe and informative.
		return err
	}
}

// switcherConfirm prompts for interactive confirmation unless autoYes is set.
// Non-*os.File readers (e.g. bytes.Buffer in tests) are treated as non-TTY.
func switcherConfirm(in io.Reader, out io.Writer, prompt string, autoYes bool) error {
	if autoYes {
		return nil
	}
	if f, ok := in.(*os.File); ok {
		stat, err := f.Stat()
		if err != nil || stat.Mode()&fs.ModeCharDevice == 0 {
			return fmt.Errorf("stdin is not a terminal; use -y to skip confirmation")
		}
	} else {
		return fmt.Errorf("stdin is not a terminal; use -y to skip confirmation")
	}
	fmt.Fprintf(out, "%s [y/N]: ", prompt)
	scanner := bufio.NewScanner(in)
	scanner.Scan()
	if strings.ToLower(strings.TrimSpace(scanner.Text())) != "y" {
		return fmt.Errorf("aborted")
	}
	return nil
}

// parseSwitcherFlags parses --json for list/status subcommands.
func parseSwitcherFlags(args []string, sub string) (jsonOut bool, err error) {
	for _, a := range args {
		if a == "--json" {
			jsonOut = true
		} else {
			err = fmt.Errorf("unknown flag %s; usage: aimebu switcher %s [--json]", a, sub)
			return
		}
	}
	return
}

// parseSwitcherPositional parses -y / --yes and exactly n positional args.
func parseSwitcherPositional(args []string, sub string, n int) (autoYes bool, positional []string, err error) {
	for _, a := range args {
		switch a {
		case "-y", "--yes":
			autoYes = true
		default:
			if strings.HasPrefix(a, "-") {
				err = fmt.Errorf("unknown flag %s", a)
				return
			}
			positional = append(positional, a)
		}
	}
	if len(positional) != n {
		err = fmt.Errorf("usage: aimebu switcher %s", switcherSubUsage(sub, n))
	}
	return
}

func switcherSubUsage(sub string, _ int) string {
	switch sub {
	case "use":
		return "use <tool> <profile> [-y]"
	case "add":
		return "add <tool> <profile>"
	case "import":
		return "import <tool> <profile>"
	case "rename":
		return "rename <tool> <old> <new>"
	case "remove":
		return "remove <tool> <profile> [-y]"
	default:
		return sub
	}
}

func switcherUsage(exitCode int) {
	fmt.Fprintf(os.Stderr, `Usage: aimebu switcher <command> [args]

Commands:
  list [--json]                    List profiles across all tools
  status [--json]                  Show active profile and eligibility per tool
  use <tool> <profile> [-y]        Switch to a profile (captures live login first)
  add <tool> <profile>             Create an empty profile (no credentials)
  import <tool> <profile>          Adopt the current live login as a named profile
  rename <tool> <old> <new>        Rename a profile
  remove <tool> <profile> [-y]     Delete a profile
  enable                           Enable the switcher
  disable                          Disable the switcher

Tools: claude, codex

Flags:
  -y, --yes   Skip interactive confirmation prompts

The switcher works without the aimebu server running.
Enable it first with 'aimebu switcher enable' or in Settings → Usages.
`)
	os.Exit(exitCode)
}
