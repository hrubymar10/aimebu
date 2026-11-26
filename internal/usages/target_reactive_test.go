package usages

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// All credentials are fake (tok_aaaa-style placeholders) under t.TempDir(); the
// executor is a scripted runner, so no token endpoint is ever contacted.

func codexRotateRunner(now time.Time) *scriptedRunner {
	return &scriptedRunner{fn: func(call int, dir string) (refreshRunResult, error) {
		body := codexFull(now.Add(240*time.Hour), "tok_new", "refresh-new", now)
		return refreshRunResult{Started: true}, os.WriteFile(filepath.Join(dir, "auth.json"), []byte(body), 0o600)
	}}
}

func TestReactiveRefreshRefreshesAFarExpiryProfileOnceAndFansOut(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := codexRotateRunner(now)
	h := newRefreshHarness(t, runner)
	// The token is nowhere near due by its own exp (10 days), yet the account rejected it.
	p := h.addProfile("codex", "work", true, codexFull(now.Add(240*time.Hour), "tok_aaaa", "refresh-old", now.Add(-time.Hour)))
	if err := h.r.RefreshDue(context.Background()); err != nil || runner.callCount() != 0 {
		t.Fatalf("scheduled pass: err=%v calls=%d, want a quiet skip", err, runner.callCount())
	}
	res := h.r.RefreshRejected(context.Background(), "codex", "work")
	if res.Outcome != ReactiveRotated {
		t.Fatalf("outcome = %q (%s), want rotated", res.Outcome, res.Detail)
	}
	if runner.callCount() != 1 || !strings.Contains(h.stored(p), "refresh-new") {
		t.Fatalf("calls=%d stored=%s", runner.callCount(), h.stored(p))
	}
	if calls := h.fan.snapshot(); len(calls) != 1 || calls[0].tool != "codex" || !strings.Contains(string(calls[0].full), "refresh-new") {
		t.Fatalf("fan-out = %+v, want one rotated codex fan-out", calls)
	}
	if kinds := h.stateAlertKinds(); len(kinds) != 0 {
		t.Fatalf("a rotation raised alerts: %v", kinds)
	}
}

func TestReactiveRefreshSpendsOncePerAccessToken(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: func(call int, dir string) (refreshRunResult, error) {
		return refreshRunResult{Started: true}, nil // exits 0, rotated nothing: the same token stays rejected
	}}
	h := newRefreshHarness(t, runner)
	h.addProfile("codex", "work", true, codexFull(now.Add(240*time.Hour), "tok_aaaa", "refresh-old", now.Add(-time.Hour)))
	ctx := context.Background()
	h.r.RefreshRejected(ctx, "codex", "work")
	second := h.r.RefreshRejected(ctx, "codex", "work")
	h.r.RefreshRejected(ctx, "codex", "work")
	if runner.callCount() != 1 {
		t.Fatalf("the same rejected token spent %d times, want 1", runner.callCount())
	}
	if second.Outcome != ReactiveSkipped {
		t.Fatalf("second outcome = %q, want skipped", second.Outcome)
	}
}

func TestReactiveRefreshInvalidGrantKeepsTheStateRaisesOneAlertAndNeverWipes(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: func(call int, dir string) (refreshRunResult, error) {
		return refreshRunResult{Started: true, Output: "Your access token could not be refreshed because your refresh token was revoked."}, errors.New("exit status 1")
	}}
	h := newRefreshHarness(t, runner)
	p := h.addProfile("codex", "work", true, codexFull(now.Add(240*time.Hour), "tok_aaaa", "refresh-old", now.Add(-time.Hour)))
	before := h.stored(p)
	ctx := context.Background()
	res := h.r.RefreshRejected(ctx, "codex", "work")
	if res.Outcome != ReactiveRevoked || !strings.Contains(res.Detail, "revoked") {
		t.Fatalf("result = %+v, want revoked naming the session", res)
	}
	h.r.RefreshRejected(ctx, "codex", "work")
	if h.stored(p) != before {
		t.Fatalf("a failed refresh changed the stored credential: %s", h.stored(p))
	}
	if h.r.AccountStates()["codex/work"] != AccountReauthRequired {
		t.Fatalf("states = %v, want the rejected state kept", h.r.AccountStates())
	}
	kinds := h.stateAlertKinds()
	if len(kinds) != 1 {
		t.Fatalf("alerts = %v, want exactly one", kinds)
	}
	h.mu.Lock()
	alert := h.alerts[len(h.alerts)-1]
	h.mu.Unlock()
	if !strings.Contains(alert, "session was revoked") {
		t.Fatalf("alert %q does not say the session was revoked", alert)
	}
	if len(h.fan.snapshot()) != 0 || runner.callCount() != 1 {
		t.Fatalf("fan-out=%d calls=%d", len(h.fan.snapshot()), runner.callCount())
	}
}

func TestReactiveRefreshOtherFailureAlertsWithItsClass(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: func(call int, dir string) (refreshRunResult, error) {
		return refreshRunResult{}, errors.New("dial tcp: connection refused")
	}}
	h := newRefreshHarness(t, runner)
	p := h.addProfile("codex", "work", true, codexFull(now.Add(240*time.Hour), "tok_aaaa", "refresh-old", now.Add(-time.Hour)))
	before := h.stored(p)
	res := h.r.RefreshRejected(context.Background(), "codex", "work")
	if res.Outcome != ReactiveFailed || !strings.Contains(res.Detail, "the refresh failed") {
		t.Fatalf("result = %+v, want failed", res)
	}
	if h.stored(p) != before {
		t.Fatal("a failed refresh changed the stored credential")
	}
	if len(h.stateAlertKinds()) != 1 {
		t.Fatalf("alerts = %v, want one", h.stateAlertKinds())
	}
}

func TestReactiveRefreshLeavesANonActiveProfileAlone(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := codexRotateRunner(now)
	h := newRefreshHarness(t, runner)
	h.addProfile("codex", "work", true, codexFull(now.Add(240*time.Hour), "tok_aaaa", "refresh-a", now.Add(-time.Hour)))
	idle := h.addProfile("codex", "spare", false, codexFull(now.Add(240*time.Hour), "tok_bbbb", "refresh-b", now.Add(-time.Hour)))
	res := h.r.RefreshRejected(context.Background(), "codex", "spare")
	if res.Outcome != ReactiveSkipped || runner.callCount() != 0 || !strings.Contains(h.stored(idle), "refresh-b") {
		t.Fatalf("result=%+v calls=%d", res, runner.callCount())
	}
}

func TestReactiveRefreshHonoursTheToolGate(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := codexRotateRunner(now)
	h := newRefreshHarness(t, runner)
	h.addProfile("codex", "work", true, codexFull(now.Add(240*time.Hour), "tok_aaaa", "refresh-old", now.Add(-time.Hour)))
	h.r.SetToolEnabled(func(string) bool { return false })
	if res := h.r.RefreshRejected(context.Background(), "codex", "work"); res.Outcome != ReactiveSkipped || runner.callCount() != 0 {
		t.Fatalf("a tool the refresher does not own was spent: %+v", res)
	}
}

func TestRefreshProfileAlertsWhenTheActiveExpiryCannotBeRead(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := codexRotateRunner(now)
	h := newRefreshHarness(t, runner)
	// An access token without a readable exp is "never due" for the scheduler.
	body := `{"tokens":{"access_token":"opaque","refresh_token":"refresh-old","account_id":"acct-1","id_token":"h.e30.s"},"last_refresh":"2026-01-01T00:00:00Z"}`
	h.addProfile("codex", "work", true, body)
	h.addProfile("codex", "spare", false, body)
	if err := h.r.RefreshDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = h.r.RefreshDue(context.Background())
	kinds := h.stateAlertKinds()
	if len(kinds) != 1 || kinds[0] != "expiry_unreadable" {
		t.Fatalf("alerts = %v, want one expiry_unreadable (the active profile only)", kinds)
	}
	if runner.callCount() != 0 {
		t.Fatal("an unreadable expiry must not spend a token")
	}
}

func TestRefreshProfileLogsEachSkipReasonOncePerState(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	h := newRefreshHarness(t, codexRotateRunner(now))
	h.addProfile("codex", "work", true, codexFull(now.Add(240*time.Hour), "tok_aaaa", "refresh-old", now.Add(-time.Hour)))
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	for i := 0; i < 3; i++ {
		_ = h.r.RefreshDue(context.Background())
	}
	if n := strings.Count(buf.String(), "skipped"); n != 1 {
		t.Fatalf("the not-due skip was logged %d times, want once:\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), "not due") {
		t.Fatalf("log does not name the reason:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "tok_aaaa") || strings.Contains(buf.String(), "refresh-old") {
		t.Fatal("a token reached the log")
	}
}

func TestActiveRejectedLoginsListsOnlyTheActiveRejectedProfile(t *testing.T) {
	tracker, err := NewAlertTracker(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := Response{Providers: []Provider{{ProviderName: "codex", Enabled: true, Available: true, Profiles: []Profile{
		{ProfileName: "work", Active: true, Status: "auth_missing", AlertIdentity: "id-a"},
		{ProfileName: "spare", Active: false, Status: "auth_missing", AlertIdentity: "id-b"},
	}}}}
	if _, err := tracker.Observe(snapshot); err != nil {
		t.Fatal(err)
	}
	got := tracker.ActiveRejectedLogins()
	if len(got) != 1 || got[0].Tool != "codex" || got[0].Profile != "work" {
		t.Fatalf("active rejected = %+v", got)
	}
	snapshot.Providers[0].Profiles[0].Status = "ok"
	if _, err := tracker.Observe(snapshot); err != nil {
		t.Fatal(err)
	}
	if got := tracker.ActiveRejectedLogins(); len(got) != 0 {
		t.Fatalf("a recovered profile is still listed: %+v", got)
	}
}

func TestReactiveRefreshHeldGuardKeepsTheTokenEligible(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := codexRotateRunner(now)
	h := newRefreshHarness(t, runner)
	p := h.addProfile("codex", "work", true, codexFull(now.Add(240*time.Hour), "tok_aaaa", "refresh-old", now.Add(-time.Hour)))
	ctx := context.Background()
	// A scheduled refresh (or anything else) holds the account lock: the first try cannot run.
	release, ok, err := AcquireMaintenance(h.root, "codex", "work", p.CredPath)
	if err != nil || !ok {
		t.Fatalf("lock: ok=%v err=%v", ok, err)
	}
	first := h.r.RefreshRejected(ctx, "codex", "work")
	release()
	if first.Outcome != ReactiveSkipped || runner.callCount() != 0 {
		t.Fatalf("held guard: %+v calls=%d, want a skip that spent nothing", first, runner.callCount())
	}
	// The next poll sees the same rejected token and now succeeds: the held try did not use up its attempt.
	second := h.r.RefreshRejected(ctx, "codex", "work")
	if second.Outcome != ReactiveRotated || runner.callCount() != 1 {
		t.Fatalf("after the guard released: %+v calls=%d, want rotated with exactly one spend", second, runner.callCount())
	}
	_ = p
}

func TestReactiveRefreshPendingLoginKeepsTheTokenEligible(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := codexRotateRunner(now)
	h := newRefreshHarness(t, runner)
	h.addProfile("codex", "work", true, codexFull(now.Add(240*time.Hour), "tok_aaaa", "refresh-old", now.Add(-time.Hour)))
	pending := true
	h.r.SetPendingLogin(func(string) bool { return pending })
	ctx := context.Background()
	if res := h.r.RefreshRejected(ctx, "codex", "work"); res.Outcome != ReactiveSkipped || runner.callCount() != 0 {
		t.Fatalf("pending login: %+v calls=%d", res, runner.callCount())
	}
	pending = false
	if res := h.r.RefreshRejected(ctx, "codex", "work"); res.Outcome != ReactiveRotated || runner.callCount() != 1 {
		t.Fatalf("after the login was settled: %+v calls=%d", res, runner.callCount())
	}
}

func TestReactiveRefreshARunSpendLimitsEveryOutcomeToOneAttempt(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for name, script := range map[string]func(int, string) (refreshRunResult, error){
		"rotated": codexRotateRunner(now).fn,
		"revoked": func(int, string) (refreshRunResult, error) {
			return refreshRunResult{Started: true, Output: "Your access token could not be refreshed because your refresh token was revoked."}, errors.New("exit status 1")
		},
		"failed": func(int, string) (refreshRunResult, error) {
			return refreshRunResult{}, errors.New("dial tcp: connection refused")
		},
	} {
		runner := &scriptedRunner{fn: script}
		h := newRefreshHarness(t, runner)
		p := h.addProfile("codex", "work", true, codexFull(now.Add(240*time.Hour), "tok_aaaa", "refresh-old", now.Add(-time.Hour)))
		first := h.r.RefreshRejected(context.Background(), "codex", "work")
		if first.Outcome == ReactiveSkipped {
			t.Fatalf("%s: the first try was skipped: %+v", name, first)
		}
		// Put the very same rejected token back (a re-import of the dead login) and signal again.
		if err := os.WriteFile(p.CredPath, []byte(codexFull(now.Add(240*time.Hour), "tok_aaaa", "refresh-old", now.Add(-time.Hour))), 0o600); err != nil {
			t.Fatal(err)
		}
		h.clock.now = h.clock.now.Add(2 * time.Hour) // past the per-profile gap and any backoff
		if again := h.r.RefreshRejected(context.Background(), "codex", "work"); again.Outcome != ReactiveSkipped || runner.callCount() != 1 {
			t.Fatalf("%s: the same token spent again: %+v calls=%d", name, again, runner.callCount())
		}
	}
}

func rotatingCodexRunner(now time.Time) *scriptedRunner {
	return &scriptedRunner{fn: func(call int, dir string) (refreshRunResult, error) {
		body := codexFull(now.Add(240*time.Hour), "tok_new"+strconv.Itoa(call), "refresh-new"+strconv.Itoa(call), now.Add(time.Duration(call)*time.Minute))
		return refreshRunResult{Started: true}, os.WriteFile(filepath.Join(dir, "auth.json"), []byte(body), 0o600)
	}}
}

func countAlerts(h *refreshHarness, kind string) (n int, last string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, a := range h.alerts {
		if strings.HasPrefix(a, kind+" ") {
			n++
			last = a
		}
	}
	return n, last
}

func TestReactiveRefreshStopsWhenTheFreshTokenIsRejectedToo(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := rotatingCodexRunner(now)
	h := newRefreshHarness(t, runner)
	p := h.addProfile("codex", "work", true, codexFull(now.Add(240*time.Hour), "tok_aaaa", "refresh-old", now.Add(-time.Hour)))
	ctx := context.Background()
	if res := h.r.RefreshRejected(ctx, "codex", "work"); res.Outcome != ReactiveRotated {
		t.Fatalf("first: %+v", res)
	}
	// The provider rejects the token the rotation produced. Past the 30 minute gap on purpose: the stop, not the gap, holds it.
	h.clock.now = h.clock.now.Add(refreshMinGap + time.Minute)
	for i := 0; i < 3; i++ {
		if res := h.r.RefreshRejected(ctx, "codex", "work"); res.Outcome != ReactiveSkipped || runner.callCount() != 1 {
			t.Fatalf("poll %d after the rotation: %+v calls=%d, want no second spend", i, res, runner.callCount())
		}
	}
	n, alert := countAlerts(h, "reactive_refresh_stopped")
	if n != 1 || !strings.Contains(alert, "codex/work: the account rejects freshly refreshed tokens; log in again") {
		t.Fatalf("stop alerts = %d (%q), want exactly one with the fixed wording", n, alert)
	}

	// A profile switch (or an API import) clears the stop; the next rejection spends again, once.
	h.r.ClearReactiveStop("codex")
	if res := h.r.RefreshRejected(ctx, "codex", "work"); res.Outcome != ReactiveRotated || runner.callCount() != 2 {
		t.Fatalf("after the stop was cleared: %+v calls=%d, want one more spend", res, runner.callCount())
	}
	if res := h.r.RefreshRejected(ctx, "codex", "work"); res.Outcome != ReactiveSkipped || runner.callCount() != 2 {
		t.Fatalf("the new fresh token rejected again: %+v calls=%d", res, runner.callCount())
	}
	if n, _ := countAlerts(h, "reactive_refresh_stopped"); n != 2 {
		t.Fatalf("a stop reached after the clear must announce itself again: %d alerts", n)
	}

	// A different credential in the profile (a login the reconciler imported) ends the stop with no explicit call.
	if err := os.WriteFile(p.CredPath, []byte(codexFull(now.Add(240*time.Hour), "tok_cccc", "refresh-login", now.Add(-30*time.Minute))), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := h.r.RefreshRejected(ctx, "codex", "work"); res.Outcome != ReactiveRotated || runner.callCount() != 3 {
		t.Fatalf("after a new login: %+v calls=%d, want one more spend", res, runner.callCount())
	}
}

func TestReactiveRefreshGapBoundsSpendsOnDifferentRejectedTokens(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: func(call int, dir string) (refreshRunResult, error) {
		return refreshRunResult{Started: true}, nil // rotates nothing
	}}
	h := newRefreshHarness(t, runner)
	p := h.addProfile("codex", "work", true, codexFull(now.Add(240*time.Hour), "tok_aaaa", "refresh-old", now.Add(-time.Hour)))
	ctx := context.Background()
	h.r.RefreshRejected(ctx, "codex", "work")
	// Another rejected token shows up (not one this refresher produced), within the gap: no spend.
	if err := os.WriteFile(p.CredPath, []byte(codexFull(now.Add(240*time.Hour), "tok_bbbb", "refresh-b", now)), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := h.r.RefreshRejected(ctx, "codex", "work"); res.Outcome != ReactiveSkipped || runner.callCount() != 1 {
		t.Fatalf("inside the gap: %+v calls=%d", res, runner.callCount())
	}
	h.clock.now = h.clock.now.Add(refreshMinGap + time.Minute)
	h.r.RefreshRejected(ctx, "codex", "work")
	if runner.callCount() != 2 {
		t.Fatalf("after the gap: calls=%d, want 2", runner.callCount())
	}
}
