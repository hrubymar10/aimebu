package credtarget

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hrubymar10/aimebu/internal/usages"
)

// Alert kinds the reconciler raises. Callers dedupe them.
const (
	// AlertLoginImported: a login in a target was stored in a profile that the
	// user did not necessarily expect (another known account, or a new one).
	AlertLoginImported = "login_imported"
	// AlertLoginImportFailed: a login sits in a target and could not be stored.
	// The target still holds a refresh token until a later pass succeeds.
	AlertLoginImportFailed = "login_import_failed"
	// AlertLoginStripFailed: the login was stored but the target could not be
	// rewritten token-only yet.
	AlertLoginStripFailed = "login_strip_failed"

	// ReasonRotated: the login's tokens changed while it was being imported, so
	// the newer pair replaces the one just stored in the same profile.
	ReasonRotated = "rotated"

	// ReasonAlreadyImported: a profile already holds exactly these bytes, so an
	// earlier pass imported the login and only the strip is left.
	ReasonAlreadyImported = "already-imported"
	// ReasonSameToken: a profile already holds this very refresh token, so the
	// login IS that profile's grant (a failed un-migration leaves the live file
	// with extra keys, such as MCP logins, beside the stored credential). It is
	// stored into that profile again, never filed as a new one.
	ReasonSameToken = "same-token"
)

// ErrNotMigrated is returned when the profile store still links profiles to the
// live files, so the credential model has not been migrated yet.
var ErrNotMigrated = errors.New("credtarget: the profile store is not migrated to regular credential files")

const (
	defaultInterval = 60 * time.Second
	defaultLockWait = 5 * time.Second
	// defaultImportSettle is how long a target must sit unchanged before a login
	// in it is filed (see settling).
	defaultImportSettle = 15 * time.Second
	// accountFileSkew tolerates the coarse timestamps of some file systems when
	// comparing the account file with the credentials.
	accountFileSkew = 2 * time.Second
)

// Reconciler is the 60 s self-healing pass over all targets:
//  1. a target holding a refresh token (a /login landed in it) is rescued,
//     imported into a profile (ImportTarget order, import.go) and stripped;
//  2. a target whose expiresAt lags its profile's is rewritten;
//  3. modes are fixed to 0600;
//  4. it runs once at server start.
//
// Owner: otto. Tests: T4.
type Reconciler struct {
	Registry *Registry
	Writer   TargetWriter
	FanOut   *FanOut
	// Store is the profile store (switcher.NewCredStore).
	Store    ProfileStore
	Interval time.Duration // default 60 s
	// Home resolves the live Claude account file (usages.ClaudeAccountFilePath);
	// tests point it at a temp dir so the real home is never read.
	Home string
	// Alert is called (deduped by the caller) for events a human should see:
	// an import into a new login-<ts> profile, a stripped target, ...
	Alert func(kind, detail string)

	// Ready, when set, gates every pass: false skips it. The reconciler must not
	// run before the credential migration: until then every live file still
	// holds a refresh token and the profile store is still linked to it.
	Ready func() bool
	// ReadyTool narrows Ready per tool: after `migrate-copies --tool claude` the
	// codex targets still hold full credentials on the link model and must be left
	// alone. Nil means every tool is ready once Ready is.
	ReadyTool func(tool string) bool

	// ImportSettle defers a login in a target that was written less than this
	// long ago (0 means 15 s, negative means no wait).
	ImportSettle time.Duration

	// Now and AccountLockWait are test seams.
	Now             func() time.Time
	AccountLockWait time.Duration

	// Unexported seams, so the pass is testable without the other pieces.
	lister targetLister
	fanout fanOuter

	mu          sync.Mutex // one pass at a time
	noReadyOnce sync.Once
	// notMigrated remembers which tools already raised the "not migrated"
	// alert, so an unmigrated install alerts once instead of every pass. It is
	// cleared for a tool as soon as a pass no longer hits the condition.
	notMigrated map[string]bool
}

type targetLister interface{ Targets() []Target }
type fanOuter interface {
	FanOut(tool, profile string, full []byte) error
}

// Run blocks until ctx is done, calling Pass every Interval after an immediate
// first pass.
func (r *Reconciler) Run(ctx context.Context) {
	interval := r.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if err := r.Pass(ctx); err != nil {
			log.Printf("credtarget: reconcile pass: %v", err)
		}
		timer.Reset(interval)
	}
}

// Pass performs one reconcile pass. Errors for single targets never stop the
// others; they are joined and returned.
func (r *Reconciler) Pass(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Store == nil {
		return errors.New("credtarget: reconciler has no profile store")
	}
	if r.Ready == nil {
		r.noReadyOnce.Do(func() {
			log.Printf("credtarget: reconciler has no Ready gate; it still refuses to import while a profile is linked to a live file")
		})
	} else if !r.Ready() {
		return nil
	}
	var errs []error
	for _, t := range r.targets() {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if r.ReadyTool != nil && !r.ReadyTool(t.Tool) {
			continue
		}
		err := r.reconcileTarget(t)
		if !errors.Is(err, ErrNotMigrated) {
			delete(r.notMigrated, t.Tool)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (r *Reconciler) targets() []Target {
	if r.lister != nil {
		return r.lister.Targets()
	}
	if r.Registry == nil {
		return nil
	}
	return r.Registry.Targets()
}

func (r *Reconciler) write(t Target, full []byte) error {
	if r.Writer == nil {
		return errors.New("credtarget: reconciler has no target writer")
	}
	return r.Writer.WriteTarget(t, full)
}

// loginWriter is the per-call way to overwrite a target that still holds a
// refresh token. WriteTarget refuses such a target unconditionally, so a fan-out
// can never destroy a login nobody has rescued; only the reconciler, after it
// has rescued and imported the login, may write one.
type loginWriter interface {
	WriteImportedLogin(t Target, full []byte) error
}

// writeImportedLogin strips a target whose login has been rescued and imported.
func (r *Reconciler) writeImportedLogin(t Target, full []byte) error {
	if r.Writer == nil {
		return errors.New("credtarget: reconciler has no target writer")
	}
	lw, ok := r.Writer.(loginWriter)
	if !ok {
		return errors.New("credtarget: the target writer cannot replace a target that holds a login")
	}
	return lw.WriteImportedLogin(t, full)
}

func (r *Reconciler) fanOut(tool, profile string, full []byte) error {
	if r.fanout != nil {
		return r.fanout.FanOut(tool, profile, full)
	}
	if r.FanOut == nil {
		return nil
	}
	return r.FanOut.FanOut(tool, profile, full)
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reconciler) alert(kind, detail string) {
	log.Printf("credtarget: %s: %s", kind, detail)
	if r.Alert != nil {
		r.Alert(kind, detail)
	}
}

func (r *Reconciler) reconcileTarget(t Target) error {
	data, err := os.ReadFile(t.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// A removed file is a logout, not damage: never resurrect it.
			return nil
		}
		return fmt.Errorf("read target: %w", err)
	}
	if HasRefreshToken(t.Tool, data) {
		return r.importLogin(t, data)
	}
	return r.heal(t, data)
}

// heal fixes a token-only target: its mode, and a token that differs from its
// profile's (a missed fan-out, or a target left over from before a switch).
//
// A follow target is wrong whenever its access token is not its profile's: every
// fan-out writes exactly the profile's token, and no CLI can mint another one
// without a refresh token. So identity decides, not expiry order, which cannot
// tell two Claude accounts apart (the file holds no account id).
//
// The decision is made twice. A cheap look without any lock says whether a
// rewrite is probably due; the rewrite itself then re-reads the active profile,
// the stored credential and the target under the switcher lock, the lock a
// Switch holds while it moves the pointer and rewrites the live file. Without
// that, a Switch finishing between the look and the write would have its live
// file overwritten with the previous account's token, and nothing would notice
// again until that token expired.
func (r *Reconciler) heal(t Target, data []byte) error {
	if info, err := os.Stat(t.Path); err == nil && info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(t.Path, 0o600); err != nil {
			return fmt.Errorf("fix mode: %w", err)
		}
		log.Printf("credtarget: %s: mode %v fixed to 0600", t.ID, info.Mode().Perm())
	}
	if _, due, err := r.healPlan(t, data); err != nil || !due {
		return err
	}
	return r.Store.WithLock(func() error {
		current, err := os.ReadFile(t.Path)
		if err != nil {
			return nil // removed meanwhile: a logout
		}
		if HasRefreshToken(t.Tool, current) {
			return nil // a login landed meanwhile; the next pass imports it
		}
		stored, due, err := r.healPlan(t, current)
		if err != nil || !due {
			return err
		}
		if err := r.write(t, stored); err != nil {
			if errors.Is(err, ErrLockBusy) {
				return nil // a worker is writing the file; the next pass retries
			}
			return fmt.Errorf("rewrite lagging target: %w", err)
		}
		log.Printf("credtarget: %s: rewritten from its profile", t.ID)
		return nil
	})
}

// healPlan reports whether target data differs from the profile the target is
// bound to, and returns that profile's stored credential.
func (r *Reconciler) healPlan(t Target, data []byte) (stored []byte, due bool, err error) {
	name := t.Profile
	if name == "" {
		active, err := r.Store.Active(t.Tool)
		if err != nil {
			return nil, false, fmt.Errorf("active profile: %w", err)
		}
		name = active
	}
	if name == "" {
		return nil, false, nil
	}
	stored, err = r.Store.StoredCredential(t.Tool, name)
	if err != nil {
		return nil, false, nil // nothing to heal from
	}
	full, err := ParseFull(t.Tool, stored)
	if err != nil {
		return nil, false, nil // an empty or unusable profile cannot refresh a target
	}
	current, err := ParseTarget(t.Tool, data)
	return stored, err != nil || current.AccessToken != full.AccessToken, nil
}

// importLogin moves a full credential found in a target into the profile store
// and strips the target: rescue -> choose -> import -> token-only rewrite ->
// fan-out. Every step is idempotent, so a failure part-way is finished by the
// next pass.
//
// A CLI can rotate the login's refresh token between the read and the strip
// (only a refresh in the token's last minutes, which a fresh login is not in,
// but a session that loaded it earlier may be). Storing the old pair would
// then keep a spent token, so the target is read again after the import and a
// login whose tokens changed is imported again before anything is stripped.
func (r *Reconciler) importLogin(t Target, data []byte) error {
	// Before anything is saved or alerted: on an unmigrated install every live
	// file holds a refresh token, and a rescue copy of it on every pass would
	// be noise at best.
	if err := r.ensureMigrated(t); err != nil {
		return err
	}
	if r.settling(t) {
		return nil
	}
	var decision ImportDecision
	var forced *ImportDecision
	for attempt := 0; ; attempt++ {
		var err error
		decision, err = r.importOnce(t, data, forced)
		if err != nil {
			if errors.Is(err, usages.ErrMaintenanceBusy) {
				return nil // a refresh of that account is running; retry next pass
			}
			return err
		}
		again, readErr := os.ReadFile(t.Path)
		if errors.Is(readErr, os.ErrNotExist) {
			// The user logged out while the login was being filed. The login is
			// safe in its profile and the rescue; a removed file is not recreated.
			log.Printf("credtarget: %s: removed while its login was imported; leaving it removed", t.ID)
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("%s: re-read target: %w", t.ID, readErr)
		}
		if !HasRefreshToken(t.Tool, again) {
			return nil // someone already rewrote it token-only; nothing left to strip
		}
		moved := !sameLogin(t.Tool, data, again)
		if !moved {
			err := r.strip(t, data)
			if err == nil {
				break
			}
			if errors.Is(err, ErrLockBusy) {
				log.Printf("credtarget: %s: write lock busy, stripping on the next pass", t.ID)
				return nil
			}
			if !errors.Is(err, errLoginMoved) {
				r.alert(AlertLoginStripFailed, fmt.Sprintf("%s: stored in profile %s but could not rewrite the target token-only: %v", t.ID, decision.Profile, err))
				return fmt.Errorf("strip target: %w", err)
			}
			// The pair changed between the check and the locked strip.
			if again, readErr = os.ReadFile(t.Path); readErr != nil || !HasRefreshToken(t.Tool, again) {
				return nil
			}
		}
		if attempt >= 2 {
			return fmt.Errorf("%s: the login keeps changing while it is imported", t.ID)
		}
		data = again
		// The rotated pair belongs in the profile the first import chose, even
		// when the account cannot be identified (which would otherwise mint a
		// second login profile and leave the spent token behind).
		forced = &ImportDecision{Profile: decision.Profile, Reason: ReasonRotated}
	}
	if err := r.fanOut(t.Tool, decision.Profile, data); err != nil {
		log.Printf("credtarget: %s: fan-out after login import: %v", t.ID, err)
	}
	return nil
}

var errLoginMoved = errors.New("credtarget: the login in the target changed before it could be stripped")

// strip rewrites a target token-only once its login is stored. Like every other
// live-target write it runs under the switcher lock, and it looks at the target
// once more inside it: a login that changed or vanished since the last read is
// left for the next pass instead of being overwritten with an older pair.
func (r *Reconciler) strip(t Target, data []byte) error {
	return r.Store.WithLock(func() error {
		current, err := os.ReadFile(t.Path)
		if errors.Is(err, os.ErrNotExist) {
			return nil // a logout: stays removed
		}
		if err != nil {
			return fmt.Errorf("re-read target: %w", err)
		}
		if !HasRefreshToken(t.Tool, current) {
			return nil // already token-only
		}
		if !sameLogin(t.Tool, data, current) {
			return errLoginMoved
		}
		return r.writeImportedLogin(t, data)
	})
}

// sameLogin reports whether two copies of a credential carry the same token
// pair. Whole-file equality would also differ when a worker only added an MCP
// login to the same file, which is no reason to import again.
func sameLogin(tool string, a, b []byte) bool {
	ca, okA := loginTokens(tool, a)
	cb, okB := loginTokens(tool, b)
	if !okA || !okB {
		return bytes.Equal(a, b)
	}
	return ca.AccessToken == cb.AccessToken && ca.RefreshToken == cb.RefreshToken
}

// sameRefreshToken reports whether two credential documents carry the same
// non-empty refresh token: the same grant, whatever else the files hold.
func sameRefreshToken(tool string, a, b []byte) bool {
	ca, errA := ParseFull(tool, a)
	cb, errB := ParseTarget(tool, b)
	if errA != nil || (errB != nil && !errors.Is(errB, ErrRefreshTokenInTarget)) {
		return false
	}
	return ca.RefreshToken != "" && ca.RefreshToken == cb.RefreshToken
}

func loginTokens(tool string, data []byte) (Credential, bool) {
	cred, err := ParseTarget(tool, data)
	if err != nil && !errors.Is(err, ErrRefreshTokenInTarget) {
		return Credential{}, false
	}
	return cred, true
}

// ensureMigrated refuses to file a login while any profile is still a link to a
// live file, once per tool until the condition clears. Importing the live file
// and stripping it would remove the only copy of the refresh token that profile
// has.
func (r *Reconciler) ensureMigrated(t Target) error {
	names, err := r.Store.ProfileNames(t.Tool)
	if err != nil {
		return fmt.Errorf("list profiles: %w", err)
	}
	for _, name := range names {
		if _, err := r.Store.StoredCredential(t.Tool, name); errors.Is(err, ErrLinkedProfile) {
			err = fmt.Errorf("profile %s is still linked to a live credential file; run the credential migration first: %w", name, ErrNotMigrated)
			if r.notMigrated == nil {
				r.notMigrated = map[string]bool{}
			}
			if !r.notMigrated[t.Tool] {
				r.notMigrated[t.Tool] = true
				r.alert(AlertLoginImportFailed, fmt.Sprintf("%s: %v", t.ID, err))
			}
			return err
		}
	}
	return nil
}

// settling reports whether a target was written so recently that a login in it
// may still be mid-way: Claude writes the credentials first and the account
// file after it has fetched the profile, so reading the account too soon names
// the previous account. A deferred target is looked at again next pass.
func (r *Reconciler) settling(t Target) bool {
	settle := r.ImportSettle
	if settle < 0 {
		return false
	}
	if settle == 0 {
		settle = defaultImportSettle
	}
	info, err := os.Stat(t.Path)
	if err != nil {
		return false
	}
	if age := time.Since(info.ModTime()); age < settle {
		log.Printf("credtarget: %s: written %s ago, waiting for the login to settle", t.ID, age.Round(time.Second))
		return true
	}
	return false
}

// importOnce rescues the login and stores it in the profile store. It holds the
// account lock of a replaced profile for exactly that long. A forced decision
// skips the choice (see importLogin).
func (r *Reconciler) importOnce(t Target, data []byte, forced *ImportDecision) (ImportDecision, error) {
	rescue, err := r.Store.SaveLoginRescue(t.Tool, data, r.now())
	if err != nil {
		// Never strip or import a login that is not safely persisted first.
		r.alert(AlertLoginImportFailed, fmt.Sprintf("%s: could not save a rescue copy of the login: %v", t.ID, err))
		return ImportDecision{}, fmt.Errorf("save login rescue: %w", err)
	}
	decision, release, err := r.decide(t, data, forced)
	if err != nil {
		if !errors.Is(err, usages.ErrMaintenanceBusy) {
			r.alert(AlertLoginImportFailed, fmt.Sprintf("%s: %v (the login is kept in %s)", t.ID, err, filepath.Base(rescue)))
		}
		return ImportDecision{}, err
	}
	defer release()
	previous, _ := r.Store.Active(t.Tool)
	// An already imported login is stored again: the import is idempotent, and
	// the first attempt may have written the credential but failed to move the
	// active pointer, which would leave the live file following the wrong
	// profile and the heal pass undoing the login.
	if err := r.storeLogin(t, data, &decision); err != nil {
		r.alert(AlertLoginImportFailed, fmt.Sprintf("%s: %v (the login is kept in %s)", t.ID, err, filepath.Base(rescue)))
		return ImportDecision{}, err
	}
	if decision.Reason == ReasonEmailMatch || decision.Reason == ReasonNewLogin {
		detail := fmt.Sprintf("%s: a login was stored in profile %s (%s)", t.ID, decision.Profile, decision.Reason)
		if previous != decision.Profile {
			// The live file now holds this account, so the active pointer follows it.
			if previous == "" {
				detail += fmt.Sprintf("; %s is now the active profile", decision.Profile)
			} else {
				detail += fmt.Sprintf("; the active profile changed from %s to %s", previous, decision.Profile)
			}
		}
		if decision.Note != "" {
			detail += "; " + decision.Note
		}
		r.alert(AlertLoginImported, detail)
	}
	return decision, nil
}

// decide picks the import target and takes the account lock of a replaced
// profile. The release function is always safe to call.
func (r *Reconciler) decide(t Target, data []byte, forced *ImportDecision) (ImportDecision, func(), error) {
	noop := func() {}
	if forced != nil {
		return r.lockedDecision(t, *forced)
	}
	names, err := r.Store.ProfileNames(t.Tool)
	if err != nil {
		return ImportDecision{}, noop, fmt.Errorf("list profiles: %w", err)
	}
	var sameToken []string
	for _, name := range names {
		stored, err := r.Store.StoredCredential(t.Tool, name)
		if errors.Is(err, ErrLinkedProfile) {
			return ImportDecision{}, noop, fmt.Errorf("profile %s is still linked to a live credential file; run the credential migration first: %w", name, ErrNotMigrated)
		}
		if err == nil && bytes.Equal(stored, data) {
			return r.lockedDecision(t, ImportDecision{Profile: name, Reason: ReasonAlreadyImported})
		}
		if err == nil && sameRefreshToken(t.Tool, stored, data) {
			sameToken = append(sameToken, name)
		}
	}
	if len(sameToken) > 0 {
		// Duplicated grants (the same login stored under two names): the active
		// profile wins so the pointer never moves, else the first by name.
		pick := sameToken[0]
		if active, aerr := r.Store.Active(t.Tool); aerr == nil {
			for _, name := range sameToken {
				if name == active {
					pick = name
				}
			}
		}
		return r.lockedDecision(t, ImportDecision{Profile: pick, Reason: ReasonSameToken})
	}
	active, err := r.Store.Active(t.Tool)
	if err != nil {
		return ImportDecision{}, noop, fmt.Errorf("active profile: %w", err)
	}
	activeEmpty := false
	if active != "" {
		if activeEmpty, err = r.profileEmpty(t.Tool, active); err != nil {
			return ImportDecision{}, noop, err
		}
	}
	env := ImportEnv{
		Tool:           t.Tool,
		ActiveProfile:  active,
		ActiveEmpty:    activeEmpty,
		LiveAccount:    r.liveAccount(t, data),
		ProfileAccount: r.profileAccounts(t.Tool, names),
		Now:            r.now(),
	}
	decision, err := ChooseImport(env)
	if err != nil {
		return ImportDecision{}, noop, err
	}
	if decision.Profile == "" {
		return ImportDecision{}, noop, errNoProfileName
	}
	return r.lockedDecision(t, decision)
}

// lockedDecision takes the account lock of a profile that is replaced rather
// than created.
func (r *Reconciler) lockedDecision(t Target, decision ImportDecision) (ImportDecision, func(), error) {
	if decision.Create {
		return decision, func() {}, nil
	}
	wait := r.AccountLockWait
	if wait <= 0 {
		wait = defaultLockWait
	}
	release, err := r.Store.HoldAccount(t.Tool, decision.Profile, wait)
	if err != nil {
		return ImportDecision{}, func() {}, err
	}
	return decision, release, nil
}

func (r *Reconciler) storeLogin(t Target, data []byte, decision *ImportDecision) error {
	email := ""
	if t.Tool == ToolClaude {
		email = r.liveAccount(t, data)
	}
	in := LoginImport{Tool: t.Tool, Profile: decision.Profile, Create: decision.Create, Full: data, Email: email, SetActive: true}
	for attempt := 0; ; attempt++ {
		err := r.Store.ImportLogin(in)
		if err == nil {
			decision.Profile = in.Profile
			return nil
		}
		// Two logins in the same second share a login-<ts> name.
		if decision.Create && errors.Is(err, ErrProfileExists) && attempt < 9 {
			in.Profile = fmt.Sprintf("%s-%d", decision.Profile, attempt+2)
			continue
		}
		return fmt.Errorf("store login: %w", err)
	}
}

// profileEmpty reports whether a profile holds no usable credential: missing, or
// readable but not a full credential. A profile that merely cannot be read
// (permissions, I/O) is not empty: importing into it would overwrite what it
// holds without being able to back it up.
func (r *Reconciler) profileEmpty(tool, name string) (bool, error) {
	stored, err := r.Store.StoredCredential(tool, name)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return true, nil
	case errors.Is(err, ErrLinkedProfile):
		return false, fmt.Errorf("profile %s is still linked to a live credential file; run the credential migration first: %w", name, ErrNotMigrated)
	case err != nil:
		return false, fmt.Errorf("read profile %s: %w", name, err)
	}
	_, err = ParseFull(tool, stored)
	return err != nil, nil
}

// liveAccount identifies the account of a login found in a target.
//
// Claude writes the credentials first and the account file once it has fetched
// the profile, so a Claude account file that is older than the credentials may
// still name the PREVIOUS account. Trusting it would file the login under the
// wrong profile and replace that profile's stored credential with another
// account's. Such an account is reported as unknown, and an unknown account
// never matches anything.
func (r *Reconciler) liveAccount(t Target, data []byte) string {
	switch t.Tool {
	case ToolClaude:
		path := r.claudeAccountFile(t)
		email := ClaudeAccountEmail(path)
		if email == "" {
			return ""
		}
		account, errA := os.Stat(path)
		creds, errC := os.Stat(t.Path)
		if errA == nil && errC == nil && account.ModTime().Add(accountFileSkew).Before(creds.ModTime()) {
			log.Printf("credtarget: %s: the account file is older than the credential; not trusting its account", t.ID)
			return ""
		}
		return email
	case ToolCodex:
		return CodexAccount(data)
	}
	return ""
}

// claudeAccountFile is the .claude.json that names the account of a Claude
// target. A live target uses claude-code's own rule (usages.ClaudeAccountFilePath:
// the home root unless CLAUDE_CONFIG_DIR is set); an external target keeps it
// beside its credentials.
func (r *Reconciler) claudeAccountFile(t Target) string {
	if t.Role == RoleLive {
		return ClaudeAccountFileFor(t.Path, r.Home)
	}
	return filepath.Join(t.ConfigDir(), ".claude.json")
}

func (r *Reconciler) profileAccounts(tool string, names []string) map[string]string {
	accounts := make(map[string]string, len(names))
	for _, name := range names {
		switch tool {
		case ToolClaude:
			accounts[name] = r.Store.ProfileEmail(tool, name)
		case ToolCodex:
			if stored, err := r.Store.StoredCredential(tool, name); err == nil {
				accounts[name] = CodexAccount(stored)
			}
		}
	}
	return accounts
}
