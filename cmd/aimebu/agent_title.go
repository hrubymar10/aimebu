package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// agentTitleState identifies which of the three terminal-title states the
// aimebu agent wrapper is in. The title lets you tell terminal windows
// apart when several wrapped agents run side by side.
type agentTitleState int

const (
	// agentTitleStarting is the pre-registration title: "<base> (starting…)".
	agentTitleStarting agentTitleState = iota
	// agentTitleRegistered is the post-registration title:
	// "<full-agent-id> - <base>".
	agentTitleRegistered
	// agentTitleExited marks the wrapper going away: "✗ <full-agent-id> - <base>".
	agentTitleExited
)

// agentTitleLastID and agentTitleLastBase remember the identity and harness
// label from the most recent setAgentTerminalTitle call. They let a fatal
// exit (agentExitWithTerminalTitle) stamp the ✗ marker without reconstructing
// those values at each os.Exit site — including agentFatalRecovery, which does
// not have command in scope to recompute the base. Guarded by agentTitleMu
// because state 2 is written from the registration watcher goroutine while
// the exit title is read from the main goroutine.
var (
	agentTitleMu       sync.Mutex
	agentTitleLastID   string
	agentTitleLastBase string
)

// agentTerminalTitle builds the title string for a given state. It is a pure
// function so the three states and the base extraction can be unit-tested
// without touching a real terminal.
//
// The harness label is filepath.Base(command[0]) — the literal command that
// was invoked (e.g. "pi-docker"), NOT the normalized harness slug (e.g. "pi"). This
// is deliberate: "pi" and "pi-docker" must render differently.
//
// For the registered and exited states, agentID is the full agent ID
// (e.g. "drew@aimebu"). If registration never landed (agentID empty), the
// exited state degrades to "✗ <base> (starting…)" rather than emitting a
// malformed title with an empty name.
func agentTerminalTitle(state agentTitleState, agentID, base string) string {
	switch state {
	case agentTitleStarting:
		return base + " (starting…)"
	case agentTitleRegistered:
		if agentID == "" {
			return base + " (starting…)"
		}
		return agentID + " - " + base
	case agentTitleExited:
		if agentID == "" {
			return "✗ " + base + " (starting…)"
		}
		return "✗ " + agentID + " - " + base
	}
	return base
}

// agentTitleBase returns the harness label for the title: filepath.Base of
// the literal command that was invoked. Empty when no command is present.
func agentTitleBase(command []string) string {
	if len(command) == 0 {
		return ""
	}
	return filepath.Base(command[0])
}

// isStderrTerminal reports whether os.Stderr is a terminal, using stdlib
// only (os.FileInfo.Mode() & os.ModeCharDevice). No golang.org/x/term or any
// other dependency is introduced.
func isStderrTerminal() bool {
	fi, err := os.Stderr.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// writeTitleEscape emits the OSC 0 title escape (ESC ] 0 ; <title> BEL) to
// the RAW os.Stderr. It is never routed through agentStderr: agentStderr is a
// tee into agent-logs/*.stderr.log and escape bytes through it would corrupt
// those logs. If os.Stderr is not a TTY, nothing is written.
func writeTitleEscape(title string) {
	if !isStderrTerminal() {
		return
	}
	fmt.Fprintf(os.Stderr, "\033]0;%s\007", title)
}

// setAgentTerminalTitle writes the title for the given state and records the
// identity/base so a later fatal exit can stamp the ✗ marker without
// reconstructing them.
func setAgentTerminalTitle(state agentTitleState, agentID, base string) {
	agentTitleMu.Lock()
	agentTitleLastID, agentTitleLastBase = agentID, base
	agentTitleMu.Unlock()
	writeTitleEscape(agentTerminalTitle(state, agentID, base))
}

// stampAgentExitTitle writes the exit title (✗ …) from the last known
// identity and base. Used both by the agentCmd defer (normal and
// signal-interrupted returns) and by agentExitWithTerminalTitle (fatal
// os.Exit aborts, which bypass defers).
func stampAgentExitTitle() {
	agentTitleMu.Lock()
	id, base := agentTitleLastID, agentTitleLastBase
	agentTitleMu.Unlock()
	writeTitleEscape(agentTerminalTitle(agentTitleExited, id, base))
}

// agentExitWithTerminalTitle stamps the ✗ exit title and then calls os.Exit.
// Fatal wrapper exits that fire after the agent has run route through here so
// a long-dead agent does not keep looking alive in the terminal tab bar —
// os.Exit skips defers, so the agentCmd exit-title defer alone cannot cover
// these paths. Pre-run error aborts (before the agent ever runs, with the
// error already printed) intentionally stay bare os.Exit.
func agentExitWithTerminalTitle(code int) {
	stampAgentExitTitle()
	os.Exit(code)
}
