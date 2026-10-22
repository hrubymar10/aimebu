package switcher

import (
	"fmt"
	"os"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/usages"
)

// Profile is the public view of one stored profile entry.
type Profile struct {
	Tool          string     `json:"tool"`
	Name          string     `json:"name"`
	AlertIdentity string     `json:"-"`
	Active        bool       `json:"active"`
	HasCreds      bool       `json:"has_credentials"`
	Email         string     `json:"email,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	// SwitchedAwayAt is when the profile last stopped being the active one, when
	// that was recorded (by this version or v1; the file is shared).
	SwitchedAwayAt *time.Time `json:"switched_away_at,omitempty"`
}

// Eligibility reports whether a tool's live credentials are usable for switching.
// Absent distinguishes no live login (a missing file or an empty logout file,
// so switching away captures nothing) from corrupt credentials (refused —
// capturing them would destroy a good stored profile). Both report
// Eligible=false; Absent lets Switch tell them apart.
type Eligibility struct {
	Tool     string `json:"tool"`
	Eligible bool   `json:"eligible"`
	Absent   bool   `json:"absent,omitempty"`
	// TargetCopy marks a token-only live copy (see ErrTargetCopy); the switcher
	// refuses to operate on it, which is not a corruption.
	TargetCopy bool   `json:"target_copy,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// Eligibility returns eligibility for all supported tools.
func (m *Manager) Eligibility() ([]Eligibility, error) {
	result := make([]Eligibility, 0, len(Tools()))
	for _, tool := range Tools() {
		el, err := m.eligibilityFor(tool)
		if err != nil {
			return nil, err
		}
		result = append(result, el)
	}
	return result, nil
}

// eligibilityFor checks whether tool's live credentials are usable for switching.
// Three cases from the spec:
//   - file exists, parses, expiry sane → eligible
//   - file absent or parseable empty logout file → not eligible, absent
//   - file present but invalid → not eligible, "credentials look corrupt"
func (m *Manager) eligibilityFor(tool string) (Eligibility, error) {
	path, err := m.liveCredPath(tool)
	if err != nil {
		return Eligibility{}, err
	}
	// Check for absence first. The usages validators convert os.ErrNotExist into
	// a non-wrapping error, so we must stat separately to distinguish absent from corrupt.
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		return Eligibility{Tool: tool, Eligible: false, Absent: true, Reason: fmt.Sprintf("no live credentials file at %s; switching away captures nothing (not logged in, or using the macOS Keychain)", path)}, nil
	}

	if !m.TokenCopiesFor(tool) && liveIsTargetCopy(tool, path) {
		return Eligibility{Tool: tool, TargetCopy: true, Reason: ErrTargetCopy.Error()}, nil
	}

	var valErr error
	switch tool {
	case ToolClaude:
		_, valErr = usages.ValidateClaudeCredentials(path)
	case ToolCodex:
		_, _, valErr = usages.ValidateCodexCredentials(path)
	default:
		return Eligibility{Tool: tool, Eligible: false, Reason: "unknown tool"}, nil
	}
	if valErr == nil {
		return Eligibility{Tool: tool, Eligible: true}, nil
	}
	if emptyLiveLogin(path, tool) {
		return Eligibility{Tool: tool, Absent: true, Reason: "no live login; switching away captures nothing"}, nil
	}
	return Eligibility{Tool: tool, Eligible: false, Reason: "credentials look corrupt"}, nil
}

// emptyLiveLogin recognizes only parseable logout placeholders. A partial
// token, an API key, an unexpected nonempty field, or invalid JSON must still
// block switching rather than being silently discarded.
func emptyLiveLogin(path, tool string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil || root == nil {
		return false
	}
	for key, value := range root {
		switch tool {
		case ToolClaude:
			if key != "claudeAiOauth" && key != "claude_ai_oauth" {
				return false
			}
		case ToolCodex:
			if key == "last_refresh" || key == "auth_mode" {
				continue // harmless metadata left behind after logout
			}
			if key != "tokens" && key != "OPENAI_API_KEY" {
				return false
			}
			if key == "OPENAI_API_KEY" {
				var apiKey string
				if err := json.Unmarshal(value, &apiKey); err != nil || apiKey != "" {
					return false
				}
				continue
			}
		default:
			return false
		}
		if string(value) == "null" {
			continue
		}
		var tokens map[string]json.RawMessage
		if err := json.Unmarshal(value, &tokens); err != nil || tokens == nil {
			return false
		}
		for tokenKey, tokenValue := range tokens {
			switch tool {
			case ToolClaude:
				switch tokenKey {
				case "expiresAt", "expires_at", "scopes", "rateLimitTier", "rate_limit_tier", "subscriptionType", "subscription_type":
					continue // non-credential metadata can remain after logout
				case "accessToken", "access_token", "refreshToken", "refresh_token":
				default:
					return false
				}
			case ToolCodex:
				switch tokenKey {
				case "id_token", "idToken", "account_id", "accountId":
					continue // identity metadata alone cannot authenticate
				case "access_token", "accessToken", "refresh_token", "refreshToken":
				default:
					return false
				}
			}
			var token string
			if err := json.Unmarshal(tokenValue, &token); err != nil || token != "" {
				return false
			}
		}
	}
	return true
}

// liveIsTargetCopy reports whether the live file at path is a token-only copy.
func liveIsTargetCopy(tool, path string) bool {
	switch tool {
	case ToolClaude:
		return usages.IsClaudeTargetCopy(path)
	case ToolCodex:
		return usages.IsCodexTargetCopy(path)
	}
	return false
}

// guardTargetCopy returns ErrTargetCopy (wrapped with the tool) when tool's
// live file is a token-only copy on an install that is not migrated to token
// copies (the copy was written by another install that owns the live files). Every mutating switcher operation calls it
// under the lock before any write.
func (m *Manager) guardTargetCopy(tool string) error {
	if m.TokenCopiesFor(tool) {
		return nil // a token-only live file is the normal state of a migrated install
	}
	path, err := m.liveCredPath(tool)
	if err != nil {
		return err
	}
	if liveIsTargetCopy(tool, path) {
		return fmt.Errorf("%w: %s", ErrTargetCopy, tool)
	}
	return nil
}
