package usages

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/telemetry"
)

// maxRotatedRescues is how many rotated-credential rescue files are kept per
// profile.
const maxRotatedRescues = 3

// Commit sentinels. A refresh runs its slow CLI step outside the switcher lock;
// by the time it wants to store the rotated tokens the profile may have been
// removed or re-logged in. Either aborts the commit after saving the rotated
// file for recovery.
var (
	errRefreshProfileGone    = errors.New("token refresh: profile no longer exists")
	errRefreshProfileChanged = errors.New("token refresh: stored credentials changed mid-refresh")
)

// commitRotatedCredentials stores rotated into the profile that started the run,
// following it across a rename (the refresh token identifies the account). The
// caller holds no lock during the slow CLI run; this whole decision runs under
// the switcher lock (withLock). The write is a compare-and-swap on the refresh
// token the run spent: a removed profile, a different account or a re-login
// leaves a mode-0600 rescue file instead of being clobbered. A profile's stored
// credential is always a regular file (the token-copy model has no links), so
// the write is a plain atomic replace.
func commitRotatedCredentials(tool string, withLock func(func() error) error, listProfiles func() ([]StoredProfile, error), name, startedPath string, started, rotated []byte, rescueRoot string, runStarted time.Time) error {
	if runStarted.IsZero() {
		runStarted = time.Now()
	}
	commit := func() error {
		beforeToken, err := refreshToken(started, tool)
		if err != nil {
			return saveAbortedRotation(tool, name, rotated, rescueRoot, fmt.Errorf("read starting refresh token: %w", err))
		}
		var removedAt time.Time
		var removed bool
		if rescueRoot != "" {
			removedAt, removed, err = removedTokenTime(rescueRoot, tool, beforeToken)
			if err != nil {
				return saveAbortedRotation(tool, name, rotated, rescueRoot, fmt.Errorf("check removed profiles: %w", err))
			}
		}
		currentPath := startedPath
		if listProfiles != nil {
			profiles, err := listProfiles()
			if err != nil {
				return saveAbortedRotation(tool, name, rotated, rescueRoot, fmt.Errorf("list profiles for commit: %w", err))
			}
			var matches []string
			var unresolved error
			if removed {
				matches, unresolved = matchingProfilePaths(profiles, tool, beforeToken)
				if removedAt.After(runStarted) && len(matches) == 0 {
					if unresolved != nil {
						return saveAbortedRotation(tool, name, rotated, rescueRoot, unresolved)
					}
					log.Printf("usages: %s token refresh profile=%q outcome=discarded reason=profile removed", tool, name)
					return errRefreshProfileGone
				}
			}
			var named *StoredProfile
			for i := range profiles {
				if profiles[i].Tool == tool && profiles[i].Name == name {
					named = &profiles[i]
					break
				}
			}
			if named != nil {
				currentPath = named.CredPath
			} else {
				// A switcher rename changes the profile name as well as its path.
				// Find the same account by the token that started the run.
				if !removed {
					matches, unresolved = matchingProfilePaths(profiles, tool, beforeToken)
				}
				if len(matches) == 0 {
					if unresolved == nil {
						unresolved = errRefreshProfileGone
					}
					return saveAbortedRotation(tool, name, rotated, rescueRoot, unresolved)
				}
				if len(matches) > 1 {
					return saveAbortedRotation(tool, name, rotated, rescueRoot, errors.New("multiple profiles match starting refresh token"))
				}
				currentPath = matches[0]
			}
		} else if removed {
			return saveAbortedRotation(tool, name, rotated, rescueRoot, errors.New("cannot resolve removed token without profile listing"))
		}
		current, err := os.ReadFile(currentPath)
		if err != nil {
			return saveAbortedRotation(tool, name, rotated, rescueRoot, errRefreshProfileGone)
		}
		currentToken, err := refreshToken(current, tool)
		if err != nil || currentToken != beforeToken {
			return saveAbortedRotation(tool, name, rotated, rescueRoot, errRefreshProfileChanged)
		}
		merged, err := mergeRotatedCredentials(tool, current, rotated)
		if err != nil {
			return saveAbortedRotation(tool, name, rotated, rescueRoot, fmt.Errorf("merge rotated credentials: %w", err))
		}
		// Preserve the CLI's exact bytes when no concurrent edit exists and there is
		// no unrelated top-level data to retain.
		if bytes.Equal(current, started) && (tool == "codex" || onlyClaudeOAuth(current)) {
			merged = rotated
		}
		if err := atomicWriteBytes(currentPath, merged, 0o600); err != nil {
			return saveAbortedRotation(tool, name, rotated, rescueRoot, err)
		}
		return nil
	}
	if withLock != nil {
		called := false
		err := withLock(func() error {
			called = true
			return commit()
		})
		if err != nil && !called {
			return saveAbortedRotation(tool, name, rotated, rescueRoot, fmt.Errorf("lock commit: %w", err))
		}
		return err
	}
	return commit()
}

func matchingProfilePaths(profiles []StoredProfile, tool, token string) ([]string, error) {
	var matches []string
	var unresolved error
	for _, p := range profiles {
		if p.Tool != tool || p.CredPath == "" {
			continue
		}
		data, err := os.ReadFile(p.CredPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			unresolved = fmt.Errorf("read candidate profile %q: %w", p.Name, err)
			continue
		}
		candidate, err := refreshToken(data, tool)
		if err != nil {
			unresolved = fmt.Errorf("read candidate profile %q refresh token: %w", p.Name, err)
			continue
		}
		if candidate == token {
			matches = append(matches, p.CredPath)
		}
	}
	return matches, unresolved
}

func onlyClaudeOAuth(data []byte) bool {
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil || len(root) != 1 {
		return false
	}
	return root["claudeAiOauth"] != nil || root["claude_ai_oauth"] != nil
}

// mergeRotatedCredentials takes only the token section of rotated into current,
// keeping every other top-level key of the stored document.
func mergeRotatedCredentials(tool string, current, rotated []byte) ([]byte, error) {
	var base, next map[string]json.RawMessage
	if err := json.Unmarshal(current, &base); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(rotated, &next); err != nil {
		return nil, err
	}
	if tool == "codex" {
		section, ok := next["tokens"]
		if !ok {
			return nil, errors.New("rotated codex tokens missing")
		}
		base["tokens"] = section
		if lastRefresh, ok := next["last_refresh"]; ok {
			base["last_refresh"] = lastRefresh
		}
	} else {
		section := "claudeAiOauth"
		if _, ok := next[section]; !ok {
			section = "claude_ai_oauth"
		}
		rotatedSection, ok := next[section]
		if !ok {
			return nil, errors.New("rotated claude OAuth section missing")
		}
		base[section] = rotatedSection
		if section == "claudeAiOauth" {
			delete(base, "claude_ai_oauth")
		} else {
			delete(base, "claudeAiOauth")
		}
	}
	return json.Marshal(base)
}

func saveAbortedRotation(tool, name string, rotated []byte, root string, cause error) error {
	if root == "" {
		return fmt.Errorf("%w; rotated credentials have no rescue root", cause)
	}
	dir := filepath.Join(root, "backups", tool)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("%w; save rotated credentials: %v", cause, err)
	}
	path := filepath.Join(dir, RotatedRescueName(name, time.Now().UTC()))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("%w; save rotated credentials: %v", cause, err)
	}
	_, writeErr := f.Write(rotated)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		return fmt.Errorf("%w; save rotated credentials: %v %v", cause, writeErr, closeErr)
	}
	pruneRotatedRescues(dir, name)
	log.Printf("usages: ERROR %s token refresh profile=%q outcome=aborted reason=%v rotated_saved=%s — the run spent the stored refresh token; the rescue file holds the new one", tool, name, cause, path)
	telemetry.ReportCause(context.Background(), "credentials", "token refresh aborted after the run spent the stored refresh token (the rescue file holds the new one)")
	return fmt.Errorf("%w; rotated_saved=%s", cause, path)
}

func pruneRotatedRescues(dir, name string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("usages: list rotated rescues for %q: %v", name, err)
		return
	}
	var paths []string
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		if _, ok := RotatedRescueTimestamp(entry.Name(), name); ok {
			paths = append(paths, filepath.Join(dir, entry.Name()))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(paths)))
	for _, path := range paths[min(len(paths), maxRotatedRescues):] {
		if err := os.Remove(path); err != nil {
			log.Printf("usages: prune rotated rescue %s: %v", path, err)
		}
	}
}

// credentialPathIsLink reports whether path is a symlink. A stored profile file of
// a migrated install is always a regular file; a link means the pre-migration
// layout, whose bytes belong to the live file the CLIs refresh themselves.
func credentialPathIsLink(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}
