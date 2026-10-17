package switcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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
				return MigrateResult{}, fmt.Errorf("%w (%s): a running codex caches its refresh token and would spend it again; end those sessions and re-run, or pass --allow-running-codex; checked: %s; nothing was changed", ErrCodexRunning, strings.Join(found, ", "), checked)
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
	return result, nil
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
	list, err := exec.CommandContext(ctx, "docker", "ps", "-q").Output()
	if err != nil {
		return nil, "docker is not reachable, so container sessions were NOT checked (stop them by hand)"
	}
	var found []string
	ids := strings.Fields(string(list))
	for _, id := range ids {
		top, terr := exec.CommandContext(ctx, "docker", "top", id, "-eo", "pid,comm").Output()
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
