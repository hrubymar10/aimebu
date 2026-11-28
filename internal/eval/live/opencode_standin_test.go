package live

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/opencode"
	"github.com/hrubymar10/aimebu/v2/internal/eval"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

// opencodeOptions runs one scenario through the real OpenCode Docker adapter, started against a stand-in launcher script that answers the feature
// probe and then runs body (a shell fragment) for the turn. No Docker, network or credential is involved.
func opencodeOptions(t *testing.T, body string) *Options {
	t.Helper()
	t.Setenv("OPENAI_API_KEY", "test-key") // the key of the built-in provider (builtinProviderKey)
	root := t.TempDir()
	launcher := filepath.Join(root, "opencode-docker")
	script := "#!/bin/sh\nif [ \"$HARNESS_DOCKER_PRINT_FEATURES\" = 1 ]; then echo forward_env; exit 0; fi\ncat >/dev/null\n" + body
	if err := testenv.WriteExecutable(launcher, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	st := &stub{}
	o := newOptions(t, st, "s01-plain-reply")
	o.ScratchDir = t.TempDir()
	o.VisibleHost = "host.docker.internal"
	o.Targets = []Target{{Name: "opencode-x", Tool: "opencode", LauncherID: "lch_oc", Model: "my-provider/glm"}}
	o.Launchers = []protocol.LauncherRow{{LauncherDefinition: protocol.LauncherDefinition{ID: "lch_oc", Tool: "opencode", Containerized: true},
		ResolvedCommand: launcher, Available: true, Features: []string{"forward_env"}, Version: opencode.SupportedVersion}}
	o.NewAdapter = func(string) (adapter.Adapter, error) { return opencode.NewDocker(), nil }
	return o
}

// OpenCode reports a failed run (an unknown model, a provider refusing the key) as an "error" event on stdout and exits 1 with nothing on
// stderr; the adapter keeps it in ExitInfo.ErrorEvent. The harness failure the report shows must carry it, or every such case reads as
// "exit 1, stderr=" and nothing says why.
func TestOpenCodeErrorEventReachesTheHarnessFailure(t *testing.T) {
	o := opencodeOptions(t, `printf '%s\n' '{"type":"error","sessionID":"ses_x","error":{"name":"ProviderModelNotFoundError","data":{"message":"model my-provider/glm not found"}}}'
exit 1
`)
	cases, err := o.Run(context.Background())
	if err != nil || len(cases) != 1 || len(cases[0].Attempts) != 1 {
		t.Fatalf("cases %v err %v", cases, err)
	}
	a := cases[0].Attempts[0]
	if a.Class != eval.ClassHarness || !strings.Contains(a.Err, "ProviderModelNotFoundError") || !strings.Contains(a.Err, "model my-provider/glm not found") {
		t.Fatalf("the harness error event is missing from %q (class %s)", a.Err, a.Class)
	}
}

// stderr is part of the harness failure too, and when the harness said nothing anywhere the failure says so instead of ending in "stderr=".
func TestHarnessFailureCarriesStderrOrSaysItWasEmpty(t *testing.T) {
	o := opencodeOptions(t, "echo 'launcher: cannot reach the docker daemon' >&2\nexit 1\n")
	cases, err := o.Run(context.Background())
	if err != nil || len(cases) != 1 {
		t.Fatalf("cases %v err %v", cases, err)
	}
	if got := cases[0].Attempts[0].Err; !strings.Contains(got, "cannot reach the docker daemon") {
		t.Fatalf("stderr missing from %q", got)
	}
	o = opencodeOptions(t, "exit 1\n")
	cases, err = o.Run(context.Background())
	if err != nil || len(cases) != 1 {
		t.Fatalf("cases %v err %v", cases, err)
	}
	if got := cases[0].Attempts[0].Err; !strings.Contains(got, "no output on stdout or stderr") || strings.HasSuffix(got, "stderr=") {
		t.Fatalf("an empty failure must say so: %q", got)
	}
}

// A mounted OpenCode auth.json can authorize the built-in provider even when
// the runner process has no provider key in its environment.
func TestOpenCodeBuiltinProviderMayUseMountedAuth(t *testing.T) {
	o := opencodeOptions(t, "exit 1\n")
	for _, name := range adapterKnownKeys() {
		t.Setenv(name, "")
	}
	cases, err := o.Run(context.Background())
	if err != nil || len(cases) != 1 {
		t.Fatalf("built-in provider must reach the launcher: cases=%v err=%v", cases, err)
	}
}

func TestUnknownErrorFailureCarriesAHint(t *testing.T) {
	got := harnessFailure(adapter.ExitInfo{ExitCode: 1, ErrorEvent: "OpenCode error: UnknownError: Unexpected server error. Check server logs for details."}, nil)
	if !strings.Contains(got, "hint:") || !strings.Contains(got, "no key") {
		t.Fatalf("%q", got)
	}
	if strings.Contains(harnessFailure(adapter.ExitInfo{ExitCode: 1, ErrorEvent: "OpenCode error: ProviderModelNotFoundError: x"}, nil), "hint:") {
		t.Fatal("the hint is for the one ambiguous text")
	}
}

func adapterKnownKeys() []string { return adapter.KnownProviderKeys }
