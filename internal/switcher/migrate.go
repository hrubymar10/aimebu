package switcher

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hrubymar10/aimebu/internal/cmdprobe"
	"github.com/hrubymar10/aimebu/internal/credtarget"
	"github.com/hrubymar10/aimebu/internal/usages"
)

// ErrMigrateNotEnabled: migration needs the switcher enabled (it works on the
// switcher's profiles).
var ErrMigrateNotEnabled = errors.New("the switcher is disabled; enable it before migrating")

// MigrateOptions drives MigrateToTokenCopies.
type MigrateOptions struct {
	// Tools to migrate; empty means both.
	Tools []string
	// Now is the clock (tests); nil means time.Now.
	Now func() time.Time
	// AllowRunningCodex migrates although codex processes are running
	// (--allow-running-codex).
	AllowRunningCodex bool
	// RunningCodex lists running codex processes ("4242" for a host PID,
	// "container <id>: 77" for one in a docker container) and says what it
	// checked; nil scans the host process table and the running containers.
	RunningCodex func() (found []string, checked string, err error)
}

// ErrCodexRunning: a codex session started before the migration caches
// auth.json, refreshes proactively shortly before expiry with the refresh token
// it cached, and writes a full credential back. That would spend the same
// token the server refreshes, so the migration waits for those sessions to end.
var ErrCodexRunning = errors.New("codex is running")

// CodexRunningError retains the process details for the CLI and logs while
// allowing the settings UI to report only how many sessions are blocking it.
type CodexRunningError struct {
	Found   []string
	Checked string
}

func (e *CodexRunningError) Error() string {
	return fmt.Sprintf("%s (%s): a running codex caches its refresh token and would spend it again; end those sessions and re-run, or pass --allow-running-codex; checked: %s; nothing was changed", ErrCodexRunning, strings.Join(e.Found, ", "), e.Checked)
}

func (e *CodexRunningError) Unwrap() error { return ErrCodexRunning }

// MigrateResult reports what happened per tool.
type MigrateResult struct {
	AlreadyMigrated bool
	// CodexChecked says what the running-codex guard looked at ("" when it did
	// not run), so the operator never reads it as covering more than it did.
	CodexChecked string
	Tools        []MigrateToolResult
}

// MigrateToolResult is one tool's outcome.
type MigrateToolResult struct {
	Tool    string
	Profile string // the active profile whose credential moved into the store
	Status  string // "migrated" | "skipped: <reason>"
}

type migratePlan struct {
	tool      string
	profile   string
	livePath  string
	liveData  []byte
	liveFull  bool // the live file still holds a refresh token
	storePath string
	// external are the logins found in external targets, validated and resolved in the guard phase and filed
	// after the conversion.
	external []externalFiling
}

// MigrateToTokenCopies converts a link-based install to the token-copy model
// (design §8). Per tool with an active profile P:
//   - the live file is the freshest FULL credential of P: it is backed up
//     (persist-first), written into P's store file as a regular file (replacing
//     the symlink), and the live file is rewritten token-only through the
//     FanOutActive hook;
//   - a live file that is already token-only (an interrupted earlier run) only
//     needs a full regular store file.
//
// C2 guard: if a live full credential has less than credtarget.MigrateMinLife
// left, the whole migration is refused with credtarget.ErrMigrateTokenExpiring
// BEFORE anything is changed (a CLI mid-refresh in its last 5 minutes could
// spend the very token being moved). The marker file is written last, so a
// crash leaves a state that a re-run completes; a second run on a migrated
// install is a no-op.
func (m *Manager) MigrateToTokenCopies(opts MigrateOptions) (MigrateResult, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	tools := opts.Tools
	if len(tools) == 0 {
		tools = []string{ToolClaude, ToolCodex}
	}
	if m.copyHooks.FanOutActive == nil {
		return MigrateResult{}, errors.New("token-copy fan-out is not wired")
	}
	allMigrated := true
	for _, tool := range tools {
		if tool != ToolClaude && tool != ToolCodex {
			return MigrateResult{}, fmt.Errorf("unknown tool %q", tool)
		}
		allMigrated = allMigrated && m.TokenCopiesFor(tool)
	}
	if allMigrated {
		return MigrateResult{AlreadyMigrated: true}, nil
	}

	// Lock order is account first, switcher flock second, as in Switch and the
	// refresher. The active profiles are read unlocked to know which account
	// locks to take, then re-checked under the flock; a switch in between makes
	// the run refuse rather than move the wrong profile.
	held := map[string]func(){}
	defer func() {
		for _, release := range held {
			release()
		}
	}()
	if pre, perr := m.loadState(); perr == nil && pre.Enabled {
		for _, tool := range tools {
			profile := pre.Active[tool]
			if profile == "" || m.TokenCopiesFor(tool) || validateProfileName(profile) != nil {
				continue
			}
			release, holdErr := usages.HoldMaintenance(m.root, tool, profile, m.profileCredPath(tool, profile), MaintenanceWait)
			if holdErr != nil {
				return MigrateResult{}, fmt.Errorf("lock %s/%s: %w", tool, profile, holdErr)
			}
			held[tool+"/"+profile] = release
		}
	}

	lock, err := m.acquireLock()
	if err != nil {
		return MigrateResult{}, fmt.Errorf("acquire switcher lock: %w", err)
	}
	defer func() { _ = lock.unlock() }()

	state, err := m.loadState()
	if err != nil {
		return MigrateResult{}, err
	}
	if !state.Enabled {
		return MigrateResult{}, ErrMigrateNotEnabled
	}

	// Pre-flight: gather every plan and apply the C2 guard before touching anything.
	var plans []migratePlan
	var danglingLinks [][2]string
	var result MigrateResult
	for _, tool := range tools {
		if tool != ToolClaude && tool != ToolCodex {
			return MigrateResult{}, fmt.Errorf("unknown tool %q", tool)
		}
		if m.TokenCopiesFor(tool) {
			result.Tools = append(result.Tools, MigrateToolResult{Tool: tool, Status: "skipped: already migrated"})
			continue
		}
		profile := state.Active[tool]
		if profile == "" {
			result.Tools = append(result.Tools, MigrateToolResult{Tool: tool, Status: "skipped: no active profile"})
			continue
		}
		if err := validateProfileName(profile); err != nil {
			return MigrateResult{}, fmt.Errorf("corrupt state: active profile name invalid: %w", err)
		}
		if _, ok := held[tool+"/"+profile]; !ok {
			return MigrateResult{}, fmt.Errorf("the active %s profile changed to %q while migrating; nothing was changed, re-run", tool, profile)
		}
		livePath, err := m.liveCredPath(tool)
		if err != nil {
			return MigrateResult{}, err
		}
		plan := migratePlan{tool: tool, profile: profile, livePath: livePath, storePath: m.profileCredPath(tool, profile)}
		liveData, readErr := os.ReadFile(livePath)
		switch {
		case os.IsNotExist(readErr):
			// No live login: nothing to move. The store file (if any) must not stay a
			// dangling link.
			result.Tools = append(result.Tools, MigrateToolResult{Tool: tool, Profile: profile, Status: "skipped: no live credentials"})
			plan.liveData = nil
		case readErr != nil:
			return MigrateResult{}, fmt.Errorf("read live %s credentials: %w", tool, readErr)
		default:
			if err := validateLiveCred(tool, livePath); err != nil {
				return MigrateResult{}, fmt.Errorf("live %s credentials are not usable: %w", tool, err)
			}
			plan.liveData = liveData
			plan.liveFull = credtarget.HasRefreshToken(tool, liveData)
		}
		if tool == ToolCodex && plan.liveFull && !opts.AllowRunningCodex {
			find := opts.RunningCodex
			if find == nil {
				find = runningCodexScan
			}
			found, checked, perr := find()
			if perr != nil {
				return MigrateResult{}, fmt.Errorf("check for running codex processes: %w (pass --allow-running-codex to skip the check)", perr)
			}
			if len(found) > 0 {
				return MigrateResult{}, &CodexRunningError{Found: found, Checked: checked}
			}
			result.CodexChecked = checked
		}
		if plan.liveFull {
			cred, perr := credtarget.ParseFull(tool, liveData)
			if perr != nil {
				return MigrateResult{}, fmt.Errorf("live %s credentials are not a full login: %w", tool, perr)
			}
			if cred.ExpiresAt != nil && cred.ExpiresAt.Sub(now()) < credtarget.MigrateMinLife {
				return MigrateResult{}, fmt.Errorf("%w: %s expires at %s (%s left, need at least %s); nothing was changed",
					credtarget.ErrMigrateTokenExpiring, tool, cred.ExpiresAt.UTC().Format(time.RFC3339),
					cred.ExpiresAt.Sub(now()).Round(time.Second), credtarget.MigrateMinLife)
			}
		} else if plan.liveData != nil {
			// Token-only live file (interrupted run): the store must already hold a full credential.
			if err := validateStoredCred(tool, plan.storePath); err != nil {
				return MigrateResult{}, fmt.Errorf("live %s credentials are token-only and the stored profile %q is not a full credential (%v); log in again, then re-run", tool, profile, err)
			}
		}
		// External logins that cannot be filed must refuse now, while nothing has been changed: filing runs after
		// the conversion, where a failure would leave the install half done.
		if m.copyHooks.PendingLogins != nil && plan.liveData != nil {
			external, perr := m.planExternalLogins(tool, profile)
			if perr != nil {
				return MigrateResult{}, perr
			}
			plan.external = external
		}
		if plan.liveData != nil {
			plans = append(plans, plan)
		} else {
			// Removed only after every tool passed its guard, so a refusal
			// leaves the install exactly as it was.
			danglingLinks = append(danglingLinks, [2]string{tool, profile})
		}
	}
	for _, l := range danglingLinks {
		if err := m.removeStoredLink(l[0], l[1]); err != nil {
			return MigrateResult{}, err
		}
	}

	// Apply. The account locks taken above are still held while credentials move.
	for _, p := range plans {
		if err := m.migrateOne(p); err != nil {
			return result, err
		}
		result.Tools = append(result.Tools, MigrateToolResult{Tool: p.tool, Profile: p.profile, Status: "migrated"})
	}
	// External targets holding a login nobody imported: on an unmigrated tool nothing else files them (the
	// reconciler only works on migrated tools), and the refresher that starts after this migration could
	// revoke a login of the same token family before it is filed. File the ones validated above under the same
	// lock, after the active profile has become a regular file (a target pinned to it files into it) and before the
	// marker. A login that appeared since is left for the reconciler.
	var filed []filedLogin
	for _, p := range plans {
		replacedActive := false
		for _, ext := range p.external {
			profile, replaced, err := m.fileExternalLogin(p.tool, ext, p.profile, state)
			if err != nil {
				return result, fmt.Errorf("file the login in %s target %s: %w", p.tool, ext.pending.ID, err)
			}
			replacedActive = replacedActive || replaced
			filed = append(filed, filedLogin{p.tool, ext.pending.ID, profile, ext.pending.Data})
			for _, sibling := range ext.siblings {
				filed = append(filed, filedLogin{p.tool, sibling.ID, profile, sibling.Data})
			}
		}
		if replacedActive {
			// A filed login replaced the active profile's credential: the live file, rewritten token-only above from
			// the old one, must follow the account that is now stored.
			stored, err := os.ReadFile(p.storePath)
			if err != nil {
				return result, err
			}
			replace := m.copyHooks.ReplaceActive
			if replace == nil {
				replace = m.copyHooks.FanOutActive
			}
			if err := replace(p.tool, stored); err != nil {
				return result, fmt.Errorf("rewrite live %s credentials after filing the login pinned to %s: %w", p.tool, p.profile, err)
			}
		}
	}

	var migrated []string
	for _, p := range plans {
		migrated = append(migrated, p.tool)
	}
	if len(migrated) == 0 {
		return result, nil
	}
	if err := m.markTokenCopies(migrated...); err != nil {
		return result, fmt.Errorf("write marker: %w", err)
	}
	// The filed logins are safe in the store: rewrite their targets token-only. A failure here is not an error,
	// the reconciler strips them on its next pass now that the tool is migrated.
	if m.copyHooks.StripLogin != nil {
		for _, f := range filed {
			stored, err := os.ReadFile(m.profileCredPath(f.tool, f.profile))
			if err == nil {
				err = m.copyHooks.StripLogin(f.tool, f.id, f.original, stored)
			}
			if err != nil {
				log.Printf("switcher: %s target %s: its login is filed in profile %s but could not be rewritten token-only yet (the reconciler will): %v", f.tool, f.id, f.profile, err)
			}
		}
	}
	return result, nil
}

// filedLogin records a login that was filed, for the strip of its target afterwards.
type filedLogin struct {
	tool, id, profile string
	original          []byte // the login found in the target when it was filed
}

// externalFiling is one login family (every target holding the same refresh token) together with where it goes.
// profile is an existing profile: the one that already holds this refresh token (stored), or the one a member of the
// group is pinned to (pin); "" means a new login-<time> profile. pending is the group's main entry, siblings the other
// targets: the login is filed once and every target is then stripped from that one profile.
type externalFiling struct {
	pending  PendingLogin
	siblings []PendingLogin
	profile  string
	stored   bool
	pin      string
}

// planExternalLogins validates and resolves every login waiting in an external target of tool, without writing
// anything. Targets holding one refresh token are one group (filing them separately would store the family twice, and
// the copies would spend each other's token after a rotation). A group goes to the profile its pinned member is pinned
// to, else to a profile that already holds the token, else to a new login-<time> profile (resolveExternalGroup). One
// invariant then covers the conflicts: a destination profile is touched by at most one group. The run is refused, with
// nothing changed, for an unusable credential, an uncreatable rescue directory, members pinned to different profiles,
// a token held only outside the pin, a pin on a profile that is still linked to a live file, or two groups resolving to
// one profile. active is the tool's active profile (the only one that may still be a link before the conversion).
func (m *Manager) planExternalLogins(tool, active string) ([]externalFiling, error) {
	pending := m.copyHooks.PendingLogins(tool)
	if len(pending) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Join(m.backupsRoot(), tool), 0o700); err != nil {
		return nil, fmt.Errorf("prepare the login rescue directory: %w", err)
	}
	names, err := m.ProfileNames(tool)
	if err != nil {
		return nil, err
	}
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format+"; nothing was changed, fix or unregister one target", append([]any{ErrLoginPending}, args...)...)
	}
	// Group by refresh token, keeping the order of the targets.
	var groups []externalFiling
	var tokens []string
	byToken := map[string]int{}
	for _, p := range pending {
		cur, perr := credtarget.ParseFull(tool, p.Data)
		if perr != nil {
			return nil, fmt.Errorf("%w: the login in %s target %s is not a usable full credential (%v); nothing was changed, fix or unregister the target", ErrInvalidCredentials, tool, p.ID, perr)
		}
		if i, ok := byToken[cur.RefreshToken]; ok && cur.RefreshToken != "" {
			groups[i].siblings = append(groups[i].siblings, p)
			continue
		}
		byToken[cur.RefreshToken] = len(groups)
		groups = append(groups, externalFiling{pending: p})
		tokens = append(tokens, cur.RefreshToken)
	}
	for i := range groups {
		g := &groups[i]
		members := append([]PendingLogin{g.pending}, g.siblings...)
		// The pin of the group: its members' pins that name an existing profile; one distinct pin is the destination.
		for _, member := range members {
			if member.Profile == "" || !hasName(names, member.Profile) {
				continue
			}
			if g.pin != "" && g.pin != member.Profile {
				return nil, refuse("%s targets holding one login are pinned to different profiles (%s and %s)", tool, g.pin, member.Profile)
			}
			g.pin = member.Profile
		}
		profile, stored, rerr := m.resolveExternalGroup(tool, names, tokens[i], g.pin)
		if rerr != nil {
			return nil, refuse("%v (the login in %s target %s)", rerr, tool, g.pending.ID)
		}
		if profile != "" && !stored {
			if info, lerr := os.Lstat(m.profileCredPath(tool, profile)); lerr == nil && info.Mode()&os.ModeSymlink != 0 && profile != active {
				return nil, refuse("%s target %s is pinned to %s, which is still linked to a live file", tool, g.pending.ID, profile)
			}
		}
		g.profile, g.stored = profile, stored
	}
	// One invariant: every destination profile is touched by at most one group, whether it is replaced by a pinned
	// filing or is the profile another group's login is already stored in. A second group on the same profile would
	// replace or strip the first one's credential and silently change that target's account.
	touched := map[string]string{}
	for _, g := range groups {
		if g.profile == "" {
			continue
		}
		if other, taken := touched[g.profile]; taken {
			return nil, refuse("%s targets %s and %s hold different logins but both resolve to profile %s, which can hold only one", tool, other, g.pending.ID, g.profile)
		}
		touched[g.profile] = g.pending.ID
	}
	return groups, nil
}

// resolveExternalGroup picks the profile a login family goes to, from the store as it is now: every profile holding the
// refresh token counts. With a pin: the pin if it is one of the holders (stored, only the targets are left to strip),
// an error if the token is held only elsewhere (filing it into the pin would store the family twice), else the pin
// (it is replaced). Without one: the first holder, else "" for a new profile.
func (m *Manager) resolveExternalGroup(tool string, names []string, token, pin string) (profile string, stored bool, err error) {
	var holders []string
	if token != "" {
		for _, name := range names {
			data, rerr := os.ReadFile(m.profileCredPath(tool, name))
			if rerr != nil {
				continue
			}
			if old, oerr := credtarget.ParseFull(tool, data); oerr == nil && old.RefreshToken == token {
				holders = append(holders, name)
			}
		}
	}
	switch {
	case pin != "" && hasName(holders, pin):
		return pin, true, nil
	case pin != "" && len(holders) > 0:
		return "", false, fmt.Errorf("the login is already stored in profile %s, not in %s, which a target is pinned to", holders[0], pin)
	case pin != "":
		return pin, false, nil
	case len(holders) > 0:
		return holders[0], true, nil
	}
	return "", false, nil
}

// fileExternalLogin stores a resolved external login in the profile store and returns the profile and whether it
// replaced the credential of the active profile. The caller holds the switcher lock. A rescue copy is saved first, and a
// login is never filed over a profile of another account unless the target is pinned to it.
func (m *Manager) fileExternalLogin(tool string, f externalFiling, active string, state State) (string, bool, error) {
	if _, err := m.SaveLoginRescue(tool, f.pending.Data, time.Now()); err != nil {
		return "", false, fmt.Errorf("save a rescue copy: %w", err)
	}
	// The plan was made before any filing: resolve again against the store as it is now (a profile planned for reuse
	// must still hold this refresh token, and an earlier filing may already have stored the family).
	names, err := m.ProfileNames(tool)
	if err != nil {
		return "", false, err
	}
	cur, perr := credtarget.ParseFull(tool, f.pending.Data)
	if perr != nil {
		return "", false, fmt.Errorf("%w: %v", ErrInvalidCredentials, perr)
	}
	profile, stored, rerr := m.resolveExternalGroup(tool, names, cur.RefreshToken, f.pin)
	if rerr != nil {
		return "", false, fmt.Errorf("%w: %v", ErrLoginPending, rerr)
	}
	if stored {
		return profile, false, nil // only the target copies are left to strip
	}
	f.profile = profile
	in := credtarget.LoginImport{Tool: tool, Profile: f.profile, Full: f.pending.Data}
	if in.Profile == "" {
		base := "login-" + time.Now().UTC().Format("20060102T150405")
		in.Profile, in.Create = base, true
		for i := 2; hasName(names, in.Profile); i++ {
			in.Profile = fmt.Sprintf("%s-%d", base, i)
		}
	}
	if err := m.importLoginLocked(in, state); err != nil {
		return "", false, err
	}
	return in.Profile, !in.Create && in.Profile == active, nil
}

func hasName(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

func (m *Manager) migrateOne(p migratePlan) error {
	if p.liveFull {
		// Persist-first: the full live credential is the only copy of the freshest
		// refresh token. It goes into a backup before anything is replaced.
		if err := m.writeUniqueBackup(p.tool, p.profile, p.liveData); err != nil {
			return fmt.Errorf("backup live %s credentials: %w", p.tool, err)
		}
		// Replace the store file (a symlink to the live file, or a stale snapshot)
		// with a regular file holding the full credential.
		if err := m.removeStoredLink(p.tool, p.profile); err != nil {
			return err
		}
		if err := os.MkdirAll(m.profileDir(p.tool, p.profile), 0o700); err != nil {
			return err
		}
		if err := atomicWrite(p.storePath, p.liveData, 0o600); err != nil {
			return fmt.Errorf("write stored %s credentials: %w", p.tool, err)
		}
		if err := validateStoredCred(p.tool, p.storePath); err != nil {
			return fmt.Errorf("stored %s credentials failed validation after the copy: %w", p.tool, err)
		}
	} else if info, err := os.Lstat(p.storePath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		// Interrupted run: the stored file is still a link although it was validated
		// as a full credential through it; make it a regular file.
		data, rerr := os.ReadFile(p.storePath)
		if rerr != nil {
			return rerr
		}
		if err := os.Remove(p.storePath); err != nil {
			return err
		}
		if err := atomicWrite(p.storePath, data, 0o600); err != nil {
			return err
		}
	}
	if p.liveFull {
		// Rewrite the live file token-only from the credential that is now safely in
		// the store.
		stored, err := os.ReadFile(p.storePath)
		if err != nil {
			return err
		}
		replace := m.copyHooks.ReplaceActive
		if replace == nil {
			replace = m.copyHooks.FanOutActive
		}
		if err := replace(p.tool, stored); err != nil {
			return fmt.Errorf("rewrite live %s credentials: %w", p.tool, err)
		}
	}
	return nil
}

// runningCodexScan is the default scan; tests replace it so the host's real
// codex sessions never influence a migration test.
var runningCodexScan = scanRunningCodex

// scanRunningCodex looks for codex on the host and, best effort, inside running
// docker containers (the fleet's codex workers run in harness-docker containers
// that mount ~/.codex read-write and are invisible to a host `ps`). Docker being
// unavailable is not an error: the returned note says the containers were not
// checked.
func scanRunningCodex() ([]string, string, error) {
	out, err := exec.Command("ps", "-axo", "pid=,comm=").Output()
	if err != nil {
		return nil, "", err
	}
	var found []string
	for _, pid := range parseCodexPIDs(string(out), os.Getpid()) {
		found = append(found, "pid "+strconv.Itoa(pid))
	}
	containerFound, note := scanContainersForCodex()
	return append(found, containerFound...), "host processes; " + note, nil
}

func scanContainersForCodex() ([]string, string) {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, "docker is not installed, so container sessions were NOT checked (stop them by hand)"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	list, err := cmdprobe.Output(exec.CommandContext(ctx, "docker", "ps", "-q"))
	if err != nil {
		return nil, "docker is not reachable, so container sessions were NOT checked (stop them by hand)"
	}
	var found []string
	ids := strings.Fields(string(list))
	for _, id := range ids {
		top, terr := cmdprobe.Output(exec.CommandContext(ctx, "docker", "top", id, "-eo", "pid,comm"))
		if terr != nil {
			continue
		}
		for _, pid := range parseCodexPIDs(string(top), -1) {
			found = append(found, "container "+id+": "+strconv.Itoa(pid))
		}
	}
	return found, fmt.Sprintf("%d running docker container(s)", len(ids))
}

func parseCodexPIDs(psOut string, self int) []int {
	var pids []int
	for _, line := range strings.Split(psOut, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid == self {
			continue
		}
		if filepath.Base(strings.Join(fields[1:], " ")) == "codex" {
			pids = append(pids, pid)
		}
	}
	return pids
}
