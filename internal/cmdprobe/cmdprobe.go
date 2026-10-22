// Package cmdprobe runs the short commands the server and the supervisor use to ask a tool something (`<tool> --version`, `npm prefix -g`,
// `docker ps`) and bounds the wait for them.
//
// A context deadline on exec.CommandContext kills the command only. A command that started helpers (an npm shim runs node) leaves them
// holding the pipe the output is read through, and without a WaitDelay the read then waits for them for as long as they live, so the
// deadline bounds nothing. With one, a command that exited successfully but left a helper behind is still a success: the version it printed
// is valid. Only a command that failed, or that the deadline killed, is an error.
package cmdprobe

import (
	"errors"
	"os/exec"
	"time"
)

// Grace is how long the read of a probe's output waits for helpers to let go of the pipe once the command is done.
const Grace = time.Second

// Output is cmd.Output with the wait for its output pipe bounded by Grace. cmd should come from exec.CommandContext, so that a deadline
// ends it.
func Output(cmd *exec.Cmd) ([]byte, error) {
	cmd.WaitDelay = Grace
	out, err := cmd.Output()
	return out, settle(cmd, err)
}

// CombinedOutput is cmd.CombinedOutput with the wait for its output pipe bounded by Grace.
func CombinedOutput(cmd *exec.Cmd) ([]byte, error) {
	cmd.WaitDelay = Grace
	out, err := cmd.CombinedOutput()
	return out, settle(cmd, err)
}

// Run is cmd.Run for callers that provide their own bounded output writers.
func Run(cmd *exec.Cmd) error {
	cmd.WaitDelay = Grace
	return settle(cmd, cmd.Run())
}

// settle turns "the command succeeded but a helper kept the pipe open" into a success. A command that the deadline killed did not succeed,
// so its error stands.
func settle(cmd *exec.Cmd, err error) error {
	if err != nil && errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		return nil
	}
	return err
}
