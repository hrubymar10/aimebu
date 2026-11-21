package adapter

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

// The exec ID line is taken out of the launcher's stderr, wherever the writes split it; everything else passes through untouched.
func TestExecIDFilterTakesTheLineOutOfTheStream(t *testing.T) {
	stream := "relay healed\nHARNESS_DOCKER_EXEC_ID=abc123:42-7-99\nharness says hi\nHARNESS_DOCKER_EXEC_ID=spoof:1\nno newline"
	for _, size := range []int{1, 3, 7, len(stream)} {
		activity := &DockerActivity{}
		var out strings.Builder
		filter := activity.Filter(&out)
		for i := 0; i < len(stream); i += size {
			_, _ = filter.Write([]byte(stream[i:min(i+size, len(stream))]))
		}
		_ = filter.Close()
		if activity.Token() != "abc123:42-7-99" {
			t.Fatalf("chunk %d: token %q", size, activity.Token())
		}
		if want := "relay healed\nharness says hi\nHARNESS_DOCKER_EXEC_ID=spoof:1\nno newline"; out.String() != want {
			t.Fatalf("chunk %d: output %q", size, out.String())
		}
	}
	// A malformed token is no exec ID, and passes through.
	activity := &DockerActivity{}
	var out strings.Builder
	filter := activity.Filter(&out)
	_, _ = filter.Write([]byte("HARNESS_DOCKER_EXEC_ID=bad token\n"))
	_ = filter.Close()
	if activity.Token() != "" || out.String() != "HARNESS_DOCKER_EXEC_ID=bad token\n" {
		t.Fatalf("token %q output %q", activity.Token(), out.String())
	}
	// No activity: the writer is unchanged.
	var nilActivity *DockerActivity
	var plain strings.Builder
	w := nilActivity.Filter(&plain)
	_, _ = w.Write([]byte("HARNESS_DOCKER_EXEC_ID=a:b\n"))
	if plain.String() != "HARNESS_DOCKER_EXEC_ID=a:b\n" || nilActivity.Busy(time.Now()) {
		t.Fatal("a nil activity must pass everything through and never be busy")
	}
}

// The probe hands the token and since back, and busy and starting are activity; idle, a failure and an unexpected answer are not.
func TestDockerActivityProbeAnswers(t *testing.T) {
	dir := t.TempDir()
	launcher := filepath.Join(dir, "vibe-docker")
	source := "#!/bin/sh\n" +
		"echo \"$HARNESS_DOCKER_PROBE $HARNESS_DOCKER_SINCE $*\" > '" + dir + "/asked'\n" +
		"case \"$HARNESS_DOCKER_PROBE\" in\n" +
		"  c:busy) echo busy;; c:starting) echo starting;; c:idle) echo idle;; c:weird) echo maybe;;\n" +
		"  *) echo 'Error: no running session for this token.' >&2; exit 1;;\n" +
		"esac\n"
	if err := testenv.WriteExecutable(launcher, []byte(source), 0700); err != nil {
		t.Fatal(err)
	}
	since := time.Unix(1_800_000_000, 0)
	for token, want := range map[string]bool{"c:busy": true, "c:starting": true, "c:idle": false, "c:weird": false, "c:gone": false} {
		activity := &DockerActivity{Command: launcher, Args: []string{"--arg"}, Dir: dir, Env: DockerControlEnv(nil)}
		activity.setToken(token)
		if got := activity.Busy(since); got != want {
			t.Errorf("%s: busy %v, want %v", token, got, want)
		}
		asked, _ := os.ReadFile(filepath.Join(dir, "asked"))
		if strings.TrimSpace(string(asked)) != token+" 1800000000 --arg" {
			t.Errorf("%s: asked %q", token, asked)
		}
	}
	// No token yet: nothing is asked.
	_ = os.Remove(filepath.Join(dir, "asked"))
	if (&DockerActivity{Command: launcher, Dir: dir}).Busy(since) {
		t.Fatal("busy without a token")
	}
	if _, err := os.Stat(filepath.Join(dir, "asked")); err == nil {
		t.Fatal("the launcher was asked without a token")
	}
	// A time before the epoch is sent as 0, which the launcher accepts (1–18 digits).
	if _, err := ProbeDockerActivity(context.Background(), launcher, nil, dir, nil, "c:idle", time.Unix(-5, 0)); err != nil {
		t.Fatal(err)
	}
	if asked, _ := os.ReadFile(filepath.Join(dir, "asked")); !strings.HasPrefix(string(asked), "c:idle 0 ") {
		t.Fatalf("asked %q", asked)
	}
}

// The control environment keeps the owner's references but never a mode switch or the agent key.
func TestDockerControlEnvDropsModeSwitches(t *testing.T) {
	t.Setenv("OWNER_SOURCE", "value")
	env := DockerControlEnv(map[string]string{"HARNESS_DOCKER_RUN_COMMAND": "OWNER_SOURCE", "HARNESS_DOCKER_PROBE": "OWNER_SOURCE",
		AgentKeyEnv: "OWNER_SOURCE", "HARNESS_DOCKER_CONTAINER": "OWNER_SOURCE"})
	joined := "\n" + strings.Join(env, "\n") + "\n"
	if strings.Contains(joined, "HARNESS_DOCKER_RUN_COMMAND=") || strings.Contains(joined, "HARNESS_DOCKER_PROBE=") ||
		strings.Contains(joined, AgentKeyEnv+"=") || !strings.Contains(joined, "\nHARNESS_DOCKER_CONTAINER=value\n") {
		t.Fatalf("env %q", env)
	}
}

// A probe runs under the turn's context: stopping the activity (a cancel, the turn's end) interrupts one in flight, and a stopped activity
// is never asked again.
func TestDockerActivityProbeStopsWithTheTurn(t *testing.T) {
	dir := t.TempDir()
	launcher := filepath.Join(dir, "vibe-docker")
	source := "#!/bin/sh\necho asked >> '" + dir + "/asked'\nexec sleep 30\n"
	if err := testenv.WriteExecutable(launcher, []byte(source), 0700); err != nil {
		t.Fatal(err)
	}
	activity := &DockerActivity{Command: launcher, Dir: dir, Env: DockerControlEnv(nil), Timeout: 20 * time.Second}
	activity.setToken("c:hung")
	activity.Bind(t.Context())
	go func() { // stop once the probe is really in flight (the launcher has recorded that it was asked)
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if asked, _ := os.ReadFile(filepath.Join(dir, "asked")); len(asked) > 0 {
				break
			}
		}
		activity.Stop()
	}()
	started := time.Now()
	if activity.Busy(time.Now()) {
		t.Fatal("an interrupted probe answered busy")
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("the stop did not interrupt the probe: %s", elapsed)
	}
	if activity.Busy(time.Now()) {
		t.Fatal("a stopped activity answered busy")
	}
	if asked, _ := os.ReadFile(filepath.Join(dir, "asked")); strings.Count(string(asked), "asked") != 1 {
		t.Fatalf("a stopped activity was asked again: %q", asked)
	}
}
