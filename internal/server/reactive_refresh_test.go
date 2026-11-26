package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/usages"
)

// Everything here is fake: an isolated home (startupEnv), a stand-in CLI, and a usage endpoint answered by a local transport that rejects
// the original access token with 401 and accepts the rotated one. Nothing reaches a real token or usage endpoint.

// fakeUsageTransport answers the claude usage request by bearer token: the dead token gets 401, every other one gets a valid reading.
type fakeUsageTransport struct {
	dead     string
	rejected atomic.Int32
}

func (f *fakeUsageTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	status, body := 200, `{"five_hour":{"utilization":25,"resets_at":"`+time.Now().Add(2*time.Hour).UTC().Format(time.RFC3339)+`"}}`
	if strings.Contains(req.Header.Get("Authorization"), f.dead) {
		f.rejected.Add(1)
		status, body = 401, `{"error":"token_expired"}`
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewBufferString(body)), Request: req}, nil
}

// reactiveCLI is the refresh CLI. rotate=false makes it exit cleanly without rotating anything (the token stays rejected); gate holds
// the spend until the test releases it.
type reactiveCLI struct {
	startupCLI
	rotate  bool
	gate    chan struct{}
	entered chan struct{}
	runs    atomic.Int32
}

func (c *reactiveCLI) Refresh(ctx context.Context, dir, flag string) error {
	c.runs.Add(1)
	select {
	case c.entered <- struct{}{}:
	default:
	}
	if c.gate != nil {
		<-c.gate
	}
	if !c.rotate {
		return nil
	}
	return c.startupCLI.Refresh(ctx, dir, flag)
}

// reactiveRig is a migrated claude profile "A" whose access token has a far expiry but is rejected by the usage endpoint, with the usage
// reader, the alert pass and the refresher all started the way the server starts them.
func reactiveRig(t *testing.T, cli *reactiveCLI) (f *fixture, live string, transport *fakeUsageTransport) {
	t.Helper()
	f, m, live := startupEnv(t, 6*time.Hour) // far from the 60 minute lead: the scheduler would never touch it
	migrateClaude(t, m)
	reader, err := usages.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.useUsages(reader)
	transport = &fakeUsageTransport{dead: "acc-A0"}
	reader.SetHTTPClient(&http.Client{Transport: transport})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		f.srv.WaitTokenCopies()
	})
	f.srv.StartTokenCopies(ctx, func(tr *usages.TargetRefresher) {
		tr.SetExecutor("claude", cli)
		tr.SetPreflight(func() bool { return true }, func(context.Context) bool { return true })
		tr.SetEnabled(func() bool { return true })
	})
	if err := f.srv.StartUsageAlerts(ctx, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	return f, live, transport
}

func waitReactive(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestReactiveRefreshHealsARejectedActiveLoginEndToEnd(t *testing.T) {
	cli := &reactiveCLI{rotate: true, gate: make(chan struct{}), entered: make(chan struct{}, 1)}
	f, live, transport := reactiveRig(t, cli)
	// Release the held spend however the test ends, so the cleanup that joins it cannot hang (a second close is skipped).
	release := func() {
		select {
		case <-cli.gate:
		default:
			close(cli.gate)
		}
	}
	t.Cleanup(release)
	ctx := context.Background()

	// (a) The usage poll reads the far-expiry login as rejected: the state spawn consults.
	f.srv.Usages.Refresh(ctx)
	if transport.rejected.Load() == 0 || !f.srv.providerLoginRejected("claude-code") {
		t.Fatalf("the active profile did not read as rejected (401s served: %d)", transport.rejected.Load())
	}
	// The usage alert pass runs the reactive refresh; the CLI is held, so the state is still rejected meanwhile.
	select {
	case <-cli.entered:
	case <-time.After(10 * time.Second):
		t.Fatalf("no reactive refresh started: the rejected login stays rejected (providerLoginRejected=%v, CLI runs=%d)",
			f.srv.providerLoginRejected("claude-code"), cli.runs.Load())
	}
	// Overlapping polls of the same rejected token do not start another run while one is in flight.
	for i := 0; i < 3; i++ {
		f.srv.Usages.Refresh(ctx)
	}
	time.Sleep(300 * time.Millisecond)
	if n := cli.runs.Load(); n != 1 {
		t.Fatalf("overlapping polls started %d refresh runs, want 1 (single flight)", n)
	}
	if !f.srv.providerLoginRejected("claude-code") {
		t.Fatal("the state cleared before the refresh finished")
	}

	// (b) The refresh rotates; the re-poll reads the new token as ok and the state clears.
	release()
	waitReactive(t, "the rejected state to clear after the rotation", func() bool { return !f.srv.providerLoginRejected("claude-code") })
	if total, twice := cli.calls(); total != 1 || len(twice) != 0 {
		t.Fatalf("CLI spent %d tokens (twice: %v), want exactly one", total, twice)
	}
	if data, err := os.ReadFile(live); err != nil || !strings.Contains(string(data), "acc-r1") || refreshTokenInFile.Match(data) {
		t.Fatalf("the live file must carry the rotated access token only: %v %s", err, data)
	}
}

func TestReactiveRefreshSpendsOncePerRejectedTokenAcrossPolls(t *testing.T) {
	cli := &reactiveCLI{rotate: false, entered: make(chan struct{}, 1)}
	f, _, _ := reactiveRig(t, cli)
	ctx := context.Background()
	f.srv.Usages.Refresh(ctx)
	select {
	case <-cli.entered:
	case <-time.After(10 * time.Second):
		t.Fatalf("no reactive refresh started: the rejected login stays rejected (CLI runs=%d)", cli.runs.Load())
	}
	// (c) The same token is rejected again and again; it never spends a second time.
	for i := 0; i < 4; i++ {
		time.Sleep(100 * time.Millisecond)
		f.srv.Usages.Refresh(ctx)
	}
	time.Sleep(300 * time.Millisecond)
	if n := cli.runs.Load(); n != 1 {
		t.Fatalf("the same rejected token spent %d times, want 1", n)
	}
	if !f.srv.providerLoginRejected("claude-code") {
		t.Fatal("a refresh that rotated nothing must leave the rejected state in place")
	}
}
