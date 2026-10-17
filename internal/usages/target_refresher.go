package usages

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Token-only design (rev 3): the server is the only refresher, working on the
// profile store's regular files for EVERY profile (the active one included),
// under the existing per-account maintenance lock.

const (
	// TargetLeadClaude / TargetLeadCodex: a profile is due when it is expired or
	// expires within the lead. Claude tokens live 8 h, codex ~10 days.
	TargetLeadClaude = 60 * time.Minute
	TargetLeadCodex  = 24 * time.Hour

	// TargetAlertWindow: a target/profile this close to expiry raises the doctor
	// and UI alert.
	TargetAlertWindow = 15 * time.Minute

	// targetTick is how often the due pass runs.
	targetTick = 30 * time.Second
	// targetRunTimeout bounds one CLI refresh run.
	targetRunTimeout = 4 * time.Minute
	// targetSwitchAwayGrace is how long after a profile stops being active the
	// refresher still leaves it alone. Under the token-only model no CLI holds a
	// profile's refresh token, so only the switch itself needs to settle.
	targetSwitchAwayGrace = 2 * time.Minute
	// journalStarted is the journal phase between "spending" (about to launch) and
	// "rotated" (new credentials persisted): the CLI process exists, so the refresh
	// token may have been spent. A crash before this phase never reached the CLI.
	journalStarted = "started"

	// codexMigrationQuiet is how long after migration the refresher leaves Codex
	// profiles alone. A Codex session that started before the migration caches its
	// auth.json and proactively refreshes at five minutes before expiry; spending
	// the same token from the server in that window would be a double spend. Claude
	// re-reads its file before refreshing and finds no refresh token, so it needs no
	// quiet period.
	codexMigrationQuiet = 10 * time.Minute
	// targetNoopLimit is how many consecutive "CLI ran, rotated nothing" attempts
	// are retried quietly before the account is reported as refresh_failed.
	targetNoopLimit = 3
)

// RefreshOutcome classifies a refresh attempt (design §3, M2 spend-once).
type RefreshOutcome int

const (
	// OutcomeRefreshed: new credentials persisted and committed.
	OutcomeRefreshed RefreshOutcome = iota
	// OutcomeNotSent: nothing was written to the network (dns/refused/connect):
	// safe to retry with backoff.
	OutcomeNotSent
	// OutcomeUnknown: the request may have been processed (timeout/reset after
	// write). NEVER re-spend that refresh token: alert (needs_relogin_soon) and
	// let the tokens run to expiry.
	OutcomeUnknown
	// OutcomeInvalidGrant: the provider refused the refresh token: the account is
	// reauth_required; stop spending, alert, never wipe anything.
	OutcomeInvalidGrant
)

// Account states surfaced to doctor/UI/API.
const (
	AccountReauthRequired   = "reauth_required"
	AccountNeedsReloginSoon = "needs_relogin_soon"
	AccountRefreshFailed    = "refresh_failed"
)

// ErrTargetHoldsLogin is what a target writer returns when the file it was asked
// to rewrite still holds a refresh token: a /login landed there and has not been
// imported. It is not a failure of the refresher. Fan-out neither retries nor
// forces; the reconciler rescues, imports and strips the login. The credential
// target writer's own error wraps this one.
var ErrTargetHoldsLogin = errors.New("usages: a target holds a login that the reconciler has not imported yet")

var (
	// ErrAccountNeedsLogin is returned for a due profile whose refresh token the
	// provider rejected, or whose spend outcome is unknown: a human has to log in.
	ErrAccountNeedsLogin = errors.New("usages: the account needs a new login; its refresh token is not spent again")
	// ErrRefreshBackoff is returned while a failed account waits out its backoff.
	ErrRefreshBackoff = errors.New("usages: token refresh is backing off after a failure")
)

// TargetRefresher is the scheduler. One instance per Manager.
//
// Every refresh follows the same spend-once protocol:
//
//  1. take the per-account lock (single flight across goroutines and processes);
//  2. journal "spending" with the hash of the refresh token about to be used;
//  3. run the CLI against a throwaway copy (pluggable refreshExecutor);
//  4. classify the result from what is on disk and what the CLI said;
//  5. on success persist the new credentials first (rescue file + journal
//     "rotated"), commit them with the CAS, delete the rescue, fan out;
//  6. on any failure keep every credential as it was: not-sent retries with
//     backoff, an unknown outcome or invalid_grant is terminal until a human
//     logs in, and nothing is ever wiped.
//
// Owner: wade. Tests: T7 (server-down/restart, no token spent twice).
type TargetRefresher struct {
	m *Manager

	mu       sync.Mutex
	accounts map[string]*targetAccount // tool/token-hash -> in-memory attempt state
	alerted  map[string]bool
	state    *refreshStateStore

	// Overridable (tests and wiring).
	executors    map[string]refreshExecutor
	available    func() bool
	reachable    func(context.Context) bool
	enabled      func() bool
	toolOn       func(tool string) bool // per-tool gate on top of enabled; nil = no tool
	pendingLogin func(tool string) bool // live full credential: never spend its active stored token
	alert        func(kind, detail string)
	reconcile    func(context.Context) error
	last         *refreshLastStore
	pending      map[string][]byte                   // rotated credentials awaiting their commit, by tool/spent-hash
	migratedAt   func(tool string) (time.Time, bool) // when tool was migrated; default reads the marker
	afterLock    func()                              // test seam: runs right after the account lock is taken
	lead         map[string]time.Duration
	runTimeout   time.Duration
	interval     time.Duration
	started      atomic.Bool // set by the first Run; a second Run is a no-op
}

// targetAccount is the in-memory, non-terminal attempt state of one account.
type targetAccount struct {
	failures int
	noops    int
	nextTry  time.Time
	detail   string
}

// Environment knobs for the refresh lead (how long before expiry a profile
// becomes due), as Go durations ("90m", "36h"). A value outside
// [TargetLeadMin, TargetLeadMax], or one that does not parse, is ignored and the
// default stays. Setting a lead longer than a token's life makes every profile
// due at once, which is how an acceptance run forces a refresh.
const (
	TargetLeadClaudeEnv = "AIMEBU_TOKEN_LEAD_CLAUDE"
	TargetLeadCodexEnv  = "AIMEBU_TOKEN_LEAD_CODEX"
	TargetLeadMin       = 10 * time.Minute
	TargetLeadMax       = 14 * 24 * time.Hour
)

func targetLeadsFromEnv() map[string]time.Duration {
	leads := map[string]time.Duration{"claude": TargetLeadClaude, "codex": TargetLeadCodex}
	for tool, name := range map[string]string{"claude": TargetLeadClaudeEnv, "codex": TargetLeadCodexEnv} {
		raw := strings.TrimSpace(os.Getenv(name))
		if raw == "" {
			continue
		}
		d, err := time.ParseDuration(raw)
		if err != nil || d < TargetLeadMin || d > TargetLeadMax {
			log.Printf("usages: ignoring %s=%q: want a duration between %s and %s", name, raw, TargetLeadMin, TargetLeadMax)
			continue
		}
		leads[tool] = d
	}
	return leads
}

// NewTargetRefresher builds the refresher over m's profile lister, account locks,
// rescue root and fan-out.
func NewTargetRefresher(m *Manager) *TargetRefresher {
	return &TargetRefresher{
		m:        m,
		accounts: map[string]*targetAccount{},
		alerted:  map[string]bool{},
		executors: map[string]refreshExecutor{
			"claude": dockerRefreshExecutor{},
			"codex":  codexRefreshExecutor{},
		},
		available:  harnessDockerCtrlAvailable,
		reachable:  dockerReachable,
		enabled:    func() bool { return false },
		lead:       targetLeadsFromEnv(),
		runTimeout: targetRunTimeout,
		interval:   targetTick,
	}
}

// SetEnabled gates the refresher. It is OFF until the server turns it on (once
// the install has been migrated to the token-only model, swMgr.TokenCopies): a
// safety-critical default must be off, because an enabled refresher on a
// link-based install would spend tokens the running sessions still own.
// SetAlertHook receives edge alerts
// (kind, detail): `reauth_required`, `needs_relogin_soon`, `fanout_failed`,
// `recovery`. The hook must not block.
func (r *TargetRefresher) SetEnabled(fn func() bool) {
	if fn != nil {
		r.enabled = fn
	}
}

// SetToolEnabled narrows the refresher to the tools that have been migrated
// (`migrate-copies --tool claude` leaves codex on the link model, whose profiles
// the refresher must not touch). Nil means no tool: the gate fails closed, so a
// refresher without an explicit set never spends anything.
func (r *TargetRefresher) SetToolEnabled(fn func(tool string) bool) { r.toolOn = fn }

func (r *TargetRefresher) SetPendingLogin(fn func(tool string) bool) { r.pendingLogin = fn }

// ownsTool reports whether the refresher is active for tool.
func (r *TargetRefresher) ownsTool(tool string) bool {
	return r.enabled() && r.toolOn != nil && r.toolOn(tool)
}

func (r *TargetRefresher) SetAlertHook(fn func(kind, detail string)) { r.alert = fn }

// migrationTime is when `migrate-copies` converted tool, from the shared marker
// reader (switcher/token-copies.json). False means the tool is not migrated or
// the time is unknown, which imposes no wait.
func (r *TargetRefresher) migrationTime(tool string) (time.Time, bool) {
	if r.migratedAt != nil {
		return r.migratedAt(tool)
	}
	if r.root() == "" {
		return time.Time{}, false
	}
	marker, ok := ReadTokenCopiesMarker(r.root())
	if !ok {
		return time.Time{}, false
	}
	at, ok := marker.Since(tool)
	return at, ok && !at.IsZero()
}

// SetReconcile installs the reconciler's pass. Run calls it once at start, after
// settling the journal and BEFORE the first due pass, so a login that landed in a
// live file while the server was down is imported before any refresh could race
// it. Nil skips the step.
func (r *TargetRefresher) SetReconcile(fn func(context.Context) error) { r.reconcile = fn }

func (r *TargetRefresher) now() time.Time { return r.m.clock.Now() }

func (r *TargetRefresher) root() string { return r.m.maintRoot }

func (r *TargetRefresher) stateStore() *refreshStateStore {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == nil {
		r.state = newRefreshStateStore(r.root())
	}
	return r.state
}

// rawProfiles lists the switcher's profiles for tool. Unlike the fetch path it
// never falls back to the implicit "local" profile: the refresher only works on
// the profile store.
func (r *TargetRefresher) rawProfiles(tool string) ([]ProfileInfo, error) {
	if r.m.profileLister == nil {
		return nil, nil
	}
	return r.m.profileLister(tool)
}

func tokenHash(tool, token string) string {
	sum := sha256.Sum256([]byte(tool + "\x00" + token))
	return hex.EncodeToString(sum[:])
}

func (r *TargetRefresher) account(tool, hash string) *targetAccount {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := tool + "/" + hash
	a := r.accounts[key]
	if a == nil {
		a = &targetAccount{}
		r.accounts[key] = a
	}
	return a
}

// emit raises an alert once per (kind, key) for the life of the process.
func (r *TargetRefresher) emit(kind, key, detail string) {
	detail = cleanDetail(detail) // idempotent; covers callers that built the text from a raw error
	r.mu.Lock()
	id := kind + "/" + key
	seen := r.alerted[id]
	r.alerted[id] = true
	hook := r.alert
	r.mu.Unlock()
	log.Printf("usages: token refresh %s account=%s: %s", kind, key, detail)
	if !seen && hook != nil {
		hook(kind, key+": "+detail)
	}
}

// rearm forgets the once-per-process dedupe of every alert about one account, on
// the edge where it is healthy again (a refresh committed, a recovery finished),
// so the next failure of the same kind is announced instead of staying silent.
func (r *TargetRefresher) rearm(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.alerted {
		if strings.HasSuffix(id, "/"+key) {
			delete(r.alerted, id)
		}
	}
}

// profileExpiry reads the expiry from the credential file itself, so the due
// check never trusts a stale listing.
func profileExpiry(tool, path string) (*time.Time, error) {
	if tool == "codex" {
		_, access, err := ValidateCodexCredentials(path)
		if err != nil {
			return nil, err
		}
		return CodexTokenExpiry(access), nil
	}
	return ValidateClaudeCredentials(path)
}

// targetProfileDue is the pure eligibility predicate: the token is expired or
// expires within the lead.
func targetProfileDue(expiresAt *time.Time, now time.Time, lead time.Duration) bool {
	if expiresAt == nil {
		return false
	}
	return !now.Before(expiresAt.Add(-lead))
}

// Run ticks every 30 s until ctx is done. The first pass is the startup one:
// settle the journal a previous process left, run the reconcile pass, then the
// due pass, in that order and never in parallel.
func (r *TargetRefresher) Run(ctx context.Context) {
	if !r.started.CompareAndSwap(false, true) {
		log.Printf("usages: token refresh: Run called more than once; ignoring the extra call")
		return
	}
	// The startup pass (settle the journal a previous process left, run the
	// reconcile pass, then the first due pass) runs the first time the refresher is
	// enabled, which is at start on a migrated install and later when
	// `migrate-copies` is run against a live server. Either way the reconcile pass
	// comes before any refresh can race a login that landed in a live file.
	bootstrapped := false
	bootstrap := func() {
		if bootstrapped || !r.enabled() {
			return
		}
		bootstrapped = true
		r.recover(ctx)
		if r.reconcile != nil {
			if err := r.reconcile(ctx); err != nil {
				log.Printf("usages: token refresh: startup reconcile: %s", errString(err))
			}
		}
		_ = r.RefreshDue(ctx)
	}
	bootstrap()
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !r.enabled() {
				// A later setting ON is a fresh ownership transition: reconcile
				// any login written while OFF before the first due refresh.
				bootstrapped = false
				continue
			}
			if !bootstrapped {
				bootstrap()
				continue
			}
			_ = r.RefreshDue(ctx)
		}
	}
}

// RefreshDue refreshes every due profile (single-flight per account via the
// maintenance lock; a held lock skips the tick). Profiles are refreshed one
// after another so a slow CLI run never fans out into a burst of requests.
func (r *TargetRefresher) RefreshDue(ctx context.Context) error {
	if !r.enabled() {
		return nil
	}
	r.reportStateNotices()
	var errs []error
	for _, tool := range []string{"claude", "codex"} {
		profiles, err := r.rawProfiles(tool)
		if err != nil {
			errs = append(errs, fmt.Errorf("list %s profiles: %w", tool, err))
			continue
		}
		for _, p := range profiles {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			err := r.refreshProfile(ctx, p, false)
			if err != nil && !errors.Is(err, ErrRefreshBackoff) && !errors.Is(err, ErrAccountNeedsLogin) {
				errs = append(errs, fmt.Errorf("%s/%s: %w", tool, p.Name, err))
			}
		}
	}
	r.emitExpiryAlerts()
	return errors.Join(errs...)
}

// RefreshIncoming refreshes one profile right now if it is within its lead; Switch
// calls it for the incoming profile and refuses the switch (409) when it fails. A
// profile that is not due returns nil.
//
// Locking contract: the caller holds the account lock and keeps it. This method
// never takes, drops or re-takes it (the commit needs the switcher lock, so the
// caller must not hold that one yet). The fan-out of the refreshed credential
// therefore runs under the caller's account lock, and the caller's own rewrite of
// the targets follows under the same one. The lock is keyed by the refresh token
// the caller locked; after a successful refresh the profile holds a new token, but
// it is no longer due, so no other job has a reason to touch it before the
// switch completes.
func (r *TargetRefresher) RefreshIncoming(ctx context.Context, tool, profile string) error {
	profiles, err := r.rawProfiles(tool)
	if err != nil {
		return fmt.Errorf("list %s profiles: %w", tool, err)
	}
	for _, p := range profiles {
		if p.Name == profile {
			return r.refreshProfile(ctx, p, true)
		}
	}
	return fmt.Errorf("profile %s/%s not found", tool, profile)
}

// refreshProfile runs one profile through the protocol. holdsLock says the
// caller already holds the account lock (RefreshIncoming); otherwise the lock is
// taken here and a busy account is skipped.
func (r *TargetRefresher) refreshProfile(ctx context.Context, p ProfileInfo, holdsLock bool) error {
	tool := p.Tool
	if (tool != "claude" && tool != "codex") || !p.HasCredentials || p.CredPath == "" {
		return nil
	}
	if r.toolOn == nil || !r.toolOn(tool) {
		return nil // this tool is still on the link model (or no tool set was given)
	}
	if p.Active && r.pendingLogin != nil && r.pendingLogin(tool) {
		return nil // /login or an interrupted un-migration owns the live refresh token
	}
	// A linked stored file is the live file itself (an install that has not been
	// migrated): spending its token would revoke the family under the sessions
	// running on it.
	if credentialPathIsLink(p.CredPath) {
		return nil
	}
	now := r.now()
	// The switch-away grace protects a profile that was just switched AWAY from.
	// The incoming profile (holdsLock) is about to become active and must be
	// refreshed if due, however recently it was last active: a quick A -> B -> A
	// would otherwise hand a due profile to Switch.
	if !holdsLock && p.SwitchedAwayAt != nil && now.Sub(*p.SwitchedAwayAt) < targetSwitchAwayGrace {
		return nil
	}
	// The due pass leaves Codex alone right after migration (see codexMigrationQuiet).
	// An incoming profile in a switch (holdsLock) is exempt: a switch is never
	// blocked by it, and the refresh it asks for is the one the caller needs.
	if tool == "codex" && !holdsLock {
		if at, ok := r.migrationTime(tool); ok && now.Sub(at) < codexMigrationQuiet {
			return nil
		}
	}
	expiry, err := profileExpiry(tool, p.CredPath)
	if err != nil {
		return nil
	}
	lead, tooSoon := r.effectiveLead(tool, p.Name, storedTokenHash(tool, p.CredPath), now)
	if tooSoon || !targetProfileDue(expiry, now, lead) {
		return nil
	}
	start, err := os.ReadFile(p.CredPath)
	if err != nil {
		return nil
	}
	token, err := refreshToken(start, tool)
	if err != nil {
		return nil // no refresh token: not a full credential, nothing to spend
	}
	hash := tokenHash(tool, token)
	store := r.stateStore()
	if entry, ok := store.account(tool, hash); ok {
		r.emit(entry.State, tool+"/"+p.Name, entry.Detail)
		return fmt.Errorf("%w (%s)", ErrAccountNeedsLogin, entry.State)
	}
	acct := r.account(tool, hash)
	r.mu.Lock()
	wait := acct.nextTry
	r.mu.Unlock()
	if now.Before(wait) {
		return ErrRefreshBackoff
	}
	if !holdsLock {
		release, ok := r.m.maintGuard.acquire(tool, p)
		if !ok {
			return nil
		}
		defer release()
	}
	if r.afterLock != nil {
		r.afterLock()
	}
	// A journal entry for this very token means an earlier run ended without a
	// settled result: the CLI may have spent it, or its new credentials sit in a
	// rescue file whose commit failed. Settle that (retry the commit from the
	// rescue, or mark the account) instead of ever spending the token again.
	if e, ok := r.journalFor(tool, p.Name, hash); ok {
		return r.settleLeftover(ctx, e, acct)
	}
	if r.available != nil && !r.available() {
		return r.noteFailure(acct, tool, p.Name, "harness-docker-ctrl is not installed", false)
	}
	if r.reachable != nil && !r.reachable(ctx) {
		return r.noteFailure(acct, tool, p.Name, "docker is not reachable", false)
	}
	// Re-read under the lock and compare the refresh token with the one this tick
	// decided on: a login imported (or a commit made) between the decision and the
	// lock replaced it, and spending the older token would be wasted at best and
	// revoke the family at worst. The tick is abandoned; the next one starts from
	// the new credential.
	start, err = os.ReadFile(p.CredPath)
	if err != nil {
		return nil
	}
	if again, err := refreshToken(start, tool); err != nil || tokenHash(tool, again) != hash {
		return nil
	}
	return r.spend(ctx, p, start, expiry, hash, acct)
}

// spend runs the CLI against a throwaway copy and acts on the classified result.
func (r *TargetRefresher) spend(ctx context.Context, p ProfileInfo, start []byte, oldExpiry *time.Time, hash string, acct *targetAccount) error {
	tool := p.Tool
	runStarted := r.now()
	var dir, tmpPath string
	var err error
	if tool == "codex" {
		dir, tmpPath, err = writeCodexAuthTempIn(r.root(), start)
	} else {
		dir, tmpPath, err = writeClaudeCredTempIn(r.root(), start, "aimebu-claude-refresh-*")
	}
	if err != nil {
		return r.noteFailure(acct, tool, p.Name, "cannot create the temporary credential copy: "+cleanDetail(err.Error()), false)
	}
	// The temp dir normally goes when the run is settled. commitRotated asks to
	// keep it while it holds the only copy of rotated credentials that neither the
	// rescue file nor the journal could record.
	keepTemp := false
	defer func() {
		if !keepTemp {
			os.RemoveAll(dir)
		}
	}()
	if tool == "codex" {
		err = forceExpireCodexTempCredentials(tmpPath)
	} else {
		err = forceExpireClaudeTempCredentials(tmpPath)
	}
	if err != nil {
		return r.noteFailure(acct, tool, p.Name, "cannot prepare the temporary credential copy: "+cleanDetail(err.Error()), false)
	}
	store := r.stateStore()
	// Journal BEFORE the CLI runs: from here the refresh token may be spent. If
	// the journal cannot be written the run does not start.
	if err := store.setJournal(refreshJournalEntry{Tool: tool, Name: p.Name, Phase: journalSpending, SpentHash: hash, TempDir: dir, StartedAt: runStarted.UTC()}); err != nil {
		log.Printf("usages: ERROR token refresh %s/%s: cannot journal the spend, not refreshing: %s", tool, p.Name, errString(err))
		return r.noteFailure(acct, tool, p.Name, "cannot write the refresh journal: "+cleanDetail(err.Error()), false)
	}
	flag := r.modelFlag(tool)
	// A server shutdown must not kill a CLI that is in the middle of spending the
	// token: the run is bounded by its own timeout, not by ctx, and the commit
	// below proceeds even if ctx was cancelled meanwhile. The pass starts no
	// further run once ctx is done.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.runTimeout)
	onStart := func() {
		// The CLI process exists: from here the token may be spent. A crash
		// before this point never reached it, and recovery treats it as not sent.
		if err := store.setJournal(refreshJournalEntry{Tool: tool, Name: p.Name, Phase: journalStarted, SpentHash: hash, TempDir: dir, StartedAt: runStarted.UTC()}); err != nil {
			log.Printf("usages: ERROR token refresh %s/%s: cannot journal that the CLI started: %s", tool, p.Name, errString(err))
		}
	}
	run, runErr := runRefreshExecutor(runCtx, r.executors[tool], dir, flag, onStart)
	cancel()

	rotated, _ := os.ReadFile(tmpPath)
	startToken, _ := refreshToken(start, tool)
	advanced := r.rotatedAdvanced(tool, p, tmpPath, start, oldExpiry)
	attempt := classifyRefreshRun(tool, run, runErr, rotated, advanced, startToken)
	key := tool + "/" + p.Name
	switch attempt.Outcome {
	case OutcomeRefreshed:
		return r.commitRotated(p, start, attempt.Rotated, hash, acct, runStarted, dir, &keepTemp)
	case OutcomeNotSent:
		r.clearJournal(store, tool, p.Name)
		if attempt.Noop {
			return r.noteFailure(acct, tool, p.Name, attempt.Detail, true)
		}
		return r.noteFailure(acct, tool, p.Name, attempt.Detail, false)
	case OutcomeInvalidGrant:
		r.settle(store, refreshAccountEntry{Tool: tool, Name: p.Name, State: AccountReauthRequired, Detail: attempt.Detail, SpentHash: hash, Since: r.now().UTC()}, tool, p.Name)
		r.emit(AccountReauthRequired, key, attempt.Detail)
		return fmt.Errorf("%w: %s", ErrAccountNeedsLogin, attempt.Detail)
	default:
		r.settle(store, refreshAccountEntry{Tool: tool, Name: p.Name, State: AccountNeedsReloginSoon, Detail: attempt.Detail, SpentHash: hash, Since: r.now().UTC()}, tool, p.Name)
		r.emit(AccountNeedsReloginSoon, key, attempt.Detail)
		return fmt.Errorf("%w: %s", ErrAccountNeedsLogin, attempt.Detail)
	}
}

// rotatedAdvanced says whether the temp credential holds a usable, newer login
// than the one the run started with.
func (r *TargetRefresher) rotatedAdvanced(tool string, p ProfileInfo, tmpPath string, start []byte, oldExpiry *time.Time) bool {
	return credentialAdvanced(tool, start, readFileOrNil(tmpPath))
}

func readFileOrNil(path string) []byte {
	data, _ := os.ReadFile(path)
	return data
}

func (r *TargetRefresher) modelFlag(tool string) string {
	if r.m.store == nil {
		return ""
	}
	var flag string
	_ = r.m.store.WithLock(func() error {
		cfg, err := r.m.store.LoadConfig()
		if err != nil {
			return err
		}
		if tool == "codex" {
			flag = cfg.CodexModelFlag
		} else {
			flag = cfg.ClaudeModelFlag
		}
		return nil
	})
	return flag
}

// persistRotated writes the new credentials to a rescue file BEFORE they are
// committed, so a crash between the spend and the commit loses nothing.
func (r *TargetRefresher) persistRotated(tool, name string, rotated []byte) (string, error) {
	root := r.root()
	if root == "" {
		return "", nil
	}
	dir := filepath.Join(root, "backups", tool)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, RotatedRescueName(name, time.Now().UTC()))
	if err := atomicWriteBytes(path, rotated, 0o600); err != nil {
		return "", err
	}
	// Rescue files kept by an aborted or failed commit would pile up; keep the
	// newest few per profile (the one just written is always among them).
	pruneRotatedRescues(dir, name)
	return path, nil
}

// holder is a profile whose store file holds a given (spent) refresh token.
type holder struct {
	p    ProfileInfo
	data []byte
}

// holders lists every profile of tool whose store file holds the refresh token
// with this hash. The same account can be stored under more than one profile, and
// a run spends one token for all of them, so a commit has to reach every holder.
// A linked store file (an unmigrated install) is never touched.
func (r *TargetRefresher) holders(tool, hash string) ([]holder, error) {
	profiles, err := r.rawProfiles(tool)
	if err != nil {
		return nil, err
	}
	var out []holder
	for _, p := range profiles {
		if p.CredPath == "" || credentialPathIsLink(p.CredPath) {
			continue
		}
		data, err := os.ReadFile(p.CredPath)
		if err != nil {
			continue
		}
		if token, err := refreshToken(data, tool); err == nil && tokenHash(tool, token) == hash {
			out = append(out, holder{p: p, data: data})
		}
	}
	return out, nil
}

// pendingHolders is holders without the profiles that already carry the rotated
// login (same access token): a provider that keeps the refresh token unchanged
// leaves a committed profile still "holding" the spent hash, and it must not be
// committed again or counted as unfinished.
func (r *TargetRefresher) pendingHolders(tool, hash string, rotated []byte) ([]holder, error) {
	hs, err := r.holders(tool, hash)
	if err != nil {
		return nil, err
	}
	want := accessTokenOf(tool, rotated)
	var out []holder
	for _, h := range hs {
		if want != "" && accessTokenOf(tool, h.data) == want {
			continue
		}
		out = append(out, h)
	}
	return out, nil
}

// commitToHolders commits the rotated credentials to every profile still holding
// the spent refresh token, each through the compare-and-swap. It returns the
// names it committed. A non-nil error means a profile STILL holds the spent token
// afterwards (an I/O error, a lock failure, a listing error): the credentials are
// not committed everywhere and the caller must retry the commit from its rescue
// copy and must not spend again. A profile that was removed or re-logged in
// meanwhile no longer holds the token; the compare-and-swap kept a rescue file for
// it and that is not an error.
func (r *TargetRefresher) commitToHolders(tool, hash string, rotated []byte, runStarted time.Time) ([]string, error) {
	hs, err := r.pendingHolders(tool, hash, rotated)
	if err != nil {
		return nil, fmt.Errorf("list %s profiles: %w", tool, err)
	}
	list := func() ([]ProfileInfo, error) { return r.rawProfiles(tool) }
	var committed []string
	var errs []error
	for _, h := range hs {
		if err := commitRotatedCredentials(tool, r.m.maintWithLock, list, h.p.Name, h.p.CredPath, h.data, rotated, r.root(), runStarted); err != nil {
			errs = append(errs, fmt.Errorf("%s/%s: %w", tool, h.p.Name, err))
			continue
		}
		committed = append(committed, h.p.Name)
	}
	left, err := r.pendingHolders(tool, hash, rotated)
	if err != nil {
		return committed, errors.Join(append(errs, fmt.Errorf("re-list %s profiles: %w", tool, err))...)
	}
	if len(left) > 0 {
		return committed, errors.Join(append(errs, fmt.Errorf("%d profile(s) still hold the spent refresh token", len(left)))...)
	}
	if len(errs) > 0 {
		log.Printf("usages: token refresh %s: a profile changed during the commit and kept a rescue file: %s", tool, errString(errors.Join(errs...)))
	}
	return committed, nil
}

// commitRotated is the persist-first success path: write the new credentials to a
// rescue file, journal `rotated`, commit them to every holder of the spent token,
// then delete the rescue and fan out. If any holder cannot be committed the
// journal and the rescue stay, so the next pass retries the COMMIT (never the
// spend).
func (r *TargetRefresher) commitRotated(p ProfileInfo, start, rotated []byte, hash string, acct *targetAccount, runStarted time.Time, tempDir string, keepTemp *bool) error {
	tool := p.Tool
	key := tool + "/" + p.Name
	store := r.stateStore()
	rescue, err := r.persistRotated(tool, p.Name, rotated)
	atRisk := false
	if err != nil {
		// Without the rescue file the commit has only the in-memory copy below;
		// still try it, but shout: a crash now would lose the new credentials.
		log.Printf("usages: ERROR token refresh %s: cannot persist the rotated credentials first: %v", key, cleanDetail(err.Error()))
		atRisk = true
	}
	r.holdPending(tool, hash, rotated)
	entry := refreshJournalEntry{Tool: tool, Name: p.Name, Phase: journalRotated, SpentHash: hash, Rescue: rescue, StartedAt: runStarted.UTC()}
	if rescue == "" {
		entry.TempDir = tempDir // recovery can still read the new credentials from the temp copy
	}
	if err := store.setJournal(entry); err != nil {
		log.Printf("usages: ERROR token refresh %s: cannot journal the rotated credentials: %v", key, cleanDetail(err.Error()))
		atRisk = true
	}
	if atRisk && keepTemp != nil {
		*keepTemp = true // the temp copy may be the only durable one until the commit lands
	}
	settled := func() {
		if keepTemp != nil {
			*keepTemp = false
		}
	}
	committed, err := r.commitToHolders(tool, hash, rotated, runStarted)
	if err != nil {
		detail := cleanDetail("commit failed, will retry from the rescue copy: " + err.Error())
		r.mu.Lock()
		acct.failures++
		acct.nextTry = r.now().Add(targetBackoff(acct.failures))
		acct.detail = detail
		r.mu.Unlock()
		r.emit("commit_failed", key, detail)
		return fmt.Errorf("commit rotated %s credentials: %s", key, detail)
	}
	if len(committed) == 0 {
		// No profile holds the spent token any more: it was removed or logged in
		// again while the CLI ran, so the new credentials have no profile to go to.
		// They stay in the rescue copy (and the one the compare-and-swap would
		// have written) for a human; nothing is lost and nothing is retried.
		r.clearJournal(store, tool, p.Name)
		r.dropPending(tool, hash)
		settled()
		detail := "the profile changed or was removed while the refresh ran; the rotated credentials were kept in a rescue file"
		r.emit("commit_aborted", key, detail)
		return fmt.Errorf("commit rotated %s credentials: %s", key, detail)
	}
	r.finishCommit(tool, p.Name, hash, rescue, acct)
	settled()
	log.Printf("usages: token refresh %s outcome=rotated", key)
	for _, name := range committed {
		r.rearm(tool + "/" + name)
		r.noteRotation(tool, name, rotated)
		r.fanOut(tool, name, p.CredPath)
	}
	return nil
}

// settle records a terminal account verdict, then ends the journal entry. When
// the verdict cannot be saved the entry stays, so the next start re-derives it
// instead of forgetting that the token may be spent.
func (r *TargetRefresher) settle(store *refreshStateStore, e refreshAccountEntry, tool, name string) {
	if err := store.setAccount(e); err != nil {
		log.Printf("usages: token refresh %s/%s: saving the account state failed, keeping the journal entry: %s", tool, name, errString(err))
		return
	}
	r.clearJournal(store, tool, name)
}

// clearJournal ends a journal entry. A failure is logged, not fatal: a leftover
// entry is settled by the next recovery pass, which never spends a token.
func (r *TargetRefresher) clearJournal(store *refreshStateStore, tool, name string) {
	if err := store.clearJournal(tool, name); err != nil {
		log.Printf("usages: token refresh %s/%s: clearing the journal entry failed: %s", tool, name, errString(err))
	}
}

// finishCommit settles a refresh whose commit reached every holder.
func (r *TargetRefresher) finishCommit(tool, name, hash, rescue string, acct *targetAccount) {
	if rescue != "" {
		_ = os.Remove(rescue)
	}
	r.clearJournal(r.stateStore(), tool, name)
	r.dropPending(tool, hash)
	if acct != nil {
		r.mu.Lock()
		acct.failures, acct.noops, acct.detail = 0, 0, ""
		acct.nextTry = time.Time{}
		r.mu.Unlock()
	}
}

// holdPending keeps the rotated credentials in memory until their commit
// succeeds, as the fallback for a retry when the rescue file could not be
// written. It is dropped on success.
func (r *TargetRefresher) holdPending(tool, hash string, rotated []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == nil {
		r.pending = map[string][]byte{}
	}
	r.pending[tool+"/"+hash] = append([]byte(nil), rotated...)
}

func (r *TargetRefresher) dropPending(tool, hash string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pending, tool+"/"+hash)
}

func (r *TargetRefresher) pendingFor(tool, hash string) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pending[tool+"/"+hash]
}

// journalFor returns the journal entry of (tool, name) if it belongs to the
// refresh token with this hash.
func (r *TargetRefresher) journalFor(tool, name, hash string) (refreshJournalEntry, bool) {
	for _, e := range r.stateStore().journal() {
		if e.Tool == tool && e.Name == name && e.SpentHash == hash {
			return e, true
		}
	}
	return refreshJournalEntry{}, false
}

// settleLeftover settles a journal entry found for a profile that is about to be
// refreshed: an earlier run ended without a settled result. Spending is never an
// option here. A failed settle backs the account off and keeps the entry.
func (r *TargetRefresher) settleLeftover(ctx context.Context, e refreshJournalEntry, acct *targetAccount) error {
	key := e.Tool + "/" + e.Name
	if err := r.recoverEntry(ctx, e); err != nil {
		detail := cleanDetail(err.Error())
		r.mu.Lock()
		acct.failures++
		acct.nextTry = r.now().Add(targetBackoff(acct.failures))
		acct.detail = detail
		r.mu.Unlock()
		return fmt.Errorf("settling the interrupted refresh of %s: %s", key, detail)
	}
	return nil
}

// fanOut distributes the committed credential to the token-only targets. The
// store is already committed, so a failure here is healed by the reconciler.
func (r *TargetRefresher) fanOut(tool, name, fallbackPath string) {
	if r.m.targetFanOut == nil {
		return
	}
	path := fallbackPath
	if profiles, err := r.rawProfiles(tool); err == nil {
		for _, q := range profiles {
			if q.Name == name && q.CredPath != "" {
				path = q.CredPath
			}
		}
	}
	full, err := os.ReadFile(path)
	if err != nil {
		r.emit("fanout_failed", tool+"/"+name, "cannot read the committed credential: "+err.Error())
		return
	}
	if err := r.m.targetFanOut.FanOut(tool, name, full); err != nil {
		if errors.Is(err, ErrTargetHoldsLogin) {
			// A /login is waiting in a target. Never retry and never force: the
			// reconciler rescues, imports and strips it, then rewrites the target.
			r.emit("login_pending", tool+"/"+name, "a login in a live credential file is waiting for the reconciler; not overwritten")
			return
		}
		r.emit("fanout_failed", tool+"/"+name, errString(err))
	}
}

// noteFailure records a retryable failure: the account backs off 5 -> 30 min and
// nothing about its credentials changes. noop failures (the CLI ran and rotated
// nothing) are retried quietly a few times before they are reported.
func (r *TargetRefresher) noteFailure(acct *targetAccount, tool, name, detail string, noop bool) error {
	r.mu.Lock()
	acct.failures++
	if noop {
		acct.noops++
	} else {
		acct.noops = 0
	}
	acct.nextTry = r.now().Add(targetBackoff(acct.failures))
	acct.detail = detail
	report := !noop || acct.noops >= targetNoopLimit
	r.mu.Unlock()
	if report {
		r.emit(AccountRefreshFailed, tool+"/"+name, detail)
	} else {
		log.Printf("usages: token refresh %s/%s outcome=noop: %s", tool, name, detail)
	}
	return fmt.Errorf("token refresh %s/%s: %s", tool, name, detail)
}

// AccountStates returns the per-account state strings (reauth_required, ...)
// for doctor and the API; the key is "<tool>/<profile>". Only profiles that
// currently have a state appear.
func (r *TargetRefresher) AccountStates() map[string]string {
	out := map[string]string{}
	store := r.stateStore()
	for _, tool := range []string{"claude", "codex"} {
		profiles, err := r.rawProfiles(tool)
		if err != nil {
			continue
		}
		for _, p := range profiles {
			if p.CredPath == "" {
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
			if entry, ok := store.account(tool, hash); ok {
				out[tool+"/"+p.Name] = entry.State
				continue
			}
			r.mu.Lock()
			a := r.accounts[tool+"/"+hash]
			failing := a != nil && a.failures > 0 && (a.noops == 0 || a.noops >= targetNoopLimit)
			r.mu.Unlock()
			if failing {
				out[tool+"/"+p.Name] = AccountRefreshFailed
			}
		}
	}
	return out
}

// recover settles the journal left by a previous process: a run that was
// killed between spending the token and committing the result must neither be
// lost nor repeated. An entry whose commit cannot be completed now stays and is
// retried on a later pass (settleLeftover).
func (r *TargetRefresher) recover(ctx context.Context) {
	store := r.stateStore()
	entries := store.journal()
	sort.Slice(entries, func(i, j int) bool {
		return journalKey(entries[i].Tool, entries[i].Name) < journalKey(entries[j].Tool, entries[j].Name)
	})
	for _, e := range entries {
		_ = r.recoverEntry(ctx, e)
	}
}

// recoverEntry settles one journal entry. It returns nil when the entry is
// settled (and removed from the journal) and an error when it must stay because
// its commit could not be completed yet.
func (r *TargetRefresher) recoverEntry(ctx context.Context, e refreshJournalEntry) error {
	store := r.stateStore()
	key := e.Tool + "/" + e.Name
	hs, err := r.holders(e.Tool, e.SpentHash)
	if err != nil {
		return err // cannot judge now; the entry stays
	}
	if len(hs) == 0 {
		// No profile holds the spent token any more: a commit, a login or a
		// removal already replaced it. Nothing to repeat; keep any rescue file.
		log.Printf("usages: token refresh recovery %s: no profile holds the spent token, dropping the journal entry", key)
		r.clearJournal(store, e.Tool, e.Name)
		r.dropPending(e.Tool, e.SpentHash)
		return nil
	}
	rotated := r.recoverRotated(e, hs[0].data)
	if rotated == nil {
		if e.Phase == journalSpending {
			// The CLI was never started (a crash between the journal write and the
			// launch): the token cannot have been spent, a later pass may retry.
			log.Printf("usages: token refresh recovery %s: the refresh never reached the CLI, dropping the journal entry", key)
			r.clearJournal(store, e.Tool, e.Name)
			return nil
		}
		if e.Phase == journalRotated {
			r.emit("recovery", key, "the rotated credentials were not found; the stored refresh token may be spent")
		}
		const detail = "the server stopped while a refresh was running; the outcome is unknown"
		r.settle(store, refreshAccountEntry{Tool: e.Tool, Name: hs[0].p.Name, State: AccountNeedsReloginSoon, Detail: detail, SpentHash: e.SpentHash, Since: r.now().UTC()}, e.Tool, e.Name)
		r.emit(AccountNeedsReloginSoon, e.Tool+"/"+hs[0].p.Name, detail)
		return nil
	}
	runStarted := e.StartedAt
	if runStarted.IsZero() {
		runStarted = r.now()
	}
	committed, err := r.commitToHolders(e.Tool, e.SpentHash, rotated, runStarted)
	if err != nil {
		r.holdPending(e.Tool, e.SpentHash, rotated)
		r.emit("recovery", key, cleanDetail("committing the rotated credentials failed, will retry: "+err.Error()))
		return err
	}
	if e.Rescue != "" {
		_ = os.Remove(e.Rescue)
	}
	r.clearJournal(store, e.Tool, e.Name)
	r.dropPending(e.Tool, e.SpentHash)
	r.emit("recovery", key, "committed credentials rotated before the server stopped")
	for _, name := range committed {
		r.rearm(e.Tool + "/" + name)
		r.noteRotation(e.Tool, name, rotated)
		r.fanOut(e.Tool, name, hs[0].p.CredPath)
	}
	return nil
}

// recoverRotated returns the new credentials of an interrupted run, from the
// persist-first rescue file, the in-memory copy, or, for a run killed before the
// rescue was written, its temp dir. A rescue file was written only after the
// result was judged a newer login, so it only has to parse; a temp copy must be
// a newer login than spent (the state still in the store) by its access token or
// expiry, never by its refresh token alone: a provider may rotate the access
// token without rotating the refresh token.
func (r *TargetRefresher) recoverRotated(e refreshJournalEntry, spent []byte) []byte {
	if e.Rescue != "" {
		if data, err := os.ReadFile(e.Rescue); err == nil && validRotated(e.Tool, data) {
			return data
		}
	}
	if data := r.pendingFor(e.Tool, e.SpentHash); data != nil && validRotated(e.Tool, data) {
		return data
	}
	if e.TempDir != "" {
		name := ".credentials.json"
		if e.Tool == "codex" {
			name = "auth.json"
		}
		if data, err := os.ReadFile(filepath.Join(e.TempDir, name)); err == nil && validRotated(e.Tool, data) && credentialAdvanced(e.Tool, spent, data) {
			return data
		}
	}
	return nil
}

// validRotated reports whether data parses as a complete credential with a
// refresh token.
func validRotated(tool string, data []byte) bool {
	if _, err := refreshToken(data, tool); err != nil {
		return false
	}
	if tool == "claude" {
		return tolerantExpiry("claude", data) != nil
	}
	_, _, err := parseCodexAuthDataFull(data)
	return err == nil
}
