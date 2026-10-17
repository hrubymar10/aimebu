package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hrubymar10/aimebu/internal/credtarget"
	"github.com/hrubymar10/aimebu/internal/switcher"
	"github.com/hrubymar10/aimebu/internal/usages"
)

// credentialAlertDedupe is how long an identical credential alert stays quiet:
// a failing reconcile import or fan-out repeats every pass.
const credentialAlertDedupe = time.Hour

// startTokenCopies wires the token-only credential model into the server: the
// live-file fan-out, the Switch hooks, the reconciler and the refresher. Every
// refresher and fan-out are gated on swMgr.TokenCopies(); the settings
// controller can turn on migration at runtime. The marker is re-read on every
// pass, so migration activates the model without a restart.
//
// Startup order matters: the refresher runs the reconciler pass (SetReconcile)
// BEFORE its first due pass, so a /login that landed while the server was down is imported and
// stripped before anything tries to fan a refreshed credential out over it.
//
// The refresher's Run loop is started by usages.Manager.Start (called after this
// by Run), which is its single owner; Run is also idempotent. configure is for
// tests: it can swap the CLI executor before anything runs.
func (s *store) startTokenCopies(ctx context.Context, wg *sync.WaitGroup, swMgr *switcher.Manager, um *usages.Manager, serverDir string, configure ...func(*usages.TargetRefresher)) *usages.TargetRefresher {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Printf("credtarget: no home directory, token-copy model not started: %v", err)
		return nil
	}
	reg := credtarget.NewRegistry(serverDir, home)
	reg.LivePath = swMgr.LiveCredPath
	if err := reg.Load(); err != nil {
		log.Printf("credtarget: load target registry: %v", err)
	}
	writer := &credtarget.Writer{}
	fan := &credtarget.FanOut{
		Registry: reg,
		Writer:   writer,
		Active:   swMgr.Active,
		WithLock: swMgr.WithLock,
	}
	um.SetTargetFanOut(fan)

	// The refresher dedupes edges itself (and re-arms them on recovery), so its
	// sink must not swallow a repeat of a failure that recovered in between; the
	// reconciler's failures repeat every pass and keep the hour-long dedupe.
	refresherAlert := s.credentialAlertSink(false)
	alert := s.credentialAlertSink(true)
	tr := usages.NewTargetRefresher(um)
	tr.SetEnabled(swMgr.TokenCopies)
	tr.SetToolEnabled(swMgr.TokenCopiesFor)
	tr.SetPendingLogin(func(tool string) bool {
		path, err := swMgr.LiveCredPath(tool)
		if err != nil {
			return true
		}
		data, err := os.ReadFile(path)
		return (err != nil && !os.IsNotExist(err)) || (err == nil && credtarget.HasRefreshToken(tool, data))
	})
	tr.SetAlertHook(refresherAlert)
	um.SetTargetRefresher(tr)

	swMgr.SetTokenCopyHooks(switcher.TokenCopyHooks{
		FanOutActive:    fan.FanOutActive,
		ReplaceActive:   fan.FanOutActiveReplacing,
		ClearActive:     fan.ClearActive,
		RefreshIncoming: tr.RefreshIncoming,
		RestoreFull: func(tool string, full []byte) error {
			path, err := swMgr.LiveCredPath(tool)
			if err != nil {
				return err
			}
			return writer.WriteFullTarget(credtarget.Target{Tool: tool, Path: path}, full)
		},
	})

	rec := &credtarget.Reconciler{
		Registry:  reg,
		Writer:    writer,
		FanOut:    fan,
		Store:     switcher.NewCredStore(swMgr),
		Home:      home,
		Alert:     alert,
		Ready:     swMgr.TokenCopies,
		ReadyTool: swMgr.TokenCopiesFor,
	}
	// The refresher runs the reconciler pass itself before its first due pass
	// (SetReconcile); the reconciler's own loop then keeps ticking every 60 s.
	tr.SetReconcile(rec.Pass)
	last := map[string]usages.TokenAutoRefreshToolState{}
	um.SetTokenCopySeed(swMgr.TokenCopiesFor)
	um.SetTokenCopyController(func(ctx context.Context, on bool) map[string]usages.TokenAutoRefreshToolState {
		states := make(map[string]usages.TokenAutoRefreshToolState, 2)
		for _, tool := range switcher.Tools() {
			state := usages.TokenAutoRefreshToolState{Status: "off"}
			migrated := swMgr.TokenCopiesFor(tool)
			switch {
			case on && migrated:
				state.Status = "migrated"
			case on:
				if !um.TokenAutoRefreshAvailable() {
					state = usages.TokenAutoRefreshToolState{Status: "waiting", Reason: "harness-docker-ctrl is unavailable"}
				} else if result, err := swMgr.MigrateToTokenCopies(switcher.MigrateOptions{Tools: []string{tool}}); err != nil {
					state = usages.TokenAutoRefreshToolState{Status: "waiting", Reason: usages.RedactDetail(err.Error())}
				} else if swMgr.TokenCopiesFor(tool) {
					state.Status = "migrated"
				} else if len(result.Tools) > 0 {
					if strings.Contains(result.Tools[0].Status, "no active profile") {
						state.Status = "n/a"
					} else {
						state = usages.TokenAutoRefreshToolState{Status: "waiting", Reason: result.Tools[0].Status}
					}
				}
			case !on:
				err := swMgr.UnmigrateTokenCopies(tool)
				if errors.Is(err, switcher.ErrLoginPending) {
					if reconcileErr := rec.Pass(ctx); reconcileErr == nil {
						err = swMgr.UnmigrateTokenCopies(tool)
					} else {
						err = reconcileErr
					}
				}
				if err != nil {
					if migrated {
						state = usages.TokenAutoRefreshToolState{Status: "waiting", Reason: usages.RedactDetail(err.Error())}
					} else {
						log.Printf("credtarget: %s active-link repair: %s", tool, usages.RedactDetail(err.Error()))
					}
				}
			}
			states[tool] = state
			if state.Status != last[tool].Status {
				log.Printf("credtarget: %s token auto-refresh: %s %s", tool, state.Status, state.Reason)
				last[tool] = state
			}
		}
		return states
	})
	for _, fn := range configure { // tests swap the CLI executor before anything runs
		fn(tr)
	}
	// The refresher itself is started by usages.Manager.Start (it owns that
	// goroutine); starting it here too would run two journal owners at once.
	wg.Add(1)
	go func() {
		defer wg.Done()
		rec.Run(ctx)
	}()
	return tr
}

// credentialAlertSink returns the alert callback of the credential pieces: it
// logs, pushes a `credential_alert` meta event to the web UI and, for the kinds a
// human has to act on, posts a persisted system message. dedupe drops an
// identical (kind, detail) pair seen within credentialAlertDedupe. The refresher
// also exposes its alerts through the usages response banner.
//
// detail can carry CLI output and error text (paths, possibly a token a failing
// CLI echoed), so only the log sees it, redacted. The meta frame goes to every
// websocket client and carries the kind alone; the system message is readable
// by every member of the room and carries a fixed sentence plus the subject.
func (s *store) credentialAlertSink(dedupe bool) func(kind, detail string) {
	var mu sync.Mutex
	seen := map[string]time.Time{}
	return func(kind, detail string) {
		if dedupe {
			key := kind + "\x00" + detail
			mu.Lock()
			if at, ok := seen[key]; ok && time.Since(at) < credentialAlertDedupe {
				mu.Unlock()
				return
			}
			seen[key] = time.Now()
			mu.Unlock()
		}
		log.Printf("credential alert: %s: %s", kind, usages.RedactDetail(detail))
		s.broadcastMeta(MetaEvent{Type: "credential_alert", Data: map[string]string{"kind": kind}})
		if credentialAlertNeedsHuman(kind) {
			s.emitCredentialSystemMessage(kind, detail)
		}
	}
}

// credentialAlertNeedsHuman reports the edge kinds that end in "log in again":
// the provider rejected the refresh token, a refresh may have been spent without
// a result, or the access token expired with nothing able to renew it.
func credentialAlertNeedsHuman(kind string) bool {
	switch kind {
	case "reauth_required", "needs_relogin_soon", "expired":
		return true
	}
	return false
}

// emitCredentialSystemMessage tells the human members of every room (the alert
// belongs to no room): a persisted, attention-raising system message, like the
// agent-offline alert. Rooms without a human member are skipped.
func (s *store) emitCredentialSystemMessage(kind, detail string) {
	subject := credentialAlertSubject(detail)
	type target struct {
		room   string
		humans []string
	}
	var targets []target
	s.mu.RLock()
	for roomID, room := range s.rooms {
		var humans []string
		for _, memberID := range room.Members {
			if a, ok := s.agents[memberID]; ok && a.Kind == "human" {
				humans = append(humans, a.ID)
			}
		}
		if len(humans) > 0 {
			targets = append(targets, target{room: roomID, humans: humans})
		}
	}
	s.mu.RUnlock()
	sort.Slice(targets, func(i, j int) bool { return targets[i].room < targets[j].room })
	body := fmt.Sprintf("Credential alert (%s): %s%s", strings.ReplaceAll(kind, "_", " "), subject, credentialAlertSentence(kind))
	for _, t := range targets {
		s.emitSystemMessageFull(t.room, body, t.humans, true)
	}
}

// credentialAlertSubject is the "claude/main" or "claude live" a detail starts
// with, followed by ": ", or "" when the detail does not start with a plain
// subject. Everything after it is free text and is never posted.
func credentialAlertSubject(detail string) string {
	head, _, ok := strings.Cut(detail, ": ")
	if !ok || head == "" || len(head) > 80 {
		return ""
	}
	for _, r := range head {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_-./ @", r)) {
			return ""
		}
	}
	if usages.RedactDetail(head) != head {
		return ""
	}
	return head + ": "
}

// credentialAlertSentence is the fixed text of a human-facing alert kind.
func credentialAlertSentence(kind string) string {
	switch kind {
	case "reauth_required":
		return "the provider rejected the refresh token. Log in again."
	case "needs_relogin_soon":
		return "a refresh may have been spent without a result and will not be retried. Log in again before the access token expires."
	case "expired":
		return "the access token has expired."
	}
	return "see the server log."
}
