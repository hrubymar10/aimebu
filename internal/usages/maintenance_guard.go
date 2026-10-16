package usages

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// errMaintenanceActiveProfile is returned when server-side maintenance (token
// refresh, warmup, codex maintenance) would touch the ACTIVE profile. The
// active account's token is in use by the harnesses running against the live
// credential file, and they rotate it themselves. A second refresh from the
// server uses the same refresh token again, which makes the provider revoke the
// whole token family and logs every session out. Maintenance therefore only
// ever touches inactive snapshots.
var errMaintenanceActiveProfile = errors.New("maintenance: profile is active; the running harness owns its token refresh")

// ErrMaintenanceBusy is returned by HoldMaintenance when a maintenance run
// for the account is still in flight after the wait budget.
var ErrMaintenanceBusy = errors.New("a background token refresh for this account is still running")

// maintenanceDirName holds one advisory lock file per account under the
// switcher root. The lock is a flock, so it excludes jobs inside this process
// and in any other process (the `aimebu switcher` CLI, a second server).
const maintenanceDirName = ".maintenance"

// maintenanceIdentity names the ACCOUNT a profile holds, not the profile: it is
// the hash of the stored refresh token, so a rename (or a switch, which moves
// the file) mid-run cannot hand a second job a free slot. A profile whose
// credentials cannot be read falls back to its name.
func maintenanceIdentity(tool, name, credPath string) string {
	if data, err := os.ReadFile(credPath); err == nil {
		if token, err := refreshToken(data, tool); err == nil && token != "" {
			sum := sha256.Sum256([]byte(tool + "\x00" + token))
			return hex.EncodeToString(sum[:])
		}
	}
	sum := sha256.Sum256([]byte(tool + "\x00name\x00" + name))
	return hex.EncodeToString(sum[:])
}

func maintenanceLockPath(root, identity string) string {
	return filepath.Join(root, maintenanceDirName, identity+".lock")
}

// tryMaintenanceLock takes the account's flock without blocking. A nil release
// with a nil error means another job holds it. The returned release removes the
// lock file while still holding the lock and then unlocks, so the directory
// does not grow; a contender that opened the file just before the removal
// notices (the path no longer names the inode it locked) and retries on a fresh
// file. It is safe to call more than once.
func tryMaintenanceLock(root, identity string) (release func(), err error) {
	dir := filepath.Join(root, maintenanceDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := maintenanceLockPath(root, identity)
	for attempt := 0; attempt < 8; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, err
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, nil
			}
			return nil, err
		}
		held, herr := f.Stat()
		current, cerr := os.Stat(path)
		if herr != nil || cerr != nil || !os.SameFile(held, current) {
			// The previous holder removed the file between our open and lock.
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
			continue
		}
		var once sync.Once
		return func() {
			once.Do(func() {
				_ = os.Remove(path)
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			})
		}, nil
	}
	return nil, errors.New("maintenance lock kept being replaced")
}

// AcquireMaintenance takes the cross-process account lock a maintenance run
// holds and returns its release function; ok is false when another run holds
// it. Maintenance coordinators use the guard instead; this is for callers that
// must exclude a run without waiting.
func AcquireMaintenance(root, tool, name, credPath string) (release func(), ok bool, err error) {
	release, err = tryMaintenanceLock(root, maintenanceIdentity(tool, name, credPath))
	if err != nil || release == nil {
		return nil, false, err
	}
	return release, true, nil
}

// HoldMaintenance waits until no maintenance run holds the account behind the
// stored credential file at credPath, then HOLDS that account's lock until the
// returned release is called, or returns ErrMaintenanceBusy after timeout. A
// switch holds it for its whole duration: waiting and then releasing would let
// a run start rotating the target's token inside the gap, leaving the live file
// holding a refresh token the run has spent. The order is always account lock
// first, switcher lock second, the same as a run (which holds its account lock
// and takes the switcher lock only for its copy-back), so the two cannot
// deadlock. The caller must take the switcher lock only after this returns.
//
// The lock is keyed by the stored refresh token, and a run that finishes while
// we wait replaces that token (its copy-back stores the rotated one). The key
// is therefore re-derived after every acquisition: a lock taken under a token
// that has since changed guards nothing, so it is released and the wait
// continues under the new key.
//
// It fails closed: if the lock cannot be taken at all (for example the
// .maintenance directory is unusable) the error is returned and the switch must
// not proceed. An empty root means no maintenance locking is configured.
func HoldMaintenance(root, tool, name, credPath string, timeout time.Duration) (release func(), err error) {
	if root == "" {
		return func() {}, nil
	}
	deadline := time.Now().Add(timeout)
	for {
		identity := maintenanceIdentity(tool, name, credPath)
		release, err := tryMaintenanceLock(root, identity)
		if err != nil {
			log.Printf("usages: ERROR cannot take the maintenance lock for %s/%s, refusing the switch: %v", tool, name, err)
			return nil, fmt.Errorf("account lock unavailable: %w", err)
		}
		if release != nil {
			if maintenanceIdentity(tool, name, credPath) == identity {
				return release, nil
			}
			release() // the token rotated while we waited; lock the new identity
			continue
		}
		if !time.Now().Before(deadline) {
			return nil, ErrMaintenanceBusy
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// maintenanceGuard serialises maintenance jobs per account: warmup,
// auto-refresh and codex maintenance each rotate the same refresh token, so two
// of them in flight for one account would reuse it. One guard is shared by all
// three coordinators; a nil guard never blocks (tests that build a lone
// coordinator). With a root set the exclusion is a cross-process flock, and
// without one (tests) an in-process set.
type maintenanceGuard struct {
	root string

	mu   sync.Mutex
	busy map[string]bool // identity -> held (in-process; also the no-root fallback)
}

func newMaintenanceGuard() *maintenanceGuard {
	return &maintenanceGuard{busy: map[string]bool{}}
}

// acquire claims the account behind profile. It returns the release for exactly
// this claim (callers keep it with the job, so a rotation that changes the
// profile's token mid-run cannot mix up claims), or ok=false when another job
// holds the account.
func (g *maintenanceGuard) acquire(tool string, profile ProfileInfo) (release func(), ok bool) {
	if g == nil {
		return func() {}, true
	}
	identity := maintenanceIdentity(tool, profile.Name, profile.CredPath)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.busy[identity] {
		return nil, false
	}
	// With a root the flock is the exclusion a switch also sees, so failing to
	// take it must not degrade to the in-process map (which a switch cannot
	// see): the job simply does not run. Without a root (tests) only the
	// in-process map exists.
	var unlock func()
	if g.root != "" {
		r, err := tryMaintenanceLock(g.root, identity)
		if err != nil {
			log.Printf("usages: ERROR maintenance lock for profile=%q unavailable, skipping the job: %v", profile.Name, err)
			return nil, false
		}
		if r == nil {
			return nil, false
		}
		unlock = r
	}
	g.busy[identity] = true
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			delete(g.busy, identity)
			g.mu.Unlock()
			if unlock != nil {
				unlock()
			}
		})
	}, true
}

// maintenanceTargetBlocked reports whether profile must not be maintained: it
// is the active profile, or its stored credential file is a link to the live
// one (the switcher links the active profile's stored file to live, so a write
// through it would land in the credentials the harnesses are using).
func maintenanceTargetBlocked(profile ProfileInfo) error {
	if profile.Active {
		return errMaintenanceActiveProfile
	}
	if credentialPathIsLink(profile.CredPath) {
		return errMaintenanceActiveProfile
	}
	return nil
}

// maintenanceSwitchAwayGrace is how long after a profile stops being active the
// server still leaves its token alone. Harness sessions started while it was
// active (a CLI left open, a long-running agent) keep refreshing the token they
// hold until they exit, so the stored snapshot is not yet "ours" to rotate.
const maintenanceSwitchAwayGrace = 8 * time.Hour

var errMaintenanceGrace = errors.New("maintenance: profile was switched away from recently; sessions started on it may still refresh its token")

// maintenanceGraceBlocked reports whether profile was active within the grace
// period before now.
func maintenanceGraceBlocked(profile ProfileInfo, now time.Time) error {
	if profile.SwitchedAwayAt != nil && now.Sub(*profile.SwitchedAwayAt) < maintenanceSwitchAwayGrace {
		return errMaintenanceGrace
	}
	return nil
}

// maintenanceGraceBlockedBy is maintenanceGraceBlocked for a run that is about
// to start: it reads the clock only when a switch-away time is recorded.
func maintenanceGraceBlockedBy(profile ProfileInfo, clock Clock) error {
	if profile.SwitchedAwayAt == nil {
		return nil
	}
	return maintenanceGraceBlocked(profile, clock.Now())
}

func credentialPathIsLink(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}
