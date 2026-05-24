package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSubstituteFleetCommand(t *testing.T) {
	got := substituteFleetCommand(
		`cd "${AIMEBU_FLEET_PATH}" && echo ${AIMEBU_FLEET_NAME}-${AIMEBU_FLEET_AGENT_INDEX} $PATH`,
		"default",
		"/tmp/project",
		2,
	)
	want := `cd "/tmp/project" && echo default-2 $PATH`
	if got != want {
		t.Fatalf("substituteFleetCommand = %q, want %q", got, want)
	}
}

func TestPlanFleetCommandFullWrap(t *testing.T) {
	got := planFleetCommand(fleetAgentConfig{Command: "echo hi"}, "default", "/tmp/p", 0)
	want := `osascript -e 'tell application "Terminal" to do script "cd /tmp/p && echo hi"'`
	if got != want {
		t.Fatalf("planFleetCommand = %q, want %q", got, want)
	}
}

func TestPlanFleetCommandSkipsCwd(t *testing.T) {
	disableCwd := false
	got := planFleetCommand(fleetAgentConfig{Command: "echo hi", AutoSetCwd: &disableCwd}, "default", "/tmp/p", 0)
	want := `osascript -e 'tell application "Terminal" to do script "echo hi"'`
	if got != want {
		t.Fatalf("planFleetCommand = %q, want %q", got, want)
	}
}

func TestPlanFleetCommandEscapesQuotes(t *testing.T) {
	got := planFleetCommand(fleetAgentConfig{Command: `say "hi"`}, "default", "/tmp/p", 0)
	want := `osascript -e 'tell application "Terminal" to do script "cd /tmp/p && say \"hi\""'`
	if got != want {
		t.Fatalf("planFleetCommand = %q, want %q", got, want)
	}
}

func TestPlanFleetCommandEscapesBackslash(t *testing.T) {
	got := planFleetCommand(fleetAgentConfig{Command: `echo a\b`}, "default", "/tmp/p", 0)
	want := `osascript -e 'tell application "Terminal" to do script "cd /tmp/p && echo a\\b"'`
	if got != want {
		t.Fatalf("planFleetCommand = %q, want %q", got, want)
	}
}

func TestFleetPlatformCheckFailsOnNonDarwin(t *testing.T) {
	if err := fleetPlatformCheck("linux"); err == nil {
		t.Fatal("fleetPlatformCheck(linux) = nil, want error")
	}
	if err := fleetPlatformCheck("darwin"); err != nil {
		t.Fatalf("fleetPlatformCheck(darwin) = %v, want nil", err)
	}
}

func TestValidateFleetLaunchConfigAcceptsCwdFalse(t *testing.T) {
	disableCwd := false
	fleet := fleetConfig{Agents: []fleetAgentConfig{{Command: "echo hi", AutoSetCwd: &disableCwd}}}
	if err := validateFleetLaunchConfig(fleet); err != nil {
		t.Fatalf("validateFleetLaunchConfig = %v, want nil", err)
	}
}

func TestValidateFleetLaunchConfigRejectsWrapFalse(t *testing.T) {
	disableWrap := false
	fleet := fleetConfig{Agents: []fleetAgentConfig{{Command: "echo hi", WrapTerminal: &disableWrap}}}
	if err := validateFleetLaunchConfig(fleet); err == nil {
		t.Fatal("validateFleetLaunchConfig = nil, want error")
	}
}

func TestPrintFleetUsageIncludesPlaceholders(t *testing.T) {
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = oldStdout })

	printFleetUsage()
	_ = w.Close()
	outBytes, _ := io.ReadAll(r)
	out := string(outBytes)

	for _, want := range []string{
		"Usage: aimebu fleet [<name> [path]]",
		"${AIMEBU_FLEET_PATH}",
		"${AIMEBU_FLEET_NAME}",
		"${AIMEBU_FLEET_AGENT_INDEX}",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("usage output missing %q: %s", want, out)
		}
	}
}

func TestResolveFleetPathDefaultsToAbsCwd(t *testing.T) {
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	got, err := resolveFleetPath(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Fatalf("resolveFleetPath(nil) = %q, want %q", got, dir)
	}
}

func TestResolveFleetPathExpandsRelative(t *testing.T) {
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	got, err := resolveFleetPath([]string{"subdir"})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "subdir")
	if got != want {
		t.Fatalf("resolveFleetPath(relative) = %q, want %q", got, want)
	}
}

func TestLaunchFleetPrintsFailedAgentSummary(t *testing.T) {
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = oldStderr })

	fleet := fleetConfig{Agents: []fleetAgentConfig{
		{Command: "echo ok ${AIMEBU_FLEET_AGENT_INDEX}"},
		{Command: "bad ${AIMEBU_FLEET_PATH} ${AIMEBU_FLEET_NAME} ${AIMEBU_FLEET_AGENT_INDEX}"},
	}}
	failures := launchFleetWithStarter("default", "/tmp/project", fleet, func(command string) error {
		if strings.Contains(command, "bad /tmp/project default 1") {
			return errors.New("boom")
		}
		return nil
	})
	_ = w.Close()
	outBytes, _ := io.ReadAll(r)

	if failures != 1 {
		t.Fatalf("failures = %d, want 1", failures)
	}
	out := string(outBytes)
	if !strings.Contains(out, `agent 1: osascript -e 'tell application "Terminal" to do script "cd /tmp/project && bad /tmp/project default 1"'`) {
		t.Fatalf("stderr missing failed agent command summary: %s", out)
	}
	if !strings.Contains(out, "1 fleet command(s) failed to start.") {
		t.Fatalf("stderr missing failure count: %s", out)
	}
}
