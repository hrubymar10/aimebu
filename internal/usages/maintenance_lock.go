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
	"sync"
	"syscall"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/telemetry"
)

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
			telemetry.ReportCause(context.Background(), "credentials", "cannot take the maintenance lock, refusing the switch")
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

// MaintenanceProfile identifies a stored account whose refresh must be idle
// throughout a switch. Two profile names may contain the same refresh token;
// the corresponding account lock is acquired only once.
type MaintenanceProfile struct {
	Name, CredPath string
}

// HoldMaintenanceProfiles holds every distinct account lock in identity order.
// All locks precede the switcher flock, matching the refresh worker's order.
// Recheck identities after acquisition because a token may rotate while waiting.
func HoldMaintenanceProfiles(root, tool string, profiles []MaintenanceProfile, timeout time.Duration) (func(), error) {
	return holdMaintenanceProfiles(root, tool, profiles, timeout, tryMaintenanceLock)
}

func holdMaintenanceProfiles(root, tool string, profiles []MaintenanceProfile, timeout time.Duration, tryLock func(string, string) (func(), error)) (func(), error) {
	if root == "" {
		return func() {}, nil
	}
	deadline := time.Now().Add(timeout)
	for {
		ids := make([]string, 0, len(profiles))
		seen := map[string]bool{}
		for _, p := range profiles {
			id := maintenanceIdentity(tool, p.Name, p.CredPath)
			if !seen[id] {
				ids = append(ids, id)
				seen[id] = true
			}
		}
		sort.Strings(ids)
		releases := make([]func(), 0, len(ids))
		releaseAll := func() {
			for i := len(releases) - 1; i >= 0; i-- {
				releases[i]()
			}
		}
		busy := false
		for _, id := range ids {
			release, err := tryLock(root, id)
			if err != nil {
				releaseAll()
				return nil, fmt.Errorf("account lock unavailable: %w", err)
			}
			if release == nil {
				busy = true
				break
			}
			releases = append(releases, release)
		}
		if !busy {
			stable := true
			for _, p := range profiles {
				if !seen[maintenanceIdentity(tool, p.Name, p.CredPath)] {
					stable = false
					break
				}
			}
			if stable {
				return releaseAll, nil
			}
		}
		releaseAll()
		if !time.Now().Before(deadline) {
			return nil, ErrMaintenanceBusy
		}
		time.Sleep(100 * time.Millisecond)
	}
}
