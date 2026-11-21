package usages

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/usages/providers"
)

const warmupGap = time.Hour

type warmupCandidate struct {
	profile StoredProfile
	oldest  time.Time
	hash    string
}

func warmupWindowOpen(snapshot Profile, now time.Time) (bool, time.Time) {
	if snapshot.Status != "ok" || !snapshot.HasCredentials {
		return false, time.Time{}
	}
	var oldest time.Time
	for _, w := range snapshot.Windows {
		switch w.Key {
		case "session":
			if w.ResetAt != nil {
				oldest = *w.ResetAt
				if now.Before(*w.ResetAt) {
					return false, oldest
				}
			}
		case "weekly", "codex_spark_weekly":
			if w.PercentUsed >= 100 && (w.ResetAt == nil || now.Before(*w.ResetAt)) {
				return false, oldest
			}
		}
	}
	return true, oldest
}

func (r *TargetRefresher) warmupQuiet(p StoredProfile, now time.Time) bool {
	if p.SwitchedAwayAt != nil && now.Sub(*p.SwitchedAwayAt) < targetSwitchAwayGrace {
		return true
	}
	if p.Tool == "codex" {
		if at, ok := r.migrationTime(p.Tool); ok && now.Sub(at) < codexMigrationQuiet {
			return true
		}
	}
	return false
}

// WarmupDue opens an idle provider session window with one short CLI request.
// It never expires a copied access token to force a refresh. If the CLI chooses
// to rotate anyway, the existing spend journal and CAS commit protect it.
func (r *TargetRefresher) WarmupDue(ctx context.Context) error {
	if !r.enabled() || r.cfg.WarmupSettings == nil || r.cfg.UsageProfile == nil {
		return nil
	}
	mode, schedule, enabled, err := r.cfg.WarmupSettings()
	if err != nil || !enabled || mode == "off" {
		return err
	}
	if err := providers.ValidateWarmup(mode, schedule); err != nil {
		return err
	}
	now := r.nowFn()
	if mode == "scheduled" {
		lines, _ := providers.ParseWarmupSchedule(schedule)
		minute := now.Truncate(time.Minute)
		location := r.cfg.WarmupLocation
		if location == nil {
			location = time.Local
		}
		r.mu.Lock()
		from := minute
		if !r.warmCheckedAt.IsZero() && minute.After(r.warmCheckedAt) {
			from = r.warmCheckedAt.Add(time.Minute)
			if floor := minute.Add(-5 * time.Minute); from.Before(floor) {
				from = floor
			}
		}
		r.warmCheckedAt = minute
		matched := time.Time{}
		for tick := from; !tick.After(minute); tick = tick.Add(time.Minute) {
			if providers.WarmupScheduleMatches(lines, tick.In(location)) {
				matched = tick
			}
		}
		if matched.IsZero() || r.warmMinute.Equal(matched) {
			r.mu.Unlock()
			return nil
		}
		r.warmMinute = matched
		r.mu.Unlock()
	}
	var candidates []warmupCandidate
	warmableCount := 0
	seenAccounts := map[string]bool{}
	last := r.warmupLastStore()
	for _, tool := range []string{"claude", "codex"} {
		if r.toolOn == nil || !r.toolOn(tool) {
			continue
		}
		profiles, err := r.rawProfiles(tool)
		if err != nil {
			return err
		}
		protected := map[string]bool{}
		for _, p := range profiles {
			if !p.Active && (r.cfg.WarmupPinned == nil || !r.cfg.WarmupPinned(tool, p.Name)) && !credentialPathIsLink(p.CredPath) {
				continue
			}
			if data, err := os.ReadFile(p.CredPath); err == nil {
				if token, err := refreshToken(data, tool); err == nil {
					protected[tokenHash(tool, token)] = true
				}
			}
		}
		provider := tool
		if tool == "claude" {
			provider = "claude-code"
		}
		for _, p := range profiles {
			if p.Active || !p.HasCredentials || p.CredPath == "" || credentialPathIsLink(p.CredPath) || r.cfg.WarmupPinned != nil && r.cfg.WarmupPinned(tool, p.Name) {
				continue
			}
			if r.warmupQuiet(p, now) {
				continue
			}
			snapshot, ok := r.cfg.UsageProfile(provider, p.Name)
			if !ok {
				continue
			}
			eligible, oldest := warmupWindowOpen(snapshot, now)
			if !eligible {
				continue
			}
			data, err := os.ReadFile(p.CredPath)
			if err != nil {
				continue
			}
			token, err := refreshToken(data, tool)
			if err != nil {
				continue
			}
			hash := tokenHash(tool, token)
			if protected[hash] {
				continue
			}
			accountKey := tool + "/" + hash
			if seenAccounts[accountKey] {
				continue
			}
			seenAccounts[accountKey] = true
			if _, blocked := r.stateStore().account(tool, hash); blocked {
				continue
			}
			warmableCount++
			if now.Sub(last.get(tool+"/"+p.Name)) < warmupGap || now.Sub(last.get(accountKey)) < warmupGap {
				continue
			}
			candidates = append(candidates, warmupCandidate{p, oldest, hash})
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.oldest.Equal(b.oldest) {
			if a.profile.Tool == b.profile.Tool {
				return a.profile.Name < b.profile.Name
			}
			return a.profile.Tool < b.profile.Tool
		}
		if a.oldest.IsZero() {
			return true
		}
		if b.oldest.IsZero() {
			return false
		}
		return a.oldest.Before(b.oldest)
	})
	if mode == "smart" {
		r.mu.Lock()
		gap := 5 * time.Hour / time.Duration(warmableCount)
		ready := now.Sub(r.warmSmartAt) >= gap
		r.mu.Unlock()
		if !ready {
			return nil
		}
		candidates = candidates[:1]
	}
	var errs []error
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		before := last.get(candidate.profile.Tool + "/" + candidate.profile.Name)
		if err := r.warmupProfile(ctx, candidate); err != nil && !accountReported(err) {
			errs = append(errs, fmt.Errorf("%s/%s: %w", candidate.profile.Tool, candidate.profile.Name, err))
		}
		if mode == "smart" && last.get(candidate.profile.Tool+"/"+candidate.profile.Name).After(before) {
			r.mu.Lock()
			r.warmSmartAt = now
			r.mu.Unlock()
		}
	}
	return errors.Join(errs...)
}

func (r *TargetRefresher) warmupProfile(ctx context.Context, c warmupCandidate) error {
	p := c.profile
	release, ok, err := AcquireMaintenance(r.root(), p.Tool, p.Name, p.CredPath)
	if err != nil || !ok {
		return err
	}
	defer release()
	// A switch, profile rename, target registration, or login may have changed
	// eligibility since the scan. Re-read the profile and grant under the lock.
	profiles, err := r.rawProfiles(p.Tool)
	if err != nil {
		return err
	}
	current := false
	for _, listed := range profiles {
		if listed.Name == p.Name && !listed.Active && listed.CredPath == p.CredPath && listed.HasCredentials && !r.warmupQuiet(listed, r.nowFn()) {
			current = true
			break
		}
	}
	if !current || credentialPathIsLink(p.CredPath) || r.cfg.WarmupPinned != nil && r.cfg.WarmupPinned(p.Tool, p.Name) {
		return nil
	}
	start, err := os.ReadFile(p.CredPath)
	if err != nil {
		return err
	}
	token, err := refreshToken(start, p.Tool)
	if err != nil || tokenHash(p.Tool, token) != c.hash {
		return nil
	}
	if _, blocked := r.stateStore().account(p.Tool, c.hash); blocked {
		return nil
	}
	last := r.warmupLastStore()
	if now := r.nowFn(); now.Sub(last.get(p.Tool+"/"+c.hash)) < warmupGap || now.Sub(last.get(p.Tool+"/"+p.Name)) < warmupGap {
		return nil
	}
	for _, other := range profiles {
		if other.Name == p.Name || !other.Active && (r.cfg.WarmupPinned == nil || !r.cfg.WarmupPinned(p.Tool, other.Name)) && !credentialPathIsLink(other.CredPath) {
			continue
		}
		data, err := os.ReadFile(other.CredPath)
		if err != nil {
			continue
		}
		otherToken, err := refreshToken(data, p.Tool)
		if err == nil && tokenHash(p.Tool, otherToken) == c.hash {
			return nil
		}
	}
	for _, e := range r.stateStore().journal() {
		if e.Tool == p.Tool && e.SpentHash == c.hash {
			return r.settleLeftover(ctx, e, r.account(p.Tool, c.hash))
		}
	}
	if r.available != nil && !r.available() || r.reachable != nil && !r.reachable(ctx) {
		return nil
	}
	if !r.beginSpend(ctx) {
		return context.Canceled
	}
	defer r.endSpend()
	var dir, tmpPath string
	if p.Tool == "codex" {
		dir, tmpPath, err = writeCodexAuthTempIn(r.root(), start)
	} else {
		dir, tmpPath, err = writeClaudeCredTempIn(r.root(), start, "aimebu-claude-warmup-*")
	}
	if err != nil {
		return err
	}
	keepTemp := false
	defer func() {
		if !keepTemp {
			os.RemoveAll(dir)
		}
	}()
	startedAt := r.nowFn()
	store := r.stateStore()
	entry := refreshJournalEntry{Tool: p.Tool, Name: p.Name, Phase: journalSpending, SpentHash: c.hash, TempDir: dir, StartedAt: startedAt.UTC()}
	if err := store.setJournal(entry); err != nil {
		return err
	}
	// A server shutdown must not kill a CLI that is in the middle of a warmup
	// request, and the run's own budget starts only after the harness image
	// exists (the executor bounds the run itself, after ensureImage).
	run, runErr := runRefreshExecutor(context.WithoutCancel(ctx), r.executors[p.Tool], dir, r.modelFlag(p.Tool), func() {
		entry.Phase = journalStarted
		if err := store.setJournal(entry); err != nil {
			log.Printf("usages: warmup journal start: %v", err)
		}
	})
	// Rate-limit every started request, including one that failed. A success is
	// only visible on the next usage fetch.
	if run.Started {
		if err := r.warmupLastStore().set(p.Tool+"/"+p.Name, startedAt); err != nil {
			log.Printf("usages: warmup rate journal: %v", err)
		}
		if err := r.warmupLastStore().set(p.Tool+"/"+c.hash, startedAt); err != nil {
			log.Printf("usages: warmup rate journal: %v", err)
		}
	}
	rotated, readErr := os.ReadFile(tmpPath)
	if readErr == nil && !bytes.Equal(start, rotated) && validRotated(p.Tool, rotated) &&
		(accessTokenOf(p.Tool, start) != accessTokenOf(p.Tool, rotated) || refreshTokenChanged(p.Tool, start, rotated)) {
		if err := r.commitRotated(p, start, rotated, c.hash, r.account(p.Tool, c.hash), startedAt, dir, &keepTemp); err != nil {
			return err
		}
		if next, err := refreshToken(rotated, p.Tool); err == nil {
			if err := r.warmupLastStore().set(p.Tool+"/"+tokenHash(p.Tool, next), startedAt); err != nil {
				log.Printf("usages: warmup rate journal: %v", err)
			}
		}
		return nil
	}
	if run.Started && (readErr != nil || !validRotated(p.Tool, rotated)) {
		if readErr != nil {
			runErr = fmt.Errorf("warmup credentials missing after CLI: %w", readErr)
		} else {
			runErr = errors.New("warmup credentials incomplete after CLI")
		}
	}
	attempt := classifyRefreshRun(p.Tool, run, runErr, rotated, false, token)
	if attempt.Outcome == OutcomeNotSent && attempt.Noop {
		r.clearJournal(store, p.Tool, p.Name)
		r.rearm(p.Tool + "/" + p.Name)
		return nil
	}
	if attempt.Outcome == OutcomeNotSent {
		r.clearJournal(store, p.Tool, p.Name)
		r.emit(AccountRefreshFailed, p.Tool+"/"+p.Name, attempt.Detail)
		// The account's own alert went out; the pass-level report must not repeat it.
		return fmt.Errorf("warmup %s/%s: %s: %w", p.Tool, p.Name, attempt.Detail, errAccountReported)
	}
	state := AccountNeedsReloginSoon
	if attempt.Outcome == OutcomeInvalidGrant {
		state = AccountReauthRequired
	}
	r.settle(store, refreshAccountEntry{Tool: p.Tool, Name: p.Name, State: state, Detail: attempt.Detail, SpentHash: c.hash, Since: r.nowFn().UTC()}, p.Tool, p.Name)
	r.emit(state, p.Tool+"/"+p.Name, attempt.Detail)
	return fmt.Errorf("%w: %s", ErrAccountNeedsLogin, attempt.Detail)
}

func refreshTokenChanged(tool string, before, after []byte) bool {
	a, _ := refreshToken(before, tool)
	b, _ := refreshToken(after, tool)
	return a != b
}
