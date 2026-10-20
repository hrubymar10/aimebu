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
	"github.com/hrubymar10/aimebu/internal/credtarget"
	"github.com/hrubymar10/aimebu/internal/switcher"
)

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

func newSwitcherManager() (switcherManager, error) {
	root, err := switcher.DefaultRoot()
	if err != nil {
		return nil, err
	}
	return switcher.New(root)
}

// switcherCommand implements `aimebu switcher`. It works with the server
// stopped: it only touches the switcher tree and the live credential files.
func switcherCommand(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		switcherUsage(stderr)
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "help", "-h", "--help":
		switcherUsage(stdout)
		return 0
	case "list", "status", "use", "add", "import", "rename", "remove", "enable", "disable", "migrate-copies":
	default:
		fmt.Fprintf(stderr, "Unknown switcher command: %s\n\n", sub)
		switcherUsage(stderr)
		return 2
	}
	if sub == "migrate-copies" {
		return switcherMigrateCopies(stdout, stderr, rest)
	}
	m, err := newSwitcherManager()
	if err != nil {
		fmt.Fprintf(stderr, "Error: switcher: %v\n", err)
		return 1
	}
	switch sub {
	case "list":
		err = switcherList(stdout, m, rest)
	case "status":
		err = switcherStatus(stdout, m, rest)
	case "use":
		err = switcherUse(stdin, stdout, m, rest)
	case "add":
		err = switcherAdd(stdout, m, rest)
	case "import":
		err = switcherImport(stdout, m, rest)
	case "rename":
		err = switcherRename(stdout, m, rest)
	case "remove":
		err = switcherRemove(stdin, stdout, m, rest)
	case "enable", "disable":
		if len(rest) != 0 {
			err = switcherUsageError{"usage: aimebu switcher " + sub}
			break
		}
		err = switcherSetEnabled(stdout, m, sub == "enable")
	}
	var usageErr switcherUsageError
	if errors.As(err, &usageErr) {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "Error: switcher %s: %v\n", sub, err)
		return 1
	}
	return 0
}

// switcherUsageError marks a malformed command line (exit 2) as opposed to a
// failed operation (exit 1).
type switcherUsageError struct{ msg string }

func (e switcherUsageError) Error() string { return e.msg }

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
	Enabled     bool                   `json:"enabled"`
	Eligibility []switcher.Eligibility `json:"eligibility"`
	Active      map[string]string      `json:"active"`
}

func switcherStatus(out io.Writer, m switcherManager, args []string) error {
	jsonOut, err := parseSwitcherFlags(args, "status")
	if err != nil {
		return err
	}
	enabled, err := m.Enabled()
	if err != nil {
		return switcherErrMsg(err)
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
		data, err := json.Marshal(switcherStatusJSON{Enabled: enabled, Eligibility: eligs, Active: active})
		if err != nil {
			return err
		}
		fmt.Fprintln(out, string(data))
		return nil
	}
	printSwitcherStatus(out, enabled, eligs, active)
	return nil
}

func printSwitcherStatus(out io.Writer, enabled bool, eligs []switcher.Eligibility, active map[string]string) {
	if enabled {
		fmt.Fprintln(out, "Switcher: enabled")
	} else {
		fmt.Fprintln(out, "Switcher: disabled")
	}
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
			err = switcherUsageError{fmt.Sprintf("unknown flag %s; usage: aimebu switcher %s [--json]", a, sub)}
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
				err = switcherUsageError{fmt.Sprintf("unknown flag %s", a)}
				return
			}
			positional = append(positional, a)
		}
	}
	if len(positional) != n {
		err = switcherUsageError{"usage: aimebu switcher " + switcherSubUsage(sub)}
	}
	return
}

func switcherSubUsage(sub string) string {
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

func switcherUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: aimebu switcher <command> [args]

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
  migrate-copies [--tool t]        Convert to token-only live credential files (see docs/switcher.md)

Tools: claude, codex

Flags:
  -y, --yes   Skip interactive confirmation prompts

The switcher works without the aimebu server running.
Enable it first with 'aimebu switcher enable' or in Settings → Usages.
`)
}

func emptyDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// switcherMigrateCopies implements `aimebu switcher migrate-copies [--tool t]
// [--allow-running-codex]`: convert a link-based install to the token-only
// model. The profile store keeps the only full credentials; the live ~/.claude
// and ~/.codex files become token-only copies that the server rewrites. It
// refuses, exit 2 and changing nothing, when a live token has less than 15
// minutes left.
func switcherMigrateCopies(stdout, stderr io.Writer, args []string) int {
	const usage = "usage: aimebu switcher migrate-copies [--tool claude|codex] [--allow-running-codex]"
	var tools []string
	var allowCodex bool
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--tool":
			if i+1 >= len(args) {
				fmt.Fprintln(stderr, usage)
				return 2
			}
			i++
			tools = append(tools, args[i])
		case "--allow-running-codex":
			allowCodex = true
		default:
			fmt.Fprintln(stderr, usage)
			return 2
		}
	}
	root, err := switcher.DefaultRoot()
	if err != nil {
		fmt.Fprintf(stderr, "Error: switcher: %v\n", err)
		return 1
	}
	m, err := switcher.New(root)
	if err != nil {
		fmt.Fprintf(stderr, "Error: switcher: %v\n", err)
		return 1
	}
	res, err := m.MigrateToTokenCopies(switcher.MigrateOptions{Tools: tools, AllowRunningCodex: allowCodex})
	if errors.Is(err, credtarget.ErrMigrateTokenExpiring) {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "Error: switcher migrate-copies: %v\n", err)
		return 1
	}
	if res.AlreadyMigrated {
		fmt.Fprintln(stdout, "Already migrated: the profile store holds the only full credentials.")
		return 0
	}
	if res.CodexChecked != "" {
		fmt.Fprintf(stdout, "codex: no running codex session found (checked: %s).\n", res.CodexChecked)
	}
	for _, t := range res.Tools {
		if t.Profile != "" {
			fmt.Fprintf(stdout, "%s: profile %q: %s\n", t.Tool, t.Profile, t.Status)
		} else {
			fmt.Fprintf(stdout, "%s: %s\n", t.Tool, t.Status)
		}
	}
	fmt.Fprintln(stdout, "Restart your running claude sessions to drop cached state (optional): one started before the migration may still hold a refresh token in memory and write a full login back into the live file (the server imports such a login).")
	fmt.Fprintln(stdout, "Done. Start or restart the aimebu server: it refreshes due profiles right away and keeps the live credential files up to date.")
	return 0
}
