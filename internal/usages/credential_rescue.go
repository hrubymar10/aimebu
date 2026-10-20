package usages

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

// rotatedRescueTimeFormat names rotated-credential rescue files under
// switcher/backups/<tool>/. v1 writes these files into the same directory, so
// the format must stay identical.
const rotatedRescueTimeFormat = "20060102T150405.000000000Z"

// RotatedRescueTimestamp recognizes an exact profile rescue name. The switcher
// uses it when renaming or removing profiles so a similar profile name cannot
// cause another account's rescue file to be touched.
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

type claudeOAuthTokens struct {
	AccessToken   string `json:"accessToken,omitempty"`
	AccessTokenS  string `json:"access_token,omitempty"`
	RefreshToken  string `json:"refreshToken,omitempty"`
	RefreshTokenS string `json:"refresh_token,omitempty"`
}

type claudeOAuthFile struct {
	ClaudeAiOAuth  claudeOAuthTokens `json:"claudeAiOauth"`
	ClaudeAiOAuthS claudeOAuthTokens `json:"claude_ai_oauth"`
}

type codexOAuthFile struct {
	Tokens struct {
		RefreshToken  string `json:"refresh_token,omitempty"`
		RefreshTokenC string `json:"refreshToken,omitempty"`
	} `json:"tokens"`
}

// refreshToken extracts the refresh token from stored credentials, accepting
// the field spellings both harnesses have been seen to write.
func refreshToken(data []byte, tool string) (string, error) {
	var token string
	if tool == "codex" {
		var raw codexOAuthFile
		if err := json.Unmarshal(data, &raw); err != nil {
			return "", err
		}
		token = firstNonEmpty(raw.Tokens.RefreshToken, raw.Tokens.RefreshTokenC)
	} else {
		var raw claudeOAuthFile
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func atomicWriteBytes(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return os.Chmod(path, mode)
}
