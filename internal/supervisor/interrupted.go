package supervisor

import (
	"fmt"
	"strings"
	"syscall"

	"github.com/hrubymar10/aimebu/internal/adapter"
)

// interruptedReason says whether a turn's harness process was cut off rather than having answered or failed with a reason: it ended
// without a final answer, by a signal, a non-zero status, or silently, and offered no explanation of its own. The harness being killed,
// its session restarted, or its container stopped all look like this. A deliberate owner cancel, a start failure, our own limit
// kill, and any exit the harness explained (an error result, a definite failure, a stderr message with an ordinary non-zero status)
// are not interruptions: they keep their own handling.
func interruptedReason(exit adapter.ExitInfo) (string, bool) {
	if exit.TerminalEvent || exit.CancelIntent || exit.StartFailure || exit.DefiniteFailure || exit.LimitStall ||
		exit.ErrorEvent != "" || exit.ErrorSummary != "" {
		return "", false
	}
	switch {
	case exit.ExitCode == -1:
		return "the harness process was killed by a signal", true
	case exit.ExitCode > 128 && exit.ExitCode < 192:
		// A shell or container runtime reports a killed child as 128 plus the signal number (137 is SIGKILL, 143 SIGTERM).
		return fmt.Sprintf("the harness process was %s (exit code %d)", signalPhrase(exit.ExitCode-128), exit.ExitCode), true
	}
	if strings.TrimSpace(exit.StderrTail) != "" {
		return "", false
	}
	if exit.ExitCode == 0 {
		return "the harness process ended without an answer (exit code 0)", true
	}
	return fmt.Sprintf("the harness process exited with code %d without an answer", exit.ExitCode), true
}

func signalPhrase(number int) string {
	if name := syscall.Signal(number).String(); !strings.HasPrefix(name, "signal ") {
		return fmt.Sprintf("stopped by signal %d (%s)", number, name)
	}
	return fmt.Sprintf("stopped by signal %d", number)
}

// emptyAnswer is a final answer with no content at all: nothing the server could parse, as opposed to an answer that parses badly.
func emptyAnswer(raw []byte) bool { return len(strings.TrimSpace(string(raw))) == 0 }
