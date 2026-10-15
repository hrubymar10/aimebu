package usages

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const maxRotatedRescues = 3

const rotatedRescueTimeFormat = "20060102T150405.000000000Z"

// commitRotatedCredentials follows the named profile across an active/inactive
// switch. The switcher changes its credential path when it captures the live
// file, but the refresh token still identifies the same account. The caller
// holds no lock during the slow executor; this whole decision runs under the
// switcher lock.
func commitRotatedCredentials(tool string, withLock func(func() error) error, listProfiles func() ([]ProfileInfo, error), name, startedPath string, started, rotated []byte, rescueRoot string, runStarted ...time.Time) error {
	startedAt := time.Now()
	if len(runStarted) > 0 {
		startedAt = runStarted[0]
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
				return saveAbortedRotation(tool, name, rotated, rescueRoot, fmt.Errorf("list profiles for copy-back: %w", err))
			}
			var matches []string
			var unresolved error
			if removed {
				matches, unresolved = matchingProfilePaths(profiles, tool, beforeToken)
				if removedAt.After(startedAt) && len(matches) == 0 {
					if unresolved != nil {
						return saveAbortedRotation(tool, name, rotated, rescueRoot, unresolved)
					}
					log.Printf("usages: %s maintenance profile=%q outcome=discarded reason=profile removed", tool, name)
					return errClaudeRefreshProfileGone
				}
			}
			var named *ProfileInfo
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
						unresolved = errClaudeRefreshProfileGone
					}
					return saveAbortedRotation(tool, name, rotated, rescueRoot, unresolved)
				}
				if len(matches) > 1 {
					return saveAbortedRotation(tool, name, rotated, rescueRoot, fmt.Errorf("multiple profiles match starting refresh token"))
				}
				currentPath = matches[0]
			}
		} else if removed {
			return saveAbortedRotation(tool, name, rotated, rescueRoot, fmt.Errorf("cannot resolve removed token without profile listing"))
		}
		current, err := os.ReadFile(currentPath)
		if err != nil {
			return saveAbortedRotation(tool, name, rotated, rescueRoot, errClaudeRefreshProfileGone)
		}
		currentToken, err := refreshToken(current, tool)
		if err != nil || currentToken != beforeToken {
			return saveAbortedRotation(tool, name, rotated, rescueRoot, errClaudeRefreshProfileChanged)
		}
		merged, err := mergeRotatedCredentials(tool, current, rotated)
		if err != nil {
			return saveAbortedRotation(tool, name, rotated, rescueRoot, fmt.Errorf("merge rotated credentials: %w", err))
		}
		// Preserve the executor's exact bytes when no concurrent edit exists and
		// Claude has no unrelated top-level data to retain.
		if bytes.Equal(current, started) && (tool == "codex" || onlyClaudeOAuth(current)) {
			merged = rotated
		}
		if err := writeStoredCredential(currentPath, merged, 0o600); err != nil {
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
			return saveAbortedRotation(tool, name, rotated, rescueRoot, fmt.Errorf("lock copy-back: %w", err))
		}
		return err
	}
	return commit()
}

func matchingProfilePaths(profiles []ProfileInfo, tool, token string) ([]string, error) {
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

func refreshToken(data []byte, tool string) (string, error) {
	var token string
	if tool == "codex" {
		var raw codexAuthFile
		if err := json.Unmarshal(data, &raw); err != nil {
			return "", err
		}
		token = firstNonEmpty(raw.Tokens.RefreshToken, raw.Tokens.RefreshTokenC)
	} else {
		var raw claudeAuthFile
		if err := json.Unmarshal(data, &raw); err != nil {
			return "", err
		}
		tokens := raw.ClaudeAiOAuth
		if firstNonEmpty(tokens.AccessToken, tokens.AccessTokenS) == "" && firstNonEmpty(raw.ClaudeAiOAuthS.AccessToken, raw.ClaudeAiOAuthS.AccessTokenS) != "" {
			tokens = raw.ClaudeAiOAuthS
		}
		token = firstNonEmpty(tokens.RefreshToken, tokens.RefreshTokenS)
	}
	if token == "" {
		return "", fmt.Errorf("missing %s refresh token", tool)
	}
	return token, nil
}

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
			return nil, fmt.Errorf("rotated codex tokens missing")
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
			return nil, fmt.Errorf("rotated claude OAuth section missing")
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
	log.Printf("usages: %s maintenance profile=%q outcome=aborted reason=%v rotated_saved=%s", tool, name, cause, path)
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

// RotatedRescueTimestamp recognizes an exact profile rescue name. The switcher
// uses the same parser when renaming or removing profiles so a similar profile
// name cannot cause another account's rescue file to be touched.
func RotatedRescueTimestamp(fileName, profileName string) (time.Time, bool) {
	timestamp, ok := strings.CutPrefix(fileName, profileName+"-rotated-")
	if !ok {
		return time.Time{}, false
	}
	timestamp, ok = strings.CutSuffix(timestamp, ".json")
	if !ok {
		return time.Time{}, false
	}
	parsed, err := time.Parse(rotatedRescueTimeFormat, timestamp)
	return parsed, err == nil
}

// RotatedRescueName formats the matching name for a rotated rescue file.
func RotatedRescueName(profileName string, at time.Time) string {
	return profileName + "-rotated-" + at.UTC().Format(rotatedRescueTimeFormat) + ".json"
}
