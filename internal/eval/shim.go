package eval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/claude"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/codex"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/opencode"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/pi"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/vibe"
)

// Harness kinds, one per adapter whose parser a case can feed.
const (
	KindClaude   = "claude"
	KindCodex    = "codex"
	KindPi       = "pi"
	KindOpenCode = "opencode"
	KindVibe     = "vibe"
)

// Kinds lists the harness kinds.
var Kinds = []string{KindClaude, KindCodex, KindPi, KindOpenCode, KindVibe}

func validKind(kind string) bool {
	for _, k := range Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// Played is what the real adapter made of a stream.
type Played struct {
	// HasAnswer says the adapter reported a final-answer event; Answer is its raw candidate.
	HasAnswer bool
	Answer    string
	Exit      adapter.ExitInfo
	Usage     *adapter.Usage
}

// Play runs the real adapter of kind against a stand-in harness executable that drains its stdin, prints stream, and exits with exitCode. The
// adapter starts it exactly as it starts the real CLI (its arguments, environment and stdin are the adapter's own), so what comes out is what
// the adapter's parser makes of the stream. Nothing real is started: the executable is a shell script in a temporary directory, no credential
// is read and no network is used. Like the adapters' own tests, the caller's HOME and tool directories must already be isolated.
func Play(ctx context.Context, kind string, stream []byte, exitCode int) (Played, error) {
	root, err := os.MkdirTemp("", "aimebu-eval-")
	if err != nil {
		return Played{}, err
	}
	defer os.RemoveAll(root)
	streamPath := filepath.Join(root, "stream")
	if err := os.WriteFile(streamPath, stream, 0o600); err != nil {
		return Played{}, err
	}
	script := "#!/bin/sh\ncat >/dev/null\ncat '" + streamPath + "'\n"
	if kind == KindVibe {
		// Vibe reports its token totals in a meta.json it saves under its session directory, which the turn reads after the stream ends.
		script += `d="$VIBE_SESSION_LOGGING__SAVE_DIR/session_20261024_120000_` + vibeSession[:8] + `"
mkdir -p "$d"
printf '{"session_id":"` + vibeSession + `","stats":{"session_prompt_tokens":10,"session_completion_tokens":5,"session_cached_tokens":0,"context_tokens":15,"session_cost":0.001}}' > "$d/meta.json"
`
	}
	script += fmt.Sprintf("exit %d\n", exitCode)
	command := filepath.Join(root, kind)
	if err := writeExecutable(command, []byte(script)); err != nil {
		return Played{}, err
	}
	checkout := filepath.Join(root, "checkout")
	if err := os.MkdirAll(checkout, 0o700); err != nil {
		return Played{}, err
	}
	runtime := filepath.Join(root, "runtime")
	tools := &adapter.AgentToolsAccess{AgentID: WorkerID, ServerURL: "http://127.0.0.1:9897", Key: "aak_eval", Command: "/usr/bin/aimebu"}
	spec := adapter.TurnSpec{TurnID: "turn_eval", Agent: adapter.AgentSpec{ID: WorkerID, Tool: kind, Variant: "native", Model: "eval/model"},
		CheckoutPath: checkout, Mode: adapter.ModeNormal, AgentTools: tools, NotesJSON: json.RawMessage(`[]`)}
	var a adapter.Adapter
	switch kind {
	case KindClaude:
		a = &claude.Adapter{Command: command, AllowNative: true}
		spec.Agent.Model = "haiku"
	case KindCodex:
		// The Docker variant runs the launcher it is given and never reads a Codex home, so no login is looked for.
		a = &codex.Adapter{Command: command, Variant: "docker"}
		spec.Agent.Variant, spec.VisibleCheckoutPath = "docker", "/workspace/project"
	case KindPi:
		a = &pi.Adapter{Command: command, Variant: "native", AllowNative: true, RuntimeRoot: runtime}
		spec.NativeSessionHandle = piSession // Pi names its session itself and refuses a turn without the server's id for it
	case KindOpenCode:
		a = &opencode.Adapter{Command: command, RuntimeRoot: runtime}
	case KindVibe:
		home := filepath.Join(root, "user-vibe")
		if err := os.MkdirAll(home, 0o700); err != nil {
			return Played{}, err
		}
		a = &vibe.Adapter{Command: command, RuntimeRoot: runtime, UserHome: home}
	default:
		return Played{}, fmt.Errorf("unknown harness kind %q", kind)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	turn, err := a.Start(ctx, spec)
	if err != nil {
		return Played{}, err
	}
	var played Played
	for event := range turn.Events() {
		switch event.Kind {
		case adapter.EventFinalAnswer:
			played.HasAnswer, played.Answer = true, string(event.Answer)
		case adapter.EventUsage:
			if event.Usage != nil && !event.Usage.Partial {
				played.Usage = event.Usage
			}
		}
	}
	played.Exit, err = turn.Wait()
	return played, err
}

const (
	vibeSession     = "0c3e1f4a-8b2d-4a6e-9f10-5d7c2b1a9e33"
	opencodeSession = "ses_eval0000000000000000000"
	piSession       = "123e4567-e89b-42d3-a456-426614174000"
)

// Wrap is the stream a harness of kind prints for a turn whose model answered with candidate: the same keys and shapes as the recorded
// streams the adapters' tests use, trimmed to what the parser reads.
func Wrap(kind, candidate string) []byte {
	text, _ := json.Marshal(candidate)
	var lines []string
	switch kind {
	case KindClaude:
		lines = []string{
			`{"type":"system","subtype":"init","session_id":"s1","model":"eval-model"}`,
			`{"type":"assistant","message":{"content":[{"type":"text","text":` + string(text) + `}],"usage":{"input_tokens":11,"output_tokens":7}}}`,
			`{"type":"result","result":` + string(text) + `,"usage":{"input_tokens":11,"output_tokens":7},"modelUsage":{"x":{"contextWindow":200000,"costUSD":0.01}}}`,
		}
	case KindCodex:
		lines = []string{
			`{"type":"thread.started","thread_id":"thread-1"}`, `{"type":"turn.started"}`,
			`{"type":"item.completed","item":{"type":"agent_message","text":` + string(text) + `}}`,
			`{"type":"turn.completed","usage":{"input_tokens":11,"output_tokens":7}}`,
		}
	case KindPi:
		message := `{"role":"assistant","content":[{"type":"text","text":` + string(text) + `}],"usage":{"input":11,"output":7,"cost":{"total":0.01}}}`
		lines = []string{`{"type":"session","version":3,"id":"` + piSession + `"}`, `{"type":"agent_start"}`, `{"type":"turn_start"}`,
			`{"type":"message_end","message":` + message + `}`, `{"type":"turn_end","message":` + message + `}`, `{"type":"agent_settled","aborted":false}`}
	case KindOpenCode:
		lines = []string{
			`{"type":"step_start","timestamp":1792175803262,"sessionID":"` + opencodeSession + `","part":{"id":"prt_1","messageID":"msg_1","sessionID":"` + opencodeSession + `","type":"step-start"}}`,
			`{"type":"text","timestamp":1792175803263,"sessionID":"` + opencodeSession + `","part":{"id":"prt_2","messageID":"msg_1","sessionID":"` + opencodeSession + `","type":"text","text":` + string(text) + `,"time":{"start":1792175803260,"end":1792175803261}}}`,
			`{"type":"step_finish","timestamp":1792175803274,"sessionID":"` + opencodeSession + `","part":{"id":"prt_3","reason":"stop","messageID":"msg_1","sessionID":"` + opencodeSession + `","type":"step-finish","tokens":{"total":18,"input":11,"output":7,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.01}}`,
		}
	case KindVibe:
		entry := func(n int, tail string) string {
			return fmt.Sprintf(`{"id":"ent_%02d","sessionId":"%s","turnId":"turn_a","createdAt":%d,"updatedAt":%d,"generationStatus":"completed","relatedEntryId":null,%s}`, n, vibeSession, 1761300000000+n, 1761300000000+n, tail)
		}
		lines = []string{
			entry(1, `"type":"message","role":"user","content":[{"type":"text","text":"prompt"}],"source":"turn_start","userDisplayContent":null`),
			entry(2, `"type":"message","role":"assistant","content":[{"type":"text","text":`+string(text)+`}],"source":null,"userDisplayContent":null`),
		}
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// FaultStream is the stream and exit code of a harness that fails: "crash" prints what a start looks like and exits 17, "silent" prints
// nothing and exits 0.
func FaultStream(kind, fault string) ([]byte, int) {
	if fault != "crash" {
		return nil, 0
	}
	first := strings.SplitN(string(Wrap(kind, "x")), "\n", 2)[0]
	return []byte(first + "\n"), 17
}

// writeExecutable writes a script the adapter is about to run so that running it cannot fail with "text file busy": a process another goroutine
// forks while the file is open for writing inherits the descriptor, and an exec of the file is refused until that process has exec'd. The
// contents go to a temporary file written under the read side of syscall.ForkLock (os/exec takes the write side around every fork) and are renamed
// into place. It is the method of internal/testenv.WriteExecutable, which is for test files only.
func writeExecutable(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".exec-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	syscall.ForkLock.RLock()
	_, err = tmp.Write(data)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return err
	}
	if err := os.Chmod(name, 0o700); err != nil {
		return err
	}
	return os.Rename(name, path)
}
