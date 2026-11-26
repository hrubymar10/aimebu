package codex

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

type rpcReply struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type codexWarmSession struct {
	cmd                           *exec.Cmd
	stdin                         io.WriteCloser
	fp, runtimeDir, cwd, authPath string
	initialAuth                   codexWarmAuth
	containerized                 bool
	container                     *adapter.ContainerIdentity // which container the launcher started the process in (nil: it was not asked)
	capture, stdoutCapture        io.Writer
	exited                        chan struct{}
	reap                          adapter.Reaper
	tail                          adapter.StderrTail

	writeMu                    sync.Mutex
	captureMu                  sync.Mutex
	mu                         sync.Mutex
	pending                    map[int64]chan rpcReply
	nextID                     int64
	cur                        *codexWarmTurn
	threadID                   string
	dead, termSent, hardKilled bool
	exitCode                   int
	lastTotal                  codexTokenUsage
	defaultEffort              string // model/list's cold default, set before the session is returned
}

var containerTermGrace = adapter.ContainerLauncherTermGrace
var containerReportedTermGrace = adapter.ContainerReportedTermGrace

func (w *codexWarmSession) modelDefaultEffort(ctx context.Context, model string) (string, error) {
	params := map[string]any{"includeHidden": true}
	for page := 0; page < 10; page++ {
		var answer struct {
			Data []struct {
				ID                     string `json:"id"`
				DefaultReasoningEffort string `json:"defaultReasoningEffort"`
			} `json:"data"`
			NextCursor string `json:"nextCursor"`
		}
		if err := w.call(ctx, "model/list", params, &answer); err != nil {
			return "", err
		}
		for _, entry := range answer.Data {
			if entry.ID == model && entry.DefaultReasoningEffort != "" {
				return entry.DefaultReasoningEffort, nil
			}
		}
		if answer.NextCursor == "" {
			break
		}
		params["cursor"] = answer.NextCursor
	}
	return "", fmt.Errorf("%w: Codex model %q has no reported default reasoning effort; native and Docker warm mode need it", adapter.ErrNoWarm, model)
}

func newWarmSession(cmd *exec.Cmd, stdin io.WriteCloser, fp, runtimeDir, cwd, authPath string,
	containerized bool, capture, stdoutCapture io.Writer) *codexWarmSession {
	initial, _ := readCodexWarmAuth(authPath)
	return &codexWarmSession{cmd: cmd, stdin: stdin, fp: fp, runtimeDir: runtimeDir, authPath: authPath, initialAuth: initial,
		cwd: cwd, containerized: containerized, capture: capture, stdoutCapture: stdoutCapture,
		exited: make(chan struct{}), pending: make(map[int64]chan rpcReply)}
}

// Container implements adapter.ContainerSession: the container the launcher said it started the process in.
func (w *codexWarmSession) Container() (id string, neverStarted bool) { return w.container.Container() }

func (w *codexWarmSession) Fingerprint() string     { return w.fp }
func (w *codexWarmSession) Exited() <-chan struct{} { return w.exited }
func (w *codexWarmSession) Alive() bool {
	select {
	case <-w.exited:
		return false
	default:
		return true
	}
}
func (w *codexWarmSession) Pid() int {
	if w.cmd.Process == nil {
		return 0
	}
	return w.cmd.Process.Pid
}

func (w *codexWarmSession) send(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	_, err = w.stdin.Write(append(data, '\n'))
	return err
}

func (w *codexWarmSession) notify(method string, params any) error {
	return w.send(map[string]any{"method": method, "params": params})
}

func (w *codexWarmSession) call(ctx context.Context, method string, params any, out any) error {
	w.mu.Lock()
	if w.dead {
		w.mu.Unlock()
		return adapter.ErrWarmGone
	}
	w.nextID++
	id := w.nextID
	response := make(chan rpcReply, 1)
	w.pending[id] = response
	w.mu.Unlock()
	if err := w.send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		w.mu.Lock()
		delete(w.pending, id)
		w.mu.Unlock()
		return err
	}
	select {
	case reply, ok := <-response:
		if !ok {
			return adapter.ErrWarmGone
		}
		if reply.Error != nil {
			return fmt.Errorf("Codex %s: %s (%d)", method, reply.Error.Message, reply.Error.Code)
		}
		if out != nil {
			return json.Unmarshal(reply.Result, out)
		}
		return nil
	case <-ctx.Done():
		w.mu.Lock()
		delete(w.pending, id)
		w.mu.Unlock()
		return ctx.Err()
	case <-w.exited:
		return adapter.ErrWarmGone
	}
}

func (w *codexWarmSession) read(stdout, stderr io.Reader) {
	output := io.Writer(&w.tail)
	if w.capture != nil {
		output = io.MultiWriter(&w.tail, warmCaptureWriter{w})
	}
	stderrDone := make(chan struct{})
	// The launcher's exec ID line is the identity's, not the harness's output; the filter is closed before the session counts as exited.
	filtered := w.container.Filter(output)
	go func() { _, _ = io.Copy(filtered, stderr); _ = filtered.Close(); close(stderrDone) }()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		w.mu.Lock()
		current := w.cur
		w.mu.Unlock()
		if current != nil {
			current.lastOutput.Store(time.Now().UnixNano())
		}
		if w.stdoutCapture != nil {
			_, _ = w.stdoutCapture.Write(append(append([]byte(nil), line...), '\n'))
		}
		w.handleLine(line)
	}
	if scanner.Err() != nil {
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
	current := w.cur
	w.cur = nil
	for id := range w.pending {
		delete(w.pending, id)
	}
	w.mu.Unlock()
	close(w.exited)
	if current != nil {
		current.finish(adapter.ExitInfo{ExitCode: w.exitCode, StderrTail: w.tail.String(), ErrorEvent: current.errorText, CancelIntent: current.cancel.Load()}, scanner.Err())
	}
}

func (w *codexWarmSession) handleLine(line []byte) {
	var frame struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(line, &frame) != nil {
		return
	}
	if len(frame.ID) > 0 && frame.Method == "" {
		var id int64
		if json.Unmarshal(frame.ID, &id) != nil {
			return
		}
		w.mu.Lock()
		response := w.pending[id]
		delete(w.pending, id)
		current := w.cur
		w.mu.Unlock()
		if response != nil {
			var reply rpcReply
			if json.Unmarshal(line, &reply) == nil {
				if current != nil && reply.Error == nil {
					var result struct {
						Turn struct {
							ID string `json:"id"`
						} `json:"turn"`
					}
					if json.Unmarshal(reply.Result, &result) == nil && result.Turn.ID != "" {
						current.mu.Lock()
						current.serverTurnID = result.Turn.ID
						current.mu.Unlock()
					}
				}
				response <- reply
			}
		}
		return
	}
	if frame.Method == "" {
		return
	}
	if len(frame.ID) > 0 {
		go w.answerServerRequest(frame.ID, frame.Method, frame.Params)
		return
	}
	w.mu.Lock()
	current := w.cur
	w.mu.Unlock()
	if current != nil {
		current.handleNotification(frame.Method, frame.Params)
	}
}

func (w *codexWarmSession) answerServerRequest(id json.RawMessage, method string, params json.RawMessage) {
	// A warm process never performs an unattended approval or login flow.
	var result any
	var rpcError any
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval":
		result = map[string]string{"decision": "decline"}
	case "mcpServer/elicitation/request":
		result = map[string]string{"action": "cancel"}
	case "account/chatgptAuthTokens/refresh":
		result, rpcError = w.refreshAuthTokens(params)
	default:
		rpcError = map[string]any{"code": -32601, "message": "method not supported by aimebu"}
	}
	_, _ = warmCaptureWriter{w}.Write([]byte("Codex app-server request " + method + " answered without user action\n"))
	response := map[string]any{"id": id}
	if rpcError != nil {
		response["error"] = rpcError
	} else {
		response["result"] = result
	}
	_ = w.send(response)
}

type warmCaptureWriter struct{ w *codexWarmSession }

func (writer warmCaptureWriter) Write(data []byte) (int, error) {
	if writer.w.capture == nil {
		return len(data), nil
	}
	writer.w.captureMu.Lock()
	defer writer.w.captureMu.Unlock()
	return writer.w.capture.Write(data)
}

func (w *codexWarmSession) waitExit(d time.Duration) bool {
	if d <= 0 {
		d = time.Second
	}
	select {
	case <-w.exited:
		return true
	case <-time.After(d):
		return false
	}
}
func (w *codexWarmSession) killGroup() {
	w.mu.Lock()
	w.hardKilled = true
	w.mu.Unlock()
	if pid := w.Pid(); pid > 0 {
		_ = adapter.SignalGroup(pid, syscall.SIGKILL, w.reap.Reaped)
	}
}
func (w *codexWarmSession) confirmed() error {
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
	// launch.sh preserves a zero docker exec exit through synchronous cleanup,
	// including when TERM arrives during that cleanup; its 143 path verifies stop.
	if !killed && (code == 0 || (term && code == 143)) {
		return nil
	}
	return errors.New("Codex process in container is not confirmed gone")
}
func (w *codexWarmSession) EndEvidence() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.reap.Reaped() {
		return fmt.Sprintf("exit=unknown term_sent=%t kill_sent=%t", w.termSent, w.hardKilled)
	}
	return fmt.Sprintf("exit=%d term_sent=%t kill_sent=%t", w.exitCode, w.termSent, w.hardKilled)
}
func (w *codexWarmSession) ContainerToken() string { return w.container.Token() }
func (w *codexWarmSession) EndReportEnabled() bool { return w.container.ReportEnabled() }
func (w *codexWarmSession) Close(grace time.Duration) error {
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
	return w.confirmed()
}

func (w *codexWarmSession) waitEnd(d time.Duration) bool {
	select {
	case <-w.exited:
		return true
	case <-w.container.Ended():
		return true
	case <-time.After(d):
		return false
	}
}
func (w *codexWarmSession) Kill() error {
	w.mu.Lock()
	w.hardKilled = true
	w.mu.Unlock()
	w.killGroup()
	if !w.waitExit(10 * time.Second) {
		return fmt.Errorf("warm Codex process did not end after kill")
	}
	return w.confirmed()
}
