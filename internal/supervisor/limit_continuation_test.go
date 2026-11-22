package supervisor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// gitCheckout makes a git repository with one commit, for the work-tree description of a limit kill.
func gitCheckout(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("commit", "-q", "-m", "feat(test): the committed work")
	return dir
}

func TestWorkTreeStateDescribesTipAndDirtyFiles(t *testing.T) {
	dir := gitCheckout(t)
	got := workTreeState(context.Background(), dir)
	if !strings.HasPrefix(got, "tip ") || !strings.Contains(got, "feat(test): the committed work") || !strings.HasSuffix(got, ", tree clean") {
		t.Fatalf("clean checkout: %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := workTreeState(context.Background(), dir); !strings.HasSuffix(got, ", 1 dirty file") {
		t.Fatalf("one changed file: %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := workTreeState(context.Background(), dir); !strings.HasSuffix(got, ", 2 dirty files") {
		t.Fatalf("a changed and an untracked file: %q", got)
	}
	if got := workTreeState(context.Background(), t.TempDir()); got != "" {
		t.Fatalf("not a checkout: %q", got)
	}
	if got := workTreeState(context.Background(), ""); got != "" {
		t.Fatalf("no folder: %q", got)
	}
}

// limitKilledContainerTurn runs one warm turn in a container that the turn-limit watchdog kills, with a launcher that never confirms the end of
// the process in the container: the agent's slot is left stuck, as xio's was. It returns the exit the server was sent.
func limitKilledContainerTurn(t *testing.T, h *warmHarness, checkout string) adapter.ExitInfo {
	t.Helper()
	var mu sync.Mutex
	var exit adapter.ExitInfo
	h.c.processSink = func(_ string, e adapter.ExitInfo) { mu.Lock(); exit = e; mu.Unlock() }
	h.c.TurnLimits = TurnLimitOverrides{Idle: durationPtr(0), Max: durationPtr(80 * time.Millisecond)}
	h.f.runHold = true // the turn ends only when its process does
	h.f.onRun = func() { h.f.sessions[len(h.f.sessions)-1].lingers.Store(true) }
	h.run("agt_a", func(s *adapter.TurnSpec) { s.LauncherContainerized = true; s.CheckoutPath = checkout })
	h.f.runHold, h.f.onRun = false, nil
	h.c.TurnLimits = TurnLimitOverrides{}
	if !h.c.warmStuck("agt_a") {
		t.Fatal("the limit kill must leave the slot stuck (its end in the container is not confirmed)")
	}
	mu.Lock()
	defer mu.Unlock()
	return exit
}

// A turn-limit kill whose end in the container is not confirmed leaves the agent's slot stuck. The automatic continuation that the server sends
// at once waits out the quarantine and then starts, instead of failing in under a second and sending the agent to recovery. The limit kill's
// exit carries the checkout's tip and dirty-file count.
func TestLimitKillContinuationWaitsOutTheQuarantine(t *testing.T) {
	oldRetry, oldQuarantine := warmRetryEvery, warmStuckQuarantine
	warmRetryEvery, warmStuckQuarantine = time.Hour, 400*time.Millisecond
	defer func() { warmRetryEvery, warmStuckQuarantine = oldRetry, oldQuarantine }()
	h := newWarmHarness(t)
	h.c.warmStuckWait, h.c.warmStuckPoll = 10*time.Second, 10*time.Millisecond
	h.spawn("agt_a")
	checkout := gitCheckout(t)
	exit := limitKilledContainerTurn(t, h, checkout)
	if !exit.LimitStall || exit.TerminalEvent || exit.CancelIntent {
		t.Fatalf("exit=%+v", exit)
	}
	if !strings.Contains(exit.WorkTree, "feat(test): the committed work") || !strings.HasSuffix(exit.WorkTree, "tree clean") ||
		!strings.Contains(exit.StderrTail, "work tree at the kill: "+exit.WorkTree) {
		t.Fatalf("the limit kill's evidence must describe the checkout: %+v", exit)
	}
	exits, err := h.c.Journal.PendingExits(context.Background())
	if err != nil || len(exits) != 1 || exits[0].Exit.WorkTree != exit.WorkTree {
		t.Fatalf("the description must be journaled for a replay: %+v %v", exits, err)
	}
	began := time.Now()
	h.run("agt_a", func(s *adapter.TurnSpec) { s.LauncherContainerized = true; s.CheckoutPath = checkout }) // fails the test if the start fails
	if h.f.started != 2 {
		t.Fatalf("the continuation must start a new process once the quarantine is over: started=%d", h.f.started)
	}
	if waited := time.Since(began); waited < 100*time.Millisecond {
		t.Fatalf("the continuation did not wait for the quarantine: %v", waited)
	}
	if h.c.warmStuck("agt_a") {
		t.Fatal("the slot is still stuck")
	}
}

// A stop that comes while the continuation waits wins: no process is started, and the wait ends at once rather than at the quarantine's end.
func TestStopDuringTheQuarantineWaitWins(t *testing.T) {
	// The background retry runs often (the launcher keeps saying "unconfirmed" until the test lets it confirm), and the quarantine never ends.
	oldRetry, oldQuarantine := warmRetryEvery, warmStuckQuarantine
	warmRetryEvery, warmStuckQuarantine = 20*time.Millisecond, time.Hour
	defer func() { warmRetryEvery, warmStuckQuarantine = oldRetry, oldQuarantine }()
	h := newWarmHarness(t)
	h.c.warmStuckWait, h.c.warmStuckPoll = 10*time.Second, 10*time.Millisecond
	h.spawn("agt_a")
	limitKilledContainerTurn(t, h, "")
	spec := adapter.TurnSpec{TurnID: "turn_continue", Agent: adapter.AgentSpec{ID: "agt_a", Tool: "claude", Variant: "native", Model: "haiku"}, Mode: adapter.ModeNormal, LauncherContainerized: true, NotesJSON: json.RawMessage(`[]`), ContextJSON: json.RawMessage(`{}`)}
	result := make(chan error, 1)
	go func() {
		_, err := h.c.startTurn(context.Background(), spec, nil, h.cred)
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("the continuation must wait for the quarantine: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if !h.c.handleControl(protocol.Frame{AgentID: "agt_a", Generation: 1, DesiredRev: 2, DesiredState: "stopped"}) {
		t.Fatal("the stop's ack must wait while the process is not confirmed gone")
	}
	select {
	case err := <-result:
		if !errors.Is(err, errStoppedWhileWaiting) {
			t.Fatalf("the stop must win over the waiting turn: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the wait did not end with the stop")
	}
	if h.f.started != 1 || h.f.cold != 0 {
		t.Fatalf("something started after the stop: started=%d cold=%d", h.f.started, h.f.cold)
	}
	// The launcher now confirms the end: the stop completes, and the background goroutines are gone before the globals are restored.
	h.f.sessions[0].lingers.Store(false)
	eventually(t, "the stop completing once the end is confirmed", func() bool { return h.c.keyOf("agt_a") == "" && !h.c.warmStuck("agt_a") })
}

// A stuck slot past the wait's limit still fails the start as before (a process that is provably alive is never released).
func TestQuarantineWaitGivesUpAtItsLimit(t *testing.T) {
	// The background retry runs often (the launcher keeps saying "unconfirmed" until the test lets it confirm), and the quarantine never ends.
	oldRetry, oldQuarantine := warmRetryEvery, warmStuckQuarantine
	warmRetryEvery, warmStuckQuarantine = 20*time.Millisecond, time.Hour
	defer func() { warmRetryEvery, warmStuckQuarantine = oldRetry, oldQuarantine }()
	h := newWarmHarness(t)
	h.c.warmStuckWait, h.c.warmStuckPoll = 150*time.Millisecond, 10*time.Millisecond
	h.spawn("agt_a")
	limitKilledContainerTurn(t, h, "")
	spec := adapter.TurnSpec{TurnID: "turn_continue", Agent: adapter.AgentSpec{ID: "agt_a", Tool: "claude", Variant: "native", Model: "haiku"}, Mode: adapter.ModeNormal, LauncherContainerized: true, NotesJSON: json.RawMessage(`[]`), ContextJSON: json.RawMessage(`{}`)}
	began := time.Now()
	if _, err := h.c.startTurn(context.Background(), spec, nil, h.cred); !errors.Is(err, errWarmUnconfirmed) {
		t.Fatalf("err=%v", err)
	}
	if waited := time.Since(began); waited < 150*time.Millisecond {
		t.Fatalf("gave up before the limit: %v", waited)
	}
	h.f.sessions[0].lingers.Store(false)
	eventually(t, "the slot released", func() bool { return !h.c.warmStuck("agt_a") })
}

// The live process's report of a background job's answer between turns reaches the server as a background_job_ended frame for the agent, with
// its generation.
func TestBackgroundResultBetweenTurnsIsReportedForTheAgent(t *testing.T) {
	h := newWarmHarness(t)
	var mu sync.Mutex
	var got []string
	h.c.backgroundEnded = func(agentID string) { mu.Lock(); got = append(got, agentID); mu.Unlock() }
	h.spawn("agt_a")
	h.run("agt_a", nil)
	callback := h.f.lastSpec.OnBackgroundResult
	if callback == nil {
		t.Fatal("a warm process must be started with the background-result callback")
	}
	callback()
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "agt_a" {
		t.Fatalf("reported=%v", got)
	}
}

func TestBackgroundJobEndedFrameCarriesTheAgentAndGeneration(t *testing.T) {
	encoded, err := encodeMachineFrame(protocol.Frame{V: protocol.Version, Type: protocol.BackgroundJobEndedFrame, AgentID: "agt_a", Generation: 3})
	if err != nil {
		t.Fatal(err)
	}
	var frame protocol.Frame
	if err := json.Unmarshal(encoded, &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Type != "background_job_ended" || frame.AgentID != "agt_a" || frame.Generation != 3 {
		t.Fatalf("frame=%s", encoded)
	}
}
