package adapter

import (
	"context"
	"errors"
	"time"
)

// WarmAdapter is implemented by an adapter whose harness can keep one process up between turns, so that a turn that follows another soon
// after is handed to the live process instead of starting a new one and resuming the session.
//
// The supervisor owns when a warm process starts, how long it idles and when it ends; the adapter owns how a turn is fed to it and where
// the turn ends. An adapter without this interface (or whose WarmFingerprint says no) is run one process per turn, as always.
type WarmAdapter interface {
	// WarmFingerprint identifies everything a warm process for this spec is started with: the command and arguments (the system prompt and
	// MCP configuration included, as they are today), the environment (the agent key as a hash), the directory. It leaves out which session to
	// continue and the turn itself. The supervisor compares it at every delivery: a warm process whose fingerprint differs from the one the
	// delivery would start is ended and the turn runs cold. A spec the warm mode cannot serve (a correction, a diagnostic, a fork) returns
	// ErrNoWarm.
	WarmFingerprint(spec TurnSpec) (string, error)
	// StartWarm starts the live process for an agent. It runs no turn: the first is handed in with Run. spec carries what the process is
	// started with (and the first turn's session); ctx bounds the process, not a turn.
	StartWarm(ctx context.Context, spec TurnSpec) (WarmSession, error)
}

// WarmSession is one live harness process taking turns one at a time.
type WarmSession interface {
	// Fingerprint is the WarmFingerprint the process was started with.
	Fingerprint() string
	// Run hands the next turn to the live process. The returned Turn is the same as a one-process turn's: its events end with a process_exit
	// event and Wait returns that exit. A turn that ends with a final answer is terminal, with exit code 0, and the process stays up; any other
	// ending (an error result, a crash, a cancel) ends the process too, and the exit is the process's own. Only one turn runs at a time.
	Run(spec TurnSpec) (Turn, error)
	// Alive says whether the process is still up.
	Alive() bool
	// Exited is closed once the process has ended and been reaped.
	Exited() <-chan struct{}
	// Pid is the process (group) id, for the supervisor's own bookkeeping.
	Pid() int
	// Close asks the process to end by closing its input, kills it if it has not ended after grace, and returns once it is reaped.
	Close(grace time.Duration) error
	// Kill ends the process group at once and returns once it is reaped.
	Kill() error
}

// ContainerLauncherTermGrace covers the legacy launcher signal path in
// harness-docker/bin/lib/launch.sh:448-485: three attempts of five seconds
// HUP plus two seconds KILL verification, 0.25-second retry pauses, and the
// final one-second Docker client wait. The session_end_report path uses a
// two-second HUP grace and has its own shorter bound.
const ContainerLauncherTermGrace = 25 * time.Second

// ContainerReportedTermGrace bounds the session_end_report path after TERM:
// with HARNESS_DOCKER_STOP_GRACE=2 the maintainer's worst case is about 13.5s.
const ContainerReportedTermGrace = 15 * time.Second

// ErrNoWarm says a spec is not served by a warm process.
var ErrNoWarm = errors.New("no warm mode for this turn")

// ErrWarmBusy is returned by Run while a turn is still running in the session.
var ErrWarmBusy = errors.New("a turn is already running in this warm session")

// ErrWarmGone is returned by Run when the session's process has ended.
var ErrWarmGone = errors.New("the warm session's process has ended")

// ErrWarmLaunch means a selected Docker launcher's reported interface failed
// at startup. The supervisor reports a start failure rather than falling cold.
var ErrWarmLaunch = errors.New("warm Docker launcher failed to start")
