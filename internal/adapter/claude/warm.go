package claude

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

var _ adapter.WarmAdapter = (*Adapter)(nil)

// WarmFingerprint is the hash of what a live process is started with: the command, its arguments (but not which session to continue), the
// environment (the agent's key only as a hash) and the directory. Only a normal turn runs warm: a correction or diagnostic differs in the
// tools it is given, and a fork continues a session as a copy, so each of them runs on a process of its own.
func (a *Adapter) WarmFingerprint(spec adapter.TurnSpec) (string, error) {
	if spec.Mode != adapter.ModeNormal || spec.ForkSession {
		return "", adapter.ErrNoWarm
	}
	l, err := a.prepare(spec, true)
	if err != nil {
		return "", err
	}
	return l.fingerprint(), nil
}

func (l *launch) fingerprint() string {
	h := sha256.New()
	field := func(parts ...string) {
		for _, part := range parts {
			fmt.Fprintf(h, "%d:%s,", len(part), part)
		}
		h.Write([]byte{'\n'})
	}
	field("name", l.name)
	field("dir", l.dir)
	for i, arg := range l.args {
		if i >= l.sessionAt[0] && i < l.sessionAt[1] {
			continue
		}
		field("arg", arg)
	}
	env := append([]string(nil), l.env...)
	sort.Strings(env)
	for _, item := range env {
		if item == l.agentKey && l.agentKey != "" {
			sum := sha256.Sum256([]byte(item))
			item = strings.SplitN(item, "=", 2)[0] + "=sha256:" + hex.EncodeToString(sum[:8])
		}
		field("env", item)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// StartWarm starts a live `claude -p --input-format stream-json` process. It reads one user message per turn and answers each with a result
// event; that event, not the end of the process, is where a turn ends.
func (a *Adapter) StartWarm(ctx context.Context, spec adapter.TurnSpec) (adapter.WarmSession, error) {
	if spec.Mode != adapter.ModeNormal || spec.ForkSession {
		return nil, adapter.ErrNoWarm
	}
	l, err := a.prepare(spec, true)
	if err != nil {
		return nil, err
	}
	cmd := l.command(ctx)
	containerized := a.variant() == "docker" || spec.LauncherContainerized
	var container *adapter.ContainerIdentity
	var endRead, endWrite *os.File
	defer func() {
		if endRead != nil {
			_ = endRead.Close()
		}
		if endWrite != nil {
			_ = endWrite.Close()
		}
	}()
	if containerized && spec.LauncherReportsContainer {
		// Asked of the process only, not of the fingerprint: what the launcher prints about itself changes nothing the harness is started with.
		container = &adapter.ContainerIdentity{}
		cmd.Env = append(cmd.Env[:len(cmd.Env):len(cmd.Env)], adapter.DockerReportExecIDEnv+"=1")
		if spec.LauncherReportsSessionEnd {
			cmd.Env = append(cmd.Env, adapter.DockerStopGraceEnv+"=2")
			endRead, endWrite, err = container.AttachEndReport(cmd)
			if err != nil {
				return nil, err
			}
		}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	if endWrite != nil {
		_ = endWrite.Close()
		endWrite = nil
		go container.ReadEndReport(endRead)
		endRead = nil
	}
	w := &warmSession{fp: l.fingerprint(), cmd: cmd, stdin: stdin, exited: make(chan struct{}), capture: adapter.JoinWriters(spec.StderrSink, a.stderrCapture), containerized: containerized, container: container, onBackground: spec.OnBackgroundResult}
	w.lastOutput.Store(time.Now().UnixNano())
	go w.run(stdout, stderr)
	if spec.LauncherReportsSessionEnd {
		// An invalid fd is rejected at launcher startup. Surface that as a
		// launch failure instead of allowing a cold retry of the same launcher.
		select {
		case <-w.exited:
			w.mu.Lock()
			code := w.exitCode
			w.mu.Unlock()
			// Exit 2 is the launcher rejecting the fd at startup only while nothing has run: once it reported its exec ID a container
			// was started, and the exit is the harness's own (the normal path and the cold fallback handle it).
			if id, _ := container.Container(); code == 2 && container != nil && id == "" {
				return nil, fmt.Errorf("%w: session end fd was rejected", adapter.ErrWarmLaunch)
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	return w, nil
}

// warmSession is one live process and, while one is running, the turn it is working on.
type warmSession struct {
	fp         string
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	capture    io.Writer
	exited     chan struct{}
	reap       adapter.Reaper
	lastOutput atomic.Int64
	tail       adapter.StderrTail

	mu        sync.Mutex
	cur       *turn
	state     *runState
	confirmed bool // the session id has been reported: later turns need not report it again
	dead      bool
	killTimer *time.Timer
	// hardKilled says the process was killed rather than asked to end; exitCode is the process's own exit status once it has ended;
	// containerized says it runs through a Docker launcher.
	hardKilled    bool
	termSent      bool // it was asked to terminate (after its input was closed)
	exitCode      int
	containerized bool
	// container is which container the launcher started the process in (nil: the launcher was not asked).
	container *adapter.ContainerIdentity
	// stray is what the process wrote while no turn was running, reported with the next turn (see noteStray).
	stray strayOutput
	// onBackground is told of a background job's answer between turns (see backgroundResult and adapter.TurnSpec.OnBackgroundResult); nil: nobody is.
	onBackground func()
}

// strayOutput is a bounded record of the output a live process writes between turns: the type of each event and the start of the first few.
// Nothing between turns belongs to a turn, but an answer that lands there was lost, so it is kept and journaled with the next turn.
type strayOutput struct {
	count    int
	types    []string
	excerpts []string
}

const (
	strayTypesKept    = 32
	strayExcerpts     = 3
	strayExcerptBytes = 300
)

// noteStray records one line written between turns.
func (w *warmSession) noteStray(line []byte) {
	var head struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
	}
	kind := "unparsed"
	if json.Unmarshal(line, &head) == nil && head.Type != "" {
		kind = head.Type
		if head.Subtype != "" {
			kind += "/" + head.Subtype
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stray.count++
	if len(w.stray.types) < strayTypesKept {
		w.stray.types = append(w.stray.types, kind)
	}
	if len(w.stray.excerpts) < strayExcerpts {
		excerpt := string(line)
		if len(excerpt) > strayExcerptBytes {
			excerpt = strings.ToValidUTF8(excerpt[:strayExcerptBytes], "") + "…"
		}
		w.stray.excerpts = append(w.stray.excerpts, excerpt)
	}
}

// backgroundResult says a line written between turns is the result of a query the CLI ran on its own for a background job that ended (a result
// event with `origin.kind` "task-notification" that is not an error): the agent answered something nobody reads.
func backgroundResult(line []byte) bool {
	var event struct {
		Type    string `json:"type"`
		IsError bool   `json:"is_error"`
		Origin  struct {
			Kind string `json:"kind"`
		} `json:"origin"`
	}
	return json.Unmarshal(line, &event) == nil && event.Type == "result" && !event.IsError && event.Origin.Kind == "task-notification"
}

// detail is the journal text for the stray output, "" when there was none.
func (s strayOutput) detail() string {
	if s.count == 0 {
		return ""
	}
	text := fmt.Sprintf("%d events arrived between turns: %s", s.count, strings.Join(s.types, ","))
	if s.count > len(s.types) {
		text += fmt.Sprintf(",… (%d more)", s.count-len(s.types))
	}
	for _, excerpt := range s.excerpts {
		text += "\n" + excerpt
	}
	return adapter.SanitizeStderr(text)
}

// Container implements adapter.ContainerSession: the container the launcher said it started the process in.
func (w *warmSession) Container() (id string, neverStarted bool) { return w.container.Container() }

func (w *warmSession) Fingerprint() string     { return w.fp }
func (w *warmSession) Exited() <-chan struct{} { return w.exited }
func (w *warmSession) Alive() bool {
	select {
	case <-w.exited:
		return false
	default:
		return true
	}
}
func (w *warmSession) Pid() int {
	if w.cmd.Process == nil {
		return 0
	}
	return w.cmd.Process.Pid
}

// Run feeds the next turn to the process: the same JSON a one-turn process reads on standard input, as one stream-json user message.
func (w *warmSession) Run(spec adapter.TurnSpec) (adapter.Turn, error) {
	if spec.TurnID == "" {
		return nil, fmt.Errorf("turn ID required")
	}
	input, err := turnInput(spec)
	if err != nil {
		return nil, err
	}
	message, err := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []map[string]string{{"type": "text", "text": string(input)}}}})
	if err != nil {
		return nil, err
	}
	// The marker is worked out before the lock: nothing between taking it and releasing it returns early without releasing it.
	marker, err := json.Marshal(spec.TurnID)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	if w.dead {
		launchFailed := w.container.ReportEnabled() && w.exitCode == 2
		w.mu.Unlock()
		if launchFailed {
			return nil, fmt.Errorf("%w: session end fd was rejected", adapter.ErrWarmLaunch)
		}
		return nil, adapter.ErrWarmGone
	}
	if w.cur != nil {
		w.mu.Unlock()
		return nil, adapter.ErrWarmBusy
	}
	t := &turn{id: spec.TurnID, warm: w, echoMarker: `{"turn_id":` + string(marker), expectedSession: spec.NativeSessionHandle, started: spec.NativeSessionStarted || w.confirmed, events: make(chan adapter.Event, 32), done: make(chan struct{}), conv: adapter.ConversationFor(spec)}
	t.cmd = w.cmd
	w.cur, w.state = t, newRunState()
	if w.killTimer != nil {
		w.killTimer.Stop()
		w.killTimer = nil
	}
	stray := w.stray
	w.stray = strayOutput{}
	w.mu.Unlock()
	if detail := stray.detail(); detail != "" {
		t.emit(adapter.Event{Kind: adapter.EventActivity, Summary: "between_turns", Detail: detail})
	}
	if _, err := w.stdin.Write(append(message, '\n')); err != nil {
		// The process cannot take the turn; it is as good as gone. Its end finishes the turn like any crash.
		_ = w.Kill()
	}
	return t, nil
}

// run reads the process's output for as long as it lives.
func (w *warmSession) run(stdout, stderr io.Reader) {
	output := io.Writer(&w.tail)
	if w.capture != nil {
		output = io.MultiWriter(&w.tail, w.capture)
	}
	stderrDone := make(chan struct{})
	// The launcher's exec ID line is the identity's, not the harness's output. The filter is closed once the stderr is read to its end, which
	// is before the session counts as exited, so an identity that says "never started" has seen everything the launcher wrote.
	filtered := w.container.Filter(output)
	go func() { _, _ = io.Copy(filtered, stderr); _ = filtered.Close(); close(stderrDone) }()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		w.lastOutput.Store(time.Now().UnixNano())
		line := append([]byte(nil), scanner.Bytes()...)
		w.mu.Lock()
		t, s := w.cur, w.state
		w.mu.Unlock()
		if t == nil {
			// Between turns the process should say nothing a turn needs; what it does say is kept for the next turn's journal. An answer to a
			// background job's end is lost all the same, so the supervisor is told and the agent gets a real turn to report it.
			w.noteStray(line)
			if w.onBackground != nil && backgroundResult(line) {
				w.onBackground()
			}
			continue
		}
		t.handleLine(s, line)
		if s.sawResult {
			w.endTurnAtResult(t, s)
		}
	}
	scanError := scanner.Err()
	if scanError != nil {
		w.killGroup()
	}
	<-stderrDone
	err := w.cmd.Wait()
	w.reap.MarkReaped()
	w.mu.Lock()
	if err != nil {
		if status, ok := err.(*exec.ExitError); ok {
			w.exitCode = status.ExitCode()
		} else {
			w.exitCode = 1
		}
	}
	w.dead = true
	t, s := w.cur, w.state
	w.cur, w.state = nil, nil
	if w.killTimer != nil {
		w.killTimer.Stop()
	}
	w.mu.Unlock()
	close(w.exited)
	if t != nil {
		// The process ended inside a turn (a crash, a cancel, the end that follows an error result): the turn ends as a one-process turn does.
		t.conclude(s, scanError)
		exit := t.exitInfo(s, w.tail.String(), err)
		t.finish(exit, scanError)
	}
}

// endTurnAtResult ends the turn whose result event has just been read. A successful result with an answer is the normal end: the exit is
// terminal with code 0 and the process stays up for the next turn. Anything else (an error result, no answer) is the end of the process too:
// it is left to end on its own, briefly, and killed if it does not, and the turn ends with the process's own exit.
func (w *warmSession) endTurnAtResult(t *turn, s *runState) {
	if !s.haveCandidate || s.errorIsResult {
		w.mu.Lock()
		w.killTimer = time.AfterFunc(2*time.Second, w.killGroup)
		w.mu.Unlock()
		return
	}
	t.conclude(s, nil)
	w.mu.Lock()
	w.cur, w.state = nil, nil
	w.confirmed = true
	w.mu.Unlock()
	t.finish(t.exitInfo(s, "", nil), nil)
}

// finish reports a turn's exit and ends its events.
func (t *turn) finish(exit adapter.ExitInfo, scanError error) {
	t.mu.Lock()
	t.exit, t.err = exit, scanError
	t.mu.Unlock()
	t.emit(adapter.Event{Kind: adapter.EventProcessExit, Exit: &exit})
	close(t.events)
	close(t.done)
}

// ErrContainerSideUnconfirmed says the host side of a containerized session has ended without the launcher's wrapper saying that the process
// inside the container is gone. `docker exec` passes no signal on, so a kill (or a wrapper that died) proves nothing about the container, and
// no amount of waiting does either.
var ErrContainerSideUnconfirmed = errors.New("the process in the container is not confirmed gone: its launcher did not say so")

// What confirms the end of a containerized session. Two ends do. The clean one: the input is closed, the CLI exits, `docker exec` returns and the
// wrapper's own session cleanup runs before it exits with status 0. And, for a launcher that advertises it (supervisor.containerEndFeature,
// harness-docker's `stop_on_signal`), a terminate: the wrapper traps TERM, ends the session in the container, waits for it, and only then exits
// with 128+signal, so exactly 143 after our SIGTERM means the container side is gone. A zero exit also confirms it even when TERM arrived during
// synchronous cleanup after docker exec ended: launch.sh leaves that exit code unchanged and returns only after run_session_cleanup. Anything
// else (another status, a kill, a wrapper that was
// killed) proves nothing about the container, since `docker exec` passes no signal on, and time proves nothing either.
var containerTermConfirmedStatus = 143

// containerTermGrace is replaceable by tests; the production bound is shared with Codex.
var containerTermGrace = adapter.ContainerLauncherTermGrace
var containerReportedTermGrace = adapter.ContainerReportedTermGrace

// Close ends the process the clean way: its input is closed, so the CLI exits and, in a container, `docker exec` returns and the wrapper
// cleans its session up before it exits; if it has not ended after grace it is told to terminate (the wrapper cleans up on that too), and
// only if it still has not ended is it killed. For a session in a container it returns nil only when the wrapper exited cleanly after its input
// was closed or, for a launcher with `stop_on_signal`, exited 143 after the terminate (see containerTermConfirmedStatus), and
// ErrContainerSideUnconfirmed otherwise.
func (w *warmSession) Close(grace time.Duration) error {
	_ = w.stdin.Close()
	if !w.waitEnd(grace) {
		w.mu.Lock()
		w.termSent = true
		w.mu.Unlock()
		if w.containerized {
			if !w.reap.Reaped() {
				_ = w.cmd.Process.Signal(syscall.SIGTERM)
			}
		} else if pid := w.Pid(); pid > 0 {
			_ = adapter.SignalGroup(pid, syscall.SIGTERM, w.reap.Reaped)
		}
		wait := grace
		if w.container.Ended() != nil {
			wait = max(wait, containerReportedTermGrace)
		} else if w.containerized {
			wait = max(wait, containerTermGrace)
		}
		if !w.waitEnd(wait) {
			return w.Kill()
		}
	}
	return w.endConfirmed()
}

func (w *warmSession) waitEnd(d time.Duration) bool {
	select {
	case <-w.exited:
		return true
	case <-w.container.Ended():
		return true
	case <-time.After(d):
		return false
	}
}

func (w *warmSession) waitExit(d time.Duration) bool {
	select {
	case <-w.exited:
		return true
	case <-time.After(d):
		return false
	}
}

// endConfirmed says whether an ended session is confirmed gone: a native one always is (it was reaped); a containerized one only by the clean
// exit described at containerTermConfirmedStatus.
func (w *warmSession) endConfirmed() error {
	if !w.containerized {
		return nil
	}
	if w.reap.Reaped() {
		w.container.WaitEndReport(time.Second)
	}
	if w.container.EndConfirmed() {
		return nil
	}
	w.mu.Lock()
	code, killed, term := w.exitCode, w.hardKilled, w.termSent
	w.mu.Unlock()
	switch {
	case killed:
		return ErrContainerSideUnconfirmed
	case code == 0:
		return nil // closed its input and exited cleanly: the wrapper's cleanup ran
	case term && code == containerTermConfirmedStatus:
		return nil
	}
	return ErrContainerSideUnconfirmed
}

// EndEvidence is local diagnostic detail for an unconfirmed warm end.
func (w *warmSession) EndEvidence() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.reap.Reaped() {
		return fmt.Sprintf("exit=unknown term_sent=%t kill_sent=%t", w.termSent, w.hardKilled)
	}
	return fmt.Sprintf("exit=%d term_sent=%t kill_sent=%t", w.exitCode, w.termSent, w.hardKilled)
}

func (w *warmSession) ContainerToken() string { return w.container.Token() }
func (w *warmSession) EndReportEnabled() bool { return w.container.ReportEnabled() }

// Kill ends the process group at once. For a session in a container that is only the host side, so it is never confirmed.
func (w *warmSession) Kill() error {
	w.mu.Lock()
	w.hardKilled = true
	w.mu.Unlock()
	w.killGroup()
	if !w.waitExit(10 * time.Second) {
		return fmt.Errorf("warm claude process did not end after a kill")
	}
	return w.endConfirmed()
}

func (w *warmSession) killGroup() {
	if pid := w.Pid(); pid > 0 {
		_ = adapter.SignalGroup(pid, syscall.SIGKILL, w.reap.Reaped)
	}
}
